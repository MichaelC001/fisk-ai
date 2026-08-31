//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package config_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
)

// A run refuses to start with no tool to call. SuppliesTools is how a caller that
// injects none asks that question before opening anything.
var _ = Describe("Config.SuppliesTools", func() {
	It("Should report false for a config naming no source of tools", func() {
		cfg := &config.Config{Identity: "agent", SystemPrompt: "p"}
		cfg.LLM.Model = "m"
		Expect(cfg.Prepare()).To(Succeed())

		Expect(cfg.SuppliesTools()).To(BeFalse())
	})

	It("Should report true for each source on its own", func() {
		withApp := &config.Config{ApplicationPath: "/usr/bin/abt"}
		Expect(withApp.SuppliesTools()).To(BeTrue())

		withHITL := &config.Config{Harness: config.HarnessConfig{
			HumanInTheLoop: &config.HumanInTheLoopConfig{Enabled: true},
		}}
		Expect(withHITL.SuppliesTools()).To(BeTrue())

		withMemory := &config.Config{Harness: config.HarnessConfig{
			Memory: &config.MemoryConfig{Enabled: true},
		}}
		Expect(withMemory.SuppliesTools()).To(BeTrue())

		withKnowledge := &config.Config{Harness: config.HarnessConfig{
			RAG: &config.RAGConfig{Enabled: true},
		}}
		Expect(withKnowledge.SuppliesTools()).To(BeTrue())

		withRemote := &config.Config{RemoteTools: []config.RemoteToolHost{{Name: "peer"}}}
		Expect(withRemote.SuppliesTools()).To(BeTrue())

		withMCP := &config.Config{MCPClients: []config.MCPServer{{Name: "fs", Command: "/usr/bin/fs"}}}
		Expect(withMCP.SuppliesTools()).To(BeTrue())
	})

	It("Should report false for a harness block that names a source but disables it", func() {
		cfg := &config.Config{Harness: config.HarnessConfig{
			Memory:         &config.MemoryConfig{Enabled: false},
			HumanInTheLoop: &config.HumanInTheLoopConfig{Enabled: false},
			RAG:            &config.RAGConfig{Enabled: false},
		}}

		Expect(cfg.SuppliesTools()).To(BeFalse())
	})
})
