//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// Package mcpclient imports the tools of a third-party MCP server into an agent
// run. It is the policy and transport layer over the client half of the Model
// Context Protocol SDK: it builds a transport from an mcp_servers entry,
// resolves that entry's "${VAR}" references against the process environment,
// connects one session per server and holds those sessions for the caller to
// reach by name and close when the run ends. Over those sessions it lists each
// server's tools, applies the entry's include and exclude filters, names each
// survivor "<alias>_<tool>", and builds it as a functool.Tool the model calls like
// any other.
//
// It is the counterpart of internal/remotetools, which is the same layer for the
// tools an agent imports from a2a peers, and it copies that package's shape:
// what a server offers is returned as data rather than printed, and errors are
// returned rather than logged, so the same code serves both an agent run, which
// treats an unreachable server as fatal, and fisk info, which reports it and
// carries on.
package mcpclient

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/choria-io/fisk-ai/config"
)

// Dialer builds the transport for one configured server. Sessions uses the
// transports this package builds from the entry itself, over stdio for a server
// with a command and streamable HTTP for one with a url, unless Options.Dialer
// supplies a Dialer of its own. An embedder speaking a transport this package
// does not build, and a test driving a server in the same process over
// mcp.NewInMemoryTransports, supply one.
//
// A transport is connected exactly once, so a Dialer that may be asked twice for
// the same server, as it is when a session is replaced, returns a new transport
// each time.
type Dialer func(ctx context.Context, server config.MCPServer) (mcp.Transport, error)

// Options configures the sessions Connect opens.
type Options struct {
	// Servers are the MCP servers to connect, in the order they were configured.
	// Each name is connected once, so two entries sharing a name are an error.
	Servers []config.MCPServer
	// Identity is the client name each server is told at initialize, the way
	// mcpserver.Options.Name identifies this process to the clients it serves. Pass
	// the agent's identity.
	Identity string
	// Version is the client version each server is told at initialize. Pass the
	// build version.
	Version string
	// CredentialEnvNames are the operator-named environment variables holding a
	// credential, which are removed from the environment of a stdio child on top of
	// the ones every llm provider linked into the build declares. Pass
	// config.Config.CredentialEnvNames.
	CredentialEnvNames []string
	// LookupEnv resolves the "${VAR}" references in an entry's env and headers.
	// Nil reads the process environment through os.LookupEnv.
	LookupEnv func(name string) (string, bool)
	// Dialer overrides how a transport is built for a server. Nil builds the stdio
	// and streamable HTTP transports this package builds from the entry.
	Dialer Dialer
}

// Sessions holds one live MCP session per configured server. It is safe for
// concurrent use: one Sessions backs every run a server process hosts, and the
// tools imported from it are called from many runs at once.
//
// Sessions owns the sessions it opens. A session that has ended cannot be
// revived, so a caller reaches its session through Use for each call rather than
// holding the mcp.ClientSession, which Sessions may have replaced since.
type Sessions struct {
	opts    Options
	names   []string
	entries map[string]*entry

	mu     sync.Mutex
	closed bool
}

// entry is one configured server and the session currently connected to it. Its
// own mutex guards the session, so replacing one server's session leaves calls
// to every other server running.
type entry struct {
	server config.MCPServer

	mu      sync.Mutex
	session *mcp.ClientSession
	// done is closed when session ends, whether the server went away, the child
	// died, or Close closed it.
	done chan struct{}
}

// Connect opens a session with every configured server, in the order they were
// configured, and returns them keyed by name. Each server gets its own
// StartupTimeout to be started or reached and to finish the initialize
// handshake, and its "${VAR}" references are resolved here rather than when the
// config was parsed, so a variable that is not set fails this call naming the
// variable and the server.
//
// A server that cannot be connected fails the call: the sessions already opened
// are closed, so a failed Connect leaves nothing running. The error is returned
// rather than logged, because whether an unreachable server is fatal is the
// caller's decision.
//
// The caller owns the returned Sessions and must Close it, which ends every
// session and stops every stdio child it started.
func Connect(ctx context.Context, opts Options) (*Sessions, error) {
	if opts.LookupEnv == nil {
		opts.LookupEnv = os.LookupEnv
	}

	s := &Sessions{
		opts:    opts,
		names:   make([]string, 0, len(opts.Servers)),
		entries: make(map[string]*entry, len(opts.Servers)),
	}

	for _, server := range opts.Servers {
		_, dup := s.entries[server.Name]
		if dup {
			_ = s.Close()
			return nil, fmt.Errorf("mcp server %q is configured more than once", server.Name)
		}

		e := &entry{server: server}
		err := s.open(ctx, e)
		if err != nil {
			_ = s.Close()
			return nil, err
		}

		s.entries[server.Name] = e
		s.names = append(s.names, server.Name)
	}

	return s, nil
}

// Names are the configured server names, in the order they were configured.
func (s *Sessions) Names() []string {
	return slices.Clone(s.names)
}

// configured are the entries the sessions were opened for, in the order they were
// configured, for the import to read each one's alias and filters.
func (s *Sessions) configured() []config.MCPServer {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]config.MCPServer, 0, len(s.names))
	for _, name := range s.names {
		out = append(out, s.entries[name].server)
	}

	return out
}

