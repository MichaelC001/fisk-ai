//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/choria-io/fisk"
	"github.com/choria-io/ui/columns"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/serve"
	ajchannel "github.com/choria-io/fisk-ai/internal/serve/asyncjobs"
	"github.com/choria-io/fisk-ai/internal/telemetry"
	"github.com/choria-io/fisk-ai/internal/util"
)

// fiskServeCommand is the state of one `fisk-ai serve` invocation. Its flags are
// fields rather than package variables so the helpers below can hang off it instead of
// crowding the command package with functions that mean nothing on their own.
type fiskServeCommand struct {
	configFile  string
	stateDir    string
	workDir     string
	apiKey      string
	baseURL     string
	workers     int
	workersSet  bool
	verbose     bool
	noTelemetry bool
}

func registerServeCommand(app *fisk.Application) {
	c := &fiskServeCommand{}

	srv := app.Command("serve", "Hosts the agent behind the surfaces its configuration enables").Action(c.serveAction)
	srv.Flag("config", "Path to the agent configuration file").Default("agent.yaml").StringVar(&c.configFile)
	srv.Flag("workers", "How many jobs to run at once, overriding the configured value").
		Default(fmt.Sprintf("%d", config.DefaultJobsWorkers)).
		IsSetByUser(&c.workersSet).
		IntVar(&c.workers)
	srv.Flag("state-dir", "Directory for checkpointed sessions (default: XDG state dir)").StringVar(&c.stateDir)
	srv.Flag("work-dir", "Directory command tools run in (default: the worker's own working directory)").StringVar(&c.workDir)
	srv.Flag("api-key", "Anthropic API key to use").Envar("ANTHROPIC_API_KEY").StringVar(&c.apiKey)
	srv.Flag("base-url", "Anthropic API base URL to use").Envar("ANTHROPIC_BASE_URL").StringVar(&c.baseURL)
	srv.Flag("no-telemetry", "Suppress OpenTelemetry export for this worker, whatever the configuration says").Envar("NO_TELEMETRY").UnNegatableBoolVar(&c.noTelemetry)
	srv.Flag("verbose", "Log what the surfaces are doing in detail").UnNegatableBoolVar(&c.verbose)
}

