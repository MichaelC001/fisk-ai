//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agent

import (
	"encoding/json"

	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/toolkit"

	"github.com/choria-io/fisk-ai/internal/mcpclient"
	"github.com/choria-io/fisk-ai/internal/remotetools"
	"github.com/choria-io/fisk-ai/internal/runstate"
)

// Events receives a run's narration, tool traces and advisories as it happens, so
// the caller owns all wording and rendering: the package decides what happened, the
// caller decides how it looks.
//
// Contract:
//   - Methods are called from the single run goroutine, so a per-run instance sees
//     exactly one run and needs no locking of its own. A caller that shares one Events
//     across concurrent runs (an aggregating server sink) must make its implementation
//     safe for concurrent use; that shared-aggregator contract is defined with the job
//     system, not here.
//   - Methods may be called during teardown, including Panicked from Run's deferred
//     recover after the run body has already returned or unwound, so an implementation
//     must stay callable until Run returns rather than tearing itself down early.
//   - It is a structured sink: methods carry typed data, not preformatted prose, so a
//     consumer is free to render, log, or stream it. The two terminal renderers happen
//     to flatten to prose; a structured (for example slog) consumer keeps the types.
type Events interface {
	// Warn reports an operator-facing advisory as structured data.
	Warn(Warning)
	// Starting reports the resolved run parameters once, before the loop begins.
	Starting(RunInfo)
	// LLMRequest reports one request's summary; emitted only when verbose.
	LLMRequest(summary string)
	// ToolCall reports a tool invocation as it is dispatched.
	ToolCall(ToolTrace)
	// ToolResult reports a tool's returned output once it has run, so a caller that
	// shows tool output can render it. It is emitted for every executed tool
	// (built-in, remote and local, including an approved confirm-gated one), but not
	// for a call that never ran (an unknown tool or a denied confirmation).
	ToolResult(ToolResultTrace)
	// Message reports an assistant turn: intermediate narration or, when terminal,
	// the final answer.
	Message(resp llm.Response, terminal bool)
	// SessionRotated reports that a context reset started a fresh checkpoint session,
	// leaving the previous one saved and resumable under prevID, so the caller can show
	// the operator how to return to it.
	SessionRotated(prevID string)

	// Panicked reports that the run crashed: Run recovered a panic on its goroutine and
	// is returning a PanicError. value is the recovered panic value and stack is the
	// captured goroutine stack. It is terminal, not an advisory (every Warning is a
	// continue-anyway note), so it is its own method and each surface renders it its own
	// way. The stack reaches this sink and nowhere else: it leaks absolute paths, module
	// layout and frame arguments, so an implementation on a path that forwards to a
	// remote peer must keep the stack local (a server log) and send the peer only the
	// generic PanicError message. It is called from Run's deferred recover during
	// unwind, so an implementation must not itself panic or block.
	Panicked(value any, stack []byte)
}

// RemoteHostReporter is the optional half of Events that hears how importing remote
// tools went, host by host. A sink that renders advisories for an operator implements
// it; one narrating to a peer, a log or a queue has nothing to do with it.
//
// It is separate from Events because the argument names this package's own run-path
// helper, and a sink should not have to import that to compile.
type RemoteHostReporter interface {
	// RemoteHostNotes reports the per-host outcome of importing remote tools, for
	// advisory rendering.
	RemoteHostNotes([]remotetools.HostImport)
}

// MCPServerReporter is the optional half of Events that hears how importing the tools
// of the configured MCP servers went, server by server. A sink that renders advisories
// for an operator implements it; one narrating to a peer, a log or a queue has nothing
// to do with it.
//
// It is separate from Events for the reason RemoteHostReporter is: the argument names
// this package's own run-path helper, and a sink should not have to import that to
// compile. It is separate from RemoteHostReporter because the two carry different
// findings, and a sink that renders one is free to ignore the other.
type MCPServerReporter interface {
	// MCPServerNotes reports the per-server outcome of importing MCP tools, for
	// advisory rendering: how long each server took to answer, which of its tools were
	// skipped and why, and which server contributed nothing.
	MCPServerNotes([]mcpclient.ServerImport)
}

