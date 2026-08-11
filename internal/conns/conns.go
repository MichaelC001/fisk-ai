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
	"fmt"

	"github.com/nats-io/jsm.go/natscontext"
	"github.com/nats-io/nats.go"
)

// Provider gives a backend access to the shared connections by kind. A backend
// uses only the kind it needs and treats a nil result as "this kind was not
// provisioned", failing loudly rather than dereferencing it. A connection the
// Provider established (Connect) is owned and released by Close; a connection
// handed in (WithNats) is borrowed and must never be closed through the Provider.
type Provider struct {
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

// Options is the option set every connection this program makes is built from.
// connName identifies the connection to the server as "fisk-ai <connName>", and
// extra is appended last so a caller can add what its own binding requires.
//
// It exists so the connections are alike. Anything that should be true of all of
// them (reconnection policy today, connection event logging next) is added here
// once rather than at each dial, and a caller that dials its own connection for a
// binding with a special requirement still inherits the rest.
func Options(connName string, extra ...nats.Option) []nats.Option {
	opts := []nats.Option{
		nats.Name(fmt.Sprintf("fisk-ai %s", connName)),
		nats.MaxReconnects(reconnectAttempts),
	}

	return append(opts, extra...)
}

// Connect establishes the shared core NATS connection from the named context and
// returns a Provider that owns it. Any extra options are appended to the standard
// set in Options. The Provider owns the connection, so the caller must Close it
// when done.
func Connect(contextName, connName string, extra ...nats.Option) (*Provider, error) {
	nc, err := natscontext.Connect(contextName, Options(connName, extra...)...)
	if err != nil {
		return nil, fmt.Errorf("connecting to NATS context %q: %w", contextName, err)
	}

	return &Provider{nats: nc, ownNats: true}, nil
}

// Nats returns the shared core NATS connection, or nil when no NATS connection
// was provisioned. The connection is shared: callers must never close it
// directly; release it through the owning Provider's Close.
func (p *Provider) Nats() *nats.Conn {
	if p == nil {
		return nil
	}

	return p.nats
}

// Close releases the connections the Provider established and owns, leaving any
// borrowed connection (provisioned with WithNats) open. It is safe to call more
// than once and on a nil Provider.
func (p *Provider) Close() {
	if p == nil {
		return
	}

	if p.ownNats && p.nats != nil {
		p.nats.Close()
		p.nats = nil
	}
}
