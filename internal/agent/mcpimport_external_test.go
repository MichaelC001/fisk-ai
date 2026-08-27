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
	. "github.com/onsi/ginkgo/v2"
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

	mu sync.Mutex
	// running is the mcp.Server behind each configured name, so a spec can change a
	// server's tool list for real and let it tell the run about it.
	running map[string]*mcp.Server
	// listed counts the tools/list requests each server answered, so a spec can prove
	// which server a notification made re-list and which it left alone.
	listed map[string]int
}

// dialer builds the mcpclient.Dialer to connect these servers with.
func (f *mcpFakeServers) dialer() mcpclient.Dialer {
	return func(_ context.Context, server config.MCPServer) (mcp.Transport, error) {
		clientSide, serverSide := mcp.NewInMemoryTransports()

		srv := mcp.NewServer(&mcp.Implementation{Name: server.Name, Version: "1.0.0"}, nil)
		for _, tool := range f.tools {
			srv.AddTool(tool, mcpEchoHandler)
		}
		srv.AddReceivingMiddleware(f.listCounter(server.Name))
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

		f.mu.Lock()
		if f.running == nil {
			f.running = map[string]*mcp.Server{}
		}
		f.running[server.Name] = srv
		f.mu.Unlock()

		return clientSide, nil
	}
}

// listCounter counts the tools/list requests one server answers.
func (f *mcpFakeServers) listCounter(name string) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "tools/list" {
				f.mu.Lock()
				if f.listed == nil {
					f.listed = map[string]int{}
				}
				f.listed[name]++
				f.mu.Unlock()
			}

			return next(ctx, method, req)
		}
	}
}

// server is the mcp.Server standing behind one configured name.
func (f *mcpFakeServers) server(t testing.TB, name string) *mcp.Server {
	t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()

	srv, ok := f.running[name]
	Expect(ok).To(BeTrue(), "no server named %q has been dialed", name)

	return srv
}

