//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package builtin_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/toolkit/builtin"
)

// The config constants are what an operator writes into harness.allow and what a
// program scripting a model provider sends, so a name that drifts from the tool it
// names breaks both without either noticing.
var _ = Describe("The built-in tool names", func() {
	It("Should be the names the tools register under", func() {
		hitl := &config.Config{Harness: config.HarnessConfig{
			HumanInTheLoop: &config.HumanInTheLoopConfig{Enabled: true},
		}}
		got := make([]string, 0, 3)
		for _, t := range builtin.HITLTools(hitl) {
			got = append(got, t.Definition(false).Name)
		}
		Expect(got).To(ConsistOf(
			config.AskHumanConfirmToolName,
			config.AskHumanSelectToolName,
			config.AskHumanInputToolName,
		))

		mem := &config.Config{Harness: config.HarnessConfig{
			Memory: &config.MemoryConfig{Enabled: true},
		}}
		got = got[:0]
		for _, t := range builtin.MemoryTools(mem, nil) {
			got = append(got, t.Definition(false).Name)
		}
		Expect(got).To(ConsistOf(
			config.MemoryListToolName,
			config.MemoryReadToolName,
			config.MemoryWriteToolName,
			config.MemoryDeleteToolName,
		))

		knowledge := &config.Config{Harness: config.HarnessConfig{
			RAG: &config.RAGConfig{Enabled: true},
		}}
		got = got[:0]
		for _, t := range builtin.RAGTools(knowledge, nil) {
			got = append(got, t.Definition(false).Name)
		}
		Expect(got).To(ConsistOf(
			config.KnowledgeSearchToolName,
			config.KnowledgeEnumerateToolName,
		))
	})
})
