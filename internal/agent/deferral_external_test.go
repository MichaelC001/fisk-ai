//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// These tests drive deferred tool results through the exported agent.Run API: a tool
// that will answer later, the suspend it produces, the resume that does not re-run it,
// and the answer that finishes the turn.
package agent_test

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/runstate"
	runstatefile "github.com/choria-io/fisk-ai/internal/runstate/file"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/toolkit/functool"
)

// deferringTool answers later rather than now, and counts how many times it was
// called so a test can prove a deferred call is never dispatched again.
func deferringTool(tb testing.TB, name string, calls *atomic.Int64) toolkit.Tool {
	tb.Helper()

	tool, err := functool.New(functool.Spec{
		Name:        name,
		Description: "raises a change request and answers when it is approved",
		Schema:      map[string]any{"type": "object"},
		Handler: func(context.Context, json.RawMessage, *functool.CallContext) (string, error) {
			calls.Add(1)
			return "", toolkit.DeferResult("waiting on change approval", "CHG-1234")
		},
	})
	Expect(err).NotTo(HaveOccurred())

	return tool
}

// countingTool answers at once and counts its calls, so a test can tell a call a
// crash interrupted (re-run) from a deferred one (never re-run).
func countingTool(tb testing.TB, name string, calls *atomic.Int64) toolkit.Tool {
	tb.Helper()

	tool, err := functool.New(functool.Spec{
		Name:        name,
		Description: "answers at once",
		Schema:      map[string]any{"type": "object"},
		Handler: func(context.Context, json.RawMessage, *functool.CallContext) (string, error) {
			calls.Add(1)
			return `{"ok":true}`, nil
		},
	})
	Expect(err).NotTo(HaveOccurred())

	return tool
}

