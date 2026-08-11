//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// Package asyncjobs hosts an agent behind a Choria asyncjobs work queue: it takes a
// job, runs the a2a request its payload carries, and stores the answer on the job's
// own task.
//
// The payload is a bare io.choria.fisk-ai.v1.request message and the answer is the
// task's own result, a v1 result or error message. Nothing of ours is in the
// submission path: a caller enqueues with the asyncjobs client and reads the answer
// off the task, so the queue name, the task type and the payload shape are a
// published contract and this worker's validation is the only thing between a
// caller's mistake and a confusing failure.
//
// The engine already enforces most of what a queue binding needs. A queue's
// MaxRunTime is the consumer's AckWait and its MaxTries the maximum delivery count,
// so the visibility window and the retry cap are server-side. What is here is the
// adapter between an engine that pushes and a serve.Channel that is pulled from, and
// the decisions about which ending each outcome earns.
//
// # Lifecycle
//
// The engine's processor and the server are two goroutines that must stop in one
// order. Construct the channel, serve it, cancel the server's context, wait for
// Serve to return, and only then Close:
//
//	ch, err := asyncjobs.New(asyncjobs.Options{...})
//	srv, err := serve.New(serve.Options{
//		Channels:    []serve.Channel{ch},
//		Concurrency: ch.Concurrency(),
//	})
//	err = srv.Serve(ctx)
//	err = ch.Close()
//
// Closing before Serve returns would cancel the processor while runs are still
// finishing, and a run's answer is stored on the processor's context after the
// handler returns. Nothing this package exposes can report that store, so the
// ordering is the guarantee.
package asyncjobs

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/choria-io/asyncjobs"
	"github.com/nats-io/nats.go"

	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/serve"
)

const (
	// defaultMaxPayload bounds a task payload before it is decoded. A request is a
	// prompt and its supporting context, and the payload rides base64 encoded inside
	// the task record, so this leaves room under the NATS default maximum for the
	// record around it.
	defaultMaxPayload = 512 * 1024

	// renewDivisor sets the renewal interval as a fraction of the queue's MaxRunTime,
	// so the lease is refreshed with a whole interval to spare.
	renewDivisor = 2

	// minRenewInterval floors the renewal interval. A queue configured with an AckWait
	// short enough to derive less than this would have the worker spending its time
	// renewing.
	minRenewInterval = 250 * time.Millisecond
)

// Options configures a Channel.
type Options struct {
	// Conn is the NATS connection the engine uses, already established and owned by
	// the caller: the channel never closes it. It must have been created with
	// nats.UseOldRequestStyle(), which is what the asyncjobs client requires.
	Conn *nats.Conn

	// Queue is the work queue to consume, which must already exist. The channel binds
	// to it and never creates it, so its MaxRunTime, MaxTries and MaxConcurrent are the
	// operator's to set and are read from the bound consumer at construction.
	Queue string

	// TaskType is the asyncjobs task type this channel handles. A task of any other
	// type on the same queue is not this channel's and is left to the engine's
	// no-handler path.
	TaskType string

	// Identity is the agent name stamped as the sender of the result or error message
	// written back to the task. It is normally the configured agent identity.
	Identity string

	// Concurrency is how many jobs the engine may have in flight. It must equal the
	// concurrency of the Server this channel is given to, and the way to make sure of
	// that is to read it back with the Concurrency method rather than to write the
	// number twice.
	//
	// The equality is load bearing and nothing can enforce it. An item the engine has
	// claimed but the server has not started is an item whose lease is running with no
	// work against it; when it lapses the job is delivered again, and the engine admits
	// the second delivery because the already-active guard has expired. So the failure
	// of this invariant is duplicated work rather than delayed work.
	Concurrency int

	// SuspendRequested is handed to every run and polled at a loop boundary, so a
	// worker draining stops its runs where they can resume from. A suspended run is
	// naked and redelivered, and resumes from its journal. Nil never suspends, which
	// makes a drain a hard stop at whatever the runs were doing.
	SuspendRequested func() bool

	// MaxPayload bounds a task payload before it is decoded; <= 0 uses the default.
	MaxPayload int

	// Logger receives structured progress, and the engine's own logging is bridged
	// into it so a worker's output is one format. Nil builds a text logger on stderr.
	Logger *slog.Logger
}

