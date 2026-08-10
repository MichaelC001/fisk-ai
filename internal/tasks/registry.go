//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package tasks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/nats-io/nats.go"
)

// Factory constructs a Store for a backend from the raw per-backend options block
// (verbatim, empty when unset). It is registered under a backend name with Register.
//
// No identity is passed. Task records are not namespaced by identity, following
// runstate rather than memory: a task submitted through one surface must be findable
// by whichever worker takes it, whatever identity that worker is running as, and a
// namespace would make that depend on who happened to pick the work up.
//
// An implementation must:
//   - be safe for concurrent use by independent processes sharing a backing store, as
//     the Store contract requires;
//   - validate every id with ValidateID before it is used as a key or path component,
//     so the format cannot drift between backends and a traversal cannot be smuggled
//     through an id;
//   - take a new record's id from the request with RequestID rather than minting one;
//   - return ErrExists from Submit when the id is present, ErrNotFound from Load and
//     Complete when it is absent, and ErrCompleted from Complete when an answer is
//     already stored;
//   - never let a reader observe a partially written record;
//   - decode its options block with DecodeOptions so an operator's mistyped option
//     fails at construction, and surface a construction failure as an error rather
//     than deferring it to the first operation;
//   - report its registered name as Info().Backend, and as Info().Location either an
//     operator-configured container identifier or "".
type Factory func(env RuntimeEnv, options json.RawMessage) (Store, error)

// RegisterOption declares what a backend needs from the host beyond its options, so
// the host can resolve it before construction without naming the backend. A backend
// that keeps its data locally needs none.
type RegisterOption func(*registration)

// RequiresNats declares that a backend needs a NATS connection on RuntimeEnv.Nats.
// The host provisions one before building the store (see NeedsNats), so a
// connection-backed backend is named nowhere outside its own registration.
func RequiresNats() RegisterOption {
	return func(r *registration) { r.needsNats = true }
}

// registration is a backend's factory plus the requirements it declared.
type registration struct {
	factory   Factory
	needsNats bool
}

// RuntimeEnv carries the environment a backend may need beyond its own options,
// mirroring the equivalents in runstate and memory. A backend uses what applies to it
// and ignores the rest.
type RuntimeEnv struct {
	// StoreDir is the store base. When set, the file backend roots records under
	// StoreDir/tasks so they sit alongside a deployment's other state. Empty keeps the
	// backend's own default. A relative configured directory is rebased under it; an
	// absolute one is honored verbatim.
	StoreDir string

	// Nats is the shared connection, set only for a backend that declared
	// RequiresNats.
	Nats *nats.Conn
}

var (
	registryMu sync.RWMutex
	registry   = map[string]*registration{}
)

// Register adds a backend under name. It panics on a duplicate name or a nil
// factory, both of which are programming errors in an init function.
func Register(name string, factory Factory, opts ...RegisterOption) {
	registryMu.Lock()
	defer registryMu.Unlock()

	if factory == nil {
		panic(fmt.Sprintf("tasks: nil factory for backend %q", name))
	}
	if _, ok := registry[name]; ok {
		panic(fmt.Sprintf("tasks: backend %q is already registered", name))
	}

	reg := &registration{factory: factory}
	for _, opt := range opts {
		opt(reg)
	}

	registry[name] = reg
}

// Backends lists the registered backend names, sorted, for diagnostics and for an
// error naming what was available.
func Backends() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()

	out := make([]string, 0, len(registry))
	for name := range registry {
		out = append(out, name)
	}
	sort.Strings(out)

	return out
}

// NeedsNats reports whether the named backend declared RequiresNats, so a host can
// provision a connection before New without knowing which backend that is.
func NeedsNats(name string) bool {
	registryMu.RLock()
	defer registryMu.RUnlock()

	reg, ok := registry[name]

	return ok && reg.needsNats
}

// New builds the named backend from its options block. It returns ErrUnknownBackend
// naming what is registered when the name is not one of them.
func New(name string, env RuntimeEnv, options json.RawMessage) (Store, error) {
	registryMu.RLock()
	reg, ok := registry[name]
	registryMu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("%w: %q (have %v)", ErrUnknownBackend, name, Backends())
	}

	return reg.factory(env, options)
}

// DecodeOptions strictly decodes a backend's raw options block into T, rejecting an
// unknown key so an operator's mistyped option fails at construction rather than at
// the first write. An empty block decodes to the zero value. what names the backend
// for the error message. Every backend decodes through this so the strict rule the
// Factory contract requires cannot drift between them.
func DecodeOptions[T any](raw json.RawMessage, what string) (T, error) {
	var opts T
	if len(raw) == 0 {
		return opts, nil
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()

	err := dec.Decode(&opts)
	if err != nil {
		return opts, fmt.Errorf("invalid %s options: %w", what, err)
	}

	return opts, nil
}
