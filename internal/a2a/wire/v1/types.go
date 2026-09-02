//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package wire

// StopReason is the neutral reason a task finished, carried by Result and
// ErrorMessage.
//
// The constants below are what this build names. A received value may be anything
// the schema allows, since refusing an unrecognized reason would cost the whole
// terminal message including the answer text, so a receiver asks Valid rather than
// assuming the value is one of them.
type StopReason string

const (
	StopEndTurn         StopReason = "end_turn"
	StopMaxTokens       StopReason = "max_tokens"
	StopRefusal         StopReason = "refusal"
	StopCanceled        StopReason = "canceled"
	StopError           StopReason = "error"
	StopBudgetExhausted StopReason = "budget_exhausted"
	// StopSuspended is a task that parked at a resumable boundary (an async
	// human-in-the-loop wait, a multi-hop delegation) rather than finishing. It mirrors
	// runstate.ReasonSuspended.
	StopSuspended StopReason = "suspended"
	// StopMaxIterations is a task that hit its iteration cap without a final answer. It
	// mirrors runstate.ReasonMaxIterations, which the runner already produces.
	StopMaxIterations StopReason = "max_iterations"
)

// Valid reports whether r is one of the eight reasons this build names. A sender
// emits only those; a receiver accepts any reason the schema allows, so false
// describes a reason from a newer peer rather than a malformed message.
//
// It is exported so a sender can check a reason it was handed before building a
// message with it, which is the same job ValidIdentityName does for an identity.
func (r StopReason) Valid() bool {
	switch r {
	case StopEndTurn, StopMaxTokens, StopRefusal, StopCanceled,
		StopError, StopBudgetExhausted, StopSuspended, StopMaxIterations:
		return true
	default:
		return false
	}
}

// Usage reports what a task consumed: tokens for what it cost, calls for what it
// did.
type Usage struct {
	// InputTokens is every input token the task consumed, cached and uncached
	// together. It is deliberately the total rather than the uncached remainder the
	// agent counts separately, because a caller reading a bill wants the number it
	// was billed for and would have no way to know it had been handed a part of one.
	InputTokens int64 `json:"input_tokens,omitempty"`
	// OutputTokens is every token the model produced.
	OutputTokens int64 `json:"output_tokens,omitempty"`

	// CacheReadTokens and CacheCreateTokens break InputTokens down, and are included
	// in it rather than additional to it. They are reported separately because they
	// are priced differently: a read is a fraction of an uncached token and a write is
	// a premium on one, so a caller costing a task cannot do it from a total alone.
	CacheReadTokens   int64 `json:"cache_read_tokens,omitempty"`
	CacheCreateTokens int64 `json:"cache_create_tokens,omitempty"`

	// ThinkingTokens is the part of OutputTokens the model spent reasoning, and is
	// included in it rather than additional to it. Zero is an answer rather than an
	// absence: reasoning is not rendered, so a caller that omitted the figure when it
	// was zero would leave a person unable to tell a model that did not reason from one
	// that reasoned where nothing showed it.
	ThinkingTokens int64 `json:"thinking_tokens,omitempty"`

	// LLMCalls and ToolCalls describe the shape of the run rather than its cost. For
	// an agent whose tools are commands, the tool count is the closest thing to a
	// measure of what was actually done, and the pair together is what distinguishes
	// an agent working from an agent stuck: five calls answering four tools is
	// progress, twenty-seven of each is a loop.
	LLMCalls  int64 `json:"llm_calls,omitempty"`
	ToolCalls int64 `json:"tool_calls,omitempty"`
}

// Budget bounds what an agent may use answering a request. The receiver's local
// configuration is the ceiling; a request may only lower a limit.
//
// MaxTokens and MaxIterations have the scopes the receiver's own configuration gives
// them, so what a request lowers is those bounds rather than bounds of its own.
// MaxIterations is per turn. MaxTokens is cumulative over the conversation, which means
// a request continuing one can be refused at once for tokens earlier turns processed,
// and lowering it below what a conversation has already used ends that conversation.
type Budget struct {
	// MaxTokens is the cumulative token limit the request asks for. The receiver
	// takes it only when its own configured limit is unset or larger: zero leaves
	// that configured limit alone, and so does a value above it.
	MaxTokens int64 `json:"max_tokens,omitempty"`
	// MaxIterations is the per-turn iteration limit the request asks for, and the
	// receiver clamps it the same way as MaxTokens.
	MaxIterations int64 `json:"max_iterations,omitempty"`
	// CallTimeout is how long the receiver may spend on one model call, as a Go
	// duration string such as "60s". The serving side here drops it: the work item a
	// channel builds carries MaxTokens and MaxIterations and has nowhere to put a
	// duration, so a run uses the receiver's own configured call timeout.
	CallTimeout string `json:"call_timeout,omitempty"`
}

// ExecResult is optional command metadata attached to a tool result when the
// tool was a shell command. It is absent for non-shell tools.
//
// The agent importing a remote tool acts on whether the block is there: it rebuilds a
// reply carrying one into the CommandResult envelope a local command tool would have
// produced, and hands on an in-process tool's output unchanged, because that output is
// already the JSON the caller asked for. The serving agent sets an exit code on the
// call's span for every command that ran, so a command exiting zero is not taken for a
// built-in that ran none.
type ExecResult struct {
	// Command is the command and its arguments, without the binary path.
	Command string `json:"command,omitempty"`
	// ExitCode is the command's exit status. It carries omitempty, so a zero exit
	// leaves the field out of the JSON; that a command ran at all is what the
	// presence of this block says.
	ExitCode int `json:"exit_code,omitempty"`
	// Truncated is true when the agent that ran the command capped its output, so
	// the output in the reply is not everything the command wrote.
	Truncated bool `json:"truncated,omitempty"`
}

// ToolResult is the outcome of a tool invocation. It is shared by the streamed
// ToolResultBlock and the direct ToolReply so a tool result has one shape
// regardless of how it is delivered. IsError reports a harness failure (the tool
// did not run cleanly), distinct from a non-zero command exit reported in Exec.
type ToolResult struct {
	IsError bool        `json:"is_error,omitempty"`
	Output  string      `json:"output,omitempty"`
	Exec    *ExecResult `json:"exec,omitempty"`
}
