//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// Package agent turns a parsed configuration into a running agent: it loads the
// tools, imports any remote ones, builds the LLM client, sets up checkpointing
// and resume, and drives the agentic loop to a terminal state. It owns no CLI
// concerns: flags, signals and terminal rendering stay with the caller, which
// receives the run's narration, tool traces and advisories as structured Events
// and gets a Result to render at the end.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/toolkit/builtin"
	"github.com/choria-io/fisk-ai/internal/toolkit/fisk"
	"github.com/choria-io/fisk-ai/internal/toolkit/functool"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/a2a"
	// Link the NATS a2a transport in so it registers itself; a2a.NewTransport
	// resolves the configured transport from the registry, and this is the sole
	// transport today.
	_ "github.com/choria-io/fisk-ai/internal/a2a/nats"
	"github.com/choria-io/fisk-ai/internal/conns"
	"github.com/choria-io/fisk-ai/internal/llm"
	// Link the anthropic provider in so it registers itself; llm.NewProvider resolves
	// the configured provider from the registry, and this is the sole provider today.
	_ "github.com/choria-io/fisk-ai/internal/llm/anthropic"
	"github.com/choria-io/fisk-ai/internal/mcpclient"
	"github.com/choria-io/fisk-ai/internal/memory"
	// Link the file memory backend in so it registers itself; memory.New resolves
	// the configured backend from the registry, and this is the default backend.
	_ "github.com/choria-io/fisk-ai/internal/memory/file"
	// Link the jetstream memory backend in so it registers itself; it binds a
	// pre-existing NATS KV bucket over the shared connection.
	_ "github.com/choria-io/fisk-ai/internal/memory/jetstream"
	"github.com/choria-io/fisk-ai/internal/rag"
	"github.com/choria-io/fisk-ai/internal/remotetools"
	"github.com/choria-io/fisk-ai/internal/runstate"
	// Link the file session backend in so it registers itself; runstate.New resolves
	// the configured backend from the registry.
	_ "github.com/choria-io/fisk-ai/internal/runstate/file"
	// Link the jetstream session backend in so it registers itself; it binds a
	// pre-existing NATS JetStream stream over the shared connection.
	_ "github.com/choria-io/fisk-ai/internal/runstate/jetstream"
	"github.com/choria-io/fisk-ai/internal/sanitize"
	"github.com/choria-io/fisk-ai/internal/telemetry"
	"github.com/choria-io/fisk-ai/internal/telemetry/genai"
)

// defaultMaxOutputTokens caps the tokens generated per LLM call. It is distinct
// from the cumulative llm.budget.max_tokens spend cap: this bounds a single
// response and must stay within every supported model's per-response limit,
// while the budget bounds the whole run.
const defaultMaxOutputTokens = 8192

// thinkingMaxOutputTokens raises the per-call output cap when thinking is enabled
// so the reasoning summary and the answer both fit; thinking output counts toward
// this per-response limit. It stays within the non-streaming ceiling that keeps
// responses clear of SDK HTTP timeouts.
const thinkingMaxOutputTokens = 16384

// resolveMaxOutputTokens picks the per-call output cap. An explicit
// llm.budget.max_output_tokens wins so an operator can fit an endpoint whose
// per-response limit is below the default; otherwise the built-in default is used,
// raised when thinking is on so the reasoning summary and the answer both fit.
func resolveMaxOutputTokens(cfg *config.Config, thinking bool) int64 {
	if n := cfg.LLM.Budget.MaxOutputTokens; n > 0 {
		return n
	}
	if thinking {
		return thinkingMaxOutputTokens
	}
	return defaultMaxOutputTokens
}

// toolSearchDegradation returns the advisory to raise when totalTools crosses the
// tool-search threshold but the provider cannot do tool search, so every tool is
// sent to the model directly. It returns nil when there is nothing to warn about:
// the provider supports tool search, the set is small enough to send directly
// anyway, or operatorEnabled is false.
//
// An operator who set no_tool_search chose the direct send, so a run does not
// report it back to them each time; fisk info reports the state and its cost when
// they ask for it.
func toolSearchDegradation(totalTools int, caps llm.Caps, operatorEnabled bool) *Warning {
	if !operatorEnabled || caps.SupportsToolSearch || totalTools < ToolSearchThreshold {
		return nil
	}

	return &Warning{Kind: WarnToolSearchUnsupported, Count: totalTools}
}

// resumeReminder is appended to the system prompt of a resumed run so the model
// re-verifies external state before acting on results captured before the
// suspension.
const resumeReminder = "This session was suspended and has now resumed. Tool results earlier in the conversation may be stale; re-verify current state before taking any state-changing action."

// Checkpoint carries the resumable-run options. Enabled starts a new checkpointed
// run; ResumeID continues an existing one; the two are mutually exclusive, which
// the caller validates before calling Run. FollowUp says what a resume does with the
// prompt it was given.
type Checkpoint struct {
	Enabled  bool
	Name     string
	ResumeID string
	Force    bool

	// CreateIfMissing makes ResumeID name a run rather than ask for one, and Run
	// answers for that name whichever of three states the store is in: no session is
	// created, an unfinished one is continued, and a completed one is reported as it
	// stands. It has no meaning without ResumeID.
	//
	// It is what an at-least-once caller needs. A queue that may deliver the same
	// item twice cannot tell a first delivery from a redelivery of a worker that
	// died mid-run, and the delivery count is the wrong authority for it: the store
	// is the only thing that knows whether the run exists. Naming the run and asking
	// for it either way removes the question, and makes the call idempotent under
	// that name.
	//
	// The completed case is what stops a lost acknowledgement from repeating: the
	// answer is returned from the journal, with the stored run's counters, rather than
	// the caller being told its own finished work is an error. Nothing runs on that
	// path, so no hook fires and no event is emitted.
	//
	// One reported value narrows under it. The root span's Resumed attribute is
	// fixed before the store is reachable, so with CreateIfMissing it reports that a
	// resume was asked for rather than that one happened. The RunStart hook's
	// Resumed is unaffected and reports what the store said.
	CreateIfMissing bool

	// FollowUp delivers Options.Prompt as a new user turn on the resumed conversation
	// instead of discarding it. It is what a caller whose conversations outlive a
	// connection needs: every turn is a fresh resumed run, so any worker can serve any
	// turn of a conversation and none of them is pinned to a process.
	//
	// It requires ResumeID and a prompt, and Run refuses it with CreateIfMissing, which
	// is the combination an at-least-once caller reaches for and the one thing this must
	// never be. A queue cannot tell a first delivery from a redelivery, so a redelivered
	// item carrying a follow-up would append the same prompt to the conversation again
	// and pay for another turn on it. A caller that may deliver the same work twice
	// discards its prompt on a resume, which is the default.
	//
	// The turn is delivered where the stored conversation can take a user message: with
	// nothing in flight it enters before the first model call, and with a turn the last
	// run left unfinished the loop finishes that turn first and the follow-up is the next
	// one. A conversation waiting on a deferred tool result reaches no such boundary, so
	// nothing is delivered and Result.FollowUpTaken reports it.
	//
	// A resume whose stored run ended by completing is continued rather than refused,
	// since a new user turn is the new input a completed conversation lacks.
	FollowUp bool

	// Answer supplies the result of a call this conversation deferred, applied once
	// the resume holds the journal and before the loop runs, so the turn continues
	// with the answer rather than stopping on the same call again.
	//
	// It is for a caller that was asked something and could not answer in time: a
	// deferred call is never dispatched again, so the answer is the only way its turn
	// finishes. An approval needs nothing here, since the call it guards is
	// dispatched again on any resume and the question is put again with it.
	//
	// It requires ResumeID and is refused with CreateIfMissing, which names a run
	// that may not exist yet. Naming a call this conversation is not waiting on, or
	// one that already has a result, refuses the resume before anything runs.
	Answer *DeferredAnswer

	// ConversationToken is the caller's handle for the conversation this run journals.
	// It is written to the Meta record where the journal is created and read by nothing
	// in the loop, so that a caller that lost its handle can be given it back and an
	// operator can say which stored conversation is which.
	//
	// It is for a channel that derives the run id from a token rather than letting a
	// caller name a journal. Recovering it needs the store access that already grants
	// reading and writing that journal directly.
	//
	// A journal is created by Enabled, or by ResumeID with CreateIfMissing where the
	// run does not exist yet. Anything else has nowhere to record it and is refused
	// rather than dropped, a token nothing wrote down being unrecoverable. Against a
	// journal that already exists it is a no-op, since that journal holds its own.
	ConversationToken string

	// Caller is what the channel that produced this run knows about who asked for it,
	// recorded in the Meta record beside ConversationToken and read by nothing.
	//
	// It is a label for a person reading a journal, so that two conversations whose
	// first prompts are alike can still be told apart. It is the caller's own claim:
	// nothing verifies it, and nothing may decide on it.
	//
	// A server that hosts channels fills it from what the channel reported, so a
	// channel that already said who its caller is does not say so twice.
	Caller string
}

// DeferredAnswer is the result of a call that deferred, supplied by whoever the tool
// was waiting on.
type DeferredAnswer struct {
	// ToolUseID is the call being answered.
	ToolUseID string
	// Content is what the tool would have returned had it answered at once, in the
	// shape that tool's own results take.
	Content string
	// IsError marks the result the way a tool's own failure would be marked.
	IsError bool
}

