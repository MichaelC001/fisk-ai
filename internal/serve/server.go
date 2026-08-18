//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/segmentio/ksuid"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/conns"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/memory"
	"github.com/choria-io/fisk-ai/internal/rag"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/telemetry"
)

const (
	// defaultConcurrency bounds how many runs execute at once. It is small because a
	// run is expensive: a model call plus tool subprocesses, not a request handler.
	defaultConcurrency = 2

	// channelRetryDelay is how long a puller waits after a channel returns an
	// unexpected error, so an unreachable queue does not become a hot loop.
	channelRetryDelay = 5 * time.Second

	// doneTimeout bounds reporting one outcome. A channel that cannot reach its store
	// must not hold up a shutdown indefinitely.
	doneTimeout = 30 * time.Second
)

// DefaultToolTimeout bounds one tool call for a run this server hosts, when the
// configuration sets none. It is far longer than the 30 seconds the MCP and a2a
// servers allow a served call, because those answer a caller who is waiting while this
// hosts whole units of work whose commands legitimately take minutes. It is a bound on
// a tool that will never answer, not on a slow one.
//
// It is exported because a caller reporting what its worker will do has to be able to
// name the value it did not set, and a startup line saying a bound is zero when it is
// five minutes is worse than saying nothing.
const DefaultToolTimeout = 5 * time.Minute

// Options configures a Server.
//
// The shared resources are the ones agent.Options already defines for running many
// agents in one process. They are passed to every run rather than rebuilt per run,
// and their contracts are the ones documented there.
type Options struct {
	// Channels supply the work. Each gets its own puller. At least one channel or one
	// service is required.
	//
	// The Server releases them: Drain and Stop close the ones that can be closed, and
	// New closes them itself if it refuses these options. Handing them over is therefore
	// handing over their lifecycle, which is what a channel holding a connection needs
	// from a constructor that may fail.
	Channels []Channel

	// Services are the endpoints that answer their callers directly rather than
	// producing work. They are already answering when they arrive here, so they are
	// released on the same terms as the channels and for a stronger reason: a service
	// left open by a constructor that failed is a live endpoint in a process that serves
	// nothing.
	//
	// A server with services and no channels has nothing to pull, so Serve holds itself
	// open until it is drained, stopped or canceled.
	Services []Service

	// Config is the parsed agent configuration every run uses. Required.
	Config *config.Config
	// ConfigFile names the file Config was read from, for diagnostics.
	ConfigFile string

	// Concurrency is how many runs a channel may have executing at once; <= 0 uses
	// the default. It is the default rather than the total: a channel that has an
	// opinion of its own states it through ConcurrentChannel and gets that instead, so
	// a process serving two channels at two runs each is running four.
	//
	// It is per channel because a channel whose work carries a lease claims that work
	// before the run starts, and it can only size its claiming to a bound it owns. A
	// shared bound would have one channel's runs deciding how much of another's claimed
	// work can proceed, and the queue channel would hold claims against a queue-wide
	// budget for jobs it is not running.
	Concurrency int

	// ToolTimeout bounds a single tool call in the runs this server hosts; <= 0 uses
	// the default. It applies only when the configuration sets no
	// harness.tool_timeout, and a configured value wins even when it is longer. That
	// is the opposite precedence to Budget, and deliberately: a budget clamps what a
	// channel asks for, which is a caller this server does not control, while both of
	// these come from whoever started it.
	//
	// A configuration read from a file arrives with the harness default already filled,
	// so this covers a Config built in process, which never runs prepare. The two
	// values agree, so which one a run gets is not something an operator can observe.
	// See config.Config.ToolTimeout for what the bound can and cannot stop.
	ToolTimeout time.Duration

	// WorkDir is the directory command tools run in. It must be an absolute path that
	// already exists. Empty inherits the process working directory, which is what a
	// run at a terminal does.
	//
	// Every run shares it. Giving each run a directory of its own would keep a tool
	// writing a relative path off a sibling run's file, but an application that
	// resolves anything relative to where it was started could then never find it, and
	// that is the ordinary case rather than the exotic one. A tool that mutates local
	// state is responsible for doing so safely under concurrency; lower Concurrency to
	// 1 if it cannot.
	WorkDir string

	// LogOutput is the sink for the default Logger; nil means os.Stderr. It is
	// ignored when Logger is supplied.
	LogOutput io.Writer
	// Logger receives structured progress; nil builds a text logger over LogOutput.
	Logger *slog.Logger

	// APIKey and BaseURL override the model provider's credentials and endpoint, as
	// they do on agent.Options.
	APIKey  string
	BaseURL string

	// Provider, when non-nil, is the llm.Provider every run uses.
	//
	// Unlike the other shared resources here, llm.Provider states no concurrency
	// contract of its own, so this is where the requirement is made: a Provider given
	// to a Server must be safe for concurrent use, because every run calls it. Leaving
	// it nil builds one per run from the configuration, which is correct but gives a
	// long-lived process no connection reuse.
	Provider llm.Provider

	// Conns, RAGStore, MemoryStore, SessionStore and A2ATransport are shared across
	// every run. Each must be safe for concurrent use and is owned by the caller: the
	// server uses them and never closes them. See agent.Options for what each one
	// changes when it is nil.
	Conns        *conns.Provider
	RAGStore     *rag.Store
	MemoryStore  memory.Store
	SessionStore runstate.Store
	A2ATransport a2a.Transport

	// StoreDir is the base directory the persistent stores resolve their paths
	// under. Unlike the per-run tool working directory it is shared, since those
	// stores belong to an identity rather than to a run.
	StoreDir string

	// Telemetry, when non-nil, receives every run's traces and metrics. The caller
	// owns its lifecycle and must shut it down after Serve returns, on a context not
	// derived from the one Serve was given.
	Telemetry *telemetry.Provider
}

