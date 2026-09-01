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
	"strconv"
	"sync"
	"time"

	slackgo "github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/serve"
)

// The environment variables the two Slack credentials are read from. A configuration file
// is committed and shared, so neither token may be written there.
const (
	// appTokenVar is the app-level token, which opens the socket mode connection.
	appTokenVar = "SLACK_APP_TOKEN"
	// botTokenVar is the bot token, which every Web API call is made with.
	botTokenVar = "SLACK_BOT_TOKEN"
)

// This channel is all three of the optional shapes a channel can have: it sizes its own
// concurrency, it holds a socket connection to release, and its credential can be revoked
// while it runs. Declaring them makes a change to any of those contracts a compile error
// here rather than a channel the server silently stops asking.
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

	// ContextLines is how many messages of surrounding conversation a turn reads. Zero is
	// unset and takes a default of 20, matching config.DefaultSlackContextLines. A
	// negative value reads none, so a turn sees only the mention it answers.
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
		return fmt.Errorf("AppToken is required: it opens the socket mode connection")
	}
	if o.BotToken == "" {
		return fmt.Errorf("BotToken is required: every Web API call is made with it")
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

	// limit is the allowance every call this channel makes to Slack is spent from. One
	// bucket for the whole channel, because Slack counts the Tier 3 methods a status
	// message, an answer, a question and a refusal all use for the app across the
	// workspace rather than per channel or per message.
	limit *limiter

	// clock is the time this channel measures a question's grace window with, so a spec
	// drives that window rather than waiting out one. The limiter holds one of its own for
	// the same reason.
	clock clock

	// asked is every question this worker is holding. It belongs here rather than to a
	// run because the goroutine reading envelopes has to find one without knowing which
	// turn asked it, and because a question outlives the turn: a deferred call waits on
	// the thread, so what is open in a thread is what the next mention is decided against.
	asked *questions

	// taken recognizes a message this worker already acted on, so a redelivery is
	// acknowledged and dropped rather than paying for the same turn again.
	taken *seen

	// names resolves the speakers in a preload or a gap read to what a person reading the
	// thread sees, once per user rather than once per line.
	names *names

	// mu guards everything admission decides on, and every one of those decisions is
	// made in memory under it, before an envelope is acknowledged.
	mu sync.Mutex

	// inFlight is the turn holding each thread, which is the only mutual exclusion
	// between two concurrent resumes of one conversation: runstate writes a claim but
	// does not enforce one.
	inFlight map[string]*turn

	// stoppable is every turn a Stop button can still reach, keyed by the turn id that
	// button carries. A turn is entered when its status message is built and dropped when
	// it gives its thread back, so a press for a turn that has ended finds nothing and
	// whoever pressed is told so. A channel posting no status message enters none.
	stoppable map[string]*turn

	// queued holds the turns waiting for a thread another turn is running in, and parked
	// counts them so the backlog bound covers them as well as the ones waiting for a
	// worker.
	queued map[string][]*turn
	parked int

	// waiting is the admitted turns Next pops from, and wake tells it there may be one.
	// A queue rather than a handover, so the goroutine reading envelopes never blocks on
	// the server's puller.
	waiting []*turn
	wake    chan struct{}

	// handed counts the turns given to the server that have not yet reported an outcome,
	// which is how a turn being admitted tells a worker it can start on from one it has
	// to wait for, and so a first hint from the queued line. The server bounds this
	// channel by Concurrency and reports nothing back about how much of it is spent.
	handed int

	// posts counts the goroutines this channel started for the messages it posts for
	// itself, which Close waits for so a refusal is not lost to a shutdown. postsClosed
	// says it has stopped waiting, after which a message is posted on the goroutine that
	// asked for it: a run reporting its outcome after the drain has moved on still owes
	// its thread an explanation.
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
// the same call supplies the workspace this bot answers in. The context limits that
// identity check and nothing else.
//
// It starts nothing. The socket opens on the first call to Next, so a channel that is
// built and never served holds no connection.
func New(ctx context.Context, opts Options) (*Channel, error) {
	err := opts.validate()
	if err != nil {
		return nil, err
	}

	log := opts.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	log = log.With("channel", channelName)

	client := slackgo.New(opts.BotToken, slackgo.OptionAppLevelToken(opts.AppToken))

	c, err := newChannel(ctx, opts, &clientAPI{client: client}, &clientSocket{
		client: socketmode.New(client),
		out:    make(chan envelope),
		fail:   make(chan error, 1),
		log:    log,
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

// newChannel assembles a Channel over an already-built API and socket, so a spec drives
// every decision this package makes without reaching Slack. The context limits the
// identity check and nothing else.
func newChannel(ctx context.Context, opts Options, a api, s socket, log *slog.Logger) (*Channel, error) {
	// The socket is rooted at Background rather than at the caller's context, because the
	// connection outlives construction: a caller building under a deadline would otherwise
	// have its bot cut off when that deadline passes.
	socketCtx, socketOff := context.WithCancel(context.Background())

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
		limit:     newLimiter(defaultRateInterval, defaultRateBurst, nil),
		clock:     wallClock{},
		asked:     newQuestions(0),
		taken:     newSeen(0),
		names:     newNames(log),
		inFlight:  map[string]*turn{},
		stoppable: map[string]*turn{},
		queued:    map[string][]*turn{},
		wake:      make(chan struct{}, 1),
		faults:    make(chan error, 1),
		shutdown:  make(chan struct{}),
		socketCtx: socketCtx,
		socketOff: socketOff,
		socketEnd: make(chan struct{}),
		intakeEnd: make(chan struct{}),
	}

	// Defaulted here as well as in the configuration accessors, since Options an embedder
	// assembles in process reaches neither. A zero would read as a limit of none rather
	// than as unset: no mention admitted, no folded line delivered, and every question
	// deferred the instant it was asked.
	if c.maxWait <= 0 {
		c.maxWait = opts.Workers * waitingPerWorker
	}
	if c.maxCoal <= 0 {
		c.maxCoal = defaultMaxCoalesced
	}
	if c.grace <= 0 {
		c.grace = defaultAnswerGrace
	}

	// A negative ContextLines is the caller asking for no surrounding conversation: preload
	// and gap read nothing, and Describe reports 0.
	switch {
	case c.lines == 0:
		c.lines = defaultContextLines
	case c.lines < 0:
		c.lines = 0
	}

	// The identity check is the last thing that can fail, so newChannel cancels the socket
	// here rather than leaving it to a caller who never received a Channel.
	ws, err := a.authTest(ctx)
	if err != nil {
		socketOff()
		return nil, fmt.Errorf("%s does not authenticate: %w", botTokenVar, err)
	}
	c.workspace = ws

	return c, nil
}

// channelName identifies this channel in the server's logs, its metrics and a worker's
// startup banner.
const channelName = "slack"

// Name identifies the channel in the server's logs.
func (c *Channel) Name() string { return channelName }

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
			w, err := c.workFor(ctx, t)
			if err != nil {
				t.log.Error("Refusing a mention whose conversation could not be read", "error", err)

				// The refusal goes on the status message where the turn has one, so the
				// thread says the outcome once and in the place the person is already
				// watching, for one call rather than two. A turn with no status message,
				// which is what no_progress leaves, is told in a message of its own.
				if t.status != nil {
					t.status.ends(storeRefusal, emojiFailed)
					t.status.stop()
				} else {
					c.reply(t.m, storeRefusal)
				}

				c.mu.Lock()
				c.releaseLocked(t)
				c.mu.Unlock()

				continue
			}

			c.mu.Lock()
			c.handed++
			c.mu.Unlock()

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
// The socket is closed last. Turns already handed to the server are still running and are
// still receiving what people click on their messages, so a socket closed at the start of
// a drain would strand every question open at that moment.
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

		// The intake goroutine stops first, so nothing is still deciding on a mention when
		// the waiting set is rendered and the answers travel out.
		<-c.intakeEnd
		c.abandonWaiting()

		// The messages this channel started are waited for next, so nobody is still being
		// told they were refused when the connection those answers travel over is taken
		// away.
		c.awaitPosts()

		c.socketOff()
		<-c.socketEnd
		c.closeErr = c.socketErr
	})

	return c.closeErr
}