func (o *Options) validate() error {
	if o.Conn == nil {
		return fmt.Errorf("a NATS connection is required")
	}
	if !o.Conn.Opts.UseOldRequestStyle {
		return fmt.Errorf("the NATS connection must be established with nats.UseOldRequestStyle(), which the asyncjobs client requires")
	}
	if o.Queue == "" {
		return fmt.Errorf("a queue name is required")
	}
	if o.TaskType == "" {
		return fmt.Errorf("a task type is required")
	}
	if o.Identity == "" {
		return fmt.Errorf("an identity is required: it is the sender of every answer this channel writes back")
	}
	if o.Concurrency <= 0 {
		return fmt.Errorf("concurrency must be greater than zero and must equal the server's")
	}

	return nil
}

// Channel is a serve.Channel over a Choria asyncjobs work queue.
type Channel struct {
	name        string
	identity    string
	concurrency int
	maxPayload  int
	renewEvery  time.Duration
	suspend     func() bool

	client    *asyncjobs.Client
	router    *asyncjobs.Mux
	validator *a2a.Validator
	log       *slog.Logger

	// work hands one job from its handler to Next. It is deliberately unbuffered: a
	// buffer would hold jobs that are claimed, whose leases are running, and which
	// nothing is working on.
	work chan *serve.Work

	startOnce sync.Once
	running   atomic.Bool
	procCtx   context.Context
	procStop  context.CancelFunc
	procDone  chan struct{}
	procErr   error

	closeOnce sync.Once
	shutdown  chan struct{}
	handlers  sync.WaitGroup
}

// New binds to the work queue and returns a Channel. It reaches the network: the
// queue must already exist, and its settings are read from the bound consumer here so
// a wrong queue name fails at construction rather than on the first job.
//
// It starts nothing. The engine's processor starts on the first call to Next, so a
// channel that is constructed and never served claims no work.
func New(opts Options) (*Channel, error) {
	err := opts.validate()
	if err != nil {
		return nil, err
	}

	validator, err := a2a.NewValidator()
	if err != nil {
		return nil, fmt.Errorf("compiling the a2a schemas: %w", err)
	}

	log := opts.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	log = log.With("channel", "asyncjobs/"+opts.Queue)

	maxPayload := opts.MaxPayload
	if maxPayload <= 0 {
		maxPayload = defaultMaxPayload
	}

	// The queue value is ours to hold rather than the client's, because binding fills
	// its MaxRunTime in from the consumer's AckWait and that is what the renewal
	// interval is derived from. BindWorkQueue would do the same binding but keeps the
	// value private.
	queue := &asyncjobs.Queue{Name: opts.Queue, NoCreate: true}

	client, err := asyncjobs.NewClient(
		asyncjobs.NatsConn(opts.Conn),
		asyncjobs.WorkQueue(queue),
		asyncjobs.ClientConcurrency(opts.Concurrency),
		asyncjobs.CustomLogger(&logBridge{log: log.With("component", "asyncjobs")}),
	)
	if err != nil {
		return nil, fmt.Errorf("connecting to queue %q: %w", opts.Queue, err)
	}

	if queue.MaxRunTime <= 0 {
		return nil, fmt.Errorf("queue %q reported no maximum run time, so a lease cannot be renewed against it", opts.Queue)
	}

	renewEvery := max(queue.MaxRunTime/renewDivisor, minRenewInterval)

	c := &Channel{
		name:        "asyncjobs/" + opts.Queue,
		identity:    opts.Identity,
		concurrency: opts.Concurrency,
		maxPayload:  maxPayload,
		renewEvery:  renewEvery,
		suspend:     opts.SuspendRequested,
		client:      client,
		validator:   validator,
		log:         log,
		work:        make(chan *serve.Work),
		procDone:    make(chan struct{}),
		shutdown:    make(chan struct{}),
	}

	c.procCtx, c.procStop = context.WithCancel(context.Background())

	c.router = asyncjobs.NewTaskRouter()
	err = c.router.HandleFunc(opts.TaskType, c.handle)
	if err != nil {
		return nil, fmt.Errorf("registering the handler for task type %q: %w", opts.TaskType, err)
	}

	log.Info("Bound to work queue",
		"queue", opts.Queue,
		"task_type", opts.TaskType,
		"max_run_time", queue.MaxRunTime,
		"max_tries", queue.MaxTries,
		"max_concurrent", queue.MaxConcurrent,
		"concurrency", opts.Concurrency,
		"renew_every", renewEvery)

	return c, nil
}

