//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

var _ = Describe("StartStartup", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("should name the span after the identity and record the remote host count", func() {
		p, exp := recording()

		_, span := p.StartStartup(ctx, StartupInfo{Identity: "demo", RemoteHosts: 2})
		span.End()

		spans := exp.GetSpans()
		Expect(spans).To(HaveLen(1))
		Expect(spans[0].Name).To(Equal("startup demo"))
		Expect(spans[0].SpanKind).To(Equal(trace.SpanKindInternal))

		v, ok := attrOf(spans[0], AttrRemoteHosts)
		Expect(ok).To(BeTrue())
		Expect(v.AsInt64()).To(Equal(int64(2)))
	})

	It("should carry the span in the returned context so setup work nests under it", func() {
		p, _ := recording()

		spanCtx, span := p.StartStartup(ctx, StartupInfo{Identity: "demo"})
		defer span.End()

		Expect(trace.SpanContextFromContext(spanCtx).IsValid()).To(BeTrue())
	})

	// The tool counts are not known until well into setup, which is why they are a
	// setter rather than a constructor argument: the span has to exist before then so
	// the failures on the way there are recorded at all.
	It("should record the tool inventory set after the span started", func() {
		p, exp := recording()

		_, span := p.StartStartup(ctx, StartupInfo{Identity: "demo"})
		span.SetTools(ToolCounts{Application: 5, Builtin: 3, Remote: 2, Custom: 1, Deferred: true})
		span.End()

		spans := exp.GetSpans()
		Expect(spans).To(HaveLen(1))

		for key, want := range map[attribute.Key]int64{
			AttrToolsApplication: 5,
			AttrToolsBuiltin:     3,
			AttrToolsRemote:      2,
			AttrToolsCustom:      1,
		} {
			v, ok := attrOf(spans[0], key)
			Expect(ok).To(BeTrue(), "expected %s to be set", key)
			Expect(v.AsInt64()).To(Equal(want), "%s", key)
		}

		v, ok := attrOf(spans[0], AttrToolsDeferred)
		Expect(ok).To(BeTrue())
		Expect(v.AsBool()).To(BeTrue())
	})

	It("should export nothing until the span ends", func() {
		p, exp := recording()

		_, span := p.StartStartup(ctx, StartupInfo{Identity: "demo"})
		Expect(exp.GetSpans()).To(BeEmpty())

		span.End()
		Expect(exp.GetSpans()).To(HaveLen(1))
	})
})

var _ = Describe("Span.Fail", func() {
	It("should record the class as error.type and mark the span failed", func() {
		p, exp := recording()

		_, span := p.StartStartup(context.Background(), StartupInfo{Identity: "demo"})
		span.Fail(errors.New("dial tcp: connection refused"), ClassStore)
		span.End()

		spans := exp.GetSpans()
		Expect(spans).To(HaveLen(1))
		Expect(spans[0].Status.Code).To(Equal(codes.Error))

		v, ok := attrOf(spans[0], "error.type")
		Expect(ok).To(BeTrue())
		Expect(v.AsString()).To(Equal(ClassStore.String()))
	})

	// This tree's errors embed absolute paths, config values and the config file path,
	// and a span status crosses a trust boundary to a backend where it cannot be
	// un-sent. Only the closed class vocabulary leaves the process.
	It("should never export the error text", func() {
		p, exp := recording()

		_, span := p.StartStartup(context.Background(), StartupInfo{Identity: "demo"})
		span.Fail(errors.New("/home/operator/secret/agent.yaml is unreadable"), ClassConfig)
		span.End()

		spans := exp.GetSpans()
		Expect(spans[0].Status.Description).To(BeEmpty())
		for _, kv := range spans[0].Attributes {
			Expect(kv.Value.String()).ToNot(ContainSubstring("/home/operator"))
		}
	})

	// span.RecordError would attach exception.stacktrace, which is exactly what the run
	// path works to keep off anything leaving the process. Nothing here may call it.
	It("should never record an exception event or a stack trace", func() {
		p, exp := recording()

		_, span := p.StartStartup(context.Background(), StartupInfo{Identity: "demo"})
		span.Fail(errors.New("boom"), ClassPanic)
		span.End()

		spans := exp.GetSpans()
		Expect(spans[0].Events).To(BeEmpty())

		_, ok := attrOf(spans[0], "exception.stacktrace")
		Expect(ok).To(BeFalse())
	})

	// A nil error being a no-op is what lets a deferred call site pass a named return
	// without guarding it, which is how the startup span covers its early returns.
	It("should do nothing for a nil error", func() {
		p, exp := recording()

		_, span := p.StartStartup(context.Background(), StartupInfo{Identity: "demo"})
		span.Fail(nil, ClassConfig)
		span.End()

		spans := exp.GetSpans()
		Expect(spans[0].Status.Code).To(Equal(codes.Unset))

		_, ok := attrOf(spans[0], "error.type")
		Expect(ok).To(BeFalse())
	})

	// A failure that named no class still has to be findable. An empty error.type is not
	// a value a backend can group by, so it is the one shape worse than an imprecise
	// class, and this is what makes the fallback deliberate rather than incidental.
	It("should export the catch-all rather than an empty class", func() {
		p, exp := recording()

		_, span := p.StartStartup(context.Background(), StartupInfo{Identity: "demo"})
		span.Fail(errors.New("boom"), ErrorClass{})
		span.End()

		spans := exp.GetSpans()
		Expect(spans[0].Status.Code).To(Equal(codes.Error))

		v, ok := attrOf(spans[0], "error.type")
		Expect(ok).To(BeTrue())
		Expect(v.AsString()).To(Equal(ClassOther.String()))
		Expect(v.AsString()).ToNot(BeEmpty())
	})
})

// ErrorClass is a struct wrapping a string rather than a defined string type, and the
// closure that buys is a compile-time property no spec can assert: there is no exported
// way to build a value, so telemetry.ErrorClass(err.Error()) does not compile from
// anywhere. What is worth asserting is that the vocabulary renders, since String() is what
// every span and metric attribute goes through and an empty one would be exported silently.
var _ = Describe("ErrorClass", func() {
	It("should render every class as a non-empty distinct value", func() {
		classes := []ErrorClass{
			ClassConfig, ClassProvider, ClassTimeout, ClassCanceled, ClassToolError,
			ClassTruncated, ClassRefusal, ClassPanic, ClassStore, ClassInvalidQuery,
			ClassRemoteUnavailable, ClassOther,
		}

		seen := map[string]bool{}
		for _, c := range classes {
			Expect(c.Set()).To(BeTrue())
			Expect(c.String()).ToNot(BeEmpty())
			Expect(seen[c.String()]).To(BeFalse(), "%q is declared twice", c.String())
			seen[c.String()] = true
		}
	})

	It("should report the zero value as unnamed", func() {
		Expect(ErrorClass{}.Set()).To(BeFalse())
		Expect(ErrorClass{}.String()).To(BeEmpty())
	})

	// ClassifyContext returns the zero value for anything it declines, which is what lets
	// a caller write `class, ok := ClassifyContext(err)` and fall through on ok alone.
	It("should return an unnamed class for an error it does not recognize", func() {
		class, ok := ClassifyContext(errors.New("connection refused"))
		Expect(ok).To(BeFalse())
		Expect(class.Set()).To(BeFalse())
	})
})
