//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package vercel_test

import (
	"encoding/json"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/serve"
	"github.com/choria-io/fisk-ai/internal/serve/web"
)

// folded is a stored conversation as the store hands one back: the records a run
// journals, folded the way a load folds them, so a spec replays a shape a run produced
// rather than one it wrote by hand.
func folded(prompt string, records ...runstate.Record) *runstate.RunState {
	GinkgoHelper()

	all := []runstate.Record{{Seq: 1, Protocol: runstate.MetaProtocol, Meta: &runstate.MetaRecord{
		Version: runstate.Version,
		RunID:   "w-abc",
		Prompt:  prompt,
	}}}

	for i, r := range records {
		r.Seq = uint64(i + 2)
		all = append(all, r)
	}

	rs, err := runstate.Fold(all)
	Expect(err).ToNot(HaveOccurred())

	return rs
}

// assistant is one assistant turn of the blocks it produced.
func assistant(blocks ...llm.ContentBlock) runstate.Record {
	return runstate.Record{Protocol: runstate.AssistantProtocol, Assistant: &runstate.AssistantRecord{
		Message: llm.Message{Role: llm.RoleAssistant, Content: blocks},
	}}
}

// typed is a turn the person typed after the conversation had started.
func typed(text string) runstate.Record {
	return runstate.Record{Protocol: runstate.UserProtocol, User: &runstate.UserRecord{
		Message: llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Text: &llm.TextBlock{Text: text}}}},
	}}
}

// answered is the result of one call.
func answered(id, output string, failed bool) runstate.Record {
	return runstate.Record{Protocol: runstate.ToolResultProtocol, ToolResult: &runstate.ToolResultRecord{
		ToolUseID: id,
		Result:    llm.ToolResultBlock{ToolUseID: id, Content: output, IsError: failed},
	}}
}

func prose(text string) llm.ContentBlock { return llm.ContentBlock{Text: &llm.TextBlock{Text: text}} }
func reason(text string) llm.ContentBlock {
	return llm.ContentBlock{Thinking: &llm.ThinkingBlock{Text: text}}
}

func calls(id, name, input string) llm.ContentBlock {
	return llm.ContentBlock{ToolUse: &llm.ToolUseBlock{ID: id, Name: name, Input: json.RawMessage(input)}}
}

