//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	wire "github.com/choria-io/fisk-ai/internal/a2a/wire/v1"
	"github.com/choria-io/fisk-ai/internal/telemetry"
)

// stubTransport answers a tool request with a canned reply, or fails the round trip.
// The client reaches Stream by asserting for ReplySetTransport, so the assertion is
// what keeps a missing method a build failure rather than a silently round-tripped call.
var _ ReplySetTransport = (*stubTransport)(nil)

type stubTransport struct {
	output  string
	isError bool
	err     error
}

func (t *stubTransport) Close() error                                   { return nil }
func (t *stubTransport) Serve(RouteHint, Handler) error                 { return nil }
func (t *stubTransport) ServeReplySet(RouteHint, ReplySetHandler) error { return nil }
func (t *stubTransport) Describe(string) []DescLine                     { return nil }

func (t *stubTransport) RoundTrip(_ context.Context, _ string, _ RouteHint, body []byte) ([]byte, error) {
	if t.err != nil {
		return nil, t.err
	}

	var req wire.ToolRequest
	err := json.Unmarshal(body, &req)
	if err != nil {
		return nil, err
	}

	reply := wire.NewToolReply(t.output, t.isError)
	wire.StampReply(&reply.Header, &req.Header, "peer")

	return json.Marshal(reply)
}

// Stream answers a tool call as a peer does, with an ack and then the reply. A failing
// stub fails when the set is opened, which is where a transport reports that it could
// not reach the peer at all.
func (t *stubTransport) Stream(_ context.Context, _ string, _ RouteHint, body []byte) (Reader, error) {
	if t.err != nil {
		return nil, t.err
	}

	var req wire.ToolRequest
	err := json.Unmarshal(body, &req)
	if err != nil {
		return nil, err
	}

	ack := wire.NewAck(true)
	wire.StampReply(&ack.Header, &req.Header, "peer")
	ack.Sequence = 1

	reply := wire.NewToolReply(t.output, t.isError)
	wire.StampReply(&reply.Header, &req.Header, "peer")
	reply.Sequence = 2

	set := make([][]byte, 0, 2)
	for _, msg := range []any{ack, reply} {
		data, err := json.Marshal(msg)
		if err != nil {
			return nil, err
		}
		set = append(set, data)
	}

	return &stubReader{msgs: set}, nil
}

// stubReader yields a prepared reply set, and reports the caller's context when it is
// done rather than when it is exhausted: a canceled call must surface as the cancel.
type stubReader struct {
	msgs [][]byte
	next int
}

func (r *stubReader) Next(ctx context.Context) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if r.next >= len(r.msgs) {
		return nil, io.EOF
	}

	msg := r.msgs[r.next]
	r.next++

	return msg, nil
}

func (r *stubReader) Close() error { return nil }

// recordingTelemetry returns a Provider writing every ended span into the exporter.
func recordingTelemetry() (*telemetry.Provider, *tracetest.InMemoryExporter) {
	exp := tracetest.NewInMemoryExporter()

	return telemetry.NewFromProviders(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp)), nil), exp
}

// attrOf returns one attribute from a recorded span.
func attrOf(stub tracetest.SpanStub, key string) (string, bool) {
	for _, kv := range stub.Attributes {
		if string(kv.Key) == key {
			return kv.Value.String(), true
		}
	}

	return "", false
}

