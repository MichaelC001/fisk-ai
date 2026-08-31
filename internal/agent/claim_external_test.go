//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// These tests exercise the acquisition fence through the exported agent.Run API: the
// claim a resume writes before it does anything, and the check a run makes before each
// tool so a worker that lost its run stops in front of the next effect rather than at
// its next append.
package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/runstate"
)

// suspendedRun journals a run that stops at a suspend with one tool call behind it, so
// there is something to resume and something to compare a resumed journal against.
func suspendedRun(tb testing.TB, store runstate.Store, app *agenttest.FakeApp) *agent.Result {
	tb.Helper()

	polls := 0
	res, err := agent.Run(context.Background(), agent.Options{
		Config:           agenttest.Config(tb, app),
		ConfigFile:       "agent.yaml",
		Prompt:           []string{"start"},
		Provider:         agenttest.NewScriptedProvider(tb, agenttest.ToolUseResponse("c1", "do", json.RawMessage(`{"subject":"x"}`))),
		Checkpoint:       agent.Checkpoint{Enabled: true},
		SessionStore:     store,
		SuspendRequested: func() bool { polls++; return polls > 1 },
	}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(tb))

	Expect(err).NotTo(HaveOccurred())
	Expect(res.Reason).To(Equal(runstate.ReasonSuspended))

	return res
}

func recordsOf(tb testing.TB, store runstate.Store, id string) []runstate.Record {
	tb.Helper()

	ctx := context.Background()

	j, err := store.Open(ctx, id)
	Expect(err).NotTo(HaveOccurred())
	// The journal has to close before this returns, since the next resume in the same
	// spec claims the run.
	defer func() { Expect(j.Close()).To(Succeed()) }()

	recs, err := j.Records(ctx)
	Expect(err).NotTo(HaveOccurred())

	return recs
}

// currentSessionID returns the only run in the store, for a spec that has to reach a
// live session it never saw an id for.
func currentSessionID(tb testing.TB, store *agenttest.FakeSessionStore) string {
	tb.Helper()

	runs, err := store.List(context.Background())
	Expect(err).NotTo(HaveOccurred())
	Expect(runs).To(HaveLen(1))

	return runs[0].RunID
}

