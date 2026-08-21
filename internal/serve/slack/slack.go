//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// Package slack hosts an agent behind a Slack bot: somebody mentions it, a thread opens,
// and that thread is one conversation for as long as people keep mentioning it there.
//
// The channel reads Slack's socket mode connection, so nothing listens for inbound
// traffic and no address is published. Slack requires an envelope to be acknowledged
// within three seconds, so intake acknowledges and hands the work over rather than
// waiting for the run: a handler that waited would stop this process taking anything
// else. That is a2aendpoint's shape rather than the queue channel's, whose handler blocks
// because returning is its acknowledgement.
//
// # What a person sees
//
// A turn posts a status message it edits while the run works, posts the answer as its own
// message, and edits the status message to point at the answer. The answer is a separate
// message because Slack sends no notification for an edit: a turn that ended by editing
// its own status message would ping somebody with "Thinking..." and never tell them the
// answer had arrived.
//
// A question the agent asks is its own message with buttons, and anybody in the thread may
// answer it. Nothing expires: past a short grace window the run defers, gives its worker
// back, and resumes whenever the answer arrives, which may be days later and across a
// restart.
//
// # Credentials
//
// SLACK_APP_TOKEN and SLACK_BOT_TOKEN, read from the environment and never from the
// configuration file. A missing one fails at construction naming the variable.
//
// # One process per bot token
//
// Slack allows an app up to ten socket connections and spreads envelopes across them,
// which this channel cannot use: the threads it is running and the questions it is holding
// are in memory, so a click landing on a process that holds neither reaches nothing. Run
// one worker per token.
package slack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	slackgo "github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/serve"
)

// The environment variables the two Slack credentials are read from. They are named here
// rather than in the configuration because a file that is committed and shared must never
// hold either.
const (
	// AppTokenVar is the app-level token, which opens the socket mode connection.
	AppTokenVar = "SLACK_APP_TOKEN"
	// BotTokenVar is the bot token, which every Web API call is made with.
	BotTokenVar = "SLACK_BOT_TOKEN"
)

// A Slack channel runs turns for people, holds a socket connection, and stops working
// when that connection cannot be re-established, so it is all three of the optional
// shapes a channel can have. Declaring them makes a change to any of those contracts a
// compile error here rather than a channel the server silently stops asking.
//
// The siblings assert the first two. FaultingEndpoint is asserted here as well because
// this channel is the one whose endpoint can be revoked while it runs.
var (
	_ serve.ConcurrentChannel = (*Channel)(nil)
	_ serve.ReleasableChannel = (*Channel)(nil)
	_ serve.FaultingEndpoint  = (*Channel)(nil)
)

// Options configures a Channel.
type Options struct {
	// AppToken opens the socket mode connection and BotToken makes every Web API call.
	// Both are required. NewFromConfig reads them from the environment; a caller building
	// this directly supplies its own.
	AppToken string
	BotToken string

	// Identity is the agent name hashed into every session this channel derives, so two
	// agents sharing a workspace keep their conversations apart. It is normally the
	// configured agent identity and is required.
	Identity string

	// Workers is how many turns run at once, which is also how many the server is told to
	// allow through Concurrency. Required to be greater than zero.
	Workers int

	// ContextLines bounds how much surrounding conversation a turn reads.
	ContextLines int

	// Progress says whether a turn posts the status message it edits while it runs. The
	// answer, the questions and the refusals post either way.
	Progress bool

	// AnswerGrace is how long a question is held while the run that asked it is still
	// loaded. Past it the run defers and the answer arrives whenever somebody clicks.
	AnswerGrace time.Duration

	// MaxWaiting is how many admitted turns may wait for a worker before a further
	// mention is refused, and MaxCoalesced how many messages fold into one follow-up
	// turn.
	MaxWaiting   int
	MaxCoalesced int

	// Sessions is the run-journal store, borrowed and never closed here since the runs
	// write to the same one. It is required: a thread is a conversation, and this channel
	// reads the store to tell a thread it already holds from one it is opening.
	Sessions runstate.Store

	// SuspendRequested is handed to every run and polled at a loop boundary, so a worker
	// draining stops its turns where they can be resumed from. Nil never suspends.
	SuspendRequested func() bool

	// Logger receives the channel's progress, which is a line per turn and per question.
	// Nil builds a text logger on stderr.
	Logger *slog.Logger
}

func (o *Options) validate() error {
	if o.AppToken == "" {
		return fmt.Errorf("an app-level token is required: set %s", AppTokenVar)
	}
	if o.BotToken == "" {
		return fmt.Errorf("a bot token is required: set %s", BotTokenVar)
	}
	if o.Identity == "" {
		return fmt.Errorf("an identity is required: it names the journals this bot's threads run in")
	}
	if o.Workers <= 0 {
		return fmt.Errorf("workers must be greater than zero")
	}
	if o.Sessions == nil {
		return fmt.Errorf("a session store is required: a thread is a conversation, so a worker with nowhere to journal one could answer a first mention and nothing after it")
	}

	return nil
}