// abandonWaiting says on its own status message that every turn admitted and never handed
// over is not going to run, and drops them.
//
// The channel renders this set itself because the server reports no outcome for work it
// never took: Outcome.Abandoned covers what the puller already holds, and a turn still in
// this queue never reached it. Nothing else would edit these messages again, so somebody
// who mentioned the bot a minute before a deploy would be left with a queued line on their
// thread for good.
func (c *Channel) abandonWaiting() {
	c.mu.Lock()

	stranded := c.waiting

	for _, behind := range c.queued {
		stranded = append(stranded, behind...)
	}

	for _, t := range stranded {
		// Neither the thread nor the Stop button reaches a turn that will not run.
		delete(c.stoppable, t.id)

		if c.inFlight[t.session] == t {
			delete(c.inFlight, t.session)
		}
	}

	c.waiting = nil
	c.queued = map[string][]*turn{}
	c.parked = 0

	c.mu.Unlock()

	for _, t := range stranded {
		t.log.Info("A turn was admitted and never started, so its thread is told the worker is going down", "user", t.m.UserID)

		c.speak(func() {
			t.status.ends(abandonedNote, emojiStopped)
			t.status.stop()
		})
	}
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

// DescLine is one label and value describing this channel, for a caller printing a
// startup banner.
type DescLine struct {
	Label string
	Value string
}

// Describe names the workspace this bot joined, the identity it joined as and the limits
// it answers under, for the banner a worker prints before its log takes over.
//
// The workspace and the bot come from the authTest at construction rather than from the
// configuration, since neither is written there: an operator holding two bot tokens has no
// other way to see which one this process is using. Both are named with their id, which is
// what a Slack admin page and an audit log are searched by.
func (c *Channel) Describe() []DescLine {
	progress := "on"
	if !c.progress {
		progress = "off"
	}

	return []DescLine{
		{Label: "Workspace", Value: fmt.Sprintf("%s (%s)", c.workspace.Team, c.workspace.TeamID)},
		{Label: "Bot", Value: fmt.Sprintf("%s (%s)", c.workspace.User, c.workspace.UserID)},
		{Label: "Workers", Value: strconv.Itoa(c.workers)},
		{Label: "Answer Grace", Value: c.grace.String()},
		{Label: "Context Lines", Value: strconv.Itoa(c.lines)},
		{Label: "Progress", Value: progress},
	}
}
