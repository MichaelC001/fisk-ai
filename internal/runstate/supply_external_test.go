//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package runstate_test

import (
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
		j, err := store.Create(id, runstate.MetaRecord{Version: runstate.Version, RunID: id, Prompt: "raise a change"})
		Expect(err).ToNot(HaveOccurred())

		Expect(j.Append(2, runstate.Record{Seq: 2, Protocol: runstate.AssistantProtocol, Assistant: assistantTurn("tu_1", "tu_2")})).To(Succeed())
		Expect(j.Append(3, runstate.Record{Seq: 3, Protocol: runstate.ToolResultProtocol, ToolResult: &runstate.ToolResultRecord{
			ToolUseID: "tu_1", Result: llm.ToolResultBlock{ToolUseID: "tu_1", Content: "filed"},
		}})).To(Succeed())
		Expect(j.Append(4, runstate.Record{Seq: 4, Protocol: runstate.DeferredProtocol, Deferred: &runstate.DeferredRecord{
			ToolUseID: "tu_2", ToolName: "change_request", Note: "waiting on approval", Handle: "CHG-1",
		}})).To(Succeed())
		Expect(j.Append(5, runstate.Record{Seq: 5, Protocol: runstate.TerminalProtocol, Terminal: &runstate.TerminalRecord{Reason: runstate.ReasonSuspended}})).To(Succeed())
		Expect(j.Close()).To(Succeed())
	})

	// The answer completes the turn, which is what makes the next resume an ordinary
	// resume: the loop reuses both results and dispatches neither tool again.
	It("Should answer an outstanding deferral and complete the turn", func() {
		Expect(runstate.SupplyToolResult(store, id, "tu_2", `{"approved":true}`, false)).To(Succeed())

		rs, err := store.Load(id)
		Expect(err).ToNot(HaveOccurred())
		Expect(rs.Pending).To(BeNil())

		last := rs.Messages[len(rs.Messages)-1]
		Expect(last.Role).To(Equal(llm.RoleUser))
		Expect(last.Content).To(HaveLen(2))
	})

	It("Should mark an answer as an error when asked to", func() {
		Expect(runstate.SupplyToolResult(store, id, "tu_2", "the request was rejected", true)).To(Succeed())

		rs, err := store.Load(id)
		Expect(err).ToNot(HaveOccurred())

		last := rs.Messages[len(rs.Messages)-1]
		Expect(last.Content[1].ToolResult.IsError).To(BeTrue())
	})

	It("Should refuse a call the run never deferred", func() {
		Expect(runstate.SupplyToolResult(store, id, "tu_1", "late", false)).To(MatchError(runstate.ErrNotDeferred))
	})

	It("Should refuse an id the turn does not carry at all", func() {
		Expect(runstate.SupplyToolResult(store, id, "tu_nope", "late", false)).To(MatchError(runstate.ErrNotDeferred))
	})

	// Answering twice would leave the turn carrying two results for one tool_use,
	// which the model API rejects and the fold cannot repair.
	It("Should refuse a deferral that already has an answer", func() {
		Expect(runstate.SupplyToolResult(store, id, "tu_2", "first", false)).To(Succeed())
		Expect(runstate.SupplyToolResult(store, id, "tu_2", "second", false)).To(MatchError(runstate.ErrAlreadyAnswered))
	})

	It("Should refuse an answer larger than the cap", func() {
		big := strings.Repeat("x", runstate.MaxSuppliedResultBytes+1)
		Expect(runstate.SupplyToolResult(store, id, "tu_2", big, false)).To(MatchError(runstate.ErrResultTooLarge))
	})

	It("Should refuse a run with nothing in flight", func() {
		other := ksuid.New().String()
		j, err := store.Create(other, runstate.MetaRecord{Version: runstate.Version, RunID: other, Prompt: "nothing"})
		Expect(err).ToNot(HaveOccurred())
		Expect(j.Close()).To(Succeed())

		Expect(runstate.SupplyToolResult(store, other, "tu_1", "late", false)).To(MatchError(runstate.ErrNotDeferred))
	})

	It("Should refuse an unknown session", func() {
		Expect(runstate.SupplyToolResult(store, ksuid.New().String(), "tu_2", "late", false)).To(MatchError(runstate.ErrNotFound))
	})

	// An answer arriving while another process holds the run loses rather than racing
	// it: the answer can be supplied again once that worker is done, and overwriting
	// its work cannot be undone.
	It("Should refuse while another writer holds the run", func() {
		held, err := store.Open(id)
		Expect(err).ToNot(HaveOccurred())
		defer held.Close()

		Expect(runstate.SupplyToolResult(store, id, "tu_2", "late", false)).To(MatchError(runstate.ErrLocked))
	})
})
