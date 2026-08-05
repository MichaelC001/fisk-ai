//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// These close the gap the live-backend gate left open: phase 4's knowledge spans and
// counter had never crossed a wire.
//
// Two things here are only answerable against a decoder. fisk.knowledge.degraded_searches
// is the first counter in this work, so the first Sum rather than Histogram to be
// exported, and a counter that arrives shaped as something else is correct in memory and
// useless in a backend. And the embeddings span has to arrive parented to the retrieval
// span that opened it: a child that arrives parented to nothing becomes a second root,
// which renders as an unrelated trace and reads to an operator as dropped spans.
package agent_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/rag"
	"github.com/choria-io/fisk-ai/internal/telemetry"
	"github.com/choria-io/fisk-ai/internal/telemetry/bootstrap"
	"github.com/choria-io/fisk-ai/internal/util"
)

// embedModel is the model the fixture's index is pinned to. It has to match between the
// writer and the reader or the search rejects the index before any request is made, and
// the embeddings span is then named for a model the index was not built with.
const embedModel = "fixture-embed-1"

// embedDim is the vector width the fake server returns. Anything consistent works; the
// manifest pins whatever the writer saw.
const embedDim = 16

// fakeEmbeddings is an OpenAI-shaped embeddings server whose replies can be switched to
// failures once the index is built.
//
// One server rather than two so the reader reaches the same host and port it was
// configured with: the embeddings span records the server address, and a spec that
// pointed the search at a different endpoint would be asserting about a machine the
// index never used.
type fakeEmbeddings struct {
	server *httptest.Server
	broken atomic.Bool
}

