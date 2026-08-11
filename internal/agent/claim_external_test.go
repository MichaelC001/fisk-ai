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

	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/runstate"
)

// suspendedRun journals a run that stops at a suspend with one tool call behind it, so
// there is something to resume and something to compare a resumed journal against.
func suspendedRun(t *testing.T, store runstate.Store, app *agenttest.FakeApp) *agent.Result {
	t.Helper()
	g := NewWithT(t)

	polls := 0
	res, err := agent.Run(context.Background(), agent.Options{
		Config:           agenttest.Config(t, app),
		ConfigFile:       "agent.yaml",
		Prompt:           []string{"start"},
		Provider:         agenttest.NewScriptedProvider(t, agenttest.ToolUseResponse("c1", "do", json.RawMessage(`{"subject":"x"}`))),
		Checkpoint:       agent.Checkpoint{Enabled: true},
		SessionStore:     store,
		SuspendRequested: func() bool { polls++; return polls > 1 },
	}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.Reason).To(Equal(runstate.ReasonSuspended))

	return res
}

func recordsOf(t *testing.T, store runstate.Store, id string) []runstate.Record {
	t.Helper()
	g := NewWithT(t)

	j, err := store.Open(id)
	g.Expect(err).NotTo(HaveOccurred())
	defer func() { g.Expect(j.Close()).To(Succeed()) }()

	recs, err := j.Records()
	g.Expect(err).NotTo(HaveOccurred())

	return recs
}

// TestClaim_ResumeClaimsTheRunAndKeepsItsRecords is the ordering test. The claim has to
// be appended before the resumed run reads its sequence, or the run's first real record
// reuses the claim's seq, CheckAppend folds it away as a duplicate, and it is lost with
// no error anywhere.
func TestClaim_ResumeClaimsTheRunAndKeepsItsRecords(t *testing.T) {
	g := NewWithT(t)

	store := agenttest.NewFakeSessionStore(t)
	app := agenttest.NewFakeApp(t, exampleApp())
	first := suspendedRun(t, store, app)

	before := recordsOf(t, store, first.SessionID)

	res, err := agent.Run(context.Background(), agent.Options{
		Config:       agenttest.Config(t, app),
		ConfigFile:   "agent.yaml",
		Provider:     agenttest.NewScriptedProvider(t, agenttest.TextResponse("finished")),
		Checkpoint:   agent.Checkpoint{ResumeID: first.SessionID},
		SessionStore: store,
		ClaimedBy:    "worker-a",
	}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

	after := recordsOf(t, store, first.SessionID)

	var claims []*runstate.ClaimRecord
	for _, rec := range after {
		if rec.Claim != nil {
			claims = append(claims, rec.Claim)
		}
	}
	g.Expect(claims).To(HaveLen(1))
	g.Expect(claims[0].By).To(Equal("worker-a"))
	g.Expect(claims[0].Claimed).NotTo(BeZero())

	// The claim is the first thing the resume wrote, so it sits directly after
	// everything the suspended run had left behind.
	g.Expect(after[len(before)].Protocol).To(Equal(runstate.ClaimProtocol))

	// And the turn the resume produced is still there. Folding is what proves it: a
	// record silently skipped for a duplicate seq leaves the append call succeeding.
	assistants := 0
	for _, rec := range after {
		if rec.Protocol == runstate.AssistantProtocol {
			assistants++
		}
	}
	g.Expect(assistants).To(Equal(2), "the resumed turn was journaled, not folded away as a duplicate seq")

	_, err = runstate.Fold(after)
	g.Expect(err).NotTo(HaveOccurred())
}

// TestClaim_DerivedWhenTheCallerNamesNobody covers the default, since a CLI sets no
// claimant and the record still has to say something a person can act on.
func TestClaim_DerivedWhenTheCallerNamesNobody(t *testing.T) {
	g := NewWithT(t)

	store := agenttest.NewFakeSessionStore(t)
	app := agenttest.NewFakeApp(t, exampleApp())
	first := suspendedRun(t, store, app)

	_, err := agent.Run(context.Background(), agent.Options{
		Config:       agenttest.Config(t, app),
		ConfigFile:   "agent.yaml",
		Provider:     agenttest.NewScriptedProvider(t, agenttest.TextResponse("finished")),
		Checkpoint:   agent.Checkpoint{ResumeID: first.SessionID},
		SessionStore: store,
	}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).NotTo(HaveOccurred())

	for _, rec := range recordsOf(t, store, first.SessionID) {
		if rec.Claim != nil {
			// The identity leads, since it is the part naming which agent is running.
			g.Expect(rec.Claim.By).To(HavePrefix("agent@"))
			g.Expect(rec.Claim.By).To(ContainSubstring("pid "))
			return
		}
	}

	g.Expect(false).To(BeTrue(), "the resume wrote no claim")
}

// TestClaim_ARefusedClaimRunsNothing is the case the claim exists to make safe: the
// resume is refused, and it is refused before anything happened.
func TestClaim_ARefusedClaimRunsNothing(t *testing.T) {
	g := NewWithT(t)

	store := agenttest.NewFakeSessionStore(t)
	app := agenttest.NewFakeApp(t, exampleApp())
	first := suspendedRun(t, store, app)

	// Another worker holds the run, so this one's claim cannot land.
	store.Evict(first.SessionID)

	provider := agenttest.NewScriptedProvider(t, agenttest.TextResponse("finished"))
	_, err := agent.Run(context.Background(), agent.Options{
		Config:       agenttest.Config(t, app),
		ConfigFile:   "agent.yaml",
		Provider:     provider,
		Checkpoint:   agent.Checkpoint{ResumeID: first.SessionID},
		SessionStore: store,
	}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))

	g.Expect(err).To(HaveOccurred())
	g.Expect(errors.Is(err, runstate.ErrLocked)).To(BeTrue(), "a caller has to be able to tell this from a broken store")
	g.Expect(err.Error()).To(ContainSubstring("nothing here ran"))
	g.Expect(provider.Requests()).To(BeEmpty(), "the model was never called, so no tool could have run")
}

// TestClaim_ATakenOverRunStopsBeforeItsNextTool is the incumbent's half. Losing the run
// has to be discovered in front of the next tool, not at the append that would have
// recorded its result.
func TestClaim_ATakenOverRunStopsBeforeItsNextTool(t *testing.T) {
	g := NewWithT(t)

	store := agenttest.NewFakeSessionStore(t)
	app := agenttest.NewFakeApp(t, exampleApp())

	// The tool records that it ran. Nothing should ever set this.
	ran := false
	provider := agenttest.NewScriptedProvider(t,
		agenttest.ToolUseResponse("c1", "do", json.RawMessage(`{"subject":"x"}`)),
		agenttest.TextResponse("finished"),
	)

	_, err := agent.Run(context.Background(), agent.Options{
		Config:       agenttest.Config(t, app),
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
				store.Evict(currentSessionID(t, store))
				return agent.PreToolUseResult{}, nil
			},
		},
	}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))

	g.Expect(ran).To(BeTrue(), "the run reached the point where a tool was about to be dispatched")
	g.Expect(err).To(HaveOccurred())
	g.Expect(errors.Is(err, runstate.ErrLocked)).To(BeTrue())
	g.Expect(err.Error()).To(ContainSubstring("no longer safe to continue"))
}

// currentSessionID returns the only run in the store, for a test that has to reach a
// live session it never saw an id for.
func currentSessionID(t *testing.T, store *agenttest.FakeSessionStore) string {
	t.Helper()
	g := NewWithT(t)

	runs, err := store.List()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(runs).To(HaveLen(1))

	return runs[0].RunID
}
