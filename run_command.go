//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/choria-io/fisk"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/multiplex"
	"github.com/choria-io/fisk-ai/internal/sanitize"
	"github.com/choria-io/fisk-ai/internal/tui"
)

// runNatsContext names the NATS context an agent is reached on, and its presence is
// what makes this process a client of a worker elsewhere rather than the host of one.
//
// The flags beside it record whether a person typed the flag rather than what it
// resolved to. Several carry environment variables, and a run refused because VERBOSE
// is exported in a shell profile would be refusing a command nobody wrote.
var (
	runNatsContext string
	// runIdentity names the agent to address, for a terminal that has no configuration
	// file describing one.
	runIdentity string

	// setConfigFile records that a person named the configuration file, which is what
	// makes a missing one an error on a path that does not otherwise need it.
	setConfigFile bool

	setAPIKey      bool
	setBaseURL     bool
	setTraceFile   bool
	setHTTPDebug   bool
	setVerbose     bool
	setStateDir    bool
	setNoTelemetry bool

	a2aDebug bool
)

func registerRunCommand(cmd *fisk.Application) {
	run := cmd.Command("run", "Runs the agent").Action(runAction)
	run.Arg("q", "Interactive prompt").StringsVar(&q)
	run.Flag("config", "Path to the agent configuration file").IsSetByUser(&setConfigFile).Default("agent.yaml").StringVar(&configFile)
	run.Flag("api-key", "Anthropic API key to use (not needed with --nats-context, where the worker holds it)").IsSetByUser(&setAPIKey).Envar("ANTHROPIC_API_KEY").StringVar(&apiKey)
	run.Flag("base-url", "Anthropic API base URL to use").IsSetByUser(&setBaseURL).Envar("ANTHROPIC_BASE_URL").StringVar(&baseURL)
	run.Flag("http-debug", "Dump Anthropic API request and response bodies to "+httpDebugFilename).IsSetByUser(&setHTTPDebug).Envar("HTTP_DEBUG").UnNegatableBoolVar(&httpDebug)
	run.Flag("a2a-debug", "Dump every a2a message between this terminal and the agent to "+a2aDebugFilename).UnNegatableBoolVar(&a2aDebug)
	run.Flag("no-color", "Disable markdown rendering of the final answer, emitting raw text").Envar("NO_COLOR").UnNegatableBoolVar(&noColor)
	run.Flag("verbose", "Shows more verbose output").IsSetByUser(&setVerbose).Envar("VERBOSE").UnNegatableBoolVar(&verbose)
	run.Flag("tool-output", "Show tool output during the run (expanded in the full-screen UI)").Envar("TOOL_OUTPUT").UnNegatableBoolVar(&showToolOutput)
	run.Flag("thinking", "Show the model's reasoning during the run, when it produces any").Envar("THINKING").UnNegatableBoolVar(&showThinking)
	run.Flag("no-tui", "Disable the full-screen terminal UI and answer one prompt with line-by-line output").Envar("NO_TUI").UnNegatableBoolVar(&noTUI)
	run.Flag("trace", "Write a JSON-lines trace of every LLM request and response to a file").IsSetByUser(&setTraceFile).PlaceHolder("FILE").StringVar(&traceFile)
	run.Flag("resume", "Continue a stored conversation, by session id or by conversation token").PlaceHolder("ID").StringVar(&resumeID)
	run.Flag("force", "Resume even if the configuration no longer matches the saved session").UnNegatableBoolVar(&forceResume)
	run.Flag("state-dir", "Directory holding the sessions of the agent this process hosts (default: XDG state dir)").IsSetByUser(&setStateDir).StringVar(&stateDirFlag)
	run.Flag("no-telemetry", "Suppress OpenTelemetry export for this run, whatever the configuration says").IsSetByUser(&setNoTelemetry).Envar("NO_TELEMETRY").UnNegatableBoolVar(&noTelemetry)
	run.Flag("nats-context", "Talk to an agent on this NATS context instead of running one in this process").PlaceHolder("NAME").StringVar(&runNatsContext)
	run.Flag("identity", "Identity of the agent to talk to, the subject it answers on; requires --nats-context").PlaceHolder("NAME").StringVar(&runIdentity)
}

