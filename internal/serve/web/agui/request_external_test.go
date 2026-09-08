//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agui_test

import (
	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/types"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/serve/web"
	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// resuming is a run input answering the interrupt of call c1 with the payload given.
func resuming(id string, payload string) string {
	return `{"threadId":"t1","runId":"r2","messages":[{"id":"m1","role":"user","content":"wipe it"}],` +
		`"resume":[{"interruptId":"` + id + `","status":"resolved","payload":` + payload + `}]}`
}

var _ = Describe("A decoded request", func() {
	It("Should ask for the newest user message and nothing else in the thread", func() {
		d := decode(`{"threadId":"t1","runId":"r1","messages":[
			{"id":"m1","role":"user","content":"list the streams"},
			{"id":"m2","role":"assistant","content":"there are two"},
			{"id":"m3","role":"user","content":"  and the consumers?  "}
		]}`)

		Expect(d.turn).To(Equal(web.Turn{ThreadID: "t1", Prompt: "and the consumers?"}))
	})

	// A browser sending an image alongside its words sends the list form, and this
	// agent takes a prompt in words.
	It("Should read the text fragments of a message that carries a list", func() {
		d := decode(`{"threadId":"t1","runId":"r1","messages":[{"id":"m1","role":"user","content":[
			{"type":"text","text":"look at this"},
			{"type":"image","source":{"type":"url","value":"https://example.net/a.png"}},
			{"type":"text","text":"and this"}
		]}]}`)

		Expect(d.turn.Prompt).To(Equal("look at this\nand this"))
	})

	// The run is journaled here, so the thread a client sends is what it wants rendered
	// rather than the conversation this worker answers from.
	It("Should take a resume as the whole of what the request asks", func() {
		d := decode(resuming("approve:c1", `{"approval":"once"}`))

		Expect(d.turn).To(Equal(web.Turn{
			ThreadID: "t1",
			Answer:   &web.Answer{ToolUseID: "c1", Kind: web.KindApprove, Approval: toolkit.ConfirmOnce},
		}))
	})

	DescribeTable("An answered interrupt",
		func(id string, payload string, expected *web.Answer) {
			d := decode(resuming(id, payload))

			Expect(d.turn.Answer).To(Equal(expected))
		},
		Entry("declining a command", "approve:c1", `{"approval":"no"}`,
			&web.Answer{ToolUseID: "c1", Kind: web.KindApprove, Approval: toolkit.ConfirmNo}),
		Entry("allowing a command for the conversation", "approve:c1", `{"approval":"always"}`,
			&web.Answer{ToolUseID: "c1", Kind: web.KindApprove, Approval: toolkit.ConfirmAlways}),
		Entry("a yes", "confirm:c1", `{"confirmed":true}`,
			&web.Answer{ToolUseID: "c1", Kind: web.KindConfirm, Confirmed: true}),
		Entry("a no", "confirm:c1", `{"confirmed":false}`,
			&web.Answer{ToolUseID: "c1", Kind: web.KindConfirm, Confirmed: false}),
		Entry("one of a list", "select:c1", `{"index":1}`,
			&web.Answer{ToolUseID: "c1", Kind: web.KindSelect, Index: 1}),
		Entry("a value", "input:c1", `{"value":"orders.new"}`,
			&web.Answer{ToolUseID: "c1", Kind: web.KindInput, Value: "orders.new"}),
		Entry("an empty value, which is an answer", "input:c1", `{"value":""}`,
			&web.Answer{ToolUseID: "c1", Kind: web.KindInput, Value: ""}),
	)

	// A client that renders its own approval control sends the boolean AG-UI
	// recommends. It can allow a command once or decline it, and cannot ask this agent
	// to stop asking.
	DescribeTable("An approval a client answered with the recommended boolean",
		func(payload string, expected toolkit.ConfirmChoice) {
			d := decode(resuming("approve:c1", payload))

			Expect(d.turn.Answer.Approval).To(Equal(expected))
		},
		Entry("allowed", `{"approved":true}`, toolkit.ConfirmOnce),
		Entry("declined", `{"approved":false}`, toolkit.ConfirmNo),
	)

	// A tool use id is minted by the model and a colon in one would otherwise split the
	// kind off in the wrong place.
	It("Should read back an interrupt id whose call carries a colon", func() {
		d := decode(resuming("approve:toolu:01ab", `{"approval":"once"}`))

		Expect(d.turn.Answer.ToolUseID).To(Equal("toolu:01ab"))
	})

	// Declining is an answer and travels in the payload. Canceling says the person
	// decided nothing, so the conversation resumes and the question is put again rather
	// than the client being refused with the interrupt still standing.
	It("Should resume on an entry the client canceled with an answer nothing can spend", func() {
		d := decode(`{"threadId":"t1","runId":"r2","messages":[],"resume":[{"interruptId":"approve:c1","status":"` +
			string(types.ResumeStatusCancelled) + `"}]}`)

		Expect(d.turn.Prompt).To(BeEmpty())
		Expect(d.turn.Answer).To(Equal(&web.Answer{ToolUseID: "approve:c1", Kind: web.KindApprove}),
			"the answer names the interrupt rather than the call, and the prompter spends one only on the call it names")
	})

	It("Should refuse a resume naming an interrupt this agent could not have raised", func() {
		_, err := decodeErr(`{"threadId":"t1","runId":"r2","messages":[],"resume":[{"interruptId":"lg-4711","status":"resolved","payload":{}}]}`)

		Expect(err).To(MatchError(ContainSubstring("no entry of the resume array names an interrupt this agent raised")))
	})

	DescribeTable("A payload the interrupt did not ask for",
		func(id string, payload string, message string) {
			_, err := decodeErr(resuming(id, payload))

			Expect(err).To(MatchError(ContainSubstring(message)))
		},
		Entry("an approval that is not one of the three", "approve:c1", `{"approval":"maybe"}`,
			`an approval is answered with no, once or always, and this one says "maybe"`),
		Entry("an approval with neither field", "approve:c1", `{}`,
			"an approval is answered with an approval field of no, once or always, or an approved boolean"),
		Entry("a yes or no with no answer", "confirm:c1", `{}`,
			"a yes or no question is answered with a confirmed field"),
		Entry("a choice with no answer", "select:c1", `{}`,
			"a choice is answered with an index field"),
		Entry("a choice outside the list", "select:c1", `{"index":-1}`,
			"the answer chooses option -1"),
		Entry("a value with no answer", "input:c1", `{}`,
			"a question asking for a value is answered with a value field"),
	)

	It("Should refuse a body that is not a run input", func() {
		_, err := decodeErr(`not json`)

		Expect(err).To(MatchError(ContainSubstring("the request is not a run this agent can answer")))
	})

	// The channel refuses a turn naming no thread, and the format decodes what the
	// request said rather than deciding for it.
	It("Should carry an empty thread through for the channel to refuse", func() {
		d := decode(`{"runId":"r1","messages":[{"id":"m1","role":"user","content":"hi"}]}`)

		Expect(d.turn.ThreadID).To(BeEmpty())
	})

	// The run frame every event sits in needs one, and a client that names none still
	// gets a response whose message ids are distinct from the run before it.
	It("Should mint a run id for a request that names none", func() {
		d := decode(`{"threadId":"t1","messages":[{"id":"m1","role":"user","content":"hi"}]}`)

		d.writer.Open()

		Expect(d.body()).To(ContainSubstring(`"type":"RUN_STARTED","threadId":"t1","runId":"`))
		Expect(d.body()).ToNot(ContainSubstring(`"runId":""`))
	})

	// The SDK reads both spellings, which is what a client written against a Python
	// server sends.
	It("Should read a body written in snake case", func() {
		d := decode(`{"thread_id":"t1","run_id":"r2","messages":[],"resume":[{"interrupt_id":"approve:c1","status":"resolved","payload":{"approval":"once"}}]}`)

		Expect(d.turn.ThreadID).To(Equal("t1"))
		Expect(d.turn.Answer).To(Equal(&web.Answer{ToolUseID: "c1", Kind: web.KindApprove, Approval: toolkit.ConfirmOnce}))
	})
})
