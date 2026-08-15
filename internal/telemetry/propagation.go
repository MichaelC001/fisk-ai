//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/propagation"
)

// traceHeader is the W3C key the trace context is carried under. It is stated here
// rather than taken from the propagator, which exposes it only through Fields.
const traceHeader = "traceparent"

// TraceContext is the W3C trace context of a span, for carrying across a process
// boundary in whatever a caller's protocol already has room for.
//
// It carries the ids alone. tracestate is deliberately absent: nothing in this
// repository produces one, and a receiver that adopted one would re-emit a peer's
// key-values on every call it made afterwards, which is what Baggage is refused for.
type TraceContext struct {
	// TraceParent is the traceparent field, empty when the sender had no span.
	TraceParent string
}

// Empty reports whether there is any trace context to carry.
func (t TraceContext) Empty() bool { return t.TraceParent == "" }

// TraceContextFrom renders the span ctx carries so it can be sent to another
// process. The result is empty when ctx carries no recording span, which is what a
// sender stamps on a message when telemetry is off.
func TraceContextFrom(ctx context.Context) TraceContext {
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)

	return TraceContext{TraceParent: carrier.Get(traceHeader)}
}

// ContextWithRemoteTrace returns ctx with tc as the parent of the next span opened on
// it. An empty or malformed tc leaves ctx unchanged, so a sender that stamped nothing
// starts a trace here rather than failing.
//
// tc arrives from a sender nothing authenticated, so the trace id it names is the
// sender's choice and nothing here may read it as evidence of who called. Its sampled
// flag is not allowed to suppress this process's own recording; see Setup.
func ContextWithRemoteTrace(ctx context.Context, tc TraceContext) context.Context {
	if tc.Empty() {
		return ctx
	}

	return propagation.TraceContext{}.Extract(ctx, propagation.MapCarrier{traceHeader: tc.TraceParent})
}