// runAction resolves the run flags, hosts an agent behind the prompts channel, and
// talks to it.
//
// The terminal is a client of that channel and owns no run: a full-screen session holds
// a conversation of many turns, and --no-tui answers one prompt. Everything either of
// them shows arrives as blocks on a wire, so a run watched here and a run watched by a
// peer agent are the same run watched the same way.
func runAction(_ *fisk.ParseContext) error {
	err := validateRunFlags()
	if err != nil {
		return err
	}

	// Every run is journaled, since a run without a journal is a conversation nothing
	// can continue: no token, no follow-up turn, no resume. The interrupt contract
	// follows from that and depends on no flag: an interrupt asks the run to stop at its
	// next boundary and a second one gives up on it.
	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()

	// Whether this process runs the agent or only talks to one, which is the largest
	// switch on this command and decides most of what follows, starting with how much
	// of a configuration this run needs.
	remote := runNatsContext != ""

	cfg, err := loadRunConfig(remote)
	if err != nil {
		return err
	}

	err = validateRunTarget(cfg, remote)
	if err != nil {
		return err
	}

	if remote {
		return runAgainstWorker(runCtx, runCancel, cfg)
	}

	// Fold --state-dir into the (possibly file-configured) session config, applied
	// last so an explicit flag wins over a configured file directory. It is a hard
	// error against a non-file backend, which has no place for a filesystem path.
	err = cfg.ApplyStateDir(stateDirFlag)
	if err != nil {
		return err
	}

	// Telemetry is resolved before anything else is opened so a bad endpoint or sample
	// ratio fails here, on a normal terminal, rather than under the full-screen UI or
	// after the http-debug file has been created. reportTelemetry flushes the pipelines
	// and says what reached the collector; it runs on a background context of its own,
	// so an interrupt cannot cancel the flush.
	tel, reportTelemetry, err := setupTelemetry(cfg, telemetrySetup{
		ConfigFile: configFile,
		TUI:        runUsesTUI(cfg),
		Disabled:   noTelemetry,
		Verbose:    verbose,
	})
	if err != nil {
		return err
	}
	defer reportTelemetry()

	// --http-debug dumps the API bodies to a file rather than stderr, so it coexists
	// with the full-screen UI whose alt-screen stderr would otherwise be corrupted.
	// The CLI owns the file's lifecycle.
	httpDebugOut, err := resolveHTTPDebugOut()
	if err != nil {
		return err
	}
	if closer, ok := httpDebugOut.(io.Closer); ok {
		defer closer.Close()
	}

	// The store the hosted agent journals into and the channel reads a conversation back
	// from. It is opened here so both are given the same one: two stores would have the
	// channel reading a conversation the run beside it is not writing.
	sessions, releaseSessions, err := sessionStoreFor(runCtx, cfg)
	if err != nil {
		return err
	}
	defer releaseSessions()

	token, err := resumeToken(runCtx, cfg, resumeID, agent.SessionOptions{StoreDir: stateDirFlag, SessionStore: sessions})
	if err != nil {
		return err
	}

	usesTUI := runUsesTUI(cfg)

	// A terminal multiplexer hosting this process is told what the run is doing, so its
	// pane shows this agent as working, idle or waiting on a decision. Whether there is
	// one to tell is read from the environment here rather than deeper down: outside a
	// pane nothing claims the process, and a run answering one prompt line by line is
	// not the session a pane is watching.
	//
	// The pane is labeled with the agent's identity, since a person watching several
	// panes is watching several agents.
	var reporter multiplex.StateReporter
	if usesTUI {
		reporter = multiplex.Detect(os.Getenv, cfg.Identity)
	}

	wire, err := resolveA2ADebugOut(os.Stderr)
	if err != nil {
		return err
	}
	if wire != nil {
		defer wire.Close()
	}

	// The agent is hosted before the screen is opened, so a worker that cannot start
	// says so on a terminal a person can read rather than behind an alt-screen. It is
	// also what keeps the fallback below to one worker instead of two.
	host, err := hostAgent(runCtx, hostOptions{
		Config:       cfg,
		ConfigFile:   configFile,
		APIKey:       apiKey,
		BaseURL:      baseURL,
		Version:      version,
		Sessions:     sessions,
		Telemetry:    tel,
		TraceFile:    traceFile,
		HTTPDebugOut: httpDebugOut,
		Verbose:      verbose,
		Logger:       clientWorkerLogger(usesTUI),
		WireLog:      wire,
		Reporter:     reporter,
	})
	if err != nil {
		return err
	}
	defer func() {
		closeErr := host.Close()
		if closeErr != nil {
			fmt.Fprintf(os.Stderr, "warning: stopping the agent: %v\n", closeErr)
		}
	}()

	// What the agent says about itself, asked while there is still a plain terminal to
	// report a failure on.
	card, err := probeAgent(runCtx, host, "")
	if err != nil {
		return err
	}

	// The full-screen view is the default on an interactive terminal. It is turned off
	// by --no-tui (or NO_TUI) or the agent config's no_tui, and without a real terminal
	// it cannot run at all.
	if usesTUI {
		err := runWithTUI(runCtx, host, cfg, token, card)
		if !errors.Is(err, tui.ErrNoTTY) {
			return err
		}
		// The terminal probe failed after the isatty checks passed; fall through to
		// the line UI silently, since the TUI was not explicitly requested.
	}

	return runAsClient(runCtx, runCancel, host, token)
}

