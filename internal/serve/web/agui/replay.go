//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agui

import (
	"strings"

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

	for _, msg := range rs.Messages {
		out = append(out, t.messagesOf(msg)...)
	}

	if rs.Pending != nil {
		out = append(out, t.messagesOf(rs.Pending.Assistant)...)
		out = append(out, t.results(rs.Pending.Results)...)
	}

	return out
}

// messagesOf is one stored message as the AG-UI messages it holds.
//
// A journal message is a list of blocks and an AG-UI message is one role's turn, so a
// message carrying prose and reasoning is two of them and a user message carrying the
// results of the calls before it is one per result.
func (t *turnWriter) messagesOf(msg llm.Message) []types.Message {
	var (
		out       []types.Message
		prose     []string
		reasoning []string
		calls     []types.ToolCall
	)

	for _, block := range msg.Content {
		switch {
		case block.ToolResult != nil:
			out = append(out, t.result(*block.ToolResult))

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
		out = append(out, types.Message{
			ID:      t.mintID(),
			Role:    types.RoleReasoning,
			Content: strings.Join(reasoning, "\n"),
		})
	}

	if len(prose) == 0 && len(calls) == 0 {
		return out
	}

	turn := types.Message{ID: t.mintID(), Role: role(msg.Role), ToolCalls: calls}
	if len(prose) > 0 {
		turn.Content = strings.Join(prose, "\n")
	}

	return append(out, turn)
}

// results is the stored results of the calls a pending turn made, which the journal
// holds beside that turn rather than in the message after it.
func (t *turnWriter) results(results []llm.ToolResultBlock) []types.Message {
	out := make([]types.Message, 0, len(results))

	for _, result := range results {
		out = append(out, t.result(result))
	}

	return out
}

// result is one call's outcome as the message that answers it.
//
// A call that failed carries what the tool said in both fields: error marks the failure,
// and a client renders content.
func (t *turnWriter) result(block llm.ToolResultBlock) types.Message {
	out := types.Message{
		ID:         t.mintID(),
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