// Name identifies the channel in the server's logs.
func (c *Channel) Name() string { return c.name }

// Concurrency is how many jobs this channel may have in flight, and therefore the
// value serve.Options.Concurrency must be set to. See Options.Concurrency for what
// goes wrong when the two disagree.
func (c *Channel) Concurrency() int { return c.concurrency }

// Next blocks until a job is available and returns it as work.
//
// It starts the engine's processor on its first call, so nothing is claimed before
// the server is ready to run it. It returns serve.ErrChannelDone once the processor
// has stopped, which is what keeps the server from waiting on a channel that can
// never produce work again.
func (c *Channel) Next(ctx context.Context) (*serve.Work, error) {
	c.start()

	select {
	case w := <-c.work:
		return w, nil
	case <-c.procDone:
		return nil, serve.ErrChannelDone
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Close stops the engine's processor and waits for it.
//
// Call it after Serve has returned, never before: a run that finishes has its answer
// stored on the processor's context after its handler returns, so a processor stopped
// while runs are still finishing loses answers that were paid for. See the package
// documentation for the ordering.
//
// A job that was claimed but never handed to the server is naked here rather than
// left to sit out its lease, so it returns to the queue promptly.
func (c *Channel) Close() error {
	c.closeOnce.Do(func() {
		// Waiting handlers return first, so their nak is issued while the processor's
		// context is still live. A handler that is dispatched during this window takes
		// the same path and may find that context already canceled; its nak then fails
		// and the job waits out its lease instead, which is the same job returning to
		// the queue by a slower route.
		close(c.shutdown)
		c.handlers.Wait()
		c.procStop()
	})

	if !c.running.Load() {
		return nil
	}

	<-c.procDone

	return c.procErr
}

// start launches the engine's processor once, on the channel's own context rather
// than a caller's, which is what lets it outlive the server.
func (c *Channel) start() {
	c.startOnce.Do(func() {
		c.running.Store(true)

		go func() {
			defer close(c.procDone)

			c.procErr = c.client.Run(c.procCtx, c.router)
			if c.procErr != nil {
				c.log.Error("The work queue processor stopped", "error", c.procErr)
			}
		}()
	})
}

// logBridge carries the engine's printf logging into the channel's structured
// logger, so a worker's output is one format on one stream.
type logBridge struct {
	log *slog.Logger
}

func (l *logBridge) Debugf(format string, v ...any) { l.log.Debug(fmt.Sprintf(format, v...)) }
func (l *logBridge) Infof(format string, v ...any)  { l.log.Info(fmt.Sprintf(format, v...)) }
func (l *logBridge) Warnf(format string, v ...any)  { l.log.Warn(fmt.Sprintf(format, v...)) }
func (l *logBridge) Errorf(format string, v ...any) { l.log.Error(fmt.Sprintf(format, v...)) }