func (o *Options) applyDefaults() {
	if o.Concurrency <= 0 {
		o.Concurrency = defaultConcurrency
	}
	if o.ToolTimeout <= 0 {
		o.ToolTimeout = DefaultToolTimeout
	}
	if o.LogOutput == nil {
		o.LogOutput = os.Stderr
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewTextHandler(o.LogOutput, nil))
	}
}

func (o *Options) validate() error {
	if len(o.Channels) == 0 && len(o.Services) == 0 {
		return fmt.Errorf("at least one channel or service is required")
	}
	if o.Config == nil {
		return fmt.Errorf("a configuration is required")
	}

	for i, ch := range o.Channels {
		if ch == nil {
			return fmt.Errorf("channel %d is nil", i)
		}
		if ch.Name() == "" {
			return fmt.Errorf("channel %d has no name", i)
		}
	}

	for i, svc := range o.Services {
		if svc == nil {
			return fmt.Errorf("service %d is nil", i)
		}
		if svc.Name() == "" {
			return fmt.Errorf("service %d has no name", i)
		}
	}

	if o.WorkDir != "" {
		if !filepath.IsAbs(o.WorkDir) {
			return fmt.Errorf("work directory %q is not an absolute path", o.WorkDir)
		}
		st, err := os.Stat(o.WorkDir)
		if err != nil {
			return fmt.Errorf("work directory %q: %w", o.WorkDir, err)
		}
		if !st.IsDir() {
			return fmt.Errorf("work directory %q is not a directory", o.WorkDir)
		}
	}

	return nil
}

// Server takes work from its channels and runs it, each channel bounded by its own
// concurrency, and hosts its services for as long as it serves.
type Server struct {
	opts Options
	log  *slog.Logger

	// released is closed by the first release, so a server holding itself open for its
	// services stops holding when it is drained or stopped. The Once is what makes the
	// ordinary drain-then-stop sequence two calls and one close.
	released    chan struct{}
	releaseOnce sync.Once
}

