//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// The remote host is a fake a2a transport and the MCP servers are real mcp.Servers
// over in-memory transports.
package agent_test

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/choria-io/fisk"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/a2a"
	wire "github.com/choria-io/fisk-ai/internal/a2a/wire/v1"
	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/mcpclient"
	"github.com/choria-io/fisk-ai/internal/rag"
	"github.com/choria-io/fisk-ai/internal/telemetry"
	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// remoteClient is an a2a client over a fake transport that answers discovery for
// every host with the given tools.
func remoteClient(tools ...string) *a2a.Client {
	GinkgoHelper()

	card := wire.AgentCard{Name: "peer", Version: "1.0.0"}
	for _, name := range tools {
		card.Tools = append(card.Tools, wire.ToolDescriptor{
			Name:        name,
			Description: name + " on the peer",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		})
	}

	client, err := a2a.NewClient(agenttest.NewFakeTransport(GinkgoTB(), card), "agent")
	Expect(err).NotTo(HaveOccurred())

	return client
}

// deadHostClient is remoteClient with discovery of one host failing.
func deadHostClient(dead string, tools ...string) *a2a.Client {
	GinkgoHelper()

	card := wire.AgentCard{Name: "peer", Version: "1.0.0"}
	for _, name := range tools {
		card.Tools = append(card.Tools, wire.ToolDescriptor{
			Name:        name,
			Description: name + " on the peer",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		})
	}

	transport := agenttest.NewFakeTransport(GinkgoTB(), card)
	transport.SetFaults(agenttest.TransportFault{Agent: dead, Err: a2a.ErrNoResponders})

	client, err := a2a.NewClient(transport, "agent")
	Expect(err).NotTo(HaveOccurred())

	return client
}

// openRAG opens a read-only knowledge store for cfg in a fresh directory. The index
// is never built, which the store treats as an empty index rather than a failure.
func openRAG(cfg *config.Config) *rag.Store {
	GinkgoHelper()

	cfg.Harness.RAG = &config.RAGConfig{Enabled: true, Directory: GinkgoT().TempDir()}

	store, err := rag.Open(cfg, "", rag.Options{})
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() { store.Close() })

	return store
}

// assembledNames is the names of the assembled tools, in order.
func assembledNames(a *agent.Assembly) []string {
	out := make([]string, 0, len(a.Tools))
	for _, t := range a.Tools {
		out = append(out, t.Tool.Name())
	}

	return out
}

// partNames is the names of one deferrable part or of the built-ins, in order.
func partNames(tools []toolkit.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name())
	}

	return out
}

// withheldNames is the names of the withheld built-ins, in order.
func withheldNames(a *agent.Assembly) []string {
	out := make([]string, 0, len(a.Withheld))
	for _, w := range a.Withheld {
		out = append(out, w.Tool)
	}

	return out
}

// memoryListApp is an application whose "memory list" command loads as the tool
// memory_list, the name of a memory built-in.
func memoryListApp() *fisk.Application {
	app := fisk.New("app", "an app")
	app.Command("do", "do a thing")
	app.Command("memory", "memory commands").Command("list", "list memories")

	return app
}

