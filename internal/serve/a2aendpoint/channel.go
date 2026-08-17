//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2aendpoint

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/serve"
)

// A prompt channel produces work and holds a transport, so it is both of the optional
// shapes a channel can have. Declaring them makes a change to either contract a compile
// error here rather than a channel the server silently stops asking.
var (
	_ serve.ConcurrentChannel = (*Channel)(nil)
	_ serve.ReleasableChannel = (*Channel)(nil)
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
	validator *a2a.Validator
	log       *slog.Logger

	// elicits is expose.agent.a2a.prompts.elicit: with it off the channel supplies no
	// prompter and the server refuses every confirmation-gated tool, which is what a
	// caller that answers nothing needs. promptWait is how long a question is held
	// open, taken from request_timeout since it measures the same thing, and it reaches
	// the server on Work.PromptWait.
	elicits    bool
	promptWait time.Duration

	// work hands one admitted prompt to Next. It is unbuffered: admission has already
	// reserved the slot, so the wait here is for the server's puller to come round
	// rather than for capacity, and it is waited out on a goroutine per prompt.
	work     chan *serve.Work
	handoffs sync.WaitGroup

	closeOnce sync.Once
	closeErr  error
	shutdown  chan struct{}

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

	validator, err := a2a.NewValidator()
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
		work:       make(chan *serve.Work),
		shutdown:   make(chan struct{}),
		inFlight:   make(map[string]*task),
	}

	err = held.transport.Serve(a2a.OpTask, c.handle)
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

// Describe returns the addresses a peer sends a prompt to and addresses a cancel under,
// for display. The transport answers it, so a later binding describes itself in its own
// terms and this endpoint never builds an address.
func (c *Channel) Describe() []a2a.DescLine { return c.stream.DescribeTasks(c.identity) }

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

// nopWriter discards a log a caller did not ask for. A channel with no logger still
// logs, since every line it writes is about work it has accepted from the network.
type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
