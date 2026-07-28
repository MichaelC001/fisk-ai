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

// ragToolNamed picks one tool out of a built-in set, so specs about a single tool
// do not encode how many others sit beside it.
func ragToolNamed(tools []*functool.Tool, name string) *functool.Tool {
	GinkgoHelper()

	for _, t := range tools {
		if t.Name() == name {
			return t
		}
	}

	Fail("no tool named " + name + " in the set")
	return nil
}

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

	// The agent path has no allowlist: internal/agent takes everything RAGTools
	// returns, so anything added here reaches the model the moment knowledge is
	// enabled. This pins the set so that addition is a deliberate edit to a spec
	// rather than a line nobody reviews.
	//
	// The name that must stay off this list is the vocabulary listing. `knowledge
	// words` dumps every word in the corpus with its frequencies, which turns
	// retrieval from something that needs a guessed word into exhaustive extraction:
	// wildcards are refused and related-forms names at most five words, and a
	// vocabulary tool removes both limits at once, handing any caller (including one
	// following injected instructions) the identifiers that make the rest of the
	// corpus searchable. It is also thousands of tokens of low signal for a model that
	// asked a question. It stays a CLI verb, which the agent cannot reach.
	It("offers the model exactly these tools", func() {
		var names []string
		for _, t := range RAGTools(enabled(""), nil) {
			names = append(names, t.Name())
		}

		Expect(names).To(ConsistOf(knowledgeSearchName, knowledgeEnumerateName))
	})

	It("returns an error when invoked with a nil store", func() {
		tools := RAGTools(enabled(""), nil)
		Expect(tools).To(HaveLen(2))
		for _, t := range tools {
			_, err := callTool(t, ctx, json.RawMessage(`{"query":"x"}`), nil)
			Expect(err).To(MatchError(errRAGStoreUnconfigured))
		}
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
		// MCP clients on the strength of its neighbour's selection. The two real
		// knowledge tools cover the case where config knows both names; this covers a
		// future tool it does not, which would otherwise be served with no config
		// change and no error.
		It("serves only the tools the operator named, not the whole set", func() {
			cfg := exposing(knowledgeSearchName)
			// Capability is satisfied, so this isolates the selection gate: a tool the
			// operator did not name stays unserved even when it may be served.
			second := mustNew(functool.Spec{
				Name:        "knowledge_extra",
				Description: "a tool sharing the knowledge set that config does not name",
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

		// The store gate asks about the knowledge group, not one name. Gating it on
		// knowledge_search would open no store for this operator and serve them
		// nothing, with the allowlist they wrote having been accepted at load.
		It("opens the store for an enumerate-only allowlist", func() {
			cfg := exposing(knowledgeEnumerateName)
			cfg.Harness.RAG.Directory = filepath.Join(GinkgoT().TempDir(), "knowledge")

			tools, store, err := MCPKnowledgeBuiltins(ctx, cfg, io.Discard)
			Expect(err).ToNot(HaveOccurred())
			Expect(store).ToNot(BeNil())
			DeferCleanup(store.Close)

			Expect(tools).To(HaveLen(1))
			Expect(tools[0].Name()).To(Equal(knowledgeEnumerateName))
		})

		It("opens no store and serves nothing when neither is allowlisted", func() {
			cfg := exposing()
			cfg.Harness.RAG.Directory = filepath.Join(GinkgoT().TempDir(), "knowledge")

			tools, store, err := MCPKnowledgeBuiltins(ctx, cfg, io.Discard)
			Expect(err).ToNot(HaveOccurred())
			Expect(store).To(BeNil())
			Expect(tools).To(BeEmpty())
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
			out, err := callTool(ragToolNamed(tools, knowledgeSearchName), ctx, json.RawMessage(`{"query":"anything"}`), nil)
			Expect(err).ToNot(HaveOccurred())

			var res knowledgeSearchOutcome
			Expect(json.Unmarshal([]byte(out), &res)).To(Succeed())
			Expect(res.Status).To(Equal(string(rag.StatusIndexNotBuilt)))
			Expect(res.Tier).To(ContainSubstring("lexical"))
		})

		It("returns cited results for a query", func() {
			buildIndex()
			open()

			out, err := callTool(ragToolNamed(tools, knowledgeSearchName), ctx, json.RawMessage(`{"query":"sharding horizontal scale"}`), nil)
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
