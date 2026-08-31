//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package serve_test

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
	"github.com/choria-io/fisk-ai/internal/serve"
)

// workerEmbedder hashes a text's words into a fixed-dimension bag of words and sends
// nothing over the network, so a worker can index and search without an embeddings
// server. It pins an identity the configuration does not name, which is what tells the
// two apart.
type workerEmbedder struct {
	model string
	dim   int
}

func (e *workerEmbedder) Model() string                    { return e.model }
func (e *workerEmbedder) QueryPrefix() string              { return "query: " }
func (e *workerEmbedder) DocumentPrefix() string           { return "title: {title} | " }
func (e *workerEmbedder) Dim(context.Context) (int, error) { return e.dim, nil }

func (e *workerEmbedder) EmbedQuery(_ context.Context, text string) ([]float32, error) {
	return e.vec(text), nil
}

func (e *workerEmbedder) EmbedDocuments(_ context.Context, docs []rag.Document) ([][]float32, error) {
	out := make([][]float32, len(docs))
	for i, d := range docs {
		out[i] = e.vec(d.Text)
	}

	return out, nil
}

func (e *workerEmbedder) vec(text string) []float32 {
	v := make([]float32, e.dim)
	for _, w := range strings.Fields(strings.ToLower(text)) {
		h := fnv.New32a()
		_, _ = h.Write([]byte(w))
		v[h.Sum32()%uint32(e.dim)]++
	}
	v[0] += 0.001 // never a zero vector

	return v
}

// The knowledge store a worker shares is the one resource whose embedder cannot come
// from a configuration, so ResourceOptions has to carry it or a hosted worker can only
// ever reach the configured embeddings server.
var _ = Describe("ResourceOptions.RAG", func() {
	var (
		cfg     *config.Config
		docsDir string
	)

	BeforeEach(func() {
		tmp := GinkgoT().TempDir()
		docsDir = filepath.Join(tmp, "docs")
		Expect(os.MkdirAll(docsDir, 0o700)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(docsDir, "pumps.md"),
			[]byte("# Pumps\n\nThe standby pump takes over when the primary stalls.\n"), 0o600)).To(Succeed())

		// The embeddings server is a port nothing listens on, so a store that builds the
		// configured embedder fails its first dimension probe rather than quietly indexing
		// against something else.
		cfg = &config.Config{
			Identity: "worker",
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

	// buildIndex writes an index pinned to the supplied embedder's identity, which is
	// what a later open has to match.
	buildIndex := func() *workerEmbedder {
		e := &workerEmbedder{model: "supplied-model", dim: 32}

		w, err := rag.OpenWriter(cfg, "", rag.Options{Embedder: e})
		Expect(err).ToNot(HaveOccurred())
		defer w.Close()

		Expect(w.VectorEnabled()).To(BeTrue())

		_, err = w.Index(context.Background(), []string{docsDir}, rag.IndexOptions{})
		Expect(err).ToNot(HaveOccurred())

		return e
	}

	It("Should open the shared knowledge store with the caller's embedder", func() {
		e := buildIndex()

		res, err := serve.NewResources(cfg, serve.ResourceOptions{
			RAG:    rag.Options{Embedder: e},
			Logger: quietLogger(),
		})
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(res.Close)

		Expect(res.RAGStore).ToNot(BeNil())
		Expect(res.RAGStore.VectorEnabled()).To(BeTrue())
	})

	It("Should refuse the index the caller's embedder wrote when it is given none", func() {
		buildIndex()

		_, err := serve.NewResources(cfg, serve.ResourceOptions{Logger: quietLogger()})
		Expect(err).To(MatchError(ContainSubstring("configured-model")))
	})
})
