//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2aendpoint

import (
	"encoding/json"
	"errors"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
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
		oversized := strings.Repeat("x", a2a.MaxMessageSize)
		events.ToolResult(agent.ToolResultTrace{CallID: "toolu_1", Output: oversized})

		Expect(sink.sent).To(HaveLen(1))
		output := blocks()[0].Content().(a2a.ToolResultBlock).Output
		Expect(len(output)).To(BeNumerically("<", a2a.MaxMessageSize))
		// The bound belongs to the wire rather than to this sink, so what is asserted
		// here is that the sink applies it, not what it cuts to.
		Expect(output).To(Equal(a2a.TrimBlockText(oversized)))
	})

	It("Should cut on a rune boundary", func() {
		events.ToolResult(agent.ToolResultTrace{CallID: "toolu_1", Output: strings.Repeat("é", a2a.MaxBlockText)})

		output := blocks()[0].Content().(a2a.ToolResultBlock).Output
		Expect(strings.ContainsRune(output, '�')).To(BeFalse(), "half a rune reaches a caller as a replacement character")
	})

	// A warning says something went wrong short of the run failing, so it travels as
	// what it is rather than as a sentence about it: the kind under its stable name,
	// and the fields that kind fills.
	It("Should send a warning as its kind and fields", func() {
		events.Warn(agent.Warning{Kind: agent.WarnToolTimeout, Name: "stream_rm", Err: errors.New("after 30s")})

		Expect(sink.sent).To(HaveLen(1))
		warning := blocks()[0].Content().(a2a.WarningBlock)
		Expect(warning.Kind).To(Equal("tool_timeout"))
		Expect(warning.Name).To(Equal("stream_rm"))
		Expect(warning.Error).To(Equal("after 30s"))
	})

	Describe("Replaying a stored conversation", func() {
		stored := func() *runstate.RunState {
			return &runstate.RunState{Messages: []llm.Message{
				{Role: llm.RoleUser, Content: []llm.ContentBlock{{Text: &llm.TextBlock{Text: "how many streams"}}}},
				{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Text: &llm.TextBlock{Text: "three"}}}},
				{Role: llm.RoleUser, Content: []llm.ContentBlock{{Text: &llm.TextBlock{Text: "and the first"}}}},
				{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Text: &llm.TextBlock{Text: "ORDERS"}}}},
			}}
		}

		// Bracketed, so a client renders what already happened as history rather than as
		// a turn arriving now.
		It("Should send the conversation between two status blocks", func() {
			events.replay = 10
			events.ResumeTranscript(stored())

			sent := blocks()
			Expect(sent).To(HaveLen(6))

			opening := sent[0].Content().(a2a.StatusBlock)
			Expect(opening.Phase).To(Equal(a2a.PhaseReplayStart))

			Expect(sent[1].Content().(a2a.PromptBlock).Text).To(Equal("how many streams"))
			Expect(sent[2].Content().(a2a.TextBlock).Text).To(Equal("three"))

			closing := sent[5].Content().(a2a.StatusBlock)
			Expect(closing.Phase).To(Equal(a2a.PhaseReplayEnd))
			Expect(closing.Count).To(Equal(4))
			Expect(closing.Truncated).To(BeFalse())
		})

		// A caller sees where its history begins rather than being left to read a
		// conversation that seems to have started mid-sentence.
		It("Should say when older blocks were left behind", func() {
			events.replay = 2
			events.ResumeTranscript(stored())

			sent := blocks()
			closing := sent[len(sent)-1].Content().(a2a.StatusBlock)
			Expect(closing.Count).To(Equal(2))
			Expect(closing.Truncated).To(BeTrue())
		})

		// Zero is what a peer agent gets and what every turn after the first gets: the
		// answer, not a transcript.
		It("Should send nothing for a caller that asked for none", func() {
			events.ResumeTranscript(stored())

			Expect(sink.sent).To(BeEmpty())
		})
	})

	// A stack names paths, module layout and frame arguments, and the rest describe a
	// run to something rendering it locally, so all four stop at this worker.
	It("Should send nothing for a panic or the local narration", func() {
		events.Panicked("boom", []byte("goroutine 1 [running]: /home/rip/agent.go:41"))
		events.Starting(agent.RunInfo{Tools: 3})
		events.LLMRequest("a summary")
		events.SessionRotated("session1")

		Expect(sink.sent).To(BeEmpty())
	})

	// The answer travels twice, as the last text block and as the result, and only the
	// run knows which message ended it.
	It("Should mark the text of the terminal turn", func() {
		events.Message(llm.Response{Content: []llm.ContentBlock{{Text: &llm.TextBlock{Text: "still working"}}}}, false)
		events.Message(llm.Response{Content: []llm.ContentBlock{{Text: &llm.TextBlock{Text: "there are three"}}}}, true)

		sent := blocks()
		Expect(sent[0].Content().(a2a.TextBlock).Final).To(BeFalse())
		Expect(sent[len(sent)-1].Content().(a2a.TextBlock).Final).To(BeTrue())
	})
})
