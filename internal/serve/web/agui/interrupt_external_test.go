//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agui_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/serve"
	"github.com/choria-io/fisk-ai/internal/serve/web"
)

// suspended is the ending of a turn a question stopped, which is what the run reports
// when the prompter aborts the gated call.
var suspended = web.Ending{Outcome: serve.Outcome{Reason: runstate.ReasonSuspended}}

// asked drives the writer to the end of a turn that stopped on one question, and
// returns the body the page read.
func asked(q web.Question) string {
	GinkgoHelper()

	d := decode(running)

	d.writer.Open()
	d.writer.Ask(q)
	d.writer.Close(suspended)

	return d.body()
}

var _ = Describe("A question the turn ended on", func() {
	// The gate runs before the run traces the call, so nothing has been sent about the
	// command. The interrupt carries the line the operator approves, the command path
	// and the tag that gated it, and no tool call is synthesized to hang it off.
	It("Should end the run on the interrupt of a gated command", func() {
		Expect(asked(web.Question{
			Kind:      web.KindApprove,
			ToolUseID: "c1",
			Command:   "stream rm",
			Display:   "stream rm ORDERS",
			Tag:       "ai:confirm",
		})).To(Equal(frames("t1", "r1",
			`{"type":"RUN_FINISHED","threadId":"t1","runId":"r1","outcome":{"type":"interrupt","interrupts":[`+
				`{"id":"approve:c1","reason":"tool_call","message":"stream rm ORDERS","toolCallId":"c1",`+
				`"responseSchema":{"properties":{"approval":{"enum":["no","once","always"],"oneOf":[`+
				`{"const":"no","title":"Do not run it"},`+
				`{"const":"once","title":"Run it this time"},`+
				`{"const":"always","title":"Run it and stop asking about this tool"}`+
				`],"type":"string"}},"required":["approval"],"type":"object"},`+
				`"metadata":{"command":"stream rm","kind":"approve","tag":"ai:confirm"}}]}}`,
		)))
	})

	It("Should end the run on the interrupt of a yes or no question", func() {
		Expect(asked(web.Question{Kind: web.KindConfirm, ToolUseID: "c1", Text: "shall I?"})).To(Equal(frames("t1", "r1",
			`{"type":"RUN_FINISHED","threadId":"t1","runId":"r1","outcome":{"type":"interrupt","interrupts":[`+
				`{"id":"confirm:c1","reason":"confirmation","message":"shall I?","toolCallId":"c1",`+
				`"responseSchema":{"properties":{"confirmed":{"type":"boolean"}},"required":["confirmed"],"type":"object"},`+
				`"metadata":{"kind":"confirm"}}]}}`,
		)))
	})

	// The answer is the option's position, since a run resumes with the answer alone
	// and the list it came from is not sent back with it. The words are the title of
	// each position in the schema, which is what a generic control renders, and the
	// metadata carries the plain list for a client that draws its own.
	It("Should end the run on the interrupt of a choice, titled by its options", func() {
		Expect(asked(web.Question{Kind: web.KindSelect, ToolUseID: "c1", Text: "which one?", Options: []string{"ORDERS", "EVENTS"}})).To(Equal(frames("t1", "r1",
			`{"type":"RUN_FINISHED","threadId":"t1","runId":"r1","outcome":{"type":"interrupt","interrupts":[`+
				`{"id":"select:c1","reason":"input_required","message":"which one?","toolCallId":"c1",`+
				`"responseSchema":{"properties":{"index":{"enum":[0,1],"oneOf":[`+
				`{"const":0,"title":"ORDERS"},{"const":1,"title":"EVENTS"}`+
				`],"type":"integer"}},"required":["index"],"type":"object"},`+
				`"metadata":{"kind":"select","options":["ORDERS","EVENTS"]}}]}}`,
		)))
	})

	It("Should end the run on the interrupt of a question asking for a value", func() {
		Expect(asked(web.Question{Kind: web.KindInput, ToolUseID: "c1", Text: "which subject?", Default: "orders"})).To(Equal(frames("t1", "r1",
			`{"type":"RUN_FINISHED","threadId":"t1","runId":"r1","outcome":{"type":"interrupt","interrupts":[`+
				`{"id":"input:c1","reason":"input_required","message":"which subject?","toolCallId":"c1",`+
				`"responseSchema":{"properties":{"value":{"default":"orders","type":"string"}},"required":["value"],"type":"object"},`+
				`"metadata":{"kind":"input"}}]}}`,
		)))
	})
})
