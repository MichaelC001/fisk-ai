//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agent_test

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/llm"
)

// The system notes are built from the assembled names, so a built-in a filter
// removed is never named to the model as a tool it can call.
var _ = Describe("the system notes", func() {
	// systemPrompt runs cfg once against a provider that answers with text and returns
	// the system prompt the first model call carried.
	systemPrompt := func(cfg *config.Config) string {
		GinkgoHelper()

		provider := agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done"))
		_, err := agent.Run(context.Background(), agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			Provider:   provider,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())

		requests := provider.Requests()
		Expect(requests).NotTo(BeEmpty())

		return strings.Join(requests[0].SystemBlocks, "\n")
	}

	// toolNamesOf is the names of the tools a request offered.
	toolNamesOf := func(req llm.Request) []string {
		out := make([]string, 0, len(req.Tools))
		for _, td := range req.Tools {
			out = append(out, td.Name)
		}

		return out
	}

	It("Should omit a knowledge tool a filter removed", func() {
		cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), exampleApp()), agenttest.WithRAG())
		cfg.RootDirectory = GinkgoT().TempDir()
		cfg.Exclude = &config.ToolFilter{Tools: []string{"^knowledge_enumerate$"}}

		system := systemPrompt(cfg)
		Expect(system).To(ContainSubstring("knowledge_search"))
		Expect(system).NotTo(ContainSubstring("knowledge_enumerate"))
	})

	It("Should say nothing about knowledge when every knowledge tool is removed", func() {
		cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), exampleApp()), agenttest.WithRAG())
		cfg.RootDirectory = GinkgoT().TempDir()
		cfg.Exclude = &config.ToolFilter{Tools: []string{"^knowledge_"}}

		Expect(systemPrompt(cfg)).NotTo(ContainSubstring("knowledge"))
	})

	It("Should not ask for a write when a filter removed memory_write", func() {
		cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), exampleApp()), agenttest.WithMemory())
		cfg.Exclude = &config.ToolFilter{Tools: []string{"^memory_write$", "^memory_delete$"}}

		provider := agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done"))
		_, err := agent.Run(context.Background(), agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			Provider:   provider,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())

		req := provider.Requests()[0]
		Expect(toolNamesOf(req)).To(ContainElements("memory_list", "memory_read"))
		Expect(toolNamesOf(req)).NotTo(ContainElements("memory_write", "memory_delete"))

		system := strings.Join(req.SystemBlocks, "\n")
		Expect(system).To(ContainSubstring("memory_list and memory_read"))
		Expect(system).NotTo(ContainSubstring("memory_write"))
		Expect(system).NotTo(ContainSubstring("Write a memory when"))
	})
})
