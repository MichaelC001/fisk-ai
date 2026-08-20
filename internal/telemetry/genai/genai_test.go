//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package genai

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/telemetry"
)

var _ = Describe("Content builders", func() {
	Describe("InputMessages", func() {
		It("renders the conversation in the schema shape", func() {
			msgs := []llm.Message{
				text(llm.RoleUser, "weather in Paris?"),
				toolCall("call_1", "get_weather", `{"location":"Paris"}`),
				toolResults("call_1", `{"conditions":"rainy"}`),
			}

			c := InputMessages(msgs, 0)(full(4096))
			Expect(c.Truncated).To(BeFalse())

			out := decode(Default, c.JSON)
			Expect(out).To(HaveLen(3))

			Expect(out[0]["role"]).To(Equal("user"))
			Expect(partsOf(Default, out[0])[0]["type"]).To(Equal(partText))

			call := partsOf(Default, out[1])[0]
			Expect(call["type"]).To(Equal(partToolCall))
			Expect(call["id"]).To(Equal("call_1"))
			Expect(call["name"]).To(Equal("get_weather"))
			Expect(call["arguments"]).To(HaveKeyWithValue("location", "Paris"))

			// A message whose every part answers a tool call takes the conventions'
			// tool role, not the user role the neutral model batches it under.
			Expect(out[2]["role"]).To(Equal(roleTool))
			Expect(partsOf(Default, out[2])[0]["type"]).To(Equal(partToolCallResponse))
		})

		It("keeps the neutral role when a follow-up was folded into a results turn", func() {
			// appendUserPrompt folds an interactive follow-up into a trailing user
			// message, and a tool-results batch is one. The message is then no longer
			// purely tool output, so calling it the tool role would misreport who
			// wrote the text block.
			mixed := toolResults("call_1", "done")
			mixed.Content = append(mixed.Content, llm.ContentBlock{Text: &llm.TextBlock{Text: "now do the other one"}})

			c := InputMessages([]llm.Message{mixed}, 0)(full(4096))

			out := decode(Default, c.JSON)
			Expect(out[0]["role"]).To(Equal("user"))
			Expect(partsOf(Default, out[0])).To(HaveLen(2))
		})

		It("renders only the delta and reports where it starts", func() {
			msgs := []llm.Message{
				text(llm.RoleUser, "first"),
				text(llm.RoleAssistant, "second"),
				text(llm.RoleUser, "third"),
			}

			c := InputMessages(msgs, 2)(opts(4096))

			out := decode(Default, c.JSON)
			Expect(out).To(HaveLen(1))
			Expect(partsOf(Default, out[0])[0]["content"]).To(Equal("third"))
			Expect(c.FromIndex).To(Equal(2))
			Expect(c.Truncated).To(BeFalse())

			// The same builder under the full opt-in carries everything and starts at
			// the beginning, which is what makes from_index reconcile with the message
			// count already on the span.
			cf := InputMessages(msgs, 2)(full(4096))
			Expect(decode(Default, cf.JSON)).To(HaveLen(3))
			Expect(cf.FromIndex).To(BeZero())
		})

		// This is the spec the clamp exists for, and it guards a run rather than an
		// attribute. The conversation is replaced wholesale by a context reset and by a
		// session rotation, so an index captured before either is larger than the slice
		// that follows it. Slicing on it panics, the run's panic barrier turns that into
		// a PanicError, and an opt-in observability feature has ended the run.
		It("cannot slice out of range when the conversation was replaced", func() {
			rotated := []llm.Message{text(llm.RoleUser, "fresh session")}

			var c telemetry.Content
			Expect(func() { c = InputMessages(rotated, 40)(opts(4096)) }).ToNot(Panic())

			Expect(decode(Default, c.JSON)).To(BeEmpty())
			Expect(c.FromIndex).To(Equal(1))

			// And the same for a conversation cleared to nothing.
			Expect(func() { InputMessages(nil, 40)(opts(4096)) }).ToNot(Panic())
		})
	})

	Describe("OutputMessages", func() {
		It("carries the finish reason on the reply", func() {
			c := OutputMessages([]llm.ContentBlock{{Text: &llm.TextBlock{Text: "all done"}}}, "end_turn")(opts(4096))

			out := decode(Default, c.JSON)
			Expect(out).To(HaveLen(1))
			Expect(out[0]["role"]).To(Equal("assistant"))
			Expect(out[0]["finish_reason"]).To(Equal("end_turn"))
		})
	})

	// Two payloads must never leave the process, and both are shaped so that an
	// implementation passing them through would look entirely reasonable.
	Describe("what is never exported", func() {
		It("exports reasoning text but never a thinking signature", func() {
			blocks := []llm.ContentBlock{{Thinking: &llm.ThinkingBlock{
				Text:      "the user wants the weather",
				Signature: []byte("SIGNATURE-a1b2c3-must-not-be-exported"),
			}}}

			c := OutputMessages(blocks, "end_turn")(opts(4096))

			Expect(c.JSON).To(ContainSubstring("the user wants the weather"))
			Expect(c.JSON).ToNot(ContainSubstring("SIGNATURE"))
			Expect(c.JSON).ToNot(ContainSubstring("a1b2c3"))

			Expect(partsOf(Default, decode(Default, c.JSON)[0])[0]["type"]).To(Equal(partReasoning))
		})

		It("keeps a provider block's shape and drops its payload", func() {
			// The payload is provider JSON nothing in this process has inspected, and
			// on the Anthropic backend it is where a server-side tool search result
			// lives. The marker stays so a reader sees the turn had a part there rather
			// than reading the absence as a gap in the instrumentation.
			blocks := []llm.ContentBlock{{Provider: &llm.ProviderBlock{
				Kind: "tool_search_tool_result",
				Raw:  json.RawMessage(`{"secret":"CONFIDENTIAL-PAYLOAD"}`),
			}}}

			c := OutputMessages(blocks, "end_turn")(opts(4096))

			Expect(c.JSON).ToNot(ContainSubstring("CONFIDENTIAL-PAYLOAD"))
			Expect(c.JSON).ToNot(ContainSubstring("secret"))

			p := partsOf(Default, decode(Default, c.JSON)[0])[0]
			Expect(p["type"]).To(Equal(partProviderBlock))
			Expect(p["kind"]).To(Equal("tool_search_tool_result"))
			Expect(p["omitted"]).To(BeTrue())
		})
	})

	Describe("Truncation", func() {
		It("stays valid JSON and keeps the newest messages", func() {
			msgs := []llm.Message{
				text(llm.RoleUser, strings.Repeat("a", 4000)),
				text(llm.RoleAssistant, strings.Repeat("b", 4000)),
				text(llm.RoleUser, "the newest thing"),
			}

			c := InputMessages(msgs, 0)(full(1024))

			Expect(c.Truncated).To(BeTrue())
			Expect(c.DroppedMessages).To(Equal(2))
			Expect(c.FromIndex).To(Equal(2))
			Expect(len(c.JSON)).To(BeNumerically("<=", 1024))

			out := decode(Default, c.JSON)
			Expect(out).To(HaveLen(1))
			Expect(partsOf(Default, out[0])[0]["content"]).To(Equal("the newest thing"))
		})

		// A kept message answering tool calls that were dropped references ids that
		// appear nowhere in the trace, so a GenAI view renders tool output attributed to
		// calls that never happened and the first reading of that is "spans were lost".
		It("does not leave a tool result orphaned by the message that requested it", func() {
			msgs := []llm.Message{
				text(llm.RoleUser, strings.Repeat("a", 4000)),
				toolCall("call_1", "do", `{"payload":"`+strings.Repeat("b", 2000)+`"}`),
				toolResults("call_1", strings.Repeat("r", 300)),
				text(llm.RoleAssistant, "finished"),
			}

			// Sized so the budget alone lands the kept suffix exactly on the results
			// message, which is the shape the rule exists for. Without it the document
			// answers call_1 while nothing in the trace ever asks it.
			c := InputMessages(msgs, 0)(full(600))

			Expect(c.DroppedMessages).To(Equal(3))
			expectToolCallsPaired(Default, c.JSON)
		})

		It("keeps a results message whose call survived alongside it", func() {
			// The counterpart, so the rule reads as "do not orphan" rather than "drop
			// every tool result", which would pass the spec above just as well.
			msgs := []llm.Message{
				text(llm.RoleUser, strings.Repeat("a", 4000)),
				toolCall("call_1", "do", `{}`),
				toolResults("call_1", "ok"),
			}

			c := InputMessages(msgs, 0)(full(600))

			out := decode(Default, c.JSON)
			Expect(out).To(HaveLen(2))
			Expect(out[1]["role"]).To(Equal(roleTool))
			expectToolCallsPaired(Default, c.JSON)
		})

		It("marks the value it cut", func() {
			msgs := []llm.Message{text(llm.RoleUser, strings.Repeat("a", 4000))}

			c := InputMessages(msgs, 0)(full(512))

			Expect(c.Truncated).To(BeTrue())
			Expect(c.JSON).To(ContainSubstring("truncated by fisk-ai"))
			Expect(c.JSON).To(ContainSubstring("max_bytes"))
			decode(Default, c.JSON)
		})

		// The budget is spent on the encoded document, not on the Go string, and the
		// gap between the two is not a rounding error. encoding/json escapes each of
		// '<', '>' and '&' to six bytes, so a tool result of angle brackets encodes to
		// six times its length.
		//
		// This spec is not the one that catches a wrong cost table, which is worth
		// saying because it looks like it is: with the HTML case removed the size
		// assertion below still passes, since the final size check falls back to the
		// marker-only message. Right size, no content. The encodedLen drift guard at
		// the end of this file is what fails, and it is the one to keep.
		It("counts the encoded size, not the string length", func() {
			msgs := []llm.Message{text(llm.RoleUser, strings.Repeat("<", 4000))}

			c := InputMessages(msgs, 0)(full(1024))

			Expect(c.Truncated).To(BeTrue())
			Expect(len(c.JSON)).To(BeNumerically("<=", 1024))
			decode(Default, c.JSON)
		})

		// Command stdout is arbitrary bytes and reaches a tool result unfiltered, where
		// the encoder replaces every invalid byte with a three-byte U+FFFD. So this is
		// the second way a byte budget on the Go string under-counts, and the one case
		// where "never split a rune" is not even well defined on the input.
		It("handles invalid UTF-8 without exceeding the budget", func() {
			junk := strings.Repeat("\xff\xfe", 2000)
			msgs := []llm.Message{text(llm.RoleUser, junk)}

			c := InputMessages(msgs, 0)(full(1024))

			Expect(len(c.JSON)).To(BeNumerically("<=", 1024))
			decode(Default, c.JSON)
		})

		It("never splits a rune", func() {
			msgs := []llm.Message{text(llm.RoleUser, strings.Repeat("é世界", 2000))}

			c := InputMessages(msgs, 0)(full(1024))

			Expect(c.Truncated).To(BeTrue())
			Expect(utf8.ValidString(c.JSON)).To(BeTrue())

			content, ok := partsOf(Default, decode(Default, c.JSON)[0])[0]["content"].(string)
			Expect(ok).To(BeTrue())
			Expect(utf8.ValidString(content)).To(BeTrue())
			Expect(content).ToNot(ContainSubstring("�"))
		})

		It("shortens one oversized message rather than dropping the only one there is", func() {
			msgs := []llm.Message{text(llm.RoleUser, strings.Repeat("a", 40000))}

			c := InputMessages(msgs, 0)(full(1024))

			Expect(c.Truncated).To(BeTrue())
			out := decode(Default, c.JSON)
			Expect(out).To(HaveLen(1))
			Expect(len(c.JSON)).To(BeNumerically("<=", 1024))
		})

		// Found by reading a decoded export, not by an assertion. Under a tight cap the
		// budget arithmetic left no room and every part fell back to the marker alone, so
		// the attribute arrived carrying none of the conversation. Asserting only that the
		// document is valid and within the cap cannot see it, because the fallback is both.
		DescribeTable("keeps most of the budget as actual content",
			func(cap int) {
				msgs := []llm.Message{text(llm.RoleUser, strings.Repeat("the original prompt ", 500))}

				c := InputMessages(msgs, 0)(full(cap))

				Expect(c.Truncated).To(BeTrue())
				Expect(len(c.JSON)).To(BeNumerically("<=", cap))
				Expect(c.JSON).To(ContainSubstring("the original prompt"))

				// Most of the room goes to the conversation rather than to structure and
				// the marker, which is the property that failed.
				Expect(len(c.JSON)).To(BeNumerically(">", cap/2),
					"a cap of %d produced only %d bytes: %s", cap, len(c.JSON), c.JSON)
			},
			Entry("at the floor", 256),
			Entry("at a tight cap", 512),
			Entry("at the default", 8192),
		)

		// The same arithmetic, on the shape where the payload is a raw JSON value rather
		// than a string.
		It("keeps content when a tool result is the oversized payload", func() {
			c := ToolResult(`{"output":"` + strings.Repeat("real output ", 500) + `"}`)(opts(512))

			Expect(c.Truncated).To(BeTrue())
			Expect(len(c.JSON)).To(BeNumerically("<=", 512))
			Expect(c.JSON).To(ContainSubstring("real output"))
		})
	})

	Describe("SystemInstructions", func() {
		It("renders each segment as a text part", func() {
			c := SystemInstructions([]string{"you are an agent", "be brief"})(opts(4096))

			out := decode(Default, c.JSON)
			Expect(out).To(HaveLen(2))
			Expect(out[0]["type"]).To(Equal(partText))
			Expect(out[1]["content"]).To(Equal("be brief"))
		})

		// The room is shared rather than spent front to back, so one long segment
		// cannot consume every later segment's. The memory index block is appended last
		// and would be the segment lost.
		It("shares the budget between segments", func() {
			c := SystemInstructions([]string{strings.Repeat("a", 4000), "the last word"})(opts(1024))

			Expect(c.Truncated).To(BeTrue())
			out := decode(Default, c.JSON)
			Expect(out).To(HaveLen(2))
			Expect(out[1]["content"]).To(Equal("the last word"))
		})
	})

	Describe("ToolArguments and ToolResult", func() {
		It("embeds a JSON result as a value rather than a quoted blob", func() {
			c := ToolResult(`{"exit_code":0,"output":"ok"}`)(opts(4096))

			var obj map[string]any
			Expect(json.Unmarshal([]byte(c.JSON), &obj)).To(Succeed())
			Expect(obj).To(HaveKeyWithValue("exit_code", float64(0)))
			Expect(c.Truncated).To(BeFalse())
		})

		// Not everything a tool returns is JSON, and the model saw whatever it was.
		It("renders a non-JSON result as a string rather than dropping it", func() {
			c := ToolResult("total 24\ndrwxr-xr-x  3 rip staff")(opts(4096))

			var s string
			Expect(json.Unmarshal([]byte(c.JSON), &s)).To(Succeed())
			Expect(s).To(ContainSubstring("drwxr-xr-x"))
		})

		// The arguments are the model's own bytes. Embedding them unvalidated fails the
		// marshal and loses the whole document, and there is a truthful fallback.
		It("renders invalid argument JSON as a string rather than losing the document", func() {
			c := ToolArguments(json.RawMessage(`{"broken": `))(opts(4096))

			var s string
			Expect(json.Unmarshal([]byte(c.JSON), &s)).To(Succeed())
			Expect(s).To(ContainSubstring("broken"))
		})

		It("reports nothing for an absent value", func() {
			Expect(ToolArguments(nil)(opts(4096)).JSON).To(BeEmpty())
		})

		It("truncates an oversized result to a string that still parses", func() {
			c := ToolResult(strings.Repeat("x", 40000))(opts(1024))

			Expect(c.Truncated).To(BeTrue())
			Expect(len(c.JSON)).To(BeNumerically("<=", 1024))

			var s string
			Expect(json.Unmarshal([]byte(c.JSON), &s)).To(Succeed())
			Expect(s).To(ContainSubstring("truncated by fisk-ai"))
		})
	})

	// The encoded-length table is hand written, so it is the one piece here that can
	// drift from the encoder it models without anything failing to compile. This walks
	// the cases that expand and compares against what encoding/json actually produces.
	Describe("encodedLen", func() {
		It("agrees with the encoder on every expanding case", func() {
			for _, s := range []string{
				"plain ascii",
				"<html> & \"quotes\"",
				"tab\there\nnewline\rreturn",
				"control\x00\x01\x1f",
				"é世界",
				"  ",
				strings.Repeat("<", 100),
			} {
				b, err := json.Marshal(s)
				Expect(err).ToNot(HaveOccurred())

				// The marshaled form carries the two quotes the body does not.
				Expect(encodedLen(s)).To(Equal(len(b)-2), "mismatch for %q: encoder produced %s", s, b)
			}
		})

		// An invalid byte is the one case the toolchains disagree on: it was a
		// six-character escape and is a three-byte U+FFFD written raw from Go 1.27, so an
		// exact count can only be right for one of them. Only one direction can hurt, and
		// that is what is asserted here: a count below what the encoder writes would let
		// an attribute past the cap it was budgeted against, while one above it truncates
		// earlier than it needed to.
		It("never counts an invalid byte as smaller than the encoder writes it", func() {
			for _, s := range []string{
				"invalid \xff\xfe bytes",
				"\xff",
				strings.Repeat("\xfe", 50),
			} {
				b, err := json.Marshal(s)
				Expect(err).ToNot(HaveOccurred())

				Expect(encodedLen(s)).To(BeNumerically(">=", len(b)-2),
					"undercount for %q: encoder produced %s", s, b)
			}
		})
	})
})

