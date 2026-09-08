//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package web

import (
	"encoding/json"
	"slices"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// PendingQuestion is the question a stored conversation is waiting on, and whether it is
// waiting on one.
//
// Nothing journals a question: runstate has no question record, and the answer lives for
// the one turn that carries it. So the question is rebuilt from the pending turn, whose
// assistant message holds each unanswered call's id, name and input.
//
// A pending turn is one journaled mid-batch, which a crash or a takeover reaches as
// readily as a suspend on a question, so its unanswered calls are read the way a resume
// dispatches them. A call the confirm gate runs without asking is passed over, and the
// first call somebody is asked about is the question. The gate asks about a call whose
// tool is toolkit.Confirmable and whose tags trigger it for confirmTags, and it asks
// about none the operator has answered already: a standing approval on the tool name in
// RunState.Approvals, or a live one-shot approval on the call in RunState.CallApprovals.
// A call whose tool the agent no longer holds reaches no gate, since the next turn's
// redispatch answers the model that the tool is unknown.
//
// The three human-in-the-loop tools are dispatched rather than gated and put their
// question themselves, so an unanswered one is outstanding and the call's input is the
// question. Every other question is an approval, whose rendered command line is the
// tool's own TraceLine for these arguments, or its Describer line where it has no
// TraceLine, the same line the runner's gate presents at dispatch. tools is the agent's
// tool set keyed by name, resolved once where the channel is built.
//
// confirmTags are the operator's extra confirm tags, which name the tag that gated a
// command the always-on ai:confirm did not.
func PendingQuestion(state *runstate.RunState, tools map[string]toolkit.Tool, confirmTags []string) (Question, bool) {
	if state == nil || state.Pending == nil {
		return Question{}, false
	}

	for _, use := range openCalls(state.Pending) {
		switch use.Name {
		case config.AskHumanConfirmToolName:
			return humanQuestion(use, KindConfirm), true
		case config.AskHumanSelectToolName:
			return humanQuestion(use, KindSelect), true
		case config.AskHumanInputToolName:
			return humanQuestion(use, KindInput), true
		}

		if approvedAlready(state, use) {
			continue
		}

		tool, held := tools[use.Name]
		if !held {
			continue
		}
		if !gated(tool, confirmTags) {
			continue
		}

		return approvalQuestion(use, tool, confirmTags), true
	}

	return Question{}, false
}

// openCalls are the calls of a pending turn that have no result and are not deferred, in
// the order a resume dispatches them.
//
// A deferred call is left out because it puts no question: the tool
// started the work and the answer arrives later, so a resume leaves it alone rather than
// dispatching it again.
func openCalls(pending *runstate.PendingTurn) []llm.ToolUseBlock {
	var out []llm.ToolUseBlock

	for _, block := range pending.Assistant.Content {
		if block.ToolUse == nil {
			continue
		}
		if pending.Answered[block.ToolUse.ID] {
			continue
		}

		_, deferred := pending.Deferred[block.ToolUse.ID]
		if deferred {
			continue
		}

		out = append(out, *block.ToolUse)
	}

	return out
}

// approvedAlready reports whether the operator has answered for this call already, which
// the gate honors on the resume that dispatches it: a standing approval recorded under
// the tool's name, or a one-shot approval recorded under the call's id.
func approvedAlready(state *runstate.RunState, use llm.ToolUseBlock) bool {
	if slices.Contains(state.Approvals, use.Name) {
		return true
	}

	return slices.ContainsFunc(state.CallApprovals, func(rec runstate.CallApprovalRecord) bool {
		return rec.ToolUseID == use.ID
	})
}

// gated reports whether the confirm gate asks about a call to tool, on the terms the
// runner applies: the tool opts into confirmation through toolkit.Confirmable and its
// tags trigger the gate for the operator's extra confirm tags.
func gated(tool toolkit.Tool, confirmTags []string) bool {
	c, confirmable := tool.(toolkit.Confirmable)

	return confirmable && c.NeedsConfirm(confirmTags)
}

// humanArgs are the arguments the three human-in-the-loop tools take, which is where the
// question the run put is stored.
type humanArgs struct {
	Question string   `json:"question"`
	Options  []string `json:"options"`
	Default  string   `json:"default"`
}

// humanQuestion reads a human-in-the-loop call's arguments as the question it asked.
// Arguments the tool would have refused leave the question empty, and the page is still
// shown the call the resume dispatches.
func humanQuestion(use llm.ToolUseBlock, kind QuestionKind) Question {
	q := Question{Kind: kind, ToolUseID: use.ID}

	var args humanArgs

	err := json.Unmarshal(use.Input, &args)
	if err != nil {
		return q
	}

	q.Text = args.Question

	switch kind {
	case KindSelect:
		q.Options = args.Options
	case KindInput:
		q.Default = args.Default
	}

	return q
}

// approvalQuestion is the approval a stored call is waiting on, rendered as the gate
// renders it at dispatch: the command path, the command line for these arguments, and the
// tag that gated it.
func approvalQuestion(use llm.ToolUseBlock, tool toolkit.Tool, confirmTags []string) Question {
	q := Question{Kind: KindApprove, ToolUseID: use.ID, Command: use.Name}

	c, confirmable := tool.(toolkit.Confirmable)
	if confirmable {
		q.Command = c.Command()
		q.Display = c.TraceLine(use.Input)
		q.Tag = c.ConfirmTrigger(confirmTags)
	}

	// A tool that renders no command line of its own describes its call instead, which
	// is the fallback the runner takes for a call rewritten onto a tool the gate cannot
	// ask about directly.
	if q.Display == "" {
		d, describes := tool.(toolkit.Describer)
		if describes {
			q.Display = d.Describe(use.Input).Display
		}
	}

	if q.Display == "" {
		q.Display = use.Name
	}

	return q
}

// toolsByName keys a resolved tool set for the lookups a rebuilt question makes.
func toolsByName(tools []toolkit.Tool) map[string]toolkit.Tool {
	out := make(map[string]toolkit.Tool, len(tools))
	for _, t := range tools {
		out[t.Name()] = t
	}

	return out
}