var _ = Describe("A replayed conversation", func() {
	It("Should write the turns it holds, each call followed by what it returned", func() {
		o := open()

		o.writer.Open()
		o.writer.Replay(folded("list the streams",
			assistant(prose("let me look"), calls("c1", "list", `{"limit":2}`)),
			answered("c1", "two streams", false),
			assistant(prose("there are two")),
			typed("and the consumers?"),
			assistant(prose("two of those too")),
		))
		o.writer.Close(completed)

		Expect(o.body()).To(Equal(sse(
			`{"type":"start","messageId":"MID"}`,
			`{"type":"data-user-message","id":"1","data":{"text":"list the streams"}}`,
			`{"type":"text-start","id":"2"}`,
			`{"type":"text-delta","id":"2","delta":"let me look"}`,
			`{"type":"text-end","id":"2"}`,
			`{"type":"tool-input-available","toolCallId":"c1","toolName":"list","input":{"limit":2},"dynamic":true}`,
			`{"type":"tool-output-available","toolCallId":"c1","output":"two streams","dynamic":true}`,
			`{"type":"text-start","id":"3"}`,
			`{"type":"text-delta","id":"3","delta":"there are two"}`,
			`{"type":"text-end","id":"3"}`,
			`{"type":"data-user-message","id":"4","data":{"text":"and the consumers?"}}`,
			`{"type":"text-start","id":"5"}`,
			`{"type":"text-delta","id":"5","delta":"two of those too"}`,
			`{"type":"text-end","id":"5"}`,
			`{"type":"finish","finishReason":"stop"}`,
			`[DONE]`,
		)))
	})

	// A stored turn is whole, so there is nothing to stream and no fragment to reconcile:
	// each block is its start, its text and its end together.
	It("Should write each text and reasoning block whole rather than streamed", func() {
		o := open()

		o.writer.Open()
		o.writer.Replay(folded("think about it",
			assistant(reason("weighing it up"), prose("here is the answer")),
		))
		o.writer.Close(completed)

		Expect(o.body()).To(Equal(sse(
			`{"type":"start","messageId":"MID"}`,
			`{"type":"data-user-message","id":"1","data":{"text":"think about it"}}`,
			`{"type":"reasoning-start","id":"2"}`,
			`{"type":"reasoning-delta","id":"2","delta":"weighing it up"}`,
			`{"type":"reasoning-end","id":"2"}`,
			`{"type":"text-start","id":"3"}`,
			`{"type":"text-delta","id":"3","delta":"here is the answer"}`,
			`{"type":"text-end","id":"3"}`,
			`{"type":"finish","finishReason":"stop"}`,
			`[DONE]`,
		)))
	})

	// A live turn sends two parts for a gated call: the one the gate synthesized for the
	// approval to mutate, and the traced call on the turn that resumed. The conversation
	// holds the call itself, so a replay draws it once.
	It("Should draw one tool part for a call that was gated and approved", func() {
		o := open()

		o.writer.Open()
		o.writer.Replay(folded("wipe it",
			assistant(calls("c1", "wipe", `{"stream":"ORDERS"}`)),
			answered("c1", "removed", false),
			assistant(prose("gone")),
		))
		o.writer.Close(completed)

		Expect(strings.Count(o.body(), `"type":"tool-input-available"`)).To(Equal(1))
		Expect(o.body()).To(ContainSubstring(`data: {"type":"tool-input-available","toolCallId":"c1","toolName":"wipe","input":{"stream":"ORDERS"},"dynamic":true}`))
		Expect(o.body()).ToNot(ContainSubstring(`"type":"tool-approval-request"`), "nobody is being asked about a call that ran")
	})

	It("Should render a call that failed on the part the call opened", func() {
		o := open()

		o.writer.Open()
		o.writer.Replay(folded("remove it",
			assistant(calls("c1", "stream_rm", `{}`)),
			answered("c1", "no such stream", true),
		))
		o.writer.Close(completed)

		Expect(o.body()).To(ContainSubstring(`data: {"type":"tool-output-error","toolCallId":"c1","errorText":"no such stream","dynamic":true}`))
	})

	// A live turn sends one part for the call it is asking about, the gate's, since the
	// call is not traced until it is approved and dispatched. The stored call's own part
	// is dropped so the replay sends that one part too, and the approval mutates it.
	//
	// The tool is named stream_rm and runs "stream rm", so a part carrying the model's
	// name for it rather than the command path is caught here.
	It("Should end on the card the conversation stopped at", func() {
		o := open()

		o.writer.Open()
		o.writer.Replay(folded("wipe it",
			assistant(calls("c1", "stream_rm", `{"stream":"ORDERS"}`)),
		))
		o.writer.Ask(web.Question{
			Kind:      web.KindApprove,
			ToolUseID: "c1",
			Command:   "stream rm",
			Display:   "stream rm --stream=ORDERS",
			Tag:       "ai:confirm",
		})
		o.writer.Close(web.Ending{Outcome: serve.Outcome{Reason: runstate.ReasonSuspended}})

		Expect(o.body()).To(Equal(sse(
			`{"type":"start","messageId":"MID"}`,
			`{"type":"data-user-message","id":"1","data":{"text":"wipe it"}}`,
			`{"type":"tool-input-available","toolCallId":"c1","toolName":"stream rm","input":{"command":"stream rm --stream=ORDERS","tag":"ai:confirm"},"dynamic":true}`,
			`{"type":"tool-approval-request","approvalId":"approval-c1","toolCallId":"c1","reason":"stream rm --stream=ORDERS"}`,
			`{"type":"finish","finishReason":"tool-calls"}`,
			`[DONE]`,
		)))
	})

	// The three human-in-the-loop questions are put by a tool the run dispatched, whose
	// part a live turn sends, so the replay sends the stored call's part beside the
	// question.
	It("Should keep the call's own part for a human-in-the-loop question", func() {
		o := open()

		o.writer.Open()
		o.writer.Replay(folded("ask me",
			assistant(calls("c1", "ask_human_select", `{"question":"which one?"}`)),
		))
		o.writer.Ask(web.Question{Kind: web.KindSelect, ToolUseID: "c1", Text: "which one?", Options: []string{"ORDERS"}})
		o.writer.Close(web.Ending{Outcome: serve.Outcome{Reason: runstate.ReasonSuspended}})

		Expect(o.body()).To(Equal(sse(
			`{"type":"start","messageId":"MID"}`,
			`{"type":"data-user-message","id":"1","data":{"text":"ask me"}}`,
			`{"type":"tool-input-available","toolCallId":"c1","toolName":"ask_human_select","input":{"question":"which one?"},"dynamic":true}`,
			`{"type":"data-question","id":"c1","data":{"toolUseId":"c1","kind":"select","question":"which one?","options":["ORDERS"]}}`,
			`{"type":"finish","finishReason":"tool-calls"}`,
			`[DONE]`,
		)))
	})

	It("Should write the turn a run left unfinished last", func() {
		o := open()

		o.writer.Open()
		o.writer.Replay(folded("do both",
			assistant(prose("first this"), calls("c1", "list", `{}`)),
			answered("c1", "two streams", false),
			assistant(calls("c2", "wipe", `{}`)),
		))
		o.writer.Close(web.Ending{Outcome: serve.Outcome{Reason: runstate.ReasonSuspended}})

		Expect(o.body()).To(ContainSubstring(`data: {"type":"tool-input-available","toolCallId":"c2","toolName":"wipe","input":{},"dynamic":true}`))
		Expect(o.body()).ToNot(ContainSubstring(`"toolCallId":"c2","output"`), "the call the run stopped at answered nothing")
	})

	It("Should write nothing for a conversation it was handed none of", func() {
		o := open()

		o.writer.Open()
		o.writer.Replay(nil)
		o.writer.Close(completed)

		Expect(o.body()).To(Equal(sse(
			`{"type":"start","messageId":"MID"}`,
			`{"type":"finish","finishReason":"stop"}`,
			`[DONE]`,
		)))
	})
})