var _ = Describe("Assemble", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("Should assemble every source in claiming order with its kind and source", func() {
		fake := &mcpFakeServers{tools: []*mcp.Tool{mcpDescriptor("search", "Searches the documentation")}}
		sessions := connectMCP(GinkgoTB(), fake, config.MCPServer{Name: "docs"})

		cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), exampleApp()),
			agenttest.WithHITL(), agenttest.WithMemory(), agenttest.WithRAG())
		cfg.RemoteTools = []config.RemoteToolHost{{Name: "weather-svc"}}
		cfg.MCPClients = []config.MCPServer{{Name: "docs"}}

		a, err := agent.Assemble(ctx, cfg, agent.Sources{
			Remote: remoteClient("forecast", "do"),
			MCP:    []*mcpclient.Sessions{sessions},
			Custom: []toolkit.Tool{plainCustomTool("zeta"), plainCustomTool("alpha")},
		}, agent.SurfaceRun, agent.Strict)
		Expect(err).NotTo(HaveOccurred())

		Expect(assembledNames(a)).To(Equal([]string{
			"do",
			"ask_human_confirm", "ask_human_select", "ask_human_input",
			"memory_list", "memory_read", "memory_write", "memory_delete",
			"knowledge_search", "knowledge_enumerate",
			"forecast", "weather-svc_do",
			"docs_search",
			"alpha", "zeta",
		}))

		kinds := make([]toolkit.Kind, 0, len(a.Tools))
		sources := make([]string, 0, len(a.Tools))
		for _, t := range a.Tools {
			kinds = append(kinds, t.Kind)
			sources = append(sources, t.Source)
		}
		Expect(kinds).To(Equal([]toolkit.Kind{
			toolkit.KindApplication,
			toolkit.KindBuiltin, toolkit.KindBuiltin, toolkit.KindBuiltin,
			toolkit.KindBuiltin, toolkit.KindBuiltin, toolkit.KindBuiltin, toolkit.KindBuiltin,
			toolkit.KindBuiltin, toolkit.KindBuiltin,
			toolkit.KindRemote, toolkit.KindRemote,
			toolkit.KindMCP,
			toolkit.KindCustom, toolkit.KindCustom,
		}))
		Expect(sources).To(Equal([]string{
			agent.SourceApplication,
			agent.SourceHumanInTheLoop, agent.SourceHumanInTheLoop, agent.SourceHumanInTheLoop,
			agent.SourceMemory, agent.SourceMemory, agent.SourceMemory, agent.SourceMemory,
			agent.SourceKnowledge, agent.SourceKnowledge,
			"weather-svc", "weather-svc",
			"docs",
			agent.SourceCustom, agent.SourceCustom,
		}))

		Expect(partNames(a.Builtins())).To(Equal([]string{
			"ask_human_confirm", "ask_human_select", "ask_human_input",
			"memory_list", "memory_read", "memory_write", "memory_delete",
			"knowledge_search", "knowledge_enumerate",
		}))
		Expect(partNames(a.BeforeMCP())).To(Equal([]string{"do", "forecast", "weather-svc_do"}))
		Expect(partNames(a.MCPTools())).To(Equal([]string{"docs_search"}))
		Expect(partNames(a.AfterMCP())).To(Equal([]string{"alpha", "zeta"}))

		Expect(a.Len()).To(Equal(15))
		Expect(a.Counts()).To(Equal(telemetry.ToolCounts{Application: 1, Builtin: 9, Remote: 2, MCP: 1, Custom: 2}))
		Expect(a.Names(toolkit.KindBuiltin, "")).To(HaveLen(9))
		Expect(a.Names(toolkit.KindUnknown, agent.SourceMemory)).To(Equal([]string{"memory_list", "memory_read", "memory_write", "memory_delete"}))
		Expect(a.Names(toolkit.KindUnknown, agent.SourceHumanInTheLoop)).To(Equal([]string{"ask_human_confirm", "ask_human_select", "ask_human_input"}))

		Expect(a.Problems).To(BeEmpty())
		Expect(a.Withheld).To(BeEmpty())
		Expect(a.Remote).To(HaveLen(1))
		Expect(a.Remote[0].Tools).To(HaveLen(2))
		Expect(a.MCP).To(HaveLen(1))
		Expect(a.MCP[0].Tools).To(HaveLen(1))
	})

	// The peer's "do" has the command's name and its "forecast" has nobody's, so one is
	// prefixed and one is bare.
	It("Should prefix a remote tool on a clash and an MCP tool always", func() {
		fake := &mcpFakeServers{tools: []*mcp.Tool{mcpDescriptor("forecast", "The forecast")}}
		sessions := connectMCP(GinkgoTB(), fake, config.MCPServer{Name: "weather"})

		cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), exampleApp()))
		cfg.RemoteTools = []config.RemoteToolHost{{Name: "peer", Alias: "p"}}
		cfg.MCPClients = []config.MCPServer{{Name: "weather"}}

		a, err := agent.Assemble(ctx, cfg, agent.Sources{
			Remote: remoteClient("forecast", "do"),
			MCP:    []*mcpclient.Sessions{sessions},
		}, agent.SurfaceRun, agent.Strict)
		Expect(err).NotTo(HaveOccurred())

		Expect(assembledNames(a)).To(Equal([]string{"do", "forecast", "p_do", "weather_forecast"}))
		Expect(a.Tools[2].Source).To(Equal("p"))
		Expect(a.Tools[3].Source).To(Equal("weather"))
	})

	DescribeTable("Should refuse a built-in whose name a command took under either policy",
		func(policy agent.Policy) {
			cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), memoryListApp()), agenttest.WithMemory())

			a, err := agent.Assemble(ctx, cfg, agent.Sources{}, agent.SurfaceRun, policy)
			Expect(err).To(MatchError(`memory adds a built-in tool "memory_list" but the application already exposes a tool with that name; exclude or rename it`))
			Expect(a).NotTo(BeNil())
		},
		Entry("under Strict", agent.Strict),
		Entry("under Lenient", agent.Lenient),
	)

	// Every way a custom tool is refused, with the message Run gives today.
	DescribeTable("Should refuse a custom tool that",
		func(build func() []toolkit.Tool, wantErr string) {
			cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), exampleApp()), agenttest.WithMemory())

			_, err := agent.Assemble(ctx, cfg, agent.Sources{Custom: build()}, agent.SurfaceRun, agent.Strict)
			Expect(err).To(MatchError(ContainSubstring(wantErr)))
		},
		Entry("is nil",
			func() []toolkit.Tool { return []toolkit.Tool{nil} },
			"custom tool at index 0 is nil"),
		Entry("has an empty name",
			func() []toolkit.Tool { return []toolkit.Tool{staticTool{}} },
			"custom tool at index 0 has an empty name"),
		Entry("disagrees with its definition",
			func() []toolkit.Tool { return []toolkit.Tool{staticTool{name: "a", defName: "b"}} },
			`custom tool "a" reports Definition name "b"; Name() and Definition().Name must match`),
		Entry("collides with an application tool",
			func() []toolkit.Tool { return []toolkit.Tool{plainCustomTool("do")} },
			`custom tool at index 0 ("do") collides with an existing application tool of the same name; a custom tool may not shadow it`),
		Entry("collides with a built-in tool",
			func() []toolkit.Tool { return []toolkit.Tool{plainCustomTool("memory_list")} },
			`custom tool at index 0 ("memory_list") collides with an existing built-in tool of the same name; a custom tool may not shadow it`),
		Entry("duplicates an earlier custom tool",
			func() []toolkit.Tool { return []toolkit.Tool{plainCustomTool("x"), plainCustomTool("x")} },
			`custom tool at index 1 ("x") duplicates an earlier custom tool of the same name`),
		Entry("declares the remote kind",
			func() []toolkit.Tool { return []toolkit.Tool{remoteKindTool()} },
			`custom tool "remote_kind_thing" declares the remote kind; injected tools run in-process and may not be accounted as another agent's`),
		Entry("has a remote spec",
			func() []toolkit.Tool { return []toolkit.Tool{remoteSpecTool()} },
			`custom tool "remote_thing" declares the remote kind`),
		Entry("declares the mcp kind",
			func() []toolkit.Tool { return []toolkit.Tool{mcpKindTool()} },
			`custom tool "mcp_thing" declares the mcp kind; injected tools run in-process and may not be accounted as an MCP server's`),
	)

	It("Should refuse a custom tool that takes a remote tool's name", func() {
		cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), exampleApp()))
		cfg.RemoteTools = []config.RemoteToolHost{{Name: "peer"}}

		_, err := agent.Assemble(ctx, cfg, agent.Sources{
			Remote: remoteClient("forecast"),
			Custom: []toolkit.Tool{plainCustomTool("forecast")},
		}, agent.SurfaceRun, agent.Strict)
		Expect(err).To(MatchError(`custom tool at index 0 ("forecast") collides with an existing remote tool of the same name; a custom tool may not shadow it`))
	})

	It("Should refuse a custom tool that takes an MCP tool's name", func() {
		fake := &mcpFakeServers{tools: []*mcp.Tool{mcpDescriptor("search", "Searches the documentation")}}
		sessions := connectMCP(GinkgoTB(), fake, config.MCPServer{Name: "docs"})

		cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), exampleApp()))
		cfg.MCPClients = []config.MCPServer{{Name: "docs"}}

		_, err := agent.Assemble(ctx, cfg, agent.Sources{
			MCP:    []*mcpclient.Sessions{sessions},
			Custom: []toolkit.Tool{plainCustomTool("docs_search")},
		}, agent.SurfaceRun, agent.Strict)
		Expect(err).To(MatchError(`custom tool at index 0 ("docs_search") collides with a tool of the same name imported from an mcp server; a custom tool may not shadow it`))
	})

	Describe("under Strict", func() {
		It("Should return the source's error with the partial assembly beside it", func() {
			fake := &mcpFakeServers{tools: []*mcp.Tool{mcpDescriptor("search", "Searches the documentation")}, failList: true}
			sessions := connectMCP(GinkgoTB(), fake, config.MCPServer{Name: "docs"})

			cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), exampleApp()), agenttest.WithHITL())
			cfg.MCPClients = []config.MCPServer{{Name: "docs"}}

			a, err := agent.Assemble(ctx, cfg, agent.Sources{MCP: []*mcpclient.Sessions{sessions}}, agent.SurfaceRun, agent.Strict)
			Expect(err.Error()).To(HavePrefix(`importing tools from mcp server "docs": listing the tools of mcp server "docs"`))

			Expect(a).NotTo(BeNil())
			Expect(assembledNames(a)).To(Equal([]string{"do", "ask_human_confirm", "ask_human_select", "ask_human_input"}))
			Expect(a.MCP).To(HaveLen(1))
			Expect(a.MCP[0].Server.Name).To(Equal("docs"))
			Expect(a.MCP[0].Err).To(HaveOccurred())
			Expect(a.Problems).To(BeEmpty())
		})

		It("Should return a dead host's error unchanged", func() {
			cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), exampleApp()))
			cfg.NatsContext = "lab"
			cfg.RemoteTools = []config.RemoteToolHost{{Name: "live"}, {Name: "dead"}}

			a, err := agent.Assemble(ctx, cfg, agent.Sources{Remote: deadHostClient("dead", "forecast")}, agent.SurfaceRun, agent.Strict)
			Expect(err).To(MatchError(ContainSubstring(`importing tools from remote agent "dead" on context "lab"`)))
			Expect(errors.Is(err, a2a.ErrNoResponders)).To(BeTrue())

			Expect(a.Remote).To(HaveLen(2))
			Expect(a.Remote[1].Err).To(HaveOccurred())
		})

		It("Should return the first Unbound entry's error", func() {
			dialErr := errors.New("nats: no servers available")

			cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), exampleApp()))
			cfg.RemoteTools = []config.RemoteToolHost{{Name: "peer"}}

			_, err := agent.Assemble(ctx, cfg, agent.Sources{
				Unbound: []agent.Problem{{Source: agent.SourceRemoteTools, Err: dialErr}},
			}, agent.SurfaceRun, agent.Strict)
			Expect(err).To(BeIdenticalTo(dialErr))
		})

		It("Should fail a configured host with no client", func() {
			cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), exampleApp()))
			cfg.RemoteTools = []config.RemoteToolHost{{Name: "peer"}}

			_, err := agent.Assemble(ctx, cfg, agent.Sources{}, agent.SurfaceRun, agent.Strict)
			Expect(err).To(MatchError(ContainSubstring("remote_tools is configured but Sources.Remote is nil")))
		})

		It("Should fail a configured server with no session", func() {
			cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), exampleApp()))
			cfg.MCPClients = []config.MCPServer{{Name: "docs"}}

			_, err := agent.Assemble(ctx, cfg, agent.Sources{}, agent.SurfaceRun, agent.Strict)
			Expect(err).To(MatchError(ContainSubstring(`no session was passed for mcp server "docs"`)))
		})
	})

	Describe("under Lenient", func() {
		It("Should import the live host's tools and record the dead host as a problem", func() {
			cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), exampleApp()))
			cfg.RemoteTools = []config.RemoteToolHost{{Name: "live"}, {Name: "dead"}}

			a, err := agent.Assemble(ctx, cfg, agent.Sources{Remote: deadHostClient("dead", "forecast")}, agent.SurfaceRun, agent.Lenient)
			Expect(err).NotTo(HaveOccurred())

			Expect(assembledNames(a)).To(Equal([]string{"do", "forecast"}))
			Expect(a.Tools[1].Source).To(Equal("live"))

			Expect(a.Problems).To(HaveLen(1))
			Expect(a.Problems[0].Source).To(Equal(agent.SourceRemoteTools))
			Expect(a.Problems[0].Alias).To(Equal("dead"))
			Expect(errors.Is(a.Problems[0].Err, a2a.ErrNoResponders)).To(BeTrue())

			Expect(a.Remote).To(HaveLen(2))
			Expect(a.Remote[0].Tools).To(HaveLen(1))
			Expect(a.Remote[1].Err).To(HaveOccurred())
		})

		// One Sessions per server, as fisk info connects them, so the server that cannot be
		// listed fails its own outcome and the other still contributes.
		It("Should import the live server's tools and record the dead server as a problem and a status row", func() {
			live := &mcpFakeServers{tools: []*mcp.Tool{mcpDescriptor("search", "Searches the documentation")}}
			liveSessions := connectMCP(GinkgoTB(), live, config.MCPServer{Name: "docs"})
			dead := &mcpFakeServers{tools: []*mcp.Tool{mcpDescriptor("lookup", "Looks a page up")}, failList: true}
			deadSessions := connectMCP(GinkgoTB(), dead, config.MCPServer{Name: "wiki"})

			cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), exampleApp()))
			cfg.MCPClients = []config.MCPServer{{Name: "docs"}, {Name: "wiki"}}

			a, err := agent.Assemble(ctx, cfg, agent.Sources{MCP: []*mcpclient.Sessions{liveSessions, deadSessions}}, agent.SurfaceRun, agent.Lenient)
			Expect(err).NotTo(HaveOccurred())

			Expect(assembledNames(a)).To(Equal([]string{"do", "docs_search"}))

			Expect(a.Problems).To(HaveLen(1))
			Expect(a.Problems[0].Source).To(Equal(agent.SourceMCPClients))
			Expect(a.Problems[0].Alias).To(Equal("wiki"))
			Expect(a.Problems[0].Err).To(MatchError(ContainSubstring(`listing the tools of mcp server "wiki"`)))

			Expect(a.MCP).To(HaveLen(2))
			Expect(a.MCP[0].Server.Name).To(Equal("docs"))
			Expect(a.MCP[0].Tools).To(HaveLen(1))
			Expect(a.MCP[1].Server.Name).To(Equal("wiki"))
			Expect(a.MCP[1].Err).To(HaveOccurred())
		})

		It("Should record an Unbound entry as a problem and, for a server, a status row", func() {
			dialErr := errors.New("nats: no servers available")
			connectErr := errors.New(`connecting to mcp server "docs": exec: "no-such-server": executable file not found`)

			cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), exampleApp()))
			cfg.RemoteTools = []config.RemoteToolHost{{Name: "peer"}}
			cfg.MCPClients = []config.MCPServer{{Name: "docs", Command: "no-such-server"}}

			a, err := agent.Assemble(ctx, cfg, agent.Sources{
				Unbound: []agent.Problem{
					{Source: agent.SourceRemoteTools, Err: dialErr},
					{Source: agent.SourceMCPClients, Alias: "docs", Err: connectErr},
				},
			}, agent.SurfaceRun, agent.Lenient)
			Expect(err).NotTo(HaveOccurred())

			Expect(assembledNames(a)).To(Equal([]string{"do"}))
			Expect(a.Problems).To(Equal([]agent.Problem{
				{Source: agent.SourceRemoteTools, Err: dialErr},
				{Source: agent.SourceMCPClients, Alias: "docs", Err: connectErr},
			}))
			Expect(a.Remote).To(BeEmpty())
			Expect(a.MCP).To(HaveLen(1))
			Expect(a.MCP[0].Server).To(Equal(config.MCPServer{Name: "docs", Command: "no-such-server"}))
			Expect(a.MCP[0].Err).To(MatchError(connectErr))
		})

		It("Should record a configured server with no session as a problem and a status row", func() {
			cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), exampleApp()))
			cfg.MCPClients = []config.MCPServer{{Name: "docs"}}

			a, err := agent.Assemble(ctx, cfg, agent.Sources{}, agent.SurfaceRun, agent.Lenient)
			Expect(err).NotTo(HaveOccurred())

			Expect(a.Problems).To(HaveLen(1))
			Expect(a.Problems[0].Alias).To(Equal("docs"))
			Expect(a.Problems[0].Err).To(MatchError(ContainSubstring(`no session was passed for mcp server "docs"`)))
			Expect(a.MCP).To(HaveLen(1))
			Expect(a.MCP[0].Err).To(MatchError(a.Problems[0].Err))
		})

		It("Should report a whole-block Unbound entry once and put its error on every server's row", func() {
			connectErr := errors.New("mcp: connect failed")

			cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), exampleApp()))
			cfg.MCPClients = []config.MCPServer{{Name: "docs"}, {Name: "wiki"}}

			a, err := agent.Assemble(ctx, cfg, agent.Sources{
				Unbound: []agent.Problem{{Source: agent.SourceMCPClients, Err: connectErr}},
			}, agent.SurfaceRun, agent.Lenient)
			Expect(err).NotTo(HaveOccurred())

			Expect(a.Problems).To(HaveLen(1))
			Expect(a.MCP).To(HaveLen(2))
			Expect(a.MCP[0].Server.Name).To(Equal("docs"))
			Expect(a.MCP[0].Err).To(BeIdenticalTo(connectErr))
			Expect(a.MCP[1].Server.Name).To(Equal("wiki"))
			Expect(a.MCP[1].Err).To(BeIdenticalTo(connectErr))
		})

		It("Should skip a nil Sessions entry and report its servers", func() {
			cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), exampleApp()))
			cfg.MCPClients = []config.MCPServer{{Name: "docs"}}

			a, err := agent.Assemble(ctx, cfg, agent.Sources{MCP: []*mcpclient.Sessions{nil}}, agent.SurfaceRun, agent.Lenient)
			Expect(err).NotTo(HaveOccurred())
			Expect(a.Problems).To(HaveLen(1))
			Expect(a.Problems[0].Alias).To(Equal("docs"))
		})

		It("Should still fail on an application that cannot be introspected", func() {
			cfg := agenttest.Config(GinkgoTB(), nil)
			cfg.ApplicationPath = "/nonexistent/application"

			_, err := agent.Assemble(ctx, cfg, agent.Sources{}, agent.SurfaceRun, agent.Lenient)
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("a nil store", func() {
		It("Should build the family with no store on the Run surface", func() {
			cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), exampleApp()), agenttest.WithMemory(), agenttest.WithRAG())

			a, err := agent.Assemble(ctx, cfg, agent.Sources{}, agent.SurfaceRun, agent.Strict)
			Expect(err).NotTo(HaveOccurred())
			Expect(a.Names(toolkit.KindUnknown, agent.SourceMemory)).To(Equal([]string{"memory_list", "memory_read", "memory_write", "memory_delete"}))
			Expect(a.Names(toolkit.KindUnknown, agent.SourceKnowledge)).To(Equal([]string{"knowledge_search", "knowledge_enumerate"}))
		})

		It("Should fail a served tool whose store was not passed", func() {
			cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), exampleApp()), agenttest.WithRAG())
			cfg.Expose = &config.ExposeConfig{Agent: &config.AgentExpose{MCP: &config.ExposedMCPConfig{Builtins: []string{"knowledge_search"}}}}

			_, err := agent.Assemble(ctx, cfg, agent.Sources{}, agent.SurfaceMCP, agent.Lenient)
			Expect(err).To(MatchError(ContainSubstring(`the built-in tool "knowledge_search" is served on this surface but no store was passed in Sources.RAG`)))
		})
	})

	Describe("the MCP surface", func() {
		It("Should serve the exposed commands and the listed exposable built-ins and withhold the rest", func() {
			fake := &mcpFakeServers{tools: []*mcp.Tool{mcpDescriptor("search", "Searches the documentation")}}
			sessions := connectMCP(GinkgoTB(), fake, config.MCPServer{Name: "docs"})

			cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), memoryListApp()), agenttest.WithHITL(), agenttest.WithMemory())
			store := openRAG(cfg)
			cfg.RemoteTools = []config.RemoteToolHost{{Name: "peer"}}
			cfg.MCPClients = []config.MCPServer{{Name: "docs"}}
			cfg.Expose = &config.ExposeConfig{Agent: &config.AgentExpose{
				Tools: &config.ExposedToolSelection{Exclude: &config.ToolFilter{Tools: []string{"^do$"}}},
				MCP:   &config.ExposedMCPConfig{Builtins: []string{"knowledge_search"}},
			}}

			a, err := agent.Assemble(ctx, cfg, agent.Sources{
				RAG:    store,
				Remote: remoteClient("forecast"),
				MCP:    []*mcpclient.Sessions{sessions},
				Custom: []toolkit.Tool{plainCustomTool("alpha")},
			}, agent.SurfaceMCP, agent.Strict)
			Expect(err).NotTo(HaveOccurred())

			// The command named memory_list is served, since the withheld memory built-in
			// of that name never claimed it.
			Expect(assembledNames(a)).To(Equal([]string{"memory_list", "knowledge_search"}))
			Expect(a.Tools[0].Kind).To(Equal(toolkit.KindApplication))
			Expect(a.Tools[1].Kind).To(Equal(toolkit.KindBuiltin))

			Expect(withheldNames(a)).To(Equal([]string{
				"ask_human_confirm", "ask_human_select", "ask_human_input",
				"memory_list", "memory_read", "memory_write", "memory_delete",
				"knowledge_enumerate",
			}))
			Expect(a.Withheld[0].Reason).To(Equal("it is reachable only in an agent run"))
			Expect(a.Withheld[7].Reason).To(Equal("it is not listed in expose.agent.mcp.builtins"))

			Expect(a.Remote).To(BeEmpty())
			Expect(a.MCP).To(BeEmpty())
			Expect(a.Problems).To(BeEmpty())
			Expect(a.Counts()).To(Equal(telemetry.ToolCounts{Application: 1, Builtin: 1}))
		})

		It("Should ignore Unbound", func() {
			cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), exampleApp()))
			cfg.RemoteTools = []config.RemoteToolHost{{Name: "peer"}}

			a, err := agent.Assemble(ctx, cfg, agent.Sources{
				Unbound: []agent.Problem{{Source: agent.SourceRemoteTools, Err: errors.New("nats: no servers available")}},
			}, agent.SurfaceMCP, agent.Strict)
			Expect(err).NotTo(HaveOccurred())
			Expect(a.Problems).To(BeEmpty())
			Expect(assembledNames(a)).To(Equal([]string{"do"}))
		})
	})

	Describe("the A2A surface", func() {
		It("Should serve only the built-ins that declare a2a exposure", func() {
			cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), exampleApp()), agenttest.WithHITL(), agenttest.WithMemory())
			openRAG(cfg)

			a, err := agent.Assemble(ctx, cfg, agent.Sources{}, agent.SurfaceA2A, agent.Strict)
			Expect(err).NotTo(HaveOccurred())

			Expect(assembledNames(a)).To(Equal([]string{"do"}))
			Expect(withheldNames(a)).To(Equal([]string{
				"ask_human_confirm", "ask_human_select", "ask_human_input",
				"memory_list", "memory_read", "memory_write", "memory_delete",
				"knowledge_search", "knowledge_enumerate",
			}))
			for _, w := range a.Withheld {
				Expect(w.Reason).To(Equal("it is reachable only in an agent run"))
			}
		})
	})
})