// TranscriptReplayer is the optional half of Events that replays a resumed run's
// prior conversation before it continues. A surface a person is watching implements it,
// so they see what was already said; nothing else needs to.
//
// It is separate from Events because a sink that renders nothing has nothing to do
// with a resumed conversation, and because what it carries is the whole stored run
// rather than one event.
//
// A sink that implements it receives that whole folded run, which carries the
// conversation's token where a channel recorded one. Implement it on a surface that
// shows the run to the person who owns it, and not on one that forwards elsewhere.
type TranscriptReplayer interface {
	// ResumeTranscript asks the caller to replay the prior conversation of a resumed
	// run before it continues.
	//
	// How a tool call reads is the caller's, from the call's name and arguments,
	// which is what makes a replayed turn render as the live one did rather than as a
	// second rendering of the same thing. It used to be handed the tool registry to
	// resolve a command line with, which no sink outside this program could hold.
	ResumeTranscript(rs *runstate.RunState)
}

// WarningKind selects which advisory a Warning carries and which of its fields
// are set.
type WarningKind int

const (
	// WarnHITLNoTerminal: human_in_the_loop is enabled and this run has nobody to ask,
	// so its tools will decline rather than prompt.
	//
	// The condition is that the run was given no prompter, not that no terminal exists.
	// A terminal run has one when it has an interactive terminal; a run serving a
	// channel has one when the channel supplies it, which the prompts channel does only
	// when elicitation is configured. The name predates the second case.
	WarnHITLNoTerminal WarningKind = iota
	// WarnConfirmNoTerminal: Count confirmation-gated tools cannot be approved, because
	// this run has nobody to ask. The condition is the one WarnHITLNoTerminal describes.
	WarnConfirmNoTerminal
	// WarnConfirmTagUnmatched: the confirm_tags entry Name matches no loaded tool.
	WarnConfirmTagUnmatched
	// WarnUnknownTool: the model called a tool Name that is not registered.
	WarnUnknownTool
	// WarnMissingRequired: the model called application tool Name without the
	// required parameters listed in Params, so the call was rejected before it ran.
	WarnMissingRequired
	// WarnJournalTerminal: recording the terminal record failed with Err.
	WarnJournalTerminal
	// WarnJournalUser: recording an interactive follow-up (user turn) failed with Err;
	// the session ends here so the journal stays resumable at the last coherent boundary.
	WarnJournalUser
	// WarnJournalMemoryRevisions: recording the memory revisions the run read failed
	// with Err. The run is over and nothing is lost but the head start: the next turn
	// of this conversation reads a memory again before overwriting it.
	WarnJournalMemoryRevisions
	// WarnResumePausedTurn: resuming at a paused-turn boundary whose server-side
	// tool state may have expired.
	WarnResumePausedTurn
	// WarnMaxIterInteractive: an interactive turn hit the per-turn iteration cap; the
	// session is not ended (the operator can steer with a follow-up), so it is an
	// advisory rather than the fatal max-iterations outcome a one-shot run returns.
	WarnMaxIterInteractive
	// WarnTurnErrorInteractive: an interactive turn failed (Err carries the cause, e.g.
	// an LLM call timeout). The session is not ended; the operator is handed back to the
	// input bar to retry or steer, so a transient failure does not stall the chat.
	WarnTurnErrorInteractive
	// WarnMemoryIndex: reading the memory store to build the start-of-run index failed
	// with Err. The run continues without the index; the model can still reach the
	// store through the memory tools.
	WarnMemoryIndex
	// WarnSessionRotate: a context reset could not start a fresh checkpoint session (Err
	// carries the cause, e.g. the store failed to create the new journal). The reset is
	// not applied; the turn runs on in the current session, which stays resumable.
	WarnSessionRotate
	// WarnToolSearchUnsupported: Count tools crossed the tool-search threshold but the
	// active provider does not support server-side tool search, so every tool is sent
	// to the model directly and uses more context on each request.
	WarnToolSearchUnsupported
	// WarnKnowledgeIndexAbsent: knowledge is enabled and a store base (StoreDir) is in
	// effect, but no index exists at the resolved path Name. Most often the knowledge
	// CLI wrote the index elsewhere because it ran with a different (or no) store base;
	// without this the run would start clean and knowledge_search would silently return
	// nothing.
	WarnKnowledgeIndexAbsent
	// WarnTraceClose: closing the trace file at run end failed with Err. It is routed
	// through the events sink rather than written to the shared process stderr so it
	// stays attributable to its run when many run at once.
	WarnTraceClose
	// WarnJournalClose: closing the session journal at run end failed with Err. Routed
	// through the events sink for the same reason as WarnTraceClose.
	WarnJournalClose
	// WarnTraceWrite: writing a line to the trace file failed with Err, so the trace is
	// incomplete. Reported once per run; the run continues, the trace is best-effort.
	WarnTraceWrite
	// WarnPromptDenied: a UserPromptSubmit hook denied an interactive follow-up prompt.
	// Name carries the operator-facing deny reason. The prompt was not run and the input
	// bar reopens; the session continues.
	WarnPromptDenied
	// WarnRunEndHook: the RunEnd hook failed with Err. It runs during teardown,
	// once the run's outcome is already decided, so its failure cannot change that
	// outcome and is reported as an advisory rather than ending the run.
	WarnRunEndHook
	// WarnUnknownReservedTag: tool Name carries the tags in Params, which claim the
	// reserved ai: namespace but are not tags the harness knows. They do nothing, which
	// looks exactly like a correctly tagged command, so a misspelled ai:read_only or
	// ai:confirm would otherwise be invisible.
	WarnUnknownReservedTag
	// WarnBehaviorTagConflict: tool Name declares the contradictory behavior tags in
	// Params, e.g. both ai:read_only and ai:destructive. The more dangerous reading was
	// taken and the tool is still available.
	WarnBehaviorTagConflict
	// WarnToolTimeout: tool Name did not finish within the configured tool timeout and
	// was stopped, with Err naming the window. It is an advisory as well as a tool
	// result because the result reaches the model and, on a host with no operator
	// attached, nothing else: a bound the operator never set firing silently is the
	// case this exists for.
	WarnToolTimeout
	// WarnToolDeferred: tool Name will answer later, with Err carrying what it is
	// waiting on. The call produced no result, the run ends at a resumable boundary,
	// and the tool is never dispatched for it again. It is an advisory because a run
	// ending with work outstanding looks like a run that stopped early, and only this
	// says which tool is holding it.
	WarnToolDeferred
	// WarnApprovalsDropped: Count standing confirm-gate approvals were not restored,
	// because the tool set moved or --force resumed the session across a changed
	// configuration. An approval is keyed on a tool name, and the tool it names may not
	// be the tool the operator approved, so the run asks again. Told at the resume
	// rather than at the next prompt, where it would look like the gate forgetting.
	WarnApprovalsDropped
	// WarnToolSetDrift: the session was saved with a different tool set, and the resume
	// continues under the current one. A stored conversation reads the same either way,
	// a provider accepting a history that names a tool it was not sent, so the change is
	// reported rather than refused. What it costs is the standing approvals, which are
	// dropped with it.
	WarnToolSetDrift
	// WarnBudgetDrift: the session was saved with different token or iteration bounds,
	// listed in Params, and the resume continues under the current ones. Neither bound
	// can leave a stored conversation incoherent, so a difference is reported rather
	// than refused; what it changes is how far this run may get.
	WarnBudgetDrift
	// WarnRemoteTagFilterIgnored: the include filter for the remote agent in Name
	// selects by tag, which discovery does not carry, so the filter was ignored and the
	// host's tools were taken unfiltered.
	WarnRemoteTagFilterIgnored
	// WarnRemoteToolsSkipped: the remote agent in Name advertised tools this run did
	// not take, listed in Params with the reason each was skipped.
	WarnRemoteToolsSkipped
	// WarnRemoteNoTools: the remote agent in Name contributed nothing after filtering.
	// It is the one that catches a mistyped include: the host answered, the run holds
	// none of its tools, and nothing else would say so.
	WarnRemoteNoTools
	// WarnPIIRedacted: harness.pii found Count values of the types in Params in the text
	// from Name (the prompt, or a tool) and replaced them before the model, the journal
	// or a collector saw them. Without it redaction is silent, and an operator wonders
	// why the model does not know an address they gave it.
	//
	// Raised once per run, on the first redaction, carrying that one's detail. Every
	// occurrence still reaches the log and the span: a warning per event would print
	// twice for each (the renderer both shows it inline and repeats it at the end), so a
	// chat redacting an author address on forty tool calls would bury its own answer.
	WarnPIIRedacted
	// WarnPIIWithheld: the text from Name was withheld rather than rewritten, with Count
	// and Params as above. It is harness.pii.mode reject refusing what it found, or,
	// with Err set, a scan that could not be completed: redaction that cannot be
	// performed is not redaction, so the text is withheld rather than passed through.
	// Raised once per run, like WarnPIIRedacted.
	WarnPIIWithheld
	// WarnMCPToolsChanged: the MCP server in Name told this run that its tool list
	// changed, and Params says what moved: the tools the model gains, the ones it
	// loses, and the ones the server now offers that this run cannot take. With Err
	// set the server said its list changed and could not be re-listed, so the run
	// keeps the tools it had.
	//
	// The model is offered the new set from its next call, so without this an operator
	// watching a run sees it start calling a tool that was not there when it started
	// and has nothing that says why.
	WarnMCPToolsChanged
)

