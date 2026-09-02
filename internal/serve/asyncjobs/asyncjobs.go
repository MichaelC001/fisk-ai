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
// The engine's processor and the server are two goroutines that have to stop
// together, and there are two ways to ask for it.
//
//	ch, err := asyncjobs.New(asyncjobs.Options{...})
//	srv, err := serve.New(serve.Options{Channels: []serve.Channel{ch}})
//
// To drain, Close while the server is still running. Jobs claimed but not started go
// back to the queue at once, jobs already running are waited for, and Serve ends by
// itself because a finished channel says so. Nothing is canceled, so a run stops at a
// boundary it can be resumed from. With nothing in flight it returns immediately.
//
//	err = ch.Close()      // from a signal handler, while Serve runs
//	err = srv.Serve(ctx)  // returns on its own
//
// To stop, cancel the server's context, wait for Serve, and then Close. Runs end
// wherever they had reached and their jobs go back to the queue.
//
//	cancel()
//	err = srv.Serve(ctx)
//	err = ch.Close()
//
// Close is idempotent, so a caller that drains on one signal and stops on the next
// calls it on both paths.
//
// # What a drain can still lose, and why it costs little
//
// Close stops the processor only once every handler has returned, which is as close as
// this package can get to safe. It is not all the way: the engine stores the answer and
// acknowledges the queue item *after* the handler returns, on the processor's context,
// so a Close that lands in that gap cancels the store of whatever finished last. The
// handler returning is what triggers the store rather than something that follows it,
// and nothing a handler can observe reflects whether it happened.
//
// The job is then neither answered on its task nor acknowledged, so its lease lapses
// and it is delivered again. What it does not cost is the work. Every run is
// checkpointed under the session its task id derives (see SessionFor), so the redelivery
// finds a completed session and is
// answered from its journal without calling a model: see agent.Checkpoint.CreateIfMissing.
// The price of the window is one delivery cycle, bounded by the queue's run time, not a
// lost answer and not a second run.
//
// Closing it entirely is the engine's to do, as a client drain that stops polling and
// finishes the items already in flight. Until then this is a documented window rather
// than a guarantee.
package asyncjobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/choria-io/asyncjobs"
	"github.com/nats-io/nats.go"

	wire "github.com/choria-io/fisk-ai/internal/a2a/wire/v1"
	"github.com/choria-io/fisk-ai/internal/conns"
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

// errProcessorStopped is the fault reported when the engine's processor returned
// without an error and without anything asking it to stop. The channel can claim no
// further job after it, so the server ends rather than running on.
var errProcessorStopped = errors.New("the queue processor stopped without being asked")

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

	// Concurrency is how many jobs the engine may claim at once, which is how many
	// runs happen at once. The server reads it back through the Concurrency method and
	// sizes this channel's slot budget from it rather than from its own configured
	// default.
	Concurrency int

	// SuspendRequested is handed to every run and polled at a loop boundary, so a
	// worker draining stops its runs where they can resume from. A suspended run is
	// naked and redelivered, and resumes from its journal. Nil never suspends, which
	// makes a drain a hard stop at whatever the runs were doing.
	SuspendRequested func() bool

	// MaxPayload bounds a task payload before it is decoded; <= 0 uses the default.
	MaxPayload int

	// Logger receives structured progress, and the engine's own logging is bridged
	// into it so a worker's output is one format. Nil discards both, since a library
	// that reached for a default logger would write to an embedder's stderr uninvited.
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
		return fmt.Errorf("concurrency must be greater than zero")
	}

	return nil
}

// A queue claims work before a run starts, holds a connection, and has a processor that
// can stop without being asked, so it is three of the optional shapes a channel can
// have. Declaring them makes a change to any of those contracts a compile error here
// rather than a channel the server silently stops asking.
var (
	_ serve.ConcurrentChannel = (*Channel)(nil)
	_ serve.ReleasableChannel = (*Channel)(nil)
	_ serve.FaultingEndpoint  = (*Channel)(nil)
)

// SessionFor is the journal a job runs in, derived from the serving identity and the task
// id rather than taken from the task id itself.
//
// A job creates a session or resumes one an earlier delivery of the same task made, and
// nothing else. Handing the store a submitter's own bytes would let a task name any journal
// this worker holds, and a journal id is not a secret: it is logged, and a deferred run's
// terminal message carries it. A submitter that learned the id of a prompts conversation
// could then run this agent's tools against that conversation's history and its standing
// approvals, or read its stored answer back without anything running.
//
// The hash is deterministic on the task id the submitter already chose, so at-least-once
// delivery and Client.RetryTaskByID both land in the same journal.
//
// It is exported for a caller that also holds the store and wants the journal a job of its
// own reached. It says nothing about a journal existing.
func SessionFor(identity, taskID string) string {
	sum := sha256.Sum256([]byte(identity + "\x00" + taskID))

	return "j-" + hex.EncodeToString(sum[:])
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
	validator *wire.Validator
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

	// faults carries the report that the processor stopped without being asked. It is
	// buffered by one and written at most once, by the goroutine start launches, since
	// Serve reads it once and the first fault is what ends the worker.
	faults chan error

	closeOnce sync.Once
	shutdown  chan struct{}
	handlers  sync.WaitGroup

	// ownConn is the connection this channel dialed for itself, which Close releases.
	// It is nil when the connection came in on Options and belongs to the caller. A nil
	// Provider's Close is a no-op, so nothing here has to ask which it is.
	ownConn  *conns.Provider
	connOnce sync.Once
}

