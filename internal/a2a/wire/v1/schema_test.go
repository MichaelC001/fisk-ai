//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package wire

import (
	"encoding/json"
	"io/fs"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
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

// everyMessage builds one instance of every protocol id in protocolSchemaFile, headers
// filled and every optional field set. The schema specs and the drift specs both read
// it.
//
// Setting every field is what makes it worth having, since a field left at its Go zero
// is omitted from the document and a property no sample writes is a property no spec
// checks. Two specs in schema_drift_test.go hold it to that: one fails when a protocol
// id has no sample, the other when a property of a sampled type is written by none of
// its samples. Where the four request kinds refuse each other's fields, the second spec
// takes the union across a type's samples, so a field belongs to whichever kind may
// carry it.
func everyMessage() []any {
	GinkgoHelper()

	yes := true
	no := false

	request := NewRequest("do the thing")
	request.Budget = &Budget{MaxTokens: 1000, MaxIterations: 5, CallTimeout: "60s"}
	request.Context = "the operator is on call"
	request.ToolHints = []string{"nats_server_info"}
	request.Stream = &yes
	request.Deltas = &yes
	request.ConversationToken = NewID()
	request.Force = true

	read, err := NewRead(NewID(), 20)
	Expect(err).ToNot(HaveOccurred())

	toolResult := NewBlock(ToolResultBlock{
		CallID:     "c1",
		ToolResult: ToolResult{Output: "ok", Exec: &ExecResult{Command: "nats server info", ExitCode: 0}},
	})

	result := NewResult(StopEndTurn)
	result.Text = "all done"
	result.Usage = &Usage{InputTokens: 10, OutputTokens: 20}
	result.TraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	result.ContentExported = true

	failure := NewError("it broke")
	failure.Code = CodeCapacity
	failure.StopReason = StopError
	failure.Usage = &Usage{InputTokens: 3, OutputTokens: 1}
	failure.TraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	failure.ContentExported = true

	cancel := NewCancel()
	cancel.Reason = "the operator pressed control-c"

	ack := NewAck(true)
	ack.Reason = "accepted with a worker free"
	ack.ConversationToken = NewID()
	ack.MaxTokens = 200000

	toolReply := NewToolReply("done", true)
	toolReply.Exec = &ExecResult{Command: "nats server info", ExitCode: 0}
	toolReply.Code = CodeCapacity

	discoveryReply := NewDiscoveryReply("agent-a", "1.2.3")
	discoveryReply.Description = "manages nats auth"
	discoveryReply.Model = "claude-sonnet-5"
	discoveryReply.Protocols = []string{ProtocolNamespace}
	discoveryReply.Telemetry = true
	discoveryReply.TelemetryContent = true
	discoveryReply.Tools = []ToolDescriptor{{
		Name:        "nats_server_info",
		Description: "show server info",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Behavior: ToolBehavior{
			ReadOnly:    &yes,
			Destructive: &no,
			Idempotent:  &yes,
			OpenWorld:   &no,
		},
	}}

	approve := NewElicitRequest(ElicitApprove, "q1")
	approve.Command = "stream rm"
	approve.Display = "stream rm ORDERS"
	approve.Tag = "ai:confirm"
	approve.ToolUseID = "toolu_1"
	approve.WaitMS = 90000

	confirmQuestion := NewElicitRequest(ElicitConfirm, "q4")
	confirmQuestion.Question = "remove it?"

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
		NewAnswerRequest(NewID(), &Answer{ToolUseID: "toolu_1", Kind: ElicitSelect, Answer: AnswerValue, Value: "east"}),
		NewResume(NewID()),
		read,
		NewEvent(NewThinkingBlock("hmm")),
		NewEvent(NewTextBlock("answer")),
		NewEvent(NewBlock(PromptBlock{Text: "do the thing"})),
		NewEvent(NewBlock(WarningBlock{Kind: "tool_dropped", Name: "ls", Count: 1, Params: []string{"ai:deny"}, Error: "refused"})),
		NewEvent(NewToolCallBlock("c1", "nats_server_info", json.RawMessage(`{"id":1}`))),
		NewEvent(toolResult),
		NewEvent(NewBlock(AgentCallBlock{ID: "a1", Name: "remote", Task: NewID()})),
		NewEvent(NewBlock(StatusBlock{Iteration: 2, Phase: "calling-llm", Usage: &Usage{InputTokens: 1}})),
		NewEvent(NewBlock(TextDeltaBlock{Index: 1, Iteration: 3, Text: "part", Final: true})),
		NewEvent(NewBlock(ThinkingDeltaBlock{Index: 0, Iteration: 3, Text: "hmm", Final: true})),
		result,
		failure,
		cancel,
		ack,
		NewToolRequest("nats_server_info", json.RawMessage(`{"id":1}`)),
		toolReply,
		NewDiscoveryRequest(),
		discoveryReply,
		approve,
		confirmQuestion,
		selectQuestion,
		input,
		choice,
		NewElicitReply("q4", AnswerConfirmed),
		NewElicitReply("q2", AnswerIndex),
		value,
		NewElicitReply("q1", AnswerNoOperator),
		NewElicitReply("q1", AnswerWaiting),
	}

	for _, msg := range messages {
		fillMessageHeader(msg)
	}

	return messages
}

