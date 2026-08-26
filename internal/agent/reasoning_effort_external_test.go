//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agent_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/runstate"
)

var _ = Describe("llm.reasoning_effort", func() {
	// This drives the whole path the key exists for: the configuration names a level,
	// and every model call the run makes carries it. Which levels a model takes is the
	// model's own list, so a level this build does not name travels unchanged and is
	// refused by the provider rather than here.
	It("Should reach every request", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		provider := agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("c1", "do", []byte(`{"subject":"hello"}`)),
			agenttest.TextResponse("done"),
		)

		cfg := agenttest.Config(GinkgoTB(), app)
		cfg.LLM.ReasoningEffort = "ludicrous"

		res, err := agent.Run(context.Background(), agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"do the thing"},
			Provider:   provider,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

		reqs := provider.Requests()
		Expect(reqs).To(HaveLen(2))
		for i, req := range reqs {
			Expect(req.ReasoningEffort).To(Equal("ludicrous"), "request %d", i)
		}
	})

	// This is the state that must stay silent: a configuration naming no level sends
	// none, so a model that rejects the parameter is unaffected by the key existing.
	It("Should ask for nothing when absent", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		provider := agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done"))

		_, err := agent.Run(context.Background(), agent.Options{
			Config:     agenttest.Config(GinkgoTB(), app),
			ConfigFile: "agent.yaml",
			Prompt:     []string{"do the thing"},
			Provider:   provider,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())

		Expect(provider.Requests()).To(HaveLen(1))
		Expect(provider.Requests()[0].ReasoningEffort).To(BeEmpty())
	})
})
