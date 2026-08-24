//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// These tests exercise the MCP import through the exported agent.Run API: what an
// imported tool is named and how a call to it dispatches, which failures abort a run
// and which are reported and survived, how a collision against each other kind of tool
// ends, and who closes the sessions. The servers are real mcp.Servers driven over
// in-memory transports and, where a run has to open its own sessions, over an httptest
// server, so no spec here needs a subprocess.
package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/choria-io/fisk"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/mcpclient"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/toolkit/functool"
)

// mcpDescriptor is a tool descriptor a fake server advertises. An empty description is
// what a server that described a tool badly sends, which the import skips.
func mcpDescriptor(name string, description string) *mcp.Tool {
	return &mcp.Tool{Name: name, Description: description, InputSchema: json.RawMessage(`{"type":"object"}`)}
}

// mcpFakeServers stands a real mcp.Server up in this process for every server the
// sessions dial, over an in-memory transport pair, so a run drives genuine protocol
// traffic with no subprocess and no socket.
type mcpFakeServers struct {
	tools    []*mcp.Tool
	failList bool
}

// dialer builds the mcpclient.Dialer to connect these servers with.
func (f *mcpFakeServers) dialer() mcpclient.Dialer {
	return func(_ context.Context, server config.MCPServer) (mcp.Transport, error) {
		clientSide, serverSide := mcp.NewInMemoryTransports()

		srv := mcp.NewServer(&mcp.Implementation{Name: server.Name, Version: "1.0.0"}, nil)
		for _, tool := range f.tools {
			srv.AddTool(tool, mcpEchoHandler)
		}
		if f.failList {
			srv.AddReceivingMiddleware(mcpFailListing())
		}

		// The server side is connected before the client, as in-memory transports
		// require, and under a context of its own: the caller's carries the connect
		// timeout, which has nothing to say about how long this server lives.
		_, err := srv.Connect(context.Background(), serverSide, nil)
		if err != nil {
			return nil, err
		}

		return clientSide, nil
	}
}

// mcpEchoHandler answers a call by naming the tool the server ran, so a dispatched
// call is visible in the result the model is shown.
func mcpEchoHandler(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "handled by " + req.Params.Name}}}, nil
}

// mcpFailListing makes a server answer tools/list with an error while it stays
// connected: a server that was reached and cannot be listed.
func mcpFailListing() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "tools/list" {
				return nil, fmt.Errorf("the tool list is unavailable")
			}

			return next(ctx, method, req)
		}
	}
}

// connectMCP opens the sessions a caller injects into a run and closes them when the
// test ends, which is the caller's job and never Run's.
func connectMCP(t *testing.T, fake *mcpFakeServers, servers ...config.MCPServer) *mcpclient.Sessions {
	t.Helper()
	g := NewWithT(t)

	sessions, err := mcpclient.Connect(context.Background(), mcpclient.Options{
		Servers:  servers,
		Identity: "agent",
		Version:  "0.0.1",
		Dialer:   fake.dialer(),
	})
	g.Expect(err).NotTo(HaveOccurred())
	t.Cleanup(func() { g.Expect(sessions.Close()).To(Succeed()) })

	return sessions
}

// mcpNoteRecorder is an Events that also implements the optional MCPServerReporter
// half, which the shared recording fake deliberately does not.
type mcpNoteRecorder struct {
	*agenttest.RecordingEvents

	mu    sync.Mutex
	notes []mcpclient.ServerImport
}

func newMCPNoteRecorder() *mcpNoteRecorder {
	return &mcpNoteRecorder{RecordingEvents: agenttest.NewRecordingEvents()}
}

func (r *mcpNoteRecorder) MCPServerNotes(imports []mcpclient.ServerImport) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notes = append(r.notes, imports...)
}

// Notes returns the per-server outcomes reported, in order.
func (r *mcpNoteRecorder) Notes() []mcpclient.ServerImport {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]mcpclient.ServerImport(nil), r.notes...)
}

// advertised reports whether the model was told about a tool of this name.
func advertised(defs []llm.ToolDef, name string) bool {
	for _, d := range defs {
		if d.Name == name {
			return true
		}
	}

	return false
}

