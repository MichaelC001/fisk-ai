//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// These specs cover the MCP sessions a worker opens once and shares with every run it
// hosts: that the shared set reaches a run, that two runs calling one server at the
// same time each get their own answer back, that a run finding a server dead fails
// naming it while the next run gets a reconnect, that a drain leaves the sessions
// alone and Close ends them, and that a server which will not start stops the worker
// at startup rather than the first job to arrive.
package serve_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/mcpclient"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/serve"
)

// callBarrier holds each caller until want of them have arrived, so a spec requires
// calls to overlap rather than hoping they did. A caller waiting longer than wait
// gives up and reports how many arrived, which turns calls that were serialized into
// a failed spec rather than a hung one.
type callBarrier struct {
	want int
	wait time.Duration

	mu      sync.Mutex
	arrived int
	ready   chan struct{}
}

func newCallBarrier(want int, wait time.Duration) *callBarrier {
	return &callBarrier{want: want, wait: wait, ready: make(chan struct{})}
}

func (b *callBarrier) arrive() error {
	if b == nil {
		return nil
	}

	b.mu.Lock()
	b.arrived++
	if b.arrived == b.want {
		close(b.ready)
	}
	b.mu.Unlock()

	select {
	case <-b.ready:
		return nil
	case <-time.After(b.wait):
		b.mu.Lock()
		defer b.mu.Unlock()

		return fmt.Errorf("only %d of %d calls were in flight at once", b.arrived, b.want)
	}
}

// mcpEchoServer stands a real mcp.Server up for every dial, over an in-memory
// transport pair, so a hosted run drives genuine protocol traffic with no subprocess
// and no socket. Its one tool repeats the token it was sent, so two runs calling it at
// once can be told apart by their answers.
type mcpEchoServer struct {
	barrier *callBarrier

	mu    sync.Mutex
	dials int
	fail  error
	links []mcp.Connection
}

// mcpLinkedTransport keeps the connection it opened so the server can break the link
// under a live session.
type mcpLinkedTransport struct {
	inner mcp.Transport
	fake  *mcpEchoServer
}

func (t *mcpLinkedTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	conn, err := t.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}

	t.fake.mu.Lock()
	t.fake.links = append(t.fake.links, conn)
	t.fake.mu.Unlock()

	return conn, nil
}

func newMCPEchoServer(barrier *callBarrier) *mcpEchoServer {
	return &mcpEchoServer{barrier: barrier}
}

// dialer builds the mcpclient.Dialer these sessions connect through. A new server is
// stood up per dial, which is what a reconnect reaches.
func (s *mcpEchoServer) dialer() mcpclient.Dialer {
	return func(_ context.Context, server config.MCPServer) (mcp.Transport, error) {
		s.mu.Lock()
		s.dials++
		fail := s.fail
		s.mu.Unlock()

		if fail != nil {
			return nil, fail
		}

		clientSide, serverSide := mcp.NewInMemoryTransports()

		srv := mcp.NewServer(&mcp.Implementation{Name: server.Name, Version: "1.0.0"}, nil)
		srv.AddTool(&mcp.Tool{
			Name:        "echo",
			Description: "Repeats the token it is sent",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}, s.echo)

		// The server side is connected before the client, as in-memory transports
		// require, and on a context of its own: the caller's carries the connect
		// timeout, which has nothing to say about how long this server lives.
		_, err := srv.Connect(context.Background(), serverSide, nil)
		if err != nil {
			return nil, err
		}

		return &mcpLinkedTransport{inner: clientSide, fake: s}, nil
	}
}

// echo answers with the token it was sent, once the barrier has released it.
func (s *mcpEchoServer) echo(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args struct {
		Token string `json:"token"`
	}

	if len(req.Params.Arguments) > 0 {
		err := json.Unmarshal(req.Params.Arguments, &args)
		if err != nil {
			return nil, err
		}
	}

	err := s.barrier.arrive()
	if err != nil {
		return nil, err
	}

	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "echoed " + args.Token}}}, nil
}

