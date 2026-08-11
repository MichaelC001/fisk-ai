//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"sync"
)

// Scope is one run's record of which memories it may overwrite.
//
// A backend that gates an overwrite on the model having read the current value
// (read-before-update) has to remember what this run read, and that state belongs to
// the run rather than to the store. A store built per run can keep it in the store and
// mean the same thing; a store shared by a host running many agents cannot, because one
// run's read would then authorize another run's blind overwrite of the same key. The
// scope travels on the context so both arrangements say "read in this run".
//
// A backend with no such gate ignores it. Every method is safe for concurrent use, and
// a nil *Scope is a working scope that grants nothing, so a backend need not branch on
// whether a host provided one.
type Scope struct {
	mu   sync.Mutex
	revs map[string]uint64
}

// NewScope returns an empty scope. A host constructs one per run and puts it on the
// run's context with WithScope.
func NewScope() *Scope {
	return &Scope{revs: map[string]uint64{}}
}

// Remember records that key is known to be at rev, granting authority to overwrite it
// for the life of this scope.
func (s *Scope) Remember(key string, rev uint64) {
	if s == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.revs == nil {
		s.revs = map[string]uint64{}
	}
	s.revs[key] = rev
}

// Forget drops the authority to overwrite key, so the next overwrite must read it
// again. A backend calls it when it learns the revision it held is stale.
func (s *Scope) Forget(key string) {
	if s == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.revs, key)
}

// Revision returns the revision key is known to be at in this scope, and whether it is
// known at all. A key that is not known has not been read and may not be overwritten.
func (s *Scope) Revision(key string) (uint64, bool) {
	if s == nil {
		return 0, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	rev, ok := s.revs[key]

	return rev, ok
}

type scopeKey struct{}

// WithScope returns a context carrying scope, which the backends serving that context
// use in place of any state of their own. A host that shares one store across runs must
// call it once per run; a host that builds a store per run need not, since the store's
// own scope already means the same thing.
func WithScope(ctx context.Context, scope *Scope) context.Context {
	return context.WithValue(ctx, scopeKey{}, scope)
}

// ScopeFrom returns the scope on ctx, or nil when it carries none. A backend that finds
// none falls back to whatever it holds itself rather than refusing to work, so a caller
// that never heard of scopes is unaffected.
func ScopeFrom(ctx context.Context) *Scope {
	scope, _ := ctx.Value(scopeKey{}).(*Scope)

	return scope
}