// TestMCPImport_ImportedToolDispatches proves the whole path: a configured server's
// tool is named "<alias>_<tool>", advertised to the model, and dispatched to the
// server it came from, with the server's answer returned as the tool result and the
// call accounted as an MCP call rather than a peer's.
func TestMCPImport_ImportedToolDispatches(t *testing.T) {
	g := NewWithT(t)

	fake := &mcpFakeServers{tools: []*mcp.Tool{mcpDescriptor("search", "Searches the documentation")}}
	sessions := connectMCP(t, fake, config.MCPServer{Name: "docs"})

	app := agenttest.NewFakeApp(t, exampleApp())
	cfg := agenttest.Config(t, app)
	cfg.MCPServers = []config.MCPServer{{Name: "docs"}}

	provider := agenttest.NewScriptedProvider(t,
		agenttest.ToolUseResponse("call-1", "docs_search", json.RawMessage(`{}`)),
		agenttest.TextResponse("done"),
	)
	events := agenttest.NewRecordingEvents()

	res, err := agent.Run(context.Background(), agent.Options{
		Config:      cfg,
		ConfigFile:  "agent.yaml",
		Prompt:      []string{"search the docs"},
		Provider:    provider,
		MCPSessions: sessions,
	}, events, agenttest.NewScriptedPrompter(t))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

	g.Expect(advertised(provider.Requests()[0].Tools, "docs_search")).To(BeTrue())

	results := events.ToolResults()
	g.Expect(results).To(HaveLen(1))
	g.Expect(results[0].IsError).To(BeFalse())
	g.Expect(results[0].Output).To(Equal("handled by search"))
	g.Expect(results[0].ProviderKind).To(Equal(toolkit.KindMCP))
}

// TestMCPImport_UnlistableServerAbortsRun pins the strict half: a server that answered
// the handshake and then cannot be listed aborts the run naming it, because the prompt
// may depend on the tools that are not there.
func TestMCPImport_UnlistableServerAbortsRun(t *testing.T) {
	g := NewWithT(t)

	fake := &mcpFakeServers{tools: []*mcp.Tool{mcpDescriptor("search", "Searches the documentation")}, failList: true}
	sessions := connectMCP(t, fake, config.MCPServer{Name: "docs"})

	app := agenttest.NewFakeApp(t, exampleApp())
	cfg := agenttest.Config(t, app)
	cfg.MCPServers = []config.MCPServer{{Name: "docs"}}

	_, err := agent.Run(context.Background(), agent.Options{
		Config:      cfg,
		ConfigFile:  "agent.yaml",
		Prompt:      []string{"go"},
		Provider:    agenttest.NewScriptedProvider(t, agenttest.TextResponse("done")),
		MCPSessions: sessions,
	}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).To(MatchError(ContainSubstring(`importing tools from mcp server "docs"`)))
}

// TestMCPImport_BadlyDescribedToolIsSkipped pins the other side of that line: a tool
// the server described badly costs that tool and not the run, since the server
// answered. What was skipped, what the server offered and how long it took to answer
// reach the caller through the optional reporter half.
func TestMCPImport_BadlyDescribedToolIsSkipped(t *testing.T) {
	g := NewWithT(t)

	fake := &mcpFakeServers{tools: []*mcp.Tool{
		mcpDescriptor("search", "Searches the documentation"),
		mcpDescriptor("broken", ""),
	}}
	sessions := connectMCP(t, fake, config.MCPServer{Name: "docs"})

	app := agenttest.NewFakeApp(t, exampleApp())
	cfg := agenttest.Config(t, app)
	cfg.MCPServers = []config.MCPServer{{Name: "docs"}}

	provider := agenttest.NewScriptedProvider(t, agenttest.TextResponse("done"))
	events := newMCPNoteRecorder()

	res, err := agent.Run(context.Background(), agent.Options{
		Config:      cfg,
		ConfigFile:  "agent.yaml",
		Prompt:      []string{"go"},
		Provider:    provider,
		MCPSessions: sessions,
	}, events, agenttest.NewScriptedPrompter(t))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

	g.Expect(advertised(provider.Requests()[0].Tools, "docs_search")).To(BeTrue())
	g.Expect(advertised(provider.Requests()[0].Tools, "docs_broken")).To(BeFalse())

	notes := events.Notes()
	g.Expect(notes).To(HaveLen(1))
	g.Expect(notes[0].Server.Name).To(Equal("docs"))
	g.Expect(notes[0].Discovered).To(Equal(2))
	g.Expect(notes[0].Tools).To(HaveLen(1))
	g.Expect(notes[0].RTT).To(BeNumerically(">", time.Duration(0)))
	g.Expect(notes[0].Skipped).To(HaveLen(1))
	g.Expect(notes[0].Skipped[0].Name).To(Equal("broken"))
	g.Expect(notes[0].Skipped[0].Reason).To(ContainSubstring("advertises no description"))
}