// dialCount is how many times a transport was built, which is one per session opened.
func (s *mcpEchoServer) dialCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.dials
}

// kill takes every server down and leaves the next dial failing, which is a stdio
// child that died or an endpoint that stopped answering.
//
// The link is broken rather than the server session closed: a client watching a
// server's tool list holds a subscriptions/listen request open, and the SDK's Close
// waits for its in-flight requests to finish, so a server closing its own session
// would wait for the client it is being closed away from.
func (s *mcpEchoServer) kill(err error) {
	s.mu.Lock()
	s.fail = err
	links := s.links
	s.links = nil
	s.mu.Unlock()

	for _, link := range links {
		_ = link.Close()
	}
}

// revive lets the next dial succeed again.
func (s *mcpEchoServer) revive() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.fail = nil
}

// mcpEchoProvider drives a run from the request rather than from a script: it asks for
// the imported tool once, carrying the run's own prompt as the token, and then answers
// with whatever the tool returned. A scripted queue of responses could not serve two
// concurrent runs, since their calls interleave and each would be handed the other's
// line.
type mcpEchoProvider struct{}

func (p *mcpEchoProvider) Capabilities() llm.Caps {
	return llm.Caps{Provider: "anthropic"}
}

func (p *mcpEchoProvider) Call(_ context.Context, req llm.Request) (*llm.Response, error) {
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("the request carries no messages")
	}

	last := req.Messages[len(req.Messages)-1]
	for _, block := range last.Content {
		if block.ToolResult != nil {
			return agenttest.TextResponse(block.ToolResult.Content), nil
		}
	}

	var token string
	for _, block := range req.Messages[0].Content {
		if block.Text != nil {
			token = block.Text.Text
			break
		}
	}

	args, err := json.Marshal(map[string]string{"token": token})
	if err != nil {
		return nil, err
	}

	return agenttest.ToolUseResponse("call-1", "docs_echo", args), nil
}

