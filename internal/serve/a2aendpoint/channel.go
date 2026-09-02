//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2aendpoint

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/a2a"
	wire "github.com/choria-io/fisk-ai/internal/a2a/wire/v1"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/serve"
)

// A prompt channel sizes its own concurrency, holds a transport to release, and names
// its addresses on a startup banner. Declaring those contracts makes a change to any of
// them a compile error here rather than a channel the server silently stops asking.
var (
	_ serve.ConcurrentChannel = (*Channel)(nil)
	_ serve.ReleasableChannel = (*Channel)(nil)
	_ serve.DescribedEndpoint = (*Channel)(nil)
)

// Channel is a serve.Channel over the a2a task route: a peer sends a prompt, this
// hands the run to a server, and the run's events and answer travel back on the reply
// set the request opened.
//
// Intake runs on the transport's serving goroutine, so it admits and hands over rather
// than waiting for the run: a handler that waited would stop the process taking any
// other prompt.
type Channel struct {
	identity  string
	workers   int
	held      *sharedTransport
	stream    a2a.StreamingTransport
	validator *wire.Validator
	log       *slog.Logger

	// elicits is expose.agent.a2a.prompts.elicit: with it off the channel supplies no
	// prompter and the server refuses every confirmation-gated tool, which is what a
	// caller that answers nothing needs. promptWait is how long one question is held
	// open, taken from request_timeout since it measures the same thing, and the
	// channel's own prompter enforces it: a caller with a person in front of the
	// question restarts it, which is why the server bounds none of them.
	elicits    bool
	promptWait time.Duration

	// maxTokens is the configured cumulative token bound on a conversation, carried on
	// every accepting ack so a caller can show how much of the allowance is left. Zero
	// where the configuration bounds nothing. It is the local ceiling: a request that
	// lowers its own budget is clamped by the server, so the ack reports the lower of
	// the two rather than this.
	maxTokens int64

	// work hands one admitted prompt to Next. It is unbuffered: admission has already
	// reserved the slot, so the wait here is for the server's puller to come round
	// rather than for capacity, and it is waited out on a goroutine per prompt.
	work     chan *serve.Work
	handoffs sync.WaitGroup

	closeOnce sync.Once
	closeErr  error
	shutdown  chan struct{}

	// sessions is the run-journal store, read to answer a request that only asks to be
	// told what a conversation holds. It is nil when the process hosting this channel
	// supplied none, which refuses such a request rather than running one.
	sessions runstate.Store

	// mu guards inFlight, which is both the capacity count and the set of request ids
	// this worker is answering. It is touched from the serving goroutine, from each
	// run's ending and from a cancel, so it is a plain mutex rather than a counter.
	mu       sync.Mutex
	inFlight map[string]*task
}

// newChannel registers the task route on the shared transport and returns the channel
// that answers it.
//
// It refuses a transport that cannot carry a reply set. Answering a prompt produces an
// ack, then events, then a terminal message, and a binding that answers once cannot
// express that at all, so the refusal names the binding at startup rather than arriving
// one request at a time.
func newChannel(cfg *config.Config, held *sharedTransport, opts ConfigOptions) (*Channel, error) {
	stream, ok := held.transport.(a2a.StreamingTransport)
	if !ok {
		return nil, fmt.Errorf("the %q transport carries a single reply, so it cannot answer prompts; remove expose.agent.a2a.prompts or use a binding that streams", cfg.A2ATransport())
	}

	validator, err := wire.SharedValidator()
	if err != nil {
		return nil, fmt.Errorf("compiling the a2a schemas: %w", err)
	}

	log := opts.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(nopWriter{}, nil))
	}

	c := &Channel{
		identity:   cfg.Identity,
		workers:    cfg.A2APromptsWorkers(),
		held:       held,
		stream:     stream,
		validator:  validator,
		log:        log.With("channel", channelName),
		elicits:    cfg.A2APromptsElicit(),
		promptWait: cfg.A2ARequestTimeout(),
		maxTokens:  cfg.LLM.Budget.MaxTokens,
		sessions:   opts.Sessions,
		work:       make(chan *serve.Work),
		shutdown:   make(chan struct{}),
		inFlight:   make(map[string]*task),
	}

	err = stream.ServeReplySet(a2a.OpTask, c.handle)
	if err != nil {
		return nil, fmt.Errorf("registering the task handler: %w", err)
	}

	c.log.Info("Answering prompts over a2a", "identity", cfg.Identity, "workers", c.workers, "elicit", c.elicits)

	return c, nil
}

// channelName identifies the prompt channel apart from the tool service beside it, which
// names itself "a2a". An operator reading a log line or a banner sees which of the two
// is speaking.
const channelName = "a2a/prompts"

// Name identifies the channel in the server's logs.
func (c *Channel) Name() string { return channelName }

// Heading names this endpoint on a startup banner. An identity that also serves tools
// prints two sections, so each one names the path it answers on.
func (c *Channel) Heading() string { return "Answering prompts over a2a" }

// Describe returns the addresses a peer sends a prompt to and addresses a cancel under,
// the one it answers questions on when this channel asks any, and how many prompts it
// runs at once. The transport supplies the addresses, so a later binding describes
// itself in its own terms and this endpoint never builds one; a binding that names no
// addresses leaves the worker count as the only row.
func (c *Channel) Describe() []serve.DescLine {
	tasks := taskLines(c.stream, c.identity, c.elicits)

	lines := make([]serve.DescLine, 0, len(tasks)+1)
	for _, l := range tasks {
		lines = append(lines, serve.DescLine{Label: l.Label, Value: l.Value})
	}

	return append(lines, serve.DescLine{Label: "Workers", Value: strconv.Itoa(c.workers)})
}

// Concurrency is how many prompts this channel may have running at once, which admission
// also refuses a caller above, so the server's slots and the channel's acks agree.
func (c *Channel) Concurrency() int { return c.workers }

// Next blocks until a peer's prompt has been admitted and returns it as work.
//
// It returns serve.ErrChannelDone once the channel has been closed, so the server stops
// asking an endpoint that no longer answers.
func (c *Channel) Next(ctx context.Context) (*serve.Work, error) {
	select {
	case w := <-c.work:
		return w, nil
	case <-c.shutdown:
		return nil, serve.ErrChannelDone
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Close stops answering and ends every prompt that was accepted but never started.
//
// Closing the transport takes this identity out of its queue group, so a peer sending
// during a drain reaches a sibling. Runs already handed to the server keep going and are
// the server's to wait for; a prompt still waiting to be handed over is ended here with
// a terminal message rather than left as an ack nothing ever answers.
//
// It is idempotent and returns the same answer to every caller, since a program that
// drains on one signal and stops on the next releases every endpoint twice.
func (c *Channel) Close() error {
	c.closeOnce.Do(func() {
		// Shutdown first, so intake refuses a request that arrives while the transport is
		// still registered rather than acking one nothing will run.
		close(c.shutdown)
		c.closeErr = c.held.Close()
		c.handoffs.Wait()
	})

	return c.closeErr
}

// Faults reports that this identity has stopped answering for a reason nobody asked
// for, which for this channel means no further prompt can arrive.
func (c *Channel) Faults() <-chan error { return c.held.faults }

// draining reports that this channel has been closed, so a question stops having its
// window restarted and the runs in flight reach an ending the shutdown can wait for.
func (c *Channel) draining() bool {
	select {
	case <-c.shutdown:
		return true
	default:
		return false
	}
}

// nopWriter discards a log a caller did not ask for. A channel with no logger still
// logs, since every line it writes is about work it has accepted from the network.
type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
