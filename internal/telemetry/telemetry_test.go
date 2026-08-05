//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel"
)

var _ = Describe("Provider", func() {
	// The nil Provider is the no-op every call site relies on: the run path wires
	// telemetry up without asking whether it is on, which is what keeps enable and
	// disable branches out of the loop and stops the two paths diverging.
	Describe("a nil Provider", func() {
		var p *Provider

		It("should report itself disabled", func() {
			Expect(p.Enabled()).To(BeFalse())
		})

		It("should return the context unchanged and a usable span", func() {
			ctx := context.Background()

			got, span := p.StartStartup(ctx, StartupInfo{Identity: "demo"})
			Expect(got).To(Equal(ctx))

			Expect(func() {
				span.SetTools(ToolCounts{Application: 1})
				span.Fail(errors.New("boom"), ClassConfig)
				span.End()
			}).ToNot(Panic())
		})

		// End and Fail are promoted from the embedded *Span, and a promoted method
		// reached through a nil OUTER pointer has to dereference that pointer to find
		// the field. So a disabled constructor returning a nil *RemoteAgentSpan panics
		// here even though every method it exposes handles a nil receiver, and it panics
		// on the path almost every run takes. The constructor returns an empty value
		// with a nil inner *Span instead; this is what holds it to that.
		It("should survive the promoted methods on every span kind", func() {
			ctx := context.Background()

			_, remote := p.StartRemoteAgent(ctx, RemoteAgentInfo{Agent: "peer", Tool: "do"})
			_, chat := p.StartChat(ctx, ChatInfo{Model: "m"})
			_, tool := p.StartTool(ctx, ToolInfo{Name: "do"})
			_, turn := p.StartTurn(ctx, TurnInfo{Identity: "demo"})
			_, run := p.StartRun(ctx, RunInfo{Identity: "demo"})
			_, search := p.StartSearch(ctx, SearchInfo{Hybrid: true, TopK: 5})
			_, enumerate := p.StartEnumerate(ctx, EnumerateInfo{Limit: 5})
			_, embeddings := p.StartEmbeddings(ctx, EmbeddingsInfo{Model: "m", Inputs: 1})

			Expect(func() {
				for _, s := range []interface {
					End()
					Fail(error, ErrorClass)
				}{remote, chat, tool, turn, run, search, enumerate, embeddings} {
					s.Fail(errors.New("boom"), ClassOther)
					s.End()
				}
			}).ToNot(Panic())
		})

		// The Finish methods are declared on the outer types, so unlike the promoted pair
		// above they are safe on a nil receiver by construction. They are driven anyway
		// because they are what the call sites actually use, and because a Finish that
		// reached the metric instruments without checking would take a different route to
		// the same panic.
		It("should survive the finish methods on every span kind that has one", func() {
			ctx := context.Background()

			_, search := p.StartSearch(ctx, SearchInfo{Hybrid: true, TopK: 5})
			_, enumerate := p.StartEnumerate(ctx, EnumerateInfo{})
			_, embeddings := p.StartEmbeddings(ctx, EmbeddingsInfo{Model: "m"})

			Expect(func() {
				search.Finish(ctx, SearchOutcome{Degraded: true, Degrade: DegradeEmbeddings})
				enumerate.Finish(EnumerateOutcome{Failed: true, Class: ClassStore})
				embeddings.Finish(EmbeddingsOutcome{Failed: true, Class: ClassProvider})
			}).ToNot(Panic())
		})

		It("should shut down without error and report nothing attempted", func() {
			d, err := p.Shutdown(context.Background())
			Expect(err).ToNot(HaveOccurred())
			Expect(d.Attempted()).To(BeFalse())
		})
	})
})

var _ = Describe("Setup", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("should build nothing for a configuration that is off", func() {
		r, err := Resolve(Settings{}, envFrom(nil))
		Expect(err).ToNot(HaveOccurred())

		p, err := Setup(ctx, r, "1.2.3")
		Expect(err).ToNot(HaveOccurred())
		Expect(p).To(BeNil())
		Expect(p.Enabled()).To(BeFalse())
	})

	It("should build a metric pipeline with telemetry", func() {
		p := setupAgainst(ctx, collector(http.StatusOK), false)
		defer p.Shutdown(ctx)

		Expect(p.Enabled()).To(BeTrue())
		Expect(p.meter).ToNot(BeNil())
	})

	It("should leave the metric pipeline unbuilt for no_metrics", func() {
		p := setupAgainst(ctx, collector(http.StatusOK), true)
		defer p.Shutdown(ctx)

		Expect(p.Enabled()).To(BeTrue())
		Expect(p.meter).To(BeNil())
		Expect(p.mp).To(BeNil())
	})
})

