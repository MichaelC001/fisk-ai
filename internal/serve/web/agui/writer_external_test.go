//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agui_test

import (
	"encoding/json"
	"errors"
	"net/http"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/serve"
	"github.com/choria-io/fisk-ai/internal/serve/web"
)

// running is a run input asking one turn, which is what a spec drives the writer from
// when the request carries no resume.
const running = `{"threadId":"t1","runId":"r1","messages":[{"id":"m1","role":"user","content":"hi"}]}`

// text is one text fragment of the first content block.
func text(s string) llm.Delta { return llm.Delta{Kind: llm.DeltaText, Text: s} }

// completed is the ending of a turn that ran to an answer.
var completed = web.Ending{Outcome: serve.Outcome{Reason: runstate.ReasonCompleted}}

// textTurn is an assembled assistant turn carrying one text block.
func textTurn(s string) llm.Response {
	return llm.Response{Content: []llm.ContentBlock{{Text: &llm.TextBlock{Text: s}}}}
}

var _ = Describe("The response", func() {
	It("Should send the headers an event stream is served with", func() {
		d := decode(running)
		d.writer.Open()

		Expect(d.rec.Code).To(Equal(http.StatusOK))
		Expect(d.rec.Header().Get("Content-Type")).To(Equal("text/event-stream"))
		Expect(d.rec.Header().Get("Cache-Control")).To(Equal("no-cache"))
		Expect(d.rec.Header().Get("Connection")).To(Equal("keep-alive"))
		Expect(d.rec.Header().Get("X-Accel-Buffering")).To(Equal("no"))
		Expect(d.rec.Flushed).To(BeTrue(), "an event the handler holds arrives when the turn ends rather than as it is written")
	})

	// The SDK stamps every event it builds with the millisecond the value was made,
	// which on a replayed conversation is now rather than when the conversation
	// happened. Nothing carries it.
	It("Should open the run under the ids the request named and carry no timestamp", func() {
		d := decode(running)

		d.writer.Open()
		d.writer.Close(completed)

		Expect(d.body()).To(Equal(frames("t1", "r1",
			`{"type":"RUN_FINISHED","threadId":"t1","runId":"r1","result":{"reason":"completed"},"outcome":{"type":"success"}}`,
		)))
	})

	It("Should stream a text turn as one message started, filled and ended", func() {
		d := decode(running)

		d.writer.Open()
		streamer(d.writer).MessageDelta(text("hello "))
		streamer(d.writer).MessageDelta(text("there"))
		streamer(d.writer).MessageDelta(llm.Delta{Kind: llm.DeltaText, Final: true})
		d.writer.Message(textTurn("hello there"), true)
		d.writer.Close(completed)

		Expect(d.body()).To(Equal(frames("t1", "r1",
			`{"type":"TEXT_MESSAGE_START","messageId":"r1-1","role":"assistant"}`,
			`{"type":"TEXT_MESSAGE_CONTENT","messageId":"r1-1","delta":"hello "}`,
			`{"type":"TEXT_MESSAGE_CONTENT","messageId":"r1-1","delta":"there"}`,
			`{"type":"TEXT_MESSAGE_END","messageId":"r1-1"}`,
			`{"type":"RUN_FINISHED","threadId":"t1","runId":"r1","result":{"reason":"completed"},"outcome":{"type":"success"}}`,
		)))
	})

	// Delta.Index restarts at zero on every model call, so the id a message is started
	// under cannot be the index: the second call's block 0 is a message of its own. The
	// run id is in every one of them because a client keeps the messages of every run
	// of a thread.
	It("Should mint message ids that are distinct across the whole thread", func() {
		d := decode(running)

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

		Expect(d.body()).To(Equal(frames("t1", "r1",
			// The role is the reasoning one rather than the assistant one: the TypeScript
			// core every browser client parses through requires this exact value, where the
			// Go SDK asks only that a role is set.
			`{"type":"REASONING_MESSAGE_START","messageId":"r1-1","role":"reasoning"}`,
			`{"type":"REASONING_MESSAGE_CONTENT","messageId":"r1-1","delta":"let me think"}`,
			`{"type":"TEXT_MESSAGE_START","messageId":"r1-2","role":"assistant"}`,
			`{"type":"TEXT_MESSAGE_CONTENT","messageId":"r1-2","delta":"the answer"}`,
			`{"type":"REASONING_MESSAGE_END","messageId":"r1-1"}`,
			`{"type":"TEXT_MESSAGE_END","messageId":"r1-2"}`,
			`{"type":"TEXT_MESSAGE_START","messageId":"r1-3","role":"assistant"}`,
			`{"type":"TEXT_MESSAGE_CONTENT","messageId":"r1-3","delta":"and now this"}`,
			`{"type":"TEXT_MESSAGE_END","messageId":"r1-3"}`,
			`{"type":"RUN_FINISHED","threadId":"t1","runId":"r1","result":{"reason":"completed"},"outcome":{"type":"success"}}`,
		)))
	})

	// A run against a provider that does not stream reaches the writer through Message
	// alone, and the page reads the same messages either way.
	It("Should write a block that streamed nothing whole", func() {
		d := decode(running)

		d.writer.Open()
		d.writer.Message(textTurn("all at once"), true)
		d.writer.Close(completed)

		Expect(d.body()).To(Equal(frames("t1", "r1",
			`{"type":"TEXT_MESSAGE_START","messageId":"r1-1","role":"assistant"}`,
			`{"type":"TEXT_MESSAGE_CONTENT","messageId":"r1-1","delta":"all at once"}`,
			`{"type":"TEXT_MESSAGE_END","messageId":"r1-1"}`,
			`{"type":"RUN_FINISHED","threadId":"t1","runId":"r1","result":{"reason":"completed"},"outcome":{"type":"success"}}`,
		)))
	})

	// A run that died between the model call and its assembled turn leaves fragments
	// whose message the client would hold in its streaming state for the rest of the
	// thread.
	It("Should end a message the run left open", func() {
		d := decode(running)

		d.writer.Open()
		streamer(d.writer).MessageDelta(text("half a sen"))
		d.writer.Close(web.Ending{Outcome: serve.Outcome{Reason: runstate.ReasonError, Err: errors.New("the provider refused the request")}})

		Expect(d.body()).To(Equal(frames("t1", "r1",
			`{"type":"TEXT_MESSAGE_START","messageId":"r1-1","role":"assistant"}`,
			`{"type":"TEXT_MESSAGE_CONTENT","messageId":"r1-1","delta":"half a sen"}`,
			`{"type":"TEXT_MESSAGE_END","messageId":"r1-1"}`,
			`{"type":"RUN_ERROR","message":"the provider refused the request","runId":"r1"}`,
		)))
	})

	It("Should render a call with its arguments and the result that answered it", func() {
		d := decode(running)

		d.writer.Open()
		d.writer.ToolCall(agent.ToolTrace{ID: "c1", Name: "list", Input: json.RawMessage(`{"limit":2}`)})
		d.writer.ToolResult(agent.ToolResultTrace{CallID: "c1", Output: "two streams"})
		d.writer.Close(completed)

		Expect(d.body()).To(Equal(frames("t1", "r1",
			`{"type":"TOOL_CALL_START","toolCallId":"c1","toolCallName":"list"}`,
			`{"type":"TOOL_CALL_ARGS","toolCallId":"c1","delta":"{\"limit\":2}"}`,
			`{"type":"TOOL_CALL_END","toolCallId":"c1"}`,
			`{"type":"TOOL_CALL_RESULT","messageId":"r1-1","toolCallId":"c1","content":"two streams","role":"tool"}`,
			`{"type":"RUN_FINISHED","threadId":"t1","runId":"r1","result":{"reason":"completed"},"outcome":{"type":"success"}}`,
		)))
	})

	// The protocol requires content on a result event, so a tool that printed nothing
	// is reported as having printed nothing. Leaving the event out would leave the call
	// reading as one that never returned.
	It("Should report a call whose tool printed nothing", func() {
		d := decode(running)

		d.writer.Open()
		d.writer.ToolCall(agent.ToolTrace{ID: "c1", Name: "list"})
		d.writer.ToolResult(agent.ToolResultTrace{CallID: "c1", Output: ""})
		d.writer.Close(completed)

		Expect(d.body()).To(ContainSubstring(`data: {"type":"TOOL_CALL_RESULT","messageId":"r1-1","toolCallId":"c1","content":"the tool produced no output","role":"tool"}`))
		accepted(d.body())
	})

	// A call with no arguments is an empty object rather than an empty string, which is
	// not JSON and which the protocol's own args event refuses.
	It("Should send an empty object for a call that takes no arguments", func() {
		d := decode(running)

		d.writer.Open()
		d.writer.ToolCall(agent.ToolTrace{ID: "c1", Name: "list"})
		d.writer.Close(completed)

		Expect(d.body()).To(ContainSubstring(`data: {"type":"TOOL_CALL_ARGS","toolCallId":"c1","delta":"{}"}`))
	})

	// An advisory is the one thing a run reports that AG-UI has no event for. The kind
	// and its fields travel rather than a sentence about them.
	It("Should send an advisory as a custom event", func() {
		d := decode(running)

		d.writer.Open()
		d.writer.Warn(agent.Warning{Kind: agent.WarnUnknownTool, Name: "wipe"})
		d.writer.Close(completed)

		Expect(d.body()).To(ContainSubstring(`data: {"type":"CUSTOM","name":"fisk.warning","value":{"kind":"unknown_tool","name":"wipe"}}`))
	})

	// A prompt sent while a question is outstanding is not delivered, and the run
	// finishes on the interrupt rather than on an error: the client answers the
	// question and sends the message again.
	It("Should tell the page a message the conversation did not take", func() {
		d := decode(running)

		d.writer.Open()
		d.writer.Ask(web.Question{Kind: web.KindApprove, ToolUseID: "c1", Command: "wipe", Display: "wipe", Tag: "ai:confirm"})
		d.writer.Close(web.Ending{Outcome: serve.Outcome{Reason: runstate.ReasonSuspended}, PromptNotTaken: true})

		Expect(d.body()).To(ContainSubstring(`data: {"type":"CUSTOM","name":"fisk.prompt_not_taken","value":{"message":"your message was not delivered`))
		Expect(d.body()).To(ContainSubstring(`"outcome":{"type":"interrupt"`))
	})

	// A run stopped by its budget reports both a reason and an error, and the code
	// serve.ErrorCode places on the outcome is what a client acts on.
	It("Should carry the code the outcome names on a run error", func() {
		d := decode(running)

		d.writer.Open()
		d.writer.Close(web.Ending{Outcome: serve.Outcome{Reason: runstate.ReasonError, Err: llm.ErrRateLimited}})

		Expect(d.body()).To(ContainSubstring(`data: {"type":"RUN_ERROR","code":"provider_busy","message":"`))
	})
})