// Channel is a serve.Channel over a Slack bot.
type Channel struct {
	identity  string
	workers   int
	lines     int
	progress  bool
	grace     time.Duration
	maxWait   int
	maxCoal   int
	suspend   func() bool
	workspace workspace
	log       *slog.Logger

	api      api
	socket   socket
	sessions runstate.Store

	// taken recognizes a message this worker already acted on, so a redelivery is
	// acknowledged and dropped rather than paying for the same turn again.
	taken *seen

	// mu guards everything admission decides on, and every one of those decisions is
	// made in memory under it, before an envelope is acknowledged.
	mu sync.Mutex

	// inFlight is the turn holding each thread, which is the only mutual exclusion
	// between two concurrent resumes of one conversation: runstate writes a claim but
	// does not enforce one.
	inFlight map[string]*turn

	// queued holds the turns waiting for a thread another turn is running in, and parked
	// counts them so the backlog bound covers them as well as the ones waiting for a
	// worker.
	queued map[string][]*turn
	parked int

	// waiting is the admitted turns Next pops from, and wake tells it there may be one.
	// A queue rather than a handover, so the goroutine reading envelopes never blocks on
	// the server's puller: what it bounds is the backlog, not who waits for whom, the
	// puller being serial either way.
	waiting []*turn
	wake    chan struct{}

	// posts counts the goroutines this channel started for the messages it posts for
	// itself, which Close waits for so a refusal is not lost to a shutdown. postsClosed
	// says it has stopped waiting, after which a message is posted on the goroutine that
	// asked for it: a run reporting its outcome after the drain has moved on still owes
	// its thread an explanation, and starting a goroutine the wait has passed is the
	// misuse that panics.
	posts       sync.WaitGroup
	postsClosed bool

	// faults carries the report that this bot has stopped answering. It is buffered by
	// one and written once, the first fault being what ends the worker.
	faults    chan error
	faultOnce sync.Once

	closeOnce sync.Once
	closeErr  error
	shutdown  chan struct{}

	// running guards the socket's lifetime: started on the first Next, waited for by
	// Close, so a channel that is constructed and never served reads no envelope.
	startOnce sync.Once
	started   bool
	socketCtx context.Context
	socketOff context.CancelFunc
	socketEnd chan struct{}
	socketErr error

	// intakeEnd is closed when the goroutine reading envelopes has stopped, which Close
	// waits for before anything else: nothing else may be taken once a drain begins.
	intakeEnd chan struct{}
}

// New builds a Channel and identifies the credential it was given, which reaches the
// network: a revoked or mistyped token fails here rather than on the first mention, and
// the same call supplies the workspace this bot answers in.
//
// It starts nothing. The socket opens on the first call to Next, so a channel that is
// built and never served holds no connection.
func New(opts Options) (*Channel, error) {
	err := opts.validate()
	if err != nil {
		return nil, err
	}

	log := opts.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	log = log.With("channel", ChannelName)

	client := slackgo.New(opts.BotToken, slackgo.OptionAppLevelToken(opts.AppToken))

	c, err := newChannel(opts, &clientAPI{client: client}, &clientSocket{
		client: socketmode.New(client),
		out:    make(chan envelope),
		fail:   make(chan error, 1),
	}, log)
	if err != nil {
		return nil, err
	}

	log.Info("Answering in Slack",
		"identity", opts.Identity,
		"team", c.workspace.Team,
		"team_id", c.workspace.TeamID,
		"bot", c.workspace.UserID,
		"workers", opts.Workers,
		"progress", opts.Progress)

	return c, nil
}

// newChannel assembles a Channel over an already-built API and socket, which is what lets
// a test drive every decision this package makes without reaching Slack.
func newChannel(opts Options, a api, s socket, log *slog.Logger) (*Channel, error) {
	ctx, cancel := context.WithCancel(context.Background())

	c := &Channel{
		identity:  opts.Identity,
		workers:   opts.Workers,
		lines:     opts.ContextLines,
		progress:  opts.Progress,
		grace:     opts.AnswerGrace,
		maxWait:   opts.MaxWaiting,
		maxCoal:   opts.MaxCoalesced,
		suspend:   opts.SuspendRequested,
		log:       log,
		api:       a,
		socket:    s,
		sessions:  opts.Sessions,
		taken:     newSeen(0),
		inFlight:  map[string]*turn{},
		queued:    map[string][]*turn{},
		wake:      make(chan struct{}, 1),
		faults:    make(chan error, 1),
		shutdown:  make(chan struct{}),
		socketCtx: ctx,
		socketOff: cancel,
		socketEnd: make(chan struct{}),
		intakeEnd: make(chan struct{}),
	}

	// Both bounds are defaulted here as well as in the configuration, because a Config
	// an embedder builds in process never runs prepare and an Options an embedder builds
	// directly never sees the configuration at all. A zero would read as a bound of none
	// rather than as unset: no mention would be admitted, and no folded line delivered.
	if c.maxWait <= 0 {
		c.maxWait = opts.Workers * waitingPerWorker
	}
	if c.maxCoal <= 0 {
		c.maxCoal = defaultMaxCoalesced
	}

	// The identity check is the last thing that can fail, so the cancel above is
	// released here rather than left to a caller who never received a Channel.
	ws, err := a.authTest(ctx)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%s does not authenticate: %w", BotTokenVar, err)
	}
	c.workspace = ws

	return c, nil
}

