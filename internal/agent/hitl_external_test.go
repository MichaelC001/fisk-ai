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
	"testing"

	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/runstate"
	runstatefile "github.com/choria-io/fisk-ai/internal/runstate/file"
	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// TestHITL_AnUnansweredQuestionIsAskedAgain is the defect the item exists to fix. An
// interrupt at ask_human_confirm used to answer the model on the operator's behalf,
// and that answer was journaled: every later resume replayed a decision they never
// made. The call is now left unanswered, so the resume puts the question again.
func TestHITL_AnUnansweredQuestionIsAskedAgain(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	store, err := runstatefile.NewFileStore(t.TempDir())
	g.Expect(err).NotTo(HaveOccurred())

	app := agenttest.NewFakeApp(t, exampleApp())

	opts := func(provider *agenttest.ScriptedProvider, cp agent.Checkpoint) agent.Options {
		return agent.Options{
			Config:       agenttest.Config(t, app, agenttest.WithHITL()),
			ConfigFile:   "agent.yaml",
			Prompt:       []string{"check with the operator"},
			Provider:     provider,
			SessionStore: store,
			Checkpoint:   cp,
		}
	}

	// Run 1: the operator interrupts at the question. The run suspends with the call
	// unanswered, so the journal holds no answer to replay.
	interrupted := agenttest.NewScriptedPrompter(t)
	interrupted.ConfirmFn = func(string) (bool, error) {
		return false, fmt.Errorf("%w: interrupt", toolkit.ErrPromptAborted)
	}

	res1, err := agent.Run(ctx, opts(
		agenttest.NewScriptedProvider(t, agenttest.ToolUseResponse("c1", "ask_human_confirm", json.RawMessage(`{"question":"Proceed?"}`))),
		agent.Checkpoint{Enabled: true},
	), agenttest.NewRecordingEvents(), interrupted)
	g.Expect(err).To(MatchError(toolkit.ErrPromptAborted))
	g.Expect(res1.Reason).To(Equal(runstate.ReasonSuspended))

	rs, err := store.Load(res1.SessionID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rs.Pending).NotTo(BeNil(), "the call the operator did not answer is still in flight")
	g.Expect(rs.Pending.Answered).To(BeEmpty())
	g.Expect(rs.Pending.OpenDeferrals()).To(BeEmpty(), "an unanswered question is re-asked, not waiting on somebody")

	// Run 2: the same question reaches the operator again, and their answer is what the
	// model is told.
	var asked atomic.Int64
	answering := agenttest.NewScriptedPrompter(t)
	answering.ConfirmFn = func(string) (bool, error) {
		asked.Add(1)
		return true, nil
	}

	res2, err := agent.Run(ctx, opts(
		agenttest.NewScriptedProvider(t, agenttest.TextResponse("the operator agreed")),
		agent.Checkpoint{ResumeID: res1.SessionID},
	), agenttest.NewRecordingEvents(), answering)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res2.Reason).To(Equal(runstate.ReasonCompleted))
	g.Expect(asked.Load()).To(Equal(int64(1)))
}

// TestHITL_NoOperatorAnswersTheModel keeps the other half of the split: with nobody
// to ask, the tool answers the model rather than stopping the run. A queued job whose
// model asks for a human has to finish, and it can only do that if the tool reports
// that no operator was reachable.
func TestHITL_NoOperatorAnswersTheModel(t *testing.T) {
	g := NewWithT(t)

	app := agenttest.NewFakeApp(t, exampleApp())

	events := agenttest.NewRecordingEvents()
	res, err := agent.Run(context.Background(), agent.Options{
		Config:     agenttest.Config(t, app, agenttest.WithHITL()),
		ConfigFile: "agent.yaml",
		Prompt:     []string{"check with the operator"},
		Provider: agenttest.NewScriptedProvider(t,
			agenttest.ToolUseResponse("c1", "ask_human_confirm", json.RawMessage(`{"question":"Proceed?"}`)),
			agenttest.TextResponse("nobody was there to ask"),
		),
	}, events, agenttest.NewScriptedPrompter(t).NoOperator())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

	results := events.ToolResults()
	g.Expect(results).To(HaveLen(1))
	g.Expect(results[0].IsError).To(BeFalse(), "no operator is an answer the model reasons about, not a tool failure")
	g.Expect(results[0].Output).To(ContainSubstring("no interactive terminal"))
}
