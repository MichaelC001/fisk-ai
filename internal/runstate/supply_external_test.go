//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package runstate_test

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/segmentio/ksuid"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
	runstatefile "github.com/choria-io/fisk-ai/internal/runstate/file"
)

var _ = Describe("SupplyToolResult", func() {
	var (
		store runstate.Store
		id    string
		ctx   = context.Background()
	)

	assistantTurn := func(ids ...string) *runstate.AssistantRecord {
		content := []llm.ContentBlock{{Text: &llm.TextBlock{Text: "working"}}}
		for _, id := range ids {
			content = append(content, llm.ContentBlock{ToolUse: &llm.ToolUseBlock{ID: id, Name: "change_request", Input: json.RawMessage(`{}`)}})
		}

		return &runstate.AssistantRecord{Message: llm.Message{Role: llm.RoleAssistant, Content: content}}
	}

	// A journal holding one answered call and one still waiting, which is the shape
	// every case below is tested against.
	BeforeEach(func() {
		var err error
		store, err = runstatefile.NewFileStore(GinkgoT().TempDir())
		Expect(err).ToNot(HaveOccurred())

		id = ksuid.New().String()
		j, err := store.Create(ctx, id, runstate.MetaRecord{Version: runstate.Version, RunID: id, Prompt: "raise a change"})
		Expect(err).ToNot(HaveOccurred())

		Expect(j.Append(ctx, 2, runstate.Record{Seq: 2, Protocol: runstate.AssistantProtocol, Assistant: assistantTurn("tu_1", "tu_2")})).To(Succeed())
		Expect(j.Append(ctx, 3, runstate.Record{Seq: 3, Protocol: runstate.ToolResultProtocol, ToolResult: &runstate.ToolResultRecord{
			ToolUseID: "tu_1", Result: llm.ToolResultBlock{ToolUseID: "tu_1", Content: "filed"},
		}})).To(Succeed())
		Expect(j.Append(ctx, 4, runstate.Record{Seq: 4, Protocol: runstate.DeferredProtocol, Deferred: &runstate.DeferredRecord{
			ToolUseID: "tu_2", ToolName: "change_request", Note: "waiting on approval", Handle: "CHG-1",
		}})).To(Succeed())
		Expect(j.Append(ctx, 5, runstate.Record{Seq: 5, Protocol: runstate.TerminalProtocol, Terminal: &runstate.TerminalRecord{Reason: runstate.ReasonSuspended}})).To(Succeed())
		Expect(j.Close()).To(Succeed())
	})

	// The answer completes the turn, which is what makes the next resume an ordinary
	// resume: the loop reuses both results and dispatches neither tool again.
	It("Should answer an outstanding deferral and complete the turn", func() {
		Expect(runstate.SupplyToolResult(ctx, store, id, "tu_2", `{"approved":true}`, false)).To(Succeed())

		rs, err := store.Load(ctx, id)
		Expect(err).ToNot(HaveOccurred())
		Expect(rs.Pending).To(BeNil())

		last := rs.Messages[len(rs.Messages)-1]
		Expect(last.Role).To(Equal(llm.RoleUser))
		Expect(last.Content).To(HaveLen(2))
	})

	// The run holds its journal already, so it answers through the same rule rather
	// than opening the store a second time, and the state it is about to run against
	// has to agree with what was written.
	It("Should answer through a journal the caller already holds and fold it in", func() {
		rs, err := store.Load(ctx, id)
		Expect(err).ToNot(HaveOccurred())
		Expect(rs.Pending.Answered).ToNot(HaveKey("tu_2"))

		j, err := store.Open(ctx, id)
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(j.Close)

		Expect(runstate.AnswerDeferredCall(ctx, j, rs, "tu_2", `{"approved":true}`, false)).To(Succeed())

		Expect(rs.Pending.Answered).To(HaveKeyWithValue("tu_2", true), "the caller's state says the call is answered")
		Expect(rs.Pending.Results).To(ContainElement(llm.ToolResultBlock{ToolUseID: "tu_2", Content: `{"approved":true}`}))

		reloaded, err := store.Load(ctx, id)
		Expect(err).ToNot(HaveOccurred())
		Expect(reloaded.Pending).To(BeNil(), "and the journal says the turn is complete")
	})

	It("Should refuse through a held journal on the same terms", func() {
		rs, err := store.Load(ctx, id)
		Expect(err).ToNot(HaveOccurred())

		j, err := store.Open(ctx, id)
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(j.Close)

		Expect(runstate.AnswerDeferredCall(ctx, j, rs, "tu_2", strings.Repeat("a", runstate.MaxSuppliedResultBytes+1), false)).To(MatchError(runstate.ErrResultTooLarge))
		Expect(runstate.AnswerDeferredCall(ctx, j, rs, "tu_1", "late", false)).To(MatchError(runstate.ErrNotDeferred), "the tool answered this one itself")
		Expect(runstate.AnswerDeferredCall(ctx, j, rs, "tu_9", "late", false)).To(MatchError(runstate.ErrNotDeferred), "and this call is not in the turn at all")

		Expect(runstate.AnswerDeferredCall(ctx, j, rs, "tu_2", "first", false)).To(Succeed())
		Expect(runstate.AnswerDeferredCall(ctx, j, rs, "tu_2", "second", false)).To(MatchError(runstate.ErrAlreadyAnswered))
	})

	It("Should mark an answer as an error when asked to", func() {
		Expect(runstate.SupplyToolResult(ctx, store, id, "tu_2", "the request was rejected", true)).To(Succeed())

		rs, err := store.Load(ctx, id)
		Expect(err).ToNot(HaveOccurred())

		last := rs.Messages[len(rs.Messages)-1]
		Expect(last.Content[1].ToolResult.IsError).To(BeTrue())
	})

	It("Should refuse a call the run never deferred", func() {
		Expect(runstate.SupplyToolResult(ctx, store, id, "tu_1", "late", false)).To(MatchError(runstate.ErrNotDeferred))
	})

	It("Should refuse an id the turn does not carry at all", func() {
		Expect(runstate.SupplyToolResult(ctx, store, id, "tu_nope", "late", false)).To(MatchError(runstate.ErrNotDeferred))
	})

	// Answering twice would leave the turn carrying two results for one tool_use,
	// which the model API rejects and the fold cannot repair.
	It("Should refuse a deferral that already has an answer", func() {
		Expect(runstate.SupplyToolResult(ctx, store, id, "tu_2", "first", false)).To(Succeed())
		Expect(runstate.SupplyToolResult(ctx, store, id, "tu_2", "second", false)).To(MatchError(runstate.ErrAlreadyAnswered))
	})

	It("Should refuse an answer larger than the cap", func() {
		big := strings.Repeat("x", runstate.MaxSuppliedResultBytes+1)
		Expect(runstate.SupplyToolResult(ctx, store, id, "tu_2", big, false)).To(MatchError(runstate.ErrResultTooLarge))
	})

	It("Should refuse a run with nothing in flight", func() {
		other := ksuid.New().String()
		j, err := store.Create(ctx, other, runstate.MetaRecord{Version: runstate.Version, RunID: other, Prompt: "nothing"})
		Expect(err).ToNot(HaveOccurred())
		Expect(j.Close()).To(Succeed())

		Expect(runstate.SupplyToolResult(ctx, store, other, "tu_1", "late", false)).To(MatchError(runstate.ErrNotDeferred))
	})

	It("Should refuse an unknown session", func() {
		Expect(runstate.SupplyToolResult(ctx, store, ksuid.New().String(), "tu_2", "late", false)).To(MatchError(runstate.ErrNotFound))
	})

	// An answer arriving while another process holds the run loses rather than racing
	// it: the answer can be supplied again once that worker is done, and overwriting
	// its work cannot be undone.
	It("Should refuse while another writer holds the run", func() {
		held, err := store.Open(ctx, id)
		Expect(err).ToNot(HaveOccurred())
		defer held.Close()

		Expect(runstate.SupplyToolResult(ctx, store, id, "tu_2", "late", false)).To(MatchError(runstate.ErrLocked))
	})
})