// serveAction hosts the agent behind whichever surfaces the configuration enables.
// Today that is the queued-jobs intake: it takes a job off a work queue, runs the
// request its payload carries, and stores the answer on the job's own task. Every run
// is checkpointed under the task id, so a redelivery continues the run a previous
// attempt started rather than paying for it again.
//
// Nothing here creates the storage it uses. The queue, the task store, the session
// stream and the memory bucket are the operator's to provision, so a cluster nobody
// prepared fails at startup rather than being laid out by whichever worker started
// first. Every store is built before the first job for that reason: binding them here
// is what turns a missing stream from a job that dead-letters into a worker that will
// not start.
//
// The order is what makes those failures readable. What the command line can be wrong
// about fails first, then the configuration, then telemetry, then the resources the
// runs share, then the surfaces. The banner prints last, once everything that can fail
// has not.
func (c *fiskServeCommand) serveAction(_ *fisk.ParseContext) error {
	// Validated at the CLI boundary so a bad base URL fails here, naming the flag the
	// operator set. Nothing downstream can name it: the shared provider is built from
	// the configuration, and a provider handed to a run is used as it arrives.
	if c.baseURL != "" {
		err := util.ValidateBaseURL("--base-url / ANTHROPIC_BASE_URL", c.baseURL)
		if err != nil {
			return err
		}
	}

	cfg, err := config.ParseConfigFile(c.configFile)
	if err != nil {
		return err
	}

	err = cfg.ApplyStateDir(c.stateDir)
	if err != nil {
		return err
	}

	if !cfg.JobsEnabled() {
		return c.noSurfaceError()
	}

	log := c.logger()

	// Resolved before anything is opened, so a bad endpoint or sample ratio fails here
	// rather than after the stores are bound. Its report is deferred first, which makes
	// it run last: the flush belongs after every run has ended and every other resource
	// has been released, and it runs on a background context of its own so an interrupt
	// cannot cancel it.
	tel, reportTelemetry, err := setupTelemetry(cfg, telemetrySetup{
		ConfigFile: c.configFile,
		Disabled:   c.noTelemetry,
		Verbose:    c.verbose,
	})
	if err != nil {
		return err
	}
	defer reportTelemetry()

	// What every run shares rather than building for itself, including the agent's own
	// NATS connection. That connection is the top-level nats_context, which is a
	// different thing from the queue's: the jobs channel dials its own from
	// expose.agent.jobs, so a deployment can keep its work queue on one cluster and its
	// session and memory stores on another.
	resources, err := serve.NewResources(cfg, serve.ResourceOptions{
		ConfigFile: c.configFile,
		ConnName:   "serve " + cfg.Identity,
		APIKey:     c.apiKey,
		BaseURL:    c.baseURL,
		Logger:     log,
	})
	if err != nil {
		return err
	}
	// After Serve has returned and before the telemetry flush, which the defer order
	// arranges: the runs using these have all ended by then, and the flush is last
	// because it is the only thing that still has something to say afterwards.
	defer func() {
		closeErr := resources.Close()
		if closeErr != nil {
			log.Error("Releasing the shared resources failed", "error", closeErr)
		}
	}()

	// An interrupt drains and a second one stops. Draining asks the runs to stop where
	// they can be resumed from and hands their jobs back to the queue, then the
	// channels report they are finished and Serve ends by itself with nothing canceled.
	// An idle worker therefore exits on the first interrupt, having nothing to wait for.
	var suspend atomic.Bool

	channels, err := serve.Channels(cfg, []serve.ChannelBuilder{ajchannel.Builder()}, serve.BuildOptions{
		Workers:          c.workerOverride(),
		SuspendRequested: suspend.Load,
		Logger:           log,
	})
	if err != nil {
		return err
	}

	// Concurrency is left to the channels: each states its own, so the number an
	// operator wrote once is the number claimed against and the number run with.
	opts := serve.Options{
		Channels:   channels,
		Config:     cfg,
		ConfigFile: c.configFile,
		// Where the commands run, so a worker started from somewhere else (a unit file,
		// a container) can still point them at the application's own directory. Empty
		// leaves them in the worker's working directory.
		WorkDir:   c.workDir,
		APIKey:    c.apiKey,
		BaseURL:   c.baseURL,
		Telemetry: tel,
		Logger:    log,
	}
	resources.ApplyTo(&opts)

	srv, err := serve.New(opts)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stop := c.onInterrupt(srv, &suspend, cancel, log)
	defer stop()

	fmt.Fprintln(os.Stderr, c.banner(cfg, channels, resources, tel).String())

	err = srv.Serve(ctx)

	// Stop either finishes the drain an interrupt started or performs the whole of it
	// now, depending on how Serve ended. It is the same call, so both are one line.
	stopErr := srv.Stop()

	if err != nil {
		return err
	}

	return stopErr
}

// onInterrupt wires the two-stage shutdown and returns the function that unwires it.
// The first interrupt drains, the second cancels; draining waits for the work in
// flight, so it runs on a goroutine of its own rather than sitting between the two.
func (c *fiskServeCommand) onInterrupt(srv *serve.Server, suspend *atomic.Bool, cancel context.CancelFunc, log *slog.Logger) func() {
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	go func() {
		_, ok := <-signals
		if !ok {
			return
		}

		suspend.Store(true)
		fmt.Fprintln(os.Stderr, "\ndraining: no new work is taken and running work stops where it can resume. Interrupt again to stop now")

		go func() {
			drainErr := srv.Drain()
			if drainErr != nil {
				log.Error("Draining failed", "error", drainErr)
			}
		}()

		<-signals
		cancel()
	}()

	return func() { signal.Stop(signals) }
}