// loadRunConfig reads the configuration this run needs, which differs by whether the
// agent runs in this process.
//
// Hosting one needs the file it is described by. Talking to one somebody else runs needs
// the agent's name and nothing else: the model, the prompt and the credentials are all
// on the worker, so the file is read in the lenient mode and, where --identity supplied
// the name, is not required at all.
func loadRunConfig(remote bool) (*config.Config, error) {
	if !remote {
		return config.ParseConfigFile(configFile)
	}

	cfg, err := clientConfig()
	if err != nil {
		return nil, err
	}

	// Before the target is validated, since the name this supplies is what that
	// validation is looking for.
	err = cfg.ApplyIdentity(runIdentity)
	if err != nil {
		return nil, err
	}

	return cfg, nil
}

// clientConfig is the configuration a terminal talking to a remote agent runs on.
//
// ModeMCP is the most lenient mode: it asks for neither a prompt nor a model, which
// belong to the worker rather than to this terminal, and it is what every other command
// that reads a file it barely uses already parses in.
//
// The file is skipped where --identity named the agent and nobody named a file. Reading
// whichever agent.yaml the working directory happened to hold would take that file's
// timeouts and its model, and refuse the run over a field this terminal never reads. A
// file a person named is read, and one that cannot be read is an error however this run
// was started.
func clientConfig() (*config.Config, error) {
	if runIdentity != "" && !setConfigFile {
		return config.NewConfig()
	}

	cfg, err := config.ParseConfigFileForMode(configFile, config.ModeMCP)
	// The file was the default rather than a path somebody chose, and all this run wanted
	// from it was a name, so say how to give one instead. A file a person named is their
	// path to correct.
	if errors.Is(err, os.ErrNotExist) && !setConfigFile {
		return nil, fmt.Errorf("%w: pass --identity NAME to name the agent to talk to", err)
	}
	if err != nil {
		return nil, err
	}

	return cfg, nil
}

// exportsFromCard reads what an agent said about its telemetry.
//
// No card is not a no: the agent was reachable and did not answer in time, so what a
// person is shown has to say that rather than the reassuring half of it.
func exportsFromCard(card *a2a.AgentCard) (bool, tui.ContentExport) {
	if card == nil {
		return false, tui.ContentExportUnknown
	}

	if card.TelemetryContent {
		return card.Telemetry, tui.ContentExported
	}

	return card.Telemetry, tui.ContentNotExported
}

// modelFromCard reads what an agent said it answers a prompt with.
//
// The card is the only source. What this process is configured for describes the wrong
// machine the moment the agent is somewhere else, and a run that names no model draws
// neither the status bar's model nor the startup card's row, which is what an operator
// should see when nobody has told them.
//
// It is sanitized here because a peer chose it: the status bar escapes widget markup and
// stops there, and a long value would push the token count and the key hints off the bar.
func modelFromCard(card *a2a.AgentCard) string {
	if card == nil {
		return ""
	}

	return sanitize.ForTerminal(card.Model, 48)
}