// lists is how many tools/list requests one server has answered.
func (f *mcpFakeServers) lists(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.listed[name]
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
// spec ends, which is the caller's job and never Run's.
func connectMCP(t testing.TB, fake *mcpFakeServers, servers ...config.MCPServer) *mcpclient.Sessions {
	t.Helper()

	sessions, err := mcpclient.Connect(context.Background(), mcpclient.Options{
		Servers:  servers,
		Identity: "agent",
		Version:  "0.0.1",
		Dialer:   fake.dialer(),
	})
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() { Expect(sessions.Close()).To(Succeed()) })

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

var _ = Describe("the MCP import", func() {
	// This proves the whole path: a configured server's tool is named
	// "<alias>_<tool>", advertised to the model, and dispatched to the server it came
	// from, with the server's answer returned as the tool result and the call accounted
	// as an MCP call rather than a peer's.
	It("Should dispatch an imported tool to the server it came from", func() {
		fake := &mcpFakeServers{tools: []*mcp.Tool{mcpDescriptor("search", "Searches the documentation")}}
		sessions := connectMCP(GinkgoTB(), fake, config.MCPServer{Name: "docs"})

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		cfg.MCPClients = []config.MCPServer{{Name: "docs"}}

		provider := agenttest.NewScriptedProvider(GinkgoTB(),
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
		}, events, agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

		Expect(advertised(provider.Requests()[0].Tools, "docs_search")).To(BeTrue())

		results := events.ToolResults()
		Expect(results).To(HaveLen(1))
		Expect(results[0].IsError).To(BeFalse())
		Expect(results[0].Output).To(Equal("handled by search"))
		Expect(results[0].ProviderKind).To(Equal(toolkit.KindMCP))
	})

	// This pins the strict half: a server that answered the handshake and then cannot be
	// listed aborts the run naming it, because the prompt may depend on the tools that
	// are not there.
	It("Should abort the run for a server that cannot be listed", func() {
		fake := &mcpFakeServers{tools: []*mcp.Tool{mcpDescriptor("search", "Searches the documentation")}, failList: true}
		sessions := connectMCP(GinkgoTB(), fake, config.MCPServer{Name: "docs"})

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		cfg.MCPClients = []config.MCPServer{{Name: "docs"}}

		_, err := agent.Run(context.Background(), agent.Options{
			Config:      cfg,
			ConfigFile:  "agent.yaml",
			Prompt:      []string{"go"},
			Provider:    agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
			MCPSessions: sessions,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).To(MatchError(ContainSubstring(`importing tools from mcp server "docs"`)))
	})

	// This pins the other side of that line: a tool the server described badly costs that
	// tool and not the run, since the server answered. What was skipped, what the server
	// offered and how long it took to answer reach the caller through the optional
	// reporter half.
	It("Should skip a badly described tool and run on", func() {
		fake := &mcpFakeServers{tools: []*mcp.Tool{
			mcpDescriptor("search", "Searches the documentation"),
			mcpDescriptor("broken", ""),
		}}
		sessions := connectMCP(GinkgoTB(), fake, config.MCPServer{Name: "docs"})

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		cfg.MCPClients = []config.MCPServer{{Name: "docs"}}

		provider := agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done"))
		events := newMCPNoteRecorder()

		res, err := agent.Run(context.Background(), agent.Options{
			Config:      cfg,
			ConfigFile:  "agent.yaml",
			Prompt:      []string{"go"},
			Provider:    provider,
			MCPSessions: sessions,
		}, events, agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

		Expect(advertised(provider.Requests()[0].Tools, "docs_search")).To(BeTrue())
		Expect(advertised(provider.Requests()[0].Tools, "docs_broken")).To(BeFalse())

		notes := events.Notes()
		Expect(notes).To(HaveLen(1))
		Expect(notes[0].Server.Name).To(Equal("docs"))
		Expect(notes[0].Discovered).To(Equal(2))
		Expect(notes[0].Tools).To(HaveLen(1))
		Expect(notes[0].RTT).To(BeNumerically(">", time.Duration(0)))
		Expect(notes[0].Skipped).To(HaveLen(1))
		Expect(notes[0].Skipped[0].Name).To(Equal("broken"))
		Expect(notes[0].Skipped[0].Reason).To(ContainSubstring("advertises no description"))
	})

	// This pins the naming pass against both of the lookups a run keeps its claimed names
	// in: the taken set the application tools write, and the name map the a2a import
	// returns and never writes there. A clash with either aborts the run naming the tool
	// and the server it came from.
	Describe("a name collision", func() {
		It("Should abort against an application tool", func() {
			// The application's "docs search" command loads as the tool "docs_search", which
			// is the name the server's "search" would take under the alias "docs".
			application := fisk.New("app", "an app")
			application.Command("docs", "documentation commands").Command("search", "search the documentation")

			fake := &mcpFakeServers{tools: []*mcp.Tool{mcpDescriptor("search", "Searches the documentation")}}
			sessions := connectMCP(GinkgoTB(), fake, config.MCPServer{Name: "docs"})

			cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), application))
			cfg.MCPClients = []config.MCPServer{{Name: "docs"}}

			_, err := agent.Run(context.Background(), agent.Options{
				Config:      cfg,
				ConfigFile:  "agent.yaml",
				Prompt:      []string{"go"},
				Provider:    agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
				MCPSessions: sessions,
			}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
			Expect(err).To(MatchError(ContainSubstring(`"docs_search" (mcp server "docs")`)))
		})

		It("Should abort against a tool imported from a peer", func() {
			// The peer's tool is imported under its own name, since nothing local claims it,
			// so it takes the name the server's "search" would take under the alias "docs".
			transport := agenttest.NewFakeTransport(GinkgoTB(), a2a.AgentCard{
				Name:    "docs-svc",
				Version: "1.0.0",
				Tools: []a2a.ToolDescriptor{{
					Name:        "docs_search",
					Description: "search the documentation",
					InputSchema: json.RawMessage(`{"type":"object"}`),
				}},
			})

			fake := &mcpFakeServers{tools: []*mcp.Tool{mcpDescriptor("search", "Searches the documentation")}}
			sessions := connectMCP(GinkgoTB(), fake, config.MCPServer{Name: "docs"})

			cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), exampleApp()))
			cfg.RemoteTools = []config.RemoteToolHost{{Name: "docs-svc"}}
			cfg.MCPClients = []config.MCPServer{{Name: "docs"}}

			_, err := agent.Run(context.Background(), agent.Options{
				Config:       cfg,
				ConfigFile:   "agent.yaml",
				Prompt:       []string{"go"},
				Provider:     agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
				A2ATransport: transport,
				MCPSessions:  sessions,
			}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
			Expect(err).To(MatchError(ContainSubstring(`"docs_search" (mcp server "docs")`)))
		})
	})

	// This pins what an operator is left with when a collision aborts the run: the
	// outcomes of every server reach the reporter before the error is returned, so the
	// alias to set or the filter to drop is decided against the second server's tools and
	// round trip rather than the collision alone.
	It("Should report every server's notes when a collision aborts the run", func() {
		// The application's "docs search" command loads as the tool "docs_search", the name
		// the first server's "search" takes under the alias "docs". The second server's takes
		// "wiki_search", which nothing claims.
		application := fisk.New("app", "an app")
		application.Command("docs", "documentation commands").Command("search", "search the documentation")

		fake := &mcpFakeServers{tools: []*mcp.Tool{mcpDescriptor("search", "Searches the documentation")}}
		sessions := connectMCP(GinkgoTB(), fake, config.MCPServer{Name: "docs"}, config.MCPServer{Name: "wiki"})

		cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), application))
		cfg.MCPClients = []config.MCPServer{{Name: "docs"}, {Name: "wiki"}}

		events := newMCPNoteRecorder()

		_, err := agent.Run(context.Background(), agent.Options{
			Config:      cfg,
			ConfigFile:  "agent.yaml",
			Prompt:      []string{"go"},
			Provider:    agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
			MCPSessions: sessions,
		}, events, agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).To(MatchError(ContainSubstring(`"docs_search" (mcp server "docs")`)))

		notes := events.Notes()
		Expect(notes).To(HaveLen(2))

		Expect(notes[0].Server.Name).To(Equal("docs"))
		Expect(notes[0].Tools).To(BeEmpty())
		Expect(notes[0].Skipped).To(HaveLen(1))
		Expect(notes[0].Skipped[0].Name).To(Equal("search"))
		Expect(notes[0].Skipped[0].Reason).To(ContainSubstring(`the name "docs_search" is already taken`))

		Expect(notes[1].Server.Name).To(Equal("wiki"))
		Expect(notes[1].Discovered).To(Equal(1))
		Expect(notes[1].Tools).To(HaveLen(1))
		Expect(notes[1].Skipped).To(BeEmpty())
		Expect(notes[1].RTT).To(BeNumerically(">", time.Duration(0)))
	})

	// This covers the collision from the other direction: the MCP import runs before the
	// caller's custom tools, so a custom tool taking a name an imported tool already holds
	// aborts the run naming the mcp server it came from rather than shadowing it.
	It("Should abort when a custom tool takes an imported tool's name", func() {
		fake := &mcpFakeServers{tools: []*mcp.Tool{mcpDescriptor("search", "Searches the documentation")}}
		sessions := connectMCP(GinkgoTB(), fake, config.MCPServer{Name: "docs"})

		// The imported tool is named "docs_search", which is the name this custom tool takes.
		custom, err := functool.New(functool.Spec{
			Name:        "docs_search",
			Description: "a custom tool",
			Schema:      map[string]any{"type": "object"},
			Handler:     noopCustomHandler,
		})
		Expect(err).NotTo(HaveOccurred())

		cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), exampleApp()))
		cfg.MCPClients = []config.MCPServer{{Name: "docs"}}

		_, err = agent.Run(context.Background(), agent.Options{
			Config:      cfg,
			ConfigFile:  "agent.yaml",
			Prompt:      []string{"go"},
			Provider:    agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
			MCPSessions: sessions,
			CustomTools: []toolkit.Tool{custom},
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).To(MatchError(ContainSubstring(`custom tool at index 0 ("docs_search") collides with a tool of the same name imported from an mcp server`)))
	})

	// This proves Run leaves injected sessions open: the caller connected them once for
	// every run it hosts, so a run that closed them would take the next run's servers
	// down with it.
	It("Should leave injected sessions open", func() {
		ctx := context.Background()

		fake := &mcpFakeServers{tools: []*mcp.Tool{mcpDescriptor("search", "Searches the documentation")}}
		sessions := connectMCP(GinkgoTB(), fake, config.MCPServer{Name: "docs"})

		cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), exampleApp()))
		cfg.MCPClients = []config.MCPServer{{Name: "docs"}}

		res, err := agent.Run(ctx, agent.Options{
			Config:      cfg,
			ConfigFile:  "agent.yaml",
			Prompt:      []string{"go"},
			Provider:    agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
			MCPSessions: sessions,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

		// The session the run borrowed still answers, which a closed Sessions refuses
		// before it reaches the server.
		err = sessions.Use(ctx, "docs", func(session *mcp.ClientSession) error {
			_, err := session.ListTools(ctx, nil)
			return err
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// This pins what a run does when the sessions it was handed were opened for other
	// servers. The import walks the list the sessions carry, so a run would otherwise
	// import the injector's servers, under the injector's aliases and filters, without
	// either side noticing. The check sits in Run rather than in a host, so it covers a
	// caller setting Options.MCPSessions directly as well as one that hosts runs behind a
	// channel.
	Describe("mismatched injected sessions", func() {
		It("Should abort on a configured server that was not connected", func() {
			fake := &mcpFakeServers{tools: []*mcp.Tool{mcpDescriptor("search", "Searches the documentation")}}
			sessions := connectMCP(GinkgoTB(), fake, config.MCPServer{Name: "docs"})

			cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), exampleApp()))
			cfg.MCPClients = []config.MCPServer{{Name: "docs"}, {Name: "wiki"}}

			_, err := agent.Run(context.Background(), agent.Options{
				Config:      cfg,
				ConfigFile:  "agent.yaml",
				Prompt:      []string{"go"},
				Provider:    agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
				MCPSessions: sessions,
			}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
			Expect(err).To(MatchError(ContainSubstring("configured but not connected: wiki")))
		})

		It("Should abort on a connected server the run never configured", func() {
			fake := &mcpFakeServers{tools: []*mcp.Tool{mcpDescriptor("search", "Searches the documentation")}}
			sessions := connectMCP(GinkgoTB(), fake, config.MCPServer{Name: "docs"}, config.MCPServer{Name: "wiki"})

			cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), exampleApp()))
			cfg.MCPClients = []config.MCPServer{{Name: "docs"}}

			_, err := agent.Run(context.Background(), agent.Options{
				Config:      cfg,
				ConfigFile:  "agent.yaml",
				Prompt:      []string{"go"},
				Provider:    agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
				MCPSessions: sessions,
			}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
			Expect(err).To(MatchError(ContainSubstring("connected but not configured: wiki")))
		})
	})

	// This is the other half of the lifetime rule: a run given no sessions connects its
	// own at start and closes them at the end, so a terminal run leaves nothing connected
	// and no stdio child running.
	It("Should close the sessions it opened itself", func() {
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
		DeferCleanup(endpoint.Close)

		cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), exampleApp()))
		cfg.MCPClients = []config.MCPServer{{Name: "docs", URL: endpoint.URL}}

		provider := agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done"))

		res, err := agent.Run(context.Background(), agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			Provider:   provider,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

		// The run reached this server, and ended the session it opened before it returned.
		Expect(advertised(provider.Requests()[0].Tools, "docs_search")).To(BeTrue())
		Eventually(terminated, 10*time.Second).Should(Receive())
	})

	// This proves the no-tools gate counts imported MCP tools: an agent wrapping an
	// application with no commands, with no built-in, remote or injected tools, starts
	// and completes on a server's tools alone rather than reporting that it has none.
	It("Should start a run on a server's tools alone", func() {
		fake := &mcpFakeServers{tools: []*mcp.Tool{mcpDescriptor("search", "Searches the documentation")}}
		sessions := connectMCP(GinkgoTB(), fake, config.MCPServer{Name: "docs"})

		cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), emptyFiskApp()))
		cfg.MCPClients = []config.MCPServer{{Name: "docs"}}

		provider := agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done"))

		res, err := agent.Run(context.Background(), agent.Options{
			Config:      cfg,
			ConfigFile:  "agent.yaml",
			Prompt:      []string{"go"},
			Provider:    provider,
			MCPSessions: sessions,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))
		Expect(advertised(provider.Requests()[0].Tools, "docs_search")).To(BeTrue())
	})
})