// New validates the options and returns a Server. It starts nothing; Serve does the
// work.
//
// A Server owns releasing its endpoints, through Drain and Stop. New therefore releases
// them itself when it refuses the options, since a caller holding an error holds no
// Server and has nothing to release them with: several channels own a connection and
// every service is already answering, and there is no third outcome where the caller is
// expected to clean up after a constructor that failed. Either this returns a Server
// that will release them, or it returns an error having released them.
func New(opts Options) (*Server, error) {
	opts.applyDefaults()

	err := opts.validate()
	if err != nil {
		releaseEndpoints(opts.Channels, opts.Services, opts.Logger)
		return nil, err
	}

	return &Server{
		opts:     opts,
		log:      opts.Logger,
		released: make(chan struct{}),
	}, nil
}

// concurrencyFor is how many runs a channel may have at once: its own answer when it
// has one, and the configured default when it does not.
func (s *Server) concurrencyFor(ch Channel) int {
	cc, ok := ch.(ConcurrentChannel)
	if !ok {
		return s.opts.Concurrency
	}

	n := cc.Concurrency()
	if n <= 0 {
		return s.opts.Concurrency
	}

	return n
}

// Serve runs every channel until ctx is canceled or all of them are finished, and
// does not return until each run it started has ended and reported its outcome.
//
// Waiting matters: the shared stores, the provider and the telemetry provider belong
// to the caller and are closed once Serve returns, so returning while a run is still
// using one would pull it out from under that run.
//
// A server with services and no channels has no puller to wait for. It holds itself
// open instead, until it is drained, stopped or canceled, since its services answer
// for as long as they are registered and returning here would have the caller release
// them from under their callers.
//
// A server that has a channel is bounded by its channels either way. One that ends,
// including one that ends because whatever produces its work failed, ends Serve and
// leaves the program to report it, which is what lets a supervisor restart a worker
// whose queue died rather than leaving it alive and taking nothing.
func (s *Server) Serve(ctx context.Context) error {
	var wg sync.WaitGroup

	s.log.Info("Serving", "channels", len(s.opts.Channels), "services", len(s.opts.Services))

	for _, svc := range s.opts.Services {
		s.log.Info("Serving service", "service", svc.Name())
	}

	// A fault is watched beside the channels rather than among them: it ends a server
	// with channels as well as one without, since a channel blocked in Next has no
	// reason to return and the drain is what makes it. It is deliberately not part of
	// the wait group, whose members are what Serve waits for; this watcher is what
	// Serve releases when it is done.
	faulted := make(chan error, 1)
	watching := make(chan struct{})
	defer close(watching)

	go s.watchFaults(ctx, faulted, watching)

	if len(s.opts.Channels) == 0 {
		wg.Go(func() {
			select {
			case <-ctx.Done():
			case <-s.released:
			}
		})
	}

	// One counter covers both the pullers and the runs they spawn. A puller only
	// adds while it is itself counted, so waiting here cannot miss a run that was
	// about to start.
	//
	// Each channel gets a slot budget of its own, so one channel's runs never decide
	// how much of another's claimed work may proceed.
	for _, ch := range s.opts.Channels {
		concurrency := s.concurrencyFor(ch)
		sem := make(chan struct{}, concurrency)

		s.log.Info("Serving channel", "channel", ch.Name(), "concurrency", concurrency)

		wg.Go(func() {
			s.pull(ctx, &wg, ch, sem)
		})
	}

	wg.Wait()
	s.log.Info("Stopped serving")

	select {
	case err := <-faulted:
		return err
	default:
	}

	return nil
}

