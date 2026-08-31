//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"

	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// Hooks are the AGENT-SIDE hooks: they run wherever the agent loop runs, which is inside
// fisk serve, inside a queue worker, or inside the worker a terminal embeds for a local
// run. They never run in a caller that reached the agent over a2a, which may be on
// another machine.
//
// The other family is a2a.ClientHooks, the CLIENT-SIDE hooks, which run in the process
// asking an agent for work and observe the protocol rather than the loop. Which side a
// thing belongs on follows from what it can see: model calls, tools and a prompt entering
// a conversation are here, while a person typing, a question waiting to be put to them
// and a sitting ending are the client's and are invisible from here.
//
// Optional callbacks the run invokes at fixed points. Every hook runs on the single run
// goroutine, in loop order; it must honor ctx and return promptly; it is trusted
// in-process code (like CustomTools) and a panic in it aborts the run as a *PanicError.
// RunEnd alone is exempt: it fires during teardown, once the outcome is already decided,
// so an error or a panic from it is downgraded to a warning.
//
// One callback per point: compose several behaviors by wrapping them in one func of your
// own. A nil field does not fire, with one exception the caller does not install and
// cannot remove: under harness.pii the run wraps UserPromptSubmit and PostToolUse with a
// scan of its own, which runs after the caller's hook and reads whatever that hook left
// behind. A nil field still fires that scan.
//
// A hook may only observe, terminate (a returned error aborts the run; PreToolUse may
// also deny one call), or adjust the data passing through the point: PreToolUse rewrites
// the tool and args, PostToolUse replaces output, UserPromptSubmit replaces the text of
// the prompt entering the conversation. A hook never introduces a prompt of its own,
// continues or extends a turn, or changes token or tool accounting, budgets, or iteration
// caps.
type Hooks struct {
	// RunStart fires once as a run begins, fresh or resumed, before the first model
	// call. A context reset does not fire it again; that rotation is reported through
	// Events.SessionRotated.
	RunStart RunStartHook

	// UserPromptSubmit fires as a prompt enters the conversation: the initial prompt of a
	// fresh run, then each follow-up, whether an operator typed it at the input bar or a
	// resume was handed one. A resume does not re-fire it for the history it
	// reconstructs.
	UserPromptSubmit UserPromptSubmitHook

	// PreModelCall fires before each model call. It sits above the provider, so it fires
	// for an injected provider too, unlike an llm.Middleware.
	PreModelCall PreModelCallHook

	// PostModelCall fires after each model reply, including one truncated at the output
	// cap. Its abort is not durable across resume, since the turn is already journaled
	// when it fires; use PreToolUse to reliably block a tool.
	PostModelCall PostModelCallHook

	// PreToolUse fires before each tool call, ahead of validation, the confirm gate, the
	// trace, and execution, so a deny or a rewrite flows through the whole pipeline: what
	// runs is what the operator confirms, what is traced, and what is journaled.
	PreToolUse PreToolUseHook

	// PostToolUse fires after each tool call, before the result is traced and journaled.
	PostToolUse PostToolUseHook

	// TurnEnd fires at each interactive continuation boundary, after a completed or
	// iteration-capped turn and before the next prompt. A one-shot run has no boundary
	// but its end, which is RunEnd.
	TurnEnd TurnEndHook

	// RunEnd fires once as a run ends, for any reason including a crash. It runs
	// during teardown, after the journal and stores are closed, so it must not depend on
	// them; it gets a stats snapshot instead.
	RunEnd RunEndHook
}

// RunStartHook observes a run starting (fresh or resumed). A non-nil error aborts
// the run.
type RunStartHook func(context.Context, RunStartInfo) error

// PreModelCallHook observes the request about to be sent to the model. A non-nil error
// aborts the run.
type PreModelCallHook func(context.Context, PreModelCallInfo) error

// PostModelCallHook observes each model reply. A non-nil error aborts the turn, but that
// abort is not durable across resume; use PreToolUse to reliably block a tool.
type PostModelCallHook func(context.Context, PostModelCallInfo) error

// TurnEndHook observes each interactive continuation boundary. A non-nil error aborts
// the run.
type TurnEndHook func(context.Context, TurnEndInfo) error