// HostImportWarnings is what importing one peer's tools has to say about it.
//
// It is here rather than in either renderer because both need it and neither owns it: a
// worker sends these to a caller that cannot see its configuration, and a command that
// inspects an import prints them directly. Only the kind and its fields are decided,
// the wording belonging to whatever renders the warning.
func HostImportWarnings(imp remotetools.HostImport) []Warning {
	var out []Warning

	if imp.IgnoredIncludeTags {
		out = append(out, Warning{Kind: WarnRemoteTagFilterIgnored, Name: imp.Host.Name})
	}
	if len(imp.Skipped) > 0 {
		out = append(out, Warning{Kind: WarnRemoteToolsSkipped, Name: imp.Host.Name, Params: imp.Skipped})
	}
	// The host answered and this run holds none of its tools. Nothing else reports it,
	// and a mistyped include filter is exactly what it looks like.
	if len(imp.Tools) == 0 {
		out = append(out, Warning{Kind: WarnRemoteNoTools, Name: imp.Host.Name})
	}

	return out
}

// warningKindNames is the stable name of every kind.
//
// The enumeration's values are positions in a list, so inserting a kind renumbers the
// ones after it. Anything that leaves this process names a kind with one of these
// instead: a wire, a log line, a stored record. A kind added later is additive, since
// a reader that does not know a name still has the warning's other fields.
var warningKindNames = map[WarningKind]string{
	WarnHITLNoTerminal:         "hitl_no_terminal",
	WarnConfirmNoTerminal:      "confirm_no_terminal",
	WarnConfirmTagUnmatched:    "confirm_tag_unmatched",
	WarnUnknownTool:            "unknown_tool",
	WarnMissingRequired:        "missing_required",
	WarnJournalTerminal:        "journal_terminal",
	WarnJournalUser:            "journal_user",
	WarnJournalMemoryRevisions: "journal_memory_revisions",
	WarnResumePausedTurn:       "resume_paused_turn",
	WarnMaxIterInteractive:     "max_iterations_interactive",
	WarnTurnErrorInteractive:   "turn_error_interactive",
	WarnMemoryIndex:            "memory_index",
	WarnSessionRotate:          "session_rotate",
	WarnToolSearchUnsupported:  "tool_search_unsupported",
	WarnKnowledgeIndexAbsent:   "knowledge_index_absent",
	WarnTraceClose:             "trace_close",
	WarnJournalClose:           "journal_close",
	WarnTraceWrite:             "trace_write",
	WarnPromptDenied:           "prompt_denied",
	WarnRunEndHook:             "run_end_hook",
	WarnUnknownReservedTag:     "unknown_reserved_tag",
	WarnBehaviorTagConflict:    "behavior_tag_conflict",
	WarnToolTimeout:            "tool_timeout",
	WarnToolDeferred:           "tool_deferred",
	WarnApprovalsDropped:       "approvals_dropped",
	WarnRemoteTagFilterIgnored: "remote_tag_filter_ignored",
	WarnRemoteToolsSkipped:     "remote_tools_skipped",
	WarnRemoteNoTools:          "remote_no_tools",
	WarnToolSetDrift:           "tool_set_drift",
	WarnBudgetDrift:            "budget_drift",
	WarnPIIRedacted:            "pii_redacted",
	WarnPIIWithheld:            "pii_withheld",
	WarnMCPToolsChanged:        "mcp_tools_changed",
}

