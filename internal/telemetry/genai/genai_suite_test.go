//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package genai

import (
	"encoding/json"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/telemetry"
)

func TestGenAI(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Internal/Telemetry/GenAI")
}

// opts is the capture configuration a spec renders under.
func opts(max int) telemetry.ContentOptions {
	return telemetry.ContentOptions{MaxBytes: max}
}

// full is opts with the whole conversation selected rather than the turn's delta.
func full(max int) telemetry.ContentOptions {
	return telemetry.ContentOptions{Full: true, MaxBytes: max}
}

// text is a user or assistant message of one text block.
func text(role llm.Role, s string) llm.Message {
	return llm.Message{Role: role, Content: []llm.ContentBlock{{Text: &llm.TextBlock{Text: s}}}}
}

// toolCall is an assistant message asking for one tool.
func toolCall(id, name, args string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{ToolUse: &llm.ToolUseBlock{ID: id, Name: name, Input: json.RawMessage(args)}},
	}}
}

// toolResults is the user message answering a batch of tool calls.
func toolResults(id, content string) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{
		{ToolResult: &llm.ToolResultBlock{ToolUseID: id, Content: content}},
	}}
}

// decode unmarshals a rendered document, which is the assertion that matters most in
// this package: a truncated value that no longer parses is worse than no value, since
// it fails only on the largest and most interesting conversations.
func decode(g Gomega, doc string) []map[string]any {
	var out []map[string]any

	g.Expect(json.Unmarshal([]byte(doc), &out)).To(Succeed(), "rendered document must be valid JSON: %s", doc)

	return out
}

// expectToolCallsPaired asserts that every tool result in a document is answered by a
// tool call in the same document.
//
// This is the invariant, rather than "no tool results survive truncation": an id that
// references nothing anywhere in the trace makes a GenAI view render tool output
// attributed to a call that appears never to have happened, and the first reading of
// that is that the instrumentation lost spans.
func expectToolCallsPaired(g Gomega, doc string) {
	calls := map[string]bool{}
	var responses []string

	for _, m := range decode(g, doc) {
		for _, p := range partsOf(g, m) {
			id, _ := p["id"].(string)
			switch p["type"] {
			case partToolCall:
				calls[id] = true
			case partToolCallResponse:
				responses = append(responses, id)
			}
		}
	}

	for _, id := range responses {
		g.Expect(calls).To(HaveKey(id), "tool result %q is answered by no call in: %s", id, doc)
	}
}

// partsOf returns one decoded message's parts.
func partsOf(g Gomega, m map[string]any) []map[string]any {
	raw, ok := m["parts"].([]any)
	g.Expect(ok).To(BeTrue(), "message must carry parts: %v", m)

	out := make([]map[string]any, 0, len(raw))
	for _, p := range raw {
		obj, ok := p.(map[string]any)
		g.Expect(ok).To(BeTrue())
		out = append(out, obj)
	}

	return out
}