// RunEndHook observes a run ending, including a crash. Its error cannot abort an
// already-decided outcome, so it is downgraded to a warning.
type RunEndHook func(context.Context, RunEndInfo) error

// UserPromptSubmitHook observes a prompt entering the conversation and may deny or
// rewrite it via the returned Result. A non-nil error aborts the whole run; to reject a
// single prompt set Result.Deny instead. Return the zero Result to change nothing.
type UserPromptSubmitHook func(context.Context, UserPromptSubmitInfo) (UserPromptSubmitResult, error)

// PreToolUseHook observes a tool call before it runs and may deny it or rewrite the tool
// and its arguments via the returned Result. A non-nil error aborts the run; to reject a
// single call set Result.Deny instead. Return the zero Result to change nothing.
type PreToolUseHook func(context.Context, PreToolUseInfo) (PreToolUseResult, error)

// PostToolUseHook observes a tool's result before it is journaled and may replace the
// output the model sees via the returned Result. A non-nil error aborts the run. Return
// the zero Result to keep the tool's own output.
type PostToolUseHook func(context.Context, PostToolUseInfo) (PostToolUseResult, error)

// RunStartInfo is the read-only snapshot handed to RunStart.
type RunStartInfo struct {
	// SessionID is the checkpoint session id; empty for a non-checkpointed run.
	SessionID string
	// Resumed is true when continuing a stored session (the hook re-fires on resume).
	Resumed bool
	// Interactive is true for a chat run with an input bar.
	Interactive bool
	// Model is the LLM model the run will use.
	Model string
	// ToolNames lists every tool the model can call as the run starts, by name. A tool
	// set that moves later does not re-fire this hook: it reports what the run began
	// with, which is when it fires.
	ToolNames []string
}

// UserPromptSubmitInfo is the read-only snapshot handed to UserPromptSubmit.
type UserPromptSubmitInfo struct {
	// Text is the prompt entering the conversation.
	Text string
	// Initial is true for the run's first prompt, false for an interactive follow-up.
	Initial bool
}

// PreModelCallInfo is the read-only snapshot handed to PreModelCall. It carries counts
// rather than the live conversation, so a hook cannot alter what is sent.
type PreModelCallInfo struct {
	// Iteration is this turn's loop index.
	Iteration int
	// Model is the LLM model the request targets.
	Model string
	// MessageCount is the number of conversation messages, not the live slice.
	MessageCount int
	// ToolCount is the number of tools offered on the request.
	ToolCount int
}

// PostModelCallInfo is the read-only snapshot handed to PostModelCall.
type PostModelCallInfo struct {
	// Iteration is this turn's loop index.
	Iteration int
	// Response is a deep copy of the model reply; mutating it cannot affect the run.
	Response llm.Response
	// Terminal is true when this reply is the final answer (no tools, not paused).
	Terminal bool
	// ToolCalls are the pre-split tool_use blocks of this reply.
	ToolCalls []llm.ToolUseBlock
}

// PreToolUseInfo is the read-only snapshot handed to PreToolUse.
type PreToolUseInfo struct {
	// ToolName is the tool the model asked to run.
	ToolName string
	// ToolUseID is the id the tool result must answer.
	ToolUseID string
	// Input is the model's raw JSON arguments (a copy).
	Input json.RawMessage
	// Kind is the provider that supplied the tool: application, builtin, remote, or custom.
	Kind toolkit.Kind
	// ConfirmGated is true when the call would be shown to the operator's confirm gate.
	ConfirmGated bool
}

// PostToolUseInfo is the read-only snapshot handed to PostToolUse.
type PostToolUseInfo struct {
	// ToolName is the tool that ran.
	ToolName string
	// ToolUseID is the id the tool result answers.
	ToolUseID string
	// Input is the arguments the tool actually ran with (a copy).
	Input json.RawMessage
	// Kind is the provider that supplied the tool.
	Kind toolkit.Kind
	// Output is what the tool returned.
	Output string
	// IsError reports whether the tool reported a failure.
	IsError bool
}