// Options is everything Run needs to execute a run. Config is already parsed so
// Run does no file IO; the caller owns flags, signals and rendering.
type Options struct {
	Config     *config.Config
	ConfigFile string
	Prompt     []string

	// Version is the caller's own build version. It identifies this client to the
	// MCP servers the run connects to and is written to the trace file's session
	// line. Empty sends no version to a server and omits the field from the trace.
	Version string

	APIKey  string
	BaseURL string
	// HTTPDebugOut, when non-nil, receives a dump of every Anthropic API request and
	// response body. The caller owns the writer's lifecycle (for example a file it
	// opens and closes); os.Stderr reproduces the old stderr-dump behavior.
	HTTPDebugOut io.Writer
	TraceFile    string
	Verbose      bool

	Checkpoint Checkpoint

	// ClaimedBy names this worker in the claim a resume writes to the journal, for a
	// person reading it later. Empty derives one from the agent identity, the hostname
	// and the pid.
	//
	// It is worth setting wherever that derivation is uninformative: a container's
	// hostname is often an opaque id, and a process running many runs under one
	// identity would otherwise stamp every claim identically, which is exactly what a
	// reader is trying to tell apart. A queue worker's own consumer or task id is the
	// better answer and only the caller has it. It is never verified and nothing
	// decides anything on it.
	ClaimedBy string

	// SuspendRequested reports that a graceful suspend was asked for; it is polled
	// at a loop boundary. Nil when suspension is not wired.
	SuspendRequested func() bool

	// NextPrompt continues the run interactively: after a turn reaches a boundary the
	// operator can act on (a completed turn, or one that hit the iteration cap), it is
	// called to gather the operator's next decision (see Continuation). Nil disables
	// interactive continuation, the default one-shot behavior. It is called only from
	// the single run goroutine, like the prompter.
	NextPrompt func(context.Context) Continuation

	// HumanPaced says the gap before the next call on this conversation is a person's
	// think time rather than a loop's cadence, which is what lets a provider choose a
	// cache lifetime long enough to still be there for the next turn.
	//
	// It is a property of the conversation rather than of this run, so a caller that
	// takes one turn at a time still sets it: under that model each turn is a run of its
	// own, and NextPrompt being nil says nothing about whether another turn is coming.
	// A one-shot job leaves it false, its history never being re-sent.
	HumanPaced bool

	// Provider, when non-nil, is the llm.Provider Run uses for every model call,
	// bypassing the registry lookup by llm.provider name. It lets a Go caller build a
	// provider itself (a fleet-wide rate-limiter wrapper, a test fake) and hand it in
	// rather than reach the process-global registry. When nil, Run resolves the
	// provider from the registry as the CLI does, and only then are the request
	// middlewares (HTTP debug dump, request tracer) assembled and attached: an injected
	// provider was built by the caller, who owns its hooks, so those middlewares are not
	// applied to it.
	//
	// Credential-scrub caveat: commandEnv strips llm.CredentialEnvNames, the union of
	// every REGISTERED provider's secret env vars, from every tool subprocess. An
	// injected provider was never registered, so its credentials are not in that union
	// and are not scrubbed. This is safe for the intended uses (a test fake holds no
	// credentials; a rate-limiter wrapper wraps a provider that was registered normally,
	// so its names are already in the union), but a caller injecting a hand-built
	// provider holding a live API key from an unregistered source would get no scrubbing
	// of that key from tool subprocesses.
	Provider llm.Provider

	// ToolWorkDir is the directory local command tools run in. With many runs sharing
	// one long-lived process, each run passes its own so a tool writing a relative path
	// does not collide with a sibling run's. It must be an absolute path that already
	// exists; the caller owns its lifecycle (creation and removal) and must not remove
	// it until Run has returned. Empty inherits the process working directory, the CLI's
	// behavior.
	//
	// It is collision avoidance, not confinement: it sets the tool subprocess's working
	// directory and nothing more, so a tool can still write anywhere the process uid can
	// (an absolute path, $HOME, $TMPDIR, all of which stay shared across runs). It is
	// never defaulted; a run with it empty behaves exactly as before. It does not affect
	// application introspection, which always runs in the process working directory.
	ToolWorkDir string

	// StoreDir is the base directory the persistent stores (memory, knowledge, and the
	// run journal) resolve their relative or default paths under, so runs sharing one
	// process place their state deterministically. Unlike ToolWorkDir it is not per-run
	// scratch: these stores are usually shared across runs of one identity. It must be
	// an absolute path that already exists; empty resolves as before (memory and
	// knowledge relative to the process working directory, the journal in the XDG state
	// directory). A backend that is not directory-backed ignores it, and an absolute
	// configured store directory is honored verbatim regardless of it.
	//
	// The standalone knowledge CLI must be pointed at the same base (its --store-dir
	// flag) or an absolute knowledge directory both read, or the agent reads its index
	// from a different directory than the CLI wrote it to.
	StoreDir string

	// Conns, when non-nil, is the shared connection Provider Run borrows for remote
	// tools instead of dialing NATS itself. It exists so a caller running many agents
	// in one process establishes one connection (conns.New(conns.WithNats(nc))) and
	// hands it to every run, rather than each run opening its own dial, a duplicate
	// connection named identically, and its own discovery round-trip. A borrowed
	// Provider is owned by the caller: Run uses it but never Closes it. When nil, Run
	// dials per run from cfg.NatsContext and Closes that connection at run end, so the
	// CLI path is unchanged. It is consulted only when the config declares remote_tools.
	Conns *conns.Provider

	// RAGStore, when non-nil, is the read-only knowledge store Run borrows instead of
	// opening its own. It lets a caller running many agents in one process open one
	// store (one sqlite handle and its database/sql connection pool) and share it across
	// every run, bounding the file descriptors a long-lived server accumulates. It must
	// be opened read-only (rag.Open, not rag.OpenWriter) and is safe to share: reads go
	// through the pool concurrently. A borrowed store is owned by the caller: Run uses it
	// but never Closes it, and does not re-check the index location (the caller resolved
	// it). When nil, Run opens a store per run from cfg and opts.StoreDir and closes it,
	// so the CLI path is unchanged. It is consulted only when the config enables
	// knowledge.
	RAGStore *rag.Store

	// MemoryStore, when non-nil, is the memory store Run borrows instead of building one
	// from cfg. It lets a caller running many agents in one process construct a store once
	// and share it across runs of one identity, rather than each run building its own from
	// the configured backend. It is used verbatim when set: Run does not consult the
	// configured backend, does not provision a memory.RuntimeEnv (no StoreDir rebasing, no
	// NATS connection), and never closes it (the memory.Store interface exposes no Close;
	// the caller closes the concrete backend it built). It must be safe for concurrent
	// use: the runs sharing it call it concurrently. Unlike an injected Provider it
	// holds no subprocess-facing secret, so the credential scrub does not apply to it.
	// When nil, Run builds a store per run from cfg and opts.StoreDir, the CLI path. It is
	// consulted only when the config enables memory (harness.memory); with memory disabled
	// it is ignored.
	//
	// It must be the store the configuration asks for when the configuration asks for one.
	// A config naming harness.memory.backend and a store reporting a different Info().Backend
	// name two stores, and that is a run-start error; a config naming no backend leaves the
	// choice to the caller and takes whatever is injected. The default is not a declaration,
	// so a store may be injected against an unset backend and report anything.
	MemoryStore memory.Store

	// SessionStore, when non-nil, is the run-journal store Run borrows for checkpointing
	// and resume instead of building one from cfg. A caller running many agents in one
	// process constructs one store and shares it. It is used verbatim when set: Run does
	// not consult the configured backend, does not provision a runstate.RuntimeEnv, and
	// never closes it. It must be safe for concurrent use: the runs sharing it call it
	// concurrently. When nil, Run builds a store per run from cfg and opts.StoreDir. It
	// is consulted only when the run journals (an explicit Checkpoint or a resume); an
	// un-checkpointed run never touches it, so injecting it into a one-shot run is a no-op.
	// It is accepted or refused against harness.sessions.backend on the same rule as
	// MemoryStore. Note that ApplyStateDir declares the file backend, so a --state-dir run
	// requires an injected store to be a file one.
	SessionStore runstate.Store

	// A2ATransport, when non-nil, is the a2a client transport Run borrows for importing
	// remote tools instead of constructing one from the registry over Conns. It is used
	// verbatim when set and never closed, and must be safe for concurrent use: the runs
	// sharing it call RoundTrip concurrently. When nil, Run constructs the transport per run
	// from cfg.A2ATransport() (its NAME string, not this value) over the shared connection.
	// It is consulted only when the config declares remote_tools; with none it is ignored,
	// so injecting it into a run with no remote tools is a no-op. It is a client transport:
	// Run never serves a2a, which the `agent a2a` command does instead. Do not
	// confuse it with config.Config.A2ATransport(), which returns the transport name string
	// this field replaces.
	A2ATransport a2a.Transport

	// MCPSessions, when non-nil, are the already-connected sessions with the configured
	// MCP servers that Run borrows for importing their tools instead of connecting its
	// own. A server process connects once at start and hands the same sessions to every
	// run it hosts, so a long-lived process is not starting and stopping a stdio child
	// around each run. They are used verbatim when set and never closed: the caller owns
	// them and closes them when it is done with them. They must be safe for concurrent
	// use, which mcpclient.Sessions is: the runs sharing them call tools over them
	// concurrently. When nil, Run connects to the configured servers at run start and
	// closes those sessions at run end, so the CLI path is unchanged. They are consulted
	// only when the config declares mcp_clients; with none they are ignored, so injecting
	// them into a run with no MCP servers is a no-op.
	MCPSessions *mcpclient.Sessions

	// CustomTools are application tools a Go caller injects into the run, addressed by
	// the model by name alongside the wrapped application's command tools, the built-ins
	// and any remote tools. The recommended way to build one is functool.New (whose
	// handler returns functool.Result), but any toolkit.Tool is accepted. They are the
	// programmatic counterpart to the config-declared tool sources: a caller embedding
	// the agent registers a tool its own process implements, without a wrapped binary or
	// a remote agent.
	//
	// A custom tool runs in-process with the agent's own privileges and the unscrubbed
	// ambient environment: unlike a command tool, no credential scrub applies to it or to
	// any subprocess it spawns (the commandEnv/llm.CredentialEnvNames scrub is
	// subprocess-only and never reaches in-process code). It is trusted code, not a
	// sandbox; the caller owns what it reads and what it hands to a child process.
	//
	// A name may not collide with an application, built-in, remote or MCP-imported tool,
	// nor with another custom tool: a collision aborts the run rather than shadowing one
	// (shadowing a confirm-gated tool would strip its gate). Each tool's Definition() JSON
	// (name, description, schema, deferral) should be deterministic across process
	// restarts: a checkpointed run fingerprints the tool set, and a Definition that varies
	// run to run moves that hash, so every resume warns that the tool set changed and drops
	// the standing approvals, leaving the operator to approve each gated tool again. The
	// resume itself continues. The slice order does not matter; the tools are ordered by
	// name internally, so a set built by ranging a map still fingerprints identically.
	//
	// A custom tool built by functool.New with no Trace renderer runs silently: its call
	// and result line are suppressed except under verbose, as a human-in-the-loop built-in
	// is. Set functool.Spec.Trace to have its calls traced like the memory tools. A
	// function tool is never rendered as an application-command line. A custom tool whose
	// Name, Definition, or Describe panics crashes the run as a *PanicError; the harness
	// does not sandbox it.
	CustomTools []toolkit.Tool

	// Hooks are optional Go callbacks the run invokes at fixed points in its loop, the
	// single place to observe a run, deny or adjust individual tool calls, and stop a run
	// from your own code. A nil field does not fire. They are trusted in-process code with
	// the agent's own privileges, like CustomTools: there is no sandbox, and a panic in a
	// hook aborts the run as a *PanicError (RunEnd apart, which fires once the
	// outcome is decided). See the Hooks type for the full contract.
	Hooks Hooks

	// Telemetry, when non-nil, receives the run's OpenTelemetry traces and metrics. A
	// nil field records nothing, and every method on it is nil-safe, so the run path
	// wires it up without asking whether telemetry is on and the instrumented and
	// uninstrumented paths cannot diverge.
	//
	// It is deliberately not a pair of OpenTelemetry interfaces. Nothing outside
	// internal/telemetry knows OpenTelemetry is underneath, so a caller who does not
	// already run it never imports it; one who does hands their own providers to
	// telemetry.NewFromProviders and passes the result here.
	//
	// The caller owns its lifecycle and must Shutdown it after the run to flush what
	// was recorded, on a context that is NOT derived from the run's: an interrupt would
	// otherwise cancel the flush and lose exactly the run worth seeing.
	Telemetry *telemetry.Provider
}

// Continuation is the operator's decision at an interactive turn boundary. Continue
// false ends the session. Reset clears the conversation context before the next turn,
// keeping the system prompt and tools and dropping the confirm-gate approvals with the
// conversation they were given in; with an empty Text it reopens the input for a fresh
// prompt without running a turn. Text is the next user prompt when the turn proceeds.
type Continuation struct {
	Text     string
	Reset    bool
	Continue bool
}

// Result is the outcome of a run, for the caller to render.
type Result struct {
	Reason runstate.TerminalReason

	// Stats is the run's accounting, nil when the run failed before it started. Run
	// returns a non-nil Result on those failures too, so a nil error does not promise
	// this field: check it before reading it, as serve.Outcome.Stats requires of the
	// same value one layer out.
	Stats *RunStats

	SessionID string

	// Text is the concatenated text of the last assistant turn the run produced,
	// empty when it produced none. It exists for a caller that must record an answer
	// without watching the run: Events.Message carries the same text, but only a
	// caller rendering the stream sees it, and the turn it arrives on is not marked
	// terminal when the run stops on the token budget or the iteration cap.
	//
	// It is the last turn rather than a successful answer. Pair it with Reason before
	// treating it as one: a run that exhausted its budget or was truncated at the
	// output cap still reports the text it had reached.
	Text string

	// Deferred lists the tool calls the run is waiting on an answer for, and is empty
	// for a run that stopped for any other reason. It is what tells a caller a suspend
	// it did not ask for from one it did: a drain suspends with nothing here, while a
	// run that called a tool answering later suspends naming the call.
	//
	// The run is resumable and will not proceed until every one of these is answered,
	// which runstate.SupplyToolResult is how to do.
	Deferred []DeferredCall

	// FollowUpTaken reports whether a Checkpoint.FollowUp prompt entered the
	// conversation. It is false when the stored conversation reached no boundary that
	// could take a user message, which is a conversation waiting on a deferred tool
	// result: the prompt was neither journaled nor answered, and the caller has to
	// send it again once the answer arrives. It is meaningless without
	// Checkpoint.FollowUp, where it is always false.
	FollowUpTaken bool
}

// DeferredCall is one tool call whose answer arrives later. ToolUseID is the key the
// answer is supplied against; Note and Handle are the tool's own account of what it
// is waiting on and are display text, so sanitize them before rendering.
type DeferredCall struct {
	ToolUseID string
	ToolName  string
	Note      string
	Handle    string
}

// ErrConversationNotFound reports that a resume named a session the store does not
// hold. It is separated from the store's own error so a channel serving follow-up
// turns can answer its caller with the one thing it can act on: the conversation it
// named is not here, and a new one is where its prompt has to go.
var ErrConversationNotFound = errors.New("conversation not found")

// PanicError is the error Run returns when it recovered a panic on its run goroutine.
// It reports that the run crashed rather than reaching a terminal outcome, so a caller
// (a job system) tells a crash from an outcome with errors.As and requeues or escalates
// it. Its message is deliberately generic and carries no stack: the full stack is
// delivered only through Events.Panicked, since it leaks absolute paths and frame
// arguments and must never cross to a remote peer. Value keeps the recovered value for
// a caller that wants to inspect or re-panic it.
type PanicError struct {
	value any
}

// Error is a fixed operator-facing message that names an internal crash and rules out
// the outcomes it would otherwise be confused with (a model refusal, a tool failure, a
// budget cap). It carries no stack.
func (e *PanicError) Error() string {
	return "internal error: fisk-ai crashed (a bug, not a model or tool failure); please report it"
}

// Value returns the recovered panic value, for a caller that wants to inspect or
// re-panic it.
func (e *PanicError) Value() any { return e.value }