// ChannelName identifies this channel in the server's logs, its metrics and a worker's
// startup banner.
const ChannelName = "slack"

// Name identifies the channel in the server's logs.
func (c *Channel) Name() string { return ChannelName }

// Concurrency is how many turns this channel may have running at once, which is also the
// number above which a mention waits rather than starting.
func (c *Channel) Concurrency() int { return c.workers }

// Next blocks until a turn has been admitted and returns it as work.
//
// It opens the socket on its first call, so nothing is accepted before the server is
// ready to run it. It returns serve.ErrChannelDone once the channel has been closed, so
// the server stops asking an endpoint that no longer answers.
//
// The turn's checkpoint is decided here rather than at admission, which is a read of the
// session store: a thread this worker already holds continues its conversation, and one
// it does not opens a new one. A store that cannot answer refuses that one turn, tells
// the thread so, and hands over whatever is behind it, since the rest of the workspace
// has done nothing wrong.
func (c *Channel) Next(ctx context.Context) (*serve.Work, error) {
	c.start()

	for {
		if c.draining() {
			return nil, serve.ErrChannelDone
		}

		t := c.takeWaiting()
		if t != nil {
			w, err := c.workFor(t)
			if err != nil {
				t.log.Error("Refusing a mention whose conversation could not be read", "error", err)
				c.reply(t.m, storeRefusal)

				c.mu.Lock()
				c.releaseLocked(t)
				c.mu.Unlock()

				continue
			}

			return w, nil
		}

		select {
		case <-c.wake:
		case <-c.shutdown:
			return nil, serve.ErrChannelDone
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// Faults reports that this bot has stopped answering for a reason nobody asked for, which
// here means the credential was refused and no mention can arrive again.
//
// A dropped connection is not one. The socket mode client re-establishes it on its own,
// so a drop is logged and waited out; what ends the worker is a refusal to let it back
// in, which no amount of waiting repairs.
func (c *Channel) Faults() <-chan error { return c.faults }

// Close stops the channel taking new work and releases the connection.
//
// The socket is closed last and deliberately. Turns already handed to the server are
// still running and are still receiving what people click on their messages, so a socket
// closed at the start of a drain would strand every question open at that moment.
//
// It is idempotent and returns the same answer to every caller, since a program that
// drains on one signal and stops on the next releases every endpoint twice.
func (c *Channel) Close() error {
	c.closeOnce.Do(func() {
		// Shutdown first, so intake refuses a mention arriving while the socket is still
		// connected rather than accepting one nothing will run.
		close(c.shutdown)

		if !c.started {
			c.socketOff()
			c.awaitPosts()

			return
		}

		// The intake goroutine stops first and the messages it started are waited for
		// next, so nothing is still deciding on a mention or still telling somebody they
		// were refused when the connection those answers travel over is taken away.
		<-c.intakeEnd
		c.awaitPosts()

		c.socketOff()
		<-c.socketEnd
		c.closeErr = c.socketErr
	})

	return c.closeErr
}

// start opens the socket once, on the channel's own context rather than a caller's, so it
// outlives the server's and Close decides when it ends. The goroutine that reads what
// arrives on it starts with it, since an envelope read before there is a server to run it
// is an envelope acknowledged and lost.
func (c *Channel) start() {
	c.startOnce.Do(func() {
		c.started = true

		go c.intake()

		go func() {
			defer close(c.socketEnd)

			err := c.socket.run(c.socketCtx)
			if err != nil && !errors.Is(err, context.Canceled) {
				c.socketErr = err
				c.log.Error("The Slack connection stopped", "error", err)
				c.fault(err)
			}
		}()
	})
}

// draining reports that this channel has been closed, so intake refuses and the turns in
// flight reach an ending the shutdown can wait for.
func (c *Channel) draining() bool {
	select {
	case <-c.shutdown:
		return true
	default:
		return false
	}
}

// fault records that this bot has stopped answering. It is called from the socket's own
// goroutine, so it must not block: the channel is buffered and written once.
func (c *Channel) fault(err error) {
	c.faultOnce.Do(func() { c.faults <- err })
}

// Workspace is the team this bot answers in, as authTest reported it at construction. A
// caller building its own banner reads it from here rather than making the call again.
func (c *Channel) Workspace() (team string, teamID string, botUserID string) {
	return c.workspace.Team, c.workspace.TeamID, c.workspace.UserID
}