// watchFaults ends the server when an endpoint reports that it has stopped working.
//
// The drain is what ends the channels: a channel blocked in Next returns
// ErrChannelDone once it is released, and the runs in flight finish rather than being
// abandoned, since an endpoint that cannot be called no longer affects work that is
// already running.
//
// It returns when Serve does, however Serve ended, so neither it nor the readers it
// starts outlive the server they watch.
func (s *Server) watchFaults(ctx context.Context, faulted chan<- error, done <-chan struct{}) {
	sources := make([]<-chan error, 0, len(s.opts.Channels)+len(s.opts.Services))

	for _, ch := range s.opts.Channels {
		f, ok := ch.(FaultingEndpoint)
		if ok {
			sources = append(sources, f.Faults())
		}
	}
	for _, svc := range s.opts.Services {
		f, ok := svc.(FaultingEndpoint)
		if ok {
			sources = append(sources, f.Faults())
		}
	}

	if len(sources) == 0 {
		return
	}

	// One goroutine per source rather than reflect.Select: the count is the number of
	// endpoints a process hosts, and the first fault is the one that ends the server.
	first := make(chan error, len(sources))
	for _, src := range sources {
		go func() {
			select {
			case err, ok := <-src:
				if ok && err != nil {
					first <- err
				}
			case <-done:
			}
		}()
	}

	select {
	case err := <-first:
		s.log.Error("A endpoint stopped working; draining", "error", err)
		faulted <- err

		derr := s.Drain()
		if derr != nil {
			s.log.Error("Draining after an endpoint fault failed", "error", derr)
		}

	case <-ctx.Done():
	case <-s.released:
	case <-done:
	}
}

// Drain asks the endpoints to stop taking callers and waits for what is already in
// flight, so Serve ends by itself with nothing canceled and every run stopped at a
// point it can be resumed from. It is called while Serve is still running, which is
// what makes it a drain rather than a stop.
//
// It blocks for as long as the work in flight does, so a caller draining from a signal
// handler runs it on a goroutine of its own. A channel that does not implement
// ReleasableChannel has nothing to drain and is left alone, and a server whose channels
// are all like that returns at once.
//
// The services are closed here rather than at the end, so an endpoint answering callers
// directly stops when the worker starts shutting down instead of when it finishes. What
// is not covered is a call already in flight: a service answers on goroutines of its
// own that nothing here can see, so closing it stops the next caller rather than the
// current one.
//
// Signals are not this package's business. A library that called signal.Notify would
// take SIGTERM from an embedder's supervisor with no way to decline, so the two verbs
// are offered and the program decides which signal means which.
func (s *Server) Drain() error {
	return s.release("Draining endpoint")
}

// release closes every endpoint that can be, saying which phase it is doing it for. A
// endpoint's Close is idempotent, so the second call in a drain-then-stop sequence does
// nothing; it is logged differently rather than twice so the output does not read as
// two drains.
//
// Closing the hold is the first thing it does and happens once however often this is
// called, so a server waiting on its services stops waiting whichever verb the program
// reached for.
func (s *Server) release(reason string) error {
	s.releaseOnce.Do(func() { close(s.released) })

	var errs []error

	for _, ch := range s.opts.Channels {
		// A channel that can be released says so by implementing ReleasableChannel; the
		// interface every channel must honor stays at Channel and asks nothing of the
		// ones holding nothing.
		closer, ok := ch.(ReleasableChannel)
		if !ok {
			continue
		}

		s.log.Debug(reason, "channel", ch.Name())

		err := closer.Close()
		if err != nil {
			errs = append(errs, fmt.Errorf("releasing %s: %w", ch.Name(), err))
		}
	}

	for _, svc := range s.opts.Services {
		s.log.Debug(reason, "service", svc.Name())

		err := svc.Close()
		if err != nil {
			errs = append(errs, fmt.Errorf("releasing %s: %w", svc.Name(), err))
		}
	}

	return errors.Join(errs...)
}

// Stop releases the endpoints after Serve has returned. It is the same call as Drain
// and differs only in when it is made: before, the runs in flight are waited for;
// after, there are none left to wait for and it is releasing what the endpoints hold.
//
// Calling it after a Drain is safe and is how a program that drains on one signal and
// stops on the next ends up releasing everything exactly once.
func (s *Server) Stop() error {
	return s.release("Releasing endpoint")
}

