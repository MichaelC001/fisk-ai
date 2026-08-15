//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	"encoding/json"
	"errors"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestA2A(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "A2A")
}

var _ = Describe("A2A", func() {
	Describe("NewID", func() {
		It("Should mint distinct ids", func() {
			Expect(NewID()).NotTo(Equal(NewID()))
		})
	})

	Describe("Decode", func() {
		It("Should dispatch on the protocol id", func() {
			req := NewRequest("hello")
			req.ID = NewID()
			req.Request = req.ID

			body, err := json.Marshal(req)
			Expect(err).ToNot(HaveOccurred())

			msg, err := Decode(body)
			Expect(err).ToNot(HaveOccurred())

			got, ok := msg.(*Request)
			Expect(ok).To(BeTrue())
			Expect(got.Prompt).To(Equal("hello"))
			Expect(got.Protocol).To(Equal(RequestProtocol))
		})

		It("Should reject an unknown protocol", func() {
			_, err := Decode([]byte(`{"protocol":"io.choria.fisk-ai.v1.bogus"}`))
			Expect(err).To(MatchError(ErrUnknownProtocol))
		})

		It("Should decode every message type to its concrete pointer", func() {
			cases := []struct {
				msg  any
				want any
			}{
				{NewRequest("p"), &Request{}},
				{NewEvent(NewTextBlock("hi")), &Event{}},
				{NewResult(StopEndTurn), &Result{}},
				{NewError("boom"), &ErrorMessage{}},
				{NewCancel(), &Cancel{}},
				{NewAck(true), &Ack{}},
				{NewToolRequest("nats_server_info", json.RawMessage(`{"id":1}`)), &ToolRequest{}},
				{NewToolReply("ok", false), &ToolReply{}},
				{NewDiscoveryRequest(), &DiscoveryRequest{}},
				{NewDiscoveryReply("agent-a", "1.0.0"), &DiscoveryReply{}},
			}

			for _, tc := range cases {
				body, err := json.Marshal(tc.msg)
				Expect(err).ToNot(HaveOccurred())

				decoded, err := Decode(body)
				Expect(err).ToNot(HaveOccurred())
				Expect(decoded).To(BeAssignableToTypeOf(tc.want))
			}
		})
	})

	Describe("ValidIdentityName", func() {
		It("Should accept what the schema accepts and refuse what it does not", func() {
			for _, name := range []string{"agent", "agent-1", "agent_1", "AGENT9"} {
				Expect(ValidIdentityName(name)).To(BeTrue(), name)
			}

			for _, name := range []string{"", "svc.example", "with space", "sl/ash"} {
				Expect(ValidIdentityName(name)).To(BeFalse(), name)
			}
		})

		// The point of exporting it: a sender checks a name before building a message,
		// rather than the receiver reporting only that the message failed validation.
		It("Should agree with the schema it states", func() {
			validator, err := NewValidator()
			Expect(err).ToNot(HaveOccurred())

			req := NewRequest("go")
			req.ID = NewID()
			req.Request = req.ID
			req.Conversation = req.ID
			req.Sender = Identity{Name: "svc.example"}

			body, err := json.Marshal(req)
			Expect(err).ToNot(HaveOccurred())

			Expect(ValidIdentityName(req.Sender.Name)).To(BeFalse())
			Expect(validator.Validate(body)).ToNot(Succeed())
		})
	})

	Describe("ValidRequestID", func() {
		It("Should accept what the schema accepts and refuse what it does not", func() {
			for _, id := range []string{"2abc", "task-1", "task_1", NewID()} {
				Expect(ValidRequestID(id)).To(BeTrue(), id)
			}

			for _, id := range []string{"", "task.other", "task>", "task*", "task with space", "sl/ash"} {
				Expect(ValidRequestID(id)).To(BeFalse(), id)
			}
		})

		// The task path makes the request id part of the address the process running
		// that task listens on, so a caller choosing those bytes freely would shape a
		// subscription. The Go rule and the schema's have to be the same rule.
		It("Should agree with the schema it states", func() {
			validator, err := NewValidator()
			Expect(err).ToNot(HaveOccurred())

			for _, id := range []string{"task.other", "task>", "with space"} {
				req := NewRequest("go")
				fillHeader(&req.Header)
				req.Request = id

				body, err := json.Marshal(req)
				Expect(err).ToNot(HaveOccurred())

				Expect(ValidRequestID(id)).To(BeFalse(), id)
				Expect(validator.Validate(body)).ToNot(Succeed(), id)
			}
		})
	})

	Describe("StopReason.Valid", func() {
		It("Should name exactly the eight reasons this build carries", func() {
			for _, reason := range []StopReason{
				StopEndTurn, StopMaxTokens, StopRefusal, StopCanceled,
				StopError, StopBudgetExhausted, StopSuspended, StopMaxIterations,
			} {
				Expect(reason.Valid()).To(BeTrue(), string(reason))
			}

			for _, reason := range []StopReason{"", "throttled", "end_trun", "tool_use"} {
				Expect(reason.Valid()).To(BeFalse(), string(reason))
			}
		})

		// The Go list and the schema's are two statements of one vocabulary. Valid is
		// what a sender checks before building a message, so a reason it accepts has to
		// be one a receiver takes.
		It("Should accept nothing the schema refuses", func() {
			validator, err := NewValidator()
			Expect(err).ToNot(HaveOccurred())

			for _, reason := range []StopReason{
				StopEndTurn, StopMaxTokens, StopRefusal, StopCanceled,
				StopError, StopBudgetExhausted, StopSuspended, StopMaxIterations,
			} {
				result := NewResult(reason)
				fillHeader(&result.Header)

				Expect(reason.Valid()).To(BeTrue(), string(reason))
				Expect(validator.ValidateMessage(result)).To(Succeed(), string(reason))
			}

			// The other direction is deliberately not symmetric: the schema takes reasons
			// Valid refuses, which is the whole of what this item changed.
			unnamed := NewResult(StopReason("throttled"))
			fillHeader(&unnamed.Header)
			Expect(unnamed.StopReason.Valid()).To(BeFalse())
			Expect(validator.ValidateMessage(unnamed)).To(Succeed())
		})
	})

	Describe("DecodeTerminal", func() {
		encode := func(msg any) []byte {
			GinkgoHelper()

			body, err := json.Marshal(msg)
			Expect(err).ToNot(HaveOccurred())

			return body
		}

		It("Should return the result of a task that answered", func() {
			msg := NewResult(StopEndTurn)
			msg.Text = "all done"

			res, err := DecodeTerminal(encode(msg))
			Expect(err).ToNot(HaveOccurred())
			Expect(res.Text).To(Equal("all done"))
			Expect(res.StopReason).To(Equal(StopEndTurn))
		})

		// The failure is the error rather than a second return value, so the ordinary
		// path is a nil check and a caller separates the two kinds of failure with
		// errors.As.
		It("Should return a failure as the error it already implements", func() {
			msg := NewError("the run failed")
			msg.StopReason = StopBudgetExhausted
			msg.Code = "budget"

			res, err := DecodeTerminal(encode(msg))
			Expect(res).To(BeNil())
			Expect(err).To(MatchError("the run failed"))

			var failed *ErrorMessage
			Expect(errors.As(err, &failed)).To(BeTrue())
			Expect(failed.StopReason).To(Equal(StopBudgetExhausted))
			Expect(failed.Code).To(Equal("budget"))
		})

		// A message that decodes but does not end a task is a different problem from a
		// task that failed, so it must not arrive as an *ErrorMessage.
		It("Should refuse a message that does not end a task", func() {
			res, err := DecodeTerminal(encode(NewRequest("go")))
			Expect(res).To(BeNil())
			Expect(err).To(MatchError(ErrProtocolMismatch))
			Expect(err).To(MatchError(ContainSubstring(RequestProtocol)))

			var failed *ErrorMessage
			Expect(errors.As(err, &failed)).To(BeFalse(), "a caller must not read this as a run that failed")
		})

		It("Should pass an undecodable body's error through", func() {
			_, err := DecodeTerminal([]byte(`{"protocol":"io.choria.fisk-ai.v1.bogus"}`))
			Expect(err).To(MatchError(ErrUnknownProtocol))
		})
	})

	Describe("Block", func() {
		It("Should add a type discriminator on marshal", func() {
			body, err := json.Marshal(NewTextBlock("hi"))
			Expect(err).ToNot(HaveOccurred())

			var fields map[string]any
			Expect(json.Unmarshal(body, &fields)).To(Succeed())
			Expect(fields["type"]).To(Equal(string(BlockText)))
			Expect(fields["text"]).To(Equal("hi"))
		})

		It("Should round-trip every block type", func() {
			blocks := []Block{
				NewThinkingBlock("reasoning"),
				NewTextBlock("answer"),
				NewToolCallBlock("c1", "nats_server_info", json.RawMessage(`{"a":1}`)),
				NewToolResultBlock("c1", "output", false),
				NewBlock(AgentCallBlock{ID: "a1", Name: "remote", Task: "t1"}),
				NewBlock(StatusBlock{Iteration: 2, Phase: "calling-llm"}),
			}

			for _, b := range blocks {
				body, err := json.Marshal(b)
				Expect(err).ToNot(HaveOccurred())

				var got Block
				Expect(json.Unmarshal(body, &got)).To(Succeed())
				Expect(got.Type()).To(Equal(b.Type()))
				Expect(got.AsAny()).To(Equal(b.AsAny()))
			}
		})

		It("Should dispatch via a type switch on AsAny", func() {
			var got Block
			body, err := json.Marshal(NewToolResultBlock("c1", "out", true))
			Expect(err).ToNot(HaveOccurred())
			Expect(json.Unmarshal(body, &got)).To(Succeed())

			switch v := got.AsAny().(type) {
			case ToolResultBlock:
				Expect(v.CallID).To(Equal("c1"))
				Expect(v.IsError).To(BeTrue())
			default:
				Fail("expected a ToolResultBlock")
			}
		})

		It("Should carry a type it does not name rather than failing the message", func() {
			var got Block
			Expect(json.Unmarshal([]byte(`{"type":"citation","source":"rfc1","page":12}`), &got)).To(Succeed())

			Expect(got.Type()).To(Equal(BlockType("citation")))

			unknown, ok := got.Content().(UnknownBlock)
			Expect(ok).To(BeTrue(), "an unnamed type decodes to an UnknownBlock")
			Expect(unknown.Type).To(Equal(BlockType("citation")))
			Expect(unknown.Raw).To(MatchJSON(`{"type":"citation","source":"rfc1","page":12}`))
		})

		It("Should re-marshal an unknown block to the same JSON value", func() {
			var got Block
			Expect(json.Unmarshal([]byte(`{"type":"citation","source":"rfc1","page":12}`), &got)).To(Succeed())

			// The same value rather than the same bytes: stamping the discriminator
			// re-encodes the object, which sorts its keys.
			out, err := json.Marshal(got)
			Expect(err).ToNot(HaveOccurred())
			Expect(out).To(MatchJSON(`{"type":"citation","source":"rfc1","page":12}`))
		})

		It("Should keep an unknown block distinguishable from an empty one", func() {
			var got Block
			Expect(json.Unmarshal([]byte(`{"type":"citation"}`), &got)).To(Succeed())

			Expect(Block{}.Type()).To(BeEmpty())
			Expect(Block{}.Content()).To(BeNil())

			Expect(got.Type()).ToNot(BeEmpty())
			Expect(got.Content()).ToNot(BeNil())
		})

		It("Should refuse a block carrying no type", func() {
			// All three leave the probe's type empty, so without their own case each would
			// decode to an UnknownBlock whose type says nothing.
			for _, body := range []string{`{}`, `{"type":null}`, `{"type":""}`, `{"source":"rfc1"}`} {
				var got Block
				Expect(json.Unmarshal([]byte(body), &got)).To(MatchError(ErrInvalidMessage), body)
			}
		})

		It("Should fail to marshal an empty block, and an unknown one holding nothing", func() {
			_, err := json.Marshal(Block{})
			Expect(err).To(HaveOccurred())

			_, err = json.Marshal(NewBlock(UnknownBlock{Type: "citation"}))
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("Request", func() {
		It("Should default to streaming when unset", func() {
			Expect(NewRequest("p").WantsStream()).To(BeTrue())
		})

		It("Should honor an explicit stream false across a round-trip", func() {
			no := false
			req := NewRequest("p")
			req.Stream = &no

			body, err := json.Marshal(req)
			Expect(err).ToNot(HaveOccurred())

			decoded, err := Decode(body)
			Expect(err).ToNot(HaveOccurred())
			Expect(decoded.(*Request).WantsStream()).To(BeFalse())
		})
	})

	Describe("ErrorMessage", func() {
		It("Should satisfy the error interface", func() {
			var err error = NewError("boom")
			Expect(err.Error()).To(Equal("boom"))
		})
	})

	Describe("Header", func() {
		It("Should marshal flat into the body", func() {
			req := NewRequest("p")
			req.ID = "id1"
			req.Sender = Identity{Name: "agent-a"}

			body, err := json.Marshal(req)
			Expect(err).ToNot(HaveOccurred())

			var fields map[string]any
			Expect(json.Unmarshal(body, &fields)).To(Succeed())
			Expect(fields).To(HaveKey("protocol"))
			Expect(fields).To(HaveKey("id"))
			Expect(fields).To(HaveKey("sender"))
			Expect(fields).To(HaveKey("prompt"))
		})
	})
})
