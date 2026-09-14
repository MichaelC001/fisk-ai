//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/choria-io/fisk"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/a2a"
	wire "github.com/choria-io/fisk-ai/internal/a2a/wire/v1"
	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/mcpclient"
	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// cardTool is a tool that answers what a card asks of one: its name, what it does, and
// what calling it does to the world.
type cardTool struct {
	name     string
	behavior toolkit.Behavior
	gated    bool
}

func (t *cardTool) Name() string             { return t.name }
func (t *cardTool) Description() string      { return t.name + " does a thing" }
func (t *cardTool) ModelDescription() string { return t.Description() }
func (t *cardTool) InputSchema() map[string]any {
	return map[string]any{"type": "object"}
}
func (t *cardTool) Definition(bool) llm.ToolDef {
	return llm.ToolDef{Name: t.name, Description: t.Description(), InputSchema: t.InputSchema()}
}

func (t *cardTool) Execute(context.Context, json.RawMessage, toolkit.ExecDeps) (*toolkit.Outcome, error) {
	return &toolkit.Outcome{Output: "ok"}, nil
}

func (t *cardTool) MCPExposable() bool               { return true }
func (t *cardTool) A2AExposable() bool               { return true }
func (t *cardTool) Behavior() toolkit.Behavior       { return t.behavior }
func (t *cardTool) Command() string                  { return t.name }
func (t *cardTool) NeedsConfirm(extra []string) bool { return t.gated }
func (t *cardTool) ConfirmTrigger(extra []string) string {
	if t.gated {
		return toolkit.ConfirmTag
	}

	return ""
}

// cardTools cover the four groups a console draws: read-only, destructive, idempotent
// and a tool that declares nothing. The gated one is what a card built through a2a's
// exposure policy drops.
func cardTools() []toolkit.Tool {
	return []toolkit.Tool{
		&cardTool{name: "ls", behavior: toolkit.Behavior{ReadOnly: toolkit.HintTrue}},
		&cardTool{name: "rm", behavior: toolkit.Behavior{Destructive: toolkit.HintTrue}},
		&cardTool{name: "sync", behavior: toolkit.Behavior{Idempotent: toolkit.HintTrue}},
		&cardTool{name: "run"},
		&cardTool{name: "gated", gated: true},
	}
}

func cardToolNames(card wire.AgentCard) []string {
	names := make([]string, len(card.Tools))
	for i, t := range card.Tools {
		names[i] = t.Name
	}

	return names
}

