//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	"encoding/json"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// fillHeader populates the required header fields with valid values so a message
// satisfies the common header schema.
func fillHeader(h *Header) {
	h.ID = NewID()
	h.Request = h.ID
	h.Conversation = NewID()
	h.Sequence = 1
	h.Time = time.Now().UTC()
	h.Sender = Identity{Name: "agent-a"}
}

// tamper round-trips a body through a map so a test can mutate it before
// re-validating.
func tamper(data []byte, mut func(map[string]any)) []byte {
	var m map[string]any
	Expect(json.Unmarshal(data, &m)).To(Succeed())
	mut(m)
	out, err := json.Marshal(m)
	Expect(err).ToNot(HaveOccurred())

	return out
}

var _ = Describe("Validator", func() {
	var v *Validator

	BeforeEach(func() {
		var err error
		v, err = NewValidator()
		Expect(err).ToNot(HaveOccurred())
	})

	Describe("valid messages", func() {
		It("Should accept every fully populated message type", func() {
			request := NewRequest("do the thing")
			request.Budget = &Budget{MaxTokens: 1000, MaxIterations: 5, CallTimeout: "60s"}

			toolResult := NewBlock(ToolResultBlock{
				CallID:     "c1",
				ToolResult: ToolResult{Output: "ok", Exec: &ExecResult{Command: "nats server info", ExitCode: 0}},
			})

			result := NewResult(StopEndTurn)
			result.Text = "all done"
			result.Usage = &Usage{InputTokens: 10, OutputTokens: 20}

			toolReply := NewToolReply("done", false)
			toolReply.Exec = &ExecResult{Command: "nats server info", ExitCode: 0}

			discoveryReply := NewDiscoveryReply("agent-a", "1.2.3")
			discoveryReply.Description = "manages nats auth"
			discoveryReply.Protocols = []string{ProtocolNamespace}
			discoveryReply.Tools = []ToolDescriptor{{
				Name:        "nats_server_info",
				Description: "show server info",
				InputSchema: json.RawMessage(`{"type":"object"}`),
				Behavior: toolkit.Behavior{
					ReadOnly:    toolkit.HintTrue,
					Destructive: toolkit.HintFalse,
					Idempotent:  toolkit.HintTrue,
					OpenWorld:   toolkit.HintFalse,
				},
			}}

			approve := NewElicitRequest(ElicitApprove, "q1")
			approve.Command = "stream rm"
			approve.Display = "stream rm ORDERS"
			approve.Tag = "ai:confirm"

			selectQuestion := NewElicitRequest(ElicitSelect, "q2")
			selectQuestion.Question = "which cluster?"
			selectQuestion.Options = []string{"east", "west"}

			input := NewElicitRequest(ElicitInput, "q3")
			input.Question = "which subject?"
			input.Default = "orders.>"

			choice := NewElicitReply("q1", AnswerChoice)
			choice.Choice = ChoiceOnce

			value := NewElicitReply("q3", AnswerValue)
			value.Value = "orders.new"

			messages := []any{
				request,
				NewEvent(NewThinkingBlock("hmm")),
				NewEvent(NewTextBlock("answer")),
				NewEvent(NewToolCallBlock("c1", "nats_server_info", json.RawMessage(`{"id":1}`))),
				NewEvent(toolResult),
				NewEvent(NewBlock(AgentCallBlock{ID: "a1", Name: "remote", Task: NewID()})),
				NewEvent(NewBlock(StatusBlock{Iteration: 2, Phase: "calling-llm", Usage: &Usage{InputTokens: 1}})),
				result,
				NewError("it broke"),
				NewCancel(),
				NewAck(true),
				NewToolRequest("nats_server_info", json.RawMessage(`{"id":1}`)),
				toolReply,
				NewDiscoveryRequest(),
				discoveryReply,
				approve,
				NewElicitRequest(ElicitConfirm, "q4"),
				selectQuestion,
				input,
				choice,
				NewElicitReply("q4", AnswerConfirmed),
				NewElicitReply("q2", AnswerIndex),
				value,
				NewElicitReply("q1", AnswerNoOperator),
			}

			for _, msg := range messages {
				switch m := msg.(type) {
				case *Request:
					fillHeader(&m.Header)
				case *Event:
					fillHeader(&m.Header)
				case *Result:
					fillHeader(&m.Header)
				case *ErrorMessage:
					fillHeader(&m.Header)
				case *Cancel:
					fillHeader(&m.Header)
				case *Ack:
					fillHeader(&m.Header)
				case *ToolRequest:
					fillHeader(&m.Header)
				case *ToolReply:
					fillHeader(&m.Header)
				case *DiscoveryRequest:
					fillHeader(&m.Header)
				case *DiscoveryReply:
					fillHeader(&m.Header)
				case *ElicitRequest:
					fillHeader(&m.Header)
				case *ElicitReply:
					fillHeader(&m.Header)
				default:
					Fail("unhandled message type in test")
				}

				err := v.ValidateMessage(msg)
				Expect(err).ToNot(HaveOccurred(), "%T should validate", msg)
			}
		})

		// The schema names traceparent and checks nothing about its shape: a version
		// pattern would refuse a valid future W3C version and cost the whole message,
		// where a receiver's propagator ignores anything it cannot parse.
		It("Should accept a traceparent whatever it says, and a message with none", func() {
			for _, tp := range []string{
				"",
				"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
				"01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-future",
				"nonsense",
			} {
				req := NewToolRequest("ping", nil)
				fillHeader(&req.Header)
				req.TraceParent = tp

				Expect(v.ValidateMessage(req)).To(Succeed(), tp)
			}
		})

		It("Should accept an event carrying a block type it does not name", func() {
			ev := NewEvent(NewTextBlock("hi"))
			fillHeader(&ev.Header)
			body, err := json.Marshal(ev)
			Expect(err).ToNot(HaveOccurred())

			good := tamper(body, func(m map[string]any) {
				m["block"] = map[string]any{"type": "citation", "source": "rfc1", "page": 12}
			})
			Expect(v.Validate(good)).To(Succeed())
		})

		It("Should accept every stop reason this build names", func() {
			for _, reason := range []StopReason{
				StopEndTurn, StopMaxTokens, StopRefusal, StopCanceled,
				StopError, StopBudgetExhausted, StopSuspended, StopMaxIterations,
			} {
				result := NewResult(reason)
				fillHeader(&result.Header)
				Expect(v.ValidateMessage(result)).To(Succeed(), "stop_reason %q should validate", reason)
			}
		})

		It("Should accept a stop reason it does not name, on both terminal messages", func() {
			result := NewResult(StopReason("throttled"))
			fillHeader(&result.Header)
			Expect(v.ValidateMessage(result)).To(Succeed())

			failed := NewError("it broke")
			failed.StopReason = StopReason("throttled")
			fillHeader(&failed.Header)
			Expect(v.ValidateMessage(failed)).To(Succeed())
		})

		It("Should accept an error carrying no stop reason, where a result must have one", func() {
			failed := NewError("it broke")
			fillHeader(&failed.Header)
			Expect(v.ValidateMessage(failed)).To(Succeed())

			result := NewResult("")
			fillHeader(&result.Header)
			Expect(v.ValidateMessage(result)).ToNot(Succeed())
		})

		// An answer arrives from whoever can address the running task, so the values it
		// can carry are pinned rather than left to the reader.
		It("Should bound what an elicit reply may answer with", func() {
			for _, answer := range []ElicitAnswer{AnswerChoice, AnswerConfirmed, AnswerIndex, AnswerValue, AnswerNoOperator} {
				reply := NewElicitReply("q1", answer)
				fillHeader(&reply.Header)
				Expect(v.ValidateMessage(reply)).To(Succeed(), "answer %q", answer)
			}

			unknown := NewElicitReply("q1", ElicitAnswer("whatever"))
			fillHeader(&unknown.Header)
			Expect(v.ValidateMessage(unknown)).ToNot(Succeed())

			choice := NewElicitReply("q1", AnswerChoice)
			choice.Choice = ElicitChoice("maybe")
			fillHeader(&choice.Header)
			Expect(v.ValidateMessage(choice)).ToNot(Succeed())

			long := NewElicitReply("q1", AnswerValue)
			long.Value = strings.Repeat("a", 4097)
			fillHeader(&long.Header)
			Expect(v.ValidateMessage(long)).ToNot(Succeed())
		})

		It("Should refuse an elicit question of a kind it does not name", func() {
			ask := NewElicitRequest(ElicitKind("interrogate"), "q1")
			fillHeader(&ask.Header)
			Expect(v.ValidateMessage(ask)).ToNot(Succeed())
		})
	})

	Describe("invalid messages", func() {
		var validRequest []byte

		BeforeEach(func() {
			req := NewRequest("hello")
			fillHeader(&req.Header)
			var err error
			validRequest, err = json.Marshal(req)
			Expect(err).ToNot(HaveOccurred())
			Expect(v.Validate(validRequest)).To(Succeed())
		})

		It("Should reject a missing required field", func() {
			bad := tamper(validRequest, func(m map[string]any) {
				delete(m, "prompt")
			})
			Expect(v.Validate(bad)).To(HaveOccurred())
		})

		It("Should reject an invalid sender identity", func() {
			bad := tamper(validRequest, func(m map[string]any) {
				m["sender"] = map[string]any{"name": ""}
			})
			Expect(v.Validate(bad)).To(HaveOccurred())
		})

		It("Should reject an unknown protocol id", func() {
			bad := tamper(validRequest, func(m map[string]any) {
				m["protocol"] = "io.choria.fisk-ai.v1.bogus"
			})
			Expect(v.Validate(bad)).To(MatchError(ErrUnknownProtocol))
		})

		It("Should reject a tool behavior hint that is not a boolean", func() {
			reply := NewDiscoveryReply("agent-a", "1.2.3")
			fillHeader(&reply.Header)
			reply.Tools = []ToolDescriptor{{Name: "t", Description: "a tool"}}

			data, err := json.Marshal(reply)
			Expect(err).ToNot(HaveOccurred())
			Expect(v.Validate(data)).To(Succeed())

			bad := tamper(data, func(m map[string]any) {
				tools := m["tools"].([]any)
				tools[0].(map[string]any)["behavior"] = map[string]any{"read_only": "yes"}
			})
			Expect(v.Validate(bad)).To(HaveOccurred())
		})

		// Accepting a type the schema does not name must not relax one it does. Each of
		// these fails its own branch on the malformed field and fails the unknown branch
		// on the excluded type, so nothing in the oneOf matches.
		It("Should still reject a malformed block whose type it does name", func() {
			ev := NewEvent(NewTextBlock("hi"))
			fillHeader(&ev.Header)
			body, err := json.Marshal(ev)
			Expect(err).ToNot(HaveOccurred())

			malformed := []map[string]any{
				{"type": "text"},
				{"type": "tool_call", "name": "t"},
				{"type": "tool_result"},
				{"type": "status", "iteration": "3"},
				{"type": "agent_call", "id": "a1", "name": "remote"},
			}

			for _, block := range malformed {
				bad := tamper(body, func(m map[string]any) {
					m["block"] = block
				})
				Expect(v.Validate(bad)).To(HaveOccurred(), "%v", block)
			}
		})

		It("Should still bound a stop reason it does not name", func() {
			result := NewResult(StopEndTurn)
			fillHeader(&result.Header)
			body, err := json.Marshal(result)
			Expect(err).ToNot(HaveOccurred())

			// A receiver displays this, so tolerating an unnamed reason must not tolerate
			// a payload, a newline or a terminal escape in the field.
			for _, reason := range []any{"", nil, 7, "Throttled", "two words", "a\nb",
				strings.Repeat("x", 65)} {
				bad := tamper(body, func(m map[string]any) {
					m["stop_reason"] = reason
				})
				Expect(v.Validate(bad)).To(HaveOccurred(), "%v", reason)
			}
		})

		It("Should reject a block with no usable type discriminator", func() {
			ev := NewEvent(NewTextBlock("hi"))
			fillHeader(&ev.Header)
			body, err := json.Marshal(ev)
			Expect(err).ToNot(HaveOccurred())

			for _, block := range []map[string]any{
				{},
				{"type": nil},
				{"type": ""},
				{"type": 7},
				{"text": "hi"},
			} {
				bad := tamper(body, func(m map[string]any) {
					m["block"] = block
				})
				Expect(v.Validate(bad)).To(HaveOccurred(), "%v", block)
			}
		})
	})
})
