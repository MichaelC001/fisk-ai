//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// Package conns is the single home for connection establishment and access. A
// connection is established once and handed to every backend through a Provider,
// so backends do not each dial their own. Today the a2a client and server consume
// it; the memory and session stores will follow the same way. A Choria connection
// manager already exposes Nats(), so it can back a Provider without adaptation.
//
// A binding whose connection must differ (the work queue engine requires
// nats.UseOldRequestStyle) dials its own rather than bending the shared one, but
// still builds it from Options so it differs only in what it had to.
package conns

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/nats-io/jsm.go/natscontext"
	"github.com/nats-io/nats.go"
)

// ErrClosed is returned when a connection was closed before it came up. Its cause
// is the connection's last error, which is what a permanent failure such as a bad
// credential arrives as.
var ErrClosed = errors.New("connection closed before it came up")

// Provider gives a backend access to the shared connections by kind. A backend
// uses only the kind it needs and treats a nil result as "this kind was not
// provisioned", failing loudly rather than dereferencing it. A connection the
// Provider established (Connect, ConnectNatsContext) is owned and released by
// Close, as is one handed in with WithOwnedNats; a connection handed in with
// WithNats is borrowed and must never be closed through the Provider.
//
// A Provider is safe for concurrent use, which is the point of it: one is shared
// across every goroutine a process runs, and Close races the handlers still using
// it. Nats and Close are each safe against the other, so a shutdown during an
// in-flight handler hands that handler either the connection or nil rather than a
// torn read. What it cannot do is make a connection valid after Close: a handler
// holding one from before must expect it to be closed under it, the same as any
// shared connection.
//
// The Option functions are not safe against them. They run inside New, before the
// Provider is reachable from another goroutine, which is the only place they run.
type Provider struct {
	mu      sync.Mutex
	nats    *nats.Conn
	ownNats bool
}

// Option provisions a connection kind on a Provider. WithNats is the only kind
// today; a Choria kind will be added the same way, so a backend that needs it
// can ask for it without changing the backends that only need NATS.
type Option func(*Provider)

// WithNats provisions a borrowed core NATS connection: the caller retains
// ownership and the Provider's Close leaves it open.
func WithNats(nc *nats.Conn) Option {
	return func(p *Provider) {
		p.nats = nc
		p.ownNats = false
	}
}

// WithOwnedNats provisions a core NATS connection the Provider owns: Close releases
// it, as it does one the Provider dialed itself.
//
// It exists for a caller that dialed its own connection because it needed an option
// this package does not offer, and wants the Provider to own it from there. Without
// it such a caller tracks the lifetime separately, which is the bookkeeping a
// Provider exists to remove.
func WithOwnedNats(nc *nats.Conn) Option {
	return func(p *Provider) {
		p.nats = nc
		p.ownNats = true
	}
}

// New builds a Provider from the given options.
func New(opts ...Option) *Provider {
	p := &Provider{}
	for _, opt := range opts {
		opt(p)
	}

	return p
}

// reconnectAttempts is how many times a connection retries before it gives up.
// Unlimited, because the processes that hold one are long lived and a broker
// restart must not end them: a worker that stops reconnecting stops taking work
// with nothing to say about why.
const reconnectAttempts = -1

// Config names a connection and carries whatever else the caller's binding needs.
type Config struct {
	// Product is the software making the connection, which for this CLI is
	// "fisk-ai". It is the first half of the name an operator reads in
	// nats server report connections, so an embedder puts its own name here rather
	// than announcing its connections as ours.
	Product string
	// Name distinguishes this connection from the product's others: the agent
	// identity, a job worker, a session store reader.
	Name string
	// Options are appended after the standard set, so a caller adds what its own
	// binding requires and overrides a standard option by repeating it.
	Options []nats.Option
}

// Options is the option set every connection this package makes is built from. The
// connection announces itself to the server as "<Product> <Name>".
//
// It exists so the connections are alike. Anything that should be true of all of
// them (reconnection policy today, connection event logging next) is added here
// once rather than at each dial, and a caller that dials its own connection for a
// binding with a special requirement still inherits the rest.
func Options(cfg Config) []nats.Option {
	opts := []nats.Option{
		nats.Name(strings.TrimSpace(cfg.Product + " " + cfg.Name)),
		nats.MaxReconnects(reconnectAttempts),
	}

	return append(opts, cfg.Options...)
}