// TurnEndInfo is the read-only snapshot handed to TurnEnd.
type TurnEndInfo struct {
	// Reason is why the turn ended (completed, max-iterations).
	Reason runstate.TerminalReason
	// Iteration is the loop index the turn ended on.
	Iteration int
}

// RunEndInfo is the read-only snapshot handed to RunEnd.
type RunEndInfo struct {
	// SessionID is the checkpoint session id; empty for a non-checkpointed run.
	SessionID string
	// Reason is why the run ended; empty on a crash, so key off Crashed.
	Reason runstate.TerminalReason
	// Crashed is true when the run ended by panic.
	Crashed bool
	// Err is the error the run ended with, if any.
	Err error
	// Stats is a snapshot of the run's counters, by value.
	Stats RunStats
}

// UserPromptSubmitResult carries a UserPromptSubmit decision. The zero value changes
// nothing.
type UserPromptSubmitResult struct {
	// Deny rejects the prompt: an initial prompt stops the run before it does any work,
	// an interactive follow-up is rejected and the input bar reopens, and a follow-up a
	// caller supplied ends the run, there being no input bar to return to.
	Deny bool
	// DenyReason is required when Deny; it is shown to the operator.
	DenyReason string

	// Rewrite replaces the text of the prompt when not empty. What is appended to the
	// conversation, journaled and sent to the model is this rather than what was
	// submitted, so a rewrite of the initial prompt is also what a resume reconstructs.
	// Ignored when Deny.
	//
	// It replaces the whole text: to change part of it, start from Info.Text.
	Rewrite string
}

// PreToolUseResult carries a PreToolUse decision. The zero value changes nothing.
type PreToolUseResult struct {
	// Deny blocks the call: the tool does not run and the run returns an error result to
	// the model in its place, marked IsError and carrying DenyReason, so the model can
	// adapt and try another approach. It is never a silent skip.
	Deny       bool
	DenyReason string

	// RewriteTool redirects the call to a different registered tool by name. Empty keeps
	// the original tool.
	RewriteTool string

	// RewriteInput replaces the call's arguments. nil keeps the model's arguments. A
	// non-nil value REPLACES the whole argument object: RewriteInput of []byte("{}") clears
	// every argument. To change one field, start from a copy of Info.Input. Ignored when Deny.
	RewriteInput json.RawMessage
}

// PostToolUseResult carries a PostToolUse decision. The zero value keeps the tool's own
// result.
type PostToolUseResult struct {
	// Replace overrides the result the model sees (and that is journaled) with Output and
	// IsError below. It is an explicit bool, not an empty-value sentinel, because an empty
	// Output is a valid replacement. When false the tool's own result is kept.
	Replace bool
	Output  string
	IsError bool
}

// firePreModelCall invokes the PreModelCall hook, a no-op when no hook is set.
func (h Hooks) firePreModelCall(ctx context.Context, info PreModelCallInfo) error {
	if h.PreModelCall == nil {
		return nil
	}
	return h.PreModelCall(ctx, info)
}

// firePostModelCall invokes the PostModelCall hook. Unlike the other invokers it takes
// the reply and iteration rather than a prebuilt Info, and builds the Info only when a
// hook is set: the hook is handed a deep copy of the reply (so a mutation cannot reach the
// live conversation, which shares resp.Content), and copying every reply for a nil hook
// would be wasted work.
func (h Hooks) firePostModelCall(ctx context.Context, iteration int, resp llm.Response, terminal bool) error {
	if h.PostModelCall == nil {
		return nil
	}
	dup, err := cloneResponse(resp)
	if err != nil {
		return fmt.Errorf("copying model reply: %w", err)
	}
	return h.PostModelCall(ctx, PostModelCallInfo{
		Iteration: iteration,
		Response:  dup,
		Terminal:  terminal,
		ToolCalls: toolUseBlocks(dup.Content),
	})
}

