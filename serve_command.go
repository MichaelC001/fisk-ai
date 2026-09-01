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
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/choria-io/fisk"
	"github.com/choria-io/ui/columns"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/sanitize"
	"github.com/choria-io/fisk-ai/internal/serve"
	"github.com/choria-io/fisk-ai/internal/serve/a2aendpoint"
	ajchannel "github.com/choria-io/fisk-ai/internal/serve/asyncjobs"
	slackchannel "github.com/choria-io/fisk-ai/internal/serve/slack"
	"github.com/choria-io/fisk-ai/internal/telemetry"
)

// fiskServeCommand is the state of one `fisk serve` invocation. Its flags are
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

	srv := app.Command("serve", "Hosts the agent behind the endpoints its configuration enables").Action(c.serveAction)
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
	srv.Flag("verbose", "Log what the endpoints are doing in detail").UnNegatableBoolVar(&c.verbose)
}

// serveAction hosts the agent behind whichever endpoints the configuration enables.
//
// The queued-jobs intake takes a job off a work queue, runs the request its payload
// carries, and stores the answer on the job's own task. Every run is checkpointed under
// the task id, so a redelivery continues the run a previous attempt started rather than
// paying for it again.
//
// The a2a endpoint serves this agent's tools to other agents. A peer invokes a tool and
// gets its result; no prompt is involved and the agent loop never runs, so it engages
// none of the machinery below beyond the connection it answers on.
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
// runs share, then the endpoints. The banner prints last, once everything that can fail
// has not.
func (c *fiskServeCommand) serveAction(_ *fisk.ParseContext) error {
	// Validated at the CLI boundary so a bad base URL fails here, naming the flag the
	// operator set. Nothing downstream can name it: the shared provider is built from
	// the configuration, and a provider handed to a run is used as it arrives.
	if c.baseURL != "" {
		err := sanitize.BaseURL("--base-url / ANTHROPIC_BASE_URL", c.baseURL)
		if err != nil {
			return err
		}
	}

	cfg, err := config.ParseConfigFileForMode(c.configFile, config.ModeServe)
	if err != nil {
		return err
	}

	err = cfg.ApplyStateDir(c.stateDir)
	if err != nil {
		return err
	}

	if !cfg.JobsEnabled() && !cfg.A2AEnabled() && !cfg.SlackEnabled() {
		return c.noEndpointError()
	}

	log := c.logger()

	// Startup has a context of its own, separate from the one Serve runs on. It governs
	// the dialing and binding below, so an interrupt while a broker is unreachable ends
	// the command instead of waiting the dial out. Nothing reads it once the endpoints
	// are built: an interrupt during a run means drain, which is a different answer and
	// is what onInterrupt arranges.
	startCtx, cancelStart := interruptContext()
	defer cancelStart()

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
	resources, err := serve.NewResources(startCtx, cfg, serve.ResourceOptions{
		ConfigFile: c.configFile,
		ConnName:   "serve " + cfg.Identity,
		Version:    version,
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

	channels, services, err := serve.Endpoints(startCtx, cfg, serve.BuildOptions{
		Workers:          c.workerOverride(),
		SuspendRequested: suspend.Load,
		Conns:            resources.Conns,
		ConfigFile:       c.configFile,
		Version:          version,
		Logger:           log,
		Telemetry:        tel,
		Sessions:         resources.SessionStore,
	}, []serve.EndpointBuilder{ajchannel.Builder(), a2aendpoint.Builder(), slackchannel.Builder()})
	if err != nil {
		return err
	}

	// Concurrency is left to the channels: each states its own, so the number an
	// operator wrote once is the number claimed against and the number run with.
	opts := serve.Options{
		Channels:   channels,
		Services:   services,
		Config:     cfg,
		ConfigFile: c.configFile,
		Version:    version,
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

	stop := c.onInterrupt(srv, &suspend, cancel, len(channels) > 0, log)
	defer stop()

	fmt.Fprintln(os.Stderr, c.banner(cfg, channels, services, resources, tel).String())

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
func (c *fiskServeCommand) onInterrupt(srv *serve.Server, suspend *atomic.Bool, cancel context.CancelFunc, runs bool, log *slog.Logger) func() {
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	// What a drain means depends on what is hosted. A worker with no channel has no
	// work to stop and nothing to resume: its endpoints stop answering and that is the
	// whole of it.
	drainNotice := "\ndraining: the endpoints stop answering. Interrupt again to stop now"
	if runs {
		drainNotice = "\ndraining: no new work is taken and running work stops where it can resume. Interrupt again to stop now"
	}

	go func() {
		_, ok := <-signals
		if !ok {
			return
		}

		suspend.Store(true)
		fmt.Fprintln(os.Stderr, drainNotice)

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

// noEndpointError names what is missing and shows the blocks that fix it. A key name
// on its own is not enough to work out what goes under it.
func (c *fiskServeCommand) noEndpointError() error {
	return fmt.Errorf(`fisk serve needs an endpoint to run, and %q enables none. Add a work queue intake:

expose:
  agent:
    jobs: {}

Every field under it defaults, so an empty block takes work from the %q queue as task type %q.

To answer other agents instead, or as well, add an a2a block saying what it answers:

expose:
  agent:
    a2a:
      serve_tools: true
      prompts: {}

serve_tools answers tool calls from peers, and a prompts block answers prompts by running
the agent loop over them; either alone is enough.

To answer people in Slack, add a slack block:

expose:
  agent:
    slack: {}

Every field under it defaults too. It needs SLACK_APP_TOKEN and SLACK_BOT_TOKEN in the
environment, which is where its credentials come from rather than this file`,
		c.configFile, config.DefaultJobsQueue, config.DefaultJobsTaskType)
}

// banner describes what the worker resolved, which is an operator's only chance to see
// the settings that decide whether it works before the log takes over.
//
// What it reports depends on which endpoints are hosted. The model, the queue and
// everything about a run describe the agent loop, and a worker serving only tools runs
// none, so printing them there would name a queue it does not consume and a tool
// directory its served calls do not use.
func (c *fiskServeCommand) banner(cfg *config.Config, channels []serve.Channel, services []serve.Service, res *serve.Resources, tel *telemetry.Provider) *columns.Document {
	doc := columns.New()
	doc.Headingf("Serving {bold}%s{/bold}/{bold}%s{/bold}", cfg.Identity, version)

	names := make([]string, 0, len(channels)+len(services))
	for _, ch := range channels {
		names = append(names, ch.Name())
	}
	for _, svc := range services {
		names = append(names, svc.Name())
	}
	doc.Values("Endpoints", names)

	runs := len(channels) > 0
	queued := cfg.JobsEnabled()

	if runs {
		doc.Item("Model", cfg.LLM.Model)
	}

	// The queue's context belongs to the queue: it may be a different cluster from the
	// one the stores and the a2a endpoints use, and naming both is the only way an
	// operator sees that the queue they meant and the store they meant are not where
	// they thought. A worker with no queue has one context and prints it once.
	if queued {
		doc.Item("Queue Context", cfg.JobsNatsContext())
	}

	// Named whenever it differs from the queue's, and whenever there is no queue to
	// differ from, since it is the connection the a2a endpoints answer on as well as the
	// one the stores are reached over.
	if cfg.NatsContext != "" && (!queued || cfg.NatsContext != cfg.JobsNatsContext()) {
		doc.Item("Agent Context", cfg.NatsContext)
	}

	if runs {
		doc.Item("Sessions", c.sessionsDescription(res))
		doc.Item("Knowledge", c.knowledgeDescription(cfg, res))
		doc.Item("MCP Clients", c.mcpClientsDescription(cfg))
	}

	doc.Item("Telemetry", c.telemetryDescription(tel))

	if runs {
		doc.Item("Tool Directory", c.toolDirectory())
		doc.Item("Tool Timeout", c.toolTimeout(cfg).String())
	}

	// The worker count belongs to the intake that claims work against it. A process
	// hosting both runs the two numbers side by side, so one line called Workers would
	// name whichever intake was asked first.
	if queued {
		doc.Item("Queue Workers", c.workersDescription(cfg))
	}

	c.describeEndpoints(doc, cfg, channels, services)

	return doc
}

// describeEndpoints adds a section per endpoint that has something to say about itself.
//
// Each a2a endpoint bounds and paces its work with numbers of its own, against the
// loop's five minutes and the queue's worker count, so they are printed under the
// endpoint they belong to rather than beside the loop's where an operator would read one
// pair as the other.
func (c *fiskServeCommand) describeEndpoints(doc *columns.Document, cfg *config.Config, channels []serve.Channel, services []serve.Service) {
	for _, ch := range channels {
		prompts, ok := ch.(*a2aendpoint.Channel)
		if !ok {
			continue
		}

		doc.Section("Answering prompts over a2a", func(d *columns.Document) {
			for _, line := range prompts.Describe() {
				d.Item(line.Label, line.Value)
			}

			d.Item("Workers", fmt.Sprintf("%d", cfg.A2APromptsWorkers()))
		})
	}

	// The Slack channel supplies its own worker count, unlike the a2a one above: it sizes
	// its own concurrency from expose.agent.slack rather than from the number the queue
	// intake uses.
	for _, ch := range channels {
		bot, ok := ch.(*slackchannel.Channel)
		if !ok {
			continue
		}

		doc.Section("Answering in Slack", func(d *columns.Document) {
			for _, line := range bot.Describe() {
				d.Item(line.Label, line.Value)
			}
		})
	}

	for _, svc := range services {
		a2aSvc, ok := svc.(*a2aendpoint.Service)
		if !ok {
			continue
		}

		doc.Section("Serving tools over a2a", func(d *columns.Document) {
			for _, line := range a2aSvc.Describe() {
				d.Item(line.Label, line.Value)
			}

			d.Item("Concurrency", c.a2aConcurrency(cfg))
			d.Item("Tool Timeout", c.a2aToolTimeout(cfg).String())
			d.Values("Exposed", a2aSvc.ExposedTools())

			withheld := a2aSvc.WithheldBuiltins()
			if len(withheld) > 0 {
				d.Values("Withheld", withheld)
				d.Printf("Withheld built-in tools are enabled by this configuration but declare no a2a exposure.")
			}
		})
	}
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

// mcpClientsDescription names the MCP servers whose tools every hosted run imports.
// They are connected once at startup and shared, so a server named here has already
// answered: the worker refuses to start when one cannot be reached, and the banner
// prints after everything that can fail has.
//
// The names come from the configuration rather than from the connected set, so the
// line reads the same whether or not this process built the sessions itself.
func (c *fiskServeCommand) mcpClientsDescription(cfg *config.Config) string {
	if len(cfg.MCPClients) == 0 {
		return "none configured"
	}

	names := make([]string, 0, len(cfg.MCPClients))
	for _, server := range cfg.MCPClients {
		names = append(names, server.Name)
	}

	return strings.Join(names, ", ")
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

// a2aToolTimeout is the bound a served call will actually get, and a2aConcurrency how
// many of them may run at once. Both report the a2a server's own default when the
// configuration sets neither, since a banner saying a bound is zero when it is thirty
// seconds is worse than saying nothing.
func (c *fiskServeCommand) a2aToolTimeout(cfg *config.Config) time.Duration {
	if cfg.A2AToolTimeout() > 0 {
		return cfg.A2AToolTimeout()
	}

	return a2a.DefaultCallTimeout
}

func (c *fiskServeCommand) a2aConcurrency(cfg *config.Config) int {
	if cfg.A2AMaxConcurrentTools() > 0 {
		return cfg.A2AMaxConcurrentTools()
	}

	return a2a.DefaultConcurrency()
}

// workersDescription reports the effective worker count and where it came from, since
// a number with two possible sources is worth attributing where it is read.
func (c *fiskServeCommand) workersDescription(cfg *config.Config) string {
	if c.workersSet {
		return fmt.Sprintf("%d (--workers)", c.workers)
	}

	return fmt.Sprintf("%d (config)", cfg.JobsWorkers())
}
