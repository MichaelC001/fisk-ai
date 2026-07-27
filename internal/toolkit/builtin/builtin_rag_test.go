//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package builtin

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/rag"
	"github.com/choria-io/fisk-ai/internal/toolkit/functool"
)

var _ = Describe("knowledge_search tool", func() {
	ctx := context.Background()

	disabled := &config.Config{}
	enabled := func(dir string) *config.Config {
		return &config.Config{Identity: "test", Harness: config.HarnessConfig{RAG: &config.RAGConfig{Enabled: true, Directory: dir}}}
	}

	It("returns no tools when RAG is disabled", func() {
		Expect(RAGTools(disabled, nil)).To(BeNil())
		Expect(RAGSystemNote(disabled)).To(Equal(""))
	})

	It("returns an error when invoked with a nil store", func() {
		tools := RAGTools(enabled(""), nil)
		Expect(tools).To(HaveLen(1))
		_, err := callTool(tools[0], ctx, json.RawMessage(`{"query":"x"}`), nil)
		Expect(err).To(MatchError(errRAGStoreUnconfigured))
	})

	It("renders a sanitized trace line", func() {
		Expect(knowledgeSearchTrace(json.RawMessage(`{"query":"how does it work"}`))).To(Equal(`knowledge_search("how does it work")`))
		Expect(knowledgeSearchTrace(json.RawMessage(`{"query":"q","top_k":3}`))).To(Equal(`knowledge_search("q", top_k=3)`))
	})

	Describe("mcpSelectedBuiltins", func() {
		exposing := func(builtins ...string) *config.Config {
			return &config.Config{
				Harness: config.HarnessConfig{RAG: &config.RAGConfig{Enabled: true}},
				Expose:  &config.ExposeConfig{Agent: &config.AgentExpose{MCP: &config.ExposedMCPConfig{Builtins: builtins}}},
			}
		}

		// A tool added to a built-in set alongside an allowlisted one must not reach
		// MCP clients on the strength of its neighbour's selection; without this the
		// next tool appended to RAGTools is served with no config change and no error.
		It("serves only the tools the operator named, not the whole set", func() {
			cfg := exposing(knowledgeSearchName)
			// Capability is satisfied, so this isolates the selection gate: a tool the
			// operator did not name stays unserved even when it may be served.
			second := mustNew(functool.Spec{
				Name:        "knowledge_enumerate",
				Description: "a second tool sharing the knowledge set",
				Schema:      map[string]any{"type": "object"},
				Expose:      &functool.ExposeSpec{MCP: true},
				Handler:     func(context.Context, json.RawMessage, *functool.CallContext) (string, error) { return "{}", nil },
			})

			out := mcpSelectedBuiltins(cfg, append(RAGTools(cfg, nil), second))
			Expect(out).To(HaveLen(1))
			Expect(out[0].Name()).To(Equal(knowledgeSearchName))
		})

		It("serves nothing when the operator named nothing", func() {
			cfg := exposing()
			Expect(mcpSelectedBuiltins(cfg, RAGTools(cfg, nil))).To(BeEmpty())
		})

		// The subset property is what MCPKnowledgeBuiltins must hold for any future
		// RAGTools, so this fails the moment a second tool is added and the filter has
		// been bypassed, which the isolated filter test above cannot catch.
		It("returns nothing beyond the allowlist from MCPKnowledgeBuiltins", func() {
			cfg := exposing(knowledgeSearchName)
			cfg.Harness.RAG.Directory = filepath.Join(GinkgoT().TempDir(), "knowledge")

			tools, store, err := MCPKnowledgeBuiltins(ctx, cfg, io.Discard)
			Expect(err).ToNot(HaveOccurred())
			Expect(store).ToNot(BeNil())
			DeferCleanup(store.Close)

			Expect(tools).ToNot(BeEmpty())
			for _, t := range tools {
				Expect(cfg.MCPBuiltins()).To(ContainElement(t.Name()))
			}
		})
	})

	Describe("capHits", func() {
		It("always includes the first hit and stops once the budget is exceeded", func() {
			hits := []rag.Hit{
				{Citation: "a#0", Content: "aaaaaaaaaa"},
				{Citation: "b#0", Content: "bbbbbbbbbb"},
			}
			// Budget of 1 token ~ 4 chars, far below the first hit's size.
			out := capHits(hits, 1)
			Expect(out).To(HaveLen(1))
			Expect(out[0].Citation).To(Equal("a#0"))
		})

		It("includes all hits when the budget is ample", func() {
			hits := []rag.Hit{{Citation: "a#0", Content: "x"}, {Citation: "b#0", Content: "y"}}
			Expect(capHits(hits, 1000)).To(HaveLen(2))
		})
	})

	Describe("against a real lexical store", func() {
		var (
			tmp   string
			cfg   *config.Config
			tools []*functool.Tool
		)

		buildIndex := func() {
			docs := filepath.Join(tmp, "docs")
			Expect(os.MkdirAll(docs, 0o755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(docs, "note.md"), []byte("# Sharding\n\nKeys are hashed to shards for horizontal scale.\n"), 0o644)).To(Succeed())

			w, err := rag.OpenWriter(cfg, "")
			Expect(err).ToNot(HaveOccurred())
			_, err = w.Index(ctx, []string{docs}, rag.IndexOptions{Reconcile: true})
			Expect(err).ToNot(HaveOccurred())
			w.Close()
		}

		open := func() {
			store, err := rag.Open(cfg, "")
			Expect(err).ToNot(HaveOccurred())
			DeferCleanup(store.Close)
			tools = RAGTools(cfg, store)
		}

		BeforeEach(func() {
			tmp = GinkgoT().TempDir()
			cfg = enabled(filepath.Join(tmp, "knowledge"))
		})

		It("reports index_not_built before any index exists", func() {
			open()
			out, err := callTool(tools[0], ctx, json.RawMessage(`{"query":"anything"}`), nil)
			Expect(err).ToNot(HaveOccurred())

			var res knowledgeSearchOutcome
			Expect(json.Unmarshal([]byte(out), &res)).To(Succeed())
			Expect(res.Status).To(Equal(string(rag.StatusIndexNotBuilt)))
			Expect(res.Tier).To(ContainSubstring("lexical"))
		})

		It("returns cited results for a query", func() {
			buildIndex()
			open()

			out, err := callTool(tools[0], ctx, json.RawMessage(`{"query":"sharding horizontal scale"}`), nil)
			Expect(err).ToNot(HaveOccurred())

			var res knowledgeSearchOutcome
			Expect(json.Unmarshal([]byte(out), &res)).To(Succeed())
			Expect(res.Status).To(Equal(string(rag.StatusOK)))
			Expect(res.Results).ToNot(BeEmpty())
			Expect(res.Results[0].Citation).To(ContainSubstring("note.md#"))
			Expect(res.Results[0].Content).To(ContainSubstring("shards"))
		})
	})
})