// Export that silently fails looks identical to export that works, because OTLP is
// fire and forget over HTTP and the exporters report neither a delivered count nor an
// error to the application. These specs are the tie-breaker.
var _ = Describe("Shutdown delivery reporting", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("should report what the collector accepted", func() {
		srv := collector(http.StatusOK)
		p := setupAgainst(ctx, srv, true)

		_, span := p.StartStartup(ctx, StartupInfo{Identity: "demo"})
		span.End()

		d, err := p.Shutdown(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(d.SpansAttempted).To(Equal(int64(1)))
		Expect(d.SpansDelivered).To(Equal(int64(1)))
		Expect(d.Err).ToNot(HaveOccurred())
		Expect(d.Attempted()).To(BeTrue())
		Expect(d.Complete()).To(BeTrue())
		Expect(d.Endpoint).To(Equal(srv.URL))
	})

	// The case an operator hits with an expired token: the run works, the traces do
	// not, and without this nothing in the process ever says so.
	//
	// Note which value carries the bad news. Shutdown's own error is nil, because the
	// batch processor gives its export errors to the global error handler and returns
	// nothing to the caller. A caller checking only the error sees a clean shutdown of
	// a pipeline that delivered none of its spans, which is exactly the tie this
	// Delivery exists to break.
	It("should report a rejected export rather than looking successful", func() {
		p := setupAgainst(ctx, collector(http.StatusUnauthorized), true)

		_, span := p.StartStartup(ctx, StartupInfo{Identity: "demo"})
		span.End()

		d, err := p.Shutdown(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(d.SpansAttempted).To(Equal(int64(1)))
		Expect(d.SpansDelivered).To(BeZero())
		Expect(d.Err).To(MatchError(ContainSubstring("401")))
		Expect(d.Attempted()).To(BeTrue())
		Expect(d.Complete()).To(BeFalse())
	})

	// Without a handler installed, OpenTelemetry's default prints the error through the
	// log package, whose default logger holds the os.Stderr it captured at init. That
	// is a channel no caller chose and one that survives a later swap of the os.Stderr
	// variable, which is how the full-screen UI keeps writes off the alt-screen. So an
	// installed handler is not a nicety: it is what stops a rejected endpoint writing
	// to the terminal for the length of the run.
	It("should send export failures to the installed handler rather than the log package", func() {
		var out strings.Builder
		DeferCleanup(SetErrorHandler(&out))

		p := setupAgainst(ctx, collector(http.StatusUnauthorized), true)

		_, span := p.StartStartup(ctx, StartupInfo{Identity: "demo"})
		span.End()

		_, err := p.Shutdown(ctx)
		Expect(err).ToNot(HaveOccurred())

		Expect(out.String()).To(ContainSubstring("401"))
		Expect(out.String()).To(HavePrefix("warning: telemetry export error:"))
	})

	It("should report nothing attempted for a run that recorded nothing", func() {
		p := setupAgainst(ctx, collector(http.StatusOK), true)

		d, err := p.Shutdown(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(d.Attempted()).To(BeFalse())
	})

	// With the metric pipeline running but nothing recorded, the periodic reader still
	// collects at shutdown and hands over an empty batch, which the OTLP exporter will
	// post rather than skip. Collectors reject that: telemetry.dev answers 400 "Request
	// contains no metric data points". Every run that recorded no metrics would end on
	// an export failure warning describing nothing wrong, so the empty collection is
	// dropped before the wire and is not counted as an attempt.
	It("should not post an empty metric collection", func() {
		requests := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests++
			w.WriteHeader(http.StatusBadRequest)
		}))
		DeferCleanup(srv.Close)

		r, err := Resolve(Settings{Enabled: true, Endpoint: srv.URL}, envFrom(nil))
		Expect(err).ToNot(HaveOccurred())

		p, err := Setup(ctx, r, "1.2.3")
		Expect(err).ToNot(HaveOccurred())
		Expect(p.meter).ToNot(BeNil())

		d, err := p.Shutdown(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(requests).To(BeZero())
		Expect(d.MetricExportsAttempted).To(BeZero())
		Expect(d.Attempted()).To(BeFalse())
		Expect(d.Err).ToNot(HaveOccurred())
	})

	// An unended span is never exported. That is the failure mode this whole area is
	// prone to, so it is pinned rather than assumed.
	It("should not deliver a span that was never ended", func() {
		p := setupAgainst(ctx, collector(http.StatusOK), true)

		_, span := p.StartStartup(ctx, StartupInfo{Identity: "demo"})
		Expect(span).ToNot(BeNil())

		d, err := p.Shutdown(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(d.SpansAttempted).To(BeZero())
	})

	// An explicit zero ratio means sample nothing, which must reach the exporter as
	// nothing sent rather than as everything sent.
	It("should export nothing at a zero sample ratio", func() {
		srv := collector(http.StatusOK)
		DeferCleanup(srv.Close)

		r, err := Resolve(Settings{Enabled: true, Endpoint: srv.URL, NoMetrics: true, SampleRatio: ratio(0)}, envFrom(nil))
		Expect(err).ToNot(HaveOccurred())

		p, err := Setup(ctx, r, "1.2.3")
		Expect(err).ToNot(HaveOccurred())

		_, span := p.StartStartup(ctx, StartupInfo{Identity: "demo"})
		span.End()

		d, err := p.Shutdown(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(d.SpansAttempted).To(BeZero())
	})
})

// collector is a stand-in OTLP/HTTP receiver answering every request with status. It
// never parses the protobuf body: what these specs are about is what the exporter
// reports back to the process, not what it serialized.
func collector(status int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
}

// setupAgainst builds a real OTLP/HTTP Provider pointed at srv and registers the
// teardown for both. noMetrics keeps most specs to the trace pipeline alone so the
// delivery counts have one unambiguous source.
func setupAgainst(ctx context.Context, srv *httptest.Server, noMetrics bool) *Provider {
	GinkgoHelper()

	DeferCleanup(srv.Close)

	r, err := Resolve(Settings{Enabled: true, Endpoint: srv.URL, NoMetrics: noMetrics}, envFrom(nil))
	Expect(err).ToNot(HaveOccurred())
	Expect(r.EndpointFromConfig).To(BeTrue())

	p, err := Setup(ctx, r, "1.2.3")
	Expect(err).ToNot(HaveOccurred())
	Expect(p).ToNot(BeNil())

	DeferCleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		p.Shutdown(shutdownCtx)
	})

	return p
}

