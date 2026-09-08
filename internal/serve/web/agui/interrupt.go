//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agui

import (
	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/types"

	"github.com/choria-io/fisk-ai/internal/serve/web"
)

// A client switches on the reason of an interrupt to render it, so each one here is a
// value the protocol names. A reason of this agent's own would have to be
// namespaced and would reach every client as the generic fallback.
const (
	// reasonToolCall is the interrupt of a command the model asked to run and the
	// operator has to allow first. The call has not been dispatched.
	reasonToolCall = "tool_call"
	// reasonConfirmation is the interrupt of a question answered yes or no.
	reasonConfirmation = "confirmation"
	// reasonInputRequired is the interrupt of a question answered with a value: one of
	// a list, or free text.
	reasonInputRequired = "input_required"
)

// interruptFor is the AG-UI interrupt one question is raised as.
//
// The payload that answers it is described by a JSON Schema rather than left to a
// client that knows this agent, which lets a frontend written elsewhere render an
// approval and the three human-in-the-loop questions.
//
// The metadata names the kind, so a client branches on it without reading the id apart.
// Everything else a client needs is in the schema.
func interruptFor(q web.Question) types.Interrupt {
	out := types.Interrupt{
		ID:         interruptID(q.Kind, q.ToolUseID),
		Message:    q.Text,
		ToolCallID: q.ToolUseID,
		Metadata:   map[string]any{"kind": string(q.Kind)},
	}

	switch q.Kind {
	case web.KindApprove:
		out.Reason = reasonToolCall
		out.Message = q.Display
		out.ResponseSchema = approvalSchema()
		out.Metadata["command"] = q.Command

		if q.Tag != "" {
			out.Metadata["tag"] = q.Tag
		}

	case web.KindConfirm:
		out.Reason = reasonConfirmation
		out.ResponseSchema = confirmSchema()

	case web.KindSelect:
		out.Reason = reasonInputRequired
		out.ResponseSchema = selectSchema(q.Options)
		out.Metadata["options"] = q.Options

	case web.KindInput:
		out.Reason = reasonInputRequired
		out.ResponseSchema = inputSchema(q.Default)
	}

	return out
}

// approvalSchema asks for the three-way answer to a confirm gate.
//
// AG-UI recommends a boolean and the payload is an object of whatever shape the schema
// asks for, so the standing allow is one of three values rather than a field beside the
// answer. A client that renders its own two-way control and sends {"approved": bool}
// instead is read too, and can allow a command once or decline it.
func approvalSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"approval": map[string]any{
				"type": "string",
				"enum": []string{"no", "once", "always"},
				"oneOf": []any{
					map[string]any{"const": "no", "title": "Do not run it"},
					map[string]any{"const": "once", "title": "Run it this time"},
					map[string]any{"const": "always", "title": "Run it and stop asking about this tool"},
				},
			},
		},
		"required": []string{"approval"},
	}
}

// confirmSchema asks for a yes or a no.
func confirmSchema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"confirmed": map[string]any{"type": "boolean"}},
		"required":   []string{"confirmed"},
	}
}

// selectSchema asks for one of a list by its position.
//
// The answer is the position rather than the option, because a run resumes with the
// answer alone and the list the options came from is not sent back with it. The words
// are in the schema as the title of each position, which a client renders, and
// in the interrupt's metadata for a client that draws its own control.
func selectSchema(options []string) map[string]any {
	choices := make([]any, 0, len(options))
	positions := make([]int, 0, len(options))

	for i, option := range options {
		choices = append(choices, map[string]any{"const": i, "title": option})
		positions = append(positions, i)
	}

	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"index": map[string]any{
				"type":  "integer",
				"enum":  positions,
				"oneOf": choices,
			},
		},
		"required": []string{"index"},
	}
}

// inputSchema asks for a value, pre-filled where the tool suggested one.
func inputSchema(value string) map[string]any {
	field := map[string]any{"type": "string"}
	if value != "" {
		field["default"] = value
	}

	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"value": field},
		"required":   []string{"value"},
	}
}
