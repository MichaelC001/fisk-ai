//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package runstate

import (
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/llm"
)

var _ = Describe("Validator", func() {
	var v *Validator

	BeforeEach(func() {
		var err error
		v, err = NewValidator()
		Expect(err).ToNot(HaveOccurred())
	})

	Describe("valid records", func() {
		It("Should accept every record type produced by a run", func() {
			for _, rec := range everyRecordType() {
				Expect(v.ValidateRecord(rec)).To(Succeed(), "protocol %s", rec.Protocol)
			}
		})

		It("Should accept a terminal record with no message", func() {
			Expect(v.ValidateRecord(Record{Seq: 2, Protocol: TerminalProtocol, Terminal: &TerminalRecord{Reason: ReasonSuspended}})).To(Succeed())
		})
	})

	Describe("invalid records", func() {
		It("Should reject an unknown protocol id", func() {
			data, err := json.Marshal(Record{Seq: 2, Protocol: "io.choria.fisk-ai.v1.session.bogus", Terminal: &TerminalRecord{Reason: ReasonCompleted}})
			Expect(err).ToNot(HaveOccurred())
			Expect(v.Validate(data)).To(MatchError(ErrNoSchema))
		})

		It("Should reject a record whose payload does not match its protocol", func() {
			data := tamperRecord(metaRecord(), func(m map[string]any) {
				delete(m, "meta")
			})
			Expect(v.Validate(data)).ToNot(Succeed())
		})

		It("Should reject a meta record missing a required field", func() {
			data := tamperRecord(metaRecord(), func(m map[string]any) {
				delete(m["meta"].(map[string]any), "run_id")
			})
			Expect(v.Validate(data)).ToNot(Succeed())
		})

		It("Should reject a stray payload key for the protocol", func() {
			data := tamperRecord(metaRecord(), func(m map[string]any) {
				m["terminal"] = map[string]any{"reason": "completed"}
			})
			Expect(v.Validate(data)).ToNot(Succeed())
		})

		It("Should reject a terminal record with an unknown reason", func() {
			data := tamperRecord(Record{Seq: 2, Protocol: TerminalProtocol, Terminal: &TerminalRecord{Reason: ReasonCompleted}}, func(m map[string]any) {
				m["terminal"].(map[string]any)["reason"] = "exploded"
			})
			Expect(v.Validate(data)).ToNot(Succeed())
		})
	})

	// A field is added to a record body without a version bump, and a build that
	// predates it folds the record with that field zero. The bodies are open so a
	// validator holding an earlier copy of these schemas agrees with that rather than
	// calling the record invalid. The record itself stays closed, because a second body
	// or a misspelled body key is a fault rather than an addition.
	Describe("fields added after a reader was built", func() {
		It("Should accept an unknown field in every record body", func() {
			for _, rec := range everyRecordType() {
				data := tamperRecord(rec, func(m map[string]any) {
					m[bodyKey(m)].(map[string]any)["a_field_from_a_later_build"] = "value"
				})
				Expect(v.Validate(data)).To(Succeed(), "protocol %s", rec.Protocol)
			}
		})

		It("Should accept an unknown field in the fingerprint", func() {
			data := tamperRecord(metaRecord(), func(m map[string]any) {
				m["meta"].(map[string]any)["fingerprint"].(map[string]any)["a_field_from_a_later_build"] = "value"
			})
			Expect(v.Validate(data)).To(Succeed())
		})

		It("Should still reject an unknown key on the record itself", func() {
			data := tamperRecord(metaRecord(), func(m map[string]any) {
				m["a_key_from_a_later_build"] = "value"
			})
			Expect(v.Validate(data)).ToNot(Succeed())
		})
	})
})