var warningKindsByName = func() map[string]WarningKind {
	out := make(map[string]WarningKind, len(warningKindNames))
	for kind, name := range warningKindNames {
		out[name] = kind
	}

	return out
}()

// String is the kind's stable name, or "unknown" for a value that names no kind.
func (k WarningKind) String() string {
	name, ok := warningKindNames[k]
	if !ok {
		return "unknown"
	}

	return name
}

// ParseWarningKind returns the kind a name belongs to. It reports false for a name
// this build does not know, which is what a peer on a newer version sends, and the
// caller renders such a warning from its other fields rather than dropping it.
func ParseWarningKind(name string) (WarningKind, bool) {
	kind, ok := warningKindsByName[name]

	return kind, ok
}

// Warning is a typed operator advisory. Kind selects which fields are meaningful;
// the caller formats the message text.
type Warning struct {
	Kind  WarningKind
	Count int
	Name  string
	Err   error
	// Params carries the parameter names for a kind that reports on a set of them,
	// such as the missing required parameters of a rejected tool call.
	Params []string
}

// RunInfo reports the resolved parameters of a run for the caller to display.
type RunInfo struct {
	Tools           int
	ThinkingEnabled bool
	ConfirmTools    int
	ConfirmTags     []string
	// TraceFile is set when the run is tracing to a file.
	TraceFile string
	// SessionID is set when the run is checkpointed.
	SessionID string
	// Resumed is true when continuing an existing session rather than starting one.
	Resumed bool
	// NoApplication is true when the run wraps no application (application_path is
	// unset), so it runs on built-in and remote tools alone.
	NoApplication bool
	// StandingApprovals names the tools a resumed session carries a confirm-gate
	// approval for, which the operator granted in an earlier sitting and will not be
	// asked about again. Empty on a fresh run and on a resume that restored none.
	StandingApprovals []string
}

