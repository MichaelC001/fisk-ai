//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package rag

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/choria-io/fisk-ai/internal/telemetry"
)

// The recording provider is built per spec through telemetry.NewFromProviders rather
// than through Setup, so nothing is registered globally and the specs stay parallel
// safe. It is reached the same way production reaches it, off the context, which is
// also what proves the context threading works.
func ragRecorder() (context.Context, *tracetest.InMemoryExporter, *sdkmetric.ManualReader) {
	exp := tracetest.NewInMemoryExporter()
	reader := sdkmetric.NewManualReader()
	p := telemetry.NewFromProviders(
		sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp)),
		sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)),
	)

	return telemetry.ContextWithProvider(context.Background(), p), exp, reader
}

// spanByName returns the single span with the given name, failing when there is not
// exactly one, so a spec cannot silently assert against the first of several.
func spanByName(exp *tracetest.InMemoryExporter, name string) tracetest.SpanStub {
	var found []tracetest.SpanStub
	for _, s := range exp.GetSpans() {
		if s.Name == name {
			found = append(found, s)
		}
	}

	ExpectWithOffset(1, found).To(HaveLen(1), "expected exactly one %q span, got %d", name, len(found))

	return found[0]
}

func spansByName(exp *tracetest.InMemoryExporter, name string) []tracetest.SpanStub {
	var found []tracetest.SpanStub
	for _, s := range exp.GetSpans() {
		if s.Name == name {
			found = append(found, s)
		}
	}

	return found
}

func attrOf(stub tracetest.SpanStub, key attribute.Key) (attribute.Value, bool) {
	for _, kv := range stub.Attributes {
		if kv.Key == key {
			return kv.Value, true
		}
	}

	return attribute.Value{}, false
}

// attrStrings renders every attribute value on a span, for the assertions that have to
// prove something is nowhere on it rather than merely absent from one key.
func attrStrings(stub tracetest.SpanStub) []string {
	out := make([]string, 0, len(stub.Attributes))
	for _, kv := range stub.Attributes {
		out = append(out, kv.Value.String())
	}

	return out
}

func counterValue(reader *sdkmetric.ManualReader, name string) (int64, []attribute.Set) {
	var rm metricdata.ResourceMetrics
	ExpectWithOffset(1, reader.Collect(context.Background(), &rm)).To(Succeed())

	var total int64
	var sets []attribute.Set
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			ExpectWithOffset(1, ok).To(BeTrue(), "%s is not an int64 sum", name)
			for _, dp := range sum.DataPoints {
				total += dp.Value
				sets = append(sets, dp.Attributes)
			}
		}
	}

	return total, sets
}

// leakingEmbedder fails every query with an error carrying exactly the shapes that must
// never reach a span: an endpoint URL and a bearer token.
type leakingEmbedder struct {
	*fakeEmbedder
}

const (
	leakedHost  = "embeddings.internal.example"
	leakedToken = "sk-abcdef0123456789"
)

func (l *leakingEmbedder) EmbedQuery(context.Context, string) ([]float32, error) {
	return nil, fmt.Errorf("contacting embeddings server at https://%s/v1: 401 Unauthorized: bad key %s", leakedHost, leakedToken)
}