var _ = Describe("The agent card", func() {
	It("Should answer under the base path with what the agent says about itself", func() {
		opts := testOptions()
		opts.Card = a2a.CardOptions{
			Version:     "1.2.3",
			Model:       "claude-sonnet-4-6",
			Description: "manages nats auth",
			DisplayName: "NATS Auth",
			IconURL:     "https://example.net/agent.png",
			Prompts:     []string{"who can publish to ORDERS?", "add a user for the billing service"},
		}
		opts.CardTools = cardTools()

		card := readCard(newTestChannel(opts), http.StatusOK)
		Expect(card.Name).To(Equal("agent1"), "the identity this channel derives its sessions under")
		Expect(card.Version).To(Equal("1.2.3"))
		Expect(card.Model).To(Equal("claude-sonnet-4-6"))
		Expect(card.Description).To(Equal("manages nats auth"))
		Expect(card.DisplayName).To(Equal("NATS Auth"))
		Expect(card.IconURL).To(Equal("https://example.net/agent.png"))
		Expect(card.Prompts).To(Equal([]string{"who can publish to ORDERS?", "add a user for the billing service"}))
	})

	// The whole approval flow is driven on a gated command, so a card that omitted it
	// would leave a console unable to show the tool the demonstration turns on. a2a drops
	// it because a served agent has nobody to approve it; this channel has an operator.
	It("Should list a confirm-gated tool that an a2a card omits", func() {
		opts := testOptions()
		opts.CardTools = cardTools()

		Expect(cardToolNames(readCard(newTestChannel(opts), http.StatusOK))).To(ConsistOf("ls", "rm", "sync", "run", "gated"))
	})

	// The groups a console draws come off the tri-state hints the descriptor already
	// carried: a tool that declares nothing lands in unstated rather than among the safe
	// ones, and no tag sets open_world.
	It("Should carry each tool's declared behavior and assert nothing for a tool with none", func() {
		opts := testOptions()
		opts.CardTools = cardTools()

		card := readCard(newTestChannel(opts), http.StatusOK)

		behavior := map[string]toolkit.Behavior{}
		for _, t := range card.Tools {
			behavior[t.Name] = a2a.BehaviorOf(t.Behavior)
		}

		Expect(behavior["ls"].ReadOnly).To(Equal(toolkit.HintTrue))
		Expect(behavior["rm"].Destructive).To(Equal(toolkit.HintTrue))
		Expect(behavior["sync"].Idempotent).To(Equal(toolkit.HintTrue))
		Expect(behavior["run"]).To(Equal(toolkit.Behavior{}))

		for name, b := range behavior {
			Expect(b.OpenWorld).To(Equal(toolkit.HintUnset), name)
		}
	})

	// A tool missing because its source is down must not read as a tool somebody
	// removed, so the source is named on the card a console renders.
	It("Should carry a note naming a source whose tools are missing", func() {
		opts := testOptions()
		opts.Card.Notes = []string{`the tools of the mcp server "github" could not be listed, so any tool it supplies is missing here`}

		card := readCard(newTestChannel(opts), http.StatusOK)
		Expect(card.Notes).To(ConsistOf(ContainSubstring(`mcp server "github"`)))
	})

	It("Should refuse to serve a card whose icon url is not an https url", func() {
		for _, icon := range []string{"javascript:alert(1)", "http://example.net/a.png"} {
			opts := testOptions()
			opts.Card.IconURL = icon

			readCard(newTestChannel(opts), http.StatusInternalServerError)
		}
	})

	It("Should refuse to serve a card whose icon url is over the length limit", func() {
		opts := testOptions()
		opts.Card.IconURL = "https://example.net/" + strings.Repeat("a", wire.MaxIconURLLength)

		readCard(newTestChannel(opts), http.StatusInternalServerError)
	})

	// The discovery reply schema limits both, so a card over either is one no peer could
	// read. The web channel refuses it per request, where the a2a server refuses it at
	// construction, since that one builds its card once.
	It("Should refuse to serve a card whose display name or icon is over the length limit", func() {
		opts := testOptions()
		opts.Card.DisplayName = strings.Repeat("a", wire.MaxDisplayNameLength+1)
		readCard(newTestChannel(opts), http.StatusInternalServerError)

		opts = testOptions()
		opts.Card.Icon = strings.Repeat("a", wire.MaxIconLength+1)
		readCard(newTestChannel(opts), http.StatusInternalServerError)
	})

	It("Should refuse to serve a card carrying more sample prompts than the schema will, or one too long", func() {
		opts := testOptions()
		opts.Card.Prompts = slices.Repeat([]string{"ask me"}, wire.MaxPrompts+1)
		readCard(newTestChannel(opts), http.StatusInternalServerError)

		opts = testOptions()
		opts.Card.Prompts = []string{strings.Repeat("a", wire.MaxPromptLength+1)}
		readCard(newTestChannel(opts), http.StatusInternalServerError)
	})

	It("Should refuse a format mounted where the card answers", func() {
		opts := testOptions()
		opts.Formats = []Mount{{Path: CardPath, Format: &fakeFormat{}}}

		_, err := New(opts)
		Expect(err).To(MatchError(ContainSubstring("where this channel answers the agent card")))
	})

	It("Should refuse a POST to the card", func() {
		rec := cardRequest(newTestChannel(testOptions()), http.MethodPost, nil)
		Expect(rec.Code).To(Equal(http.StatusMethodNotAllowed))
	})

	It("Should refuse a card request from an unlisted origin", func() {
		rec := cardRequest(newTestChannel(testOptions()), http.MethodGet, map[string]string{"Origin": "http://evil.example"})
		Expect(rec.Code).To(Equal(http.StatusForbidden))
	})

	It("Should let a listed page read the card", func() {
		rec := cardRequest(newTestChannel(testOptions()), http.MethodGet, map[string]string{"Origin": testOrigin})
		Expect(rec.Code).To(Equal(http.StatusOK))
		Expect(rec.Header().Get("Access-Control-Allow-Origin")).To(Equal(testOrigin))
	})
})

// cardApp is an application with a confirm-gated command and a plain one whose name a
// remote host's tool shares.
func cardApp() *fisk.Application {
	app := fisk.New("app", "an app")
	app.Command("backup", "back a thing up").Tag(toolkit.ConfirmTag)
	app.Command("status", "show the status")

	return app
}

