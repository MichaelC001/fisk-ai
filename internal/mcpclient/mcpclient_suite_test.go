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
}

func newFakeServers() *fakeServers {
	return &fakeServers{dials: map[string]int{}, fail: map[string]error{}}
}

// dialer builds the Dialer to hand to Options.
func (f *fakeServers) dialer() Dialer {
	return func(_ context.Context, server config.MCPServer) (mcp.Transport, error) {
		f.mu.Lock()
		f.dials[server.Name]++
		err := f.fail[server.Name]
		f.mu.Unlock()

		if err != nil {
			return nil, err
		}

		clientSide, serverSide := mcp.NewInMemoryTransports()

		srv := mcp.NewServer(&mcp.Implementation{Name: server.Name, Version: "9.9.9"}, nil)
		srv.AddTool(&mcp.Tool{
			Name:        "search",
			Description: "Searches the documentation",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "found"}}}, nil
		})

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
