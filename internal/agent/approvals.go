//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agent

import (
	"github.com/choria-io/fisk-ai/internal/runstate"
)

// journalApprovals is the util.GateApprovals a run gives its confirm gate. A grant
// the operator makes is honored from the moment they give it and written to the
// journal once the call that triggered it is answered, so a crash between the
// approval and the tool result loses the grant and the resume asks again for a
// command it is about to re-run.
//
// It backs a non-checkpointed run as readily as a journaled one: emit is the
// runner's, which writes nothing when the run has no journal, so the grant lasts
// the process and goes nowhere, which is what an un-checkpointed run has always
// done.
//
// It belongs to one run and is used only from the run goroutine.
type journalApprovals struct {
	granted map[string]bool
	// staged holds grants the operator has given whose call has not been answered
	// yet, in the order they were given.
	staged []string
	// calls holds the one-shot approvals this run was resumed with, by tool_use id.
	// They are journaled by whatever supplied the operator's answer while the run was
	// suspended, so nothing writes one here: the run reads them and spends them.
	calls map[string]bool
	// emit appends a record to the run's journal. The runner sets it on itself at
	// construction, since the gate is built before the journal is opened.
	emit func(runstate.Record) error
}

func newJournalApprovals() *journalApprovals {
	return &journalApprovals{granted: map[string]bool{}, calls: map[string]bool{}}
}

// Granted reports whether the operator has approved tool for this conversation.
func (a *journalApprovals) Granted(tool string) bool { return a.granted[tool] }

// Grant takes the operator's approval for tool. It is honored from here on, and
// flush is what makes it durable.
func (a *journalApprovals) Grant(tool string) error {
	if a.granted[tool] {
		return nil
	}

	a.granted[tool] = true
	a.staged = append(a.staged, tool)

	return nil
}

// flush journals the grants taken since the last call, and is called once the tool
// call that triggered them has been answered or has deferred. Nothing is staged for
// the common case, so calling it after every tool call costs a length check.
func (a *journalApprovals) flush() error {
	for _, tool := range a.staged {
		err := a.emit(runstate.Record{
			Protocol: runstate.DecisionProtocol,
			Optional: true,
			Decision: &runstate.DecisionRecord{Tool: tool},
		})
		if err != nil {
			return err
		}
	}

	a.staged = nil

	return nil
}

// TakeCall reports whether the operator approved this call while the run was
// suspended, and spends the approval. It authorizes the dispatch about to happen and
// nothing after it, so a second call of the same tool is asked about again.
func (a *journalApprovals) TakeCall(toolUseID string) bool {
	if !a.calls[toolUseID] {
		return false
	}

	delete(a.calls, toolUseID)

	return true
}

// seed restores the grants a resumed conversation carries. It is not a journal
// write: these records are already in the journal that supplied them.
func (a *journalApprovals) seed(tools []string, calls []runstate.CallApprovalRecord) {
	for _, tool := range tools {
		a.granted[tool] = true
	}

	for _, call := range calls {
		a.calls[call.ToolUseID] = true
	}
}

// clear drops every grant, for a context reset. The cleared conversation is a new
// one, by rotation to a fresh journal or by emptying an unjournaled context, and a
// grant is scoped to the conversation it was given in. Staged grants go with them:
// they belong to the conversation being left, as do the one-shot approvals, whose
// calls are in it.
func (a *journalApprovals) clear() {
	a.granted = map[string]bool{}
	a.staged = nil
	a.calls = map[string]bool{}
}