var _ = Describe("Client telemetry", func() {
	var ctx context.Context

	invoke := func(t *stubTransport) (*telemetry.Provider, *tracetest.InMemoryExporter, *wire.ToolReply, error) {
		GinkgoHelper()

		tel, exp := recordingTelemetry()
		c, err := NewClient(t, "local")
		Expect(err).ToNot(HaveOccurred())

		reply, err := c.InvokeTool(telemetry.ContextWithProvider(ctx, tel), "peer", "stream_info", json.RawMessage(`{"stream":"ORDERS"}`))

		return tel, exp, reply, err
	}

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("Should record the hop as a client span naming the peer", func() {
		_, exp, reply, err := invoke(&stubTransport{output: "all good"})
		Expect(err).ToNot(HaveOccurred())
		Expect(reply.Output).To(Equal("all good"))

		spans := exp.GetSpans()
		Expect(spans).To(HaveLen(1))

		span := spans[0]
		Expect(span.Name).To(Equal("invoke_agent peer"))
		Expect(span.SpanKind).To(Equal(trace.SpanKindClient))

		// The peer's tool name, which is peer-supplied and so stays a span attribute.
		tool, ok := attrOf(span, "gen_ai.tool.name")
		Expect(ok).To(BeTrue())
		Expect(tool).To(Equal("stream_info"))

		agent, ok := attrOf(span, "fisk.tool.remote_agent")
		Expect(ok).To(BeTrue())
		Expect(agent).To(Equal("peer"))

		// gen_ai.agent.name means THIS agent everywhere else in a trace. Reusing it for
		// the peer would make a backend filter on that key return other agents' spans.
		_, ok = attrOf(span, "gen_ai.agent.name")
		Expect(ok).To(BeFalse())

		Expect(span.Status.Code).ToNot(Equal(codes.Error))
	})

	// The failure that only an in-band check can see. A tool that failed on the far side
	// answers with IsError set and a nil Go error, so a span keyed on the error alone
	// renders a green remote call underneath the red execute_tool span that wraps it.
	It("Should mark a remote tool failure that returns no Go error", func() {
		_, exp, reply, err := invoke(&stubTransport{output: "no such stream", isError: true})
		Expect(err).ToNot(HaveOccurred())
		Expect(reply.IsError).To(BeTrue())

		spans := exp.GetSpans()
		Expect(spans).To(HaveLen(1))

		class, ok := attrOf(spans[0], "error.type")
		Expect(ok).To(BeTrue())
		Expect(class).To(Equal(telemetry.ClassToolError.String()))
		Expect(spans[0].Status.Code).To(Equal(codes.Error))
	})

	It("Should classify an unreachable peer", func() {
		_, exp, _, err := invoke(&stubTransport{err: fmt.Errorf("%w: no responders", ErrAgentUnavailable)})
		Expect(err).To(MatchError(ErrAgentUnavailable))

		spans := exp.GetSpans()
		Expect(spans).To(HaveLen(1))

		class, ok := attrOf(spans[0], "error.type")
		Expect(ok).To(BeTrue())
		Expect(class).To(Equal(telemetry.ClassRemoteUnavailable.String()))
	})

	It("Should classify a canceled call from the closed vocabulary", func() {
		_, exp, _, err := invoke(&stubTransport{err: context.Canceled})
		Expect(err).To(MatchError(context.Canceled))

		spans := exp.GetSpans()
		Expect(spans).To(HaveLen(1))

		class, ok := attrOf(spans[0], "error.type")
		Expect(ok).To(BeTrue())
		Expect(class).To(Equal(telemetry.ClassCanceled.String()))
	})

	// The error text names hosts, subjects and reply fragments, so none of it travels.
	It("Should export no error text for a failed hop", func() {
		_, exp, _, err := invoke(&stubTransport{err: errors.New("dial tcp 10.0.0.5:4222: connection refused")})
		Expect(err).To(HaveOccurred())

		spans := exp.GetSpans()
		Expect(spans).To(HaveLen(1))
		Expect(spans[0].Status.Description).To(BeEmpty())

		for _, kv := range spans[0].Attributes {
			Expect(kv.Value.String()).ToNot(ContainSubstring("10.0.0.5"))
		}
		Expect(spans[0].Events).To(BeEmpty())
	})

	// The path almost every run takes: telemetry off, so nothing is in the context.
	It("Should invoke cleanly with no provider in the context", func() {
		c, err := NewClient(&stubTransport{output: "ok"}, "local")
		Expect(err).ToNot(HaveOccurred())

		reply, err := c.InvokeTool(ctx, "peer", "stream_info", nil)
		Expect(err).ToNot(HaveOccurred())
		Expect(reply.Output).To(Equal("ok"))
	})
})