// validateCallerDir checks a caller-owned directory option (ToolWorkDir today,
// StoreDir next): empty is allowed and preserves today's behavior, but a value that
// is set must be an absolute path that already exists as a directory. The caller
// creates and removes these directories; Run only validates them, so a mistake
// surfaces at run start with a message naming the option rather than as a confusing
// subprocess or store error later. name is the option name for the message.
func validateCallerDir(name, dir string) error {
	if dir == "" {
		return nil
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("%s must be an absolute path, got %q", name, dir)
	}

	info, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s %q does not exist; the caller must create it before the run", name, dir)
	}
	if err != nil {
		return fmt.Errorf("%s %q: %w", name, dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s %q is not a directory", name, dir)
	}

	return nil
}

// memoryInfo maps a store's self-description onto the telemetry shape. It is the whole
// mapping: internal/telemetry imports nothing from this tree, so the memory type never
// crosses into it and the two fields are carried by hand, once, here.
func memoryInfo(s memory.Store) telemetry.MemoryInfo {
	if s == nil {
		return telemetry.MemoryInfo{}
	}

	info := s.Info()

	return telemetry.MemoryInfo{Backend: info.Backend, Location: info.Location}
}

// memoryToolNames is the set of tool names the memory built-ins registered, so a tool
// span can be told whether the call it covers was served by the memory store.
//
// It is derived from the tools that were actually built rather than restated as a name
// prefix or a list of its own: this is the copy that is not authoritative, so it reads
// the real one. A memory tool added later is attributed without touching this.
func memoryToolNames(tools []*functool.Tool) map[string]bool {
	if len(tools) == 0 {
		return nil
	}

	names := make(map[string]bool, len(tools))
	for _, t := range tools {
		names[t.Name()] = true
	}

	return names
}

// startupErrorClass names the telemetry error class for a failure before the run
// loop starts. Cancellation and deadlines are told apart by their standard library
// sentinels; everything else is reported as a configuration failure, because the
// early returns it covers are overwhelmingly setup rejections: a bad directory
// option, an unresolvable provider, a tool name collision, a refused resume.
//
// It classifies rather than reporting the error itself. error.type is low-cardinality
// by spec, and these errors embed absolute paths, config values and the config file
// path, none of which may leave the process on a span headed for a backend.
func startupErrorClass(err error) telemetry.ErrorClass {
	class, ok := telemetry.ClassifyContext(err)
	if ok {
		return class
	}

	return telemetry.ClassConfig
}

// setupFailedReason is the terminal reason for a run that never reached the loop. The
// run path itself has no such outcome, because from its point of view nothing ran, but
// a trace with an empty reason reads as a bug in the instrumentation rather than as a
// refused resume or a bad config. It is the trace an operator goes looking for when a
// run is rejected in CI.
const setupFailedReason = "setup_failed"

// runOutcome assembles what the root span records about a finished run.
//
// reachedRunner separates a crash during setup from one inside the loop, which
// res.Reason cannot: a crash deliberately leaves the reason unset, because a crash is
// not an outcome, so without this every crash would be reported as a setup failure.
//
// seed is the token state a resume restored, nil for a fresh run, and seedCalls,
// seedRemoteCalls and seedMCPCalls are the tool call counts it restored. Where seed is
// set, the token and tool call attributes carry this process's own consumption and the
// session totals are reported separately, so that summing either one across a session's
// traces gives an answer that means something.
func runOutcome(res *Result, err error, reachedRunner bool, seed *telemetry.TokenUsage, seedCalls, seedRemoteCalls, seedMCPCalls int64) telemetry.RunOutcome {
	out := telemetry.RunOutcome{TerminalReason: string(res.Reason)}

	var panicErr *PanicError
	out.Crashed = errors.As(err, &panicErr)

	if out.TerminalReason == "" {
		out.TerminalReason = setupFailedReason
		if reachedRunner {
			// The loop was running and did not reach a terminal state, which today means
			// it crashed; reporting that as a setup failure would send an operator to the
			// wrong half of the run.
			out.TerminalReason = string(runstate.ReasonError)
		}
	}

	if err != nil {
		out.Failed = true
		out.Class = runErrorClass(err, reachedRunner)
	}

	if res.Stats == nil {
		return out
	}

	out.ToolCalls = res.Stats.ToolCalls
	out.RemoteToolCalls = res.Stats.RemoteToolCalls
	out.MCPToolCalls = res.Stats.MCPToolCalls
	out.Usage = telemetry.TokenUsage{
		Input:       res.Stats.InTokens + res.Stats.CacheReadTokens + res.Stats.CacheCreateTokens,
		Output:      res.Stats.OutTokens,
		CacheRead:   res.Stats.CacheReadTokens,
		CacheCreate: res.Stats.CacheCreateTokens,
		Uncached:    res.Stats.InTokens,
		Reasoning:   res.Stats.ThinkingTokens,
	}

	if seed == nil {
		return out
	}

	// A resume seeds the counters with the session's history, so what is in stats is
	// cumulative from the first instruction. The cumulative view is reported under its
	// own keys and the delta becomes this process's usage.
	session := out.Usage
	out.SessionUsage = &session
	out.SessionLLMCalls = res.Stats.LlmCalls

	out.Usage = telemetry.TokenUsage{
		Input:       session.Input - (seed.Input + seed.CacheRead + seed.CacheCreate),
		Output:      session.Output - seed.Output,
		CacheRead:   session.CacheRead - seed.CacheRead,
		CacheCreate: session.CacheCreate - seed.CacheCreate,
		Uncached:    session.Uncached - seed.Input,
		Reasoning:   session.Reasoning - seed.Reasoning,
	}
	out.ToolCalls = res.Stats.ToolCalls - seedCalls
	out.RemoteToolCalls = res.Stats.RemoteToolCalls - seedRemoteCalls
	out.MCPToolCalls = res.Stats.MCPToolCalls - seedMCPCalls

	return out
}

// runErrorClass names the telemetry error class for a run that ended on an error.
//
// It stays deliberately coarse. Naming a class needs a domain sentinel to recognize,
// and the ones that would refine this (a provider failure, a store failure) are raised
// deep in packages this classification would otherwise have to import. A wrong class is
// worse than a vague one, so anything not positively identified is the spec's catch-all
// rather than a guess, and never the error's own text: these errors embed absolute
// paths and config values, and error.type is low cardinality by spec.
func runErrorClass(err error, reachedRunner bool) telemetry.ErrorClass {
	var panicErr *PanicError
	if errors.As(err, &panicErr) {
		return telemetry.ClassPanic
	}

	class, ok := telemetry.ClassifyContext(err)
	if ok {
		return class
	}

	// Everything before the runner exists is setup: a bad directory option, an
	// unresolvable provider, a tool name collision, a refused resume.
	if !reachedRunner {
		return telemetry.ClassConfig
	}

	return telemetry.ClassOther
}

