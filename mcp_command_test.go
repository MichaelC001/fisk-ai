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

	// enableKnowledge enables knowledge on a fresh index directory.
	enableKnowledge := func() {
		cfg.Harness.RAG = &config.RAGConfig{Enabled: true, Directory: GinkgoT().TempDir()}
	}

	// exposeExcluding sets expose.agent.tools to exclude the given names.
	exposeExcluding := func(names ...string) {
		cfg.Expose = &config.ExposeConfig{Agent: &config.AgentExpose{
			MCP:   &config.ExposedMCPConfig{},
			Tools: &config.ExposedToolSelection{Exclude: &config.ToolFilter{Tools: names}},
		}}
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
		printMCPNotes(&buf, asm)

		return buf.String()
	}

	// Knowledge enabled is the whole opt-in: the store is opened and both knowledge
	// tools are served with no allowlist step.
	It("Should serve the knowledge tools on harness.knowledge alone and no other built-in", func() {
		cfg.Harness.HumanInTheLoop = &config.HumanInTheLoopConfig{Enabled: true}
		cfg.Harness.Memory = &config.MemoryConfig{Enabled: true}
		enableKnowledge()

		asm := served()
		Expect(servedNames(asm)).To(Equal([]string{"backup", "status", "knowledge_search", "knowledge_enumerate"}))
		Expect(asm.Names(toolkit.KindBuiltin, "")).To(Equal([]string{"knowledge_search", "knowledge_enumerate"}))

		Expect(notes.String()).To(ContainSubstring("knowledge tier: lexical"))
		Expect(notes.String()).To(ContainSubstring("note: the knowledge index is not built yet; knowledge_search will return index_not_built until it is\nrun: fisk knowledge index\n"))

		out := renderNotes(asm)
		Expect(out).To(Equal("note: 7 built-in tool(s) this config enables are not served over MCP: ask_human_confirm, ask_human_select, ask_human_input, memory_list, memory_read, memory_write, memory_delete. They need operator state or an operator at a terminal, so they are reachable only in an agent run\n"))
	})

	It("Should open no store with knowledge off", func() {
		asm := served()
		Expect(servedNames(asm)).To(Equal([]string{"backup", "status"}))
		Expect(notes.String()).To(BeEmpty())
		Expect(renderNotes(asm)).To(BeEmpty())
	})

	It("Should note the missing half of a knowledge set the filters split", func() {
		enableKnowledge()
		exposeExcluding("^knowledge_enumerate$")

		asm := served()
		Expect(servedNames(asm)).To(Equal([]string{"backup", "status", "knowledge_search"}))
		Expect(asm.Withheld).To(Equal([]agent.Withheld{{Tool: "knowledge_enumerate", Reason: agent.WithheldFiltered}}))

		Expect(renderNotes(asm)).To(Equal("note: 1 built-in tool(s) this config enables are not served over MCP: knowledge_enumerate. They are excluded by include, exclude or expose.agent.tools\n" +
			"note: knowledge_search is served but knowledge_enumerate is not; clients can rank results but cannot tell an absent term from a low-scoring one. Let knowledge_enumerate through include, exclude and expose.agent.tools to serve both\n"))
	})

	It("Should serve every enabled harness tool and withhold the one a filter excludes", func() {
		cfg.Harness.Tools = []config.HarnessToolConfig{
			{Name: config.ReadFileToolName, Confirm: true},
			{Name: config.Base64EncodeToolName},
		}

		asm := served()
		Expect(servedNames(asm)).To(Equal([]string{"backup", "status", "read_file", "base64_encode"}))
		Expect(asm.Withheld).To(BeEmpty())

		exposeExcluding("^read_file$")

		asm = served()
		Expect(servedNames(asm)).To(Equal([]string{"backup", "status", "base64_encode"}))
		Expect(asm.Withheld).To(Equal([]agent.Withheld{{Tool: "read_file", Reason: agent.WithheldFiltered}}))
		Expect(notes.String()).To(BeEmpty())
		Expect(renderNotes(asm)).To(Equal("note: 1 built-in tool(s) this config enables are not served over MCP: read_file. They are excluded by include, exclude or expose.agent.tools\n"))
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