// pull takes work from one channel until it is finished or the context ends, bounded
// by that channel's own slots.
//
// The order is take, then acquire a slot, then run. Acquiring first would park a slot
// on a channel that has nothing to offer. The cost of this order is that an item is
// claimed while it waits for a slot, so a channel whose work carries a lease must
// tolerate that wait; sizing the slots per channel is what keeps that wait bounded by
// the channel's own load rather than by its neighbours'.
func (s *Server) pull(ctx context.Context, wg *sync.WaitGroup, ch Channel, sem chan struct{}) {
	log := s.log.With("channel", ch.Name())

	for {
		if ctx.Err() != nil {
			return
		}

		work, err := ch.Next(ctx)
		switch {
		case errors.Is(err, ErrChannelDone):
			log.Info("Channel has no more work")
			return
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return
		case err != nil:
			log.Error("Taking work failed", "error", err, "retry_in", channelRetryDelay)
			select {
			case <-ctx.Done():
				return
			case <-time.After(channelRetryDelay):
			}
			continue
		case work == nil:
			log.Error("Channel returned no work and no error")
			continue
		}

		if work.ID == "" {
			work.ID = ksuid.New().String()
		}
		if work.Done == nil {
			log.Error("Discarding work with no way to report its outcome", "work", work.ID)
			continue
		}

		// The slot is acquired here, after the work is in hand, so a run that is
		// waiting is a claimed item rather than an idle puller holding capacity.
		select {
		case <-ctx.Done():
			// Taken but never started, which is a distinct outcome: nothing ran, so
			// whoever supplied it may hand it to another worker unchanged.
			s.report(work, Outcome{ID: work.ID, Abandoned: true}, log)
			return
		case sem <- struct{}{}:
		}

		wg.Go(func() {
			defer func() { <-sem }()
			s.execute(ctx, work, log)
		})
	}
}

// execute runs one piece of work and reports its outcome exactly once.
//
// It carries its own recover because a panic here is on the server's goroutine, which
// agent.Run's own barrier does not cover: Result.Stats is nil on every failure during
// setup, and an unguarded read of it would take the process down and strand the work.
func (s *Server) execute(ctx context.Context, work *Work, log *slog.Logger) {
	log = log.With("work", work.ID)

	out := Outcome{ID: work.ID}

	defer func() {
		r := recover()
		if r != nil {
			log.Error("Serving a run panicked", "panic", r)
			out.Crashed = true
			if out.Err == nil {
				out.Err = fmt.Errorf("internal error while serving the run")
			}
		}

		s.report(work, out, log)
	}()

	events := newEventRecorder(work.Events, log)

	prompter := promptsThrough(work)

	log.Info("Running", "caller", work.Caller.Name, "caller_verified", work.Caller.Verified, "resume", work.Checkpoint.ResumeID)

	runCtx, cancel := s.runContext(ctx, work)
	if cancel != nil {
		defer cancel()
	}

	start := time.Now()
	res, err := agent.Run(runCtx, s.runOptions(work), events, prompter)
	duration := time.Since(start)

	if res != nil {
		out.SessionID = res.SessionID
		out.Text = res.Text
		out.Reason = res.Reason
		out.Stats = res.Stats
		out.Deferred = res.Deferred
		out.FollowUpTaken = res.FollowUpTaken
	}
	out.Err = err

	var panicErr *agent.PanicError
	out.Crashed = errors.As(err, &panicErr)

	switch {
	case out.Crashed:
		log.Error("Run crashed", "duration", duration)
	case err != nil:
		log.Warn("Run failed", "duration", duration, "reason", out.Reason, "error", err)
	default:
		log.Info("Run completed", "duration", duration, "reason", out.Reason)
	}
}

// runContext derives the context one run executes under, asking the channel when it
// supplied a hook and leaving the work on the server's context when it did not.
//
// A channel that answers with nothing usable is corrected rather than obeyed: a nil
// context would panic the run and a nil cancel would panic the defer that calls it,
// and neither is worth a stranded piece of work and a dead worker.
func (s *Server) runContext(ctx context.Context, work *Work) (context.Context, context.CancelFunc) {
	if work.RunContext == nil {
		return ctx, nil
	}

	runCtx, cancel := work.RunContext(ctx)
	if runCtx == nil {
		runCtx = ctx
	}

	return runCtx, cancel
}