// TestMCPImport_CollisionsAbortRun pins the naming pass against both of the lookups a
// run keeps its claimed names in: the taken set the application tools write, and the
// name map the a2a import returns and never writes there. A clash with either aborts
// the run naming the tool and the server it came from.
func TestMCPImport_CollisionsAbortRun(t *testing.T) {
	t.Run("against an application tool", func(t *testing.T) {
		g := NewWithT(t)

		// The application's "docs search" command loads as the tool "docs_search", which
		// is the name the server's "search" would take under the alias "docs".
		application := fisk.New("app", "an app")
		application.Command("docs", "documentation commands").Command("search", "search the documentation")

		fake := &mcpFakeServers{tools: []*mcp.Tool{mcpDescriptor("search", "Searches the documentation")}}
		sessions := connectMCP(t, fake, config.MCPServer{Name: "docs"})

		cfg := agenttest.Config(t, agenttest.NewFakeApp(t, application))
		cfg.MCPServers = []config.MCPServer{{Name: "docs"}}

		_, err := agent.Run(context.Background(), agent.Options{
			Config:      cfg,
			ConfigFile:  "agent.yaml",
			Prompt:      []string{"go"},
			Provider:    agenttest.NewScriptedProvider(t, agenttest.TextResponse("done")),
			MCPSessions: sessions,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
		g.Expect(err).To(MatchError(ContainSubstring(`"docs_search" (mcp server "docs")`)))
	})

	t.Run("against a tool imported from a peer", func(t *testing.T) {
		g := NewWithT(t)

		// The peer's tool is imported under its own name, since nothing local claims it,
		// so it takes the name the server's "search" would take under the alias "docs".
		transport := agenttest.NewFakeTransport(t, a2a.AgentCard{
			Name:    "docs-svc",
			Version: "1.0.0",
			Tools: []a2a.ToolDescriptor{{
				Name:        "docs_search",
				Description: "search the documentation",
				InputSchema: json.RawMessage(`{"type":"object"}`),
			}},
		})

		fake := &mcpFakeServers{tools: []*mcp.Tool{mcpDescriptor("search", "Searches the documentation")}}
		sessions := connectMCP(t, fake, config.MCPServer{Name: "docs"})

		cfg := agenttest.Config(t, agenttest.NewFakeApp(t, exampleApp()))
		cfg.RemoteTools = []config.RemoteToolHost{{Name: "docs-svc"}}
		cfg.MCPServers = []config.MCPServer{{Name: "docs"}}

		_, err := agent.Run(context.Background(), agent.Options{
			Config:       cfg,
			ConfigFile:   "agent.yaml",
			Prompt:       []string{"go"},
			Provider:     agenttest.NewScriptedProvider(t, agenttest.TextResponse("done")),
			A2ATransport: transport,
			MCPSessions:  sessions,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
		g.Expect(err).To(MatchError(ContainSubstring(`"docs_search" (mcp server "docs")`)))
	})
}

// TestMCPImport_CollisionStillReportsNotes pins what an operator is left with when a
// collision aborts the run: the outcomes of every server reach the reporter before the
// error is returned, so the alias to set or the filter to drop is decided against the
// second server's tools and round trip rather than the collision alone.
func TestMCPImport_CollisionStillReportsNotes(t *testing.T) {
	g := NewWithT(t)

	// The application's "docs search" command loads as the tool "docs_search", the name
	// the first server's "search" takes under the alias "docs". The second server's takes
	// "wiki_search", which nothing claims.
	application := fisk.New("app", "an app")
	application.Command("docs", "documentation commands").Command("search", "search the documentation")

	fake := &mcpFakeServers{tools: []*mcp.Tool{mcpDescriptor("search", "Searches the documentation")}}
	sessions := connectMCP(t, fake, config.MCPServer{Name: "docs"}, config.MCPServer{Name: "wiki"})

	cfg := agenttest.Config(t, agenttest.NewFakeApp(t, application))
	cfg.MCPServers = []config.MCPServer{{Name: "docs"}, {Name: "wiki"}}

	events := newMCPNoteRecorder()

	_, err := agent.Run(context.Background(), agent.Options{
		Config:      cfg,
		ConfigFile:  "agent.yaml",
		Prompt:      []string{"go"},
		Provider:    agenttest.NewScriptedProvider(t, agenttest.TextResponse("done")),
		MCPSessions: sessions,
	}, events, agenttest.NewScriptedPrompter(t))
	g.Expect(err).To(MatchError(ContainSubstring(`"docs_search" (mcp server "docs")`)))

	notes := events.Notes()
	g.Expect(notes).To(HaveLen(2))

	g.Expect(notes[0].Server.Name).To(Equal("docs"))
	g.Expect(notes[0].Tools).To(BeEmpty())
	g.Expect(notes[0].Skipped).To(HaveLen(1))
	g.Expect(notes[0].Skipped[0].Name).To(Equal("search"))
	g.Expect(notes[0].Skipped[0].Reason).To(ContainSubstring(`the name "docs_search" is already taken`))

	g.Expect(notes[1].Server.Name).To(Equal("wiki"))
	g.Expect(notes[1].Discovered).To(Equal(1))
	g.Expect(notes[1].Tools).To(HaveLen(1))
	g.Expect(notes[1].Skipped).To(BeEmpty())
	g.Expect(notes[1].RTT).To(BeNumerically(">", time.Duration(0)))
}

// TestMCPImport_CustomToolCollisionAbortsRun covers the collision from the other
// direction: the MCP import runs before the caller's custom tools, so a custom tool
// taking a name an imported tool already holds aborts the run naming the mcp server it
// came from rather than shadowing it.
func TestMCPImport_CustomToolCollisionAbortsRun(t *testing.T) {
	g := NewWithT(t)

	fake := &mcpFakeServers{tools: []*mcp.Tool{mcpDescriptor("search", "Searches the documentation")}}
	sessions := connectMCP(t, fake, config.MCPServer{Name: "docs"})

	// The imported tool is named "docs_search", which is the name this custom tool takes.
	custom, err := functool.New(functool.Spec{
		Name:        "docs_search",
		Description: "a custom tool",
		Schema:      map[string]any{"type": "object"},
		Handler:     noopCustomHandler,
	})
	g.Expect(err).NotTo(HaveOccurred())

	cfg := agenttest.Config(t, agenttest.NewFakeApp(t, exampleApp()))
	cfg.MCPServers = []config.MCPServer{{Name: "docs"}}

	_, err = agent.Run(context.Background(), agent.Options{
		Config:      cfg,
		ConfigFile:  "agent.yaml",
		Prompt:      []string{"go"},
		Provider:    agenttest.NewScriptedProvider(t, agenttest.TextResponse("done")),
		MCPSessions: sessions,
		CustomTools: []toolkit.Tool{custom},
	}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).To(MatchError(ContainSubstring(`custom tool at index 0 ("docs_search") collides with a tool of the same name imported from an mcp server`)))
}

// TestMCPImport_InjectedSessionsAreBorrowed proves Run leaves injected sessions open:
// the caller connected them once for every run it hosts, so a run that closed them
// would take the next run's servers down with it.
func TestMCPImport_InjectedSessionsAreBorrowed(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	fake := &mcpFakeServers{tools: []*mcp.Tool{mcpDescriptor("search", "Searches the documentation")}}
	sessions := connectMCP(t, fake, config.MCPServer{Name: "docs"})

	cfg := agenttest.Config(t, agenttest.NewFakeApp(t, exampleApp()))
	cfg.MCPServers = []config.MCPServer{{Name: "docs"}}

	res, err := agent.Run(ctx, agent.Options{
		Config:      cfg,
		ConfigFile:  "agent.yaml",
		Prompt:      []string{"go"},
		Provider:    agenttest.NewScriptedProvider(t, agenttest.TextResponse("done")),
		MCPSessions: sessions,
	}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

	// The session the run borrowed still answers, which a closed Sessions refuses
	// before it reaches the server.
	err = sessions.Use(ctx, "docs", func(session *mcp.ClientSession) error {
		_, err := session.ListTools(ctx, nil)
		return err
	})
	g.Expect(err).NotTo(HaveOccurred())
}

// TestMCPImport_MismatchedInjectedSessionsAbortRun pins what a run does when the
// sessions it was handed were opened for other servers. The import walks the list the
// sessions carry, so a run would otherwise import the injector's servers, under the
// injector's aliases and filters, without either side noticing. The check sits in Run
// rather than in a host, so it covers a caller setting Options.MCPSessions directly as
// well as one that hosts runs behind a channel.
func TestMCPImport_MismatchedInjectedSessionsAbortRun(t *testing.T) {
	t.Run("a configured server that was not connected", func(t *testing.T) {
		g := NewWithT(t)

		fake := &mcpFakeServers{tools: []*mcp.Tool{mcpDescriptor("search", "Searches the documentation")}}
		sessions := connectMCP(t, fake, config.MCPServer{Name: "docs"})

		cfg := agenttest.Config(t, agenttest.NewFakeApp(t, exampleApp()))
		cfg.MCPServers = []config.MCPServer{{Name: "docs"}, {Name: "wiki"}}

		_, err := agent.Run(context.Background(), agent.Options{
			Config:      cfg,
			ConfigFile:  "agent.yaml",
			Prompt:      []string{"go"},
			Provider:    agenttest.NewScriptedProvider(t, agenttest.TextResponse("done")),
			MCPSessions: sessions,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
		g.Expect(err).To(MatchError(ContainSubstring("configured but not connected: wiki")))
	})

	t.Run("a connected server the run never configured", func(t *testing.T) {
		g := NewWithT(t)

		fake := &mcpFakeServers{tools: []*mcp.Tool{mcpDescriptor("search", "Searches the documentation")}}
		sessions := connectMCP(t, fake, config.MCPServer{Name: "docs"}, config.MCPServer{Name: "wiki"})

		cfg := agenttest.Config(t, agenttest.NewFakeApp(t, exampleApp()))
		cfg.MCPServers = []config.MCPServer{{Name: "docs"}}

		_, err := agent.Run(context.Background(), agent.Options{
			Config:      cfg,
			ConfigFile:  "agent.yaml",
			Prompt:      []string{"go"},
			Provider:    agenttest.NewScriptedProvider(t, agenttest.TextResponse("done")),
			MCPSessions: sessions,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
		g.Expect(err).To(MatchError(ContainSubstring("connected but not configured: wiki")))
	})
}

// TestMCPImport_SelfOpenedSessionsAreClosed is the other half of the lifetime rule: a
// run given no sessions connects its own at start and closes them at the end, so a
// terminal run leaves nothing connected and no stdio child running.
func TestMCPImport_SelfOpenedSessionsAreClosed(t *testing.T) {
	g := NewWithT(t)

	srv := mcp.NewServer(&mcp.Implementation{Name: "docs", Version: "1.0.0"}, nil)
	srv.AddTool(mcpDescriptor("search", "Searches the documentation"), mcpEchoHandler)

	// Ending a streamable HTTP session is a DELETE to the endpoint, so the request the
	// server receives is how the run's own Close is observed from outside it.
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	terminated := make(chan struct{}, 1)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			select {
			case terminated <- struct{}{}:
			default:
			}
		}

		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(endpoint.Close)

	cfg := agenttest.Config(t, agenttest.NewFakeApp(t, exampleApp()))
	cfg.MCPServers = []config.MCPServer{{Name: "docs", URL: endpoint.URL}}

	provider := agenttest.NewScriptedProvider(t, agenttest.TextResponse("done"))

	res, err := agent.Run(context.Background(), agent.Options{
		Config:     cfg,
		ConfigFile: "agent.yaml",
		Prompt:     []string{"go"},
		Provider:   provider,
	}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

	// The run reached this server, and ended the session it opened before it returned.
	g.Expect(advertised(provider.Requests()[0].Tools, "docs_search")).To(BeTrue())
	g.Eventually(terminated, 10*time.Second).Should(Receive())
}

// TestMCPImport_OnlyMCPToolsStartsRun proves the no-tools gate counts imported MCP
// tools: an agent wrapping an application with no commands, with no built-in, remote or
// injected tools, starts and completes on a server's tools alone rather than reporting
// that it has none.
func TestMCPImport_OnlyMCPToolsStartsRun(t *testing.T) {
	g := NewWithT(t)

	fake := &mcpFakeServers{tools: []*mcp.Tool{mcpDescriptor("search", "Searches the documentation")}}
	sessions := connectMCP(t, fake, config.MCPServer{Name: "docs"})

	cfg := agenttest.Config(t, agenttest.NewFakeApp(t, emptyFiskApp()))
	cfg.MCPServers = []config.MCPServer{{Name: "docs"}}

	provider := agenttest.NewScriptedProvider(t, agenttest.TextResponse("done"))

	res, err := agent.Run(context.Background(), agent.Options{
		Config:      cfg,
		ConfigFile:  "agent.yaml",
		Prompt:      []string{"go"},
		Provider:    provider,
		MCPSessions: sessions,
	}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.Reason).To(Equal(runstate.ReasonCompleted))
	g.Expect(advertised(provider.Requests()[0].Tools, "docs_search")).To(BeTrue())
}