// Use calls fn with the live session for the named server. It is how a tool
// handler reaches its server: a session that has ended is replaced first, so fn
// is always given one that was live when it was handed over, which a caller
// holding a session value from an earlier call would not be.
//
// fn's error is returned as it is. An unknown name, a session that cannot be
// replaced, and a Sessions that has been closed are reported without calling fn.
// A Close that lands while the call is in flight is reported the same way.
func (s *Sessions) Use(ctx context.Context, name string, fn func(session *mcp.ClientSession) error) error {
	session, err := s.live(ctx, name)
	if err != nil {
		return err
	}

	return fn(session)
}

// Close ends every session, which closes the stdin of each stdio child and gives
// it the SDK's terminate window to exit before it is signaled. It is idempotent,
// and every session is closed even when one of them reports a failure.
func (s *Sessions) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	entries := make([]*entry, 0, len(s.entries))
	for _, name := range s.names {
		entries = append(entries, s.entries[name])
	}
	s.mu.Unlock()

	var errs []error
	for _, e := range entries {
		// Waiting on the entry lock is what stops a session opened by a concurrent
		// replacement from outliving Close: that replacement either sees the closed
		// flag and refuses, or completes and is closed here.
		e.mu.Lock()
		if e.session != nil {
			err := e.session.Close()
			if err != nil {
				errs = append(errs, fmt.Errorf("closing the session with mcp server %q: %w", e.server.Name, err))
			}
			e.session = nil
		}
		e.mu.Unlock()
	}

	return errors.Join(errs...)
}

// live returns the session for the named server, replacing one that has ended.
func (s *Sessions) live(ctx context.Context, name string) (*mcp.ClientSession, error) {
	s.mu.Lock()
	closed := s.closed
	e, known := s.entries[name]
	s.mu.Unlock()

	if closed {
		return nil, fmt.Errorf("the sessions with the configured mcp servers are closed")
	}
	if !known {
		return nil, fmt.Errorf("no mcp server named %q is configured", name)
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	// Reading the closed flag again under the entry lock catches a Close that
	// landed since the read above: Close sets the flag before it takes this lock,
	// so a Close that has begun is seen here, and one that has not begun cannot
	// clear the session until this call releases the lock. The done channel does
	// not catch it alone, because Close clears the session while the goroutine
	// that closes the channel is still waiting for the session to end.
	s.mu.Lock()
	closed = s.closed
	s.mu.Unlock()
	if closed {
		return nil, fmt.Errorf("the sessions with the configured mcp servers are closed")
	}

	select {
	case <-e.done:
	default:
		if e.session != nil {
			return e.session, nil
		}
	}

	err := s.open(ctx, e)
	if err != nil {
		return nil, err
	}

	return e.session, nil
}

// open connects a session for e and records it. The caller holds e's lock, except
// in Connect where e is not yet reachable.
//
// The context it connects under carries the entry's startup timeout, and is
// canceled as soon as the handshake is done: the SDK detaches the session from
// it, so the deadline limits the connect and not the life of the session. The
// import applies the same timeout again when it lists the server's tools.
func (s *Sessions) open(ctx context.Context, e *entry) error {
	ctx, cancel := context.WithTimeout(ctx, e.server.StartupTimeout())
	defer cancel()

	transport, err := s.transport(ctx, e.server)
	if err != nil {
		return err
	}

	session, err := s.client().Connect(ctx, transport, nil)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("connecting to mcp server %q: it did not answer the initialize handshake within %v: %w", e.server.Name, e.server.StartupTimeout(), err)
		}

		return fmt.Errorf("connecting to mcp server %q: %w", e.server.Name, err)
	}

	done := make(chan struct{})
	go func() {
		_ = session.Wait()
		close(done)
	}()

	e.session = session
	e.done = done

	return nil
}

// client builds the MCP client for one server. Each server gets its own, rather
// than sharing one across all of them, because the handlers a client answers with
// (elicitation, the tool-list notification) are fixed on mcp.ClientOptions before
// Connect, so one shared client would make every such choice global to the
// process instead of per server.
//
// The client advertises nothing. Passing an empty mcp.ClientCapabilities drops
// the roots capability the SDK advertises by default, along with its listChanged:
// what goes on the wire is the empty object, on both the stateless server/discover
// path and the legacy initialize handshake. Reading mcp.ClientCapabilities alone
// suggests otherwise, since its deprecated Roots field is a non-pointer struct
// that encoding/json will not omit, but that field never reaches the wire: the
// SDK marshals capabilities through an unexported wrapper whose own roots field,
// a nil pointer at the shallower depth, shadows it (see #607 in the SDK). Both
// paths are pinned by a test that reads the bytes the client writes, so a change
// in the SDK is caught rather than assumed.
//
// No sampling handler is set, so a foreign server cannot spend this agent's model
// budget, and no elicitation handler, so nothing is advertised that no one here
// answers yet.
func (s *Sessions) client() *mcp.Client {
	return mcp.NewClient(&mcp.Implementation{Name: s.opts.Identity, Version: s.opts.Version}, &mcp.ClientOptions{
		Capabilities: &mcp.ClientCapabilities{},
	})
}