func (c *fiskServeCommand) logger() *slog.Logger {
	level := slog.LevelInfo
	if c.verbose {
		level = slog.LevelDebug
	}

	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// workerOverride is the flag's value when the operator typed one, and zero otherwise.
// Zero leaves the configured value alone, which is what makes the flag win only when
// it was used rather than whenever it has a default.
func (c *fiskServeCommand) workerOverride() int {
	if !c.workersSet {
		return 0
	}

	return c.workers
}

// noSurfaceError names what is missing and shows the block that fixes it. A key name
// on its own is not enough to work out what goes under it.
func (c *fiskServeCommand) noSurfaceError() error {
	return fmt.Errorf(`fisk-ai serve needs a surface to host, and %q enables none. Add a work queue intake:

expose:
  agent:
    jobs: {}

Every field under it defaults, so an empty block takes work from the %q queue as task type %q`,
		c.configFile, config.DefaultJobsQueue, config.DefaultJobsTaskType)
}

// banner describes what the worker resolved, which is an operator's only chance to see
// the settings that decide whether it works before the log takes over.
func (c *fiskServeCommand) banner(cfg *config.Config, channels []serve.Channel, res *serve.Resources, tel *telemetry.Provider) *columns.Document {
	doc := columns.New()
	doc.Headingf("Serving {bold}%s{/bold}/{bold}%s{/bold}", cfg.Identity, util.Version())

	names := make([]string, len(channels))
	for i, ch := range channels {
		names[i] = ch.Name()
	}
	doc.Values("Surfaces", names)

	doc.Item("Model", cfg.LLM.Model)

	// Two contexts, because the queue and the agent's own stores may be on different
	// clusters. Naming both is the only way an operator sees that the queue they meant
	// and the store they meant are not where they thought.
	doc.Item("Queue Context", cfg.JobsNatsContext())
	if cfg.NatsContext != "" && cfg.NatsContext != cfg.JobsNatsContext() {
		doc.Item("Agent Context", cfg.NatsContext)
	}

	doc.Item("Sessions", c.sessionsDescription(res))
	doc.Item("Knowledge", c.knowledgeDescription(cfg, res))
	doc.Item("Telemetry", c.telemetryDescription(tel))
	doc.Item("Tool Directory", c.toolDirectory())
	doc.Item("Tool Timeout", c.toolTimeout(cfg).String())
	doc.Item("Workers", c.workersDescription(cfg))

	return doc
}

// sessionsDescription names the session store that was actually built, and the
// container it is bound to when the backend has one to name.
//
// It asks the store rather than the configuration because the store is the thing that
// resolved: an operator running several machines against file journals needs to see the
// backend that will silently restart their redelivered runs, and a name read back from
// the file they just wrote cannot tell them anything they did not already type. The file
// backends deliberately report no location, so this prints one line for them either way.
func (c *fiskServeCommand) sessionsDescription(res *serve.Resources) string {
	info := res.SessionStore.Info()
	if info.Location == "" {
		return info.Backend
	}

	return fmt.Sprintf("%s (%s)", info.Backend, info.Location)
}

// knowledgeDescription says whether the knowledge tool has an index to search.
//
// A configuration can enable knowledge with nothing indexed yet, and the tool then
// answers every search with "not built" rather than failing, which is correct and
// invisible. Nobody is watching a worker to notice, so it is said once at startup.
func (c *fiskServeCommand) knowledgeDescription(cfg *config.Config, res *serve.Resources) string {
	if !cfg.RAGEnabled() {
		return "disabled"
	}
	if res.RAGStore == nil {
		return "enabled, no index built yet"
	}

	return fmt.Sprintf("enabled (%s)", res.RAGStore.Path())
}

// telemetryDescription reports whether traces and metrics will be exported, read off
// the resolved provider rather than the configuration so a --no-telemetry veto is
// visible. Without it an operator who enabled telemetry in the file and passed the flag
// sees nothing at all, since the note that names a veto only speaks when the OTEL_*
// transport variables are set.
func (c *fiskServeCommand) telemetryDescription(tel *telemetry.Provider) string {
	if !tel.Enabled() {
		return "disabled"
	}

	return "enabled"
}

// toolDirectory names where command tools will run, which is the process's own
// directory unless the operator moved it. It is on the banner because an application
// that cannot find its own files fails one paid model call at a time rather than at
// startup.
func (c *fiskServeCommand) toolDirectory() string {
	if c.workDir != "" {
		return c.workDir
	}

	wd, err := os.Getwd()
	if err != nil {
		return "unknown"
	}

	return wd
}

// toolTimeout is the bound a run will actually get, which is the configured one when
// there is one and the server's own default otherwise. Reporting the configured value
// alone would print zero for a worker that in fact bounds every call.
func (c *fiskServeCommand) toolTimeout(cfg *config.Config) time.Duration {
	if cfg.ToolTimeout() > 0 {
		return cfg.ToolTimeout()
	}

	return serve.DefaultToolTimeout
}

// workersDescription reports the effective worker count and where it came from, since
// a number with two possible sources is worth attributing where it is read.
func (c *fiskServeCommand) workersDescription(cfg *config.Config) string {
	if c.workersSet {
		return fmt.Sprintf("%d (--workers)", c.workers)
	}

	return fmt.Sprintf("%d (config)", cfg.JobsWorkers())
}