// runAgainstWorker holds a conversation with an agent somebody else is running.
//
// Nothing is hosted here: no broker, no server, no model provider, no journal and no
// telemetry pipeline. What this process is, is a terminal.
func runAgainstWorker(ctx context.Context, stop context.CancelFunc, cfg *config.Config) error {
	wire, err := resolveA2ADebugOut(os.Stderr)
	if err != nil {
		return err
	}
	if wire != nil {
		defer wire.Close()
	}

	usesTUI := runUsesTUI(cfg)

	// The same reporting a hosted run does, for the same reason: what a pane shows is
	// what this terminal is doing, whoever is running the agent behind it. The label is
	// the identity too, which remotely is the agent this terminal is addressing.
	var reporter multiplex.StateReporter
	if usesTUI {
		reporter = multiplex.Detect(os.Getenv, cfg.Identity)
	}

	host, err := dialAgent(ctx, cfg, runNatsContext, clientWorkerLogger(usesTUI), wire, reporter)
	if err != nil {
		return err
	}
	defer func() {
		closeErr := host.Close()
		if closeErr != nil {
			fmt.Fprintf(os.Stderr, "warning: disconnecting: %v\n", closeErr)
		}
	}()

	// Remotely a session id names a journal this machine does not hold, so the token is
	// what a person carries. It is sent as it stands and the worker decides.
	token := resumeID

	card, err := probeAgent(ctx, host, runNatsContext)
	if err != nil {
		return err
	}

	if usesTUI {
		err := runWithTUI(ctx, host, cfg, token, card)
		if !errors.Is(err, tui.ErrNoTTY) {
			return err
		}
	}

	return runAsClient(ctx, stop, host, token)
}

// validateRunTarget refuses a combination of flags that describes work this process
// will not be doing.
//
// Each refusal names the same escape, since somebody who typed one of these habitually
// needs to hear how to get it back rather than only that it is wrong. A flag that
// arrived from the environment is ignored in silence: refusing a run because VERBOSE is
// exported in a shell profile would refuse a command nobody wrote.
func validateRunTarget(cfg *config.Config, remote bool) error {
	if !remote {
		// The agent runs here, so its model credentials have to be here.
		if apiKey == "" {
			return fmt.Errorf("--api-key is required to run an agent in this process; set it, export ANTHROPIC_API_KEY, or pass --nats-context to talk to an agent that already has one")
		}

		// This command injects no tools of its own, so the configuration is the whole
		// answer and the refusal can come before telemetry, the stores and the debug
		// files are opened. A Go program embedding the agent may inject its own, which
		// is why config validation stays quiet about this and the caller asks.
		if !cfg.SuppliesTools() {
			return fmt.Errorf("no tools available: this agent wraps no application (application_path unset) and enables no built-in, remote or mcp tools; set application_path, or enable harness.knowledge, harness.memory, human_in_the_loop, remote_tools or mcp_clients in %q", configFile)
		}

		return nil
	}

	// The identity is the address on a bus rather than a label, and the transport queue
	// groups on it, so a name arrived at by accident joins somebody else's fleet rather
	// than failing to resolve.
	if !cfg.IdentityIsNamed() {
		return fmt.Errorf("--nats-context needs an agent to address: pass --identity NAME, or set identity in %q, since the name is the subject the agent answers on and would otherwise be taken from the application or left at the default", configFile)
	}

	for _, refusal := range []struct {
		typed bool
		flag  string
		why   string
	}{
		{setAPIKey, "--api-key", "the agent's own model credentials, which the worker holds"},
		{setBaseURL, "--base-url", "the agent's own model endpoint, which the worker chooses"},
		{setTraceFile, "--trace", "a file written on the machine running the agent, which is not this one"},
		{setHTTPDebug, "--http-debug", "a file written on the machine running the agent, which is not this one"},
		{setVerbose, "--verbose", "the agent's own narration, which happens on the worker"},
		{setStateDir, "--state-dir", "where the agent this process hosts keeps its sessions, and this process hosts none"},
		{setNoTelemetry, "--no-telemetry", "export by the process running the agent, which is not this one"},
	} {
		if refusal.typed {
			return fmt.Errorf("%s cannot be used with --nats-context: it is %s; run it on the worker, or drop --nats-context to run the agent here", refusal.flag, refusal.why)
		}
	}

	return nil
}

// httpDebugFilename is where --http-debug writes the API body dumps. It is a file
// (not stderr) so debugging coexists with the full-screen UI, whose alt-screen would
// otherwise be corrupted by inline dumps.
const httpDebugFilename = "http-debug.log"

// a2aDebugFilename is where --a2a-debug writes the protocol dump, on the same terms and
// for the same reason. It is a fixed name this program owns rather than a path somebody
// gives it, which is what makes removing a stale one safe.
const a2aDebugFilename = "a2a-debug.log"