// runOptions assembles the agent options for one piece of work: the channel's
// attachment points and the shared resources.
func (s *Server) runOptions(work *Work) agent.Options {
	opts := agent.Options{
		Config:           s.withToolTimeout(s.clampedConfig(work.Budget)),
		ConfigFile:       s.opts.ConfigFile,
		Prompt:           []string{work.Prompt},
		APIKey:           s.opts.APIKey,
		BaseURL:          s.opts.BaseURL,
		Checkpoint:       work.Checkpoint,
		ClaimedBy:        work.ClaimedBy,
		SuspendRequested: work.SuspendRequested,
		NextPrompt:       work.Continue,
		Provider:         s.opts.Provider,
		ToolWorkDir:      s.opts.WorkDir,
		StoreDir:         s.opts.StoreDir,
		Conns:            s.opts.Conns,
		RAGStore:         s.opts.RAGStore,
		MemoryStore:      s.opts.MemoryStore,
		SessionStore:     s.opts.SessionStore,
		A2ATransport:     s.opts.A2ATransport,
		Telemetry:        s.opts.Telemetry,
	}

	if work.Context != "" {
		opts.Prompt = append(opts.Prompt, work.Context)
	}

	// Filled here rather than by each channel, which said who its caller was once when
	// it built the work. It reaches the journal's Meta record where the run creates one,
	// so an operator reading the store can tell whose conversation it is.
	opts.Checkpoint.Caller = work.Caller.Name

	return opts
}

// clampedConfig applies a piece of work's budget to a copy of the configuration.
//
// The copy is shallow, which is safe because the budget fields are values and nothing
// in the run path writes to the configuration. Local configuration is the ceiling: an
// unset field on the work leaves the configured limit alone, and a value above it is
// ignored rather than honored.
func (s *Server) clampedConfig(budget Budget) *config.Config {
	if budget.MaxTokens <= 0 && budget.MaxIterations <= 0 {
		return s.opts.Config
	}

	cfg := *s.opts.Config

	if budget.MaxTokens > 0 && (cfg.LLM.Budget.MaxTokens <= 0 || budget.MaxTokens < cfg.LLM.Budget.MaxTokens) {
		cfg.LLM.Budget.MaxTokens = budget.MaxTokens
	}
	if budget.MaxIterations > 0 && (cfg.LLM.Budget.MaxIterations <= 0 || budget.MaxIterations < cfg.LLM.Budget.MaxIterations) {
		cfg.LLM.Budget.MaxIterations = budget.MaxIterations
	}

	return &cfg
}

// withToolTimeout fills this server's tool bound into a configuration that sets none.
//
// It copies rather than writes: the configuration belongs to the caller and every run
// shares it, so filling it in place would mutate something under a run already using
// it. The copy is shallow for the same reason clampedConfig's is, the budget and
// timeout fields being values that nothing in the run path writes to.
//
// A configured harness.tool_timeout is left alone however long it is. Unlike a budget,
// which clamps what a channel asks for, both of these come from whoever started this
// server, and only one of the two was chosen deliberately.
func (s *Server) withToolTimeout(cfg *config.Config) *config.Config {
	if cfg.ToolTimeout() > 0 {
		return cfg
	}

	out := *cfg
	out.Harness.ToolTimeoutParsed = s.opts.ToolTimeout

	return &out
}

// report hands an outcome back to its channel on a context of its own, so a run that
// was canceled or timed out still records what happened. There is nowhere to take a
// failure here beyond the log: the channel is the thing that would have recorded it.
func (s *Server) report(work *Work, out Outcome, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), doneTimeout)
	defer cancel()

	err := work.Done(ctx, out)
	if err != nil {
		log.Error("Reporting an outcome failed", "work", out.ID, "error", err)
	}
}