var _ = Describe("the run claim", func() {
	// This is the ordering test. The claim has to be appended before the resumed run reads
	// its sequence, or the run's first real record reuses the claim's seq, CheckAppend
	// folds it away as a duplicate, and it is lost with no error anywhere.
	It("Should claim the run on resume and keep its records", func() {
		store := agenttest.NewFakeSessionStore(GinkgoTB())
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		first := suspendedRun(GinkgoTB(), store, app)

		before := recordsOf(GinkgoTB(), store, first.SessionID)

		res, err := agent.Run(context.Background(), agent.Options{
			Config:       agenttest.Config(GinkgoTB(), app),
			ConfigFile:   "agent.yaml",
			Provider:     agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("finished")),
			Checkpoint:   agent.Checkpoint{ResumeID: first.SessionID},
			SessionStore: store,
			ClaimedBy:    "worker-a",
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

		after := recordsOf(GinkgoTB(), store, first.SessionID)

		var claims []*runstate.ClaimRecord
		for _, rec := range after {
			if rec.Claim != nil {
				claims = append(claims, rec.Claim)
			}
		}
		Expect(claims).To(HaveLen(1))
		Expect(claims[0].By).To(Equal("worker-a"))
		Expect(claims[0].Claimed).NotTo(BeZero())

		// The claim is the first thing the resume wrote, so it sits directly after
		// everything the suspended run had left behind.
		Expect(after[len(before)].Protocol).To(Equal(runstate.ClaimProtocol))

		// And the turn the resume produced is still there. Folding is what proves it: a
		// record silently skipped for a duplicate seq leaves the append call succeeding.
		assistants := 0
		for _, rec := range after {
			if rec.Protocol == runstate.AssistantProtocol {
				assistants++
			}
		}
		Expect(assistants).To(Equal(2), "the resumed turn was journaled, not folded away as a duplicate seq")

		_, err = runstate.Fold(after)
		Expect(err).NotTo(HaveOccurred())
	})

	// This covers the default, since a CLI sets no claimant and the record still has to
	// say something a person can act on.
	It("Should derive a claimant when the caller names nobody", func() {
		store := agenttest.NewFakeSessionStore(GinkgoTB())
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		first := suspendedRun(GinkgoTB(), store, app)

		_, err := agent.Run(context.Background(), agent.Options{
			Config:       agenttest.Config(GinkgoTB(), app),
			ConfigFile:   "agent.yaml",
			Provider:     agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("finished")),
			Checkpoint:   agent.Checkpoint{ResumeID: first.SessionID},
			SessionStore: store,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())

		for _, rec := range recordsOf(GinkgoTB(), store, first.SessionID) {
			if rec.Claim != nil {
				// The identity leads, since it is the part naming which agent is running.
				Expect(rec.Claim.By).To(HavePrefix("agent@"))
				Expect(rec.Claim.By).To(ContainSubstring("pid "))
				return
			}
		}

		Expect(false).To(BeTrue(), "the resume wrote no claim")
	})

	// This is the case the claim exists to make safe: the resume is refused, and it is
	// refused before anything happened.
	It("Should run nothing when the claim is refused", func() {
		store := agenttest.NewFakeSessionStore(GinkgoTB())
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		first := suspendedRun(GinkgoTB(), store, app)

		// Another worker holds the run, so this one's claim cannot land.
		store.Evict(first.SessionID)

		provider := agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("finished"))
		_, err := agent.Run(context.Background(), agent.Options{
			Config:       agenttest.Config(GinkgoTB(), app),
			ConfigFile:   "agent.yaml",
			Provider:     provider,
			Checkpoint:   agent.Checkpoint{ResumeID: first.SessionID},
			SessionStore: store,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))

		Expect(err).To(HaveOccurred())
		Expect(errors.Is(err, runstate.ErrLocked)).To(BeTrue(), "a caller has to be able to tell this from a broken store")
		Expect(err.Error()).To(ContainSubstring("nothing here ran"))
		Expect(provider.Requests()).To(BeEmpty(), "the model was never called, so no tool could have run")
	})

	// This is the incumbent's half. Losing the run has to be discovered in front of the
	// next tool, not at the append that would have recorded its result.
	It("Should stop a taken-over run before its next tool", func() {
		store := agenttest.NewFakeSessionStore(GinkgoTB())
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())

		// The tool records that it ran. Nothing should ever set this.
		ran := false
		provider := agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("c1", "do", json.RawMessage(`{"subject":"x"}`)),
			agenttest.TextResponse("finished"),
		)

		_, err := agent.Run(context.Background(), agent.Options{
			Config:       agenttest.Config(GinkgoTB(), app),
			ConfigFile:   "agent.yaml",
			Prompt:       []string{"start"},
			Provider:     provider,
			Checkpoint:   agent.Checkpoint{Enabled: true},
			SessionStore: store,
			Hooks: agent.Hooks{
				// The run is taken over between the model asking for a tool and the tool
				// running, which is the window a lease expiry opens.
				PreToolUse: func(_ context.Context, _ agent.PreToolUseInfo) (agent.PreToolUseResult, error) {
					ran = true
					store.Evict(currentSessionID(GinkgoTB(), store))
					return agent.PreToolUseResult{}, nil
				},
			},
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))

		Expect(ran).To(BeTrue(), "the run reached the point where a tool was about to be dispatched")
		Expect(err).To(HaveOccurred())
		Expect(errors.Is(err, runstate.ErrLocked)).To(BeTrue())
		Expect(err.Error()).To(ContainSubstring("no longer safe to continue"))
	})
})