// resolveA2ADebugOut opens the a2a dump when --a2a-debug is set, and returns nil when it
// is not. The caller owns closing it. The notice naming the file is written to notice,
// which the run passes os.Stderr and a spec passes io.Discard, so driving this from a
// test does not print into the test output.
//
// The file holds the conversation token, the prompts and every tool result, so it gets
// what the http dump gets: created 0600, and removed before being created exclusively,
// which drops a symlink somebody may have planted at the fixed name rather than
// following it.
func resolveA2ADebugOut(notice io.Writer) (io.WriteCloser, error) {
	if !a2aDebug {
		return nil, nil
	}

	err := os.Remove(a2aDebugFilename)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("opening a2a-debug file %q: %w", a2aDebugFilename, err)
	}

	f, err := os.OpenFile(a2aDebugFilename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("opening a2a-debug file %q: %w", a2aDebugFilename, err)
	}

	fmt.Fprintf(notice, "%s\n", a2aDebugNotice)

	return f, nil
}

// a2aDebugNotice says the dump is being written.
//
// It is printed to stderr and shown again in the transcript, because under the
// full-screen view stderr is taken for the whole run and flushed after the terminal is
// restored, so the first copy is read by nobody until the file is already written.
var a2aDebugNotice = fmt.Sprintf("Dumping a2a messages to %s", a2aDebugFilename)

// resolveHTTPDebugOut opens the http-debug dump file when --http-debug is set,
// discarding any previous run's dump, and returns nil when it is not set. The caller
// owns closing the returned writer.
//
// The file holds the full conversation, so it is created 0600. It is removed first
// and then created exclusively: removing drops a symlink an attacker may have planted
// at the fixed name (which O_TRUNC would otherwise follow and overwrite), and O_EXCL
// fails rather than following one re-created in the race window.
func resolveHTTPDebugOut() (io.Writer, error) {
	if !httpDebug {
		return nil, nil
	}

	err := os.Remove(httpDebugFilename)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("opening http-debug file %q: %w", httpDebugFilename, err)
	}

	f, err := os.OpenFile(httpDebugFilename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("opening http-debug file %q: %w", httpDebugFilename, err)
	}
	fmt.Fprintf(os.Stderr, "Dumping Anthropic API request and response bodies to %s (contains the full conversation; file is created mode 0600, do not share it)\n", httpDebugFilename)

	return f, nil
}

// runUsesTUI reports whether a run should render in the full-screen UI. The UI is
// the default on an interactive terminal; it is turned off by --no-tui (or NO_TUI)
// or the agent config's no_tui, and it cannot run without a real terminal on both
// stdin and stdout.
func runUsesTUI(cfg *config.Config) bool {
	return !noTUI && !cfg.TUIDisabled() && stdinIsTerminal() && stdoutIsTerminal()
}

