//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// GateApprovals holds the standing approvals a ConfirmGate honors: an approval the
// operator granted for a whole conversation rather than for one call.
//
// It holds grants only. The gate has no standing denial, so a refusal is recorded
// nowhere and is asked again next time. That is what keeps a run which ended before
// the operator could answer from persisting a decision they never made, and an
// implementation must not add a way to record one. It does not forbid a later
// revocation, which removes a grant rather than recording a refusal.
//
// A source belongs to one run and is never shared between concurrent runs, so it
// needs no locking of its own.
type GateApprovals interface {
	// Granted reports whether tool carries a standing approval.
	Granted(tool string) bool

	// Grant records a standing approval for tool. An error ends the run, matching
	// every other non-terminal journal write: continuing would mean the operator's
	// answer was taken and then quietly lost.
	Grant(tool string) error

	// TakeCall reports whether toolUseID carries an approval for this dispatch, and
	// takes it. An operator who answered "allow once" for a call the run had already
	// suspended on approved that call and no other, so the approval is spent when the
	// gate reads it and a second call of the same tool is asked about again.
	TakeCall(toolUseID string) bool
}

// memoryApprovals keeps grants for the life of the gate and writes them nowhere,
// which is what a run with no journal behind it has always done.
type memoryApprovals struct {
	allow map[string]bool
}

func (m *memoryApprovals) Granted(tool string) bool { return m.allow[tool] }

func (m *memoryApprovals) Grant(tool string) error {
	m.allow[tool] = true

	return nil
}

// TakeCall reports false: a one-shot approval reaches a run through its journal, and
// this source has none.
func (m *memoryApprovals) TakeCall(string) bool { return false }

// ConfirmGate enforces confirmation tags in the agent loop: a tool carrying
// ai:confirm or any operator-configured confirm tag must be approved before it
// runs. An "allow for the conversation" answer is remembered by tool name, so a
// tool the operator has blessed is not asked about again, regardless of its
// arguments; the approval covers that one command, not every tool that happens to
// share its triggering tag. How long it is remembered is the approval source's
// business: in memory it lasts the run, journal-backed it lasts the conversation.
//
// ConfirmGate is not safe for concurrent use. It is created once per run and used
// only from the single-goroutine agent loop; it must never be wired into the
// concurrent MCP path, which has no local operator and where confirmation is
// instead requested from the calling client through elicitation.
type ConfirmGate struct {
	// approvals holds the standing grants this gate honors and records new ones to.
	approvals GateApprovals
	// prompter renders the approval request and reports the operator's choice. All
	// prompt and trace rendering lives behind it, so nothing writes to the raw
	// terminal while a full-screen Prompter owns the screen. The gate keeps the
	// default-deny policy; the prompter only reports a choice or an error.
	prompter toolkit.Prompter
}

// NewConfirmGate returns a ConfirmGate putting its approval prompts to prompter.
// approvals holds the standing grants; nil keeps them in memory for the life of the
// gate, so a run that does not journal behaves as it always has.
func NewConfirmGate(prompter toolkit.Prompter, approvals GateApprovals) *ConfirmGate {
	if approvals == nil {
		approvals = &memoryApprovals{allow: map[string]bool{}}
	}

	return &ConfirmGate{approvals: approvals, prompter: prompter}
}