// Run loads the tools and prompt from opts.Config, sets up checkpointing and
// resume as requested, and drives the agentic loop to a terminal state. It emits
// the run's narration, tool traces and advisories through events and returns a
// Result the caller renders. Interactive decisions (confirm-gate approval and the
// human-in-the-loop questions) are put to the operator through prompter, injected
// per run so the concurrent MCP path never receives it. The returned Result is
// non-nil even on error so the caller can always print the stats. The context
// governs cancellation; a graceful suspend is requested via opts.SuspendRequested.
//
// A panic on the run goroutine is recovered and returned as a *PanicError, so one
// nil dereference cannot take down a long-lived server and every sibling run; the
// stack is delivered to events (Events.Panicked), never on the returned error. See
// the panic barrier below for the scope limits.
func Run(ctx context.Context, opts Options, events Events, prompter toolkit.Prompter) (res *Result, err error) {
	cfg := opts.Config
	res = &Result{}

	// activeRunner is nil until the runner is constructed; the panic barrier reads it to
	// report the session the run ended on, which the normal path sets only after the
	// runner returns, and the root span reads it to tell a crash during setup from one
	// inside the loop.
	var activeRunner *runner

	// resumeSeed is the counter state a resume restored, captured the instant it is
	// applied. The root span reports this process's own consumption, which means
	// subtracting what was inherited: without that, summing token usage across a
	// session's traces counts the restored prefix once per resume, so a session resumed
	// five times reports roughly fifteen times its true input tokens. It is nil for a
	// fresh run, which is also what suppresses the session-cumulative attributes.
	//
	// The three tool call seeds are the same thing for the call counts, and all three
	// are subtracted: a total with the restored prefix taken out beside per-kind counts
	// that still carried it would report more MCP calls than tool calls.
	var resumeSeed *telemetry.TokenUsage
	var resumeSeedCalls, resumeSeedRemoteCalls, resumeSeedMCPCalls int64

	// Resolved here rather than where it is first used, because the root span's
	// operation name turns on it: a one-shot run is a single agent invocation, so its
	// root is that invocation, while a chat is several invocations of one agent, which
	// is what a workflow describes, with each turn nested underneath.
	interactive := opts.NextPrompt != nil

	// A follow-up turn is refused before anything runs rather than being described in a
	// comment and left to a caller to get right, because both mistakes cost money: with
	// no session to continue there is nothing to add the turn to, and under
	// CreateIfMissing a redelivery would append the prompt again and pay for a turn on
	// it.
	if opts.Checkpoint.FollowUp {
		switch {
		case opts.Checkpoint.ResumeID == "":
			return res, fmt.Errorf("Checkpoint.FollowUp needs Checkpoint.ResumeID: a follow-up turn joins a conversation and there is none to join")
		case opts.Checkpoint.CreateIfMissing:
			return res, fmt.Errorf("Checkpoint.FollowUp cannot be set with Checkpoint.CreateIfMissing: a caller that may deliver the same work twice would append its prompt to the conversation on every redelivery")
		case strings.TrimSpace(strings.Join(opts.Prompt, " ")) == "":
			return res, fmt.Errorf("Checkpoint.FollowUp needs Options.Prompt: the follow-up turn is the prompt")
		}
	}

	if opts.Checkpoint.Answer != nil {
		switch {
		case opts.Checkpoint.ResumeID == "":
			return res, fmt.Errorf("Checkpoint.Answer needs Checkpoint.ResumeID: an answer belongs to a call in a conversation and there is none to answer")
		case opts.Checkpoint.CreateIfMissing:
			return res, fmt.Errorf("Checkpoint.Answer cannot be set with Checkpoint.CreateIfMissing: a conversation that may not exist has no call to answer")
		case opts.Checkpoint.Answer.ToolUseID == "":
			return res, fmt.Errorf("Checkpoint.Answer needs a ToolUseID: it is the call being answered")
		}
	}

	// The token is recorded where the journal is created and nowhere else, so a
	// checkpoint that creates none has nowhere to put it. It is refused rather than
	// dropped because dropping it is the failure it exists to prevent: a token nothing
	// wrote down cannot be recovered, which leaves its conversation unreachable.
	if opts.Checkpoint.ConversationToken != "" {
		switch {
		case opts.Checkpoint.FollowUp:
			return res, fmt.Errorf("Checkpoint.ConversationToken cannot be set with Checkpoint.FollowUp: the conversation this turn joins recorded its token when it was created")
		case opts.Checkpoint.Answer != nil:
			return res, fmt.Errorf("Checkpoint.ConversationToken cannot be set with Checkpoint.Answer: the conversation this answer belongs to recorded its token when it was created")
		case !opts.Checkpoint.Enabled && !opts.Checkpoint.CreateIfMissing:
			return res, fmt.Errorf("Checkpoint.ConversationToken needs a checkpoint that creates a journal: set Checkpoint.Enabled, or Checkpoint.ResumeID with Checkpoint.CreateIfMissing")
		}
	}

	// The root span is the entire run, which is what makes one run one trace. It starts
	// as the first statement so it covers every early return, and its Finish is deferred
	// immediately, before the panic barrier below: defers unwind
	// last-registered-first, so the barrier runs first and this runs last, seeing the
	// final named return values including the PanicError the barrier substituted.
	// The provider rides the context from here so subsystems that hold a ctx but are
	// not constructed per run can open spans: a2a's client today, the knowledge store
	// next. Options.Telemetry stays the injection point; this is only how it reaches
	// them, and it is per call rather than global, so concurrent runs in one process do
	// not see each other's.
	ctx = telemetry.ContextWithProvider(ctx, opts.Telemetry)

	// The memory scope rides the context for the same reason and is established here for
	// the same reason it is per call: a backend that gates an overwrite on this run
	// having read the value needs to know what "this run" read, and a store injected by a
	// host serving many runs cannot answer that from state of its own. A store built per
	// run answers it either way, so this changes nothing on the CLI path.
	//
	// It is held here as well so a resume can seed it from the journal and the runner can
	// record it, which is what carries a read across the turns of one conversation.
	memScope := memory.NewScope()
	ctx = memory.WithScope(ctx, memScope)

	ctx, runSpan := opts.Telemetry.StartRun(ctx, telemetry.RunInfo{
		Identity:    cfg.Identity,
		Interactive: interactive,
		Resumed:     opts.Checkpoint.ResumeID != "",
		Model:       cfg.LLM.Model,
	})
	defer func() {
		runSpan.Finish(runOutcome(res, err, activeRunner != nil, resumeSeed, resumeSeedCalls, resumeSeedRemoteCalls, resumeSeedMCPCalls))
	}()

	// The startup span covers everything from here to the handoff to the run loop:
	// loading tools, dialing NATS, opening the stores, importing remote tools, and
	// resolving or restoring the session. It is a child of the root, which is why the
	// root's ctx goes in.
	//
	// Its context is kept SEPARATE and never assigned to ctx. Setup now opens a child
	// span of its own (the memory index load), which needs this context, but the run
	// loop must not: reassigning ctx here would make every span the loop opens a child
	// of startup instead of the root, nesting the whole run inside a span that ended at
	// the handoff. That was a real defect once. The loop is handed ctx, the root's, and
	// anything in setup that wants to nest uses setupCtx.
	setupCtx, startupSpan := opts.Telemetry.StartStartup(ctx, telemetry.StartupInfo{
		Identity:    cfg.Identity,
		RemoteHosts: len(cfg.RemoteTools),
	})

	// startupDone is set at the handoff, where the span is ended explicitly. Until
	// then this deferred End is what closes it, and it has to exist: Run has 38 early
	// returns before the runner is constructed, and they are exactly the slow paths
	// this span is for (the NATS dial, opening the knowledge index, the remote tool
	// import). A plain deferred End at the handoff would leak every one of them, and an
	// unended span is never exported, so those runs would produce no trace at all.
	//
	// Registered before the panic barrier below, so it unwinds after it. Fail is a
	// no-op for a nil err, which is why the named return can be passed unguarded.
	startupDone := false
	defer func() {
		if startupDone {
			return
		}
		startupSpan.Fail(err, startupErrorClass(err))
		startupSpan.End()
	}()

	// Panic barrier. Registered after the root and startup spans so it runs before both
	// of them (defers are LIFO), which is what lets the root's Finish observe the
	// PanicError this substitutes. Otherwise as before: the deferred
	// stores, journal and tracer close before it, so it also catches a panic thrown by
	// one of those cleanups. It converts a panic into a PanicError the caller tells from
	// a terminal outcome with errors.As (a job system requeues or escalates a crash but
	// records an outcome), delivers the stack to the Events sink for local rendering
	// (never onto the returned error, which may cross to a remote peer and leaks absolute
	// paths and frame arguments), and leaves res.Reason unset because a crash is not an
	// outcome. Being the one point every exit passes through, crash included, it is also
	// where the RunEnd hook fires, exactly once.
	// It catches only this goroutine; the agent package spawns none today, but a
	// future goroutine would escape it, and it cannot catch a fatal runtime error
	// (concurrent map write), OOM, or runtime.Goexit, so it is not a substitute for the
	// per-run isolation the rest of this work provides.
	defer func() {
		p := recover()
		crashed := p != nil

		// The stack is captured first, while the panicking frames are still on this
		// goroutine, since the RunEnd hook below runs caller code (which may itself
		// panic) before the stack reaches the events sink.
		var stack []byte
		if crashed {
			stack = debug.Stack()

			// The normal path records the session the run ended on after the runner
			// returns; a crash skipped that, so report it here instead.
			if activeRunner != nil {
				res.SessionID = activeRunner.sessionID
				if res.Stats != nil {
					res.Stats.Session = activeRunner.sessionID
				}
			}
		}

		// RunEnd fires from here and nowhere else, so it fires exactly once for
		// every run that reached the runner, whatever ended it: completed, budget,
		// suspended, error, or this crash. A setup failure before the runner exists
		// never started a session, so it does not fire for one. It reads opts.Hooks
		// rather than the runner's copy because the runner may be nil here. On a crash
		// Err is still nil, the PanicError being set below, so a hook keys off Crashed.
		if activeRunner != nil && res.Stats != nil {
			opts.Hooks.fireRunEnd(ctx, events, RunEndInfo{
				SessionID: res.SessionID,
				Reason:    res.Reason,
				Crashed:   crashed,
				Err:       err,
				Stats:     *res.Stats,
			})
		}

		if !crashed {
			return
		}

		// Panicked renders caller-supplied code during unwind; a panic in it must not
		// escape and crash the process the barrier exists to protect.
		func() {
			defer func() { recover() }()
			events.Panicked(p, stack)
		}()

		err = &PanicError{value: p}
	}()

	// Directory options are validated before anything runs, so a bad path fails with a
	// clear message rather than a confusing subprocess ENOENT partway through. The
	// caller owns these directories; Run only checks them.
	if err := validateCallerDir("tool_work_dir", opts.ToolWorkDir); err != nil {
		return res, err
	}
	if err := validateCallerDir("store_dir", opts.StoreDir); err != nil {
		return res, err
	}

	tools, err := fisk.LoadTools(ctx, cfg)
	if err != nil {
		return res, err
	}

	byName := make(map[string]*fisk.FiskCommandTool, len(tools))
	for _, t := range tools {
		byName[t.Name()] = t
	}

	// taken tracks every tool name already claimed (local tools, then built-ins, then
	// the tools imported from remote agents and from MCP servers), so a clash across
	// those namespaces is caught rather than silently shadowing one with another, since
	// the model addresses every tool by a single flat name.
	taken := make(map[string]bool, len(tools))
	for name := range byName {
		taken[name] = true
	}

	// Built-in human-in-the-loop tools are injected only here, in the agent run
	// path, so they are never reachable over MCP where there is no operator. They
	// are never deferred, so enabling them neither hides them behind tool search
	// nor changes how the application tools are presented.
	builtins := builtin.HITLTools(cfg)
	builtinByName := make(map[string]*functool.Tool, len(builtins))
	for _, b := range builtins {
		if taken[b.Name()] {
			return res, fmt.Errorf("human_in_the_loop adds a built-in tool %q but the application already exposes a tool with that name; exclude or rename it", b.Name())
		}
		builtinByName[b.Name()] = b
		taken[b.Name()] = true
	}

	if len(builtins) > 0 && !prompter.CanPrompt() {
		events.Warn(Warning{Kind: WarnHITLNoTerminal})
	}

	// Whether this run journals its state: an explicit --checkpoint, or a --resume of
	// a stored session. It gates both the session-store dial just below and the store
	// construction further down, so it is resolved once here, above the dial.
	checkpointing := opts.Checkpoint.Enabled || opts.Checkpoint.ResumeID != ""

	// Acquire the shared NATS connection once, ahead of every subsystem that needs
	// it: the jetstream memory backend just below, the jetstream session store, and
	// remote tools further down. A run that uses several of them then establishes a
	// single connection. A caller-injected Provider is borrowed: the caller
	// established it and shares it across concurrent runs, so Run uses it but never
	// Closes it. Only a connection Run dials itself is owned and released here; dialing
	// per run is the CLI path.
	// An injected store or transport is self-contained (the caller provisioned it), so
	// it must not force Run to dial, so each term is gated on nothing being injected.
	memNeedsNats := opts.MemoryStore == nil && memory.NeedsNats(cfg)
	// A jetstream session store only needs NATS when the run actually journals: an
	// un-checkpointed run stores no session, so gate the dial on checkpointing rather
	// than dialing (and failing on) NATS for a run that never touches the store.
	sessionNeedsNats := opts.SessionStore == nil && checkpointing && runstate.NeedsNats(cfg.SessionBackend())
	transportNeedsNats := opts.A2ATransport == nil && len(cfg.RemoteTools) > 0
	var natsConns *conns.Provider
	if opts.Conns != nil {
		natsConns = opts.Conns
	} else if memNeedsNats || sessionNeedsNats || transportNeedsNats {
		p, err := conns.Connect(cfg.NatsContext, cfg.Identity)
		if err != nil {
			return res, fmt.Errorf("connecting to NATS: %w", err)
		}
		defer p.Close()
		natsConns = p
	}

	// Built-in memory tools are added here in the agent run path too, but tracked
	// in their own slice so they never perturb the human-in-the-loop system note or
	// its no-terminal warning. They are pure (no operator), and like the HITL tools
	// they are not reachable over MCP. The store is built now so a misconfiguration
	// (unknown backend, bad options, an unwritable directory or an unusable KV
	// bucket) fails before the loop. natsConns.Nats() is nil-safe and yields nil for
	// a backend that needs no connection (the file backend ignores it).
	var memStore memory.Store
	var memBuiltins []*functool.Tool
	if cfg.MemoryEnabled() {
		// A caller-injected store is borrowed: a fleet shares one store across runs of one
		// identity rather than each building its own. It is used verbatim (no configured
		// backend, no RuntimeEnv, never closed).
		//
		// An operator who named a backend gets the store they asked for, so an injected
		// one must be that store: it is refused when it reports running on something else,
		// which names two stores. Naming none leaves the choice to the caller, so any
		// injected store is accepted and the default is never a declaration.
		if opts.MemoryStore != nil {
			declared := cfg.MemoryBackendDeclared()
			running := opts.MemoryStore.Info().Backend
			if declared != "" && declared != running {
				return res, fmt.Errorf("Options.MemoryStore runs on the %q backend but harness.memory.backend in %q selects %q: an injected store must be the store the configuration asks for; build it from this configuration, or set harness.memory.backend to %q", running, opts.ConfigFile, declared, running)
			}
			memStore = opts.MemoryStore
		} else {
			memStore, err = memory.New(cfg, memory.RuntimeEnv{StoreDir: opts.StoreDir, Nats: natsConns.Nats()})
			if err != nil {
				return res, err
			}
		}

		// Asked of the store, not of the config. They agree for every configured
		// backend and disagree for an injected one, where the config still says "file"
		// while something else entirely is serving the tools.
		startupSpan.SetMemory(memoryInfo(memStore))

		memBuiltins = builtin.MemoryTools(cfg, memStore)
		for _, b := range memBuiltins {
			if taken[b.Name()] {
				return res, fmt.Errorf("memory adds a built-in tool %q but the application already exposes a tool with that name; exclude or rename it", b.Name())
			}
			builtinByName[b.Name()] = b
			taken[b.Name()] = true
		}
	}

	// The built-in knowledge tools are added here in the agent run path too,
	// tracked in their own slice like the memory tools. rag.Open validates the config
	// (a bad embeddings block fails before the loop) but treats a missing index file
	// as a soft empty state, so a first run never fails to start. The store is opened
	// read-only; knowledge index is the writer.
	var ragStore *rag.Store
	var ragBuiltins []*functool.Tool
	if cfg.RAGEnabled() {
		// A caller-injected store is borrowed: a fleet shares one read-only store (one
		// sqlite handle and its database/sql pool) across every run rather than each
		// opening its own, bounding file descriptors in a long-lived server. It is owned
		// by the caller, so Run neither closes it nor re-checks the index (the caller
		// resolved its location). Otherwise Run opens per run, as the CLI does, and closes
		// it. rag.Open validates the config (a bad embeddings block fails before the loop)
		// but treats a missing index file as a soft empty state, so a first run never fails
		// to start. The store is opened read-only; knowledge index is the writer.
		ragStore = opts.RAGStore
		if ragStore == nil {
			ragStore, err = rag.Open(cfg, opts.StoreDir, rag.Options{})
			if err != nil {
				return res, err
			}
			defer ragStore.Close()

			// A missing index is a soft state (first run), but with a store base set the
			// caller expected an index there; most often the knowledge CLI wrote it elsewhere
			// under a different base. Surface it, since knowledge_search would otherwise
			// silently return nothing.
			if opts.StoreDir != "" && !ragStore.Built() {
				events.Warn(Warning{Kind: WarnKnowledgeIndexAbsent, Name: ragStore.Path()})
			}
		}

		ragBuiltins = builtin.RAGTools(cfg, ragStore)
		for _, b := range ragBuiltins {
			if taken[b.Name()] {
				return res, fmt.Errorf("knowledge adds a built-in tool %q but the application already exposes a tool with that name; exclude or rename it", b.Name())
			}
			builtinByName[b.Name()] = b
			taken[b.Name()] = true
		}
	}

	// Import remote tools, if any, before building the request tool set. A run is
	// strict: a named remote agent that cannot be reached or imported aborts the
	// run rather than silently dropping tools the prompt may depend on. The
	// connection is held open for the whole run since each remote tool call uses it.
	var remoteTools []*functool.Tool
	remoteByName := map[string]*functool.Tool{}
	if len(cfg.RemoteTools) > 0 {
		// A caller-injected transport is borrowed: a fleet shares one client transport
		// across runs rather than each constructing its own. It is used verbatim and never
		// closed (the caller owns it). Otherwise the shared connection acquired above
		// (natsConns, borrowed or owned) is used to construct the registry transport, which
		// reuses it rather than dialing a second time.
		transport := opts.A2ATransport
		if transport == nil {
			transportName := cfg.A2ATransport()
			transport, err = a2a.NewTransport(transportName, natsConns, a2a.TransportConfig{Identity: cfg.Identity, Timeout: cfg.A2ARequestTimeout()})
			if err != nil {
				return res, err
			}
		}

		client, err := a2a.NewClient(transport, cfg.Identity, a2a.WithIdleTimeout(cfg.A2ARequestTimeout()))
		if err != nil {
			return res, err
		}

		var imports []remotetools.HostImport
		remoteTools, remoteByName, imports, err = remotetools.ImportForRun(ctx, client, cfg, taken)
		if err != nil {
			return res, err
		}
		reporter, ok := events.(RemoteHostReporter)
		if ok {
			reporter.RemoteHostNotes(imports)
		}
	}

	// Import the tools of the configured MCP servers, after the remote agents so a
	// name is checked against everything claimed so far. A run is as strict about a
	// server as it is about a remote agent: one that cannot be started, reached or
	// listed aborts the run rather than dropping tools the prompt may depend on. A tool
	// the server described badly does not abort it, since the server answered: that one
	// is skipped and reported.
	var mcpTools []*functool.Tool
	var mcpSessions *mcpclient.Sessions
	var mcpImports []mcpclient.ServerImport
	mcpByName := map[string]*functool.Tool{}
	if len(cfg.MCPClients) > 0 {
		// Caller-injected sessions are borrowed: a server process connects once and hands
		// the same sessions to every run it hosts rather than starting a stdio child around
		// each one. They are used verbatim and never closed (the caller owns them).
		// Otherwise Run connects here and closes at the end of the run, the CLI path.
		// setupCtx, not ctx: the per-server connect and import spans nest under startup,
		// as the memory index load does. A run given sessions somebody else opened opens
		// no connect span, since the connect happened before this run existed.
		sessions := opts.MCPSessions
		if sessions == nil {
			sessions, err = mcpclient.Connect(setupCtx, mcpclient.Options{
				Servers:            cfg.MCPClients,
				Identity:           cfg.Identity,
				Version:            opts.Version,
				CredentialEnvNames: cfg.CredentialEnvNames(),
			})
			if err != nil {
				return res, err
			}
			defer sessions.Close()
		}

		// The import walks the server list the sessions carry, not cfg.MCPClients, so a
		// set opened from another configuration would import its servers under its
		// aliases and filters in a run that never declared them. That is refused rather
		// than substituted. The check is here rather than in the host that injects
		// because this is the one path every injected set reaches the import through, so
		// a second host, or an embedder passing Options.MCPSessions directly, gets it
		// without arranging anything.
		err = sessions.CheckServers(cfg.MCPClients)
		if err != nil {
			return res, err
		}

		mcpSessions = sessions

		// The names the remote import settled on are never written into taken, so the
		// naming pass is given both lookups. The names this import settles on are added to
		// taken below, which keeps it the whole set of claimed names.
		mcpTools, mcpByName, mcpImports, err = mcpclient.ImportForRun(setupCtx, sessions, mcpclient.NewClaimedNames(taken, remoteByName))

		// ImportForRun returns the per-server outcomes with its error as well as without
		// one, so the notes are reported before the error is: an operator deciding whether
		// to set an alias or drop a filter needs the skipped tools and round trips that came
		// back with the failure, not the failure alone.
		reporter, ok := events.(MCPServerReporter)
		if ok {
			reporter.MCPServerNotes(mcpImports)
		}

		if err != nil {
			return res, err
		}

		for name := range mcpByName {
			taken[name] = true
		}
	}

	// Caller-injected custom tools are registered last, after every other source has
	// claimed its names, so a collision is caught against all of them and the clashing
	// kind can be named. A custom tool may never shadow an existing one: shadowing a
	// confirm-gated command would strip its gate, so this aborts the run like every
	// other name clash rather than silently replacing a tool.
	customByName := make(map[string]toolkit.Tool, len(opts.CustomTools))
	for i, t := range opts.CustomTools {
		if t == nil {
			return res, fmt.Errorf("custom tool at index %d is nil", i)
		}

		name := t.Name()
		if name == "" {
			return res, fmt.Errorf("custom tool at index %d has an empty name", i)
		}

		// The model addresses a tool by its Definition name but the runner dispatches on
		// Name(); a mismatch would advertise a tool the model could call but the runner
		// could not find. Reject it here rather than leave it silently unreachable.
		if defName := t.Definition(false).Name; defName != name {
			return res, fmt.Errorf("custom tool %q reports Definition name %q; Name() and Definition().Name must match", name, defName)
		}

		switch {
		case byName[name] != nil:
			return res, fmt.Errorf("custom tool at index %d (%q) collides with an existing application tool of the same name; a custom tool may not shadow it", i, name)
		case builtinByName[name] != nil:
			return res, fmt.Errorf("custom tool at index %d (%q) collides with an existing built-in tool of the same name; a custom tool may not shadow it", i, name)
		case remoteByName[name] != nil:
			return res, fmt.Errorf("custom tool at index %d (%q) collides with an existing remote tool of the same name; a custom tool may not shadow it", i, name)
		case mcpByName[name] != nil:
			return res, fmt.Errorf("custom tool at index %d (%q) collides with a tool of the same name imported from an mcp server; a custom tool may not shadow it", i, name)
		case customByName[name] != nil:
			return res, fmt.Errorf("custom tool at index %d (%q) duplicates an earlier custom tool of the same name", i, name)
		}

		// A custom tool runs in-process, so it may not claim a provider whose work
		// happens elsewhere. KindRemote is journaled remote and recomputed into the
		// remote-call counters on resume, and KindMCP owns its own bucket in the per-kind
		// accounting; either one declared by an injected tool reports work this process
		// did as work a peer did. The check is on the kind rather than the presentation
		// because the kind is what the accounting reads.
		d, ok := t.(toolkit.Describer)
		if ok {
			switch d.Describe(json.RawMessage("{}")).Kind {
			case toolkit.KindRemote:
				return res, fmt.Errorf("custom tool %q declares the remote kind; injected tools run in-process and may not be accounted as another agent's", name)
			case toolkit.KindMCP:
				return res, fmt.Errorf("custom tool %q declares the mcp kind; injected tools run in-process and may not be accounted as an MCP server's", name)
			}
		}

		customByName[name] = t
		taken[name] = true
	}

	// The run needs at least one callable tool, counting every source the model can
	// address: filtered application tools, the built-in HITL/memory/knowledge tools,
	// tools imported from remote agents and from MCP servers, and caller-injected custom
	// tools. Checking only the application tools would abort a run whose sole tools are
	// native (e.g. knowledge_search), imported, or injected by the caller.
	if len(tools)+len(builtins)+len(memBuiltins)+len(ragBuiltins)+len(remoteTools)+len(mcpTools)+len(opts.CustomTools) == 0 {
		if cfg.ApplicationPath == "" {
			return res, fmt.Errorf("no tools available: this agent wraps no application (application_path unset) and enables no built-in, remote or mcp tools; set application_path, or enable harness.knowledge, harness.memory, human_in_the_loop, remote_tools or mcp_clients in %q", opts.ConfigFile)
		}
		return res, fmt.Errorf("no tools available after filtering; check include/exclude in %q", opts.ConfigFile)
	}

	// The confirm gate enforces confirmation tags: a tool carrying ai:confirm (always
	// on) or any tag listed in confirm_tags must be approved by the operator before
	// each run, with an "allow for the conversation" answer remembered for the rest of
	// it: for the rest of the process on a run that does not journal, and across every
	// later resume on one that does. It is independent of human_in_the_loop. With no
	// terminal there is no operator to ask, so a gated tool can never be approved and
	// will always be declined; warn loudly, naming the count, since otherwise those
	// commands would silently fail mid-run.
	//
	// The source is built here with the gate and given its journal appender once the
	// runner exists, which is also where a resume seeds the grants it inherited.
	approvals := newJournalApprovals()
	gate := NewConfirmGate(prompter, approvals)
	confirmTags := cfg.ConfirmTags()
	confirmTools := 0
	for _, t := range tools {
		if t.NeedsConfirm(confirmTags) {
			confirmTools++
		}
	}
	// A custom tool is gated exactly as the runner gates it: it opts into confirmation
	// through toolkit.Confirmable, so count the same interface the gate consults, else a
	// gated injected tool would go uncounted and the no-operator advisory would undercount.
	for _, t := range opts.CustomTools {
		if c, ok := t.(toolkit.Confirmable); ok && c.NeedsConfirm(confirmTags) {
			confirmTools++
		}
	}
	if confirmTools > 0 && !prompter.CanPrompt() {
		events.Warn(Warning{Kind: WarnConfirmNoTerminal, Count: confirmTools})
	}

	// A configured confirm tag that matches no loaded tool is almost always a typo;
	// left unreported it gives a false sense of safety, since the operator believes a
	// command is gated when nothing actually carries the tag. Warn per unmatched tag.
	for _, tag := range confirmTags {
		matched := false
		for _, t := range tools {
			if slices.Contains(t.Tags(), tag) {
				matched = true
				break
			}
		}
		if !matched {
			events.Warn(Warning{Kind: WarnConfirmTagUnmatched, Name: tag})
		}
	}

	// A tag under the reserved ai: namespace that the harness does not know does
	// nothing at all, and a command carrying one is indistinguishable from a correctly
	// tagged command until someone says so. Contradictory behavior tags are reported
	// for the same reason: the tool still runs, resolved the more dangerous way, but
	// its author asked for two things and got one.
	for _, t := range tools {
		unknown, conflicting := toolkit.TagIssues(t)
		if len(unknown) > 0 {
			events.Warn(Warning{Kind: WarnUnknownReservedTag, Name: t.Name(), Params: unknown})
		}
		if len(conflicting) > 0 {
			events.Warn(Warning{Kind: WarnBehaviorTagConflict, Name: t.Name(), Params: conflicting})
		}
	}

	prompt := opts.Prompt
	if len(prompt) == 0 {
		prompt = []string{"assist the user"}
	}

	// TraceID is set from the root span so the run's own summary points at the trace it
	// produced. It is empty when telemetry is off, which is what keeps it off the line.
	// ContentExported is read off the provider for the same reason and one more: it is a
	// privacy marker, so it has to report what happened rather than what was asked for.
	stats := &RunStats{
		Start:           time.Now(),
		Model:           cfg.LLM.Model,
		TraceID:         runSpan.TraceID(),
		ContentExported: opts.Telemetry.CaptureEnabled(),
	}
	res.Stats = stats

	// The provider owns the wire call. When the caller injected one on Options it is
	// used as-is: it was built by the caller, who owns its request hooks, so the tracer
	// and HTTP-debug middlewares assembled below apply only to the registry path.
	// Otherwise the provider is resolved from the registry by name rather than
	// constructed directly, so a second backend is linked in the same way the a2a,
	// memory and session backends are; the name comes from llm.provider, which defaults
	// to anthropic, the only provider linked in today.
	provider := opts.Provider
	if provider == nil {
		if opts.BaseURL != "" {
			if err := sanitize.BaseURL("--base-url / ANTHROPIC_BASE_URL", opts.BaseURL); err != nil {
				return res, err
			}
		}

		// The provider's cross-cutting request hooks (the HTTP debug dump and the request
		// tracer) are assembled here, where their lifecycle lives: the tracer's summary
		// and close are deferred against this run's stats and exit paths.
		var middlewares []llm.Middleware
		if opts.HTTPDebugOut != nil {
			middlewares = append(middlewares, HttpDebugMiddleware(opts.HTTPDebugOut))
		}

		if opts.TraceFile != "" {
			tracer, terr := NewTracer(opts.TraceFile, func(err error) {
				events.Warn(Warning{Kind: WarnTraceWrite, Err: err})
			}, nil)
			if terr != nil {
				return res, terr
			}
			// Close runs last; the summary line is written just before it. Both are
			// deferred so they fire on every exit path, including errors.
			defer func() {
				if cerr := tracer.Close(); cerr != nil {
					events.Warn(Warning{Kind: WarnTraceClose, Err: cerr})
				}
			}()
			defer tracer.RecordSummary(stats)

			tracer.RecordSession(cfg.LLM.Model, opts.ConfigFile, opts.Version)
			middlewares = append(middlewares, tracer.Middleware)
		}

		// Appended last, which puts it innermost: the SDK runs the first element
		// outermost, so anything after this one would have its work charged to the
		// attempt duration this records. It annotates the chat span it finds on the
		// request context and is inert when there is none, so it is installed
		// unconditionally like every other telemetry call site.
		middlewares = append(middlewares, telemetry.HTTPMiddleware())

		provider, err = llm.NewProvider(cfg.LLMProvider(), llm.Config{
			APIKey:      opts.APIKey,
			BaseURL:     opts.BaseURL,
			Timeout:     cfg.LLM.Budget.CallTimeoutParsed,
			Middlewares: middlewares,
		})
		if err != nil {
			return res, err
		}
	}

	// Large tool sets are deferred and discovered via the tool search tool; small
	// ones are sent directly. Deferral is decided over the combined local and remote
	// set. Built-in tools are appended after, never deferred. Deferral is offered only
	// when the resolved provider supports tool search and the operator has not disabled
	// it, so a backend that cannot honor deferred loading always gets every tool direct.
	caps := provider.Capabilities()

	// Read off the backend actually in use rather than off the config: an injected
	// provider never went through the registry, so the config would report what was
	// asked for while this reports what ran.
	runSpan.SetProvider(caps.SemconvProviderName())

	toolSearchAllowed := caps.SupportsToolSearch && cfg.ToolSearchEnabled()

	// The deferrable tools are assembled in three parts rather than one list, because a
	// server changing its tool list replaces the middle part and leaves the two either
	// side of it alone.
	beforeMCP := make([]toolkit.Tool, 0, len(tools)+len(remoteTools))
	for _, t := range tools {
		beforeMCP = append(beforeMCP, t)
	}
	for _, rt := range remoteTools {
		beforeMCP = append(beforeMCP, rt)
	}
	// Custom tools are appended in name order rather than the caller's slice order, so the
	// tool set the run fingerprints is identical whether the caller built the slice in a
	// fixed order or by ranging a map. Each honors deferral through its own Definition (a
	// tool built to never defer stays direct even inside a deferred set), like the
	// application tools, so they need no special handling here.
	customNames := make([]string, 0, len(customByName))
	for name := range customByName {
		customNames = append(customNames, name)
	}
	slices.Sort(customNames)
	afterMCP := make([]toolkit.Tool, 0, len(customNames))
	for _, name := range customNames {
		afterMCP = append(afterMCP, customByName[name])
	}

	deferrable := make([]toolkit.Tool, 0, len(beforeMCP)+len(mcpTools)+len(afterMCP))
	deferrable = append(deferrable, beforeMCP...)
	for _, mt := range mcpTools {
		deferrable = append(deferrable, mt)
	}
	deferrable = append(deferrable, afterMCP...)
	// The built-in tools in the order their definitions follow the deferrable ones:
	// human-in-the-loop, then memory, then knowledge. They are kept in their own slices
	// above so that neither the HITL system note nor its no-terminal warning sees the
	// others.
	builtinTools := make([]toolkit.Tool, 0, len(builtins)+len(memBuiltins)+len(ragBuiltins))
	for _, b := range builtins {
		builtinTools = append(builtinTools, b)
	}
	for _, b := range memBuiltins {
		builtinTools = append(builtinTools, b)
	}
	for _, b := range ragBuiltins {
		builtinTools = append(builtinTools, b)
	}

	// The definitions the model is offered and the registry the runner dispatches on
	// are built together from one list, so neither can name a tool the other does not.
	// The source is what the runner reads before each model call, and a configured MCP
	// server reporting that its tool list changed is what publishes to it.
	toolSet := NewToolSet(deferrable, builtinTools, toolSearchAllowed)
	toolSrc := NewToolSource(toolSet)

	// A server can tell its session that its tool list changed at any point in the run.
	// The rebuild happens on that session's goroutine, so the advisory it raises is
	// queued for the loop to report from the run goroutine, where every other one is
	// reported from.
	//
	// The registration is dropped when the run returns. Sessions the caller injected
	// back every run a process hosts and outlive this one, so a run that left its
	// registration behind would go on rebuilding a tool set nobody reads.
	mcpWarnings := &warnQueue{}
	if mcpSessions != nil {
		live := newLiveMCPTools(liveMCPSetup{
			Source:            toolSrc,
			Caller:            mcpSessions,
			Warnings:          mcpWarnings,
			Imports:           mcpImports,
			Claimed:           taken,
			Remote:            remoteByName,
			Before:            beforeMCP,
			After:             afterMCP,
			Builtins:          builtinTools,
			ToolSearchAllowed: toolSearchAllowed,
		})

		stopWatching := mcpSessions.OnToolListChanged(live.changed)
		defer stopWatching()
	}

	// A tool set that crosses the tool-search threshold but cannot use tool search is
	// sent to the model in full every request, spending context the search tool exists
	// to save. That is a silent degradation worth surfacing. The runner asks again as
	// the set moves, and reports it once for the run, so a set that already crossed
	// here is not reported a second time.
	toolSearchWarned := false
	if w := toolSearchDegradation(len(toolSet.defs), caps, cfg.ToolSearchEnabled()); w != nil {
		events.Warn(*w)
		toolSearchWarned = true
	}

	// The first point where every tool source has resolved, which is why this is a
	// setter on the span rather than an argument to it: the span had to start far
	// earlier, before the work that can fail on the way here.
	//
	// These are the counts the run started with. A set that moves later does not
	// rewrite them: this is the startup span, and what it reports is what startup
	// resolved.
	startupSpan.SetTools(telemetry.ToolCounts{
		Application: len(tools),
		Builtin:     len(builtins) + len(memBuiltins) + len(ragBuiltins),
		Remote:      len(remoteTools),
		MCP:         len(mcpTools),
		Custom:      len(customByName),
		Deferred:    toolSet.search,
	})

	messages := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Text: &llm.TextBlock{Text: strings.Join(prompt, " ")}}}},
	}

	maxIter := cfg.LLM.Budget.MaxIterations
	maxTokens := cfg.LLM.Budget.MaxTokens

	// The system prompt is the user's prompt, plus a note about reaching the
	// operator when the human-in-the-loop tools are enabled: the agent loop ends on
	// a text-only turn, so without it the model tends to "ask the user" in prose and
	// silently end the run instead of calling a tool. It is constant across
	// iterations, so build it once.
	system := []string{cfg.SystemPrompt}
	if note := builtin.HITLSystemNote(builtins); note != "" {
		system = append(system, note)
	}
	if note := builtin.MemorySystemNote(cfg); note != "" {
		system = append(system, note)
	}
	if note := builtin.RAGSystemNote(cfg); note != "" {
		system = append(system, note)
	}

	// The per-call output cap is constant across iterations, so resolve it once. It is
	// raised only for a run that will actually think: a run that asked for thinking to
	// be turned off needs the room no more than one that asked for nothing.
	thinking := thinkingMode(cfg)
	maxOutputTokens := resolveMaxOutputTokens(cfg, thinking == llm.ThinkingOn)

	// The feature switches belong here rather than repeated on every model call, where
	// they would cost export bandwidth per span and carry no extra information. The
	// tool-search value is what was actually decided for the set the run starts with,
	// not what the operator allowed: the provider has to support it and the tool count
	// has to cross the threshold. A set that moves mid-run re-decides it per call
	// without rewriting this.
	runSpan.SetMaxTokens(maxOutputTokens)
	runSpan.SetFeatures(thinking == llm.ThinkingOn, cfg.PromptCacheEnabled(), toolSet.search)

	// checkpointing was resolved above the NATS dial (the session store it gates
	// depends on that connection); interactive was resolved at the top, where the root
	// span needed it.
	info := RunInfo{
		Tools:           len(toolSet.defs),
		ThinkingEnabled: cfg.ThinkingEnabled(),
		ConfirmTools:    confirmTools,
		ConfirmTags:     confirmTags,
		TraceFile:       opts.TraceFile,
		NoApplication:   cfg.ApplicationPath == "",
	}

	var (
		journal               runstate.Journal
		seq                   uint64
		startIter             int64
		pending               *runstate.PendingTurn
		sessionID             string
		resumeAtInputBoundary bool
		followUpAtStart       bool
		newSession            func(prompt string) (runstate.Journal, string, error)
		store                 runstate.Store
		rs                    *runstate.RunState
	)

	// Resolve the session id up front, before any session is created or opened, so
	// RunStart can carry it and an aborting hook leaves no orphan session. A resume
	// reuses the id it was asked to continue; a fresh checkpointed run takes its configured
	// name or a fresh id (generated once here, then reused when the session is created); a
	// non-checkpointed run has none.
	switch {
	case opts.Checkpoint.ResumeID != "":
		sessionID = opts.Checkpoint.ResumeID
	case opts.Checkpoint.Enabled:
		sessionID = opts.Checkpoint.Name
		if sessionID == "" {
			sessionID = a2a.NewID()
		}
	}

	// resuming is whether this run continues a stored session. It is not the same
	// question as whether a resume was asked for: under CreateIfMissing the store is
	// the authority, and it is asked here rather than at the branch further down
	// because the two hooks below turn on the same answer.
	resuming := opts.Checkpoint.ResumeID != ""

	// The store is built, and a resumed session read, before either hook fires. That
	// ordering is what RunStart's contract already promises (it fires before any
	// session is created or opened), and it is what lets a create-or-resume know which
	// of the two it is doing. The cost is that a resume naming a session no store has
	// fails before the hooks rather than after.
	if checkpointing {
		// A caller-injected store is borrowed: a fleet shares one session store across runs
		// rather than each building its own. It is used verbatim (no configured backend, no
		// RuntimeEnv, never closed). It is accepted or refused on the same rule as an
		// injected memory store, for the same reason.
		if opts.SessionStore != nil {
			declared := cfg.SessionBackendDeclared()
			running := opts.SessionStore.Info().Backend
			if declared != "" && declared != running {
				return res, fmt.Errorf("Options.SessionStore runs on the %q backend but harness.sessions.backend in %q selects %q: an injected store must be the store the configuration asks for; build it from this configuration, or set harness.sessions.backend to %q", running, opts.ConfigFile, declared, running)
			}
			store = opts.SessionStore
		} else {
			store, err = runstate.New(cfg.SessionBackend(), cfg.SessionRawOptions(), runstate.RuntimeEnv{StoreDir: opts.StoreDir, Nats: natsConns.Nats()})
			if err != nil {
				return res, err
			}
		}

		if resuming {
			loaded, lerr := store.Load(sessionID)
			switch {
			case errors.Is(lerr, runstate.ErrNotFound) && opts.Checkpoint.CreateIfMissing:
				resuming = false
			case errors.Is(lerr, runstate.ErrNotFound):
				return res, fmt.Errorf("%w %q: %w", ErrConversationNotFound, sessionID, lerr)
			case lerr != nil:
				return res, lerr
			default:
				rs = loaded
			}
		}

		// A session that has already completed is refused below, because a finished
		// conversation cannot be continued. Under CreateIfMissing it is answered
		// instead: a caller naming a run and asking for it either way is asking for its
		// answer, and the answer is journaled. An at-least-once caller whose
		// acknowledgement was lost would otherwise be told its own completed work is an
		// error on every redelivery, and would keep paying deliveries to be told so.
		//
		// Nothing runs, so nothing is narrated: no hook fires and no event is emitted.
		// The counters are the stored run's, since they are what the work cost.
		if resuming && rs.Completed() && opts.Checkpoint.CreateIfMissing {
			res.SessionID = sessionID
			res.Reason = rs.Terminal.Reason
			res.Text = lastAssistantText(rs.Messages)

			stats.Session = sessionID
			stats.LlmCalls = rs.Counters.LlmCalls
			stats.ToolCalls = rs.Counters.ToolCalls
			stats.RemoteToolCalls = rs.Counters.RemoteToolCalls
			stats.MCPToolCalls = rs.Counters.MCPToolCalls
			stats.ToolCallsByKind = maps.Clone(rs.Counters.ToolCallsByKind)
			stats.InTokens = rs.Counters.InTokens
			stats.OutTokens = rs.Counters.OutTokens
			stats.CacheReadTokens = rs.Counters.CacheReadTokens
			stats.CacheCreateTokens = rs.Counters.CacheCreateTokens
			stats.ThinkingTokens = rs.Counters.ThinkingTokens

			return res, nil
		}
	}

	// harness.pii wraps the caller's hooks rather than replacing them, and is installed
	// here so every path into the loop is covered without a wiring site remembering to
	// ask for it. It is built before the first prompt can enter and closed with the run.
	guard, err := newPIIGuard(cfg, events)
	if err != nil {
		return res, err
	}
	defer guard.close()
	opts.Hooks = guard.wrap(opts.Hooks)

	// RunStart fires once as the run begins, on a fresh run and a resume alike (Resumed
	// distinguishes them), before any session is created or opened and before the first
	// model call, so an aborting hook leaves nothing behind. ToolNames lists every tool the
	// model can address, including those deferred behind tool search.
	toolNames := make([]string, 0, len(taken))
	for name := range taken {
		toolNames = append(toolNames, name)
	}
	slices.Sort(toolNames)

	err = opts.Hooks.fireRunStart(ctx, RunStartInfo{
		SessionID:   sessionID,
		Resumed:     resuming,
		Interactive: interactive,
		Model:       cfg.LLM.Model,
		ToolNames:   toolNames,
	})
	if err != nil {
		return res, fmt.Errorf("RunStart hook: %w", err)
	}

	// The initial prompt enters the conversation now, on a fresh run only (a resume
	// reconstructs its history and does not re-fire the hook), ordered after RunStart.
	// A Deny stops the run before any session is created or any model call is made; it is
	// surfaced as an error so the caller exits with the reason. To reject the prompt the
	// hook sets Deny; a returned error instead aborts the run.
	if !resuming {
		dec, uerr := opts.Hooks.fireUserPromptSubmit(ctx, UserPromptSubmitInfo{
			Text:    strings.Join(prompt, " "),
			Initial: true,
		})
		if uerr != nil {
			return res, fmt.Errorf("UserPromptSubmit hook: %w", uerr)
		}
		if dec.Deny {
			res.Reason = runstate.ReasonError
			return res, fmt.Errorf("the initial prompt was rejected by a policy hook: %s", dec.DenyReason)
		}
		// A rewrite has to reach both the slice and the conversation. messages was seeded
		// from the same prompt well before this point, and it is what goes to the model
		// and what content capture exports, while the slice is what Meta.Prompt and a
		// follow-up turn read. Changing one and not the other would send the model the
		// text a hook asked to have removed and store the text it asked to keep.
		if dec.Rewrite != "" {
			prompt = []string{dec.Rewrite}
			messages[0] = llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Text: &llm.TextBlock{Text: dec.Rewrite}}}}
		}
	}

	if checkpointing {
		// The tool set as the run starts, which is what the fingerprint records: it
		// captures the configuration the run began with, and ToolsDiff compares one
		// run's start to another's. A set that moves during the run does not
		// retroactively change what was written down.
		fp, err := computeFingerprint(cfg, provider.Capabilities().Provider, system, toolSet.defs)
		if err != nil {
			return res, err
		}

		if resuming {
			// A completed run is not resumed with nothing to add: the model would be called
			// on a finished conversation with no new input. A follow-up turn is that input,
			// so it continues past one and the record its own run writes replaces this one.
			if rs.Completed() && !opts.Checkpoint.FollowUp {
				return res, fmt.Errorf("session %q has already completed and cannot be resumed", sessionID)
			}
			// The stored session and the caller must agree on interactivity: a chat
			// session needs the input bar wired (or it would make a spurious LLM call at
			// its resting boundary and never take a follow-up), and a one-shot session
			// has no free-standing user-turn journaling and must not be handed a prompter.
			// The CLI reconciles this before calling Run (it reads the flag from the
			// session), so these are defense in depth.
			if rs.Interactive && !interactive {
				return res, fmt.Errorf("session %q is an interactive chat session; resume it with the full-screen chat UI", sessionID)
			}
			if !rs.Interactive && interactive {
				return res, fmt.Errorf("session %q was not started as a chat session and cannot be resumed as one", sessionID)
			}
			// Provider is a hard gate: a turn from another provider cannot be folded
			// coherently, so --force (which is for configuration drift) must not cross
			// it. Checked before the forceable drift so the message is unambiguous.
			if rs.Fingerprint.Provider != fp.Provider {
				return res, fmt.Errorf("cannot resume %q: it was started with provider %q but the current configuration uses %q; a run cannot change provider, and --force does not apply",
					sessionID, rs.Fingerprint.Provider, fp.Provider)
			}
			// Drift the resume must refuse, which is every part of the configuration that
			// can leave a stored conversation the provider will not accept. The two budget
			// bounds are not in it and are reported below, since neither can corrupt
			// history and a served conversation's caller may lower both per turn. Nor is
			// the tool set, which a provider reads a history against either way; it is
			// read separately here because it endangers a grant rather than a
			// conversation.
			blocking := rs.Fingerprint.BlockingDiff(fp)
			toolDrift := rs.Fingerprint.ToolsDiff(fp)
			if len(blocking) > 0 && !opts.Checkpoint.Force {
				return res, fmt.Errorf("cannot resume %q, the configuration changed since it was saved:\n  %s\nre-run against the original configuration, or pass --force to continue with the current one",
					sessionID, strings.Join(blocking, "\n  "))
			}

			j, err := store.Open(sessionID)
			if err != nil {
				return res, err
			}
			defer closeJournal(j, events)

			// Claim the run before reading the sequence, and before anything runs.
			//
			// The order matters twice. Against another worker, appending is what moves
			// the tail, so a process that still believes it holds this run is refused at
			// its own next write; doing it here means that happens before this worker has
			// caused any effect. Against this worker, seq below has to be read after the
			// claim landed, or the runner's first record collides with the claim's seq
			// and CheckAppend folds it away as a duplicate, silently losing it.
			err = claimRun(j, cfg.Identity, opts.ClaimedBy)
			if err != nil {
				return res, fmt.Errorf("cannot resume %q: %w", sessionID, err)
			}

			// The answer lands under the claim taken above and before anything runs, so
			// a worker that lost the run writes nothing, and the loop below sees the
			// call answered rather than dispatching the tool a second time. A call this
			// conversation is not waiting on refuses the resume here, where the caller
			// still gets an error, rather than part way through a turn.
			if opts.Checkpoint.Answer != nil {
				a := opts.Checkpoint.Answer

				err = runstate.AnswerDeferredCall(j, rs, a.ToolUseID, a.Content, a.IsError)
				if err != nil {
					return res, fmt.Errorf("cannot answer call %q of %q: %w", a.ToolUseID, sessionID, err)
				}
			}

			journal = j
			seq = j.LastSeq()
			startIter = rs.NextIteration
			pending = rs.Pending
			messages = rs.Messages

			// A chat session's iteration cap grows one turn's worth per accepted
			// follow-up; on resume that grown cap is not stored, only the position, so
			// give the resumed turn a fresh per-turn budget from where it left off. A turn
			// delivered on a resume needs the same, since the cap is an absolute position
			// and turn five of a conversation would otherwise start past a cap set for turn
			// one. A one-shot resume keeps the absolute cap (cumulative across the run).
			if interactive || opts.Checkpoint.FollowUp {
				maxIter = startIter + cfg.LLM.Budget.MaxIterations
			}

			// Where the restored conversation can take a user message: nothing in flight,
			// it ends on an assistant turn, and the model did not mean to continue. The
			// last two conjuncts are what keep a user turn out of a conversation the model
			// is part way through, since a journal whose tool results are all recorded but
			// unanswered has nothing pending and ends on a user message.
			atUserBoundary := rs.Pending == nil &&
				endsOnAssistant(messages) &&
				rs.LastStopReason != string(llm.StopPauseTurn)

			// A chat session resting there resumes straight to the input bar. With an
			// in-flight turn, or a paused turn the model means to continue, the loop runs
			// first to finish it.
			resumeAtInputBoundary = rs.Interactive && atUserBoundary

			// A follow-up turn enters before the first model call when the conversation
			// already rests there. Otherwise the loop finishes the turn the last run left
			// and the follow-up is the turn after it.
			followUpAtStart = atUserBoundary

			stats.LlmCalls = rs.Counters.LlmCalls
			stats.ToolCalls = rs.Counters.ToolCalls
			stats.RemoteToolCalls = rs.Counters.RemoteToolCalls
			stats.MCPToolCalls = rs.Counters.MCPToolCalls
			// Cloned rather than shared: the loop counts into this map for the rest of
			// the run, and the folded RunState is the caller's to read afterwards.
			stats.ToolCallsByKind = maps.Clone(rs.Counters.ToolCallsByKind)
			stats.InTokens = rs.Counters.InTokens
			stats.OutTokens = rs.Counters.OutTokens
			stats.CacheReadTokens = rs.Counters.CacheReadTokens
			stats.CacheCreateTokens = rs.Counters.CacheCreateTokens
			stats.ThinkingTokens = rs.Counters.ThinkingTokens

			// Snapshot what was just seeded, immediately, so the root span can report
			// this process's own consumption rather than the session's. From the next
			// statement on, stats is cumulative and there is no other way back to the
			// split. Uncached carries the raw restored InTokens because that is what the
			// delta above subtracts it from.
			resumeSeed = &telemetry.TokenUsage{
				Input:       rs.Counters.InTokens,
				Output:      rs.Counters.OutTokens,
				CacheRead:   rs.Counters.CacheReadTokens,
				CacheCreate: rs.Counters.CacheCreateTokens,
				Reasoning:   rs.Counters.ThinkingTokens,
			}
			resumeSeedCalls = rs.Counters.ToolCalls
			resumeSeedRemoteCalls = rs.Counters.RemoteToolCalls
			resumeSeedMCPCalls = rs.Counters.MCPToolCalls

			// Tell the model it resumed so it re-verifies external state before
			// acting on possibly-stale results. Appended after the fingerprint was
			// computed so it never perturbs the fingerprint comparison, and it is
			// never persisted.
			system = append(system, resumeReminder)

			// Standing approvals restore with the conversation, unless the tool set moved
			// or --force carried the resume across a changed configuration: a grant is
			// keyed on a tool name alone, so a tool set that moved under it may have
			// changed the very command the operator approved. A one-shot approval is
			// dropped on the same terms, its call naming a tool that may have moved under
			// it too. A budget difference drops neither.
			dropApprovals := len(blocking) > 0 || len(toolDrift) > 0
			if !dropApprovals {
				approvals.seed(rs.Approvals, rs.CallApprovals)
				info.StandingApprovals = rs.Approvals
			}

			// The memory revisions restore whatever the fingerprint says, and a tool set
			// that moved does not drop them: they say what the store held rather than what
			// an operator agreed to, and the store checks the revision when the write is
			// made, so one that moved fails there and the model reads the value again.
			memScope.Seed(rs.MemoryRevisions)

			info.SessionID = sessionID
			info.Resumed = true
			events.Starting(info)
			if dropApprovals && len(rs.Approvals) > 0 {
				events.Warn(Warning{Kind: WarnApprovalsDropped, Count: len(rs.Approvals)})
			}
			if len(toolDrift) > 0 {
				events.Warn(Warning{Kind: WarnToolSetDrift, Params: toolDrift})
			}
			budgetDrift := rs.Fingerprint.BudgetDiff(fp)
			if len(budgetDrift) > 0 {
				events.Warn(Warning{Kind: WarnBudgetDrift, Params: budgetDrift})
			}
			for _, w := range resumeHazards(rs) {
				events.Warn(w)
			}
			replayer, ok := events.(TranscriptReplayer)
			if ok {
				replayer.ResumeTranscript(rs)
			}
		} else {
			meta := runstate.MetaRecord{
				Version:     runstate.Version,
				RunID:       sessionID,
				Created:     time.Now(),
				Fingerprint: fp,
				Prompt:      strings.Join(prompt, " "),
				Interactive: interactive,
				// Recorded here because this is where the journal is created, which is the
				// only place either is written: a resume finds them already in the Meta
				// record it folded.
				ConversationToken: opts.Checkpoint.ConversationToken,
				Caller:            opts.Checkpoint.Caller,
			}
			j, err := store.Create(sessionID, meta)
			if err != nil {
				return res, err
			}
			defer closeJournal(j, events)

			journal = j
			seq = 1

			info.SessionID = sessionID
			events.Starting(info)
		}

		// newSession lets a context reset rotate to a fresh session mid-run: it creates a new
		// journal with the same fingerprint and a new id, seeded with the reset prompt as its
		// Meta.Prompt. It closes over the store and fingerprint the runner does not hold.
		//
		// It carries no conversation token. A channel names a journal by hashing the token,
		// so this id is not that hash and no caller reaches this journal by holding one.
		// Copying it would put two conversations in a listing claiming one token, only one
		// of which can be continued. The caller is copied, since who asked did not change.
		newSession = func(prompt string) (runstate.Journal, string, error) {
			id := a2a.NewID()
			meta := runstate.MetaRecord{
				Version:     runstate.Version,
				RunID:       id,
				Created:     time.Now(),
				Fingerprint: fp,
				Prompt:      prompt,
				Interactive: interactive,
				Caller:      opts.Checkpoint.Caller,
			}
			j, err := store.Create(id, meta)
			if err != nil {
				return nil, "", err
			}
			return j, id, nil
		}

		stats.Session = sessionID
	} else {
		events.Starting(info)
	}

	res.SessionID = sessionID

	// The session id the run STARTS on, recorded as soon as it resolves and before any
	// turn, which is also the right moment for an attribute a sampler might read. A
	// context reset can rotate it mid-run; that is reported separately rather than by
	// overwriting this, so every turn stays attributed to the session that journaled it.
	// It is empty for a run that is not checkpointed, and the spec forbids inventing
	// one, so an un-checkpointed chat correlates by trace id alone.
	runSpan.SetConversation(sessionID)

	// The memory index lists the stored memories for the model. It is appended after
	// the fingerprint was computed so that memory changing between a suspend and a
	// resume never blocks the resume: memory is data, not configuration, and the
	// resume reminder already tells the model to re-verify state. It is a start-of-run
	// snapshot; memory_list is the live view during the run. A read failure is an
	// advisory, not fatal, since the tools still reach the store.
	if cfg.MemoryIndexEnabled() {
		// Spanned because List reads every value to recover its description, which on a
		// network backend is a round trip per entry, and it happens here inside setup
		// with nothing else naming it. setupCtx, not ctx: this nests under startup.
		indexCtx, memSpan := opts.Telemetry.StartMemoryIndex(setupCtx, memoryInfo(memStore))
		entries, lerr := memStore.List(indexCtx)
		memSpan.Finish(lerr, len(entries))

		if lerr != nil {
			events.Warn(Warning{Kind: WarnMemoryIndex, Err: lerr})
		} else {
			system = append(system, builtin.MemoryIndexBlock(entries))
		}
	}

	// The turn a follow-up delivers, empty for every other run. It carries whatever the
	// caller put in Options.Prompt, including the supporting Context it appended, which
	// is the same text a first prompt would have entered the conversation with.
	var followUp string
	if opts.Checkpoint.FollowUp {
		followUp = strings.Join(prompt, " ")
	}

	r := &runner{
		cfg:             cfg,
		provider:        provider,
		stats:           stats,
		system:          system,
		thinking:        thinking,
		maxOutputTokens: maxOutputTokens,
		maxIter:         maxIter,
		maxTokens:       maxTokens,
		toolTimeout:     cfg.ToolTimeout(),
		// The source the loop reads before each model call, and the set it starts on,
		// which is what a restored in-flight batch dispatches against before the first
		// call of this run.
		toolSrc:          toolSrc,
		set:              toolSet,
		queuedWarnings:   mcpWarnings,
		toolSearchWarned: toolSearchWarned,
		confirmTags:      confirmTags,
		gate:             gate,
		approvals:        approvals,
		verbose:          opts.Verbose,
		promptCache:      cfg.PromptCacheEnabled(),
		interactive:      interactive,
		humanPaced:       opts.HumanPaced,
		events:           events,
		hooks:            opts.Hooks,
		prompter:         prompter,
		toolWorkDir:      opts.ToolWorkDir,
		messages:         messages,
		journal:          journal,
		seq:              seq,
		startIter:        startIter,
		pending:          pending,
		nextPrompt:       opts.NextPrompt,
		sessionID:        sessionID,
		newSession:       newSession,
		telemetry:        opts.Telemetry,
		providerName:     caps.SemconvProviderName(),
		sessionBackend:   cfg.SessionBackend(),
		identity:         cfg.Identity,
		memoryTools:      memoryToolNames(memBuiltins),
		memory:           memoryInfo(memStore),
		memScope:         memScope,

		resumeAtInputBoundary: resumeAtInputBoundary,
		followUp:              followUp,
		followUpAtStart:       followUpAtStart,
	}
	activeRunner = r
	// The gate was built before the journal was opened, so its approval source gets the
	// runner's own appender here, once there is a runner to append through.
	approvals.emit = r.emit
	if checkpointing {
		r.suspendRequested = opts.SuspendRequested
	}

	// The system prompt, when content capture is on. It has to be recorded here and
	// not beside the other startup setters: it is not final where it looks final, since
	// a resumed run appends its reminder and a memory-enabled run appends the memory
	// index long after the tool inventory resolves, and a prompt captured early is
	// short, plausible and missing exactly those pieces.
	startupSpan.SetSystemInstructions(genai.SystemInstructions(system))

	// Setup is over: close the startup span here rather than letting the deferred End
	// above run at function exit, which would charge the whole run loop to startup.
	startupDone = true
	startupSpan.End()

	reason, err := r.run(ctx)
	res.Reason = reason
	res.Text = r.finalText
	res.Deferred = r.deferred
	res.FollowUpTaken = r.followUpTaken
	// A context reset may have rotated to a fresh session mid-run, so report the session the
	// run ended on (the one an operator resumes) rather than the one it started with.
	res.SessionID = r.sessionID
	stats.Session = r.sessionID
	if reason == runstate.ReasonSuspended {
		stats.Suspended = true
	}

	// Only when a rotation actually moved it. Recording an end id equal to the start id
	// on every run would make the attribute meaningless as a search key, and the event
	// would claim a transition that never happened.
	if r.sessionID != sessionID {
		runSpan.SetSessionRotated(r.sessionID)
	}

	return res, err
}

