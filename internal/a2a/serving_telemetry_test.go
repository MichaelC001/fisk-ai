//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	"context"
	"encoding/json"
	"io"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	wire "github.com/choria-io/fisk-ai/internal/a2a/wire/v1"
	"github.com/choria-io/fisk-ai/internal/telemetry"
)

// servedSpan returns the one served-call span the exporter holds.
func servedSpan(exp *tracetest.InMemoryExporter) tracetest.SpanStub {
	GinkgoHelper()

	var served []tracetest.SpanStub
	for _, s := range exp.GetSpans() {
		if s.SpanKind == trace.SpanKindServer {
			served = append(served, s)
		}
	}
	Expect(served).To(HaveLen(1))

	return served[0]
}

// toolRequestFrom builds a tool request body stamped from ctx, so it carries whatever
// trace context that has.
func toolRequestFrom(ctx context.Context, name string) []byte {
	GinkgoHelper()

	req := wire.NewToolRequest(name, nil)
	StampRequest(ctx, &req.Header, "caller", "svc")

	data, err := json.Marshal(req)
	Expect(err).NotTo(HaveOccurred())

	return data
}

var _ = Describe("Serving telemetry", func() {
	It("Should stamp the sending span's trace context on a request, and nothing when untraced", func() {
		tel, _ := recordingTelemetry()

		ctx, span := tel.StartRemoteAgent(context.Background(), telemetry.RemoteAgentInfo{Agent: "svc"})
		defer span.Finish(telemetry.RemoteAgentOutcome{})

		var traced wire.ToolRequest
		Expect(json.Unmarshal(toolRequestFrom(ctx, "ping"), &traced)).To(Succeed())
		Expect(traced.TraceParent).ToNot(BeEmpty())
		Expect(traced.TraceParent).To(ContainSubstring(trace.SpanContextFromContext(ctx).TraceID().String()))

		var untraced wire.ToolRequest
		Expect(json.Unmarshal(toolRequestFrom(context.Background(), "ping"), &untraced)).To(Succeed())
		Expect(untraced.TraceParent).To(BeEmpty())
	})

	It("Should open a served span in the caller's trace", func() {
		tel, exp := recordingTelemetry()

		ft := newFakeTransport()
		_, err := NewServer(ft, servingApp("ping", "echo pong\n"),
			ServerOptions{Identity: "svc", LogOutput: io.Discard, Telemetry: tel})
		Expect(err).NotTo(HaveOccurred())

		callerCtx, callerSpan := tel.StartRemoteAgent(context.Background(), telemetry.RemoteAgentInfo{Agent: "svc"})
		wantTrace := trace.SpanContextFromContext(callerCtx).TraceID()

		rep := &fakeReplier{}
		ft.replySets[OpTool](context.Background(), Caller{}, toolRequestFrom(callerCtx, "ping"), rep)
		Eventually(rep.responded.Load).Should(BeTrue())
		callerSpan.Finish(telemetry.RemoteAgentOutcome{})

		Eventually(func() int { return len(exp.GetSpans()) }).Should(BeNumerically(">=", 2))

		got := servedSpan(exp)
		Expect(got.SpanContext.TraceID()).To(Equal(wantTrace))
		Expect(got.Name).To(Equal("execute_tool ping"))

		outcome, ok := attrOf(got, "fisk.tool.outcome")
		Expect(ok).To(BeTrue())
		Expect(outcome).To(Equal(telemetry.ToolOutcomeExecuted.String()))
	})

	It("Should open a root when the caller sent no trace context", func() {
		tel, exp := recordingTelemetry()

		ft := newFakeTransport()
		_, err := NewServer(ft, servingApp("ping", "echo pong\n"),
			ServerOptions{Identity: "svc", LogOutput: io.Discard, Telemetry: tel})
		Expect(err).NotTo(HaveOccurred())

		rep := &fakeReplier{}
		ft.replySets[OpTool](context.Background(), Caller{}, toolRequestFrom(context.Background(), "ping"), rep)
		Eventually(rep.responded.Load).Should(BeTrue())

		Eventually(func() int { return len(exp.GetSpans()) }).Should(BeNumerically(">=", 1))
		Expect(servedSpan(exp).Parent.IsValid()).To(BeFalse())
	})

	// The peer chooses the trace id and nothing authenticates it, so an unparseable one
	// must not cost the call.
	It("Should serve a call whose trace context cannot be parsed", func() {
		tel, exp := recordingTelemetry()

		ft := newFakeTransport()
		_, err := NewServer(ft, servingApp("ping", "echo pong\n"),
			ServerOptions{Identity: "svc", LogOutput: io.Discard, Telemetry: tel})
		Expect(err).NotTo(HaveOccurred())

		req := wire.NewToolRequest("ping", nil)
		StampRequest(context.Background(), &req.Header, "caller", "svc")
		req.TraceParent = "nonsense"

		body, err := json.Marshal(req)
		Expect(err).NotTo(HaveOccurred())

		rep := &fakeReplier{}
		ft.replySets[OpTool](context.Background(), Caller{}, body, rep)
		Eventually(rep.responded.Load).Should(BeTrue())

		var reply wire.ToolReply
		Expect(json.Unmarshal(rep.body, &reply)).To(Succeed())
		Expect(reply.IsError).To(BeFalse())

		Eventually(func() int { return len(exp.GetSpans()) }).Should(BeNumerically(">=", 1))
		Expect(servedSpan(exp).Parent.IsValid()).To(BeFalse())
	})

	It("Should end the span of a call naming a tool that does not exist", func() {
		tel, exp := recordingTelemetry()

		ft := newFakeTransport()
		_, err := NewServer(ft, servingApp("ping", "echo pong\n"),
			ServerOptions{Identity: "svc", LogOutput: io.Discard, Telemetry: tel})
		Expect(err).NotTo(HaveOccurred())

		rep := &fakeReplier{}
		ft.replySets[OpTool](context.Background(), Caller{}, toolRequestFrom(context.Background(), "missing"), rep)
		Expect(rep.responded.Load()).To(BeTrue())

		got := servedSpan(exp)
		Expect(got.Name).To(Equal("execute_tool unknown_tool"))

		outcome, ok := attrOf(got, "fisk.tool.outcome")
		Expect(ok).To(BeTrue())
		Expect(outcome).To(Equal(telemetry.ToolOutcomeUnknownTool.String()))

		// The peer's string is an attribute and never the span name.
		requested, ok := attrOf(got, "fisk.tool.requested_name")
		Expect(ok).To(BeTrue())
		Expect(requested).To(Equal("missing"))
	})

	It("Should serve a call with telemetry off", func() {
		ft := newFakeTransport()
		_, err := NewServer(ft, servingApp("ping", "echo pong\n"),
			ServerOptions{Identity: "svc", LogOutput: io.Discard})
		Expect(err).NotTo(HaveOccurred())

		rep := &fakeReplier{}
		ft.replySets[OpTool](context.Background(), Caller{}, toolRequestFrom(context.Background(), "ping"), rep)
		Eventually(rep.responded.Load).Should(BeTrue())

		var reply wire.ToolReply
		Expect(json.Unmarshal(rep.body, &reply)).To(Succeed())
		Expect(reply.IsError).To(BeFalse())
	})
})