// cardRemote is an a2a client over a fake transport that answers discovery for every
// host with the given tools and fails it for the host named dead. The transport is
// returned beside the client so a run can be handed the same one.
func cardRemote(dead string, tools ...string) (*a2a.Client, *agenttest.FakeTransport) {
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
	if dead != "" {
		transport.SetFaults(agenttest.TransportFault{Agent: dead, Err: a2a.ErrNoResponders})
	}

	client, err := a2a.NewClient(transport, "agent")
	Expect(err).ToNot(HaveOccurred())

	return client, transport
}

// cardMCP connects a session to an mcp.Server named name over an in-memory transport,
// advertising tools, and closes it when the spec ends. failList makes the server
// answer tools/list with an error while it stays connected: a server that was reached
// and cannot be listed.
func cardMCP(name string, failList bool, tools ...string) *mcpclient.Sessions {
	GinkgoHelper()

	dialer := func(_ context.Context, server config.MCPServer) (mcp.Transport, error) {
		clientSide, serverSide := mcp.NewInMemoryTransports()

		srv := mcp.NewServer(&mcp.Implementation{Name: server.Name, Version: "1.0.0"}, nil)
		for _, tool := range tools {
			srv.AddTool(&mcp.Tool{Name: tool, Description: tool + " on the server", InputSchema: json.RawMessage(`{"type":"object"}`)}, cardMCPHandler)
		}
		if failList {
			srv.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
				return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
					if method == "tools/list" {
						return nil, fmt.Errorf("the tool list is unavailable")
					}

					return next(ctx, method, req)
				}
			})
		}

		// The server side is connected before the client, as in-memory transports
		// require, and under a context of its own: the caller's has the connect timeout
		// on it, which is unrelated to how long this server runs.
		_, err := srv.Connect(context.Background(), serverSide, nil)
		if err != nil {
			return nil, err
		}

		return clientSide, nil
	}

	sessions, err := mcpclient.Connect(context.Background(), mcpclient.Options{
		Servers:  []config.MCPServer{{Name: name}},
		Identity: "agent",
		Version:  "0.0.1",
		Dialer:   dialer,
	})
	Expect(err).ToNot(HaveOccurred())
	DeferCleanup(func() { Expect(sessions.Close(context.Background())).To(Succeed()) })

	return sessions
}

// cardMCPHandler answers a call, which no spec here makes: a card reads names and
// descriptions and calls nothing.
func cardMCPHandler(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "handled by " + req.Params.Name}}}, nil
}

