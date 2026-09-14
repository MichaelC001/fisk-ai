//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"time"

	"github.com/choria-io/fisk"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// memoryListApp is an application whose "memory list" command loads as the tool
// memory_list, the name of a memory built-in.
func memoryListApp() *fisk.Application {
	app := fisk.New("app", "an app")
	app.Command("status", "show the status")
	app.Command("memory", "memory commands").Command("list", "list memories")

	return app
}

var _ = Describe("mcp served tools", func() {
	var (
		ctx    context.Context
		cancel context.CancelFunc
		cfg    *config.Config
		notes  bytes.Buffer
	)

	BeforeEach(func() {
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		DeferCleanup(cancel)

		notes.Reset()
		cfg = agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), infoApp()))
	})

	// exposeKnowledge enables knowledge on a fresh index directory and lists the
	// given tools in expose.agent.mcp.builtins.
	exposeKnowledge := func(builtins ...string) {
		cfg.Harness.RAG = &config.RAGConfig{Enabled: true, Directory: GinkgoT().TempDir()}
		cfg.Expose = &config.ExposeConfig{Agent: &config.AgentExpose{MCP: &config.ExposedMCPConfig{Builtins: builtins}}}
	}

	// served assembles the set as the command does and closes the store after the spec.
	served := func() *agent.Assembly {
		GinkgoHelper()

		asm, release, err := mcpServedTools(ctx, cfg, &notes)
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(release)

		return asm
	}

	servedNames := func(asm *agent.Assembly) []string {
		names := make([]string, 0, asm.Len())
		for _, t := range asm.Tools {
			names = append(names, t.Tool.Name())
		}

		return names
	}

	renderNotes := func(asm *agent.Assembly) string {
		var buf bytes.Buffer
		printMCPNotes(&buf, cfg, asm)

		return buf.String()
	}

	It("Should serve the listed knowledge tools and no other built-in", func() {
		cfg.Harness.HumanInTheLoop = &config.HumanInTheLoopConfig{Enabled: true}
		cfg.Harness.Memory = &config.MemoryConfig{Enabled: true}
		exposeKnowledge(config.KnowledgeSearchToolName, config.KnowledgeEnumerateToolName)

		asm := served()
		Expect(servedNames(asm)).To(Equal([]string{"backup", "status", "knowledge_search", "knowledge_enumerate"}))
		Expect(asm.Names(toolkit.KindBuiltin, "")).To(Equal([]string{"knowledge_search", "knowledge_enumerate"}))

		Expect(notes.String()).To(ContainSubstring("knowledge tier: lexical"))
		Expect(notes.String()).To(ContainSubstring("note: the knowledge index is not built yet; knowledge_search will return index_not_built until it is\nrun: fisk knowledge index\n"))

		out := renderNotes(asm)
		Expect(out).To(Equal("note: 7 built-in tool(s) this config enables are not served over MCP: ask_human_confirm, ask_human_select, ask_human_input, memory_list, memory_read, memory_write, memory_delete. They need operator state or an operator at a terminal, so they are reachable only in an agent run\n"))
	})

	It("Should open no store and note that knowledge is not exposed", func() {
		cfg.Harness.RAG = &config.RAGConfig{Enabled: true, Directory: GinkgoT().TempDir()}

		asm := served()
		Expect(servedNames(asm)).To(Equal([]string{"backup", "status"}))
		Expect(notes.String()).To(BeEmpty())

		Expect(renderNotes(asm)).To(Equal("note: knowledge is enabled but not exposed over MCP; add knowledge_search and knowledge_enumerate to expose.agent.mcp.builtins to let MCP clients search your knowledge base\n"))
	})

	It("Should note the missing half of the knowledge set", func() {
		exposeKnowledge(config.KnowledgeSearchToolName)

		asm := served()
		Expect(servedNames(asm)).To(Equal([]string{"backup", "status", "knowledge_search"}))

		out := renderNotes(asm)
		Expect(out).ToNot(ContainSubstring("knowledge is enabled but not exposed"))
		Expect(out).To(Equal("note: knowledge_search is exposed but knowledge_enumerate is not; clients can rank results but cannot tell an absent term from a low-scoring one. Add knowledge_enumerate to expose.agent.mcp.builtins to serve both\n"))
	})

	It("Should serve the listed harness tool and withhold the unlisted one", func() {
		cfg.Harness.Tools = []config.HarnessToolConfig{
			{Name: config.ReadFileToolName, Confirm: true},
			{Name: config.Base64EncodeToolName},
		}
		cfg.Expose = &config.ExposeConfig{Agent: &config.AgentExpose{MCP: &config.ExposedMCPConfig{Builtins: []string{config.Base64EncodeToolName}}}}

		asm := served()
		Expect(servedNames(asm)).To(Equal([]string{"backup", "status", "base64_encode"}))
		Expect(asm.Withheld).To(Equal([]agent.Withheld{{Tool: "read_file", Reason: agent.WithheldNotListed}}))
		Expect(notes.String()).To(BeEmpty())
		Expect(renderNotes(asm)).To(BeEmpty())
	})

	It("Should serve a command named after a withheld memory built-in", func() {
		cfg = agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), memoryListApp()), agenttest.WithMemory())

		asm := served()
		Expect(servedNames(asm)).To(Equal([]string{"status", "memory_list"}))
		Expect(asm.Tools[1].Kind).To(Equal(toolkit.KindApplication))
		Expect(renderNotes(asm)).To(ContainSubstring("not served over MCP: memory_list, memory_read, memory_write, memory_delete."))
	})

	// mcpServedTools passes no client and no session under Strict, so an assembler
	// that consulted either block would fail rather than serve.
	It("Should serve none of the remote or MCP tools and dial nothing for them", func() {
		cfg.NatsContext = "nowhere"
		cfg.RemoteTools = []config.RemoteToolHost{{Name: "peer"}}
		cfg.MCPClients = []config.MCPServer{{Name: "docs", Command: "/nonexistent/mcp-server"}}

		asm := served()
		Expect(servedNames(asm)).To(Equal([]string{"backup", "status"}))
		Expect(asm.Remote).To(BeEmpty())
		Expect(asm.MCP).To(BeEmpty())
		Expect(asm.Problems).To(BeEmpty())
		Expect(renderNotes(asm)).To(BeEmpty())
	})
})