// Export errors reach a handler that belongs to one Provider.
//
// The reason this exists rather than leaving it to OpenTelemetry's own handler is that
// the SDK's is process-global: with two agents in one process the second one started
// takes it over, so the first agent's failures are reported through a sink belonging to
// the second, and nothing says so. These specs pin the property, not the plumbing.
var _ = Describe("WithExportErrorHandler", func() {
	It("Should report an export failure to the handler for that Provider", func() {
		var seen []error

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		DeferCleanup(srv.Close)

		resolved, err := Resolve(Settings{Enabled: true, Endpoint: srv.URL, NoMetrics: true}, envFrom(nil))
		Expect(err).ToNot(HaveOccurred())

		p, err := Setup(context.Background(), resolved, "test",
			WithExportErrorHandler(func(e error) { seen = append(seen, e) }))
		Expect(err).ToNot(HaveOccurred())

		_, span := p.StartStartup(context.Background(), StartupInfo{Identity: "agent"})
		span.End()

		_, err = p.Shutdown(context.Background())
		Expect(err).ToNot(HaveOccurred())

		Expect(seen).ToNot(BeEmpty(), "the export failed and nothing was told")
	})

	// The property the whole option exists for, and the ordering matters: the provider
	// that fails is started FIRST and the second is started before it flushes. That is
	// the shape a process-global handler gets wrong, because the second registration is
	// what takes the first provider's reporting over. Starting the broken one second
	// passes against a singleton and proves nothing.
	It("Should keep two Providers in one process from sharing a handler", func() {
		var first, second []error

		bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		DeferCleanup(bad.Close)

		ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/x-protobuf")
		}))
		DeferCleanup(ok.Close)

		start := func(endpoint string, sink *[]error) *Provider {
			resolved, err := Resolve(Settings{Enabled: true, Endpoint: endpoint, NoMetrics: true}, envFrom(nil))
			Expect(err).ToNot(HaveOccurred())

			p, err := Setup(context.Background(), resolved, "test",
				WithExportErrorHandler(func(e error) { *sink = append(*sink, e) }))
			Expect(err).ToNot(HaveOccurred())

			_, span := p.StartStartup(context.Background(), StartupInfo{Identity: "agent"})
			span.End()

			return p
		}

		broken := start(bad.URL, &first)
		healthy := start(ok.URL, &second)

		_, err := broken.Shutdown(context.Background())
		Expect(err).ToNot(HaveOccurred())
		_, err = healthy.Shutdown(context.Background())
		Expect(err).ToNot(HaveOccurred())

		Expect(first).ToNot(BeEmpty(), "the failing Provider's own failure was reported somewhere else")
		Expect(second).To(BeEmpty(), "a Provider was told about another Provider's failure")
	})

	It("Should be safe when no handler was asked for", func() {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		DeferCleanup(srv.Close)

		resolved, err := Resolve(Settings{Enabled: true, Endpoint: srv.URL, NoMetrics: true}, envFrom(nil))
		Expect(err).ToNot(HaveOccurred())

		p, err := Setup(context.Background(), resolved, "test")
		Expect(err).ToNot(HaveOccurred())

		_, span := p.StartStartup(context.Background(), StartupInfo{Identity: "agent"})
		span.End()

		delivery, err := p.Shutdown(context.Background())
		Expect(err).ToNot(HaveOccurred())
		Expect(delivery.Err).To(HaveOccurred(), "the failure is still recorded on the delivery")
	})
})

