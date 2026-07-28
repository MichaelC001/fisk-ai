// Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("MCP builtins allowlist", func() {
	build := func(builtins []string, ragEnabled bool) *Config {
		cfg := &Config{
			Expose: &ExposeConfig{Agent: &AgentExpose{MCP: &ExposedMCPConfig{Port: 8080, Builtins: builtins}}},
		}
		if ragEnabled {
			cfg.Harness.RAG = &RAGConfig{Enabled: true}
		}

		return cfg
	}

	It("accepts knowledge_search when knowledge is enabled, trimming and de-duplicating", func() {
		cfg := build([]string{"knowledge_search", " knowledge_search "}, true)
		Expect(cfg.prepare()).To(Succeed())
		Expect(cfg.MCPBuiltins()).To(Equal([]string{"knowledge_search"}))
		Expect(cfg.MCPExposesKnowledge()).To(BeTrue())
	})

	It("accepts both knowledge tools, preserving the operator's order", func() {
		cfg := build([]string{"knowledge_enumerate", "knowledge_search"}, true)
		Expect(cfg.prepare()).To(Succeed())
		Expect(cfg.MCPBuiltins()).To(Equal([]string{"knowledge_enumerate", "knowledge_search"}))
		Expect(cfg.MCPExposesKnowledge()).To(BeTrue())
	})

	// The store is opened for the group, not for one name, so an enumerate-only
	// allowlist must still open it or the operator is served nothing at all.
	It("reports knowledge exposed when only knowledge_enumerate is listed", func() {
		cfg := build([]string{"knowledge_enumerate"}, true)
		Expect(cfg.prepare()).To(Succeed())
		Expect(cfg.MCPExposesKnowledge()).To(BeTrue())
	})

	It("rejects a real but unexposable built-in, naming the accepted set", func() {
		cfg := build([]string{"ask_human_confirm"}, true)
		err := cfg.prepare()
		Expect(err).To(MatchError(ContainSubstring("is not an accepted built-in name")))
		Expect(err).To(MatchError(ContainSubstring("knowledge_search")))
		Expect(err).To(MatchError(ContainSubstring("knowledge_enumerate")))
		Expect(err).To(MatchError(ContainSubstring("an operator at a terminal")))
	})

	It("rejects an unknown built-in name", func() {
		cfg := build([]string{"frobnicate"}, true)
		Expect(cfg.prepare()).To(MatchError(ContainSubstring("is not an accepted built-in name")))
	})

	It("rejects knowledge_search when knowledge is not enabled", func() {
		cfg := build([]string{"knowledge_search"}, false)
		Expect(cfg.prepare()).To(MatchError(ContainSubstring("knowledge is not enabled")))
	})

	It("names what was listed when knowledge is not enabled", func() {
		cfg := build([]string{"knowledge_enumerate"}, false)
		Expect(cfg.prepare()).To(MatchError(ContainSubstring("lists knowledge_enumerate")))
	})

	It("is a no-op with no builtins listed", func() {
		cfg := build(nil, false)
		Expect(cfg.prepare()).To(Succeed())
		Expect(cfg.MCPExposesKnowledge()).To(BeFalse())
	})
})