// closeJournal closes a session journal, warning rather than failing the run if
// the close errors, since the run's own outcome is already decided.
// claimRun records this worker's takeover of a resumed run. Every failure is fatal to
// the resume: a claim that is skipped when the store is briefly unreachable is not a
// claim, and continuing would run the work with no idea whether anyone else is.
func claimRun(j runstate.Journal, identity string, claimedBy string) error {
	if claimedBy == "" {
		claimedBy = derivedClaimant(identity)
	}

	err := j.Append(j.LastSeq()+1, runstate.Record{
		Protocol: runstate.ClaimProtocol,
		Claim:    &runstate.ClaimRecord{By: claimedBy, Claimed: time.Now().UTC()},
	})
	if errors.Is(err, runstate.ErrLocked) {
		return fmt.Errorf("%w: another process took it while this one was starting, and is running it now; nothing here ran, so let that process finish or stop it, then resume", err)
	}
	if err != nil {
		return fmt.Errorf("claiming the run: %w", err)
	}

	return nil
}

// lastAssistantText is the concatenated text of the last assistant turn in a stored
// conversation that carried any, which is the same answer a live run reports: a turn
// that only called tools leaves the previous one standing rather than clearing it.
func lastAssistantText(messages []llm.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != llm.RoleAssistant {
			continue
		}

		var sb strings.Builder
		for _, block := range messages[i].Content {
			if block.Text == nil {
				continue
			}
			sb.WriteString(block.Text.Text)
		}

		if sb.Len() > 0 {
			return sb.String()
		}
	}

	return ""
}

