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
	"github.com/choria-io/fisk-ai/internal/toolkit"
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

	// defaultToolTimeout bounds one tool call for a run this server hosts, when the
	// configuration sets none. It is far longer than the 30 seconds the MCP and a2a
	// servers allow a served call, because those answer a caller who is waiting while
	// this hosts whole units of work whose commands legitimately take minutes. It is a
	// bound on a tool that will never answer, not on a slow one.
	defaultToolTimeout = 5 * time.Minute
)

// Options configures a Server.
//
// The shared resources are the ones agent.Options already defines for running many
// agents in one process. They are passed to every run rather than rebuilt per run,
// and their contracts are the ones documented there.
type Options struct {
	// Channels supply the work. At least one is required; each gets its own puller.
	Channels []Channel

	// Config is the parsed agent configuration every run uses. Required.
	Config *config.Config
	// ConfigFile names the file Config was read from, for diagnostics.
	ConfigFile string

	// Concurrency is the maximum number of runs executing at once across all
	// channels; <= 0 uses the default.
	Concurrency int

	// ToolTimeout bounds a single tool call in the runs this server hosts; <= 0 uses
	// the default. It applies only when the configuration sets no
	// harness.tool_timeout, and a configured value wins even when it is longer. That
	// is the opposite precedence to Budget, and deliberately: a budget clamps what a
	// channel asks for, which is a caller this server does not control, while both of
	// these come from whoever started it.
	//
	// A run at a terminal is unbounded by default because an operator can interrupt a
	// command that will never answer. Nobody can interrupt one here, so this is set
	// rather than left off. See config.Config.ToolTimeout for what the bound can and
	// cannot stop.
	ToolTimeout time.Duration

	// WorkDir is the directory the per-run tool working directories are created
	// under. It must be an absolute path that already exists. Empty uses the system
	// temporary directory.
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
		o.ToolTimeout = defaultToolTimeout
	}
	if o.LogOutput == nil {
		o.LogOutput = os.Stderr
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewTextHandler(o.LogOutput, nil))
	}
}

func (o *Options) validate() error {
	if len(o.Channels) == 0 {
		return fmt.Errorf("at least one channel is required")
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

// Server takes work from its channels and runs it, bounded by its concurrency.
type Server struct {
	opts Options
	log  *slog.Logger
	sem  chan struct{}
}

// New validates the options and returns a Server. It performs no I/O and starts
// nothing; Serve does the work.
func New(opts Options) (*Server, error) {
	opts.applyDefaults()

	err := opts.validate()
	if err != nil {
		return nil, err
	}

	return &Server{
		opts: opts,
		log:  opts.Logger,
		sem:  make(chan struct{}, opts.Concurrency),
	}, nil
}

// Serve runs every channel until ctx is canceled or all of them are finished, and
// does not return until each run it started has ended and reported its outcome.
//
// Waiting matters: the shared stores, the provider and the telemetry provider belong
// to the caller and are closed once Serve returns, so returning while a run is still
// using one would pull it out from under that run.
func (s *Server) Serve(ctx context.Context) error {
	var wg sync.WaitGroup

	s.log.Info("Serving", "channels", len(s.opts.Channels), "concurrency", s.opts.Concurrency)

	// One counter covers both the pullers and the runs they spawn. A puller only
	// adds while it is itself counted, so waiting here cannot miss a run that was
	// about to start.
	for _, ch := range s.opts.Channels {
		wg.Go(func() {
			s.pull(ctx, &wg, ch)
		})
	}

	wg.Wait()
	s.log.Info("Stopped serving")

	return nil
}

// pull takes work from one channel until it is finished or the context ends.
//
// The order is take, then acquire a slot, then run. Acquiring first would park a slot
// on a channel that has nothing to offer, and with more channels than slots a channel
// with work waiting would never be served. The cost of this order is that an item is
// claimed while it waits for a slot, so a channel whose work carries a lease must
// tolerate that wait.
func (s *Server) pull(ctx context.Context, wg *sync.WaitGroup, ch Channel) {
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
		case s.sem <- struct{}{}:
		}

		wg.Go(func() {
			defer func() { <-s.sem }()
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

	workDir, err := os.MkdirTemp(s.opts.WorkDir, "fisk-work-")
	if err != nil {
		out.Err = fmt.Errorf("creating the tool working directory: %w", err)
		return
	}
	defer func() {
		rmErr := os.RemoveAll(workDir)
		if rmErr != nil {
			log.Warn("Removing the tool working directory failed", "dir", workDir, "error", rmErr)
		}
	}()

	events := newEventRecorder(work.Events, log)

	// A nil prompter cannot be passed through: the run and the confirm gate call
	// CanPrompt without guarding it, so a configuration carrying any gated tool would
	// dereference nil. Denying is also the right answer, since a channel that supplied
	// no prompter has nobody to ask.
	prompter := work.Prompter
	if prompter == nil {
		prompter = toolkit.DefaultDenyPrompter()
	}

	log.Info("Running", "caller", work.Caller.Name, "resume", work.Checkpoint.ResumeID)

	start := time.Now()
	res, err := agent.Run(ctx, s.runOptions(work, workDir), events, prompter)
	duration := time.Since(start)

	if res != nil {
		out.SessionID = res.SessionID
		out.Text = res.Text
		out.Reason = res.Reason
		out.Stats = res.Stats
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

// runOptions assembles the agent options for one piece of work: the channel's
// attachment points, the shared resources, and this run's own working directory.
func (s *Server) runOptions(work *Work, workDir string) agent.Options {
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
		ToolWorkDir:      workDir,
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
