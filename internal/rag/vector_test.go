//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package rag

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
)

// fakeEmbedder is a deterministic, network-free Embedder for the vector-tier
// tests: it maps a text's words into a fixed-dimension bag-of-words vector, so a
// query sharing words with a chunk lands near it under L2 distance.
type fakeEmbedder struct {
	model     string
	qp, dp    string
	dim       int
	failQuery bool
	// queryErr is returned by EmbedQuery when set, so a test can choose the class of
	// failure rather than only the transient one failQuery produces.
	queryErr error
}

func (f *fakeEmbedder) Model() string                    { return f.model }
func (f *fakeEmbedder) QueryPrefix() string              { return f.qp }
func (f *fakeEmbedder) DocumentPrefix() string           { return f.dp }
func (f *fakeEmbedder) Dim(context.Context) (int, error) { return f.dim, nil }

func (f *fakeEmbedder) EmbedQuery(_ context.Context, text string) ([]float32, error) {
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	if f.failQuery {
		return nil, errors.New("connection refused")
	}

	return f.vec(text), nil
}

func (f *fakeEmbedder) EmbedDocuments(_ context.Context, docs []Document) ([][]float32, error) {
	out := make([][]float32, len(docs))
	for i, d := range docs {
		out[i] = f.vec(d.Text)
	}

	return out, nil
}

func (f *fakeEmbedder) vec(text string) []float32 {
	v := make([]float32, f.dim)
	for _, w := range strings.Fields(strings.ToLower(text)) {
		h := fnv.New32a()
		_, _ = h.Write([]byte(w))
		v[h.Sum32()%uint32(f.dim)] += 1
	}
	v[0] += 0.001 // never a zero vector

	return v
}

func vectorConfig(dir, model string) *config.Config {
	return &config.Config{
		Identity: "test",
		Harness: config.HarnessConfig{
			RAG: &config.RAGConfig{
				Enabled:   true,
				Directory: dir,
				Embeddings: &config.RAGEmbeddingsConfig{
					BaseURL:       "http://127.0.0.1:1/v1", // nothing listens here, so an open that builds the configured embedder fails its first probe
					Model:         model,
					TimeoutParsed: time.Second,
				},
			},
		},
	}
}

// openWriterMock opens a writer against the mock embedder so no network is
// touched.
func openWriterMock(cfg *config.Config, emb Embedder) *Store {
	w, err := OpenWriter(cfg, "", Options{Embedder: emb})
	Expect(err).ToNot(HaveOccurred())

	return w
}

