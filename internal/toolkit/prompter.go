//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package toolkit

import (
	"context"
	"errors"
)

// ConfirmChoice is the operator's three-way answer to a confirm-gate approval:
// decline, allow once, or allow for the rest of the conversation. It is
// deliberately distinct from the boolean answer of ask_human_confirm; the standing
// choice carries a policy consequence a yes/no answer does not, and on a
// checkpointed run it outlives the process that recorded it.
type ConfirmChoice int

const (
	// ConfirmNo declines the command; it does not run.
	ConfirmNo ConfirmChoice = iota
	// ConfirmOnce runs the command this time only.
	ConfirmOnce
	// ConfirmAlways runs the command and stops asking for that tool for the rest of
	// the conversation, which on a checkpointed run covers every later resume of it.
	ConfirmAlways
)

// GateRequest describes a confirm-gated command put to the operator for approval.
// Its argument values come from the model, so Display is already sanitized by the
// caller; a Prompter must still escape it for whatever widget markup its own
// rendering layer uses.
type GateRequest struct {
	// Command is the human-readable command path, e.g. "stream rm", used in the
	// prompt wording and as the subject of a standing allow.
	Command string
	// Display is the sanitized full command line shown to the operator so they see
	// exactly what they are approving.
	Display string
	// Tag is the tag that gated the command, e.g. "ai:confirm" or "impact:rw", named
	// in the prompt so the operator sees why approval is being asked.
	Tag string
}

// Prompter puts a run's interactive decisions to the local operator: the confirm
// gate's three-way approval and the three human-in-the-loop questions. It is
// injected once per run and is the only path permitted to read the terminal or
// draw a prompt, so a future full-screen UI can supply its own implementation
// without agent.Run learning about it.
//
// A Prompter never owns the default-deny outcome. The caller decides what an
// error, a missing terminal, or a canceled context mean, and every method here
// reports an error the caller is free to treat as a denial. ErrPromptAborted is the
// exception it must not: that error says the operator never answered, and recording
// a decision from it would journal an answer nobody gave. Every implementation
// reports its own interrupt, closed input and cancellation as that error.
//
// A Prompter is created per run and used only from the single agent goroutine; like
// ConfirmGate it must never be wired into the concurrent MCP path, which reaches its
// client through elicitation instead.
type Prompter interface {
	// CanPrompt reports whether an operator is reachable through this prompter, so
	// every site that decides whether a run may ask a human (the confirm-gate
	// default-deny, the human-in-the-loop tools, and their start-of-run advisories)
	// consults one source of truth rather than each testing the terminal itself. It
	// decouples "an operator can be asked" from "stdin is a terminal": the line
	// prompter reports it from the terminal, a non-interactive prompter reports false,
	// and a prompter backed by a non-terminal channel (a chat integration, a web
	// callback) reports true, so such a channel can drive human-in-the-loop on a run
	// with no TTY. It governs only the start-of-run decision; a live send failure is
	// still reported per call and treated as a denial.
	CanPrompt() bool

	// ApproveCommand renders the approval request for a confirm-gated command (the
	// header naming the command and its gating tag, and the sanitized command line)
	// and returns the operator's three-way choice. An interrupt, EOF, closed input,
	// or canceled ctx is returned as an error wrapping ErrPromptAborted, which the
	// caller must not record as a decline; any other error the caller treats as a
	// denial.
	ApproveCommand(ctx context.Context, req GateRequest) (ConfirmChoice, error)

	// Confirm puts a yes/no question to the operator and returns their answer. An
	// interrupt, EOF, or canceled ctx returns false with an error wrapping
	// ErrPromptAborted.
	Confirm(ctx context.Context, question string) (bool, error)

	// Select asks the operator to choose one of options and returns its index, or a
	// negative index with an error when no choice was made. An interrupt, EOF, or
	// canceled ctx returns an error wrapping ErrPromptAborted.
	Select(ctx context.Context, question string, options []string) (int, error)

	// Input asks the operator for a free-text value, pre-filled with def, and returns
	// what they entered (which may be empty). An interrupt, EOF, or canceled ctx
	// returns an error wrapping ErrPromptAborted.
	Input(ctx context.Context, question, def string) (string, error)
}

// errNoOperator is the denial every DefaultDenyPrompter method returns: there is
// no operator on this path to answer.
var errNoOperator = errors.New("no operator is available to answer on this path")

// ErrPromptAborted reports that a question reached an operator who never answered
// it: they interrupted, closed the input, or the context ended while the prompt was
// up. It is not a decline, and a caller that records decisions must not record one.
//
// The distinction has to be made by the Prompter, because only it can tell an
// operator choosing "no" from an operator walking away, and the difference is
// durable: on a checkpointed run a decline is journaled and replayed on every later
// resume, so treating an interrupt as one puts an answer in the record that nobody
// gave.
//
// Wrap it with %w when the underlying cause is worth carrying.
var ErrPromptAborted = errors.New("the operator did not answer the prompt")

// denyPrompter is a Prompter with no operator behind it: every method fails closed,
// returning both a denial value and an error. It backs the MCP builtin dispatch,
// where there is no terminal and the concurrent path must never reach a real
// prompter (see the Prompter doc). It is defense in depth only: the real gate is
// the expose.agent.mcp.builtins allowlist, which serves only knowledge_search, a
// tool that never prompts. Should a prompting built-in ever be wired here by
// mistake, this makes it deny rather than hang or panic.
type denyPrompter struct{}

// DefaultDenyPrompter returns a Prompter whose every method fails closed. Use it
// wherever an in-process tool must be invoked with no operator reachable.
func DefaultDenyPrompter() Prompter { return denyPrompter{} }

// CanPrompt reports false: this prompter exists precisely because no operator is
// reachable, so every human-facing decision must fail closed.
func (denyPrompter) CanPrompt() bool { return false }

func (denyPrompter) ApproveCommand(context.Context, GateRequest) (ConfirmChoice, error) {
	return ConfirmNo, errNoOperator
}

func (denyPrompter) Confirm(context.Context, string) (bool, error) {
	return false, errNoOperator
}

func (denyPrompter) Select(context.Context, string, []string) (int, error) {
	return -1, errNoOperator
}

func (denyPrompter) Input(context.Context, string, string) (string, error) {
	return "", errNoOperator
}