// derivedClaimant names this worker when the caller supplied no name. The identity
// leads because it says which agent is running, which is the part an operator can act
// on; the host and pid narrow it to a process to go and look at.
func derivedClaimant(identity string) string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}

	return fmt.Sprintf("%s@%s pid %d", identity, host, os.Getpid())
}

func closeJournal(j runstate.Journal, events Events) {
	err := j.Close()
	if err != nil {
		events.Warn(Warning{Kind: WarnJournalClose, Err: err})
	}
}

// endsOnAssistant reports whether the conversation's last message is an assistant
// turn, which for an interactive session means it rests at an input boundary (the
// operator's turn) rather than mid-flight awaiting the next LLM call.
func endsOnAssistant(messages []llm.Message) bool {
	n := len(messages)
	return n > 0 && messages[n-1].Role == llm.RoleAssistant
}

// SessionOptions are the inputs a session read shares with a run: which journal store
// to read, expressed the same way Run takes it.
type SessionOptions struct {
	// StoreDir is Options.StoreDir, the base directory a directory-backed session store
	// resolves its relative or default path under. Empty puts the journal in the XDG
	// state directory, the CLI's behavior.
	StoreDir string

	// SessionStore is Options.SessionStore, a store the caller has already opened. When
	// set, LoadSession reads through it and consults neither StoreDir nor the configured
	// backend, so a caller sharing one store across runs pre-flights through that store
	// rather than opening a second connection to the same journals.
	SessionStore runstate.Store

	// Conns is Options.Conns, the shared connection a jetstream session store binds its
	// stream over. LoadSession borrows it and never Closes it, as Run does. When nil,
	// and only for a backend that needs a connection, LoadSession dials cfg.NatsContext
	// and releases that connection at the end of the read. For a jetstream backend the
	// connection decides which server and stream the journal is read from, so a caller
	// that injected one into Run passes the same one here.
	Conns *conns.Provider
}