// Approve decides whether a confirm-tagged command may run, putting the approval
// request to the prompter when needed. toolUseID names the call being decided, which
// is what a one-shot approval is keyed on, toolName is the key a standing approval is
// recorded and consulted under, commandPath is the human-readable command
// (e.g. "stream rm") used in messages, display is the sanitized command line
// shown in the approval prompt, and tag is the command tag that gated it (e.g.
// "ai:confirm" or "impact:rw"), named in the prompt so the operator sees why
// approval is being asked. The command's trace line is emitted by the caller for
// every tool that runs, so the gate itself renders nothing on approval.
//
// It returns true with an empty reason when the command may run. It returns false
// with a reason when it may not: the operator declined, or no interactive
// terminal was attached to ask one. A false result is authoritative; the reason
// is surfaced to the model via ConfirmDeniedResult.
//
// A non-nil error is never a denial, and the caller must not send a result to the
// model on this path. It means one of three things. The operator was asked and did not
// answer, so the run ends: on a checkpointed run a denial is journaled and replayed on
// every later resume, so recording one from an interrupt puts an answer in the record
// the operator never gave. The grant could not be recorded, so the run ends rather than
// take an answer and lose it. Or the prompter reports toolkit.ErrDeferredResult, which
// says the question was put and the answer arrives later: the caller journals the call
// as deferred and suspends the run, and the operator answers it on a later resume.
func (g *ConfirmGate) Approve(ctx context.Context, toolUseID, toolName, commandPath, display, tag string) (bool, string, error) {
	// Default-deny lives here at the caller, not in the prompter: with no operator
	// reachable there is no one to ask. It is checked before any prompt is shown so the
	// prompter is never reached in a state where its answer could not be trusted.
	// Whether an operator is reachable is the prompter's own report, so a non-terminal
	// channel that can still ask a human is honored rather than declined for lacking a
	// TTY.
	if !g.prompter.CanPrompt() {
		return false, toolkit.NoTerminalReason, nil
	}

	// A run whose context is already over is not asked, and is not answered either.
	// This used to manufacture a refusal, which is the durable false record the error
	// return exists to stop.
	if err := ctx.Err(); err != nil {
		return false, "", fmt.Errorf("%w: %w", toolkit.ErrPromptAborted, err)
	}

	// The standing grant is consulted below both checks, not above them. A grant can
	// outlive the process that recorded it, so honoring one first would run a gated
	// command on a resume with no operator present, or on a run whose context has
	// already ended, in both cases without anyone to stop it.
	if g.approvals.Granted(toolName) {
		return true, "", nil
	}

	// A one-shot approval is consulted here for the same reason and on the same terms,
	// and it is spent whether or not the command then succeeds: the operator approved
	// this dispatch, so a retry is a new question.
	if g.approvals.TakeCall(toolUseID) {
		return true, "", nil
	}

	choice, err := g.prompter.ApproveCommand(ctx, toolkit.GateRequest{ToolUseID: toolUseID, Command: commandPath, Display: display, Tag: tag})
	switch {
	case errors.Is(err, toolkit.ErrPromptAborted):
		return false, "", err
	case errors.Is(err, toolkit.ErrDeferredResult):
		// A prompter that reaches its operator through something slower than a terminal
		// reports a deferral: the question is put and the answer arrives later. The caller
		// journals the call as deferred and suspends, so folding this into a denial would
		// refuse a command the operator is still deciding on.
		return false, "", err
	case err != nil:
		// A prompt that could not be put is not an operator walking away, so it stays a
		// denial: the command is gated and nothing established that it may run.
		return false, fmt.Sprintf("the operator did not permit this command: %v; this decision is final, do not retry", err), nil
	}

	switch choice {
	case toolkit.ConfirmAlways:
		err = g.approvals.Grant(toolName)
		if err != nil {
			return false, "", err
		}

		return true, "", nil
	case toolkit.ConfirmOnce:
		return true, "", nil
	default:
		return false, "the operator declined to permit this command; this decision is final, do not retry", nil
	}
}

// confirmDeniedOutcome is the JSON result returned to the model when a
// confirm-tagged command was not permitted to run. It mirrors the human-in-the-
// loop outcomes: a normal (non-error) result the model should reason about, not a
// tool failure to route around.
type confirmDeniedOutcome struct {
	// Allowed is always false here; the command did not run.
	Allowed bool `json:"allowed"`
	// Reason explains why, so the model knows whether the operator declined or no
	// operator could be reached.
	Reason string `json:"reason,omitempty"`
}

// ConfirmDeniedResult builds the tool_result for a confirm-tagged command the gate
// did not permit. It is a non-error result so the model treats the refusal as
// authoritative rather than as a failure to work around.
func ConfirmDeniedResult(useID, reason string) llm.ToolResultBlock {
	data, err := json.Marshal(confirmDeniedOutcome{Reason: reason})
	if err != nil {
		return llm.ToolResultBlock{ToolUseID: useID, Content: `{"allowed":false}`}
	}

	return llm.ToolResultBlock{ToolUseID: useID, Content: string(data)}
}