var _ = Describe("deferred tool results", func() {
	// This is the whole cycle: a tool defers, the run suspends naming what it is waiting
	// on, a resume before the answer makes no model call and re-runs nothing, and the
	// answer supplied out of band lets the next resume finish the turn without dispatching
	// the tool again.
	It("Should suspend and resume on the supplied answer", func() {
		ctx := context.Background()

		store, err := runstatefile.NewFileStore(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		var calls atomic.Int64
		tool := deferringTool(GinkgoTB(), "change_request", &calls)

		opts := func(provider *agenttest.ScriptedProvider, cp agent.Checkpoint) agent.Options {
			return agent.Options{
				Config:       agenttest.Config(GinkgoTB(), app),
				ConfigFile:   "agent.yaml",
				Prompt:       []string{"raise a change"},
				Provider:     provider,
				SessionStore: store,
				Checkpoint:   cp,
				CustomTools:  []toolkit.Tool{tool},
			}
		}

		// Run 1: the model calls the tool, the tool defers, and the run ends at a
		// resumable boundary with nothing committed.
		res1, err := agent.Run(ctx, opts(
			agenttest.NewScriptedProvider(GinkgoTB(), agenttest.ToolUseResponse("c1", "change_request", json.RawMessage(`{}`))),
			agent.Checkpoint{Enabled: true},
		), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res1.Reason).To(Equal(runstate.ReasonSuspended))
		Expect(calls.Load()).To(Equal(int64(1)))

		// What the run is waiting on is reported, which is what separates this suspend
		// from one an operator asked for.
		Expect(res1.Deferred).To(HaveLen(1))
		Expect(res1.Deferred[0].ToolName).To(Equal("change_request"))
		Expect(res1.Deferred[0].Note).To(Equal("waiting on change approval"))
		Expect(res1.Deferred[0].Handle).To(Equal("CHG-1234"))
		useID := res1.Deferred[0].ToolUseID

		// Run 2: resumed before the answer exists. The scripted provider carries no
		// responses, so any model call fails the test; the tool must not run again either.
		res2, err := agent.Run(ctx, opts(
			agenttest.NewScriptedProvider(GinkgoTB()),
			agent.Checkpoint{ResumeID: res1.SessionID},
		), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res2.Reason).To(Equal(runstate.ReasonSuspended))
		Expect(calls.Load()).To(Equal(int64(1)))

		// The deferral inherited from the journal is reported as readily as the one this
		// process made, so an operator resuming into the same wait is told why.
		Expect(res2.Deferred).To(HaveLen(1))
		Expect(res2.Deferred[0].ToolUseID).To(Equal(useID))

		// The answer arrives from outside the run, as a ticket system or an operator
		// would supply it.
		Expect(runstate.SupplyToolResult(store, res1.SessionID, useID, `{"approved":true}`, false)).To(Succeed())

		// Run 3: the answer is folded in, the turn completes, and the tool is still not
		// dispatched again.
		res3, err := agent.Run(ctx, opts(
			agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("the change was approved")),
			agent.Checkpoint{ResumeID: res1.SessionID},
		), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res3.Reason).To(Equal(runstate.ReasonCompleted))
		Expect(res3.Text).To(Equal("the change was approved"))
		Expect(res3.Deferred).To(BeEmpty())
		Expect(calls.Load()).To(Equal(int64(1)))
	})

	// This proves the rest of a batch still runs: those results are journaled and then
	// never re-run, so running them now costs nothing a resume would not cost anyway. It
	// also proves the two kinds of unanswered call are told apart, which is the whole
	// reason the deferred record exists: the sibling that never ran is dispatched on
	// resume and the deferred one is not.
	It("Should finish the batch around a deferred call", func() {
		ctx := context.Background()

		store, err := runstatefile.NewFileStore(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		var deferCalls, doneCalls atomic.Int64
		tools := []toolkit.Tool{
			deferringTool(GinkgoTB(), "change_request", &deferCalls),
			countingTool(GinkgoTB(), "record_note", &doneCalls),
		}

		opts := func(provider *agenttest.ScriptedProvider, cp agent.Checkpoint) agent.Options {
			return agent.Options{
				Config:       agenttest.Config(GinkgoTB(), app),
				ConfigFile:   "agent.yaml",
				Prompt:       []string{"raise a change and note it"},
				Provider:     provider,
				SessionStore: store,
				Checkpoint:   cp,
				CustomTools:  tools,
			}
		}

		batch := agenttest.ToolUseResponse("c1", "change_request", json.RawMessage(`{}`))
		batch.Content = append(batch.Content, agenttest.ToolUseResponse("c2", "record_note", json.RawMessage(`{}`)).Content...)

		res1, err := agent.Run(ctx, opts(agenttest.NewScriptedProvider(GinkgoTB(), batch), agent.Checkpoint{Enabled: true}),
			agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res1.Reason).To(Equal(runstate.ReasonSuspended))

		// Both ran once: the deferral did not stop the batch.
		Expect(deferCalls.Load()).To(Equal(int64(1)))
		Expect(doneCalls.Load()).To(Equal(int64(1)))
		Expect(res1.Deferred).To(HaveLen(1))

		// A resume re-runs neither: one is answered and the other is deferred.
		res2, err := agent.Run(ctx, opts(agenttest.NewScriptedProvider(GinkgoTB()), agent.Checkpoint{ResumeID: res1.SessionID}),
			agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res2.Reason).To(Equal(runstate.ReasonSuspended))
		Expect(deferCalls.Load()).To(Equal(int64(1)))
		Expect(doneCalls.Load()).To(Equal(int64(1)))
	})

	// This proves the answer can arrive with the resume rather than ahead of it, which is
	// what a caller answering over the wire does: it holds no journal, so the run applies
	// the answer under the claim it takes.
	It("Should carry the answer in with the resume", func() {
		ctx := context.Background()

		store, err := runstatefile.NewFileStore(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		var calls atomic.Int64
		tool := deferringTool(GinkgoTB(), "change_request", &calls)

		opts := func(provider *agenttest.ScriptedProvider, cp agent.Checkpoint) agent.Options {
			return agent.Options{
				Config:       agenttest.Config(GinkgoTB(), app),
				ConfigFile:   "agent.yaml",
				Prompt:       []string{"raise a change"},
				Provider:     provider,
				SessionStore: store,
				Checkpoint:   cp,
				CustomTools:  []toolkit.Tool{tool},
			}
		}

		res1, err := agent.Run(ctx, opts(
			agenttest.NewScriptedProvider(GinkgoTB(), agenttest.ToolUseResponse("c1", "change_request", json.RawMessage(`{}`))),
			agent.Checkpoint{Enabled: true},
		), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res1.Reason).To(Equal(runstate.ReasonSuspended))
		useID := res1.Deferred[0].ToolUseID

		// A call this conversation is not waiting on refuses the resume before anything
		// runs, so the caller hears about it rather than the run ending on the same wait.
		_, err = agent.Run(ctx, opts(
			agenttest.NewScriptedProvider(GinkgoTB()),
			agent.Checkpoint{ResumeID: res1.SessionID, Answer: &agent.DeferredAnswer{ToolUseID: "nobody", Content: `{}`}},
		), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).To(MatchError(runstate.ErrNotDeferred))

		// The answer rides in with the resume: the turn completes on it and the tool is
		// not dispatched a second time.
		res2, err := agent.Run(ctx, opts(
			agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("the change was approved")),
			agent.Checkpoint{ResumeID: res1.SessionID, Answer: &agent.DeferredAnswer{ToolUseID: useID, Content: `{"approved":true}`}},
		), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res2.Reason).To(Equal(runstate.ReasonCompleted))
		Expect(res2.Text).To(Equal("the change was approved"))
		Expect(res2.Deferred).To(BeEmpty())
		Expect(calls.Load()).To(Equal(int64(1)))

		// Answering it a second time is refused, the conversation having finished the turn
		// the call belonged to. A caller that sent its answer twice is told so rather than
		// paying for a run that has nothing to do.
		_, err = agent.Run(ctx, opts(
			agenttest.NewScriptedProvider(GinkgoTB()),
			agent.Checkpoint{ResumeID: res1.SessionID, Answer: &agent.DeferredAnswer{ToolUseID: useID, Content: `{"approved":true}`}},
		), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).To(MatchError(ContainSubstring("already completed")))
	})
})