// SessionOptions returns the fields a session read shares with this run, so a caller
// pre-flighting a resume with LoadSession reads the journal this run will write.
func (o Options) SessionOptions() SessionOptions {
	return SessionOptions{StoreDir: o.StoreDir, SessionStore: o.SessionStore, Conns: o.Conns}
}

// LoadSession reads one stored run without holding it, for a caller deciding what to do
// before it starts: whether the conversation was interactive, what token continues it,
// what it was asked in the first place.
//
// It is a pre-flight read and takes no lock, so what it returns describes the journal at
// the moment it was read and not a run in progress. Resuming is Run's business.
//
// opts names the journal, and a caller that is about to call Run passes
// Options.SessionOptions() from the same Options so both reach the same store. An
// injected store is read through as it stands. Without one, the store dir, the injected
// connection and the configured backend resolve the journal on the rules Run applies to
// the same three fields, and a jetstream backend with no injected connection gets one
// dialed here for the read, with the resume that follows dialing its own.
//
// ctx limits the read: a canceled context returns before anything is dialed or read, and
// a cancel while the dial is outstanding ends the wait, with the connection that dial
// produces closed rather than left open.
func LoadSession(ctx context.Context, cfg *config.Config, id string, opts SessionOptions) (*runstate.RunState, error) {
	err := ctx.Err()
	if err != nil {
		return nil, err
	}

	store := opts.SessionStore
	if store == nil {
		env := runstate.RuntimeEnv{StoreDir: opts.StoreDir}
		if runstate.NeedsNats(cfg.SessionBackend()) {
			// An injected connection is borrowed: the caller established it and shares it,
			// so the read uses it and leaves it open. Only a connection dialed here is
			// released here.
			natsConns := opts.Conns
			if natsConns == nil {
				p, derr := dialSessionNats(ctx, cfg)
				if derr != nil {
					return nil, fmt.Errorf("connecting to NATS for the jetstream session pre-flight read: %w", derr)
				}
				defer p.Close()
				natsConns = p
			}
			env.Nats = natsConns.Nats()
		}

		store, err = runstate.New(cfg.SessionBackend(), cfg.SessionRawOptions(), env)
		if err != nil {
			return nil, err
		}
	}

	return store.Load(id)
}