// These come from reading a decoded export rather than from an assertion. Every spec on
// the shape of these documents passed while a text part was being rendered with no text
// in it, because nothing looked at whether the part had anything to say.
var _ = Describe("Parts with nothing to say", func() {
	// The prompt is the configured one followed by optional notes, so an agent that
	// configures none leads the list with an empty string.
	It("skips an empty system prompt segment rather than rendering an empty part", func() {
		c := SystemInstructions([]string{"", "be brief", ""})(opts(4096))

		var parts []map[string]any
		Expect(json.Unmarshal([]byte(c.JSON), &parts)).To(Succeed())

		Expect(parts).To(HaveLen(1))
		Expect(parts[0]["content"]).To(Equal("be brief"))

		for _, p := range parts {
			Expect(p).To(HaveKey("content"), "a text part must carry text: %s", c.JSON)
		}
	})

	// Absent rather than an empty document: an agent that configures no system prompt
	// and enables no notes has nothing to say, and "[]" would say it had instructions
	// that were empty.
	It("produces no document at all when every segment is empty", func() {
		c := SystemInstructions([]string{"", ""})(opts(4096))

		Expect(c.JSON).To(BeEmpty())
		Expect(c.Truncated).To(BeFalse())
	})

	It("skips an empty text block in a turn", func() {
		blocks := []llm.ContentBlock{
			{Text: &llm.TextBlock{Text: ""}},
			{Text: &llm.TextBlock{Text: "the real answer"}},
		}

		c := OutputMessages(blocks, "end_turn")(opts(4096))

		parts := partsOf(Default, decode(Default, c.JSON)[0])
		Expect(parts).To(HaveLen(1))
		Expect(parts[0]["content"]).To(Equal("the real answer"))
	})

	// Redacted reasoning is a signature with no readable text. Dropping the part would
	// say the model did not reason; rendering a bare type would say it reasoned about
	// nothing. The marker says what actually happened, like a provider block.
	It("marks redacted reasoning rather than dropping it or emptying it", func() {
		blocks := []llm.ContentBlock{{Thinking: &llm.ThinkingBlock{Signature: []byte("REDACTED-SIG")}}}

		c := OutputMessages(blocks, "end_turn")(opts(4096))
		Expect(c.JSON).ToNot(ContainSubstring("REDACTED-SIG"))

		parts := partsOf(Default, decode(Default, c.JSON)[0])
		Expect(parts).To(HaveLen(1))
		Expect(parts[0]["type"]).To(Equal(partReasoning))
		Expect(parts[0]["omitted"]).To(BeTrue())
		Expect(parts[0]).ToNot(HaveKey("content"))
	})
})
