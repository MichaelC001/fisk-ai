//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package mcpclient

import (
	"context"
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/llm"
)

func TestMCPClient(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Internal/MCPClient")
}

// fakeCredEnvVar is the sentinel credential variable the fake provider below
// declares, so the credential-strip test can prove the mechanism without linking
// a real provider or duplicating its list.
const fakeCredEnvVar = "FISK_MCPCLIENT_PROVIDER_SECRET"

// literalToken stands in for a credential an operator wrote into a url as literal text
// rather than as a "${VAR}" reference, so a spec can assert that it reaches no error.
const literalToken = "fisk-mcpclient-literal-token"

// referencedToken stands in for a credential an operator kept in a variable and
// referenced from a url, so a spec can assert that its value reaches no error even
// where a url's structure gives no clue that a segment holds a credential.
const referencedToken = "fisk-mcpclient-referenced-token"

// referencedTokenVar is the variable referencedToken is read from.
const referencedTokenVar = "FISK_MCPCLIENT_PATH_TOKEN"

type fakeProvider struct{}

func (fakeProvider) Call(context.Context, llm.Request) (*llm.Response, error) { return nil, nil }
func (fakeProvider) Capabilities() llm.Caps                                   { return llm.Caps{} }

// init registers a fake provider so llm.CredentialEnvNames is non-empty in this
// test binary; childEnv strips whatever a linked provider declared, and this
// package links none of the real ones.
func init() {
	llm.Register("mcpclient-fake", func(llm.Config) (llm.Provider, error) {
		return fakeProvider{}, nil
	}, []string{fakeCredEnvVar})
}

// fakeServers stands a real mcp.Server up in this process for every server a
// Sessions asks it to dial, connected over an in-memory transport pair, so the
// specs drive genuine protocol traffic without a socket or a subprocess. It
// keeps every session it serves so a spec can watch them end.
type fakeServers struct {
	mu       sync.Mutex
	sessions []*mcp.ServerSession
	dials    map[string]int
	fail     map[string]error
	tools    map[string][]fakeTool
	pageSize map[string]int
	listing  map[string]func(*mcp.ListToolsResult)
	stall    map[string]chan struct{}
	// running is the mcp.Server standing behind each name, so a spec can change a
	// server's tool list for real and let it tell its client about it.
	running map[string]*mcp.Server
	// listed counts the tools/list requests each server answered, so a spec can prove
	// which server a notification made re-list and which it left alone.
	listed map[string]int
	// listFail is the error a server answers tools/list with once a spec has asked it
	// to stop being listable.
	listFail map[string]error
	// links are the client-side connections, so a spec can break one.
	links map[string]mcp.Connection
}

// linkedTransport keeps the connection it opened, so a spec can break the link under a
// live session.
type linkedTransport struct {
	inner mcp.Transport
	name  string
	fake  *fakeServers
}

func (t *linkedTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	conn, err := t.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}

	t.fake.mu.Lock()
	t.fake.links[t.name] = conn
	t.fake.mu.Unlock()

	return conn, nil
}

// fakeTool is one tool a fake server offers: the descriptor it advertises and the
// handler that answers a call to it.
type fakeTool struct {
	tool    *mcp.Tool
	handler mcp.ToolHandler
}

func newFakeServers() *fakeServers {
	return &fakeServers{
		dials:    map[string]int{},
		fail:     map[string]error{},
		tools:    map[string][]fakeTool{},
		pageSize: map[string]int{},
		listing:  map[string]func(*mcp.ListToolsResult){},
		stall:    map[string]chan struct{}{},
		running:  map[string]*mcp.Server{},
		listed:   map[string]int{},
		listFail: map[string]error{},
		links:    map[string]mcp.Connection{},
	}
}

// server is the mcp.Server standing behind one configured name.
func (f *fakeServers) server(name string) *mcp.Server {
	GinkgoHelper()

	f.mu.Lock()
	defer f.mu.Unlock()

	srv, ok := f.running[name]
	Expect(ok).To(BeTrue(), "no server named %q has been dialed", name)

	return srv
}

// failListing makes the named server answer every later tools/list with err, so a
// spec can drive a server that answered once and then stopped being listable.
func (f *fakeServers) failListing(name string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.listFail[name] = err
}

// lists is how many tools/list requests one server has answered.
func (f *fakeServers) lists(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.listed[name]
}

// listCounter counts the tools/list requests a server answers and refuses them once
// failListing has been called for it.
func (f *fakeServers) listCounter(name string) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/list" {
				return next(ctx, method, req)
			}

			f.mu.Lock()
			f.listed[name]++
			err := f.listFail[name]
			f.mu.Unlock()

			if err != nil {
				return nil, err
			}

			return next(ctx, method, req)
		}
	}
}

// stallList makes the named server accept its connection, answer the handshake and
// then never answer tools/list, so a spec can drive a server that goes quiet once it
// is connected. The stall is released when the spec ends, so the handler it holds
// does not outlive it.
func (f *fakeServers) stallList(server string) {
	GinkgoHelper()

	release := make(chan struct{})

	f.mu.Lock()
	f.stall[server] = release
	f.mu.Unlock()

	DeferCleanup(func() { close(release) })
}

// listStaller holds every tools/list answer until release is closed or the request
// is canceled.
func listStaller(release <-chan struct{}) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/list" {
				return next(ctx, method, req)
			}

			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}

			return next(ctx, method, req)
		}
	}
}