var _ = Describe("ResolveAgentTools", func() {
	toolNames := func(tools AgentTools) []string {
		names := make([]string, len(tools.Tools))
		for i, t := range tools.Tools {
			names[i] = t.Name()
		}

		return names
	}

	// builtinNames are the built-ins every family enables, in assembly order.
	builtinNames := []string{
		config.AskHumanConfirmToolName,
		config.AskHumanSelectToolName,
		config.AskHumanInputToolName,
		config.MemoryListToolName,
		config.MemoryReadToolName,
		config.MemoryWriteToolName,
		config.MemoryDeleteToolName,
		config.KnowledgeSearchToolName,
		config.KnowledgeEnumerateToolName,
		config.ReadFileToolName,
		config.Base64EncodeToolName,
	}

	// withOptInTools lists both harness.tools built-ins, read_file gated.
	withOptInTools := func(c *config.Config) {
		c.Harness.Tools = []config.HarnessToolConfig{
			{Name: config.ReadFileToolName, Confirm: true},
			{Name: config.Base64EncodeToolName},
		}
	}

	// fullConfig is a configuration with commands, the four families, two remote hosts
	// and one MCP server.
	fullConfig := func() *config.Config {
		GinkgoHelper()

		cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), cardApp()), agenttest.WithHITL(), agenttest.WithMemory(), agenttest.WithRAG(), withOptInTools)
		cfg.RemoteTools = []config.RemoteToolHost{{Name: "weather", Alias: "wx"}, {Name: "docs"}}
		cfg.MCPClients = []config.MCPServer{{Name: "wiki"}}

		return cfg
	}

	// A run injects these alongside the application's commands, so a card built from the
	// commands alone names fewer tools than the agent behind it calls. The gated
	// read_file is listed like a confirm-gated command, since the channel has an
	// operator to approve it.
	It("Should list the built-ins the configuration enables", func() {
		cfg := agenttest.Config(GinkgoTB(), nil, agenttest.WithHITL(), agenttest.WithMemory(), agenttest.WithRAG(), withOptInTools)

		tools, err := ResolveAgentTools(context.Background(), cfg, nil, nil)
		Expect(err).ToNot(HaveOccurred())
		Expect(toolNames(tools)).To(Equal(builtinNames))

		gated, ok := tools.Tools[len(tools.Tools)-2].(toolkit.Confirmable)
		Expect(ok).To(BeTrue())
		Expect(gated.NeedsConfirm(nil)).To(BeTrue())
	})

	It("Should list no built-in the configuration leaves off", func() {
		tools, err := ResolveAgentTools(context.Background(), agenttest.Config(GinkgoTB(), nil), nil, nil)
		Expect(err).ToNot(HaveOccurred())
		Expect(tools.Tools).To(BeEmpty())
	})

	// Both hosts advertise forecast, so both are prefixed; status is the command's name,
	// so both hosts' are prefixed too. The server that cannot be listed gets the note.
	It("Should list the commands, the built-ins and two hosts' tools, with a note for the server that cannot be listed", func() {
		client, _ := cardRemote("", "forecast", "status")
		sessions := cardMCP("wiki", true, "search")

		tools, err := ResolveAgentTools(context.Background(), fullConfig(), client, sessions)
		Expect(err).ToNot(HaveOccurred())

		want := append([]string{"backup", "status"}, builtinNames...)
		want = append(want, "wx_forecast", "wx_status", "docs_forecast", "docs_status")
		Expect(toolNames(tools)).To(Equal(want))
		Expect(tools.Notes).To(Equal([]string{`the tools of the mcp server "wiki" could not be listed, so any tool it supplies is missing here`}))
	})

	// A console reads off the card the tools the agent behind it calls, so the names
	// must be the names a run of the same configuration offers the model.
	It("Should list the tool names a run of the same configuration offers", func() {
		cfg := fullConfig()
		client, transport := cardRemote("", "forecast", "status")
		sessions := cardMCP("wiki", false, "search")

		tools, err := ResolveAgentTools(context.Background(), cfg, client, sessions)
		Expect(err).ToNot(HaveOccurred())
		Expect(tools.Notes).To(BeEmpty())

		var start agent.RunStartInfo
		_, err = agent.Run(context.Background(), agent.Options{
			Config:       cfg,
			ConfigFile:   "agent.yaml",
			Prompt:       []string{"go"},
			Provider:     agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
			StoreDir:     GinkgoT().TempDir(),
			A2ATransport: transport,
			MCPSessions:  sessions,
			Hooks: agent.Hooks{
				RunStart: func(_ context.Context, in agent.RunStartInfo) error {
					start = in
					return nil
				},
			},
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).ToNot(HaveOccurred())

		// RunStartInfo.ToolNames is sorted; the card is in assembly order.
		Expect(toolNames(tools)).To(ConsistOf(start.ToolNames))
	})

	// With one host answering, forecast is nobody else's and stays bare, where a run
	// with both hosts up would prefix it.
	It("Should list the live host's tools with a note for the dead one", func() {
		client, _ := cardRemote("docs", "forecast", "status")
		sessions := cardMCP("wiki", false, "search")

		tools, err := ResolveAgentTools(context.Background(), fullConfig(), client, sessions)
		Expect(err).ToNot(HaveOccurred())

		want := append([]string{"backup", "status"}, builtinNames...)
		want = append(want, "forecast", "wx_status", "wiki_search")
		Expect(toolNames(tools)).To(Equal(want))
		Expect(tools.Notes).To(Equal([]string{`the tools of the remote agent "docs" could not be listed, so any tool it supplies is missing here`}))
	})

	// A channel built with no connection has no client to discover the hosts through,
	// which the card says once for the block rather than once per host.
	It("Should note that no remote agent could be listed when hosts are configured and no client was passed", func() {
		tools, err := ResolveAgentTools(context.Background(), fullConfig(), nil, cardMCP("wiki", false, "search"))
		Expect(err).ToNot(HaveOccurred())

		want := append([]string{"backup", "status"}, builtinNames...)
		want = append(want, "wiki_search")
		Expect(toolNames(tools)).To(Equal(want))
		Expect(tools.Notes).To(Equal([]string{"the tools of every remote agent could not be listed, so any tool one supplies is missing here"}))
	})
})
