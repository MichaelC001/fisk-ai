//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/rag"
	"github.com/choria-io/fisk-ai/internal/toolkit/functool"
)

var _ = Describe("knowledge_read tool", func() {
	ctx := context.Background()

	knowledge := func(dir string, read bool) *config.Config {
		return &config.Config{
			Identity: "test",
			Harness: config.HarnessConfig{
				RAG: &config.RAGConfig{Enabled: true, Directory: dir, ReadTool: read},
			},
		}
	}

	// The key is the whole gate: knowledge alone offers the model search and
	// enumerate, and an operator who did not ask for reading is never handed a tool
	// that returns a document a section at a time.
	It("is absent from the toolkit until the operator sets read_tool", func() {
		var names []string
		for _, t := range RAGTools(knowledge("", false), nil) {
			names = append(names, t.Name())
		}
		Expect(names).To(ConsistOf(knowledgeSearchName, knowledgeEnumerateName))

		names = nil
		for _, t := range RAGTools(knowledge("", true), nil) {
			names = append(names, t.Name())
		}
		Expect(names).To(ConsistOf(knowledgeSearchName, knowledgeEnumerateName, knowledgeReadName))
	})

	It("declares MCP exposure, not a2a, and is nameable in config", func() {
		cfg := knowledge("", true)
		tool := ragToolNamed(RAGTools(cfg, nil), knowledgeReadName)

		Expect(tool.MCPExposable()).To(BeTrue())
		Expect(tool.A2AExposable()).To(BeFalse())
		Expect(WithheldFromMCP(cfg)).ToNot(ContainElement(knowledgeReadName))
		Expect(WithheldFromA2A(cfg)).To(ContainElement(knowledgeReadName))

		exposed := knowledge("", true)
		exposed.Expose = &config.ExposeConfig{Agent: &config.AgentExpose{MCP: &config.ExposedMCPConfig{Port: 8080, Builtins: []string{knowledgeReadName}}}}
		Expect(exposed.Prepare()).To(Succeed())
		Expect(exposed.MCPBuiltins()).To(ContainElement(knowledgeReadName))
	})

	It("returns an error when invoked with a nil store", func() {
		tool := ragToolNamed(RAGTools(knowledge("", true), nil), knowledgeReadName)
		_, err := callTool(tool, ctx, json.RawMessage(`{"index_ref":"docs/a.md#0"}`), nil)
		Expect(err).To(MatchError(errRAGStoreUnconfigured))
	})

	It("renders a sanitized trace line", func() {
		Expect(knowledgeReadTrace(json.RawMessage(`{"index_ref":"docs/a.md#3"}`))).To(Equal(`knowledge_read("docs/a.md#3")`))
		Expect(knowledgeReadTrace(json.RawMessage(`{"index_ref":"docs/a.md#3","before":1,"after":2}`))).To(Equal(`knowledge_read("docs/a.md#3", before=1, after=2)`))
	})

	// The model is told a mapped citation is quoted rather than fetched, which leaves
	// an agent with no file reader nowhere to go for the rest of a document. The note
	// names this tool only where the operator turned it on.
	It("is offered in the system note only when it is enabled", func() {
		Expect(RAGSystemNote(knowledge("", false))).ToNot(ContainSubstring(knowledgeReadName))

		note := RAGSystemNote(knowledge("", true))
		Expect(note).To(ContainSubstring("rather than fetching it"))
		Expect(note).To(ContainSubstring("call knowledge_read with a result's index_ref"))
		Expect(note).To(ContainSubstring("before and after arguments"))
	})

	Describe("against a real lexical store", func() {
		var (
			tmp  string
			cfg  *config.Config
			tool *functool.Tool
			path string
		)

		// Three sections of one document, so a read of the middle one with a neighbor
		// either side has to cross two heading boundaries to answer.
		doc := "# Guide\n\n## One\n\nKeys are hashed to shards.\n\n## Two\n\nShards are replicated.\n\n## Three\n\nReplicas fail over.\n"

		buildIndex := func() {
			GinkgoHelper()

			docs := filepath.Join(tmp, "docs")
			Expect(os.MkdirAll(docs, 0o755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(docs, "note.md"), []byte(doc), 0o644)).To(Succeed())
			Expect(rag.ChunkDocument(doc)).To(HaveLen(3))

			w, err := rag.OpenWriter(cfg, "", rag.Options{})
			Expect(err).ToNot(HaveOccurred())
			_, err = w.Index(ctx, []string{docs}, rag.IndexOptions{Reconcile: true})
			Expect(err).ToNot(HaveOccurred())
			w.Close()

			path = filepath.ToSlash(filepath.Join(docs, "note.md"))
		}

		open := func() {
			GinkgoHelper()

			store, err := rag.Open(cfg, "", rag.Options{})
			Expect(err).ToNot(HaveOccurred())
			DeferCleanup(store.Close)
			tool = ragToolNamed(RAGTools(cfg, store), knowledgeReadName)
		}

		call := func(input string) knowledgeReadOutcome {
			GinkgoHelper()

			out, err := callTool(tool, ctx, json.RawMessage(input), nil)
			Expect(err).ToNot(HaveOccurred())

			var res knowledgeReadOutcome
			Expect(json.Unmarshal([]byte(out), &res)).To(Succeed())

			return res
		}

		BeforeEach(func() {
			tmp = GinkgoT().TempDir()
			cfg = knowledge(filepath.Join(tmp, "knowledge"), true)
		})

		It("reports index_not_built before any index exists", func() {
			open()
			res := call(`{"index_ref":"docs/note.md#0"}`)
			Expect(res.Status).To(Equal(knowledgeReadIndexNotBuilt))
			Expect(res.Content).To(BeEmpty())
		})

		It("returns the cited section with its citation pair and path", func() {
			buildIndex()
			open()

			res := call(`{"index_ref":"` + path + `#1"}`)
			Expect(res.Status).To(Equal(knowledgeReadOK))
			Expect(res.Content).To(Equal("Shards are replicated."))
			Expect(res.IndexRef).To(Equal(path + "#1"))
			Expect(res.Citation).To(Equal(path + "#1"))
			Expect(res.Path).To(HaveSuffix(filepath.Join("docs", "note.md")))
			Expect(res.Section).To(Equal("Guide > Two"))
			Expect(res.Span).To(BeEmpty())
			Expect(res.DocumentSections).To(Equal(3))
			Expect(res.Note).To(BeEmpty())
		})

		It("crosses heading boundaries for the sections either side", func() {
			buildIndex()
			open()

			res := call(`{"index_ref":"` + path + `#1","before":1,"after":1}`)
			Expect(res.Span).To(Equal(path + "#0..#2"))
			Expect(res.Content).To(ContainSubstring("hashed to shards"))
			Expect(res.Content).To(ContainSubstring("fail over"))
		})

		// A block that runs to the edge of the document is narrower than the call asked
		// for, which the text itself does not show.
		It("says the document ended where a read asked for more", func() {
			buildIndex()
			open()

			res := call(`{"index_ref":"` + path + `#0","before":2,"after":5}`)
			Expect(res.Span).To(Equal(path + "#0..#2"))
			Expect(res.Note).To(ContainSubstring("the document holds 3 sections"))
		})

		// The cap affords no neighbor at all, so the block is the cited section and
		// there is no span to read on from. The note has to say that rather than send
		// the model back for text it already has.
		It("says the cited section fills the cap by itself", func() {
			cfg.Harness.RAG.MaxInjectedTokens = 1
			buildIndex()
			open()

			res := call(`{"index_ref":"` + path + `#1","before":1,"after":1}`)
			Expect(res.Status).To(Equal(knowledgeReadOK))
			Expect(res.Content).To(Equal("Shards are replicated."))
			Expect(res.Span).To(BeEmpty())
			Expect(res.Note).To(ContainSubstring("fills the limit on one call by itself"))
		})

		It("reports a citation the index does not hold as not_found", func() {
			buildIndex()
			open()

			res := call(`{"index_ref":"` + path + `#9"}`)
			Expect(res.Status).To(Equal(knowledgeReadNotFound))
			Expect(res.Content).To(BeEmpty())
			Expect(res.Note).To(ContainSubstring("search again"))

			res = call(`{"index_ref":"docs/never-indexed.md#0"}`)
			Expect(res.Status).To(Equal(knowledgeReadNotFound))
		})

		// The model wrote the argument, so the failure has to name the form that would
		// have worked rather than come back as an empty answer about the corpus.
		It("errors on a token that is not a citation", func() {
			buildIndex()
			open()

			_, err := callTool(tool, ctx, json.RawMessage(`{"index_ref":"docs/note.md"}`), nil)
			Expect(err).To(MatchError(ContainSubstring("<relpath>#<ordinal>")))
		})
	})
})