// New binds to the work queue and returns a Channel. It reaches the network, and it
// creates nothing: the queue, the task store and the engine's configuration buckets
// must all already exist, so a cluster nobody has provisioned fails here rather than
// being laid out by whichever worker started first. The queue's settings are read
// from the bound consumer at the same time, so a wrong queue name also fails at
// construction rather than on the first job.
//
// It starts nothing. The engine's processor starts on the first call to Next, so a
// channel that is constructed and never served claims no work.
func New(opts Options) (*Channel, error) {
	err := opts.validate()
	if err != nil {
		return nil, err
	}

	validator, err := wire.SharedValidator()
	if err != nil {
		return nil, fmt.Errorf("compiling the a2a schemas: %w", err)
	}

	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
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
		// The operator owns the storage layout, all of it. Without this the client
		// creates the task store and its configuration buckets on the way past, at one
		// replica and with whatever retention it was built with, so the first worker to
		// start against an unprovisioned cluster would quietly decide how every answer
		// is kept.
		asyncjobs.NoStorageCreate(),
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
		faults:      make(chan error, 1),
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

// Concurrency is how many jobs this channel may have in flight, which the server takes
// as this channel's slot budget. See Options.Concurrency.
func (c *Channel) Concurrency() int { return c.concurrency }

// Faults reports that the engine's processor stopped for a reason nobody asked for,
// leaving this channel unable to claim another job. Serve drains what is in flight and
// returns the error, so the worker exits non-zero and a supervisor restarts it.
//
// Close ends the channel after a fault, as on every other path. The server's drain
// calls it; a caller pulling Next itself reads this and calls it. A caller that reads
// neither leaves Next blocked for the life of the process, since a faulted processor
// closes nothing Next selects on.
//
// Close cancels the processor's context, so a drain and a stop each end the processor
// without a fault.
func (c *Channel) Faults() <-chan error { return c.faults }

// Next blocks until a job is available and returns it as work.
//
// It starts the engine's processor on its first call, so nothing is claimed before
// the server is ready to run it. It returns serve.ErrChannelDone once Close has
// stopped the processor, which is what keeps the server from waiting on a channel that
// can never produce work again. A processor that stopped without being asked arrives on
// Faults instead, and Next goes on blocking until Close, which is how the server reads
// that fault before its puller exits and returns the error from Serve.
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

// Close drains the channel and stops the engine's processor: a job claimed but never
// handed to the server is naked at once rather than left to sit out its lease, a job
// already handed over is waited for, and only then does the processor stop. It is safe
// to call while the server is still running, which is how a drain is asked for, and
// safe after Serve has returned, which is how a stop finishes. See the package
// documentation for both.
//
// It waits as long as the runs in flight do, so a caller draining from a signal handler
// runs it on a goroutine of its own.
//
// It is idempotent and returns the same answer to every caller.
func (c *Channel) Close() error {
	c.closeOnce.Do(func() {
		// Waiting handlers return first, so their nak is issued while the processor's
		// context is still live. The engine issues that nak after the handler returns,
		// though, so a handler still unwinding when the context is canceled below has
		// its nak fail and its job waits out the lease instead. Same job, slower route.
		// See the package documentation for the same window on the answering path.
		close(c.shutdown)
		c.handlers.Wait()
		c.procStop()

		// A channel the server never pulled from has no processor to wait for, so the
		// connection is released here rather than after procDone below, which that
		// channel never reaches.
		if !c.running.Load() {
			c.releaseConn()
		}
	})

	if !c.running.Load() {
		return nil
	}

	<-c.procDone
	c.releaseConn()

	return c.procErr
}

// releaseConn closes the connection this channel dialed for itself, once. A borrowed
// connection is the caller's and is left alone.
func (c *Channel) releaseConn() {
	c.connOnce.Do(func() {
		c.ownConn.Close()
	})
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

			// Close closes shutdown before it cancels the processor's context, so a
			// processor that has returned while shutdown is still open stopped for a
			// reason nobody here asked for. Reporting a drain would exit a worker
			// non-zero for shutting down cleanly.
			select {
			case <-c.shutdown:
				return
			default:
			}

			err := c.procErr
			if err == nil {
				err = errProcessorStopped
			}

			c.faults <- err

			// The server reads Faults on a goroutine of its own and answers a fault by
			// draining, which is what closes shutdown here. Closing procDone before that
			// would end the puller while the fault was still in flight, and Serve returns
			// nil when its channels have all ended and no fault has reached it.
			<-c.shutdown
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