var _ = Describe("Knowledge telemetry", func() {
	var (
		tmp    string
		storeD string
		docsD  string
	)

	BeforeEach(func() {
		tmp = GinkgoT().TempDir()
		storeD = filepath.Join(tmp, "knowledge")
		docsD = filepath.Join(tmp, "docs")

		writeDoc(docsD, "backpressure.md", "# Design\n\n## Backpressure\n\nThe queue applies backpressure when the buffer is full so producers slow down.\n")
		writeDoc(docsD, "auth.md", "# Authentication\n\nTokens are validated against the issuer before any request proceeds.\n")
	})

	indexLexical := func() {
		w, err := OpenWriter(lexicalConfig(storeD), "", Options{})
		Expect(err).ToNot(HaveOccurred())
		defer w.Close()
		_, err = w.Index(context.Background(), []string{docsD}, IndexOptions{Reconcile: true})
		Expect(err).ToNot(HaveOccurred())
	}

	indexHybrid := func(model string, dim int) {
		w := openWriterMock(vectorConfig(storeD, model), &fakeEmbedder{model: model, dim: dim})
		defer w.Close()
		_, err := w.Index(context.Background(), []string{docsD}, IndexOptions{Reconcile: true})
		Expect(err).ToNot(HaveOccurred())
	}

	hybridReader := func(model string, emb Embedder) *Store {
		r, err := Open(vectorConfig(storeD, model), "", Options{})
		Expect(err).ToNot(HaveOccurred())
		r.emb = emb

		return r
	}

	Describe("the search span", func() {
		It("reports an index that was never built, with no corpus size and no tier that ran", func() {
			ctx, exp, _ := ragRecorder()

			r, err := Open(lexicalConfig(storeD), "", Options{})
			Expect(err).ToNot(HaveOccurred())
			defer r.Close()

			res, err := r.Search(ctx, "backpressure", 5)
			Expect(err).ToNot(HaveOccurred())
			Expect(res.Status).To(Equal(StatusIndexNotBuilt))

			span := spanByName(exp, "retrieval")
			status, ok := attrOf(span, telemetry.AttrKnowledgeSearchStatus)
			Expect(ok).To(BeTrue())
			Expect(status.AsString()).To(Equal(string(StatusIndexNotBuilt)))

			// Absent rather than zero. A zero corpus size would read as an empty index,
			// which is a different answer with a different fix, and the count was never
			// taken on this path.
			_, ok = attrOf(span, telemetry.AttrKnowledgeIndexedChunks)
			Expect(ok).To(BeFalse())

			// Neither retriever ran, so naming a tier would report a retrieval that did
			// not happen.
			_, ok = attrOf(span, telemetry.AttrKnowledgeTierEffective)
			Expect(ok).To(BeFalse())

			tier, ok := attrOf(span, telemetry.AttrKnowledgeTierConfigured)
			Expect(ok).To(BeTrue())
			Expect(tier.AsString()).To(Equal(telemetry.TierLexical))
		})

		It("records the corpus size, the clamped top_k and the sections a lexical search returned", func() {
			indexLexical()
			ctx, exp, _ := ragRecorder()

			r, err := Open(lexicalConfig(storeD), "", Options{})
			Expect(err).ToNot(HaveOccurred())
			defer r.Close()

			// Far past the ceiling, so the span proves it carries the effective value
			// rather than what the caller asked for.
			res, err := r.Search(ctx, "backpressure buffer", 500)
			Expect(err).ToNot(HaveOccurred())
			Expect(res.Hits).ToNot(BeEmpty())

			span := spanByName(exp, "retrieval")
			Expect(span.SpanKind.String()).To(Equal("client"))

			topK, ok := attrOf(span, telemetry.AttrKnowledgeTopK)
			Expect(ok).To(BeTrue())
			Expect(topK.AsInt64()).To(Equal(int64(topKCeiling)))

			sections, ok := attrOf(span, telemetry.AttrKnowledgeSections)
			Expect(ok).To(BeTrue())
			Expect(sections.AsInt64()).To(Equal(int64(len(res.Hits))))

			chunks, ok := attrOf(span, telemetry.AttrKnowledgeIndexedChunks)
			Expect(ok).To(BeTrue())
			Expect(chunks.AsInt64()).To(BeNumerically(">", 0))

			effective, ok := attrOf(span, telemetry.AttrKnowledgeTierEffective)
			Expect(ok).To(BeTrue())
			Expect(effective.AsString()).To(Equal(telemetry.TierLexical))

			_, ok = attrOf(span, telemetry.AttrKnowledgeDegraded)
			Expect(ok).To(BeFalse())
		})

		It("reports the configured tier and the effective tier apart when a hybrid query degrades", func() {
			indexHybrid("m1", 32)
			ctx, exp, reader := ragRecorder()

			r := hybridReader("m1", &fakeEmbedder{model: "m1", dim: 32, failQuery: true})
			defer r.Close()

			res, err := r.Search(ctx, "backpressure buffer", 5)
			Expect(err).ToNot(HaveOccurred())
			Expect(res.Degraded).To(BeTrue())
			Expect(res.DegradeKind).To(Equal(DegradeEmbeddings))

			span := spanByName(exp, "retrieval")

			// The whole point of the pair. Asserting only that degraded is true passes
			// on an implementation that reports the same tier in both keys, which
			// answers "did this query use vectors" with a value that is never wrong and
			// never useful.
			configured, ok := attrOf(span, telemetry.AttrKnowledgeTierConfigured)
			Expect(ok).To(BeTrue())
			Expect(configured.AsString()).To(Equal(telemetry.TierHybrid))

			effective, ok := attrOf(span, telemetry.AttrKnowledgeTierEffective)
			Expect(ok).To(BeTrue())
			Expect(effective.AsString()).To(Equal(telemetry.TierLexical))

			degraded, ok := attrOf(span, telemetry.AttrKnowledgeDegraded)
			Expect(ok).To(BeTrue())
			Expect(degraded.AsBool()).To(BeTrue())

			reason, ok := attrOf(span, telemetry.AttrKnowledgeDegradedReason)
			Expect(ok).To(BeTrue())
			Expect(reason.AsString()).To(Equal(telemetry.DegradeEmbeddings.String()))

			// The counter exists because spans are head sampled: at a fleet's sample
			// ratio most degraded searches never reach a backend at all.
			total, sets := counterValue(reader, telemetry.MetricKnowledgeDegradedSearches)
			Expect(total).To(Equal(int64(1)))
			Expect(sets).To(HaveLen(1))
			value, ok := sets[0].Value(telemetry.AttrKnowledgeDegradedReason)
			Expect(ok).To(BeTrue())
			Expect(value.AsString()).To(Equal(telemetry.DegradeEmbeddings.String()))
		})

		It("puts no part of the underlying error on the span", func() {
			indexHybrid("m1", 32)
			ctx, exp, _ := ragRecorder()

			r := hybridReader("m1", &leakingEmbedder{&fakeEmbedder{model: "m1", dim: 32}})
			defer r.Close()

			res, err := r.Search(ctx, "backpressure buffer", 5)
			Expect(err).ToNot(HaveOccurred())
			Expect(res.Degraded).To(BeTrue())

			// The local surfaces still get the full text; that is what they are for.
			Expect(res.DegradeReason).To(ContainSubstring(leakedHost))
			Expect(res.DegradeReason).To(ContainSubstring(leakedToken))

			span := spanByName(exp, "retrieval")
			reason, ok := attrOf(span, telemetry.AttrKnowledgeDegradedReason)
			Expect(ok).To(BeTrue())
			Expect(reason.AsString()).To(Equal(telemetry.DegradeEmbeddings.String()))

			// Nowhere on the span, not merely absent from the reason key, and not in the
			// status description either.
			for _, v := range attrStrings(span) {
				Expect(v).ToNot(ContainSubstring(leakedHost))
				Expect(v).ToNot(ContainSubstring(leakedToken))
				Expect(v).ToNot(ContainSubstring("401"))
			}
			Expect(span.Status.Description).To(BeEmpty())
		})

		It("separates an unreadable index manifest from an unreachable embeddings server", func() {
			indexHybrid("m1", 32)
			ctx, exp, reader := ragRecorder()

			// A writer so the manifest can be dropped after the open-time validation has
			// already passed, which is the only way to reach the failure a live index
			// would hit mid-run.
			w := openWriterMock(vectorConfig(storeD, "m1"), &fakeEmbedder{model: "m1", dim: 32})
			defer w.Close()
			_, err := w.db.ExecContext(context.Background(), `DROP TABLE rag_meta`)
			Expect(err).ToNot(HaveOccurred())

			res, err := w.Search(ctx, "backpressure buffer", 5)
			Expect(err).ToNot(HaveOccurred())
			Expect(res.Degraded).To(BeTrue())

			// The store failed, not the embeddings server. Without the split this reads
			// as an outage on a machine that is working, and it is also the one degrade
			// path that opens no child span, so this attribute is all a trace has.
			Expect(res.DegradeKind).To(Equal(DegradeIndexMeta))

			span := spanByName(exp, "retrieval")
			reason, ok := attrOf(span, telemetry.AttrKnowledgeDegradedReason)
			Expect(ok).To(BeTrue())
			Expect(reason.AsString()).To(Equal(telemetry.DegradeIndexMeta.String()))

			Expect(spansByName(exp, "embeddings m1")).To(BeEmpty())

			_, sets := counterValue(reader, telemetry.MetricKnowledgeDegradedSearches)
			Expect(sets).To(HaveLen(1))
			value, ok := sets[0].Value(telemetry.AttrKnowledgeDegradedReason)
			Expect(ok).To(BeTrue())
			Expect(value.AsString()).To(Equal(telemetry.DegradeIndexMeta.String()))
		})
	})

	Describe("the degrade classification", func() {
		// The deadline outranks the step, and the common failure is what settles it: the
		// embeddings client carries its own timeout, so a hung server produces an error
		// that answers to both.
		It("gives the context cases precedence over the step that failed", func() {
			Expect(degradeKind(context.DeadlineExceeded, DegradeEmbeddings)).To(Equal(DegradeTimeout))
			Expect(degradeKind(context.Canceled, DegradeIndexMeta)).To(Equal(DegradeCanceled))
			Expect(degradeKind(fmt.Errorf("wrapped: %w", context.DeadlineExceeded), DegradeIndexMeta)).To(Equal(DegradeTimeout))
			Expect(degradeKind(errors.New("connection refused"), DegradeEmbeddings)).To(Equal(DegradeEmbeddings))
			Expect(degradeKind(errors.New("no such table"), DegradeIndexMeta)).To(Equal(DegradeIndexMeta))
		})

		// A drift guard that reads this package's list rather than restating it. A spec
		// naming the pairs it expects would be a third hand-written copy: it would agree
		// with both lists and catch nothing when one of them gained a value.
		It("maps every degrade kind to a distinct telemetry reason", func() {
			kinds := []DegradeKind{DegradeEmbeddings, DegradeTimeout, DegradeCanceled, DegradeIndexMeta}

			seen := map[string]DegradeKind{}
			for _, k := range kinds {
				reason := degradeReason(k)
				Expect(reason.Set()).To(BeTrue(), "%q maps to no reason", k)
				Expect(reason.String()).ToNot(Equal(telemetry.DegradeOther.String()), "%q falls through to the catch-all", k)

				other, dup := seen[reason.String()]
				Expect(dup).To(BeFalse(), "%q and %q both map to %q", k, other, reason.String())
				seen[reason.String()] = k
			}
		})
	})

	Describe("the enumerate span", func() {
		enumerate := func(ctx context.Context, query string, opts EnumerateOptions) (*EnumerateResult, error) {
			r, err := Open(lexicalConfig(storeD), "", Options{})
			Expect(err).ToNot(HaveOccurred())
			DeferCleanup(r.Close)

			return r.Enumerate(ctx, query, opts)
		}

		It("carries no tier attributes at all", func() {
			indexLexical()
			ctx, exp, _ := ragRecorder()

			_, err := enumerate(ctx, "backpressure", EnumerateOptions{})
			Expect(err).ToNot(HaveOccurred())

			span := spanByName(exp, "knowledge_enumerate")
			Expect(span.SpanKind.String()).To(Equal("internal"))

			// Enumeration never fuses vectors whatever the config says, so a tier pair
			// here would report a degradation on every call and make the one query the
			// pair exists for fire constantly with nothing wrong.
			_, ok := attrOf(span, telemetry.AttrKnowledgeTierConfigured)
			Expect(ok).To(BeFalse())
			_, ok = attrOf(span, telemetry.AttrKnowledgeTierEffective)
			Expect(ok).To(BeFalse())
		})

		It("reports the matched set, the returned set and the corpus size", func() {
			indexLexical()
			ctx, exp, _ := ragRecorder()

			res, err := enumerate(ctx, "the", EnumerateOptions{Limit: 1})
			Expect(err).ToNot(HaveOccurred())
			Expect(res.Status).To(Equal(EnumOK))
			Expect(res.Matched).To(BeNumerically(">", 1))

			span := spanByName(exp, "knowledge_enumerate")

			limit, ok := attrOf(span, telemetry.AttrKnowledgeLimit)
			Expect(ok).To(BeTrue())
			Expect(limit.AsInt64()).To(Equal(int64(1)))

			matched, ok := attrOf(span, telemetry.AttrKnowledgeMatched)
			Expect(ok).To(BeTrue())
			Expect(matched.AsInt64()).To(Equal(int64(res.Matched)))

			docs, ok := attrOf(span, telemetry.AttrKnowledgeDocuments)
			Expect(ok).To(BeTrue())
			Expect(docs.AsInt64()).To(Equal(int64(1)))

			truncated, ok := attrOf(span, telemetry.AttrKnowledgeTruncated)
			Expect(ok).To(BeTrue())
			Expect(truncated.AsBool()).To(BeTrue())

			indexed, ok := attrOf(span, telemetry.AttrKnowledgeIndexedDocuments)
			Expect(ok).To(BeTrue())
			Expect(indexed.AsInt64()).To(Equal(int64(2)))
		})

		// The four states a single count cannot tell apart, and the reason enumeration
		// reports its own status set rather than reusing the search one.
		It("reports each status, and omits the corpus size when there is no index", func() {
			ctx, exp, _ := ragRecorder()

			_, err := enumerate(ctx, "backpressure", EnumerateOptions{})
			Expect(err).ToNot(HaveOccurred())

			span := spanByName(exp, "knowledge_enumerate")
			status, ok := attrOf(span, telemetry.AttrKnowledgeEnumerateStatus)
			Expect(ok).To(BeTrue())
			Expect(status.AsString()).To(Equal(string(EnumIndexNotBuilt)))

			_, ok = attrOf(span, telemetry.AttrKnowledgeIndexedDocuments)
			Expect(ok).To(BeFalse())
		})

		It("reports a query whose every term was dropped as its own status", func() {
			indexLexical()
			ctx, exp, _ := ragRecorder()

			res, err := enumerate(ctx, "a b", EnumerateOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(res.Status).To(Equal(EnumQueryEmpty))

			span := spanByName(exp, "knowledge_enumerate")
			status, ok := attrOf(span, telemetry.AttrKnowledgeEnumerateStatus)
			Expect(ok).To(BeTrue())
			Expect(status.AsString()).To(Equal(string(EnumQueryEmpty)))
		})

		// The lifetime trap. The result is initialized to a successful status before
		// anything runs, so a Finish that read it rather than the abandoned return would
		// export a completed enumeration alongside its own error.
		It("records a failed query with no status, rather than a successful one", func() {
			indexLexical()
			ctx, exp, _ := ragRecorder()

			_, err := enumerate(ctx, `"retention`, EnumerateOptions{})
			Expect(err).To(MatchError(ContainSubstring("unbalanced quote")))

			span := spanByName(exp, "knowledge_enumerate")

			_, ok := attrOf(span, telemetry.AttrKnowledgeEnumerateStatus)
			Expect(ok).To(BeFalse())
			_, ok = attrOf(span, telemetry.AttrKnowledgeMatched)
			Expect(ok).To(BeFalse())

			class, ok := attrOf(span, "error.type")
			Expect(ok).To(BeTrue())
			Expect(class.AsString()).To(Equal(telemetry.ClassInvalidQuery.String()))
			Expect(span.Status.Description).To(BeEmpty())
		})
	})

	Describe("the embeddings span", func() {
		// The real embedder rather than the mock, since the span reports what actually
		// went over the wire and a mocked embedder deliberately emits nothing.
		liveReader := func(url string) *Store {
			indexHybrid("m1", 32)

			emb := newEmbedder(url)
			// The model has to match the one pinned in the manifest, or the span is named
			// for a model the index was not built with.
			emb.model = "m1"
			emb.host, emb.port = parseServerAddress(url)

			return hybridReader("m1", emb)
		}

		It("records the status code and the server for a rejected request", func() {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":{"message":"model not loaded"}}`))
			}))
			defer srv.Close()

			ctx, exp, _ := ragRecorder()
			r := liveReader(srv.URL)
			defer r.Close()

			res, err := r.Search(ctx, "backpressure buffer", 5)
			Expect(err).ToNot(HaveOccurred())
			Expect(res.Degraded).To(BeTrue())

			span := spanByName(exp, "embeddings m1")
			Expect(span.SpanKind.String()).To(Equal("client"))

			code, ok := attrOf(span, "http.response.status_code")
			Expect(ok).To(BeTrue())
			Expect(code.AsInt64()).To(Equal(int64(http.StatusInternalServerError)))

			class, ok := attrOf(span, "error.type")
			Expect(ok).To(BeTrue())
			Expect(class.AsString()).To(Equal(telemetry.ClassProvider.String()))

			host, ok := attrOf(span, "server.address")
			Expect(ok).To(BeTrue())
			Expect(host.AsString()).To(Equal("127.0.0.1"))

			// The raw base URL never reaches the span; it can carry userinfo.
			for _, v := range attrStrings(span) {
				Expect(v).ToNot(ContainSubstring("http://"))
			}
		})

		It("records no status code when the request never got a response", func() {
			ctx, exp, _ := ragRecorder()

			// A port nothing listens on, so the transport fails before any response
			// exists. Reaching for a status code without that branch is a nil
			// dereference, and the span would then never end and never be exported.
			r := liveReader("http://127.0.0.1:1/v1")
			defer r.Close()

			res, err := r.Search(ctx, "backpressure buffer", 5)
			Expect(err).ToNot(HaveOccurred())
			Expect(res.Degraded).To(BeTrue())

			span := spansByName(exp, "embeddings m1")[0]
			_, ok := attrOf(span, "http.response.status_code")
			Expect(ok).To(BeFalse())

			class, ok := attrOf(span, "error.type")
			Expect(ok).To(BeTrue())
			Expect(class.AsString()).To(Equal(telemetry.ClassProvider.String()))
		})

		It("does not blame the server for a request this process could not build", func() {
			ctx, _, _ := ragRecorder()
			exp := tracetest.NewInMemoryExporter()
			p := telemetry.NewFromProviders(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp)), nil)
			ctx = telemetry.ContextWithProvider(ctx, p)

			_, err := newEmbedder("http://127.0.0.1:1").embedBatch(ctx, telemetry.EmbeddingsPurposeQuery, []string{"   "})
			Expect(err).To(MatchError(ContainSubstring("empty")))

			span := spanByName(exp, "embeddings test-model")
			class, ok := attrOf(span, "error.type")
			Expect(ok).To(BeTrue())
			Expect(class.AsString()).To(Equal(telemetry.ClassOther.String()))
			_, ok = attrOf(span, "http.response.status_code")
			Expect(ok).To(BeFalse())
		})

		// The probe is lazy and cached per embedder, so the first search of a process
		// makes two requests that are otherwise identical in shape. Without the purpose
		// they are indistinguishable, and a server that is down never lets the probe
		// cache, so every later search emits a probe and no query embedding at all.
		It("distinguishes the dimension probe from the query embedding", func() {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				vec := make([]string, 32)
				for i := range vec {
					vec[i] = "0.1"
				}
				_, _ = fmt.Fprintf(w, `{"data":[{"index":0,"embedding":[%s]}]}`, strings.Join(vec, ","))
			}))
			defer srv.Close()

			ctx, exp, _ := ragRecorder()
			r := liveReader(srv.URL)
			defer r.Close()

			_, err := r.Search(ctx, "backpressure buffer", 5)
			Expect(err).ToNot(HaveOccurred())

			spans := spansByName(exp, "embeddings m1")
			Expect(spans).To(HaveLen(2))

			purposes := map[string]bool{}
			for _, s := range spans {
				purpose, ok := attrOf(s, telemetry.AttrEmbeddingsPurpose)
				Expect(ok).To(BeTrue())
				purposes[purpose.AsString()] = true
			}
			Expect(purposes).To(HaveKey(telemetry.EmbeddingsPurposeDimensionProbe))
			Expect(purposes).To(HaveKey(telemetry.EmbeddingsPurposeQuery))
		})

		// The parenting assertion, which is the only one that catches this: a search
		// that failed to thread its context produces an embeddings span with every name
		// and every attribute correct, sitting beside the retrieval span rather than
		// under it.
		It("nests under the search span that opened it", func() {
			ctx, exp, _ := ragRecorder()

			r := liveReader("http://127.0.0.1:1/v1")
			defer r.Close()

			_, err := r.Search(ctx, "backpressure buffer", 5)
			Expect(err).ToNot(HaveOccurred())

			search := spanByName(exp, "retrieval")
			embeddings := spansByName(exp, "embeddings m1")[0]

			Expect(embeddings.Parent.SpanID()).To(Equal(search.SpanContext.SpanID()))
			Expect(embeddings.SpanContext.TraceID()).To(Equal(search.SpanContext.TraceID()))
		})
	})

	Describe("with telemetry off", func() {
		// The nil Provider contract, which every call site here relies on: the span
		// kinds must return an empty value with a nil inner span, never a nil outer
		// pointer, or the promoted End and Fail panic on the path almost every run takes.
		It("runs the whole knowledge path against a context carrying no provider", func() {
			indexHybrid("m1", 32)

			r := hybridReader("m1", &fakeEmbedder{model: "m1", dim: 32})
			defer r.Close()

			ctx := context.Background()
			Expect(telemetry.ProviderFromContext(ctx)).To(BeNil())

			res, err := r.Search(ctx, "backpressure buffer", 5)
			Expect(err).ToNot(HaveOccurred())
			Expect(res.Hits).ToNot(BeEmpty())

			enum, err := r.Enumerate(ctx, "backpressure", EnumerateOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(enum.Status).To(Equal(EnumOK))
		})
	})
})

