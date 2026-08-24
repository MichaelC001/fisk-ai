//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package mcpclient

import (
	"context"
	"encoding/json"
	"os"
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
		f.mu.Unlock()

		return clientSide, nil
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