// cloneResponse returns a deep copy of a model reply by round-tripping it through JSON.
// These blocks are exactly the journal's serialized form, so the round-trip is faithful
// and adapts if a block gains a field; it isolates the copy's byte slices (a thinking
// signature, a tool_use input, a provider block's raw JSON) from the originals the live
// conversation still references. A marshal or unmarshal error is not expected for a
// provider-produced reply (the same content is journaled just before this), so it is
// surfaced rather than silently returning a shallow copy that would break the isolation
// the hook is promised.
func cloneResponse(resp llm.Response) (llm.Response, error) {
	data, err := json.Marshal(resp)
	if err != nil {
		return llm.Response{}, err
	}

	var out llm.Response
	err = json.Unmarshal(data, &out)
	if err != nil {
		return llm.Response{}, err
	}

	return out, nil
}

// toolUseBlocks extracts the tool_use blocks from a reply's content, in order. It reads
// from a copy so the returned blocks share nothing with the live conversation.
func toolUseBlocks(content []llm.ContentBlock) []llm.ToolUseBlock {
	var out []llm.ToolUseBlock
	for _, block := range content {
		if block.ToolUse == nil {
			continue
		}
		out = append(out, *block.ToolUse)
	}
	return out
}

// firePreToolUse invokes the PreToolUse hook, returning the zero Result (change nothing)
// when no hook is set.
func (h Hooks) firePreToolUse(ctx context.Context, info PreToolUseInfo) (PreToolUseResult, error) {
	if h.PreToolUse == nil {
		return PreToolUseResult{}, nil
	}
	return h.PreToolUse(ctx, info)
}

// firePostToolUse invokes the PostToolUse hook, returning the zero Result (keep the
// tool's own output) when no hook is set.
func (h Hooks) firePostToolUse(ctx context.Context, info PostToolUseInfo) (PostToolUseResult, error) {
	if h.PostToolUse == nil {
		return PostToolUseResult{}, nil
	}
	return h.PostToolUse(ctx, info)
}

// fireRunStart invokes the RunStart hook, a no-op when no hook is set.
func (h Hooks) fireRunStart(ctx context.Context, info RunStartInfo) error {
	if h.RunStart == nil {
		return nil
	}
	return h.RunStart(ctx, info)
}

// fireUserPromptSubmit invokes the UserPromptSubmit hook, returning the zero Result
// (change nothing) when no hook is set.
func (h Hooks) fireUserPromptSubmit(ctx context.Context, info UserPromptSubmitInfo) (UserPromptSubmitResult, error) {
	if h.UserPromptSubmit == nil {
		return UserPromptSubmitResult{}, nil
	}
	return h.UserPromptSubmit(ctx, info)
}

// fireTurnEnd invokes the TurnEnd hook, a no-op when no hook is set.
func (h Hooks) fireTurnEnd(ctx context.Context, info TurnEndInfo) error {
	if h.TurnEnd == nil {
		return nil
	}
	return h.TurnEnd(ctx, info)
}

// fireRunEnd invokes the RunEnd hook, a no-op when no hook is set. Alone among
// the invokers it returns nothing: it runs from the panic barrier during teardown, once
// the run's outcome is already decided, so neither an error nor a panic from the hook can
// change that outcome and both are reported as a WarnRunEndHook advisory instead. It
// shields itself rather than leaning on the barrier, whose own recover has already run by
// the time this is called and so cannot catch a second panic.
func (h Hooks) fireRunEnd(ctx context.Context, events Events, info RunEndInfo) {
	if h.RunEnd == nil {
		return
	}

	// Stats is a snapshot by value, but its by-kind counts are a map the caller still
	// reads after Run returns (the run summary), so the hook gets its own copy of them.
	info.Stats.ToolCallsByKind = maps.Clone(info.Stats.ToolCallsByKind)

	defer func() {
		p := recover()
		if p == nil {
			return
		}
		warnRunEnd(events, fmt.Errorf("RunEnd hook panicked: %v", p))
	}()

	err := h.RunEnd(ctx, info)
	if err != nil {
		warnRunEnd(events, fmt.Errorf("RunEnd hook: %w", err))
	}
}

// warnRunEnd delivers the RunEnd advisory, shielding the caller-supplied sink so
// that a panic in it does not escape the panic barrier this runs inside either.
func warnRunEnd(events Events, err error) {
	defer func() { recover() }()
	events.Warn(Warning{Kind: WarnRunEndHook, Err: err})
}