// fillWholeHeader sets every header field, including the four a message is valid
// without. everyMessage uses it rather than fillHeader so that a sample writes each of
// them, which is what lets the drift spec see a header tag renamed.
func fillWholeHeader(h *Header) {
	fillHeader(h)

	h.Parent = NewID()
	h.Recipient = &Identity{Name: "agent-b"}
	h.MustUnderstand = true
	h.TraceParent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
}

// fillMessageHeader fills the header of whichever message type this is. The switch is
// the price of a slice of any; a message type added without a case fails the spec
// rather than validating an empty header.
func fillMessageHeader(msg any) {
	GinkgoHelper()

	switch m := msg.(type) {
	case *Request:
		fillWholeHeader(&m.Header)
	case *Event:
		fillWholeHeader(&m.Header)
	case *Result:
		fillWholeHeader(&m.Header)
	case *ErrorMessage:
		fillWholeHeader(&m.Header)
	case *Cancel:
		fillWholeHeader(&m.Header)
	case *Ack:
		fillWholeHeader(&m.Header)
	case *ToolRequest:
		fillWholeHeader(&m.Header)
	case *ToolReply:
		fillWholeHeader(&m.Header)
	case *DiscoveryRequest:
		fillWholeHeader(&m.Header)
	case *DiscoveryReply:
		fillWholeHeader(&m.Header)
	case *ElicitRequest:
		fillWholeHeader(&m.Header)
	case *ElicitReply:
		fillWholeHeader(&m.Header)
	default:
		Fail("unhandled message type in test")
	}
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
			for _, msg := range everyMessage() {
				Expect(v.ValidateMessage(msg)).To(Succeed(), "%T should validate", msg)
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

		// A kind added since this build has no schema of its own, so the framing is what
		// is left to check: the header, and that a block is there at all.
		It("Should accept an event of a kind it does not name", func() {
			ev := NewEvent(NewTextBlock("hi"))
			fillHeader(&ev.Header)
			body, err := json.Marshal(ev)
			Expect(err).ToNot(HaveOccurred())

			good := tamper(body, func(m map[string]any) {
				m["protocol"] = "io.choria.fisk-ai.v1.event.citation"
				m["block"] = map[string]any{"source": "rfc1", "page": 12}
			})
			Expect(v.Validate(good)).To(Succeed())
		})

		It("Should hold every kind it does name to that kind's own schema", func() {
			// Every kind but status requires a field of its own, so an empty block is what
			// its schema refuses. A status carries only optional fields, and a value of
			// the wrong type is what it has to say no to.
			for _, tc := range []struct {
				protocol string
				block    map[string]any
				bad      map[string]any
			}{
				{EventTextProtocol, map[string]any{"text": "hi", "final": true}, map[string]any{}},
				{EventThinkingProtocol, map[string]any{"text": "reasoning"}, map[string]any{}},
				{EventToolCallProtocol, map[string]any{"id": "c1", "name": "ls"}, map[string]any{"id": "c1"}},
				{EventToolResultProtocol, map[string]any{"call_id": "c1", "output": "out"}, map[string]any{}},
				{EventAgentCallProtocol, map[string]any{"id": "a1", "name": "peer", "task": "t1"}, map[string]any{"id": "a1"}},
				{EventStatusProtocol, map[string]any{"iteration": 2, "phase": "calling-llm"}, map[string]any{"iteration": "two"}},
				{EventWarningProtocol, map[string]any{"kind": "tool_timeout"}, map[string]any{}},
				{EventPromptProtocol, map[string]any{"text": "remove the stream"}, map[string]any{}},
				{EventTextDeltaProtocol, map[string]any{"index": 1, "iteration": 3, "text": "part"}, map[string]any{}},
				{EventThinkingDeltaProtocol, map[string]any{"index": 0, "iteration": 3, "text": "hmm"}, map[string]any{}},
			} {
				ev := NewEvent(NewTextBlock("hi"))
				fillHeader(&ev.Header)
				body, err := json.Marshal(ev)
				Expect(err).ToNot(HaveOccurred())

				good := tamper(body, func(m map[string]any) {
					m["protocol"] = tc.protocol
					m["block"] = tc.block
				})
				Expect(v.Validate(good)).To(Succeed(), tc.protocol)

				bad := tamper(body, func(m map[string]any) {
					m["protocol"] = tc.protocol
					m["block"] = tc.bad
				})
				Expect(v.Validate(bad)).To(HaveOccurred(), tc.protocol)
			}
		})

		// Every one of the ten is a separate file, which is ten chances to leave the
		// header out and stop checking who sent it and in what order.
		It("Should check the header of every kind it names", func() {
			for _, protocol := range []string{
				EventTextProtocol, EventThinkingProtocol, EventToolCallProtocol,
				EventToolResultProtocol, EventAgentCallProtocol, EventStatusProtocol,
				EventWarningProtocol, EventPromptProtocol,
				EventTextDeltaProtocol, EventThinkingDeltaProtocol,
			} {
				ev := NewEvent(NewTextBlock("hi"))
				fillHeader(&ev.Header)
				body, err := json.Marshal(ev)
				Expect(err).ToNot(HaveOccurred())

				// A block every one of them accepts, so the missing header id is the only
				// thing left to fail on.
				bad := tamper(body, func(m map[string]any) {
					m["protocol"] = protocol
					m["block"] = map[string]any{"text": "hi", "kind": "k", "call_id": "c1", "id": "i", "name": "n", "task": "t", "index": 0}
					delete(m, "id")
				})
				Expect(v.Validate(bad)).To(HaveOccurred(), protocol)
			}
		})

		// The two fragments have one shape between them, so only the id tells them apart.
		// Each side refuses what it requires and the other has no reason to send: a
		// fragment requires an index, a whole block requires its text.
		It("Should refuse a whole block under a fragment's id and a fragment under a whole block's", func() {
			for _, tc := range []struct {
				protocol string
				block    map[string]any
			}{
				{EventTextDeltaProtocol, map[string]any{"text": "answer", "final": true}},
				{EventThinkingDeltaProtocol, map[string]any{"text": "reasoning", "signature": "sig"}},
				{EventTextProtocol, map[string]any{"index": 1, "iteration": 3, "final": true}},
				{EventThinkingProtocol, map[string]any{"index": 1, "iteration": 3, "final": true}},
			} {
				ev := NewEvent(NewTextBlock("hi"))
				fillHeader(&ev.Header)
				body, err := json.Marshal(ev)
				Expect(err).ToNot(HaveOccurred())

				bad := tamper(body, func(m map[string]any) {
					m["protocol"] = tc.protocol
					m["block"] = tc.block
				})
				Expect(v.Validate(bad)).To(HaveOccurred(), tc.protocol)
			}
		})

		// An event id with no entry in protocolSchemaFile falls back to the framing
		// schema, which checks only that the block is an object, so the block's own
		// schema is never applied and nothing fails. A missing file fails loudly in
		// NewValidator; this is the half that fails nothing.
		It("Should map every event schema to a protocol id", func() {
			entries, err := fs.ReadDir(schemaFS, schemaDir)
			Expect(err).ToNot(HaveOccurred())

			mapped := map[string]bool{}
			for _, file := range protocolSchemaFile {
				mapped[file] = true
			}

			for _, entry := range entries {
				name := entry.Name()
				// event.json is the fallback itself, compiled by name rather than mapped.
				if !strings.HasPrefix(name, "event.") || name == eventFallbackSchemaFile {
					continue
				}

				Expect(mapped).To(HaveKey(name), "%s validates nothing until a protocol id names it", name)
			}
		})

		// No request schema closes its properties, so validating a body proves nothing
		// about where the property is declared. A peer reads the schemas as the wire
		// contract, so this spec reads the declarations off the files: deltas goes where
		// stream goes, on the three requests that run a model, and nowhere else.
		It("Should declare deltas on every request that runs a model and on no other", func() {
			for file, wanted := range map[string]bool{
				"request.prompt.json": true,
				"request.answer.json": true,
				"request.resume.json": true,
				"request.read.json":   false,
			} {
				raw, err := schemaFS.ReadFile(schemaDir + "/" + file)
				Expect(err).ToNot(HaveOccurred())

				var schema struct {
					Properties map[string]any `json:"properties"`
				}

				err = json.Unmarshal(raw, &schema)
				Expect(err).ToNot(HaveOccurred())

				_, streams := schema.Properties["stream"]
				Expect(streams).To(Equal(wanted), "%s should declare stream: %v", file, wanted)

				_, deltas := schema.Properties["deltas"]
				Expect(deltas).To(Equal(wanted), "%s should declare deltas: %v", file, wanted)
			}
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
			answers := map[ElicitAnswer]func(*ElicitReply){
				AnswerChoice:     func(r *ElicitReply) { r.Choice = ChoiceOnce },
				AnswerConfirmed:  func(r *ElicitReply) { r.Confirmed = true },
				AnswerIndex:      func(r *ElicitReply) { r.Index = 1 },
				AnswerValue:      func(r *ElicitReply) { r.Value = "orders.new" },
				AnswerNoOperator: func(*ElicitReply) {},
				AnswerWaiting:    func(*ElicitReply) {},
			}

			for answer, fill := range answers {
				reply := NewElicitReply("q1", answer)
				fill(reply)
				fillHeader(&reply.Header)
				Expect(v.ValidateMessage(reply)).To(Succeed(), "answer %q", answer)
			}

			// A confirmation of no, a selection of the first option and an empty input
			// are answers somebody gave, so each reaches the wire and satisfies the
			// required field rather than being omitted for being empty.
			for _, answer := range []ElicitAnswer{AnswerConfirmed, AnswerIndex, AnswerValue} {
				zero := NewElicitReply("q1", answer)
				fillHeader(&zero.Header)

				body, err := json.Marshal(zero)
				Expect(err).ToNot(HaveOccurred())
				Expect(body).To(ContainSubstring(map[ElicitAnswer]string{
					AnswerConfirmed: `"confirmed":false`,
					AnswerIndex:     `"index":0`,
					AnswerValue:     `"value":""`,
				}[answer]))
				Expect(v.Validate(body)).To(Succeed(), "answer %q carrying its zero value", answer)
			}

			// An approval names one of three, so the value a caller never set is the
			// one zero that is no answer at all.
			bare := NewElicitReply("q1", AnswerChoice)
			fillHeader(&bare.Header)
			Expect(v.ValidateMessage(bare)).ToNot(Succeed())

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

		// The window is what a caller paces its acks against, so it travels on the
		// question rather than being agreed in advance.
		It("Should carry the window a question is held open for", func() {
			ask := NewElicitRequest(ElicitConfirm, "q1")
			ask.Question = "remove it?"
			ask.WaitMS = 120000
			fillHeader(&ask.Header)
			Expect(v.ValidateMessage(ask)).To(Succeed())
			Expect(ask.AckInterval()).To(Equal(40 * time.Second))

			negative := NewElicitRequest(ElicitConfirm, "q1")
			negative.Question = "remove it?"
			negative.WaitMS = -1
			fillHeader(&negative.Header)
			Expect(v.ValidateMessage(negative)).ToNot(Succeed())

			// An agent that predates the window takes no acks, which is what a caller
			// reads an absent one as.
			Expect(NewElicitRequest(ElicitConfirm, "q1").AckInterval()).To(BeZero())
		})

		// A question outlives its run, so an answer to one has to say what it answers
		// without the receiver consulting anything it kept.
		It("Should carry an answer to a question whose run has ended", func() {
			ask := NewElicitRequest(ElicitSelect, "q1")
			ask.ToolUseID = "toolu_1"
			ask.Question = "which cluster?"
			ask.Options = []string{"east", "west"}
			fillHeader(&ask.Header)
			Expect(v.ValidateMessage(ask)).To(Succeed())

			req, err := NewAnsweringRequest("token1", ask, NewSelectReply(ask, "caller1", 1))
			Expect(err).ToNot(HaveOccurred())
			fillHeader(&req.Header)

			Expect(req.Prompt).To(BeEmpty(), "an answer is not a prompt")
			Expect(req.ConversationToken).To(Equal("token1"))
			Expect(req.Answer.ToolUseID).To(Equal("toolu_1"))
			Expect(req.Answer.Kind).To(Equal(ElicitSelect))
			Expect(req.Answer.Answer).To(Equal(AnswerValue))
			Expect(req.Answer.Value).To(Equal("west"), "the option chosen, not where it sat in a list the receiver no longer holds")
			Expect(v.ValidateMessage(req)).To(Succeed())

			_, err = NewAnsweringRequest("token1", ask, NewSelectReply(ask, "caller1", 7))
			Expect(err).To(MatchError(ContainSubstring("not one of the 2")))
		})

		// An endpoint decides whether the prompt it carries is one it will run, and says
		// so in its own words. What the schema settles is that one of the two is there
		// at all, so a message naming neither is refused before anybody reads it.
		It("Should refuse a request naming neither a prompt nor an answer", func() {
			bare := []byte(`{"protocol":"io.choria.fisk-ai.v1.request","id":"i1","request":"r1","conversation":"c1","sequence":0,"time":"2026-08-16T11:24:10Z","sender":{"name":"caller1"}}`)
			Expect(v.Validate(bare)).ToNot(Succeed())
		})

		It("Should correlate a waiting ack to the question it holds open", func() {
			ask := NewElicitRequest(ElicitApprove, "q1")
			fillHeader(&ask.Header)

			held := NewWaitingAck(ask, "caller1")
			Expect(held.Answer).To(Equal(AnswerWaiting))
			Expect(held.QuestionID).To(Equal("q1"))
			Expect(held.Request).To(Equal(ask.Request))
			Expect(held.Sender.Name).To(Equal("caller1"))
			Expect(v.ValidateMessage(held)).To(Succeed())
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

		// A receiver displays the model, so the card cannot carry an unbounded one.
		It("Should reject a model longer than a card may carry", func() {
			reply := NewDiscoveryReply("agent-a", "1.2.3")
			fillHeader(&reply.Header)
			reply.Model = "claude-sonnet-5"

			data, err := json.Marshal(reply)
			Expect(err).ToNot(HaveOccurred())
			Expect(v.Validate(data)).To(Succeed())

			bad := tamper(data, func(m map[string]any) {
				m["model"] = strings.Repeat("m", 129)
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

		// The id says what the block is, so an id that names nothing usable leaves the
		// message unreadable however well formed the rest of it is.
		It("Should reject an event whose id names no kind", func() {
			ev := NewEvent(NewTextBlock("hi"))
			fillHeader(&ev.Header)
			body, err := json.Marshal(ev)
			Expect(err).ToNot(HaveOccurred())

			for _, protocol := range []any{
				"io.choria.fisk-ai.v1.event",
				"io.choria.fisk-ai.v1.event.",
				"io.choria.fisk-ai.v1.event.text.evil",
				"io.choria.fisk-ai.v1.citation",
				nil,
				7,
			} {
				bad := tamper(body, func(m map[string]any) {
					m["protocol"] = protocol
				})
				Expect(v.Validate(bad)).To(HaveOccurred(), "%v", protocol)
			}
		})

		It("Should reject an event carrying no block", func() {
			ev := NewEvent(NewTextBlock("hi"))
			fillHeader(&ev.Header)
			body, err := json.Marshal(ev)
			Expect(err).ToNot(HaveOccurred())

			for _, protocol := range []string{EventTextProtocol, "io.choria.fisk-ai.v1.event.citation"} {
				bad := tamper(body, func(m map[string]any) {
					m["protocol"] = protocol
					delete(m, "block")
				})
				Expect(v.Validate(bad)).To(HaveOccurred(), protocol)
			}
		})
	})
})
