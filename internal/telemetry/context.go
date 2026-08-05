//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package telemetry

import "context"

// providerKey carries the Provider down to a subsystem that has a ctx but no
// injection point of its own.
type providerKey struct{}

// ContextWithProvider puts p in ctx so code further down can open spans without being
// handed a Provider through its constructor.
//
// The explicit injection point is still agent.Options.Telemetry; this is how it reaches
// packages the run does not construct per run. Two of them need it. internal/rag holds a
// Store that is deliberately shared across runs in a server (Options.RAGStore is
// documented as exactly that), so a Provider stored on the Store would be per store
// while traces are per run: run B's searches would emit through run A's provider, into
// run A's trace, possibly after run A's Shutdown. That is not a missing span, it is
// wrong data, and it is the same failure the plan already fixed once by reading
// gen_ai.provider.name off what ran rather than off what was configured. internal/a2a
// takes it the same way for consistency and because it keeps three constructor call
// sites unchanged.
//
// This is not global state: it is per call, so concurrent runs in one process each carry
// their own, which is the distinction from otel.SetTracerProvider that this package
// refuses to touch. The honest cost is that it is an implicit dependency, so a call path
// that fails to thread its ctx emits nothing rather than failing to compile.
func ContextWithProvider(ctx context.Context, p *Provider) context.Context {
	return context.WithValue(ctx, providerKey{}, p)
}

// ProviderFromContext returns the Provider ctx carries, or nil. A nil Provider is safe
// on every method, so a caller uses the result without checking it.
func ProviderFromContext(ctx context.Context) *Provider {
	p, _ := ctx.Value(providerKey{}).(*Provider)

	return p
}