// Connect establishes the shared core NATS connection to the given servers, a
// comma separated list in the form nats.Connect takes, and returns a Provider that
// owns it. The caller must Close the Provider when done.
//
// It is the constructor for a caller that holds connection details. Credentials and
// TLS travel as nats.Options in cfg, so every authentication mode nats.go supports
// reaches it without this package naming any of them.
func Connect(ctx context.Context, servers string, cfg Config) (*Provider, error) {
	return connect(ctx, cfg, fmt.Sprintf("NATS servers %q", servers), func(opts []nats.Option) (*nats.Conn, error) {
		return nats.Connect(servers, opts...)
	})
}

// ConnectNatsContext establishes the shared core NATS connection from a named nats
// CLI context and returns a Provider that owns it. The caller must Close the
// Provider when done.
//
// It reads the CLI's context files from the user's home directory, which is a
// convenience for people who already keep their brokers there and is why it is
// separate from Connect rather than the only way in.
func ConnectNatsContext(ctx context.Context, natsContext string, cfg Config) (*Provider, error) {
	return connect(ctx, cfg, fmt.Sprintf("NATS context %q", natsContext), func(opts []nats.Option) (*nats.Conn, error) {
		return natscontext.Connect(natsContext, opts...)
	})
}

// connect is the body both constructors share: the same options, the same wait for
// the connection to come up, and the same error text bar the target it names.
func connect(ctx context.Context, cfg Config, target string, dial func([]nats.Option) (*nats.Conn, error)) (*Provider, error) {
	err := ctx.Err()
	if err != nil {
		return nil, err
	}

	// RetryOnFailedConnect is what makes the context mean anything: without it a
	// broker that is down fails the dial outright and there is nothing for a caller
	// to cancel. It is appended after the caller's options so it cannot be turned
	// off, since waitConnected is written against it.
	nc, err := dial(append(Options(cfg), nats.RetryOnFailedConnect(true)))
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", target, err)
	}

	err = waitConnected(ctx, nc)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", target, err)
	}

	return &Provider{nats: nc, ownNats: true}, nil
}

// waitConnected waits for a connection dialed with RetryOnFailedConnect to come up,
// closing it and returning the context's error when the caller gives up first.
//
// A cancel is answered within one nats.Timeout, 2 seconds by default, rather than at
// once: nats makes the first dial attempt synchronously before this is reached, and
// Close waits out an attempt already in flight.
//
// A permanent failure arrives as CLOSED rather than as a retry that never ends. The
// server closes the connection on a bad credential, and the connection's last error
// is what says so, so it becomes the cause of ErrClosed rather than being lost.
func waitConnected(ctx context.Context, nc *nats.Conn) error {
	// Registered before the status is read, since Conn.StatusChanged does not replay
	// the current status and a connection that comes up in between would be missed.
	changed := nc.StatusChanged(nats.CONNECTED, nats.CLOSED)
	defer nc.RemoveStatusListener(changed)

	if nc.Status() == nats.CONNECTED {
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			nc.Close()
			return ctx.Err()

		case status := <-changed:
			switch status {
			case nats.CONNECTED:
				return nil
			case nats.CLOSED:
				return fmt.Errorf("%w: %w", ErrClosed, nc.LastError())
			}
		}
	}
}

// Nats returns the shared core NATS connection, or nil when no NATS connection
// was provisioned or the Provider has been closed. The connection is shared:
// callers must never close it directly; release it through the owning Provider's
// Close.
func (p *Provider) Nats() *nats.Conn {
	if p == nil {
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	return p.nats
}

// Close releases the connections the Provider established and owns, leaving any
// borrowed connection (provisioned with WithNats) open. It is safe to call more
// than once, on a nil Provider, and concurrently with Nats.
func (p *Provider) Close() {
	if p == nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.ownNats && p.nats != nil {
		p.nats.Close()
		p.nats = nil
	}
}
