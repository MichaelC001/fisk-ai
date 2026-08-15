//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package runstate

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func deferredRecord(id string) *DeferredRecord {
	return &DeferredRecord{ToolUseID: id, ToolName: "file_change_request", Note: "waiting on approval", Handle: "CHG-1"}
}

var _ = Describe("deferred calls", func() {
	metaRecord := func(id string) Record {
		return Record{Seq: 1, Protocol: MetaProtocol, Meta: &MetaRecord{
			Version: Version, RunID: id, Prompt: "raise a change",
			Fingerprint: Fingerprint{Model: "claude-opus-4-8"},
		}}
	}

	Describe("Fold", func() {
		// A deferred call is unanswered, which is what keeps the turn pending: the
		// answer has to land in this same turn for the conversation to stay coherent.
		It("Should leave a deferred call pending and unanswered", func() {
			rs, err := Fold([]Record{
				metaRecord(newID()),
				{Seq: 2, Protocol: AssistantProtocol, Assistant: assistantWithTools(0, "tu_1", "tu_2")},
				{Seq: 3, Protocol: ToolResultProtocol, ToolResult: toolResult("tu_1")},
				{Seq: 4, Protocol: DeferredProtocol, Deferred: deferredRecord("tu_2")},
				{Seq: 5, Protocol: TerminalProtocol, Terminal: &TerminalRecord{Reason: ReasonSuspended}},
			})
			Expect(err).ToNot(HaveOccurred())

			Expect(rs.Pending).ToNot(BeNil())
			Expect(rs.Pending.Answered).To(HaveKeyWithValue("tu_1", true))
			Expect(rs.Pending.Answered).ToNot(HaveKey("tu_2"))
			Expect(rs.Pending.Deferred).To(HaveKey("tu_2"))

			open := rs.Pending.OpenDeferrals()
			Expect(open).To(HaveLen(1))
			Expect(open[0].ToolName).To(Equal("file_change_request"))
			Expect(open[0].Handle).To(Equal("CHG-1"))
		})

		// The answer is an ordinary tool_result, so the fold needs no special case for
		// it and the resume that follows is an ordinary resume.
		It("Should mark a deferred call answered once its result arrives", func() {
			rs, err := Fold([]Record{
				metaRecord(newID()),
				{Seq: 2, Protocol: AssistantProtocol, Assistant: assistantWithTools(0, "tu_1")},
				{Seq: 3, Protocol: DeferredProtocol, Deferred: deferredRecord("tu_1")},
				{Seq: 4, Protocol: TerminalProtocol, Terminal: &TerminalRecord{Reason: ReasonSuspended}},
				{Seq: 5, Protocol: ToolResultProtocol, ToolResult: toolResult("tu_1")},
			})
			Expect(err).ToNot(HaveOccurred())

			// The turn is complete, so it commits rather than staying pending.
			Expect(rs.Pending).To(BeNil())
			Expect(rs.Counters.ToolCalls).To(Equal(int64(1)))
		})

		// A deferral is not a result, so counting it would double once the answer lands.
		It("Should count no tool call for a deferral on its own", func() {
			rs, err := Fold([]Record{
				metaRecord(newID()),
				{Seq: 2, Protocol: AssistantProtocol, Assistant: assistantWithTools(0, "tu_1")},
				{Seq: 3, Protocol: DeferredProtocol, Deferred: deferredRecord("tu_1")},
			})
			Expect(err).ToNot(HaveOccurred())
			Expect(rs.Counters.ToolCalls).To(BeZero())
		})

		It("Should reject a deferred record with no payload", func() {
			_, err := Fold([]Record{
				metaRecord(newID()),
				{Seq: 2, Protocol: AssistantProtocol, Assistant: assistantWithTools(0, "tu_1")},
				{Seq: 3, Protocol: DeferredProtocol},
			})
			Expect(err).To(MatchError(ErrCorrupt))
		})

		It("Should reject a deferred record before any assistant turn", func() {
			_, err := Fold([]Record{
				metaRecord(newID()),
				{Seq: 2, Protocol: DeferredProtocol, Deferred: deferredRecord("tu_1")},
			})
			Expect(err).To(MatchError(ErrCorrupt))
		})
	})

	Describe("OpenDeferrals", func() {
		It("Should omit a deferral whose answer has arrived", func() {
			p := &PendingTurn{
				Answered: map[string]bool{"tu_1": true},
				Deferred: map[string]DeferredRecord{
					"tu_1": *deferredRecord("tu_1"),
					"tu_2": *deferredRecord("tu_2"),
				},
			}

			open := p.OpenDeferrals()
			Expect(open).To(HaveLen(1))
			Expect(open[0].ToolUseID).To(Equal("tu_2"))
		})

		It("Should report nothing for a turn that is merely mid-batch", func() {
			p := &PendingTurn{Answered: map[string]bool{"tu_1": true}}
			Expect(p.OpenDeferrals()).To(BeEmpty())
		})
	})
})
