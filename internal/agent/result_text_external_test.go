//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// These tests cover Result.Text, which exists for a caller that hosts a run without
// rendering its event stream and so cannot reach the answer any other way.
package agent_test

import (
	"context"
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
)

var _ = Describe("Result.Text", func() {
	// The ordinary case: the run completes and Text is the terminal turn's prose,
	// concatenated across its text blocks.
	It("Should report the final answer", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		provider := agenttest.NewScriptedProvider(GinkgoTB(), &llm.Response{
			StopReason: llm.StopEndTurn,
			Content: []llm.ContentBlock{
				{Text: &llm.TextBlock{Text: "the answer is "}},
				{Text: &llm.TextBlock{Text: "42"}},
			},
		})

		res, err := agent.Run(context.Background(), agent.Options{
			Config:     agenttest.Config(GinkgoTB(), app),
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			Provider:   provider,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))

		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))
		Expect(res.Text).To(Equal("the answer is 42"))
	})

	// This is why the field exists. A run stopped by the iteration cap never produces a
	// turn marked terminal, so a caller watching only for one would record nothing, but
	// the text the model had reached is still the best account of where it got to.
	It("Should survive a non-terminal ending", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		provider := agenttest.NewScriptedProvider(GinkgoTB(), &llm.Response{
			StopReason: llm.StopToolUse,
			Content: []llm.ContentBlock{
				{Text: &llm.TextBlock{Text: "checking the subject first"}},
				{ToolUse: &llm.ToolUseBlock{ID: "call-1", Name: "do", Input: json.RawMessage(`{"subject":"x"}`)}},
			},
		})

		res, err := agent.Run(context.Background(), agent.Options{
			Config:     agenttest.Config(GinkgoTB(), app, agenttest.WithMaxIterations(1)),
			ConfigFile: "agent.yaml",
			Prompt:     []string{"keep working"},
			Provider:   provider,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))

		Expect(err).To(MatchError(ContainSubstring("max iterations")))
		Expect(res.Reason).To(Equal(runstate.ReasonMaxIterations))
		Expect(res.Text).To(Equal("checking the subject first"))
	})

	// A run that only ever called tools reports no answer rather than inventing one.
	It("Should be empty when nothing was said", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		provider := agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("call-1", "do", json.RawMessage(`{"subject":"x"}`)),
		)

		res, err := agent.Run(context.Background(), agent.Options{
			Config:     agenttest.Config(GinkgoTB(), app, agenttest.WithMaxIterations(1)),
			ConfigFile: "agent.yaml",
			Prompt:     []string{"keep working"},
			Provider:   provider,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))

		Expect(err).To(MatchError(ContainSubstring("max iterations")))
		Expect(res.Text).To(BeEmpty())
	})
})
