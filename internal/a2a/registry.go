//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/choria-io/fisk-ai/internal/conns"
)

// TransportConfig configures a transport at construction. Identity is this agent's
// own identity (it keys a served transport's paths); Timeout bounds a request that
// carries no deadline of its own; Options is the raw per-transport options block,
// decoded strictly by the transport (reject unknown keys) so an operator's mistyped
// option fails at construction rather than on the first call. Options is empty when
// unset.
type TransportConfig struct {
	Identity string
	Timeout  time.Duration
	Options  json.RawMessage

	// Logger receives what the binding notices about its own health. Nil discards it,
	// since a library that reached for a default logger would write to an embedder's
	// stderr uninvited.
	Logger *slog.Logger

	// OnFault reports that the transport has stopped serving for a reason nobody asked
	// for: a substrate that dropped the registration, a subscription that overflowed.
	// It is not called for a stop the program asked for by closing the transport.
	//
	// A served surface cannot recover from this on its own, so the callback exists to
	// let whoever hosts it decide: a worker whose identity is registered nowhere is
	// answering nothing while still running. It may be called more than once and is
	// called from the binding's own goroutine, so an implementation must not block.
	OnFault func(error)
}

// Factory constructs a Transport from the shared connection Provider and the
// per-transport configuration. It is registered under a transport name with
// RegisterTransport. The Provider is borrowed: the caller established it and closes
// it, so the Transport must never close it.
//
// An implementation must decode its Options block strictly (reject unknown keys)
// and surface a construction failure (bad options, a missing connection) as an
// error rather than deferring it to the first call.
type Factory func(p *conns.Provider, cfg TransportConfig) (Transport, error)

var (
	registryMu sync.Mutex
	registry   = map[string]Factory{}
)

// RegisterTransport adds a transport factory under name. It is meant to be called
// from a transport package's init, so a program links a transport in simply by
// importing its package. It panics on an empty name, a nil factory, or a duplicate
// registration: each is a programming error resolved at compile time, mirroring
// database/sql.Register. Do not call it outside init.
func RegisterTransport(name string, factory Factory) {
	if name == "" {
		panic("a2a: RegisterTransport called with an empty transport name")
	}
	if factory == nil {
		panic("a2a: RegisterTransport called with a nil factory for transport " + name)
	}

	registryMu.Lock()
	defer registryMu.Unlock()

	if _, dup := registry[name]; dup {
		panic("a2a: RegisterTransport called twice for transport " + name)
	}

	registry[name] = factory
}

// Transports returns the names of every transport linked into this build, sorted.
// A caller can show it so an operator sees which transports are available without
// triggering an unknown-transport error.
func Transports() []string {
	registryMu.Lock()
	defer registryMu.Unlock()

	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)

	return names
}

// NewTransport constructs the named transport over the shared Provider. It returns
// an error for an unknown transport (most often because its package was not
// imported into this build; the error lists the transports that are linked in) or
// a construction failure, so an operator's mistake surfaces up front rather than on
// the first call.
func NewTransport(name string, p *conns.Provider, cfg TransportConfig) (Transport, error) {
	registryMu.Lock()
	factory, ok := registry[name]
	registryMu.Unlock()

	if !ok {
		return nil, fmt.Errorf("unknown a2a transport %q: known transports are %v", name, Transports())
	}

	return factory(p, cfg)
}