// listRewriter edits the tools/list answer on its way out, so a spec can serve a
// descriptor mcp.Server.AddTool refuses to register, such as one whose input schema
// is not an object. It edits a copy, leaving the server's own registry alone.
func listRewriter(edit func(*mcp.ListToolsResult)) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			if err != nil || method != "tools/list" {
				return res, err
			}

			list, ok := res.(*mcp.ListToolsResult)
			if !ok {
				return res, nil
			}

			copied := *list
			copied.Tools = make([]*mcp.Tool, 0, len(list.Tools))
			for _, tool := range list.Tools {
				dup := *tool
				copied.Tools = append(copied.Tools, &dup)
			}
			edit(&copied)

			return &copied, nil
		}
	}
}

// dialer builds the Dialer to hand to Options.
func (f *fakeServers) dialer() Dialer {
	return func(_ context.Context, server config.MCPServer) (mcp.Transport, error) {
		f.mu.Lock()
		f.dials[server.Name]++
		err := f.fail[server.Name]
		tools, custom := f.tools[server.Name]
		pageSize := f.pageSize[server.Name]
		rewrite := f.listing[server.Name]
		stall := f.stall[server.Name]
		f.mu.Unlock()

		if err != nil {
			return nil, err
		}

		if !custom {
			tools = []fakeTool{{
				tool: &mcp.Tool{
					Name:        "search",
					Description: "Searches the documentation",
					InputSchema: json.RawMessage(`{"type":"object"}`),
				},
				handler: func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "found"}}}, nil
				},
			}}
		}

		clientSide, serverSide := mcp.NewInMemoryTransports()

		srv := mcp.NewServer(&mcp.Implementation{Name: server.Name, Version: "9.9.9"}, &mcp.ServerOptions{PageSize: pageSize})
		for _, t := range tools {
			srv.AddTool(t.tool, t.handler)
		}
		srv.AddReceivingMiddleware(f.listCounter(server.Name))
		if rewrite != nil {
			srv.AddReceivingMiddleware(listRewriter(rewrite))
		}
		if stall != nil {
			srv.AddReceivingMiddleware(listStaller(stall))
		}

		// The server side is connected before the client, as in-memory transports
		// require, and under a context of its own: the caller's carries the connect
		// timeout, which has nothing to say about how long this server lives.
		session, err := srv.Connect(context.Background(), serverSide, nil)
		if err != nil {
			return nil, err
		}

		f.mu.Lock()
		f.sessions = append(f.sessions, session)
		f.running[server.Name] = srv
		f.mu.Unlock()

		return &linkedTransport{inner: clientSide, name: server.Name, fake: f}, nil
	}
}

// breakLink closes the connection under one server's client session, which is how a
// spec ends a session from outside it: the client reads a broken link and the session
// ends, as it does when a stdio child dies.
//
// A server that has a client subscribed to its tool list cannot close its own session
// instead. The subscriptions/listen handler runs until the client cancels it, and the
// SDK's Close waits for its in-flight requests to finish, so the two wait for each
// other.
func (f *fakeServers) breakLink(name string) {
	GinkgoHelper()

	f.mu.Lock()
	conn, ok := f.links[name]
	f.mu.Unlock()

	Expect(ok).To(BeTrue(), "no server named %q has been dialed", name)
	Expect(conn.Close()).To(Succeed())
}

// listenGoroutines counts the goroutines parked in the SDK's subscriptions/listen
// watcher. One is opened per client session, since the tool-list handler is always
// set, and it waits on a context derived from context.Background() that only
// ClientSession.Close cancels, so a session replaced without being closed leaves one
// running for the life of the process.
//
// The frame is matched rather than the "created by" line a dump also carries for it,
// so one goroutine counts once. A probe that matched nothing would make every count
// zero, which the spec catches by asserting the live session's watcher before it
// breaks anything.
func listenGoroutines() int {
	buf := make([]byte, 64*1024)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return strings.Count(string(buf[:n]), "mcp.callSubscriptionsListen.func1(")
		}

		buf = make([]byte, 2*len(buf))
	}
}

// served returns the sessions the fake servers have accepted.
func (f *fakeServers) served() []*mcp.ServerSession {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]*mcp.ServerSession(nil), f.sessions...)
}

// ended reports a channel that is closed once ss has ended, for Eventually.
func ended(ss *mcp.ServerSession) chan struct{} {
	done := make(chan struct{})
	go func() {
		_ = ss.Wait()
		close(done)
	}()

	return done
}

// recordingTransport wraps a transport and keeps every message the client writes
// through it, so a spec can assert on the bytes that actually went to the server
// rather than on the SDK types they were built from.
type recordingTransport struct {
	inner mcp.Transport

	mu   sync.Mutex
	sent [][]byte
}

func (t *recordingTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	conn, err := t.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}

	return &recordingConn{Connection: conn, transport: t}, nil
}

func (t *recordingTransport) written() []string {
	t.mu.Lock()
	defer t.mu.Unlock()

	out := make([]string, 0, len(t.sent))
	for _, msg := range t.sent {
		out = append(out, string(msg))
	}

	return out
}

type recordingConn struct {
	mcp.Connection
	transport *recordingTransport
}

func (c *recordingConn) Write(ctx context.Context, msg jsonrpc.Message) error {
	data, err := jsonrpc.EncodeMessage(msg)
	if err == nil {
		c.transport.mu.Lock()
		c.transport.sent = append(c.transport.sent, data)
		c.transport.mu.Unlock()
	}

	return c.Connection.Write(ctx, msg)
}

// setenv sets an environment variable for the duration of one spec.
func setenv(name string, value string) {
	GinkgoHelper()

	Expect(os.Setenv(name, value)).To(Succeed())
	DeferCleanup(func() {
		Expect(os.Unsetenv(name)).To(Succeed())
	})
}
