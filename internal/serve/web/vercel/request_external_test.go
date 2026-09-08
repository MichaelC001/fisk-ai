//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package vercel_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/serve/web"
	"github.com/choria-io/fisk-ai/internal/serve/web/vercel"
	"github.com/choria-io/fisk-ai/internal/toolkit"
)

var _ = Describe("Decode", func() {
	It("Should read the chat id and the newest user message and nothing else", func() {
		out := decode(`{
			"id": "chat-1",
			"messages": [
				{"role": "user", "parts": [{"type": "text", "text": "the first turn"}]},
				{"role": "assistant", "parts": [{"type": "text", "text": "an answer"}]},
				{"role": "user", "parts": [{"type": "text", "text": " the newest turn "}, {"type": "text", "text": "and its second part"}]}
			]
		}`)

		Expect(out.turn.ThreadID).To(Equal("chat-1"))
		Expect(out.turn.Prompt).To(Equal("the newest turn \nand its second part"))
		Expect(out.turn.Answer).To(BeNil())
		Expect(out.rec.Body.Len()).To(BeZero(), "Decode writes nothing to the response")
	})

	It("Should read an approval the client answered in the newest assistant message", func() {
		out := decode(`{
			"id": "chat-1",
			"messages": [
				{"role": "user", "parts": [{"type": "text", "text": "wipe it"}]},
				{"role": "assistant", "parts": [
					{"type": "text", "text": "about to"},
					{"type": "dynamic-tool", "toolCallId": "c1", "state": "approval-responded", "approval": {"id": "approval-c1", "approved": true}}
				]}
			]
		}`)

		Expect(out.turn.Prompt).To(BeEmpty(), "the newest message is the assistant's, so nobody typed anything")
		Expect(out.turn.Answer).To(Equal(&web.Answer{ToolUseID: "c1", Kind: web.KindApprove, Approval: toolkit.ConfirmOnce}))
	})

	It("Should read an approval answered false as a decline", func() {
		out := decode(`{
			"id": "chat-1",
			"messages": [
				{"role": "assistant", "parts": [
					{"type": "dynamic-tool", "toolCallId": "c1", "state": "approval-responded", "approval": {"id": "approval-c1", "approved": false}}
				]}
			]
		}`)

		Expect(out.turn.Answer).To(Equal(&web.Answer{ToolUseID: "c1", Kind: web.KindApprove, Approval: toolkit.ConfirmNo}))
	})

	// A declined call gets no output part, so its answered part stays in the assistant
	// message useChat keeps appending to. Reading forwards would answer the decline
	// again on every later approval and put the newest question again forever.
	It("Should answer the last question the person answered rather than the first", func() {
		out := decode(`{
			"id": "chat-1",
			"messages": [
				{"role": "assistant", "parts": [
					{"type": "dynamic-tool", "toolCallId": "c1", "state": "approval-responded", "approval": {"id": "approval-c1", "approved": false}},
					{"type": "dynamic-tool", "toolCallId": "c2", "state": "approval-responded", "approval": {"id": "approval-c2", "approved": true}}
				]}
			]
		}`)

		Expect(out.turn.Answer).To(Equal(&web.Answer{ToolUseID: "c2", Kind: web.KindApprove, Approval: toolkit.ConfirmOnce}))
	})

	// A page sending a new message resends its whole history, and the approvals in it
	// were answered on the request that made each of them newest.
	It("Should leave an approval in the history alone when the newest message is the user's", func() {
		out := decode(`{
			"id": "chat-1",
			"messages": [
				{"role": "assistant", "parts": [
					{"type": "dynamic-tool", "toolCallId": "c1", "state": "approval-responded", "approval": {"id": "approval-c1", "approved": true}}
				]},
				{"role": "user", "parts": [{"type": "text", "text": "and now this"}]}
			]
		}`)

		Expect(out.turn.Answer).To(BeNil())
		Expect(out.turn.Prompt).To(Equal("and now this"))
	})

	// The standing allow has no boolean in the SDK's own approval to travel in, so it
	// arrives in the field the three human-in-the-loop answers use.
	It("Should read the standing allow out of fiskAnswer", func() {
		out := decode(`{
			"id": "chat-1",
			"messages": [
				{"role": "user", "parts": [{"type": "text", "text": "wipe it"}]},
				{"id": "msg-1", "role": "assistant", "parts": [
					{"type": "dynamic-tool", "toolCallId": "c1", "state": "approval-requested"}
				]}
			],
			"fiskAnswer": {"toolUseId": "c1", "kind": "approve", "approval": "always"}
		}`)

		Expect(out.turn.Prompt).To(BeEmpty(), "the thread ends on the message the card sits in, so nobody typed anything")
		Expect(out.turn.Answer).To(Equal(&web.Answer{ToolUseID: "c1", Kind: web.KindApprove, Approval: toolkit.ConfirmAlways}))
	})

	// Somebody who answers a card and types in the same breath submits one body carrying
	// both, and the message is appended to the history behind the card.
	It("Should read an answer and the message sent with it", func() {
		out := decode(`{
			"id": "chat-1",
			"messages": [
				{"role": "user", "parts": [{"type": "text", "text": "wipe it"}]},
				{"id": "msg-1", "role": "assistant", "parts": [
					{"type": "dynamic-tool", "toolCallId": "c1", "state": "approval-requested"}
				]},
				{"role": "user", "parts": [{"type": "text", "text": "and then list what is left"}]}
			],
			"fiskAnswer": {"toolUseId": "c1", "kind": "approve", "approval": "once"}
		}`)

		Expect(out.turn.Prompt).To(Equal("and then list what is left"))
		Expect(out.turn.Answer).To(Equal(&web.Answer{ToolUseID: "c1", Kind: web.KindApprove, Approval: toolkit.ConfirmOnce}))
	})

	It("Should read a confirm answer", func() {
		out := decode(`{"id": "chat-1", "fiskAnswer": {"toolUseId": "c1", "kind": "confirm", "confirmed": true}}`)

		Expect(out.turn.Answer).To(Equal(&web.Answer{ToolUseID: "c1", Kind: web.KindConfirm, Confirmed: true}))
	})

	It("Should read a selection as the position of the option chosen", func() {
		out := decode(`{"id": "chat-1", "fiskAnswer": {"toolUseId": "c1", "kind": "select", "index": 2}}`)

		Expect(out.turn.Answer).To(Equal(&web.Answer{ToolUseID: "c1", Kind: web.KindSelect, Index: 2}))
	})

	It("Should read a free-text answer, an empty one included", func() {
		out := decode(`{"id": "chat-1", "fiskAnswer": {"toolUseId": "c1", "kind": "input", "value": ""}}`)

		Expect(out.turn.Answer).To(Equal(&web.Answer{ToolUseID: "c1", Kind: web.KindInput}))
	})

	It("Should refuse a body that is not JSON", func() {
		_, err := decodeErr(`{"id": `)

		Expect(err).To(MatchError(vercel.ErrBadRequest))
	})

	It("Should refuse an answer to a question this agent does not ask", func() {
		_, err := decodeErr(`{"id": "chat-1", "fiskAnswer": {"toolUseId": "c1", "kind": "elicit"}}`)

		Expect(err).To(MatchError(vercel.ErrBadRequest))
		Expect(err.Error()).To(ContainSubstring(`"elicit" is not a question this agent asks`))
	})

	It("Should refuse an approval answered with anything but no, once or always", func() {
		_, err := decodeErr(`{"id": "chat-1", "fiskAnswer": {"toolUseId": "c1", "kind": "approve", "approval": "yes"}}`)

		Expect(err).To(MatchError(vercel.ErrBadRequest))
		Expect(err.Error()).To(ContainSubstring(`this one says "yes"`))
	})

	It("Should refuse a selection outside the options offered", func() {
		_, err := decodeErr(`{"id": "chat-1", "fiskAnswer": {"toolUseId": "c1", "kind": "select", "index": -1}}`)

		Expect(err).To(MatchError(vercel.ErrBadRequest))
	})

	// The channel refuses a turn that asks for nothing, so the format reports what the
	// body held rather than deciding for it.
	It("Should report an empty turn for a body carrying neither a message nor an answer", func() {
		out := decode(`{"id": "chat-1", "messages": []}`)

		Expect(out.turn).To(Equal(web.Turn{ThreadID: "chat-1"}))
	})
})
