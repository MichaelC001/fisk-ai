//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// These tests cover Result.Text, which exists for a caller that hosts a run without
// rendering its event stream and so cannot reach the answer any other way.
package agent_test

import (
	"context"
	"encoding/json"
	"testing"

	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
)

// TestResultText_ReportsTheFinalAnswer covers the ordinary case: the run completes and
// Text is the terminal turn's prose, concatenated across its text blocks.
func TestResultText_ReportsTheFinalAnswer(t *testing.T) {
	g := NewWithT(t)

	app := agenttest.NewFakeApp(t, exampleApp())
	provider := agenttest.NewScriptedProvider(t, &llm.Response{
		StopReason: llm.StopEndTurn,
		Content: []llm.ContentBlock{
			{Text: &llm.TextBlock{Text: "the answer is "}},
			{Text: &llm.TextBlock{Text: "42"}},
		},
	})

	res, err := agent.Run(context.Background(), agent.Options{
		Config:     agenttest.Config(t, app),
		ConfigFile: "agent.yaml",
		Prompt:     []string{"go"},
		Provider:   provider,
	}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.Reason).To(Equal(runstate.ReasonCompleted))
	g.Expect(res.Text).To(Equal("the answer is 42"))
}

// TestResultText_SurvivesANonTerminalEnding is why the field exists. A run stopped by
// the iteration cap never produces a turn marked terminal, so a caller watching only
// for one would record nothing, but the text the model had reached is still the best
// account of where it got to.
func TestResultText_SurvivesANonTerminalEnding(t *testing.T) {
	g := NewWithT(t)

	app := agenttest.NewFakeApp(t, exampleApp())
	provider := agenttest.NewScriptedProvider(t, &llm.Response{
		StopReason: llm.StopToolUse,
		Content: []llm.ContentBlock{
			{Text: &llm.TextBlock{Text: "checking the subject first"}},
			{ToolUse: &llm.ToolUseBlock{ID: "call-1", Name: "do", Input: json.RawMessage(`{"subject":"x"}`)}},
		},
	})

	res, err := agent.Run(context.Background(), agent.Options{
		Config:     agenttest.Config(t, app, agenttest.WithMaxIterations(1)),
		ConfigFile: "agent.yaml",
		Prompt:     []string{"keep working"},
		Provider:   provider,
	}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))

	g.Expect(err).To(MatchError(ContainSubstring("max iterations")))
	g.Expect(res.Reason).To(Equal(runstate.ReasonMaxIterations))
	g.Expect(res.Text).To(Equal("checking the subject first"))
}

// TestResultText_IsEmptyWhenNothingWasSaid proves a run that only ever called tools
// reports no answer rather than inventing one.
func TestResultText_IsEmptyWhenNothingWasSaid(t *testing.T) {
	g := NewWithT(t)

	app := agenttest.NewFakeApp(t, exampleApp())
	provider := agenttest.NewScriptedProvider(t,
		agenttest.ToolUseResponse("call-1", "do", json.RawMessage(`{"subject":"x"}`)),
	)

	res, err := agent.Run(context.Background(), agent.Options{
		Config:     agenttest.Config(t, app, agenttest.WithMaxIterations(1)),
		ConfigFile: "agent.yaml",
		Prompt:     []string{"keep working"},
		Provider:   provider,
	}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))

	g.Expect(err).To(MatchError(ContainSubstring("max iterations")))
	g.Expect(res.Text).To(BeEmpty())
}
