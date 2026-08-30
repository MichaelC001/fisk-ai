//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestA2A(t *testing.T) {
	RegisterFailHandler(Fail)

	// The specs here wait on a shell the served tool starts and on the goroutine that
	// owns a reply set, and go test runs packages in parallel, so Gomega's one second
	// measures the machine's load rather than this code. Waiting longer costs nothing
	// when the assertion holds, since Eventually returns as soon as it is satisfied.
	SetDefaultEventuallyTimeout(30 * time.Second)

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
			Expect(got.Protocol).To(Equal(RequestPromptProtocol))
			Expect(got.Kind).To(Equal(RequestPrompt))
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
				{NewElicitRequest(ElicitConfirm, "q1"), &ElicitRequest{}},
				{NewElicitReply("q1", AnswerConfirmed), &ElicitReply{}},
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

			for _, id := range []string{"task.other", "task>", "with space", strings.Repeat("a", MaxRequestIDBytes+1)} {
				req := NewRequest("go")
				fillHeader(&req.Header)
				req.Request = id

				body, err := json.Marshal(req)
				Expect(err).ToNot(HaveOccurred())

				Expect(ValidRequestID(id)).To(BeFalse(), id)
				Expect(validator.Validate(body)).ToNot(Succeed(), id)
			}
		})

		// The character set alone left the tag limited only by the message cap, and it
		// reaches a subject, so the length is part of the rule rather than a courtesy.
		It("Should refuse a tag longer than the cap and accept one at it", func() {
			Expect(ValidRequestID(strings.Repeat("a", MaxRequestIDBytes))).To(BeTrue())
			Expect(ValidRequestID(strings.Repeat("a", MaxRequestIDBytes+1))).To(BeFalse())
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
		// A block is read as part of the event that named it, so these go through one
		// rather than through the block alone: on the wire nothing but the id says what
		// a block is.
		eventOf := func(b Block) []byte {
			GinkgoHelper()

			body, err := json.Marshal(NewEvent(b))
			Expect(err).ToNot(HaveOccurred())

			return body
		}

		It("Should carry no type of its own on marshal, the event's id naming it", func() {
			body := eventOf(NewTextBlock("hi"))

			var fields map[string]any
			Expect(json.Unmarshal(body, &fields)).To(Succeed())
			Expect(fields["protocol"]).To(Equal(EventTextProtocol))

			block, ok := fields["block"].(map[string]any)
			Expect(ok).To(BeTrue())
			Expect(block).ToNot(HaveKey("type"), "what it is travels once, in the id")
			Expect(block["text"]).To(Equal("hi"))
		})

		It("Should round-trip every block type", func() {
			blocks := []Block{
				NewThinkingBlock("reasoning"),
				NewTextBlock("answer"),
				NewToolCallBlock("c1", "nats_server_info", json.RawMessage(`{"a":1}`)),
				NewToolResultBlock("c1", "output", false),
				NewBlock(AgentCallBlock{ID: "a1", Name: "remote", Task: "t1"}),
				NewBlock(StatusBlock{Iteration: 2, Phase: "calling-llm"}),
				NewBlock(WarningBlock{Kind: "tool_timeout", Name: "ls"}),
				NewBlock(PromptBlock{Text: "remove the stream"}),
				NewBlock(TextDeltaBlock{Index: 1, Iteration: 3, Text: "part"}),
				NewBlock(TextDeltaBlock{Index: 1, Iteration: 3, Final: true}),
				NewBlock(ThinkingDeltaBlock{Index: 0, Iteration: 3, Text: "hmm"}),
				NewBlock(ThinkingDeltaBlock{Index: 0, Iteration: 3, Final: true}),
				NewBlock(TextBlock{Text: "answer", Index: 1, Trimmed: true}),
				NewBlock(ThinkingBlock{Text: "reasoning", Index: 0, Trimmed: true}),
			}

			for _, b := range blocks {
				var got Event
				Expect(json.Unmarshal(eventOf(b), &got)).To(Succeed())
				Expect(got.Protocol).To(Equal(EventProtocolFor(b.Type())))
				Expect(got.Block.Type()).To(Equal(b.Type()))
				Expect(got.Block.AsAny()).To(Equal(b.AsAny()))
			}
		})

		It("Should dispatch via a type switch on AsAny", func() {
			var got Event
			Expect(json.Unmarshal(eventOf(NewToolResultBlock("c1", "out", true)), &got)).To(Succeed())

			switch v := got.Block.AsAny().(type) {
			case ToolResultBlock:
				Expect(v.CallID).To(Equal("c1"))
				Expect(v.IsError).To(BeTrue())
			default:
				Fail("expected a ToolResultBlock")
			}
		})

		It("Should carry a kind it does not name rather than failing the message", func() {
			body := `{"protocol":"io.choria.fisk-ai.v1.event.citation","id":"m1","request":"r1",` +
				`"sender":{"name":"svc"},"block":{"source":"rfc1","page":12}}`

			var got Event
			Expect(json.Unmarshal([]byte(body), &got)).To(Succeed())

			Expect(got.ID).To(Equal("m1"), "the message it held arrives whole")
			Expect(got.Block.Type()).To(Equal(BlockType("citation")))

			unknown, ok := got.Block.Content().(UnknownBlock)
			Expect(ok).To(BeTrue(), "a kind nobody here names decodes to an UnknownBlock")
			Expect(unknown.Type).To(Equal(BlockType("citation")))
			Expect(unknown.Raw).To(MatchJSON(`{"source":"rfc1","page":12}`))
		})

		It("Should send an unknown block back out as the peer's own value", func() {
			body := `{"protocol":"io.choria.fisk-ai.v1.event.citation","id":"m1","request":"r1",` +
				`"sender":{"name":"svc"},"block":{"source":"rfc1","page":12}}`

			var got Event
			Expect(json.Unmarshal([]byte(body), &got)).To(Succeed())

			out, err := json.Marshal(got)
			Expect(err).ToNot(HaveOccurred())

			var fields map[string]json.RawMessage
			Expect(json.Unmarshal(out, &fields)).To(Succeed())
			Expect(string(fields["protocol"])).To(Equal(`"io.choria.fisk-ai.v1.event.citation"`),
				"it goes back out under the id it arrived on")
			Expect(fields["block"]).To(MatchJSON(`{"source":"rfc1","page":12}`),
				"and with the peer's own value, not one this build re-made")
		})

		// A fragment travels under an id of its own, so a build with no case for either
		// still reads the message: the id puts it in the event family, and the block it
		// cannot name is kept as the peer sent it.
		It("Should reach a build naming neither fragment as an unknown block", func() {
			validator, err := NewValidator()
			Expect(err).ToNot(HaveOccurred())

			for _, b := range []Block{
				NewBlock(TextDeltaBlock{Index: 1, Iteration: 3, Text: "part"}),
				NewBlock(ThinkingDeltaBlock{Index: 0, Iteration: 3, Text: "hmm", Final: true}),
			} {
				id := EventProtocolFor(b.Type())
				kind, ok := blockTypeOf(id)
				Expect(ok).To(BeTrue(), id)
				Expect(kind).To(Equal(b.Type()))

				ev := NewEvent(b)
				fillHeader(&ev.Header)
				body, err := json.Marshal(ev)
				Expect(err).ToNot(HaveOccurred())
				Expect(validator.Validate(body)).To(Succeed(), id)

				var fields map[string]json.RawMessage
				Expect(json.Unmarshal(body, &fields)).To(Succeed())

				// The same bytes under an id nobody names, which is what the pair is to a
				// build that has neither case.
				older := tamper(body, func(m map[string]any) {
					m["protocol"] = id + "_future"
				})
				Expect(validator.Validate(older)).To(Succeed(), "the framing is what is left to check")

				decoded, err := Decode(older)
				Expect(err).ToNot(HaveOccurred())

				unknown, ok := decoded.(*Event).Block.Content().(UnknownBlock)
				Expect(ok).To(BeTrue(), "a kind nobody here names decodes to an UnknownBlock")
				Expect(unknown.Raw).To(MatchJSON(fields["block"]))
			}
		})

		// Index is 0 for the first block of a call, so it cannot be omitted when unset
		// and still name that block. The two fields the whole blocks gained are the other
		// way round: absent is what every producer sends today.
		It("Should send a fragment's index always and a whole block's only when set", func() {
			body := eventOf(NewBlock(TextDeltaBlock{Index: 0, Final: true}))

			var ev map[string]json.RawMessage
			Expect(json.Unmarshal(body, &ev)).To(Succeed())

			var fragment map[string]any
			Expect(json.Unmarshal(ev["block"], &fragment)).To(Succeed())
			Expect(fragment).To(HaveKeyWithValue("index", BeEquivalentTo(0)))
			Expect(fragment).ToNot(HaveKey("iteration"))
			Expect(fragment).ToNot(HaveKey("text"))

			Expect(json.Unmarshal(eventOf(NewTextBlock("answer")), &ev)).To(Succeed())

			var whole map[string]any
			Expect(json.Unmarshal(ev["block"], &whole)).To(Succeed())
			Expect(whole).To(HaveKeyWithValue("text", "answer"))
			Expect(whole).ToNot(HaveKey("index"))
			Expect(whole).ToNot(HaveKey("trimmed"))
		})

		It("Should keep an unknown block distinguishable from an empty one", func() {
			var got Event
			Expect(json.Unmarshal([]byte(`{"protocol":"io.choria.fisk-ai.v1.event.citation",`+
				`"id":"m1","request":"r1","sender":{"name":"svc"},"block":{}}`), &got)).To(Succeed())

			Expect(Block{}.Type()).To(BeEmpty())
			Expect(Block{}.Content()).To(BeNil())

			Expect(got.Block.Type()).ToNot(BeEmpty())
			Expect(got.Block.Content()).ToNot(BeNil())
		})

		// The id is the only thing that says what a block is, so a message that carries
		// no usable one carries a block nobody can read.
		It("Should refuse an event whose id names no block", func() {
			for _, protocol := range []string{
				"io.choria.fisk-ai.v1.event",
				"io.choria.fisk-ai.v1.event.",
				"io.choria.fisk-ai.v1.event.text.evil",
				"io.choria.fisk-ai.v1.result",
			} {
				body, err := json.Marshal(map[string]any{
					"protocol": protocol,
					"id":       "m1",
					"request":  "r1",
					"sender":   map[string]any{"name": "svc"},
					"block":    map[string]any{"text": "hi"},
				})
				Expect(err).ToNot(HaveOccurred())

				var got Event
				Expect(json.Unmarshal(body, &got)).To(MatchError(ErrInvalidMessage), protocol)
			}
		})

		It("Should refuse an event carrying no block at all", func() {
			var got Event
			Expect(json.Unmarshal([]byte(`{"protocol":"io.choria.fisk-ai.v1.event.text",`+
				`"id":"m1","request":"r1","sender":{"name":"svc"}}`), &got)).To(MatchError(ErrInvalidMessage))
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

		// Defaulting to none is what keeps a caller that says nothing paying nothing.
		It("Should send no fragments unless the caller asks for them", func() {
			Expect(NewRequest("p").WantsDeltas()).To(BeFalse())

			yes := true
			req := NewRequest("p")
			req.Deltas = &yes
			Expect(req.WantsDeltas()).To(BeTrue())

			no := false
			req.Deltas = &no
			Expect(req.WantsDeltas()).To(BeFalse())
		})

		It("Should send no fragments to a caller that asked for no stream", func() {
			yes, no := true, false
			req := NewRequest("p")
			req.Deltas = &yes
			req.Stream = &no

			Expect(req.WantsStream()).To(BeFalse())
			Expect(req.WantsDeltas()).To(BeFalse(), "a fragment is an event")
		})

		It("Should carry the ask across a round-trip on every request that runs a model", func() {
			validator, err := NewValidator()
			Expect(err).ToNot(HaveOccurred())

			yes := true
			answer := &Answer{ToolUseID: "toolu_1", Kind: ElicitConfirm, Answer: AnswerConfirmed, Confirmed: true}

			for _, req := range []*Request{
				NewRequest("p"),
				NewResume("2Ab3Cd4Ef5Gh"),
				NewAnswerRequest("2Ab3Cd4Ef5Gh", answer),
			} {
				req.Deltas = &yes
				fillHeader(&req.Header)

				body, err := json.Marshal(req)
				Expect(err).ToNot(HaveOccurred())
				Expect(validator.Validate(body)).To(Succeed(), string(req.Kind))

				decoded, err := Decode(body)
				Expect(err).ToNot(HaveOccurred())
				Expect(decoded.(*Request).WantsDeltas()).To(BeTrue(), string(req.Kind))
			}
		})
	})

	Describe("ErrorMessage", func() {
		It("Should satisfy the error interface", func() {
			var err error = NewError("boom")
			Expect(err.Error()).To(Equal("boom"))
		})
	})

	Describe("stampRequest", func() {
		// The tag is the caller's own correlation and means nothing to a receiver, so the
		// turns of one conversation can carry one tag only if the stamp leaves it alone.
		It("Should keep a conversation tag the caller set and mint one when it did not", func() {
			fresh := NewRequest("p")
			stampRequest(context.Background(), &fresh.Header, "caller1", "svc")
			Expect(fresh.Conversation).ToNot(BeEmpty())

			carried := NewRequest("p")
			carried.Conversation = "conv1"
			stampRequest(context.Background(), &carried.Header, "caller1", "svc")
			Expect(carried.Conversation).To(Equal("conv1"))
		})

		// Canceling a task and answering its questions both name the request tag, so a
		// caller that only learned it when the call returned could not name the call it
		// was inside.
		It("Should keep a request tag the caller set and mint one when it did not", func() {
			fresh := NewRequest("p")
			minted := fresh.Request
			Expect(minted).ToNot(BeEmpty(), "the constructor names the turn")
			stampRequest(context.Background(), &fresh.Header, "caller1", "svc")
			Expect(fresh.Request).To(Equal(minted), "the send keeps what it finds")

			carried := NewRequest("p")
			carried.Request = "req1"
			stampRequest(context.Background(), &carried.Header, "caller1", "svc")
			Expect(carried.Request).To(Equal("req1"))
			Expect(carried.Conversation).ToNot(Equal("req1"), "a conversation the caller did not name is still fresh")
		})

		// The tag names the turn every reply echoes and the id names one message, so a
		// turn that sends more than one message can tell them apart.
		It("Should give the message an id of its own", func() {
			req := NewRequest("p")
			req.Request = "req1"
			stampRequest(context.Background(), &req.Header, "caller1", "svc")

			Expect(req.ID).ToNot(BeEmpty())
			Expect(req.ID).ToNot(Equal("req1"))
		})

		// A tool or discovery RPC belongs to no larger task, so nothing minted a tag for
		// it and the send does, which is what its subject is built from.
		It("Should mint a request tag for a message that carries none", func() {
			tool := NewToolRequest("read_file", json.RawMessage(`{}`))
			stampRequest(context.Background(), &tool.Header, "caller1", "svc")

			Expect(tool.Request).ToNot(BeEmpty())
			Expect(tool.Conversation).To(Equal(tool.Request))
		})
	})

	Describe("NewFollowUp", func() {
		// A follow-up is built from the ack that accepted the conversation, so a caller
		// copies neither the token nor the tag across by hand.
		It("Should correlate the turn to the conversation the ack accepted", func() {
			ack := NewAck(true)
			ack.Conversation = "conv1"
			ack.ConversationToken = "2Ab3Cd4Ef5Gh"

			req := NewFollowUp(ack, "and the other one")
			Expect(req.Protocol).To(Equal(RequestPromptProtocol))
			Expect(req.Prompt).To(Equal("and the other one"))
			Expect(req.ConversationToken).To(Equal("2Ab3Cd4Ef5Gh"))
			Expect(req.Conversation).To(Equal("conv1"))
			// The reply set is its own, so the turn carries the fresh tag its constructor
			// minted rather than the one the ack answered.
			Expect(req.Request).ToNot(BeEmpty())
			Expect(req.Request).ToNot(Equal(ack.Request))
		})

		It("Should round-trip the token through the schema on both messages", func() {
			validator, err := NewValidator()
			Expect(err).ToNot(HaveOccurred())

			ack := NewAck(true)
			fillHeader(&ack.Header)
			ack.ConversationToken = "2Ab3Cd4Ef5Gh"

			body, err := json.Marshal(ack)
			Expect(err).ToNot(HaveOccurred())
			Expect(validator.Validate(body)).To(Succeed())

			decoded, err := Decode(body)
			Expect(err).ToNot(HaveOccurred())
			Expect(decoded.(*Ack).ConversationToken).To(Equal("2Ab3Cd4Ef5Gh"))

			req := NewFollowUp(decoded.(*Ack), "carry on")
			fillHeader(&req.Header)
			req.Conversation = decoded.(*Ack).Conversation

			body, err = json.Marshal(req)
			Expect(err).ToNot(HaveOccurred())
			Expect(validator.Validate(body)).To(Succeed())

			decoded, err = Decode(body)
			Expect(err).ToNot(HaveOccurred())
			Expect(decoded.(*Request).ConversationToken).To(Equal("2Ab3Cd4Ef5Gh"))
		})

		// The token reaches a store key and a subject, so a receiver refuses one carrying
		// anything else at the boundary rather than after it has been used.
		It("Should refuse a token outside the character set", func() {
			validator, err := NewValidator()
			Expect(err).ToNot(HaveOccurred())

			req := NewRequest("carry on")
			fillHeader(&req.Header)
			req.ConversationToken = "../../etc/passwd"

			body, err := json.Marshal(req)
			Expect(err).ToNot(HaveOccurred())
			Expect(validator.Validate(body)).ToNot(Succeed())
		})
	})

	Describe("NewElicitReplyFromRequest", func() {
		// The reply is addressed by its header: the task it belongs to, the question it
		// answers and who is answering. A caller filling those by hand gets no error from
		// one it fills wrong, only a question that stays unanswered.
		It("Should correlate the reply to the task and the question", func() {
			ask := NewElicitRequest(ElicitApprove, "q1")
			ask.ID = NewID()
			ask.Request = "task1"
			ask.Conversation = "conv1"
			ask.Sequence = 4
			ask.Sender = Identity{Name: "worker1"}

			reply := NewElicitReplyFromRequest(ask, "caller1", AnswerChoice)
			reply.Choice = ChoiceOnce

			// The suffix is the question's kind, not the field the answer carries, so a
			// choice answers approve.
			Expect(reply.Protocol).To(Equal(ElicitReplyApproveProtocol))
			Expect(reply.QuestionID).To(Equal("q1"))
			Expect(reply.Request).To(Equal("task1"))
			Expect(reply.Conversation).To(Equal("conv1"))
			Expect(reply.Sender.Name).To(Equal("caller1"))
			Expect(reply.Recipient.Name).To(Equal("worker1"), "the answer goes back to the agent that asked")

			// Its own id, and a sequence of zero: a reply travels alone rather than in the
			// task's numbered set.
			Expect(reply.ID).ToNot(BeEmpty())
			Expect(reply.ID).ToNot(Equal(ask.ID))
			Expect(reply.Sequence).To(BeZero())
			Expect(reply.Time).ToNot(BeZero())
		})

		// One constructor per answer kind, so a reply cannot carry "choice" with only
		// Confirmed set, which reaches the agent that asked as an approval nobody made.
		It("Should set the answer kind and its value together", func() {
			ask := NewElicitRequest(ElicitApprove, "q1")
			ask.Request = "task1"
			ask.Conversation = "task1"
			ask.Sender = Identity{Name: "worker1"}

			approve := NewApproveReply(ask, "caller1", ChoiceAlways)
			Expect(approve.Answer).To(Equal(AnswerChoice))
			Expect(approve.Choice).To(Equal(ChoiceAlways))

			confirm := NewConfirmReply(ask, "caller1", true)
			Expect(confirm.Answer).To(Equal(AnswerConfirmed))
			Expect(confirm.Confirmed).To(BeTrue())

			selected := NewSelectReply(ask, "caller1", 2)
			Expect(selected.Answer).To(Equal(AnswerIndex))
			Expect(selected.Index).To(Equal(2))

			input := NewInputReply(ask, "caller1", "orders.new")
			Expect(input.Answer).To(Equal(AnswerValue))
			Expect(input.Value).To(Equal("orders.new"))

			none := NewNoOperatorReply(ask, "caller1")
			Expect(none.Answer).To(Equal(AnswerNoOperator))

			// All five are addressed the same way, since they share the header
			// NewElicitReplyFromRequest built.
			for _, reply := range []*ElicitReply{approve, confirm, selected, input, none} {
				Expect(reply.QuestionID).To(Equal("q1"))
				Expect(reply.Request).To(Equal("task1"))
				Expect(reply.Sender.Name).To(Equal("caller1"))
				Expect(reply.Recipient.Name).To(Equal("worker1"))
			}
		})

		It("Should build a reply the schemas accept", func() {
			validator, err := NewValidator()
			Expect(err).ToNot(HaveOccurred())

			ask := NewElicitRequest(ElicitConfirm, "q2")
			ask.ID = NewID()
			ask.Request = NewID()
			ask.Conversation = ask.Request
			ask.Sender = Identity{Name: "worker1"}

			reply := NewElicitReplyFromRequest(ask, "caller1", AnswerConfirmed)
			reply.Confirmed = true

			body, err := json.Marshal(reply)
			Expect(err).ToNot(HaveOccurred())
			Expect(validator.Validate(body)).To(Succeed())

			decoded, err := Decode(body)
			Expect(err).ToNot(HaveOccurred())
			Expect(decoded.(*ElicitReply).Confirmed).To(BeTrue())
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
