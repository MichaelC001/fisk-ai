//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// These specs build an Embedder out of nothing but rag's exported API, which is all
// a caller plugging in Ollama, Bedrock, a local model or a test double has to work
// with.
package rag_test

import (
	"context"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/rag"
)

// suppliedEmbedder hashes a text's words into a fixed-dimension bag of words, so a
// query sharing words with a chunk lands near it under L2 distance and no request
// leaves the process. The specs read the Documents and queries it records to assert
// the store used this embedder rather than the one the configuration names.
type suppliedEmbedder struct {
	model string
	dim   int

	docs    []rag.Document
	queries []string
}

func (e *suppliedEmbedder) Model() string                    { return e.model }
func (e *suppliedEmbedder) QueryPrefix() string              { return "query: " }
func (e *suppliedEmbedder) DocumentPrefix() string           { return "title: {title} | " }
func (e *suppliedEmbedder) Dim(context.Context) (int, error) { return e.dim, nil }

func (e *suppliedEmbedder) EmbedQuery(_ context.Context, text string) ([]float32, error) {
	e.queries = append(e.queries, text)

	return e.vec(text), nil
}

func (e *suppliedEmbedder) EmbedDocuments(_ context.Context, docs []rag.Document) ([][]float32, error) {
	out := make([][]float32, len(docs))
	for i, d := range docs {
		e.docs = append(e.docs, d)
		out[i] = e.vec(d.Text)
	}

	return out, nil
}

func (e *suppliedEmbedder) vec(text string) []float32 {
	v := make([]float32, e.dim)
	for _, w := range strings.Fields(strings.ToLower(text)) {
		h := fnv.New32a()
		_, _ = h.Write([]byte(w))
		v[h.Sum32()%uint32(e.dim)]++
	}
	v[0] += 0.001 // never a zero vector

	return v
}

var _ = Describe("Options.Embedder", func() {
	ctx := context.Background()

	var (
		docsD string
		cfg   *config.Config
	)

	BeforeEach(func() {
		tmp := GinkgoT().TempDir()
		docsD = filepath.Join(tmp, "docs")
		Expect(os.MkdirAll(docsD, 0o700)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(docsD, "backpressure.md"),
			[]byte("# Backpressure\n\nThe queue applies backpressure when the buffer is full.\n"), 0o600)).To(Succeed())

		// The config points the embeddings server at a port nothing listens on, so a
		// store that built it rather than taking the supplied one fails at the first
		// dimension probe instead of quietly indexing.
		cfg = &config.Config{
			Identity: "test",
			Harness: config.HarnessConfig{
				RAG: &config.RAGConfig{
					Enabled:   true,
					Directory: filepath.Join(tmp, "knowledge"),
					Embeddings: &config.RAGEmbeddingsConfig{
						BaseURL:       "http://127.0.0.1:1/v1",
						Model:         "configured-model",
						TimeoutParsed: time.Second,
					},
				},
			},
		}
	})

	buildIndex := func() *suppliedEmbedder {
		emb := &suppliedEmbedder{model: "supplied-model", dim: 32}

		w, err := rag.OpenWriter(cfg, "", rag.Options{Embedder: emb})
		Expect(err).ToNot(HaveOccurred())
		defer w.Close()
		Expect(w.VectorEnabled()).To(BeTrue())

		stats, err := w.Index(ctx, []string{docsD}, rag.IndexOptions{Reconcile: true})
		Expect(err).ToNot(HaveOccurred())
		Expect(stats.Embeddings).To(BeNumerically(">", 0))
		Expect(emb.docs).To(HaveLen(stats.Embeddings))

		return emb
	}

	It("embeds the corpus through the supplied Embedder and pins its identity", func() {
		emb := buildIndex()

		Expect(emb.docs[0].Title).To(Equal("Backpressure"))
		Expect(emb.docs[0].Text).To(ContainSubstring("The queue applies backpressure"))
		Expect(emb.docs[0].Text).ToNot(ContainSubstring("#"))

		reader := &suppliedEmbedder{model: "supplied-model", dim: 32}
		r, err := rag.Open(cfg, "", rag.Options{Embedder: reader})
		Expect(err).ToNot(HaveOccurred())
		defer r.Close()

		st, err := r.Stats(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(st.Meta.Model).To(Equal("supplied-model"))
		Expect(st.Meta.QueryPrefix).To(Equal("query: "))
		Expect(st.Vectors).To(Equal(st.Chunks))

		res, err := r.Search(ctx, "backpressure buffer", 5)
		Expect(err).ToNot(HaveOccurred())
		Expect(res.Degraded).To(BeFalse())
		Expect(res.Hits).ToNot(BeEmpty())
		Expect(reader.queries).To(Equal([]string{"backpressure buffer"}))
	})

	It("builds the embedder the configuration names when Options carries none", func() {
		buildIndex()

		_, err := rag.Open(cfg, "", rag.Options{})
		Expect(err).To(MatchError(rag.ErrMetaMismatch))
		Expect(err.Error()).To(ContainSubstring("configured-model"))
	})
})
