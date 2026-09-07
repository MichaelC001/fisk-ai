//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package vercel_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/serve"
	"github.com/choria-io/fisk-ai/internal/serve/web"
)

// sse is the body a run of parts writes: one data line each, and the terminator when
// the turn ended.
func sse(parts ...string) string {
	var out strings.Builder

	for _, p := range parts {
		fmt.Fprintf(&out, "data: %s\n\n", p)
	}

	return out.String()
}

// prompting is a request body asking one turn, which is what a spec drives the writer
// from when the request carries no answer.
const prompting = `{"id":"chat-1","messages":[{"role":"user","parts":[{"type":"text","text":"hi"}]}]}`

// text is one text fragment of the first content block.
func text(s string) llm.Delta { return llm.Delta{Kind: llm.DeltaText, Text: s} }

// completed is the ending of a turn that ran to an answer.
var completed = web.Ending{Outcome: serve.Outcome{Reason: runstate.ReasonCompleted}}

// textTurn is an assembled assistant turn carrying one text block.
func textTurn(s string) llm.Response {
	return llm.Response{Content: []llm.ContentBlock{{Text: &llm.TextBlock{Text: s}}}}
}

var _ = Describe("The response", func() {
	It("Should send the headers the AI SDK's own server sets", func() {
		d := decode(prompting)
		d.writer.Open()

		Expect(d.rec.Code).To(Equal(http.StatusOK))
		Expect(d.rec.Header().Get("Content-Type")).To(Equal("text/event-stream"))
		Expect(d.rec.Header().Get("Cache-Control")).To(Equal("no-cache"))
		Expect(d.rec.Header().Get("Connection")).To(Equal("keep-alive"))
		Expect(d.rec.Header().Get("X-Vercel-Ai-Ui-Message-Stream")).To(Equal("v1"))
		Expect(d.rec.Header().Get("X-Accel-Buffering")).To(Equal("no"))
		Expect(d.rec.Flushed).To(BeTrue(), "a part the handler holds arrives when the turn ends rather than as it is written")
	})

	It("Should stream a text turn as one part opened, filled and ended", func() {
		d := decode(prompting)

		d.writer.Open()
		streamer(d.writer).MessageDelta(text("hello "))
		streamer(d.writer).MessageDelta(text("there"))
		streamer(d.writer).MessageDelta(llm.Delta{Kind: llm.DeltaText, Final: true})
		d.writer.Message(textTurn("hello there"), true)
		d.writer.Close(completed)

		Expect(d.body()).To(Equal(sse(
			`{"type":"start","messageId":"MID"}`,
			`{"type":"text-start","id":"1"}`,
			`{"type":"text-delta","id":"1","delta":"hello "}`,
			`{"type":"text-delta","id":"1","delta":"there"}`,
			`{"type":"text-end","id":"1"}`,
			`{"type":"finish","finishReason":"stop"}`,
			`[DONE]`,
		)))
	})

	// Delta.Index restarts at zero on every model call, so the id a part is opened
	// under cannot be the index: the second call's block 0 is a part of its own.
	It("Should mint part ids that are unique across the whole message", func() {
		d := decode(prompting)

		d.writer.Open()
		streamer(d.writer).MessageDelta(llm.Delta{Kind: llm.DeltaThinking, Index: 0, Text: "let me think"})
		streamer(d.writer).MessageDelta(llm.Delta{Kind: llm.DeltaText, Index: 1, Text: "the answer"})
		d.writer.Message(llm.Response{Content: []llm.ContentBlock{
			{Thinking: &llm.ThinkingBlock{Text: "let me think"}},
			{Text: &llm.TextBlock{Text: "the answer"}},
		}}, false)
		streamer(d.writer).MessageDelta(text("and now this"))
		d.writer.Message(textTurn("and now this"), true)
		d.writer.Close(completed)

		Expect(d.body()).To(Equal(sse(
			`{"type":"start","messageId":"MID"}`,
			`{"type":"reasoning-start","id":"1"}`,
			`{"type":"reasoning-delta","id":"1","delta":"let me think"}`,
			`{"type":"text-start","id":"2"}`,
			`{"type":"text-delta","id":"2","delta":"the answer"}`,
			`{"type":"reasoning-end","id":"1"}`,
			`{"type":"text-end","id":"2"}`,
			`{"type":"text-start","id":"3"}`,
			`{"type":"text-delta","id":"3","delta":"and now this"}`,
			`{"type":"text-end","id":"3"}`,
			`{"type":"finish","finishReason":"stop"}`,
			`[DONE]`,
		)))
	})

	// A run against a provider that does not stream reaches the writer through Message
	// alone, and the page reads the same parts either way.
	It("Should write a block that streamed nothing whole", func() {
		d := decode(prompting)

		d.writer.Open()
		d.writer.Message(textTurn("all at once"), true)
		d.writer.Close(completed)

		Expect(d.body()).To(Equal(sse(
			`{"type":"start","messageId":"MID"}`,
			`{"type":"text-start","id":"1"}`,
			`{"type":"text-delta","id":"1","delta":"all at once"}`,
			`{"type":"text-end","id":"1"}`,
			`{"type":"finish","finishReason":"stop"}`,
			`[DONE]`,
		)))
	})

	// A run that dies between the model call and its assembled turn leaves fragments
	// whose part the client would otherwise hold in its streaming state for the rest of
	// the message.
	It("Should end a part the run left open", func() {
		d := decode(prompting)

		d.writer.Open()
		streamer(d.writer).MessageDelta(text("half a sen"))
		d.writer.Close(web.Ending{Outcome: serve.Outcome{Reason: runstate.ReasonError, Err: fmt.Errorf("the model call timed out")}})

		Expect(d.body()).To(Equal(sse(
			`{"type":"start","messageId":"MID"}`,
			`{"type":"text-start","id":"1"}`,
			`{"type":"text-delta","id":"1","delta":"half a sen"}`,
			`{"type":"text-end","id":"1"}`,
			`{"type":"error","errorText":"the model call timed out"}`,
			`{"type":"finish","finishReason":"error"}`,
			`[DONE]`,
		)))
	})

	It("Should render a tool call and what it returned", func() {
		d := decode(prompting)

		d.writer.Open()
		d.writer.ToolCall(agent.ToolTrace{ID: "c1", Name: "stream_ls", Input: json.RawMessage(`{"limit":2}`)})
		d.writer.ToolResult(agent.ToolResultTrace{CallID: "c1", Output: "two streams"})
		d.writer.ToolCall(agent.ToolTrace{ID: "c2", Name: "stream_rm"})
		d.writer.ToolResult(agent.ToolResultTrace{CallID: "c2", Output: "no such stream", IsError: true})
		d.writer.Close(completed)

		Expect(d.body()).To(Equal(sse(
			`{"type":"start","messageId":"MID"}`,
			`{"type":"tool-input-available","toolCallId":"c1","toolName":"stream_ls","input":{"limit":2},"dynamic":true}`,
			`{"type":"tool-output-available","toolCallId":"c1","output":"two streams","dynamic":true}`,
			`{"type":"tool-input-available","toolCallId":"c2","toolName":"stream_rm","input":{},"dynamic":true}`,
			`{"type":"tool-output-error","toolCallId":"c2","errorText":"no such stream","dynamic":true}`,
			`{"type":"finish","finishReason":"stop"}`,
			`[DONE]`,
		)))
	})

	// The gate asks before the call is traced, so the tool part the approval mutates
	// does not exist and the client would drop the approval in silence.
	It("Should send the tool part an approval mutates before the approval", func() {
		d := decode(prompting)

		d.writer.Open()
		d.writer.Ask(web.Question{
			Kind:      web.KindApprove,
			ToolUseID: "c1",
			Command:   "stream rm",
			Display:   "stream rm ORDERS",
			Tag:       "ai:confirm",
		})
		d.writer.Close(web.Ending{Outcome: serve.Outcome{Reason: runstate.ReasonSuspended, Err: fmt.Errorf("the operator did not answer the prompt")}})

		Expect(d.body()).To(Equal(sse(
			`{"type":"start","messageId":"MID"}`,
			`{"type":"tool-input-available","toolCallId":"c1","toolName":"stream rm","input":{"command":"stream rm ORDERS","tag":"ai:confirm"},"dynamic":true}`,
			`{"type":"tool-approval-request","approvalId":"approval-c1","toolCallId":"c1","reason":"stream rm ORDERS"}`,
			`{"type":"finish","finishReason":"tool-calls"}`,
			`[DONE]`,
		)), "the suspend the run reports is the question said a second time, so it is not sent as an error")
	})

	It("Should send each human-in-the-loop question as a data part", func() {
		confirm := decode(prompting)
		confirm.writer.Open()
		confirm.writer.Ask(web.Question{Kind: web.KindConfirm, ToolUseID: "c1", Text: "shall I?"})
		confirm.writer.Close(web.Ending{Outcome: serve.Outcome{Reason: runstate.ReasonSuspended}})

		Expect(confirm.body()).To(Equal(sse(
			`{"type":"start","messageId":"MID"}`,
			`{"type":"data-question","id":"c1","data":{"toolUseId":"c1","kind":"confirm","question":"shall I?"}}`,
			`{"type":"finish","finishReason":"tool-calls"}`,
			`[DONE]`,
		)))

		selection := decode(prompting)
		selection.writer.Open()
		selection.writer.Ask(web.Question{Kind: web.KindSelect, ToolUseID: "c2", Text: "which one?", Options: []string{"ORDERS", "EVENTS"}})
		selection.writer.Close(web.Ending{Outcome: serve.Outcome{Reason: runstate.ReasonSuspended}})

		Expect(selection.body()).To(ContainSubstring(
			`data: {"type":"data-question","id":"c2","data":{"toolUseId":"c2","kind":"select","question":"which one?","options":["ORDERS","EVENTS"]}}`,
		))

		input := decode(prompting)
		input.writer.Open()
		input.writer.Ask(web.Question{Kind: web.KindInput, ToolUseID: "c3", Text: "which subject?", Default: "orders.new"})
		input.writer.Close(web.Ending{Outcome: serve.Outcome{Reason: runstate.ReasonSuspended}})

		Expect(input.body()).To(ContainSubstring(
			`data: {"type":"data-question","id":"c3","data":{"toolUseId":"c3","kind":"input","question":"which subject?","default":"orders.new"}}`,
		))
	})

	// The turn that asked already drew the card for this call, and the resume traces the
	// same call again.
	It("Should leave out the traced call the answering request named", func() {
		d := decode(`{
			"id": "chat-1",
			"messages": [{"role": "assistant", "parts": [
				{"type": "dynamic-tool", "toolCallId": "c1", "state": "approval-responded", "approval": {"id": "approval-c1", "approved": true}}
			]}]
		}`)

		d.writer.Open()
		d.writer.ToolCall(agent.ToolTrace{ID: "c1", Name: "stream_rm", Input: json.RawMessage(`{"stream":"ORDERS"}`)})
		d.writer.ToolResult(agent.ToolResultTrace{CallID: "c1", Output: "removed"})
		d.writer.ToolCall(agent.ToolTrace{ID: "c2", Name: "stream_ls"})
		d.writer.Close(completed)

		Expect(d.body()).To(Equal(sse(
			`{"type":"start","messageId":"MID"}`,
			`{"type":"tool-output-available","toolCallId":"c1","output":"removed","dynamic":true}`,
			`{"type":"tool-input-available","toolCallId":"c2","toolName":"stream_ls","input":{},"dynamic":true}`,
			`{"type":"finish","finishReason":"stop"}`,
			`[DONE]`,
		)), "the suppression is spent on the call the answer named and covers nothing else")
	})

	It("Should tell the page its message did not enter the conversation", func() {
		d := decode(prompting)

		d.writer.Open()
		d.writer.Ask(web.Question{Kind: web.KindApprove, ToolUseID: "c1", Command: "wipe", Display: "wipe", Tag: "ai:confirm"})
		d.writer.Close(web.Ending{
			Outcome:        serve.Outcome{Reason: runstate.ReasonSuspended},
			PromptNotTaken: true,
		})

		Expect(d.body()).To(ContainSubstring(`data: {"type":"error","errorText":"your message was not delivered: this conversation is waiting on a question, so answer that first and send the message again"}`))
		Expect(d.body()).To(ContainSubstring(`data: {"type":"tool-approval-request"`), "the question is put again beside the notice")
	})

	It("Should end a turn that produced no event as a complete response", func() {
		d := decode(prompting)

		d.writer.Close(web.Ending{Outcome: serve.Outcome{Reason: runstate.ReasonSuspended}})

		Expect(d.rec.Code).To(Equal(http.StatusOK))
		Expect(d.body()).To(Equal(sse(
			`{"type":"start","messageId":"MID"}`,
			`{"type":"finish","finishReason":"other"}`,
			`[DONE]`,
		)))
	})

	It("Should report a run stopped by its budget as a turn that ran out of room", func() {
		d := decode(prompting)

		d.writer.Close(web.Ending{Outcome: serve.Outcome{Reason: runstate.ReasonBudget, Err: fmt.Errorf("the token budget is spent")}})

		Expect(d.body()).To(ContainSubstring(`data: {"type":"error","errorText":"the token budget is spent"}`))
		Expect(d.body()).To(ContainSubstring(`data: {"type":"finish","finishReason":"length"}`))
	})

	// Replay belongs to the item that opens a stored session, and the interface carries
	// it because a Format implements it.
	It("Should render nothing for a replay", func() {
		d := decode(prompting)

		d.writer.Open()
		d.writer.Replay(&runstate.RunState{})
		d.writer.Close(completed)

		Expect(d.body()).To(Equal(sse(
			`{"type":"start","messageId":"MID"}`,
			`{"type":"finish","finishReason":"stop"}`,
			`[DONE]`,
		)))
	})
})

// streamer is the writer's streaming half, which the runner asserts at run time.
func streamer(w web.TurnWriter) agent.MessageStreamer {
	GinkgoHelper()

	s, ok := w.(agent.MessageStreamer)
	Expect(ok).To(BeTrue(), "a format that streams implements agent.MessageStreamer")

	return s
}