var _ = Describe("Shared MCP sessions", func() {
	var (
		ctx    context.Context
		cancel context.CancelFunc
	)

	BeforeEach(func() {
		ctx, cancel = context.WithTimeout(context.Background(), 60*time.Second)
		DeferCleanup(cancel)
	})

	// connected opens the sessions a worker holds for every run it hosts and closes
	// them when the spec ends, which is what owning them means.
	connected := func(fake *mcpEchoServer, names ...string) *mcpclient.Sessions {
		GinkgoHelper()

		entries := make([]config.MCPServer, 0, len(names))
		for _, name := range names {
			entries = append(entries, config.MCPServer{Name: name})
		}

		sessions, err := mcpclient.Connect(ctx, mcpclient.Options{
			Servers:  entries,
			Identity: "agent",
			Version:  "0.0.1",
			Dialer:   fake.dialer(),
		})
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(func() { Expect(sessions.Close(ctx)).To(Succeed()) })

		return sessions
	}

	mcpConfig := func(names ...string) *config.Config {
		GinkgoHelper()

		cfg := servedConfig()
		for _, name := range names {
			cfg.MCPClients = append(cfg.MCPClients, config.MCPServer{Name: name})
		}

		return cfg
	}

	// The claim the shared set rests on: the SDK's client session multiplexes by
	// request id, so two runs calling one server at the same time are two calls in
	// flight on one connection rather than one queued behind the other. The tool holds
	// each caller until both have arrived, so a run that had been serialized behind its
	// sibling fails the spec instead of quietly passing.
	It("Should give two runs calling one server at once their own answer each", func() {
		fake := newMCPEchoServer(newCallBarrier(2, 20*time.Second))
		sessions := connected(fake, "docs")

		ch := agenttest.NewScriptedChannel(GinkgoTB(), "jobs",
			&serve.Work{ID: "job-a", Prompt: "token-a"},
			&serve.Work{ID: "job-b", Prompt: "token-b"},
		)

		srv, err := serve.New(serve.Options{
			Channels:    []serve.Channel{ch},
			Config:      mcpConfig("docs"),
			Provider:    &mcpEchoProvider{},
			MCPSessions: sessions,
			Concurrency: 2,
			Logger:      quietLogger(),
		})
		Expect(err).ToNot(HaveOccurred())
		Expect(srv.Serve(ctx)).To(Succeed())

		answers := map[string]string{}
		for _, out := range ch.Outcomes() {
			Expect(out.Err).ToNot(HaveOccurred())
			Expect(out.Reason).To(Equal(runstate.ReasonCompleted))
			answers[out.ID] = out.Text
		}

		Expect(answers).To(HaveLen(2))
		Expect(answers["job-a"]).To(Equal("echoed token-a"))
		Expect(answers["job-b"]).To(Equal("echoed token-b"))

		Expect(fake.dialCount()).To(Equal(1), "both runs used the one session opened at startup")
	})

	// A session that has ended cannot be revived, so the run that finds the server gone
	// fails naming it rather than holding a tool set that no longer answers. Reconnect
	// belongs to the sessions, so the run after it gets a live server without the worker
	// restarting.
	It("Should fail a run naming a dead server and reconnect for the next one", func() {
		fake := newMCPEchoServer(nil)
		sessions := connected(fake, "docs")

		runOne := func(id string) serve.Outcome {
			GinkgoHelper()

			ch := agenttest.NewScriptedChannel(GinkgoTB(), "jobs", &serve.Work{ID: id, Prompt: "go"})

			srv, err := serve.New(serve.Options{
				Channels:    []serve.Channel{ch},
				Config:      mcpConfig("docs"),
				Provider:    agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
				MCPSessions: sessions,
				Logger:      quietLogger(),
			})
			Expect(err).ToNot(HaveOccurred())
			Expect(srv.Serve(ctx)).To(Succeed())

			outcomes := ch.Outcomes()
			Expect(outcomes).To(HaveLen(1))

			return outcomes[0]
		}

		Expect(runOne("job-1").Err).ToNot(HaveOccurred())

		fake.kill(fmt.Errorf("the server is not running"))

		// The client learns a session ended asynchronously, so the spec waits for the
		// sessions to have noticed rather than racing the run against the notification.
		Eventually(func() error {
			return sessions.Use(ctx, "docs", func(session *mcp.ClientSession) error {
				_, err := session.ListTools(ctx, nil)
				return err
			})
		}).Should(MatchError(ContainSubstring("the server is not running")))

		out := runOne("job-2")
		Expect(out.Err).To(MatchError(ContainSubstring(`mcp server "docs"`)))
		Expect(out.Err).To(MatchError(ContainSubstring("the server is not running")))

		fake.revive()

		Expect(runOne("job-3").Err).ToNot(HaveOccurred())
		Expect(fake.dialCount()).To(BeNumerically(">", 1), "the next run got a new session")
	})

	// A run borrows the sessions and never closes them: one that did would take the
	// servers away from every run after it.
	It("Should leave the sessions open for the runs after one has ended", func() {
		fake := newMCPEchoServer(nil)
		sessions := connected(fake, "docs")

		ch := agenttest.NewScriptedChannel(GinkgoTB(), "jobs", &serve.Work{ID: "job-1", Prompt: "go"})

		srv, err := serve.New(serve.Options{
			Channels:    []serve.Channel{ch},
			Config:      mcpConfig("docs"),
			Provider:    agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
			MCPSessions: sessions,
			Logger:      quietLogger(),
		})
		Expect(err).ToNot(HaveOccurred())
		Expect(srv.Serve(ctx)).To(Succeed())

		Expect(ch.Outcomes()[0].Err).ToNot(HaveOccurred())

		// Drain releases the channels and the services and deliberately leaves the
		// borrowed resources alone, so the same session still answers afterwards.
		Expect(srv.Drain()).To(Succeed())

		err = sessions.Use(ctx, "docs", func(session *mcp.ClientSession) error {
			_, err := session.ListTools(ctx, nil)
			return err
		})
		Expect(err).ToNot(HaveOccurred())
		Expect(fake.dialCount()).To(Equal(1))
	})

	// The run imports the servers the sessions hold rather than the ones its
	// configuration names, so a set built somewhere else would import its own servers
	// under its own aliases and filters. That is refused instead.
	It("Should refuse a run whose configured servers are not the ones it was given", func() {
		fake := newMCPEchoServer(nil)
		sessions := connected(fake, "docs")

		ch := agenttest.NewScriptedChannel(GinkgoTB(), "jobs", &serve.Work{ID: "job-1", Prompt: "go"})

		srv, err := serve.New(serve.Options{
			Channels:    []serve.Channel{ch},
			Config:      mcpConfig("docs", "wiki"),
			Provider:    agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
			MCPSessions: sessions,
			Logger:      quietLogger(),
		})
		Expect(err).ToNot(HaveOccurred())
		Expect(srv.Serve(ctx)).To(Succeed())

		Expect(ch.Outcomes()[0].Err).To(MatchError(ContainSubstring("configured but not connected: wiki")))
	})
})