// runWithTUI holds a conversation in the full-screen view: the run's blocks draw into
// the viewport, its questions go to the operator through native widgets, and the input
// row takes the next turn when one ends.
//
// The view stays up after the conversation ends so the transcript can be read. On exit
// the terminal is restored and the answer, the advisories, the handles and the summary
// are reprinted to the normal buffer, so the result survives in scrollback and a piped
// answer stays clean. It returns ErrNoTTY (wrapped) when the screen cannot be opened,
// so the caller falls back to the line UI.
func runWithTUI(ctx context.Context, host *hostedAgent, cfg *config.Config, token string, card *a2a.AgentCard) error {
	renderer := &blockRenderer{showThinking: showThinking}
	session := &chatSession{host: host, conversation: token}

	exports, content := exportsFromCard(card)

	// The reporting is invisible from inside the pane, so the bar says which multiplexer
	// is being told about this run rather than leaving the operator to find out from the
	// pane list whether the integration found anything.
	var multiplexer string
	if host.reporter != nil {
		multiplexer = host.reporter.Name()
	}

	live, err := tui.NewLive(tui.Meta{
		Version: version,
		Query:   strings.Join(q, " "),
		Resume:  token != "",
		Dir:     runDir(),
		// What the agent says about itself rather than what this process is configured
		// for. The exporting process is whichever one runs the agent, and so is the one
		// calling the model, so a client that read its own configuration would be
		// answering for the wrong machine the moment the agent is somewhere else.
		Model:            modelFromCard(card),
		Telemetry:        exports,
		TelemetryContent: content,
		Multiplexer:      multiplexer,
	}, noColor, session.requestStop)
	if err != nil {
		return err
	}

	session.live = live
	session.client = &tuiClient{live: live, renderer: renderer}

	live.SetBell(cfg.BellEnabled())
	// Tool output is always shown in the full-screen view but starts folded to a
	// placeholder; --tool-output expands it by default so the raw results are visible
	// inline without pressing Z.
	if showToolOutput {
		live.ExpandToolOutput()
	}
	// Every full-screen run is a conversation, so the input row is always there. What a
	// turn ends is the turn, and the row is what takes the next one.
	live.EnableInteractive()

	// The view classifies its terminal state and its closing lines from how the last
	// turn ended, which it consults only after the run function has returned.
	live.SetSuspendedFunc(session.suspended)
	live.SetResumeHintFunc(func() string { return resumeHint(host.identity, host.natsContext, session.conversation) })
	live.SetTraceHintFunc(session.traceLine)

	runErr := live.Run(ctx, func(runCtx context.Context) error {
		// Said again here because the stderr copy was covered by this screen the moment
		// it was printed, and stays covered until the run is over and the file is
		// written. It happens on the run goroutine rather than before Run: appending
		// marshals onto the tview loop, and the loop is not running until Run starts it,
		// so a line queued before then blocks forever with the screen already taken.
		if a2aDebug {
			live.Append(tui.Line{Kind: tui.LineWarning, Text: a2aDebugNotice})
		}

		return session.run(runCtx)
	})

	// Named the way the line view names them: this lands in scrollback once the screen is
	// gone, where the pane that said which agent was answering is gone with it.
	lead := warningLead(host.identity, host.natsContext)
	for _, w := range renderer.warnings {
		fmt.Fprintf(os.Stderr, "%s: %s\n", lead, sanitize.ForTerminal(w, 400))
	}
	// A reset during the session left earlier conversations stored and continuable;
	// reprint their handles so they survive the alt-screen teardown.
	for _, hint := range session.left {
		fmt.Fprintf(os.Stderr, "previous conversation saved; %s\n", hint)
	}
	if renderer.answer != "" {
		fmt.Fprintln(os.Stdout, tui.RenderAnswer(renderer.answer, noColor))
	}
	// Every conversation continues, so the handle prints however this one ended rather
	// than only when it stopped somewhere it can be resumed from.
	hint := resumeHint(host.identity, host.natsContext, session.conversation)
	if hint != "" {
		fmt.Fprintf(os.Stderr, "\n%s\n", hint)
	}
	printUsage(session.usage(), ackMaxTokens(session.outcome))
	if line := session.traceLine(); line != "" {
		fmt.Fprintf(os.Stderr, "%s\n", line)
	}

	return runErr
}

// runDir is the working directory shown on the live view's startup card, with the home
// prefix collapsed to ~. It returns "" when the directory cannot be determined, which
// hides the line.
func runDir() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return dir
	}
	if dir == home {
		return "~"
	}
	if strings.HasPrefix(dir, home+string(os.PathSeparator)) {
		return "~" + dir[len(home):]
	}

	return dir
}

// validateRunFlags rejects incompatible combinations of the checkpoint and
// resume flags before any work is done.
func validateRunFlags() error {
	if resumeID != "" && len(q) > 0 {
		return fmt.Errorf("--resume does not take a query; the prompt is restored from the session")
	}
	if forceResume && resumeID == "" {
		return fmt.Errorf("--force only applies when resuming")
	}
	// Refused here rather than beside the other --nats-context refusals, which run after
	// the configuration is read: this flag decides whether that file is needed at all,
	// so a run that gets as far as reading one has already taken the wrong branch.
	if runIdentity != "" && runNatsContext == "" {
		return fmt.Errorf("--identity only applies with --nats-context: it names the agent to address on a bus, and an agent hosted in this process takes its identity from the configuration, which is also what names where its conversations are stored")
	}
	// Validate at the CLI boundary so a bad base URL fails on a normal terminal,
	// before the http-debug file is created or the full-screen UI is launched.
	if baseURL != "" {
		if err := sanitize.BaseURL("--base-url / ANTHROPIC_BASE_URL", baseURL); err != nil {
			return err
		}
	}

	return nil
}