var _ = Describe("Store (vector tier)", func() {
	ctx := context.Background()

	var (
		tmp    string
		storeD string
		docsD  string
	)

	BeforeEach(func() {
		tmp = GinkgoT().TempDir()
		storeD = filepath.Join(tmp, "knowledge")
		docsD = filepath.Join(tmp, "docs")
		writeDoc(docsD, "backpressure.md", "# Backpressure\n\nThe queue applies backpressure when the buffer is full.\n")
		writeDoc(docsD, "auth.md", "# Authentication\n\nTokens are validated against the issuer.\n")
	})

	indexVector := func(model string, dim int) {
		w := openWriterMock(vectorConfig(storeD, model), &fakeEmbedder{model: model, dim: dim})
		defer w.Close()
		_, err := w.Index(ctx, []string{docsD}, IndexOptions{Reconcile: true})
		Expect(err).ToNot(HaveOccurred())
	}

	It("builds a hybrid index, pins the manifest, and retrieves via fusion", func() {
		indexVector("m1", 32)

		st, err := statsFor(vectorConfig(storeD, "m1"))
		Expect(err).ToNot(HaveOccurred())
		Expect(st.Vectors).To(Equal(st.Chunks))
		Expect(st.Vectors).To(BeNumerically(">", 0))
		Expect(st.Meta.Model).To(Equal("m1"))
		Expect(st.Meta.Dimension).To(Equal(32))
		Expect(st.Meta.Normalized).To(BeTrue())

		r, err := Open(vectorConfig(storeD, "m1"), "", Options{Embedder: &fakeEmbedder{model: "m1", dim: 32}})
		Expect(err).ToNot(HaveOccurred())
		defer r.Close()

		res, err := r.Search(ctx, "backpressure buffer full", 5)
		Expect(err).ToNot(HaveOccurred())
		Expect(res.Status).To(Equal(StatusOK))
		Expect(res.Degraded).To(BeFalse())
		Expect(res.Hits).ToNot(BeEmpty())
	})

	It("estimates the full embedding work for a dry-run reindex of an unchanged corpus", func() {
		indexVector("m1", 32)

		// A plain dry run over an unchanged corpus embeds nothing.
		w := openWriterMock(vectorConfig(storeD, "m1"), &fakeEmbedder{model: "m1", dim: 32})
		defer w.Close()
		incremental, err := w.Index(ctx, []string{docsD}, IndexOptions{DryRun: true})
		Expect(err).ToNot(HaveOccurred())
		Expect(incremental.Skipped).To(Equal(2))
		Expect(incremental.Embeddings).To(Equal(0))

		// A dry-run reindex re-embeds everything, so the estimate must reflect that
		// rather than skipping every unchanged file and reporting zero work.
		reindex, err := w.Index(ctx, []string{docsD}, IndexOptions{DryRun: true, Reindex: true})
		Expect(err).ToNot(HaveOccurred())
		Expect(reindex.Added).To(Equal(2))
		Expect(reindex.Skipped).To(Equal(0))
		Expect(reindex.Chunks).To(BeNumerically(">", 0))
		Expect(reindex.Embeddings).To(Equal(reindex.Chunks))
	})

	It("refuses a dimension change upfront without a reindex", func() {
		indexVector("m1", 32)

		w := openWriterMock(vectorConfig(storeD, "m1"), &fakeEmbedder{model: "m1", dim: 64})
		defer w.Close()
		_, err := w.Index(ctx, []string{docsD}, IndexOptions{Reconcile: true})
		Expect(err).To(MatchError(ErrMetaMismatch))
	})

	It("allows a dimension change with a reindex", func() {
		indexVector("m1", 32)

		w := openWriterMock(vectorConfig(storeD, "m1"), &fakeEmbedder{model: "m1", dim: 64})
		defer w.Close()
		_, err := w.Index(ctx, []string{docsD}, IndexOptions{Reconcile: true, Reindex: true})
		Expect(err).ToNot(HaveOccurred())

		st, err := w.Stats(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(st.Meta.Dimension).To(Equal(64))
	})

	It("refuses on the read path when the configured model differs from the manifest", func() {
		indexVector("m1", 32)

		_, err := Open(vectorConfig(storeD, "m2"), "", Options{})
		Expect(err).To(MatchError(ErrMetaMismatch))
	})

	It("refuses to add the vector tier to a lexical-only index without a reindex", func() {
		// Build lexical first.
		lw, err := OpenWriter(lexicalConfig(storeD), "", Options{})
		Expect(err).ToNot(HaveOccurred())
		_, err = lw.Index(ctx, []string{docsD}, IndexOptions{Reconcile: true})
		Expect(err).ToNot(HaveOccurred())
		lw.Close()

		w := openWriterMock(vectorConfig(storeD, "m1"), &fakeEmbedder{model: "m1", dim: 32})
		defer w.Close()
		_, err = w.Index(ctx, []string{docsD}, IndexOptions{Reconcile: true})
		Expect(err).To(MatchError(ErrMetaMismatch))
	})

	It("degrades to lexical when the embeddings server is unreachable at query time", func() {
		indexVector("m1", 32)

		r, err := Open(vectorConfig(storeD, "m1"), "", Options{Embedder: &fakeEmbedder{model: "m1", dim: 32, failQuery: true}})
		Expect(err).ToNot(HaveOccurred())
		defer r.Close()

		res, err := r.Search(ctx, "backpressure buffer", 5)
		Expect(err).ToNot(HaveOccurred())
		Expect(res.Status).To(Equal(StatusOK))
		Expect(res.Degraded).To(BeTrue())
		Expect(res.DegradeReason).To(ContainSubstring("connection refused"))
		Expect(res.Hits).ToNot(BeEmpty()) // lexical still answers
	})

	Describe("OnFile events", func() {
		collect := func(opts IndexOptions) []IndexEvent {
			var events []IndexEvent
			opts.OnFile = func(ev IndexEvent) { events = append(events, ev) }
			w := openWriterMock(vectorConfig(storeD, "m1"), &fakeEmbedder{model: "m1", dim: 32})
			defer w.Close()
			_, err := w.Index(ctx, []string{docsD}, opts)
			Expect(err).ToNot(HaveOccurred())

			return events
		}

		It("reports every embedded chunk on a first build", func() {
			events := collect(IndexOptions{Reconcile: true})
			Expect(events).To(HaveLen(2))

			total := 0
			for _, ev := range events {
				Expect(ev.Action).To(Equal(IndexAdded))
				Expect(ev.Path).ToNot(BeEmpty())
				Expect(ev.Embeddings).To(Equal(ev.Chunks))
				total += ev.Embeddings
			}
			Expect(total).To(BeNumerically(">", 0))
		})

		It("reports no embeddings for an unchanged file, so a progress total is not overrun", func() {
			// The chunk count of an unchanged file is what a previous run produced.
			// Counting it against a total covering only the files being embedded is
			// what would complete a progress display before the work was done.
			indexVector("m1", 32)

			events := collect(IndexOptions{Reconcile: true})
			Expect(events).To(HaveLen(2))
			for _, ev := range events {
				Expect(ev.Action).To(Equal(IndexUnchanged))
				Expect(ev.Chunks).To(BeNumerically(">", 0))
				Expect(ev.Embeddings).To(BeZero())
			}
		})

		It("reports an updated file with the chunks it re-embedded", func() {
			indexVector("m1", 32)
			writeDoc(docsD, "auth.md", "# Authentication\n\nTokens are validated against the issuer and the audience.\n")

			events := collect(IndexOptions{Reconcile: true})
			byAction := map[IndexAction]IndexEvent{}
			for _, ev := range events {
				byAction[ev.Action] = ev
			}
			Expect(byAction).To(HaveKey(IndexUpdated))
			Expect(byAction[IndexUpdated].Embeddings).To(Equal(byAction[IndexUpdated].Chunks))
			Expect(byAction[IndexUnchanged].Embeddings).To(BeZero())
		})

		It("stays silent on a dry run, which commits nothing", func() {
			Expect(collect(IndexOptions{Reconcile: true, DryRun: true})).To(BeEmpty())
		})
	})

	It("fails the query rather than degrading when the server serves a different model", func() {
		// The wrong model's vectors would silently answer from the wrong space, so this
		// is a config disagreement like a dimension mismatch, not a transient outage.
		indexVector("m1", 32)

		emb := &fakeEmbedder{model: "m1", dim: 32, queryErr: fmt.Errorf("%w: served %q", ErrModelMismatch, "m2")}
		r, err := Open(vectorConfig(storeD, "m1"), "", Options{Embedder: emb})
		Expect(err).ToNot(HaveOccurred())
		defer r.Close()

		_, err = r.Search(ctx, "backpressure buffer", 5)
		Expect(err).To(MatchError(ErrModelMismatch))
	})

	It("clears vector rows via the trigger when a document is reconciled away", func() {
		indexVector("m1", 32)

		before, err := statsFor(vectorConfig(storeD, "m1"))
		Expect(err).ToNot(HaveOccurred())

		Expect(os.Remove(filepath.Join(docsD, "auth.md"))).To(Succeed())
		w := openWriterMock(vectorConfig(storeD, "m1"), &fakeEmbedder{model: "m1", dim: 32})
		_, err = w.Index(ctx, []string{docsD}, IndexOptions{Reconcile: true})
		Expect(err).ToNot(HaveOccurred())
		w.Close()

		after, err := statsFor(vectorConfig(storeD, "m1"))
		Expect(err).ToNot(HaveOccurred())
		Expect(after.Vectors).To(Equal(after.Chunks))
		Expect(after.Vectors).To(BeNumerically("<", before.Vectors))
	})
})