var _ = Describe("Resources and MCP servers", func() {
	// A worker connects its servers where an operator is watching it start, rather than
	// discovering on the first job that a server it needs will not run.
	It("Should stop the build when a configured server will not start", func() {
		cfg := servedConfig()
		cfg.MCPClients = []config.MCPServer{{Name: "docs", Command: "fisk-mcp-server-that-does-not-exist"}}

		res, err := serve.NewResources(context.Background(), cfg, serve.ResourceOptions{ConfigFile: "agent.yaml"})
		Expect(err).To(MatchError(ContainSubstring(`connecting to mcp server "docs"`)))
		Expect(res).To(BeNil())
	})

	It("Should leave the sessions nil when the configuration declares no servers", func() {
		res, err := serve.NewResources(context.Background(), servedConfig(), serve.ResourceOptions{})
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(func() { Expect(res.Close()).To(Succeed()) })

		Expect(res.MCPSessions).To(BeNil())
	})

	// The whole path a hosted run's tools travel: the worker connects once, ApplyTo puts
	// the sessions on the server's options, and Close ends them when Serve has returned.
	It("Should connect the configured servers, hand them to a server and close them", func() {
		endpoint := mcpEndpoint()

		cfg := servedConfig()
		cfg.MCPClients = []config.MCPServer{{Name: "docs", URL: endpoint}}

		res, err := serve.NewResources(context.Background(), cfg, serve.ResourceOptions{ConfigFile: "agent.yaml"})
		Expect(err).ToNot(HaveOccurred())
		Expect(res.MCPSessions).ToNot(BeNil())
		Expect(res.MCPSessions.Names()).To(Equal([]string{"docs"}))

		var opts serve.Options
		res.ApplyTo(&opts)
		Expect(opts.MCPSessions).To(BeIdenticalTo(res.MCPSessions))

		sessions := res.MCPSessions
		Expect(res.Close()).To(Succeed())
		Expect(res.MCPSessions).To(BeNil())

		err = sessions.Use(context.Background(), "docs", func(*mcp.ClientSession) error { return nil })
		Expect(err).To(MatchError(ContainSubstring("are closed")))
	})
})

// mcpEndpoint stands a streamable HTTP MCP server up for the spec's lifetime and
// returns its url, so NewResources connects over a transport it built itself rather
// than one a spec handed it.
func mcpEndpoint() string {
	GinkgoHelper()

	srv := mcp.NewServer(&mcp.Implementation{Name: "docs", Version: "1.0.0"}, nil)
	srv.AddTool(&mcp.Tool{
		Name:        "search",
		Description: "Searches the documentation",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "searched"}}}, nil
	})

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	endpoint := httptest.NewServer(handler)
	DeferCleanup(endpoint.Close)

	return endpoint.URL
}
