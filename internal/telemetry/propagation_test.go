//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// tracing returns a provider recording into memory, and the exporter holding what it
// recorded.
func tracing(sampler sdktrace.Sampler) *Provider {
	GinkgoHelper()

	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sampler))
	DeferCleanup(func() { _ = tp.Shutdown(context.Background()) })

	return &Provider{tracer: tp.Tracer(scopeName), delivery: &delivery{}}
}

var _ = Describe("Trace context propagation", func() {
	It("Should put a receiver's span in the sender's trace, under the sending span", func() {
		p := tracing(sdktrace.AlwaysSample())

		sendCtx, span := p.tracer.Start(context.Background(), "send")
		sent := TraceContextFrom(sendCtx)
		Expect(sent.Empty()).To(BeFalse())

		// A fresh context, as a receiving process has: nothing of the sender's is on it
		// but the two strings that crossed the wire.
		recvCtx := ContextWithRemoteTrace(context.Background(), sent)
		_, child := p.tracer.Start(recvCtx, "receive")

		Expect(child.SpanContext().TraceID()).To(Equal(span.SpanContext().TraceID()))
		Expect(child.SpanContext().SpanID()).ToNot(Equal(span.SpanContext().SpanID()))

		// The receiver's parent is the span that sent, which is what makes one trace
		// rather than two sharing an id.
		readOnly, ok := child.(interface{ Parent() trace.SpanContext })
		Expect(ok).To(BeTrue())
		Expect(readOnly.Parent().SpanID()).To(Equal(span.SpanContext().SpanID()))
	})

	It("Should render nothing when the caller is not tracing", func() {
		Expect(TraceContextFrom(context.Background()).Empty()).To(BeTrue())

		// A provider that records still renders nothing without a span to describe.
		p := tracing(sdktrace.AlwaysSample())
		Expect(TraceContextFrom(context.Background()).Empty()).To(BeTrue())
		_ = p
	})

	It("Should leave the context alone for anything it cannot parse", func() {
		p := tracing(sdktrace.AlwaysSample())

		for _, bad := range []string{
			"",
			"nonsense",
			"00-abc-def-01",
			"00-00000000000000000000000000000000-0000000000000000-01",
			"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7",
			"zz-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		} {
			ctx := ContextWithRemoteTrace(context.Background(), TraceContext{TraceParent: bad})
			_, span := p.tracer.Start(ctx, "receive")

			Expect(trace.SpanContextFromContext(ctx).IsValid()).To(BeFalse(), bad)
			Expect(span.SpanContext().IsValid()).To(BeTrue(), bad)

			// A root, so a peer that stamped rubbish starts a trace here rather than
			// failing or joining one that does not exist.
			readOnly, ok := span.(interface{ Parent() trace.SpanContext })
			Expect(ok).To(BeTrue())
			Expect(readOnly.Parent().IsValid()).To(BeFalse(), bad)
		}
	})

	// The sender's flag decides for the receiver under ParentBased's defaults, and the
	// sender is a peer nothing authenticated.
	It("Should not let a peer that is not tracing switch this process's recording off", func() {
		notSampled := TraceContext{TraceParent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00"}

		p := tracing(sampler(1))
		_, span := p.tracer.Start(ContextWithRemoteTrace(context.Background(), notSampled), "served")
		Expect(span.IsRecording()).To(BeTrue())

		// The default this overrides, shown so the spec says what it is protecting.
		def := tracing(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(1)))
		_, defSpan := def.tracer.Start(ContextWithRemoteTrace(context.Background(), notSampled), "served")
		Expect(defSpan.IsRecording()).To(BeFalse())
	})

	It("Should follow a peer that is tracing, so a delegation is one trace", func() {
		sampled := TraceContext{TraceParent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}

		// The local ratio is zero, so nothing this process roots is recorded, and the
		// peer's sampled call still is.
		p := tracing(sampler(0))

		_, root := p.tracer.Start(context.Background(), "local")
		Expect(root.IsRecording()).To(BeFalse())

		_, served := p.tracer.Start(ContextWithRemoteTrace(context.Background(), sampled), "served")
		Expect(served.IsRecording()).To(BeTrue())
	})
})
