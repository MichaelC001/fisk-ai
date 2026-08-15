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
func deferringTool(t *testing.T, name string, calls *atomic.Int64) toolkit.Tool {
	t.Helper()

	tool, err := functool.New(functool.Spec{
		Name:        name,
		Description: "raises a change request and answers when it is approved",
		Schema:      map[string]any{"type": "object"},
		Handler: func(context.Context, json.RawMessage, *functool.CallContext) (string, error) {
			calls.Add(1)
			return "", toolkit.DeferResult("waiting on change approval", "CHG-1234")
		},
	})
	NewWithT(t).Expect(err).NotTo(HaveOccurred())

	return tool
}

// countingTool answers at once and counts its calls, so a test can tell a call a
// crash interrupted (re-run) from a deferred one (never re-run).
func countingTool(t *testing.T, name string, calls *atomic.Int64) toolkit.Tool {
	t.Helper()

	tool, err := functool.New(functool.Spec{
		Name:        name,
		Description: "answers at once",
		Schema:      map[string]any{"type": "object"},
		Handler: func(context.Context, json.RawMessage, *functool.CallContext) (string, error) {
			calls.Add(1)
			return `{"ok":true}`, nil
		},
	})
	NewWithT(t).Expect(err).NotTo(HaveOccurred())

	return tool
}

// TestDeferral_SuspendsAndResumesOnTheSuppliedAnswer is the whole cycle: a tool
// defers, the run suspends naming what it is waiting on, a resume before the answer
// makes no model call and re-runs nothing, and the answer supplied out of band lets
// the next resume finish the turn without dispatching the tool again.
func TestDeferral_SuspendsAndResumesOnTheSuppliedAnswer(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	store, err := runstatefile.NewFileStore(t.TempDir())
	g.Expect(err).NotTo(HaveOccurred())

	app := agenttest.NewFakeApp(t, exampleApp())
	var calls atomic.Int64
	tool := deferringTool(t, "change_request", &calls)

	opts := func(provider *agenttest.ScriptedProvider, cp agent.Checkpoint) agent.Options {
		return agent.Options{
			Config:       agenttest.Config(t, app),
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
		agenttest.NewScriptedProvider(t, agenttest.ToolUseResponse("c1", "change_request", json.RawMessage(`{}`))),
		agent.Checkpoint{Enabled: true},
	), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res1.Reason).To(Equal(runstate.ReasonSuspended))
	g.Expect(calls.Load()).To(Equal(int64(1)))

	// What the run is waiting on is reported, which is what separates this suspend
	// from one an operator asked for.
	g.Expect(res1.Deferred).To(HaveLen(1))
	g.Expect(res1.Deferred[0].ToolName).To(Equal("change_request"))
	g.Expect(res1.Deferred[0].Note).To(Equal("waiting on change approval"))
	g.Expect(res1.Deferred[0].Handle).To(Equal("CHG-1234"))
	useID := res1.Deferred[0].ToolUseID

	// Run 2: resumed before the answer exists. The scripted provider carries no
	// responses, so any model call fails the test; the tool must not run again either.
	res2, err := agent.Run(ctx, opts(
		agenttest.NewScriptedProvider(t),
		agent.Checkpoint{ResumeID: res1.SessionID},
	), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res2.Reason).To(Equal(runstate.ReasonSuspended))
	g.Expect(calls.Load()).To(Equal(int64(1)))

	// The deferral inherited from the journal is reported as readily as the one this
	// process made, so an operator resuming into the same wait is told why.
	g.Expect(res2.Deferred).To(HaveLen(1))
	g.Expect(res2.Deferred[0].ToolUseID).To(Equal(useID))

	// The answer arrives from outside the run, as a ticket system or an operator
	// would supply it.
	g.Expect(runstate.SupplyToolResult(store, res1.SessionID, useID, `{"approved":true}`, false)).To(Succeed())

	// Run 3: the answer is folded in, the turn completes, and the tool is still not
	// dispatched again.
	res3, err := agent.Run(ctx, opts(
		agenttest.NewScriptedProvider(t, agenttest.TextResponse("the change was approved")),
		agent.Checkpoint{ResumeID: res1.SessionID},
	), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res3.Reason).To(Equal(runstate.ReasonCompleted))
	g.Expect(res3.Text).To(Equal("the change was approved"))
	g.Expect(res3.Deferred).To(BeEmpty())
	g.Expect(calls.Load()).To(Equal(int64(1)))
}

// TestDeferral_BatchFinishesAroundADeferredCall proves the rest of a batch still
// runs: those results are journaled and then never re-run, so running them now costs
// nothing a resume would not cost anyway. It also proves the two kinds of unanswered
// call are told apart, which is the whole reason the deferred record exists: the
// sibling that never ran is dispatched on resume and the deferred one is not.
func TestDeferral_BatchFinishesAroundADeferredCall(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	store, err := runstatefile.NewFileStore(t.TempDir())
	g.Expect(err).NotTo(HaveOccurred())

	app := agenttest.NewFakeApp(t, exampleApp())
	var deferCalls, doneCalls atomic.Int64
	tools := []toolkit.Tool{
		deferringTool(t, "change_request", &deferCalls),
		countingTool(t, "record_note", &doneCalls),
	}

	opts := func(provider *agenttest.ScriptedProvider, cp agent.Checkpoint) agent.Options {
		return agent.Options{
			Config:       agenttest.Config(t, app),
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

	res1, err := agent.Run(ctx, opts(agenttest.NewScriptedProvider(t, batch), agent.Checkpoint{Enabled: true}),
		agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res1.Reason).To(Equal(runstate.ReasonSuspended))

	// Both ran once: the deferral did not stop the batch.
	g.Expect(deferCalls.Load()).To(Equal(int64(1)))
	g.Expect(doneCalls.Load()).To(Equal(int64(1)))
	g.Expect(res1.Deferred).To(HaveLen(1))

	// A resume re-runs neither: one is answered and the other is deferred.
	res2, err := agent.Run(ctx, opts(agenttest.NewScriptedProvider(t), agent.Checkpoint{ResumeID: res1.SessionID}),
		agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res2.Reason).To(Equal(runstate.ReasonSuspended))
	g.Expect(deferCalls.Load()).To(Equal(int64(1)))
	g.Expect(doneCalls.Load()).To(Equal(int64(1)))
}
