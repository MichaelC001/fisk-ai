//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package toolkit

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/choria-io/fisk-ai/internal/llm"
)

// ExecDeps carries the per-run dependencies a tool may need to run a model
// tool_use. A kind that needs none ignores it; only the human-in-the-loop
// built-ins use the Prompter and only local command tools use the WorkDir.
type ExecDeps struct {
	// Prompter is the operator interaction path a built-in tool uses to reach a
	// person. It is nil for tools that never prompt.
	Prompter Prompter

	// WorkDir is the directory a local command tool runs in, so concurrent runs
	// sharing one process do not collide on relative-path writes. Empty inherits the
	// process working directory. It sets the child's directory only and confines
	// nothing.
	WorkDir string
}

// Outcome is the surface-neutral result of one tool call. The agent loop, the MCP
// server and the a2a server each adapt it to their own shape, so a tool kind
// describes what it produced once rather than once per surface.
type Outcome struct {
	// Output is the tool's own output: an in-process tool's JSON result, or a
	// command's stdout and stderr combined in the order they were written.
	Output string

	// Exec is the command metadata when the tool ran an external command, and nil
	// when it did not. Its presence is what tells a surface how to frame Output: a
	// command's is rendered as a CommandResult envelope, an in-process tool's is
	// passed through verbatim because it is already the JSON the caller asked for.
	Exec *CommandExec
}

// CommandExec is the execution metadata of a command tool's call, carried
// alongside its output so a surface can reconstruct a CommandResult without the
// tool having marshaled one.
type CommandExec struct {
	// Command is the command and arguments that were run, without the binary path.
	Command string
	// ExitCode is the command's exit status; 0 on success.
	ExitCode int
	// Truncated is true when the output was capped.
	Truncated bool
}

// CommandResult renders the outcome as the JSON shape a command tool's result
// takes on every surface. It is only meaningful for an outcome carrying Exec; an
// in-process tool's Output is passed through verbatim instead.
func (o *Outcome) CommandResult() CommandResult {
	res := CommandResult{Output: o.Output}
	if o.Exec != nil {
		res.Command = o.Exec.Command
		res.ExitCode = o.Exec.ExitCode
		res.Truncated = o.Exec.Truncated
	}

	return res
}

// Tool is the contract every tool kind satisfies, however it runs: a local fisk
// command, an in-process built-in, or a tool invoked on a remote agent. The runner
// and every serving surface dispatch uniformly over this interface; kind-specific
// policy (confirmation, missing-argument checks, per-kind tracing) is exposed
// through narrow capability interfaces the caller consults, never folded in here.
//
// Exposure is the exception, and deliberately so. It is not kind-specific policy
// but a property every tool must have an answer for, and a tool reaching a serving
// surface without one is the failure the methods exist to prevent. Declaring it
// here makes the compiler ask, so a new tool kind cannot be served by omission.
type Tool interface {
	// Name is the model-facing tool name; it is unique within a run and is the key
	// the runner dispatches on.
	Name() string
	// Description is the model-facing description.
	Description() string
	// ModelDescription is the description advertised to a model or a peer agent,
	// which may carry more than Description does: a command tool appends its tags.
	// Description is the plain help; this is what is served.
	ModelDescription() string
	// InputSchema is the JSON schema advertised to the model.
	InputSchema() map[string]any
	// Definition renders the tool as a provider-neutral definition. deferLoading asks
	// for the tool to be hidden behind tool search; a kind may decline it (a
	// built-in is never deferred, a tool tagged ai:no_defer opts out).
	Definition(deferLoading bool) llm.ToolDef
	// Execute runs the tool for raw input and returns its surface-neutral outcome.
	// A returned error is a harness failure (the tool could not run); an outcome the
	// caller should reason about, including a non-zero exit, is a normal result.
	Execute(ctx context.Context, input json.RawMessage, deps ExecDeps) (*Outcome, error)
	// MCPExposable reports whether the tool may ever be served over MCP. It is the
	// capability ceiling only: an operator's allowlist narrows it further and can
	// never widen past it.
	MCPExposable() bool
	// A2AExposable reports whether the tool may ever be served over a2a, on the same
	// terms as MCPExposable.
	A2AExposable() bool
}

// Tools widens a slice of one concrete tool kind to the interface, which Go does
// not do implicitly. Callers assembling a served set from several kinds use it to
// concatenate them, and the concatenation order is meaningful: the first tool to
// claim a name keeps it.
func Tools[T Tool](in []T) []Tool {
	out := make([]Tool, len(in))
	for i, t := range in {
		out[i] = t
	}

	return out
}

// ExecuteUse runs a tool for a model tool_use block and returns the matching tool
// result. It is the single adaptation from a tool's neutral outcome to the LLM
// shape, so every kind is presented to the model identically: a harness failure is
// an error result, and an outcome the model should reason about (including a
// non-zero exit) is a normal result.
func ExecuteUse(t Tool, ctx context.Context, use llm.ToolUseBlock, deps ExecDeps) llm.ToolResultBlock {
	out, err := t.Execute(ctx, use.Input, deps)
	if err != nil {
		return llm.ToolResultBlock{ToolUseID: use.ID, Content: err.Error(), IsError: true}
	}

	// An in-process tool's output is already the JSON the model asked for; only a
	// command's is wrapped, so the model sees the exit code and truncation flag.
	if out.Exec == nil {
		return llm.ToolResultBlock{ToolUseID: use.ID, Content: out.Output}
	}

	res := out.CommandResult()
	data, err := json.Marshal(res)
	if err != nil {
		return llm.ToolResultBlock{ToolUseID: use.ID, Content: fmt.Sprintf("marshaling tool result: %v", err), IsError: true}
	}

	return llm.ToolResultBlock{ToolUseID: use.ID, Content: string(data)}
}

// Confirmable is implemented by the tool kinds that can require operator
// confirmation before running. Only local command tools do; the runner consults it
// to decide whether to gate a call and to render the approval prompt, then drives
// the confirm gate itself so the gate's per-run state stays out of the tool.
// Keeping the whole contract here lets the runner drive the gate without knowing
// the concrete tool type.
type Confirmable interface {
	// NeedsConfirm reports whether a call must be approved, given the operator's
	// extra confirm tags on top of the always-on ai:confirm.
	NeedsConfirm(extraTags []string) bool
	// ConfirmTrigger names the tag that gated the call, for the prompt.
	ConfirmTrigger(extraTags []string) string
	// Command is the bare command the call runs, shown in the approval prompt.
	Command() string
	// TraceLine is the full command line for these arguments, shown in the prompt
	// so the operator approves exactly what will run.
	TraceLine(input json.RawMessage) string
}

// ArgumentValidator is implemented by the tool kinds that can pre-validate a
// model's input against required parameters before running. Only local command
// tools do; the runner rejects a structurally invalid call before the confirm gate
// and before execution, so the operator is never asked to approve an incomplete
// call and nothing runs that would fail only on its own exit.
type ArgumentValidator interface {
	// MissingRequired returns the required parameters absent from input, or nil
	// when the call is complete.
	MissingRequired(input json.RawMessage) []string
	// MissingRequiredMessage is the result returned to the model naming the missing
	// parameters so it can correct and retry.
	MissingRequiredMessage(missing []string) string
}
