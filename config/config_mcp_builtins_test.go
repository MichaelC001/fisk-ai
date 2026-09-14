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
		Expect(cfg.Prepare()).To(Succeed())
		Expect(cfg.MCPBuiltins()).To(Equal([]string{"knowledge_search"}))
		Expect(cfg.MCPExposesKnowledge()).To(BeTrue())
	})

	It("accepts both knowledge tools, preserving the operator's order", func() {
		cfg := build([]string{"knowledge_enumerate", "knowledge_search"}, true)
		Expect(cfg.Prepare()).To(Succeed())
		Expect(cfg.MCPBuiltins()).To(Equal([]string{"knowledge_enumerate", "knowledge_search"}))
		Expect(cfg.MCPExposesKnowledge()).To(BeTrue())
	})

	// The store is opened for the group, not for one name, so an enumerate-only
	// allowlist must still open it or the operator is served nothing at all.
	It("reports knowledge exposed when only knowledge_enumerate is listed", func() {
		cfg := build([]string{"knowledge_enumerate"}, true)
		Expect(cfg.Prepare()).To(Succeed())
		Expect(cfg.MCPExposesKnowledge()).To(BeTrue())
	})

	// knowledge_read has a gate of its own, so an operator who allowlisted it without
	// setting the key would be served the other two and told nothing.
	It("rejects knowledge_read when read_tool is not set", func() {
		cfg := build([]string{"knowledge_read"}, true)
		Expect(cfg.Prepare()).To(MatchError(ContainSubstring("harness.knowledge.read_tool is not set")))
	})

	It("accepts knowledge_read when read_tool is set", func() {
		cfg := build([]string{"knowledge_search", "knowledge_read"}, true)
		cfg.Harness.RAG.ReadTool = true
		Expect(cfg.Prepare()).To(Succeed())
		Expect(cfg.MCPBuiltins()).To(Equal([]string{"knowledge_search", "knowledge_read"}))
		Expect(cfg.MCPExposesKnowledge()).To(BeTrue())
	})

	It("rejects a real but unexposable built-in, naming the accepted set", func() {
		cfg := build([]string{"ask_human_confirm"}, true)
		err := cfg.Prepare()
		Expect(err).To(MatchError(ContainSubstring("is not an accepted built-in name")))
		Expect(err).To(MatchError(ContainSubstring("accepted: knowledge_search, knowledge_enumerate, knowledge_read, read_file, base64_encode")))
		Expect(err).To(MatchError(ContainSubstring("an operator at a terminal")))
	})

	It("rejects a harness.tools built-in that harness.tools does not enable", func() {
		cfg := build([]string{"read_file"}, true)
		err := cfg.Prepare()
		Expect(err).To(MatchError(ContainSubstring("lists read_file but harness.tools does not enable it")))
		Expect(err).To(MatchError(ContainSubstring("add a harness.tools entry with 'name: read_file'")))
	})

	// Neither opens the knowledge store, so selecting them alone needs no knowledge
	// block and must not make fisk mcp open one.
	It("accepts both harness.tools built-ins with no knowledge block", func() {
		cfg := build([]string{"read_file", "base64_encode"}, false)
		cfg.Harness.Tools = []HarnessToolConfig{{Name: "read_file"}, {Name: "base64_encode"}}
		Expect(cfg.Prepare()).To(Succeed())
		Expect(cfg.MCPBuiltins()).To(Equal([]string{"read_file", "base64_encode"}))
		Expect(cfg.MCPExposesKnowledge()).To(BeFalse())
	})

	It("rejects a knowledge name beside a harness.tools built-in when knowledge is not enabled", func() {
		cfg := build([]string{"base64_encode", "knowledge_search"}, false)
		cfg.Harness.Tools = []HarnessToolConfig{{Name: "base64_encode"}}
		Expect(cfg.Prepare()).To(MatchError(ContainSubstring("lists knowledge_search but knowledge is not enabled")))
	})

	It("rejects an unknown built-in name", func() {
		cfg := build([]string{"frobnicate"}, true)
		Expect(cfg.Prepare()).To(MatchError(ContainSubstring("is not an accepted built-in name")))
	})

	It("rejects knowledge_search when knowledge is not enabled", func() {
		cfg := build([]string{"knowledge_search"}, false)
		Expect(cfg.Prepare()).To(MatchError(ContainSubstring("knowledge is not enabled")))
	})

	It("names what was listed when knowledge is not enabled", func() {
		cfg := build([]string{"knowledge_enumerate"}, false)
		Expect(cfg.Prepare()).To(MatchError(ContainSubstring("lists knowledge_enumerate")))
	})

	It("is a no-op with no builtins listed", func() {
		cfg := build(nil, false)
		Expect(cfg.Prepare()).To(Succeed())
		Expect(cfg.MCPExposesKnowledge()).To(BeFalse())
	})
})
