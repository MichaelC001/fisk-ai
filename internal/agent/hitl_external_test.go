//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// These tests drive the human-in-the-loop built-ins through the exported agent.Run
// API: a question the operator never answered, and what a resume does about it.
package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/runstate"
	runstatefile "github.com/choria-io/fisk-ai/internal/runstate/file"
	"github.com/choria-io/fisk-ai/internal/toolkit"
)

var _ = Describe("the human-in-the-loop built-ins", func() {
	// This is the defect the item exists to fix. An interrupt at ask_human_confirm used to
	// answer the model on the operator's behalf, and that answer was journaled: every later
	// resume replayed a decision they never made. The call is now left unanswered, so the
	// resume puts the question again.
	It("Should ask an unanswered question again on resume", func() {
		ctx := context.Background()

		store, err := runstatefile.NewFileStore(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())

		opts := func(provider *agenttest.ScriptedProvider, cp agent.Checkpoint) agent.Options {
			return agent.Options{
				Config:       agenttest.Config(GinkgoTB(), app, agenttest.WithHITL()),
				ConfigFile:   "agent.yaml",
				Prompt:       []string{"check with the operator"},
				Provider:     provider,
				SessionStore: store,
				Checkpoint:   cp,
			}
		}

		// Run 1: the operator interrupts at the question. The run suspends with the call
		// unanswered, so the journal holds no answer to replay.
		interrupted := agenttest.NewScriptedPrompter(GinkgoTB())
		interrupted.ConfirmFn = func(string) (bool, error) {
			return false, fmt.Errorf("%w: interrupt", toolkit.ErrPromptAborted)
		}

		res1, err := agent.Run(ctx, opts(
			agenttest.NewScriptedProvider(GinkgoTB(), agenttest.ToolUseResponse("c1", "ask_human_confirm", json.RawMessage(`{"question":"Proceed?"}`))),
			agent.Checkpoint{Enabled: true},
		), agenttest.NewRecordingEvents(), interrupted)
		Expect(err).To(MatchError(toolkit.ErrPromptAborted))
		Expect(res1.Reason).To(Equal(runstate.ReasonSuspended))

		rs, err := store.Load(res1.SessionID)
		Expect(err).NotTo(HaveOccurred())
		Expect(rs.Pending).NotTo(BeNil(), "the call the operator did not answer is still in flight")
		Expect(rs.Pending.Answered).To(BeEmpty())
		Expect(rs.Pending.OpenDeferrals()).To(BeEmpty(), "an unanswered question is re-asked, not waiting on somebody")

		// Run 2: the same question reaches the operator again, and their answer is what the
		// model is told.
		var asked atomic.Int64
		answering := agenttest.NewScriptedPrompter(GinkgoTB())
		answering.ConfirmFn = func(string) (bool, error) {
			asked.Add(1)
			return true, nil
		}

		res2, err := agent.Run(ctx, opts(
			agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("the operator agreed")),
			agent.Checkpoint{ResumeID: res1.SessionID},
		), agenttest.NewRecordingEvents(), answering)
		Expect(err).NotTo(HaveOccurred())
		Expect(res2.Reason).To(Equal(runstate.ReasonCompleted))
		Expect(asked.Load()).To(Equal(int64(1)))
	})

	// This keeps the other half of the split: with nobody to ask, the tool answers the
	// model rather than stopping the run. A queued job whose model asks for a human has to
	// finish, and it can only do that if the tool reports that no operator was reachable.
	It("Should answer the model when no operator is reachable", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())

		events := agenttest.NewRecordingEvents()
		res, err := agent.Run(context.Background(), agent.Options{
			Config:     agenttest.Config(GinkgoTB(), app, agenttest.WithHITL()),
			ConfigFile: "agent.yaml",
			Prompt:     []string{"check with the operator"},
			Provider: agenttest.NewScriptedProvider(GinkgoTB(),
				agenttest.ToolUseResponse("c1", "ask_human_confirm", json.RawMessage(`{"question":"Proceed?"}`)),
				agenttest.TextResponse("nobody was there to ask"),
			),
		}, events, agenttest.NewScriptedPrompter(GinkgoTB()).NoOperator())
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

		results := events.ToolResults()
		Expect(results).To(HaveLen(1))
		Expect(results[0].IsError).To(BeFalse(), "no operator is an answer the model reasons about, not a tool failure")
		Expect(results[0].Output).To(ContainSubstring("no interactive terminal"))
	})
})