// The embeddings client's timeout is set explicitly wherever these specs build one:
// a config that never ran its prepare step leaves it zero, which is a client with no
// timeout at all rather than an immediate one.
var _ = Describe("parseServerAddress", func() {
	It("keeps the host and port and drops everything else", func() {
		host, port := parseServerAddress("https://user:secret@embeddings.example:8443/v1")
		Expect(host).To(Equal("embeddings.example"))
		Expect(port).To(Equal(8443))
	})

	It("reports no port when the URL names none", func() {
		host, port := parseServerAddress("https://embeddings.example/v1")
		Expect(host).To(Equal("embeddings.example"))
		Expect(port).To(Equal(0))
	})

	It("reports nothing for a URL it cannot parse", func() {
		host, port := parseServerAddress("://nope")
		Expect(host).To(BeEmpty())
		Expect(port).To(Equal(0))
	})
})

var _ = Describe("DegradeNote", func() {
	It("names the store rather than the server when the manifest is unreadable", func() {
		Expect(DegradeNote(DegradeIndexMeta)).To(ContainSubstring("index metadata"))
		Expect(DegradeNote(DegradeIndexMeta)).ToNot(ContainSubstring("embeddings server"))
		Expect(DegradedTierLine(DegradeIndexMeta, "boom")).ToNot(ContainSubstring("unreachable"))
	})

	It("still names the server for a real outage", func() {
		Expect(DegradeNote(DegradeEmbeddings)).To(ContainSubstring("embeddings server was unreachable"))
		Expect(DegradeNote(DegradeTimeout)).To(ContainSubstring("did not respond in time"))
	})
})

var _ = Describe("embeddings client timeout", func() {
	It("is set on every embedder these specs build", func() {
		Expect(newEmbedder("http://127.0.0.1:1").client.Timeout).To(BeNumerically(">", time.Duration(0)))
	})
})
