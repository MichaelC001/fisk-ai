//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package rag

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/telemetry"
)

// embedRequest and embedItem mirror the OpenAI embeddings request/response shapes
// the fake server speaks.
type embedRequest struct {
	Input []string `json:"input"`
	Model string   `json:"model"`
}

// fakeServer builds an httptest server whose handler is provided by the test.
func fakeServer(handler func(w http.ResponseWriter, req embedRequest)) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embedRequest
		Expect(json.NewDecoder(r.Body).Decode(&req)).To(Succeed())
		handler(w, req)
	}))
}

// writeVectors writes a well-formed response with one vector per input, in the
// given index order, each vector a distinct constant so the mapping is checkable.
func writeVectors(w http.ResponseWriter, indices []int) {
	type item struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	}
	var data []item
	for _, idx := range indices {
		data = append(data, item{Embedding: []float32{float32(idx) + 1, 0.5}, Index: idx})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func newEmbedder(url string) *openAIEmbedder {
	return &openAIEmbedder{baseURL: url, model: "test-model", client: &http.Client{Timeout: 5 * time.Second}}
}

var _ = Describe("Embedding client", func() {
	ctx := context.Background()

	Describe("buildEmbedder", func() {
		It("returns nil when the vector tier is off", func() {
			cfg := &config.Config{Identity: "t", Harness: config.HarnessConfig{RAG: &config.RAGConfig{Enabled: true}}}
			emb, err := buildEmbedder(cfg)
			Expect(err).ToNot(HaveOccurred())
			Expect(emb).To(BeNil())
		})

		It("accepts a non-loopback http base_url via config", func() {
			cfg := &config.Config{Identity: "t", Harness: config.HarnessConfig{RAG: &config.RAGConfig{
				Enabled:    true,
				Embeddings: &config.RAGEmbeddingsConfig{BaseURL: "http://example.com/v1", Model: "m", TimeoutParsed: time.Second},
			}}}
			emb, err := buildEmbedder(cfg)
			Expect(err).ToNot(HaveOccurred())
			Expect(emb).ToNot(BeNil())
		})

		// A Compose secret is a file ending in a newline; the token is sent without it.
		It("sends a token read from api_key_file as the bearer, resolved under the root", func() {
			root := GinkgoT().TempDir()
			Expect(os.MkdirAll(filepath.Join(root, "secrets"), 0700)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(root, "secrets", "embed_token"), []byte("from-the-file\n"), 0600)).To(Succeed())

			received := make(chan string, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received <- r.Header.Get("Authorization")
				writeVectors(w, []int{0})
			}))
			defer srv.Close()

			cfg := &config.Config{Identity: "t", RootDirectory: root, Harness: config.HarnessConfig{RAG: &config.RAGConfig{
				Enabled:    true,
				Embeddings: &config.RAGEmbeddingsConfig{BaseURL: srv.URL, Model: "test-model", APIKeyFile: "secrets/embed_token", TimeoutParsed: time.Second},
			}}}
			emb, err := buildEmbedder(cfg)
			Expect(err).ToNot(HaveOccurred())

			_, err = emb.EmbedQuery(ctx, "a")
			Expect(err).ToNot(HaveOccurred())
			Expect(received).To(Receive(Equal("Bearer from-the-file")))
		})

		It("fails at build when api_key_file cannot be read, naming the field and the path", func() {
			path := filepath.Join(GinkgoT().TempDir(), "absent")
			cfg := &config.Config{Identity: "t", Harness: config.HarnessConfig{RAG: &config.RAGConfig{
				Enabled:    true,
				Embeddings: &config.RAGEmbeddingsConfig{BaseURL: "http://example.com/v1", Model: "m", APIKeyFile: path, TimeoutParsed: time.Second},
			}}}
			_, err := buildEmbedder(cfg)
			Expect(err).To(MatchError(ContainSubstring("knowledge.embeddings.api_key_file")))
			Expect(err).To(MatchError(ContainSubstring(path)))
			Expect(err).To(MatchError(os.ErrNotExist))
		})
	})

	Describe("index-field mapping", func() {
		It("maps vectors by the response index, not array position", func() {
			srv := fakeServer(func(w http.ResponseWriter, req embedRequest) {
				// Return the objects in reverse order but with correct index fields.
				idx := make([]int, len(req.Input))
				for i := range req.Input {
					idx[len(req.Input)-1-i] = i
				}
				writeVectors(w, idx)
			})
			defer srv.Close()

			vecs, err := newEmbedder(srv.URL).embedBatch(ctx, telemetry.EmbeddingsPurposeQuery, []string{"a", "b", "c"})
			Expect(err).ToNot(HaveOccurred())
			Expect(vecs[0][0]).To(Equal(float32(1))) // index 0 -> value idx+1 = 1
			Expect(vecs[1][0]).To(Equal(float32(2)))
			Expect(vecs[2][0]).To(Equal(float32(3)))
		})

		It("fails the batch on a duplicated index", func() {
			srv := fakeServer(func(w http.ResponseWriter, req embedRequest) { writeVectors(w, []int{0, 0}) })
			defer srv.Close()
			_, err := newEmbedder(srv.URL).embedBatch(ctx, telemetry.EmbeddingsPurposeQuery, []string{"a", "b"})
			Expect(err).To(MatchError(ContainSubstring("duplicate index")))
		})

		It("fails the batch on a count mismatch", func() {
			srv := fakeServer(func(w http.ResponseWriter, req embedRequest) { writeVectors(w, []int{0}) })
			defer srv.Close()
			_, err := newEmbedder(srv.URL).embedBatch(ctx, telemetry.EmbeddingsPurposeQuery, []string{"a", "b"})
			Expect(err).To(MatchError(ContainSubstring("2 inputs")))
		})

		It("treats an error-shaped 200 as a failure", func() {
			srv := fakeServer(func(w http.ResponseWriter, req embedRequest) {
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": "model not loaded"}})
			})
			defer srv.Close()
			_, err := newEmbedder(srv.URL).embedBatch(ctx, telemetry.EmbeddingsPurposeQuery, []string{"a"})
			Expect(err).To(MatchError(ContainSubstring("model not loaded")))
		})

		It("rejects an empty input before sending", func() {
			_, err := newEmbedder("http://127.0.0.1:1").embedBatch(ctx, telemetry.EmbeddingsPurposeQuery, []string{"  "})
			Expect(err).To(MatchError(ContainSubstring("empty")))
		})
	})

	Describe("served model", func() {
		// writeVectorsAs answers like a server that reports which model it used.
		writeVectorsAs := func(w http.ResponseWriter, model string) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"model": model,
				"data":  []map[string]any{{"embedding": []float32{1, 0.5}, "index": 0}},
			})
		}

		It("accepts vectors from a model named differently and records the served name", func() {
			// A gateway routing to an upstream provider reports the provider's name
			// for the model it was asked for, so the response is accepted; the name is
			// kept for the index run and the doctor to report. The manifest still pins
			// the configured name.
			srv := fakeServer(func(w http.ResponseWriter, req embedRequest) {
				writeVectorsAs(w, "some-other-model")
			})
			defer srv.Close()

			e := newEmbedder(srv.URL)
			Expect(e.ServedModel()).To(BeEmpty())

			vecs, err := e.embedBatch(ctx, telemetry.EmbeddingsPurposeQuery, []string{"a"})
			Expect(err).ToNot(HaveOccurred())
			Expect(vecs).To(HaveLen(1))
			Expect(e.ServedModel()).To(Equal("some-other-model"))
			Expect(e.Model()).To(Equal("test-model"))
		})

		It("names the model and the status, without the server's body, when the probe is rejected", func() {
			// The body's shape is provider-specific, so it never reaches the user.
			srv := fakeServer(func(w http.ResponseWriter, req embedRequest) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"Invalid model identifier \"test-model\"."}`))
			})
			defer srv.Close()

			_, err := newEmbedder(srv.URL).Dim(ctx)
			Expect(err).To(MatchError(ContainSubstring(`failed to confirm embeddings model "test-model" exists`)))
			Expect(err).To(MatchError(ContainSubstring("400 Bad Request")))
			Expect(err.Error()).ToNot(ContainSubstring("Invalid model identifier"))
		})

		It("probes the dimension through a differently named model and records the served name", func() {
			srv := fakeServer(func(w http.ResponseWriter, req embedRequest) {
				writeVectorsAs(w, "some-other-model")
			})
			defer srv.Close()

			e := newEmbedder(srv.URL)
			dim, err := e.Dim(ctx)
			Expect(err).ToNot(HaveOccurred())
			Expect(dim).To(Equal(2))
			Expect(e.ServedModel()).To(Equal("some-other-model"))
		})

		It("reports the served name through Progress on an index run and pins the configured one", func() {
			// The fake answers every input with one vector, whatever the batch size,
			// under a name a gateway would report for the configured model.
			srv := fakeServer(func(w http.ResponseWriter, req embedRequest) {
				type item struct {
					Embedding []float32 `json:"embedding"`
					Index     int       `json:"index"`
				}
				data := make([]item, len(req.Input))
				for i := range req.Input {
					data[i] = item{Embedding: []float32{1, 0.5}, Index: i}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"model": "private/gateway/test-model", "data": data})
			})
			defer srv.Close()

			tmp := GinkgoT().TempDir()
			docsD := filepath.Join(tmp, "docs")
			writeDoc(docsD, "a.md", "# A\n\nsome text\n")

			w, err := OpenWriter(lexicalConfig(filepath.Join(tmp, "knowledge")), "", Options{Embedder: newEmbedder(srv.URL)})
			Expect(err).ToNot(HaveOccurred())
			defer w.Close()

			var notes []string
			_, err = w.Index(ctx, []string{docsD}, IndexOptions{Progress: func(msg string) { notes = append(notes, msg) }})
			Expect(err).ToNot(HaveOccurred())
			Expect(notes).To(ContainElement(`embeddings server served model "private/gateway/test-model" for configured model "test-model"; the index is pinned to the configured name`))

			meta, err := w.readMeta(ctx)
			Expect(err).ToNot(HaveOccurred())
			Expect(meta.Model).To(Equal("test-model"))
		})

		It("accepts vectors when the server reports the configured model", func() {
			srv := fakeServer(func(w http.ResponseWriter, req embedRequest) {
				writeVectorsAs(w, "test-model")
			})
			defer srv.Close()

			vecs, err := newEmbedder(srv.URL).embedBatch(ctx, telemetry.EmbeddingsPurposeQuery, []string{"a"})
			Expect(err).ToNot(HaveOccurred())
			Expect(vecs).To(HaveLen(1))
		})

		It("takes a server that omits the model at its word", func() {
			srv := fakeServer(func(w http.ResponseWriter, req embedRequest) { writeVectors(w, []int{0}) })
			defer srv.Close()

			vecs, err := newEmbedder(srv.URL).embedBatch(ctx, telemetry.EmbeddingsPurposeQuery, []string{"a"})
			Expect(err).ToNot(HaveOccurred())
			Expect(vecs).To(HaveLen(1))
		})
	})

	Describe("batch fallback", func() {
		It("falls back to smaller batches when the server rejects a multi-input batch", func() {
			srv := fakeServer(func(w http.ResponseWriter, req embedRequest) {
				if len(req.Input) > 1 {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte("batch too large"))
					return
				}
				writeVectors(w, []int{0})
			})
			defer srv.Close()

			vecs, err := newEmbedder(srv.URL).EmbedDocuments(ctx, []Document{{Text: "a"}, {Text: "b"}, {Text: "c"}})
			Expect(err).ToNot(HaveOccurred())
			Expect(vecs).To(HaveLen(3))
		})
	})

	Describe("dimension probe", func() {
		It("probes once and caches the dimension", func() {
			calls := 0
			srv := fakeServer(func(w http.ResponseWriter, req embedRequest) {
				calls++
				writeVectors(w, []int{0})
			})
			defer srv.Close()

			e := newEmbedder(srv.URL)
			dim, err := e.Dim(ctx)
			Expect(err).ToNot(HaveOccurred())
			Expect(dim).To(Equal(2))
			_, _ = e.Dim(ctx)
			Expect(calls).To(Equal(1), "the dimension is cached after the first probe")
		})

		It("probes the dimension safely under concurrent callers", func() {
			srv := fakeServer(func(w http.ResponseWriter, req embedRequest) {
				writeVectors(w, []int{0})
			})
			defer srv.Close()

			e := newEmbedder(srv.URL)

			const n = 16
			var wg sync.WaitGroup
			dims := make([]int, n)
			errs := make([]error, n)
			wg.Add(n)
			for i := 0; i < n; i++ {
				go func(i int) {
					defer wg.Done()
					dims[i], errs[i] = e.Dim(ctx)
				}(i)
			}
			wg.Wait()

			for i := 0; i < n; i++ {
				Expect(errs[i]).ToNot(HaveOccurred())
				Expect(dims[i]).To(Equal(2))
			}
		})
	})
})
