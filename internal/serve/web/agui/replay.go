//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agui

import (
	"strings"
	"time"

	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/types"

	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
)

// snapshot is a stored conversation as the messages of the thread it was: what the
// person typed, what the model wrote and reasoned, the calls it made and what each one
// returned.
//
// The turn the run left unfinished is last. It is not part of the committed
// conversation and it is the one a resume continues, so a client opening a conversation
// stopped on a question holds the call the interrupt names.
func (t *turnWriter) snapshot(rs *runstate.RunState) []types.Message {
	var out []types.Message

	for i, msg := range rs.Messages {
		out = append(out, t.messagesOf(msg, messageTime(rs, i), rs.ResultTimes)...)
	}

	if rs.Pending != nil {
		out = append(out, t.messagesOf(rs.Pending.Assistant, rs.Pending.Time, rs.ResultTimes)...)
		out = append(out, t.results(rs.Pending.Results, rs.ResultTimes)...)
	}

	return out
}

// messageTime is when the message at i was journaled, zero where the run carries no time
// for it. RunState.Times is index aligned with Messages where Fold built it, and a
// RunState assembled by hand may carry none at all.
func messageTime(rs *runstate.RunState, i int) time.Time {
	if i >= len(rs.Times) {
		return time.Time{}
	}

	return rs.Times[i]
}

// messagesOf is one stored message as the AG-UI messages it holds.
//
// A journal message is a list of blocks and an AG-UI message is one role's turn, so a
// message carrying prose and reasoning is two of them and a user message carrying the
// results of the calls before it is one per result.
//
// at is when the record carrying this message was journaled and dates every message it
// produces. A tool result is dated from resultTimes instead, each result being its own
// record.
func (t *turnWriter) messagesOf(msg llm.Message, at time.Time, resultTimes map[string]time.Time) []types.Message {
	var (
		out       []types.Message
		prose     []string
		reasoning []string
		calls     []types.ToolCall
	)

	for _, block := range msg.Content {
		switch {
		case block.ToolResult != nil:
			out = append(out, t.result(*block.ToolResult, resultTimes[block.ToolResult.ToolUseID]))

		case block.ToolUse != nil:
			calls = append(calls, types.ToolCall{
				ID:       block.ToolUse.ID,
				Type:     types.ToolCallTypeFunction,
				Function: types.FunctionCall{Name: block.ToolUse.Name, Arguments: toolInput(block.ToolUse.Input)},
			})

		case block.Thinking != nil && block.Thinking.Text != "":
			reasoning = append(reasoning, block.Thinking.Text)

		case block.Text != nil && block.Text.Text != "":
			prose = append(prose, block.Text.Text)
		}
	}

	if len(reasoning) > 0 {
		id := t.mintID()
		t.date(id, at)

		out = append(out, types.Message{
			ID:      id,
			Role:    types.RoleReasoning,
			Content: strings.Join(reasoning, "\n"),
		})
	}

	if len(prose) == 0 && len(calls) == 0 {
		return out
	}

	id := t.mintID()
	t.date(id, at)

	turn := types.Message{ID: id, Role: role(msg.Role), ToolCalls: calls}
	if len(prose) > 0 {
		turn.Content = strings.Join(prose, "\n")
	}

	return append(out, turn)
}

// results is the stored results of the calls a pending turn made, which the journal
// holds beside that turn rather than in the message after it.
func (t *turnWriter) results(results []llm.ToolResultBlock, resultTimes map[string]time.Time) []types.Message {
	out := make([]types.Message, 0, len(results))

	for _, result := range results {
		out = append(out, t.result(result, resultTimes[result.ToolUseID]))
	}

	return out
}

// result is one call's outcome as the message that answers it, dated at when the journal
// holds its time.
//
// A call that failed carries what the tool said in both fields: error marks the failure,
// and a client renders content.
func (t *turnWriter) result(block llm.ToolResultBlock, at time.Time) types.Message {
	id := t.mintID()
	t.date(id, at)

	out := types.Message{
		ID:         id,
		Role:       types.RoleTool,
		Content:    block.Content,
		ToolCallID: block.ToolUseID,
	}

	if block.IsError {
		out.Error = block.Content
	}

	return out
}

// role is the AG-UI role a stored message's own role is written under. A journal holds
// the two roles a conversation takes, and the system prompt is not one of its messages.
func role(r llm.Role) types.Role {
	if r == llm.RoleAssistant {
		return types.RoleAssistant
	}

	return types.RoleUser
}