// metaRecord is a fully populated meta record, the one every tamper case starts from.
func metaRecord() Record {
	return Record{
		Seq:      1,
		Protocol: MetaProtocol,
		Meta: &MetaRecord{
			Version:           Version,
			RunID:             "run-1",
			Created:           time.Now().UTC(),
			Fingerprint:       Fingerprint{Model: "claude-opus-4-8", SystemHash: "abc", ToolsHash: "def", ThinkingMode: "on", MaxTokens: 1000, MaxIterations: 5},
			Prompt:            "do the thing",
			ConversationToken: "3Hzmp8VqrKL42NmXcPd7bTgWfR1",
			Caller:            "peer1",
		},
	}
}

// everyRecordType is one populated record per protocol id, so a case that has to hold
// for all of them is written once and gains the next record type when this list does.
func everyRecordType() []Record {
	return []Record{
		metaRecord(),
		{Seq: 2, Protocol: AssistantProtocol, Assistant: assistantWithTools(0, "tu_1")},
		{Seq: 3, Protocol: ToolResultProtocol, ToolResult: toolResult("tu_1")},
		{Seq: 4, Protocol: DeferredProtocol, Deferred: &DeferredRecord{ToolUseID: "tu_2", ToolName: "change_request", Note: "waiting on change approval", Handle: "CHG-1234"}},
		{Seq: 5, Protocol: UserProtocol, User: userRecord("a follow-up")},
		{Seq: 6, Protocol: ClaimProtocol, Claim: &ClaimRecord{By: "agent@host pid 42", Claimed: time.Now().UTC()}},
		{Seq: 7, Protocol: DecisionProtocol, Optional: true, Decision: &DecisionRecord{Tool: "stream_rm"}},
		{Seq: 8, Protocol: CallApprovalProtocol, Optional: true, CallApproval: &CallApprovalRecord{ToolUseID: "tu_3", ToolName: "stream_rm"}},
		{Seq: 9, Protocol: TerminalProtocol, Terminal: &TerminalRecord{Reason: ReasonCompleted, Message: "done"}},
		{Seq: 10, Protocol: MemoryRevisionsProtocol, Optional: true, MemoryRevisions: &MemoryRevisionsRecord{Revisions: map[string]uint64{"team.notes": 7}}},
	}
}

// bodyKey is the one top-level key holding a record's body, the three envelope keys
// aside. It is derived rather than tabulated so a new record type needs no entry.
func bodyKey(m map[string]any) string {
	for k := range m {
		switch k {
		case "protocol", "seq", "optional":
		default:
			return k
		}
	}

	Fail("record has no body key")

	return ""
}

// tamperRecord round-trips a record through a map so a test can mutate it before
// re-validating.
func tamperRecord(rec Record, mut func(map[string]any)) []byte {
	data, err := json.Marshal(rec)
	Expect(err).ToNot(HaveOccurred())

	var m map[string]any
	Expect(json.Unmarshal(data, &m)).To(Succeed())
	mut(m)

	out, err := json.Marshal(m)
	Expect(err).ToNot(HaveOccurred())

	return out
}

var _ = Describe("assistant record message", func() {
	It("Should validate an assistant message stored verbatim", func() {
		v, err := NewValidator()
		Expect(err).ToNot(HaveOccurred())

		asst := &AssistantRecord{
			Iteration: 0,
			Message:   llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Text: &llm.TextBlock{Text: "hello"}}}},
			InTokens:  1,
			OutTokens: 2,
		}
		Expect(v.ValidateRecord(Record{Seq: 2, Protocol: AssistantProtocol, Assistant: asst})).To(Succeed())
	})

	It("Should validate an assistant record carrying the cache token split", func() {
		v, err := NewValidator()
		Expect(err).ToNot(HaveOccurred())

		asst := &AssistantRecord{
			Iteration:         0,
			Message:           llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Text: &llm.TextBlock{Text: "hello"}}}},
			InTokens:          1,
			OutTokens:         2,
			CacheReadTokens:   100,
			CacheCreateTokens: 40,
		}
		Expect(v.ValidateRecord(Record{Seq: 2, Protocol: AssistantProtocol, Assistant: asst})).To(Succeed())
	})
})
