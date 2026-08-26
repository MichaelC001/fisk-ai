//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// These specs drive the memory scope across the turns of one conversation through the
// exported agent.Run API: what a run records when it ends, and what the next turn of
// that conversation starts with.
package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/memory"
	"github.com/choria-io/fisk-ai/internal/runstate"
	runstatefile "github.com/choria-io/fisk-ai/internal/runstate/file"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/toolkit/functool"
)

// scopeTools are a pair of custom tools standing in for a memory backend that gates an
// overwrite on a read: one records a revision on the run's scope the way Read does, the
// other reports what the scope knows the way overwrite consults it. A real backend is
// reached only over a network, and what is under test is what the agent carries between
// runs rather than what any backend does with it.
func scopeTools(tb testing.TB) []toolkit.Tool {
	tb.Helper()

	read, err := functool.New(functool.Spec{
		Name:        "scope_read",
		Description: "reads a memory and records its revision",
		Schema:      map[string]any{"type": "object"},
		Handler: func(ctx context.Context, _ json.RawMessage, _ *functool.CallContext) (string, error) {
			memory.ScopeFrom(ctx).Remember("notes", 7)

			return `{"read":true}`, nil
		},
	})
	Expect(err).NotTo(HaveOccurred())

	check, err := functool.New(functool.Spec{
		Name:        "scope_check",
		Description: "reports the revision the scope knows",
		Schema:      map[string]any{"type": "object"},
		Handler: func(ctx context.Context, _ json.RawMessage, _ *functool.CallContext) (string, error) {
			rev, ok := memory.ScopeFrom(ctx).Revision("notes")

			return fmt.Sprintf(`{"known":%t,"revision":%d}`, ok, rev), nil
		},
	})
	Expect(err).NotTo(HaveOccurred())

	return []toolkit.Tool{read, check}
}

var _ = Describe("memory revisions across turns", func() {
	// This is the defect the item exists to fix. A turn is a run, so the scope died with
	// it and the next turn of the same conversation had to read a value it already had
	// before it could write it.
	It("Should carry the revisions to the next turn", func() {
		ctx := context.Background()

		store, err := runstatefile.NewFileStore(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())

		opts := func(provider *agenttest.ScriptedProvider, cp agent.Checkpoint) agent.Options {
			return agent.Options{
				Config:       agenttest.Config(GinkgoTB(), app),
				ConfigFile:   "agent.yaml",
				Prompt:       []string{"edit the note"},
				Provider:     provider,
				SessionStore: store,
				Checkpoint:   cp,
				CustomTools:  scopeTools(GinkgoTB()),
			}
		}

		res1, err := agent.Run(ctx, opts(
			agenttest.NewScriptedProvider(GinkgoTB(),
				agenttest.ToolUseResponse("c1", "scope_read", json.RawMessage(`{}`)),
				agenttest.TextResponse("read it"),
			),
			agent.Checkpoint{Enabled: true},
		), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res1.Reason).To(Equal(runstate.ReasonCompleted))

		// The record is written as the run ends, which is after the terminal record: a
		// memory read on the last tool call of a run counts as much as one on the first.
		records := journalRecords(GinkgoTB(), store, res1.SessionID)
		Expect(recordIndex(records, runstate.MemoryRevisionsProtocol)).To(BeNumerically(">", recordIndex(records, runstate.TerminalProtocol)))

		rs, err := store.Load(res1.SessionID)
		Expect(err).NotTo(HaveOccurred())
		Expect(rs.MemoryRevisions).To(Equal(map[string]uint64{"notes": 7}))

		// The next turn starts knowing what the first one read, so it can overwrite without
		// reading again.
		events := agenttest.NewRecordingEvents()
		res2, err := agent.Run(ctx, opts(
			agenttest.NewScriptedProvider(GinkgoTB(),
				agenttest.ToolUseResponse("c2", "scope_check", json.RawMessage(`{}`)),
				agenttest.TextResponse("wrote it"),
			),
			agent.Checkpoint{ResumeID: res1.SessionID, FollowUp: true},
		), events, agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res2.Reason).To(Equal(runstate.ReasonCompleted))

		results := events.ToolResults()
		Expect(results).ToNot(BeEmpty())
		Expect(results[0].Output).To(ContainSubstring(`"known":true`))
		Expect(results[0].Output).To(ContainSubstring(`"revision":7`))
	})

	// A run that read no memory records nothing, so a conversation that never touches
	// memory carries no record for it.
	It("Should record nothing when no memory was read", func() {
		ctx := context.Background()

		store, err := runstatefile.NewFileStore(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())

		res, err := agent.Run(ctx, agent.Options{
			Config:       agenttest.Config(GinkgoTB(), app),
			ConfigFile:   "agent.yaml",
			Prompt:       []string{"say hello"},
			Provider:     agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("hello")),
			SessionStore: store,
			Checkpoint:   agent.Checkpoint{Enabled: true},
			CustomTools:  scopeTools(GinkgoTB()),
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())

		records := journalRecords(GinkgoTB(), store, res.SessionID)
		Expect(recordIndex(records, runstate.MemoryRevisionsProtocol)).To(Equal(-1))

		rs, err := store.Load(res.SessionID)
		Expect(err).NotTo(HaveOccurred())
		Expect(rs.MemoryRevisions).To(BeEmpty())
	})
})