// ToolTrace describes one tool invocation for display. Display is the full,
// un-elided command line; DisplayShort is the same line with long argument values
// middle-elided. A width-aware surface (the TUI viewport) shows Display when it fits
// a row and falls back to DisplayShort otherwise, while a plain stream that cannot
// measure a screen uses DisplayShort. Both are empty for non-command tools.
type ToolTrace struct {
	// ID is the model's tool_use id for this call, which ToolResultTrace.CallID
	// echoes. A surface rendering calls and results as separate items needs it to
	// pair them, since a turn may carry several calls and a call may produce no
	// result at all.
	ID           string
	Name         string
	Display      string
	DisplayShort string
	// Input is the raw JSON arguments the model supplied, exactly as they were
	// dispatched. It is untrusted and unsanitized, like Output on the result, and is
	// what a surface shows when Display is empty because the tool runs no command.
	Input json.RawMessage
	// Present is the visibility axis: how a renderer shows and suppresses the call.
	// A renderer keys its suppression off this, never off ProviderKind, so a built-in
	// self-renders (the human-in-the-loop tools) or is traced (memory and knowledge)
	// by the same rules regardless of which provider supplied it.
	Present toolkit.Presentation
	// ProviderKind is the accounting axis: the provider the tool is accounted under
	// (the kind= log token). It is distinct from Present above; a built-in traces one
	// ProviderKind whether it self-renders or is traced.
	ProviderKind toolkit.Kind
	Agent        string
}

// ToolResultTrace describes one tool's returned output for display. Present mirrors
// the ToolTrace it answers, so a caller can apply the same visibility rules to the
// result as to the call (for example suppressing a self-rendering built-in's result
// unless verbose). Output is the raw result text, untrusted and unsanitized; IsError
// reports whether the tool reported a failure.
type ToolResultTrace struct {
	// CallID is the tool_use id of the ToolTrace this answers. Not every call
	// produces a result: a denied confirmation, a call missing required arguments,
	// a tool that answers later and an aborted run all end without one, so a
	// surface pairing the two must tolerate a call that is never answered.
	CallID  string
	Present toolkit.Presentation
	// ProviderKind mirrors the ToolTrace it answers, so the result's kind= log token
	// matches the call's.
	ProviderKind toolkit.Kind
	Output       string
	IsError      bool
}