// dialSessionNats dials the connection a jetstream session read needs. conns.Connect
// blocks until the dial resolves and takes no context, so it runs in a goroutine while
// this waits on ctx as well. On a cancel this returns ctx.Err() and starts a second
// goroutine that waits for the dial and closes the connection it produced.
func dialSessionNats(ctx context.Context, cfg *config.Config) (*conns.Provider, error) {
	type dial struct {
		provider *conns.Provider
		err      error
	}

	done := make(chan dial, 1)
	go func() {
		p, err := conns.Connect(cfg.NatsContext, cfg.Identity)
		done <- dial{provider: p, err: err}
	}()

	select {
	case d := <-done:
		return d.provider, d.err

	case <-ctx.Done():
		go func() {
			d := <-done
			d.provider.Close()
		}()

		return nil, ctx.Err()
	}
}

// resumeHazards reports the resume situation that can misbehave: a pause at a
// server-tool boundary whose state may have expired.
func resumeHazards(rs *runstate.RunState) []Warning {
	var out []Warning

	if rs.Pending == nil && rs.LastStopReason == string(llm.StopPauseTurn) {
		out = append(out, Warning{Kind: WarnResumePausedTurn})
	}

	return out
}

// thinkingMode is the configuration's three-state answer in the neutral model's
// vocabulary. A configuration naming no thinking block asks for nothing, which is not
// the same as asking for thinking to be off: the first sends no parameter and leaves
// the model to its own behavior, and the second tells a model that would otherwise
// reason to stop.
func thinkingMode(cfg *config.Config) llm.ThinkingMode {
	switch {
	case cfg.ThinkingEnabled():
		return llm.ThinkingOn
	case cfg.ThinkingDisabled():
		return llm.ThinkingOff
	default:
		return llm.ThinkingUnset
	}
}

// computeFingerprint captures the configuration a checkpointed run depends on, so
// a resume against a changed model, prompt, tool set or budget is caught. The
// system prompt is hashed, never stored. providerID is the resolved provider's own
// id (Capabilities().Provider), not the config selector, so the fingerprint records
// the backend the journal was actually written against.
func computeFingerprint(cfg *config.Config, providerID string, system []string, toolDefs []llm.ToolDef) (runstate.Fingerprint, error) {
	sys, err := json.Marshal(system)
	if err != nil {
		return runstate.Fingerprint{}, fmt.Errorf("hashing system prompt: %w", err)
	}
	tools, err := json.Marshal(toolDefs)
	if err != nil {
		return runstate.Fingerprint{}, fmt.Errorf("hashing tool set: %w", err)
	}

	// Only whether the run will think is recorded, so the two modes that produce no
	// thinking share one value. What a resume can be incoherent about is the stored
	// conversation, and neither saying nothing nor asking for thinking off adds a
	// thinking block to it; telling them apart here would refuse a resume over a
	// configuration edit that cannot change what the journal holds. It also keeps "off"
	// meaning what it meant before the third state existed, so sessions journaled until
	// now still resume.
	mode := "off"
	if thinkingMode(cfg) == llm.ThinkingOn {
		mode = "summarized"
	}

	return runstate.Fingerprint{
		Provider:     providerID,
		Model:        cfg.LLM.Model,
		SystemHash:   runstate.HashHex(sys),
		ToolsHash:    runstate.HashHex(tools),
		ThinkingMode: mode,
		// Recorded verbatim rather than folded the way the thinking mode is: an effort
		// change alters how the run reasons and what it costs, and nothing about it is
		// equivalent to another level.
		ReasoningEffort: cfg.ReasoningEffort(),
		MaxTokens:       cfg.LLM.Budget.MaxTokens,
		MaxIterations:   cfg.LLM.Budget.MaxIterations,
	}, nil
}