// The restore SetErrorHandler returns. Without it a test that points the handler at a
// local buffer leaves the process writing into that buffer for every later spec, which
// is why the suite hand-rolled a guess at one before this existed.
var _ = Describe("SetErrorHandler restore", func() {
	It("should stop writing to the previous writer once restored", func() {
		var first, second strings.Builder

		restore := SetErrorHandler(&first)
		SetErrorHandler(&second)

		otel.Handle(errors.New("after the second install"))
		Expect(second.String()).To(ContainSubstring("after the second install"))
		Expect(first.String()).To(BeEmpty())

		// Restoring the outer installation puts back whatever was there before it, not
		// the one installed in between.
		restore()

		otel.Handle(errors.New("after restore"))
		Expect(second.String()).ToNot(ContainSubstring("after restore"))

		DeferCleanup(SetErrorHandler(io.Discard))
	})

	It("should be safe to call more than once", func() {
		var out strings.Builder

		restore := SetErrorHandler(&out)
		restore()
		Expect(restore).ToNot(Panic())

		DeferCleanup(SetErrorHandler(io.Discard))
	})
})

// telemetry.endpoint is a base URL, so the signal path is this package's to append. The
// exporters will not do it: WithEndpointURL treats a pathless URL as targeting the root,
// so a bare base URL posts every export to / and the collector answers 404. Nothing in
// the process reports that except the delivery counts, and it reads as a broken
// collector rather than a misconfigured client.
var _ = Describe("signalEndpoint", func() {
	It("should append the signal path to a bare base URL", func() {
		Expect(signalEndpoint("http://127.0.0.1:4318", tracesPath)).To(Equal("http://127.0.0.1:4318/v1/traces"))
		Expect(signalEndpoint("http://127.0.0.1:4318", metricsPath)).To(Equal("http://127.0.0.1:4318/v1/metrics"))
	})

	It("should not double the separator on a base URL with a trailing slash", func() {
		Expect(signalEndpoint("http://127.0.0.1:4318/", tracesPath)).To(Equal("http://127.0.0.1:4318/v1/traces"))
	})

	// The OTLP specification says the base variable's path prefix is kept, so a
	// collector mounted behind a gateway at /otlp still receives /otlp/v1/traces.
	It("should keep a path prefix on the base URL", func() {
		Expect(signalEndpoint("https://gw.example.net/otlp", tracesPath)).To(Equal("https://gw.example.net/otlp/v1/traces"))
		Expect(signalEndpoint("https://gw.example.net/otlp/", metricsPath)).To(Equal("https://gw.example.net/otlp/v1/metrics"))
	})

	It("should preserve the scheme, which is what decides TLS", func() {
		Expect(signalEndpoint("https://otel.example.net:4318", tracesPath)).To(HavePrefix("https://"))
	})

	It("should return an unparseable endpoint unchanged rather than mangling it", func() {
		Expect(signalEndpoint("://nonsense", tracesPath)).To(Equal("://nonsense"))
	})
})
