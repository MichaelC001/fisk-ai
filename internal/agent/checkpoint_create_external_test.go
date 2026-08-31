//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// These tests exercise Checkpoint.CreateIfMissing through the exported agent.Run API.
// It is what an at-least-once caller needs: a run is named, and the store rather than
// the caller decides whether that name is a fresh session or one to continue. The
// hooks turn on the same decision, so they are asserted alongside it.
package agent_test

import (
	"context"
	"encoding/json"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/runstate"
)

// completedRun journals a finished run under the given name and returns what it
// produced, so a spec can ask for the same name again.
func completedRun(tb testing.TB, store runstate.Store, app *agenttest.FakeApp, id string) *agent.Result {
	tb.Helper()

	res, err := agent.Run(context.Background(), agent.Options{
		Config:       agenttest.Config(tb, app),
		ConfigFile:   "agent.yaml",
		Prompt:       []string{"go"},
		Provider:     agenttest.NewScriptedProvider(tb, agenttest.TextResponse("done")),
		Checkpoint:   agent.Checkpoint{ResumeID: id, CreateIfMissing: true},
		SessionStore: store,
	}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(tb))

	Expect(err).NotTo(HaveOccurred())
	Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

	return res
}

var _ = Describe("Checkpoint.CreateIfMissing", func() {
	// This proves a first delivery: the store has no such session, so one is created
	// under the caller's name and the run is a fresh one, with the prompt delivered and
	// both entry hooks reporting it as such.
	It("Should create a missing session", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		store := agenttest.NewFakeSessionStore(GinkgoTB())

		var start agent.RunStartInfo
		var submits int

		res, err := agent.Run(context.Background(), agent.Options{
			Config:       agenttest.Config(GinkgoTB(), app),
			ConfigFile:   "agent.yaml",
			Prompt:       []string{"go"},
			Provider:     agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
			Checkpoint:   agent.Checkpoint{ResumeID: "job-1", CreateIfMissing: true},
			SessionStore: store,
			Hooks: agent.Hooks{
				RunStart: func(_ context.Context, in agent.RunStartInfo) error {
					start = in
					return nil
				},
				UserPromptSubmit: func(_ context.Context, _ agent.UserPromptSubmitInfo) (agent.UserPromptSubmitResult, error) {
					submits++
					return agent.UserPromptSubmitResult{}, nil
				},
			},
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))

		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))
		Expect(res.SessionID).To(Equal("job-1"), "the session takes the name it was asked for")
		Expect(res.Text).To(Equal("done"))

		Expect(start.Resumed).To(BeFalse(), "nothing was there to resume")
		Expect(submits).To(Equal(1), "the prompt is new on a first delivery")

		state, err := store.Load(context.Background(), "job-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(state.Completed()).To(BeTrue())
	})

	// This proves a redelivery: the same name now has a journal, so the run continues it
	// rather than making the same model calls a second time, and the prompt is not
	// delivered again.
	It("Should resume an existing session", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		store := agenttest.NewFakeSessionStore(GinkgoTB())

		// A first attempt that suspends leaves a resumable journal behind, which is the
		// state a worker killed mid-run leaves for the delivery that follows it.
		polls := 0
		first, err := agent.Run(context.Background(), agent.Options{
			Config:           agenttest.Config(GinkgoTB(), app),
			ConfigFile:       "agent.yaml",
			Prompt:           []string{"go"},
			Provider:         agenttest.NewScriptedProvider(GinkgoTB(), agenttest.ToolUseResponse("c1", "do", json.RawMessage(`{"subject":"x"}`))),
			Checkpoint:       agent.Checkpoint{ResumeID: "job-1", CreateIfMissing: true},
			SessionStore:     store,
			SuspendRequested: func() bool { polls++; return polls > 1 },
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(first.Reason).To(Equal(runstate.ReasonSuspended))

		var start agent.RunStartInfo
		var submits int

		second, err := agent.Run(context.Background(), agent.Options{
			Config:       agenttest.Config(GinkgoTB(), app),
			ConfigFile:   "agent.yaml",
			Prompt:       []string{"go"},
			Provider:     agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("finished")),
			Checkpoint:   agent.Checkpoint{ResumeID: "job-1", CreateIfMissing: true},
			SessionStore: store,
			ClaimedBy:    "worker-b",
			Hooks: agent.Hooks{
				RunStart: func(_ context.Context, in agent.RunStartInfo) error {
					start = in
					return nil
				},
				UserPromptSubmit: func(_ context.Context, _ agent.UserPromptSubmitInfo) (agent.UserPromptSubmitResult, error) {
					submits++
					return agent.UserPromptSubmitResult{}, nil
				},
			},
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))

		Expect(err).NotTo(HaveOccurred())
		Expect(second.Reason).To(Equal(runstate.ReasonCompleted))
		Expect(second.SessionID).To(Equal("job-1"))

		Expect(start.Resumed).To(BeTrue(), "the store, not the caller, said this was a resume")
		Expect(submits).To(Equal(0), "a resumed run rebuilds its conversation and re-delivers no prompt")

		// The resume claimed the run before it did anything, which is what arbitrates two
		// workers arriving on the same redelivery.
		var claims int
		for _, rec := range recordsOf(GinkgoTB(), store, "job-1") {
			if rec.Claim != nil {
				claims++
			}
		}
		Expect(claims).To(Equal(1))
	})

	// This proves the flag is what changes the behavior: a plain resume of a session no
	// store holds still fails rather than quietly starting a new run under that name.
	It("Should be off by default", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())

		var started int

		_, err := agent.Run(context.Background(), agent.Options{
			Config:       agenttest.Config(GinkgoTB(), app),
			ConfigFile:   "agent.yaml",
			Prompt:       []string{"go"},
			Provider:     agenttest.NewScriptedProvider(GinkgoTB()),
			Checkpoint:   agent.Checkpoint{ResumeID: "job-1"},
			SessionStore: agenttest.NewFakeSessionStore(GinkgoTB()),
			Hooks: agent.Hooks{
				RunStart: func(_ context.Context, _ agent.RunStartInfo) error {
					started++
					return nil
				},
			},
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))

		Expect(err).To(MatchError(runstate.ErrNotFound))
		Expect(started).To(Equal(0), "the store is consulted before either entry hook fires")
	})

	// This proves the case that ends a lost acknowledgement. A queue redelivers a job
	// whose run already finished, and the caller is handed the journaled answer rather
	// than being told its own work is an error. Nothing runs: no model is called and no
	// hook fires.
	It("Should report a completed session's stored answer", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		store := agenttest.NewFakeSessionStore(GinkgoTB())

		first := completedRun(GinkgoTB(), store, app, "job-1")

		var started, submits int

		// An exhausted provider errors on any call, so a run that started would fail here
		// rather than quietly pass.
		second, err := agent.Run(context.Background(), agent.Options{
			Config:       agenttest.Config(GinkgoTB(), app),
			ConfigFile:   "agent.yaml",
			Prompt:       []string{"go"},
			Provider:     agenttest.NewScriptedProvider(GinkgoTB()),
			Checkpoint:   agent.Checkpoint{ResumeID: "job-1", CreateIfMissing: true},
			SessionStore: store,
			Hooks: agent.Hooks{
				RunStart: func(_ context.Context, _ agent.RunStartInfo) error {
					started++
					return nil
				},
				UserPromptSubmit: func(_ context.Context, _ agent.UserPromptSubmitInfo) (agent.UserPromptSubmitResult, error) {
					submits++
					return agent.UserPromptSubmitResult{}, nil
				},
			},
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))

		Expect(err).NotTo(HaveOccurred())
		Expect(second.SessionID).To(Equal("job-1"))
		Expect(second.Reason).To(Equal(runstate.ReasonCompleted))
		Expect(second.Text).To(Equal("done"), "the answer comes back out of the journal")

		Expect(started).To(Equal(0), "nothing ran, so nothing is announced")
		Expect(submits).To(Equal(0))

		// The counters are the stored run's, since they are what the work cost. This call
		// cost nothing and must not report the work as free either.
		Expect(second.Stats).NotTo(BeNil())
		Expect(second.Stats.LlmCalls).To(Equal(first.Stats.LlmCalls))
		Expect(second.Stats.InTokens).To(Equal(first.Stats.InTokens))
		Expect(second.Stats.OutTokens).To(Equal(first.Stats.OutTokens))
		Expect(second.Stats.Session).To(Equal("job-1"))
	})

	// This proves the answering behavior belongs to the flag. An operator resuming a
	// finished session has made a mistake and is told so, as before.
	It("Should still refuse a plain resume of a completed session", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		store := agenttest.NewFakeSessionStore(GinkgoTB())

		completedRun(GinkgoTB(), store, app, "job-1")

		_, err := agent.Run(context.Background(), agent.Options{
			Config:       agenttest.Config(GinkgoTB(), app),
			ConfigFile:   "agent.yaml",
			Prompt:       []string{"go"},
			Provider:     agenttest.NewScriptedProvider(GinkgoTB()),
			Checkpoint:   agent.Checkpoint{ResumeID: "job-1"},
			SessionStore: store,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))

		Expect(err).To(MatchError(ContainSubstring("has already completed")))
	})
})