func newFakeEmbeddings(tb testing.TB) *fakeEmbeddings {
	tb.Helper()

	f := &fakeEmbeddings{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if f.broken.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"model not loaded"}}`))
			return
		}

		var body struct {
			Input []string `json:"input"`
		}
		err := json.NewDecoder(req.Body).Decode(&body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		type item struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}

		out := struct {
			Data []item `json:"data"`
		}{}

		for i := range body.Input {
			vec := make([]float32, embedDim)
			// A deterministic non-zero vector. Its direction does not matter: these specs
			// assert on the shape of the telemetry, never on which document ranked first.
			for d := range vec {
				vec[d] = float32((i+1)*(d+1)%7) + 1
			}
			out.Data = append(out.Data, item{Embedding: vec, Index: i})
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
	tb.Cleanup(f.server.Close)

	return f
}

// URL is the base URL to configure, including the version prefix the embedder appends
// its path to.
func (f *fakeEmbeddings) URL() string { return f.server.URL + "/v1" }

// Break makes every later request fail, which is what degrades a search from hybrid to
// lexical and increments the counter.
func (f *fakeEmbeddings) Break() { f.broken.Store(true) }

// knowledgeFixture writes a small corpus, builds a hybrid index over it against emb, and
// returns the directory the index lives in.
func knowledgeFixture(t *testing.T, g *WithT, emb *fakeEmbeddings) string {
	t.Helper()

	tmp := t.TempDir()
	storeDir := filepath.Join(tmp, "knowledge")
	docsDir := filepath.Join(tmp, "docs")

	g.Expect(os.MkdirAll(docsDir, 0o755)).To(Succeed())
	g.Expect(os.WriteFile(filepath.Join(docsDir, "backpressure.md"),
		[]byte("# Design\n\n## Backpressure\n\nThe queue applies backpressure when the buffer is full so producers slow down.\n"),
		0o644)).To(Succeed())
	g.Expect(os.WriteFile(filepath.Join(docsDir, "auth.md"),
		[]byte("# Authentication\n\nTokens are validated against the issuer before any request proceeds.\n"),
		0o644)).To(Succeed())

	w, err := rag.OpenWriter(knowledgeConfig(storeDir, emb.URL()), "")
	g.Expect(err).ToNot(HaveOccurred())
	defer w.Close()

	_, err = w.Index(context.Background(), []string{docsDir}, rag.IndexOptions{Reconcile: true})
	g.Expect(err).ToNot(HaveOccurred())

	return storeDir
}

// knowledgeConfig is a configuration whose knowledge store is a hybrid index at dir
// backed by the embeddings server at baseURL.
func knowledgeConfig(dir string, baseURL string) *config.Config {
	return &config.Config{
		Identity: "agent",
		Harness: config.HarnessConfig{
			RAG: &config.RAGConfig{
				Enabled:   true,
				Directory: dir,
				Embeddings: &config.RAGEmbeddingsConfig{
					BaseURL:       baseURL,
					Model:         embedModel,
					TimeoutParsed: 5 * time.Second,
				},
			},
		},
	}
}

// exportKnowledgeRun drives a run whose model searches the knowledge index, through the
// real export path into rx.
func exportKnowledgeRun(t *testing.T, g *WithT, rx *agenttest.OTLPReceiver, storeDir string, baseURL string) {
	t.Helper()

	app := agenttest.NewFakeApp(t, exampleApp())
	cfg := agenttest.Config(t, app, func(c *config.Config) {
		c.Harness.RAG = knowledgeConfig(storeDir, baseURL).Harness.RAG
		c.Telemetry.Enabled = true
		c.Telemetry.Endpoint = rx.Endpoint()
	})

	tel, err := bootstrap.Start(context.Background(), bootstrap.Options{
		Config:  cfg,
		Version: util.Version(),
	})
	g.Expect(err).ToNot(HaveOccurred())

	responses := []*llm.Response{
		exportUsage(agenttest.ToolUseResponse("call_1", "knowledge_search", []byte(`{"query":"backpressure buffer"}`))),
		exportUsage(agenttest.TextResponse("the queue applies backpressure")),
	}

	_, err = agent.Run(context.Background(), agent.Options{
		Config:     cfg,
		ConfigFile: "agent.yaml",
		Prompt:     []string{"how does backpressure work"},
		Provider:   agenttest.NewScriptedProvider(t, responses...),
		Telemetry:  tel.Provider,
	}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).ToNot(HaveOccurred())

	delivery, err := tel.Close()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(delivery.Complete()).To(BeTrue(),
		"the receiver did not accept everything: %d of %d spans, err=%v",
		delivery.SpansDelivered, delivery.SpansAttempted, delivery.Err)
}

// TestExport_EmbeddingsNestUnderRetrieval is the assertion the StartSearch doc comment
// calls for: the two spans have every name and attribute right whether they nest or not,
// and only a comparison of parent ids tells them apart.
func TestExport_EmbeddingsNestUnderRetrieval(t *testing.T) {
	g := NewWithT(t)

	emb := newFakeEmbeddings(t)
	storeDir := knowledgeFixture(t, g, emb)

	rx := agenttest.NewOTLPReceiver(t)
	exportKnowledgeRun(t, g, rx, storeDir, emb.URL())

	retrieval := rx.Span(t, "retrieval")

	// One hybrid search makes two embeddings requests, so two spans: the dimension probe
	// that checks the live model against the index's pinned manifest, and the query
	// embedding itself. Every one of them has to nest, which is a stronger assertion than
	// picking one: a threading mistake that reparented only the second would pass any
	// spec that looked at a single span.
	embeddings := rx.SpansNamed("embeddings ")
	g.Expect(embeddings).To(HaveLen(2))

	for _, s := range embeddings {
		g.Expect(s.ParentSpanID).To(Equal(retrieval.SpanID),
			"embeddings arrived parented to %q rather than to the retrieval span %q, so a backend renders it as an unrelated trace",
			s.ParentSpanID, retrieval.SpanID)
		g.Expect(s.TraceID).To(Equal(retrieval.TraceID))
	}

	g.Expect(rx.ChildrenOf(retrieval)).To(HaveLen(len(embeddings)))

	// The retrieval span is itself a child rather than a root, which is what puts the
	// whole knowledge subtree under the tool call that asked for it.
	g.Expect(retrieval.ParentSpanID).ToNot(BeEmpty())
	tool := rx.Span(t, "execute_tool knowledge_search")
	g.Expect(retrieval.ParentSpanID).To(Equal(tool.SpanID))
}

// TestExport_DegradedSearchCounterCrossesTheWire is the first counter in this work to be
// exported, so the first Sum rather than Histogram. A counter arriving shaped as
// something else is correct in memory and answers nothing in a backend.
func TestExport_DegradedSearchCounterCrossesTheWire(t *testing.T) {
	g := NewWithT(t)

	emb := newFakeEmbeddings(t)
	storeDir := knowledgeFixture(t, g, emb)

	// The index is built; from here every embeddings request fails, so the search
	// degrades from hybrid to lexical rather than failing.
	emb.Break()

	rx := agenttest.NewOTLPReceiver(t)
	exportKnowledgeRun(t, g, rx, storeDir, emb.URL())

	m := rx.Metric(t, telemetry.MetricKnowledgeDegradedSearches)
	g.Expect(m.Histogram).To(BeFalse(),
		"%s is a counter and must arrive as a Sum, not a Histogram", telemetry.MetricKnowledgeDegradedSearches)
	g.Expect(m.IntValue).To(BeNumerically(">", 0))

	// The reason is what makes the counter actionable: an unreachable embeddings server
	// and an unreadable index manifest both degrade a search and have different fixes.
	reason, ok := m.Attributes[string(telemetry.AttrKnowledgeDegradedReason)]
	g.Expect(ok).To(BeTrue(), "the counter arrived with no degrade reason: %v", m.Attributes)
	g.Expect(reason).To(Equal(telemetry.DegradeEmbeddings.String()))
}

// TestExport_DegradedSearchRecordsTheFailedEmbeddings asserts the embeddings span still
// arrives, and still nests, when the request it covers failed. A degraded search that
// exported no embeddings span would leave the counter with nothing explaining it.
func TestExport_DegradedSearchRecordsTheFailedEmbeddings(t *testing.T) {
	g := NewWithT(t)

	emb := newFakeEmbeddings(t)
	storeDir := knowledgeFixture(t, g, emb)
	emb.Break()

	rx := agenttest.NewOTLPReceiver(t)
	exportKnowledgeRun(t, g, rx, storeDir, emb.URL())

	retrieval := rx.Span(t, "retrieval")

	// The dimension probe is the request that fails here, so it is the only embeddings
	// span: the query is never embedded once the probe has failed.
	embeddings := rx.SpansNamed("embeddings ")
	g.Expect(embeddings).ToNot(BeEmpty())

	for _, s := range embeddings {
		g.Expect(s.ParentSpanID).To(Equal(retrieval.SpanID))

		status, ok := s.Int("http.response.status_code")
		g.Expect(ok).To(BeTrue(), "the failed embeddings span carried no status code: %v", s.Attributes)
		g.Expect(status).To(Equal(int64(http.StatusInternalServerError)))

		// No error text ever reaches a span: this tree's errors embed absolute paths and
		// config values, and a status description cannot be un-sent.
		g.Expect(s.StatusMessage).To(BeEmpty())
	}

	// The retrieval span reports the degrade rather than an error, since the search
	// answered from the lexical tier and the caller got results.
	reason, ok := retrieval.String(string(telemetry.AttrKnowledgeDegradedReason))
	g.Expect(ok).To(BeTrue(), "the retrieval span did not say why it degraded: %v", retrieval.Attributes)
	g.Expect(reason).To(Equal(telemetry.DegradeEmbeddings.String()))
}
