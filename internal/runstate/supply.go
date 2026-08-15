//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package runstate

import (
	"errors"
	"fmt"

	"github.com/choria-io/fisk-ai/internal/llm"
)

// MaxSuppliedResultBytes caps the answer SupplyToolResult will accept. A supplied
// result is untrusted text that becomes the next thing the model reads, and it is
// stored in a journal a backend may cap independently, so it is bounded here where
// the refusal can name what happened rather than at a publish that reports a size.
const MaxSuppliedResultBytes = 256 * 1024

var (
	// ErrNotDeferred is returned when the tool_use id names a call the run never
	// deferred. Answering one would put a result in the journal for a call that is
	// either still to run or already answered by the tool itself.
	ErrNotDeferred = errors.New("tool call was not deferred")
	// ErrAlreadyAnswered is returned when the deferred call already has a result.
	// Answering twice would leave the turn carrying two results for one tool_use.
	ErrAlreadyAnswered = errors.New("tool call is already answered")
	// ErrResultTooLarge is returned when the supplied answer exceeds
	// MaxSuppliedResultBytes.
	ErrResultTooLarge = errors.New("supplied result is too large")
)

// SupplyToolResult answers a deferred tool call, so the run can be resumed and
// finish the turn the call belongs to. content is what the tool would have returned
// had it answered at once, and isError marks it the way a tool's own failure would
// be marked.
//
// It refuses anything but an outstanding deferral: ErrNotDeferred for a call this
// run never deferred, ErrAlreadyAnswered for one that has a result. Both are about
// which call may be answered rather than about who may answer, since anybody who can
// reach the store can already write to the journal.
//
// It writes one ordinary ToolResult record and nothing else, which is what makes the
// next resume an ordinary resume: the fold marks the call answered and the loop
// reuses the result rather than dispatching the tool again.
//
// Concurrency is the backends' own. Open locks the run where the backend has a lock
// (the file journal's flock), and appends are fenced on the tail where it does not
// (the jetstream backend's expected-sequence publish), so a run another worker has
// taken over refuses this rather than racing it. Losing that race is the right
// outcome here: the answer can be supplied again once the other worker is done,
// while overwriting its work cannot be undone.
func SupplyToolResult(store Store, sessionID, toolUseID, content string, isError bool) error {
	if store == nil {
		return fmt.Errorf("a session store is required")
	}
	if sessionID == "" {
		return fmt.Errorf("a session id is required")
	}
	if toolUseID == "" {
		return fmt.Errorf("a tool_use id is required")
	}
	if len(content) > MaxSuppliedResultBytes {
		return fmt.Errorf("%w: %d bytes, limit %d", ErrResultTooLarge, len(content), MaxSuppliedResultBytes)
	}

	// Loaded before the journal is opened so a refusal costs no lock, and read again
	// under the open handle below is unnecessary: the append is fenced on the tail the
	// handle itself read, so a journal that moved between these two points is refused
	// by the append rather than accepted on a stale view.
	state, err := store.Load(sessionID)
	if err != nil {
		return err
	}

	err = checkDeferred(state, toolUseID)
	if err != nil {
		return err
	}

	journal, err := store.Open(sessionID)
	if err != nil {
		return err
	}
	defer journal.Close()

	return journal.Append(journal.LastSeq()+1, Record{
		Protocol: ToolResultProtocol,
		ToolResult: &ToolResultRecord{
			ToolUseID: toolUseID,
			Result: llm.ToolResultBlock{
				ToolUseID: toolUseID,
				Content:   content,
				IsError:   isError,
			},
		},
	})
}

// checkDeferred reports whether toolUseID names a deferred call of state that is
// still waiting for its answer.
//
// The committed conversation is consulted first, because answering a deferral
// completes its turn and commits it: the call is then no longer pending and reporting
// it as one that was never deferred would tell somebody answering twice that their
// first answer never landed.
func checkDeferred(state *RunState, toolUseID string) error {
	if answeredInConversation(state, toolUseID) {
		return fmt.Errorf("%w: %q", ErrAlreadyAnswered, toolUseID)
	}
	if state.Pending == nil {
		return fmt.Errorf("%w: the run has no tool calls in flight", ErrNotDeferred)
	}
	if _, ok := state.Pending.Deferred[toolUseID]; !ok {
		return fmt.Errorf("%w: %q", ErrNotDeferred, toolUseID)
	}
	if state.Pending.Answered[toolUseID] {
		return fmt.Errorf("%w: %q", ErrAlreadyAnswered, toolUseID)
	}

	return nil
}

// answeredInConversation reports whether the committed conversation already carries a
// result for toolUseID, which is where an answered call lives once its turn closed.
func answeredInConversation(state *RunState, toolUseID string) bool {
	for _, msg := range state.Messages {
		for _, block := range msg.Content {
			if block.ToolResult != nil && block.ToolResult.ToolUseID == toolUseID {
				return true
			}
		}
	}

	return false
}
