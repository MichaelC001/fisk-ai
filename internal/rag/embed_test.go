//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package rag

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

		It("refuses vectors from a model other than the configured one", func() {
			// A local server handed a model name it does not have may answer 200 with
			// whichever model happens to be loaded, at a dimension that agrees with the
			// index. The reported model is the only evidence of the substitution.
			srv := fakeServer(func(w http.ResponseWriter, req embedRequest) {
				writeVectorsAs(w, "some-other-model")
			})
			defer srv.Close()

			_, err := newEmbedder(srv.URL).embedBatch(ctx, telemetry.EmbeddingsPurposeQuery, []string{"a"})
			Expect(err).To(MatchError(ErrModelMismatch))
			Expect(err).To(MatchError(ContainSubstring("test-model")))
			Expect(err).To(MatchError(ContainSubstring("some-other-model")))
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

		It("fails the dimension probe, so a substitution is caught before any index is written", func() {
			srv := fakeServer(func(w http.ResponseWriter, req embedRequest) {
				writeVectorsAs(w, "some-other-model")
			})
			defer srv.Close()

			_, err := newEmbedder(srv.URL).Dim(ctx)
			Expect(err).To(MatchError(ErrModelMismatch))
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
