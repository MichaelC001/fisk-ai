//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2asurface

import (
	"encoding/json"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/llm"
)

// collectingSink records what a reply stream was asked to send, so a spec reads the
// blocks a run produced without a broker.
type collectingSink struct {
	sent [][]byte
}

func (c *collectingSink) Respond(body []byte) error { c.sent = append(c.sent, body); return nil }
func (c *collectingSink) Error(string, string) error {
	return nil
}
func (c *collectingSink) Publish(body []byte, _ bool) error {
	c.sent = append(c.sent, body)

	return nil
}

var _ = Describe("The events adapter", func() {
	var (
		sink   *collectingSink
		events *eventSink
	)

	BeforeEach(func() {
		sink = &collectingSink{}

		req := a2a.NewRequest("do the thing")
		req.ID = "req1"
		req.Request = "req1"
		req.Conversation = "req1"
		req.Sender = a2a.Identity{Name: "caller1"}

		events = &eventSink{stream: a2a.NewReplyStream(sink, &req.Header, "agent1"), log: quietLogger()}
	})

	// blocks decodes what the sink received, so a spec asserts on the wire form rather
	// than on the values it handed over.
	blocks := func() []a2a.Block {
		GinkgoHelper()

		out := make([]a2a.Block, 0, len(sink.sent))
		for _, body := range sink.sent {
			var ev a2a.Event
			Expect(json.Unmarshal(body, &ev)).To(Succeed())
			out = append(out, ev.Block)
		}

		return out
	}

	It("Should send the model's text and its reasoning", func() {
		events.Message(llm.Response{Content: []llm.ContentBlock{
			{Thinking: &llm.ThinkingBlock{Text: "considering", Signature: []byte("opaque")}},
			{Text: &llm.TextBlock{Text: "the answer"}},
		}}, true)

		Expect(blocks()).To(HaveLen(2))
		thinking := blocks()[0].Content().(a2a.ThinkingBlock)
		Expect(thinking.Text).To(Equal("considering"))
		Expect(thinking.Signature).To(BeEmpty(), "the provider payload stays local")
		Expect(blocks()[1].Content().(a2a.TextBlock).Text).To(Equal("the answer"))
	})

	// A tool call reaches the wire once. The model's own tool-use content is skipped
	// because ToolCall carries the same call with the tool's description of it.
	It("Should leave the model's tool-use content to ToolCall", func() {
		events.Message(llm.Response{Content: []llm.ContentBlock{
			{ToolUse: &llm.ToolUseBlock{ID: "toolu_1", Name: "backup"}},
		}}, true)

		Expect(sink.sent).To(BeEmpty())
	})

	It("Should mark the end of an iteration with what the call cost", func() {
		events.Message(llm.Response{
			Content: []llm.ContentBlock{{Text: &llm.TextBlock{Text: "working"}}},
			Usage:   llm.Usage{In: 10, Out: 5, CacheRead: 90},
		}, false)

		status := blocks()[1].Content().(a2a.StatusBlock)
		Expect(status.Iteration).To(Equal(1))
		Expect(status.Usage.InputTokens).To(Equal(int64(100)), "cached input is part of what the call consumed")
		Expect(status.Usage.OutputTokens).To(Equal(int64(5)))
	})

	It("Should send no status block for the terminal turn", func() {
		events.Message(llm.Response{Content: []llm.ContentBlock{{Text: &llm.TextBlock{Text: "done"}}}}, true)

		Expect(blocks()).To(HaveLen(1))
	})

	It("Should pair a call and its result on the tool_use id", func() {
		events.ToolCall(agent.ToolTrace{ID: "toolu_1", Name: "backup", Input: json.RawMessage(`{"target":"orders"}`)})
		events.ToolResult(agent.ToolResultTrace{CallID: "toolu_1", Output: "backed up", IsError: false})

		call := blocks()[0].Content().(a2a.ToolCallBlock)
		Expect(call.ID).To(Equal("toolu_1"))
		Expect(call.Name).To(Equal("backup"))
		Expect(string(call.Input)).To(Equal(`{"target":"orders"}`))

		result := blocks()[1].Content().(a2a.ToolResultBlock)
		Expect(result.CallID).To(Equal("toolu_1"))
		Expect(result.Output).To(Equal("backed up"))
	})

	// The schema types the field as an object, and a receiver validates every message of
	// the set, so anything else would cost the caller the whole event rather than one
	// field.
	It("Should drop an input that is not a JSON object", func() {
		for _, input := range []string{`"a string"`, `null`, `[1,2]`, `{"broken":`} {
			sink.sent = nil
			events.ToolCall(agent.ToolTrace{ID: "toolu_1", Name: "backup", Input: json.RawMessage(input)})

			Expect(blocks()[0].Content().(a2a.ToolCallBlock).Input).To(BeEmpty(), "input %q", input)
		}
	})

	// A ReplyStream refuses an oversized message without advancing the sequence, so an
	// event dropped for size would leave no gap for a caller to notice. Trimming keeps
	// the event and says what happened to the rest.
	It("Should trim a tool result that would not fit", func() {
		events.ToolResult(agent.ToolResultTrace{CallID: "toolu_1", Output: strings.Repeat("x", a2a.MaxMessageSize)})

		Expect(sink.sent).To(HaveLen(1))
		output := blocks()[0].Content().(a2a.ToolResultBlock).Output
		Expect(len(output)).To(BeNumerically("<", a2a.MaxMessageSize))
		Expect(output).To(HaveSuffix(trimMarker))
	})

	It("Should cut on a rune boundary", func() {
		events.ToolResult(agent.ToolResultTrace{CallID: "toolu_1", Output: strings.Repeat("é", maxWireText)})

		output := blocks()[0].Content().(a2a.ToolResultBlock).Output
		Expect(strings.TrimSuffix(output, trimMarker)).To(BeAssignableToTypeOf(""))
		Expect(strings.ContainsRune(output, '�')).To(BeFalse(), "half a rune reaches a caller as a replacement character")
	})

	// No block type carries an advisory and no peer may see a stack, so both stop at
	// this worker's log.
	It("Should send nothing for a warning or a panic", func() {
		events.Warn(agent.Warning{Kind: agent.WarnConfirmNoTerminal, Count: 2})
		events.Panicked("boom", []byte("goroutine 1 [running]: /home/rip/agent.go:41"))
		events.Starting(agent.RunInfo{Tools: 3})
		events.LLMRequest("a summary")
		events.SessionRotated("session1")

		Expect(sink.sent).To(BeEmpty())
	})
})
