//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2aendpoint

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/a2a"
	wire "github.com/choria-io/fisk-ai/internal/a2a/wire/v1"
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

		req := wire.NewRequest("do the thing")
		req.ID = "req1"
		req.Request = "req1"
		req.Conversation = "req1"
		req.Sender = wire.Identity{Name: "caller1"}

		events = &eventSink{stream: a2a.NewReplyStream(sink, &req.Header, "agent1"), log: quietLogger()}
	})

	// blocks decodes what the sink received, so a spec asserts on the wire form rather
	// than on the values it handed over.
	blocks := func() []wire.Block {
		GinkgoHelper()

		out := make([]wire.Block, 0, len(sink.sent))
		for _, body := range sink.sent {
			var ev wire.Event
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
		thinking := blocks()[0].Content().(wire.ThinkingBlock)
		Expect(thinking.Text).To(Equal("considering"))
		Expect(thinking.Signature).To(BeEmpty(), "the provider payload stays local")
		Expect(blocks()[1].Content().(wire.TextBlock).Text).To(Equal("the answer"))
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

		status := blocks()[1].Content().(wire.StatusBlock)
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

		call := blocks()[0].Content().(wire.ToolCallBlock)
		Expect(call.ID).To(Equal("toolu_1"))
		Expect(call.Name).To(Equal("backup"))
		Expect(string(call.Input)).To(Equal(`{"target":"orders"}`))

		result := blocks()[1].Content().(wire.ToolResultBlock)
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

			Expect(blocks()[0].Content().(wire.ToolCallBlock).Input).To(BeEmpty(), "input %q", input)
		}
	})

	// A ReplyStream refuses an oversized message without advancing the sequence, so an
	// event dropped for size would leave no gap for a caller to notice. Trimming keeps
	// the event and says what happened to the rest.
	It("Should trim a tool result that would not fit", func() {
		oversized := strings.Repeat("x", wire.MaxMessageSize)
		events.ToolResult(agent.ToolResultTrace{CallID: "toolu_1", Output: oversized})

		Expect(sink.sent).To(HaveLen(1))
		output := blocks()[0].Content().(wire.ToolResultBlock).Output
		Expect(len(output)).To(BeNumerically("<", wire.MaxMessageSize))
		// The bound belongs to the wire rather than to this sink, so what is asserted
		// here is that the sink applies it, not what it cuts to.
		Expect(output).To(Equal(wire.TrimBlockText(oversized)))
	})

	It("Should cut on a rune boundary", func() {
		events.ToolResult(agent.ToolResultTrace{CallID: "toolu_1", Output: strings.Repeat("é", wire.MaxBlockText)})

		output := blocks()[0].Content().(wire.ToolResultBlock).Output
		Expect(strings.ContainsRune(output, '�')).To(BeFalse(), "half a rune reaches a caller as a replacement character")
	})

	// A warning says something went wrong short of the run failing, so it travels as
	// what it is rather than as a sentence about it: the kind under its stable name,
	// and the fields that kind fills.
	It("Should send a warning as its kind and fields", func() {
		events.Warn(agent.Warning{Kind: agent.WarnToolTimeout, Name: "stream_rm", Err: errors.New("after 30s")})

		Expect(sink.sent).To(HaveLen(1))
		warning := blocks()[0].Content().(wire.WarningBlock)
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

			opening := sent[0].Content().(wire.StatusBlock)
			Expect(opening.Phase).To(Equal(wire.PhaseReplayStart))

			Expect(sent[1].Content().(wire.PromptBlock).Text).To(Equal("how many streams"))
			Expect(sent[2].Content().(wire.TextBlock).Text).To(Equal("three"))

			closing := sent[5].Content().(wire.StatusBlock)
			Expect(closing.Phase).To(Equal(wire.PhaseReplayEnd))
			Expect(closing.Count).To(Equal(4))
			Expect(closing.Truncated).To(BeFalse())
		})

		// A caller sees where its history begins rather than being left to read a
		// conversation that seems to have started mid-sentence.
		It("Should say when older blocks were left behind", func() {
			events.replay = 2
			events.ResumeTranscript(stored())

			sent := blocks()
			closing := sent[len(sent)-1].Content().(wire.StatusBlock)
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

	// A caller reconciles the fragments it buffered against the whole block, so the block
	// says where it sat in the call that produced it rather than where it sat in what
	// reached the wire.
	It("Should number a block by its place in the model call", func() {
		events.Message(llm.Response{Content: []llm.ContentBlock{
			{Thinking: &llm.ThinkingBlock{Text: "considering"}},
			{ToolUse: &llm.ToolUseBlock{ID: "toolu_1", Name: "backup"}},
			{Text: &llm.TextBlock{Text: "the answer"}},
		}}, true)

		sent := blocks()
		Expect(sent).To(HaveLen(2), "the tool-use block is left to ToolCall")
		Expect(sent[0].Content().(wire.ThinkingBlock).Index).To(Equal(0))
		Expect(sent[1].Content().(wire.TextBlock).Index).To(Equal(2), "a block that never reaches the wire still counts")
	})

	// A caller holding fragments of a trimmed block has the more complete copy of it, the
	// fragments never having been capped in aggregate, so it is told which block was cut.
	It("Should mark a block whose text was cut", func() {
		events.Message(llm.Response{Content: []llm.ContentBlock{
			{Text: &llm.TextBlock{Text: strings.Repeat("x", wire.MaxBlockText+1)}},
			{Thinking: &llm.ThinkingBlock{Text: "considering"}},
		}}, false)

		sent := blocks()
		Expect(sent[0].Content().(wire.TextBlock).Trimmed).To(BeTrue())
		Expect(sent[0].Content().(wire.TextBlock).Text).To(Equal(wire.TrimBlockText(strings.Repeat("x", wire.MaxBlockText+1))))
		Expect(sent[1].Content().(wire.ThinkingBlock).Trimmed).To(BeFalse(), "nothing was cut from it")
	})

	Describe("Streaming an assistant turn as it is written", func() {
		// fragment is one text fragment of the block at index, so a spec says what the
		// provider produced rather than building the value each time.
		fragment := func(index int, text string, final bool) llm.Delta {
			return llm.Delta{Kind: llm.DeltaText, Index: index, Text: text, Final: final}
		}

		// deltaText is what the fragments of the block at index carried, in the order
		// they reached the wire.
		deltaText := func(index int) string {
			GinkgoHelper()

			var out strings.Builder
			for _, block := range blocks() {
				delta, ok := block.Content().(wire.TextDeltaBlock)
				if ok && delta.Index == index {
					out.WriteString(delta.Text)
				}
			}

			return out.String()
		}

		// A caller that asked for nothing gets the blocks it gets without the property.
		Context("for a caller that asked for none", func() {
			It("Should send no fragment and the whole blocks unchanged", func() {
				Expect(events.StreamDeltas()).To(BeFalse())

				events.MessageDelta(fragment(0, "the ", false))
				events.MessageDelta(fragment(0, "answer", true))
				Expect(sink.sent).To(BeEmpty())

				events.Message(llm.Response{Content: []llm.ContentBlock{{Text: &llm.TextBlock{Text: "the answer"}}}}, true)

				Expect(sink.sent).To(HaveLen(1))
				body, err := json.Marshal(blocks()[0])
				Expect(err).ToNot(HaveOccurred())
				Expect(string(body)).To(Equal(`{"text":"the answer","final":true}`), "the two new fields are unset and omitted")
			})
		})

		Context("for a caller that asked for them", func() {
			BeforeEach(func() {
				events.deltas = true
			})

			It("Should answer the runner from the request", func() {
				Expect(events.StreamDeltas()).To(BeTrue())
			})

			// A message per fragment would tie the wire cost of a run to the rate the
			// backend writes at.
			//
			// The window is measured from a wall clock, so a spec that must not flush on
			// it holds the buffer's clock ahead of now. A loaded machine would otherwise
			// take longer between two statements here than a run takes between fragments.
			It("Should hold a fragment that is under both limits", func() {
				events.MessageDelta(fragment(0, "the ", false))
				events.buffered[0].since = time.Now().Add(time.Hour)
				events.MessageDelta(fragment(0, "answer", false))

				Expect(sink.sent).To(BeEmpty())
			})

			It("Should send what it holds when the fragments reach the byte limit", func() {
				events.MessageDelta(fragment(0, strings.Repeat("x", maxDeltaText-1), false))
				Expect(sink.sent).To(BeEmpty())

				events.buffered[0].since = time.Now().Add(time.Hour)
				events.MessageDelta(fragment(0, "yz", false))

				Expect(sink.sent).To(HaveLen(1))
				delta := blocks()[0].Content().(wire.TextDeltaBlock)
				Expect(delta.Text).To(HaveLen(maxDeltaText))
				Expect(delta.Final).To(BeFalse())
				Expect(deltaText(0)).To(Equal(strings.Repeat("x", maxDeltaText-1)+"y"), "the byte over the limit is still held")
			})

			// The window is read when a fragment arrives and never on a timer: a timer
			// publishing a partial buffer would race the run's own events on a stream
			// whose sequence counter is unguarded.
			It("Should send what it holds once the oldest byte has waited", func() {
				events.MessageDelta(fragment(0, "the ", false))
				Expect(sink.sent).To(BeEmpty())

				events.buffered[0].since = time.Now().Add(-2 * deltaFlushWindow)
				events.MessageDelta(fragment(0, "answer", false))

				Expect(sink.sent).To(HaveLen(1))
				Expect(deltaText(0)).To(Equal("the answer"))
			})

			It("Should send what it holds when the block ends", func() {
				events.MessageDelta(fragment(0, "the ", false))
				events.MessageDelta(fragment(0, "answer", true))

				Expect(sink.sent).To(HaveLen(1))
				delta := blocks()[0].Content().(wire.TextDeltaBlock)
				Expect(delta.Text).To(Equal("the answer"))
				Expect(delta.Final).To(BeTrue())
			})

			// The end of a block cannot be read off the fragments, so the mark that
			// closes it travels even when there is nothing left to send.
			It("Should close a block whose last fragment carries no text", func() {
				events.MessageDelta(fragment(0, "the answer", true))
				events.MessageDelta(llm.Delta{Kind: llm.DeltaText, Index: 1, Final: true})

				sent := blocks()
				Expect(sent).To(HaveLen(2))
				Expect(sent[1].Content().(wire.TextDeltaBlock).Text).To(BeEmpty())
				Expect(sent[1].Content().(wire.TextDeltaBlock).Final).To(BeTrue())
			})

			// A caller assembling text out of fragments cannot notice a missing one, so a
			// fragment too large for one message is split across two rather than dropped.
			It("Should split a fragment larger than the flush cap", func() {
				written := strings.Repeat("x", 2*maxDeltaText+7)
				events.MessageDelta(fragment(0, written, true))

				Expect(sink.sent).To(HaveLen(3))
				for _, body := range sink.sent {
					Expect(len(body)).To(BeNumerically("<", wire.MaxMessageSize))
				}
				Expect(deltaText(0)).To(Equal(written), "nothing is lost to the split")
				Expect(blocks()[2].Content().(wire.TextDeltaBlock).Final).To(BeTrue())
			})

			It("Should split on a rune boundary", func() {
				written := "x" + strings.Repeat("☃", maxDeltaText)
				events.MessageDelta(fragment(0, written, true))

				Expect(deltaText(0)).To(Equal(written))
				for _, block := range blocks() {
					Expect(strings.ContainsRune(block.Content().(wire.TextDeltaBlock).Text, '�')).To(BeFalse())
				}
			})

			// Text that is not valid UTF-8 has no rune boundary to cut back to. Without a
			// floor the cut walks to zero, the split loop re-slices by nothing and the run
			// goroutine hangs inside the provider call, so this spec hangs the suite when
			// the floor is gone. JSON encoding replaces the bytes on the way out, so what
			// arrives cannot be compared with what was written.
			It("Should move text that begins no rune rather than stalling on it", func() {
				events.MessageDelta(fragment(0, strings.Repeat("\x80", maxDeltaText+1), true))

				Expect(len(sink.sent)).To(BeNumerically(">", 1))
				Expect(blocks()[len(sink.sent)-1].Content().(wire.TextDeltaBlock).Final).To(BeTrue())
			})

			// A caller reconciles its buffer against the whole block, so the block never
			// arrives before the fragments it replaces.
			It("Should send every fragment of a block before the block itself", func() {
				events.MessageDelta(llm.Delta{Kind: llm.DeltaThinking, Index: 0, Text: "considering", Final: true})
				events.MessageDelta(fragment(1, "the ", false))
				events.buffered[1].since = time.Now().Add(time.Hour)
				events.MessageDelta(fragment(1, "answer", true))
				events.Message(llm.Response{Content: []llm.ContentBlock{
					{Thinking: &llm.ThinkingBlock{Text: "considering"}},
					{Text: &llm.TextBlock{Text: "the answer"}},
				}}, true)

				types := make([]wire.BlockType, 0, len(sink.sent))
				for _, block := range blocks() {
					types = append(types, block.Type())
				}

				Expect(types).To(Equal([]wire.BlockType{
					wire.BlockThinkingDelta,
					wire.BlockTextDelta,
					wire.BlockThinking,
					wire.BlockText,
				}))
			})

			// Index restarts at 0 on every call while the status block that separates two
			// calls arrives after the first has ended, so a caller keying on index alone
			// would append the first block of one call to the last block of the one before.
			It("Should say which model call a fragment came from", func() {
				events.MessageDelta(fragment(0, "working", true))
				events.Message(llm.Response{Content: []llm.ContentBlock{{Text: &llm.TextBlock{Text: "working"}}}}, false)
				events.MessageDelta(fragment(0, "the answer", true))

				sent := blocks()
				Expect(sent[0].Content().(wire.TextDeltaBlock).Iteration).To(Equal(1))
				Expect(sent[2].Content().(wire.StatusBlock).Iteration).To(Equal(1), "the fragments and the status block count the same call")
				Expect(sent[3].Content().(wire.TextDeltaBlock).Iteration).To(Equal(2))
			})

			// A block left open by a call that ended without closing it would otherwise
			// have the next call's first fragment appended to it.
			It("Should not carry a buffer across a model call", func() {
				events.MessageDelta(fragment(0, "half a th", false))
				events.Message(llm.Response{Content: []llm.ContentBlock{{Text: &llm.TextBlock{Text: "half a thought"}}}}, false)
				events.MessageDelta(fragment(0, "the answer", true))

				sent := blocks()
				Expect(sent).To(HaveLen(3), "the whole block, the status block that ends the call, and the fragment after it")
				Expect(sent[2].Content().(wire.TextDeltaBlock).Text).To(Equal("the answer"))
			})

			// A provider bounds its own indexes, but this sink sees fragments alone.
			It("Should buffer no more blocks than the cap", func() {
				for index := range maxDeltaBlocks {
					events.MessageDelta(fragment(index, "held", false))
				}
				Expect(sink.sent).To(BeEmpty())
				Expect(events.buffered).To(HaveLen(maxDeltaBlocks))

				for index := maxDeltaBlocks; index < maxDeltaBlocks+10; index++ {
					events.MessageDelta(fragment(index, "invented", false))
					events.MessageDelta(fragment(index, "invented", true))
				}

				Expect(events.buffered).To(HaveLen(maxDeltaBlocks))
				Expect(sink.sent).To(BeEmpty(), "a fragment the sink cannot buffer sends nothing")
			})

			It("Should send a reasoning fragment as a thinking delta", func() {
				events.MessageDelta(llm.Delta{Kind: llm.DeltaThinking, Index: 0, Text: "considering", Final: true})

				delta := blocks()[0].Content().(wire.ThinkingDeltaBlock)
				Expect(delta.Text).To(Equal("considering"))
				Expect(delta.Final).To(BeTrue())
			})

			It("Should send nothing for a kind it does not name", func() {
				events.MessageDelta(llm.Delta{Kind: "images", Index: 0, Text: "...", Final: true})

				Expect(sink.sent).To(BeEmpty())
			})
		})
	})

	// The answer travels twice, as the last text block and as the result, and only the
	// run knows which message ended it.
	It("Should mark the text of the terminal turn", func() {
		events.Message(llm.Response{Content: []llm.ContentBlock{{Text: &llm.TextBlock{Text: "still working"}}}}, false)
		events.Message(llm.Response{Content: []llm.ContentBlock{{Text: &llm.TextBlock{Text: "there are three"}}}}, true)

		sent := blocks()
		Expect(sent[0].Content().(wire.TextBlock).Final).To(BeFalse())
		Expect(sent[len(sent)-1].Content().(wire.TextBlock).Final).To(BeTrue())
	})
})
