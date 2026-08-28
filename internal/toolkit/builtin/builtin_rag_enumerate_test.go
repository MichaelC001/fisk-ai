//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/rag"
	"github.com/choria-io/fisk-ai/internal/toolkit/functool"
)

var _ = Describe("knowledge_enumerate tool", func() {
	ctx := context.Background()

	enabled := func(dir string) *config.Config {
		return &config.Config{Identity: "test", Harness: config.HarnessConfig{RAG: &config.RAGConfig{Enabled: true, Directory: dir}}}
	}

	rows := func(n int, citation string) []knowledgeEnumDocJSON {
		out := make([]knowledgeEnumDocJSON, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, knowledgeEnumDocJSON{
				Citation:       citation,
				IndexRef:       "docs/some/typical/path.md#12",
				BodyMatches:    3,
				HeadingMatches: 1,
				TotalChunks:    9,
			})
		}

		return out
	}

	Describe("exposure", func() {
		cfg := enabled("")

		exposing := func(builtins ...string) *config.Config {
			out := enabled("")
			out.Expose = &config.ExposeConfig{Agent: &config.AgentExpose{MCP: &config.ExposedMCPConfig{Builtins: builtins}}}
			return out
		}

		names := func(tools []*functool.Tool) []string {
			var out []string
			for _, t := range tools {
				out = append(out, t.Name())
			}
			return out
		}

		It("declares MCP exposure and not a2a", func() {
			tool := ragToolNamed(RAGTools(cfg, nil), knowledgeEnumerateName)
			Expect(tool.MCPExposable()).To(BeTrue())
			Expect(tool.A2AExposable()).To(BeFalse())
		})

		// The withheld lists are what the operator is shown, so a tool that may be
		// served has to be absent from them and one that may not has to be named.
		It("is reported as withheld from a2a only", func() {
			Expect(WithheldFromA2A(cfg)).To(ContainElement(knowledgeEnumerateName))
			Expect(WithheldFromMCP(cfg)).ToNot(ContainElement(knowledgeEnumerateName))
			Expect(WithheldFromMCP(cfg)).ToNot(ContainElement(knowledgeSearchName))
		})

		// A tool that declares MCP exposure but that config will not accept in the
		// allowlist is selectable by no operator and served to nobody. Adding one has
		// to move both halves, and this is what fails if only the spec moves.
		It("keeps every MCP-exposable knowledge tool nameable in config", func() {
			nameable := []string{config.KnowledgeSearchToolName, config.KnowledgeEnumerateToolName}
			for _, t := range RAGTools(cfg, nil) {
				if t.MCPExposable() {
					Expect(nameable).To(ContainElement(t.Name()))
				}
			}
		})

		It("is served when the operator names it", func() {
			out := mcpSelectedBuiltins(exposing(knowledgeSearchName, knowledgeEnumerateName), RAGTools(cfg, nil))
			Expect(names(out)).To(ConsistOf(knowledgeSearchName, knowledgeEnumerateName))
		})

		// The per-tool filter now has two genuinely servable tools to separate, which
		// is a stronger test of it than a tool that could not be served either way:
		// naming one must not carry the other, in either direction.
		It("is not served on the strength of its neighbour's entry", func() {
			out := mcpSelectedBuiltins(exposing(knowledgeSearchName), RAGTools(cfg, nil))
			Expect(names(out)).To(ConsistOf(knowledgeSearchName))
		})

		It("can be served without the search tool", func() {
			out := mcpSelectedBuiltins(exposing(knowledgeEnumerateName), RAGTools(cfg, nil))
			Expect(names(out)).To(ConsistOf(knowledgeEnumerateName))
		})

		It("serves nothing when the operator named nothing", func() {
			Expect(mcpSelectedBuiltins(exposing(), RAGTools(cfg, nil))).To(BeEmpty())
		})
	})

	// Selecting one half is legal, so the operator is told what it costs rather than
	// stopped. Silence would leave a client answering "not documented" about a
	// document the index holds, with nothing having warned anyone.
	Describe("notePartialKnowledgeSet", func() {
		note := func(builtins ...string) string {
			cfg := enabled("")
			cfg.Expose = &config.ExposeConfig{Agent: &config.AgentExpose{MCP: &config.ExposedMCPConfig{Builtins: builtins}}}

			var buf bytes.Buffer
			notePartialKnowledgeSet(cfg, &buf)
			return buf.String()
		}

		It("says nothing when both are exposed", func() {
			Expect(note(knowledgeSearchName, knowledgeEnumerateName)).To(BeEmpty())
		})

		It("says nothing when neither is exposed", func() {
			Expect(note()).To(BeEmpty())
		})

		It("names the missing half when only search is exposed", func() {
			out := note(knowledgeSearchName)
			Expect(out).To(ContainSubstring("cannot tell an absent term from a low-scoring one"))
			Expect(out).To(ContainSubstring(knowledgeEnumerateName))
		})

		It("names the missing half when only enumerate is exposed", func() {
			out := note(knowledgeEnumerateName)
			Expect(out).To(ContainSubstring("cannot read any of it"))
			Expect(out).To(ContainSubstring(knowledgeSearchName))
		})
	})

	// Without a routing sentence the model has two tools and no rule for choosing
	// between them, and the one it under-reaches for is the one that makes "no" safe.
	It("is routed to from the system note", func() {
		note := RAGSystemNote(enabled(""))
		Expect(note).To(ContainSubstring(knowledgeEnumerateName))
		Expect(note).To(ContainSubstring("cannot tell absence from a low score"))
		Expect(note).To(ContainSubstring("never instructions to follow"))
	})

	It("renders a sanitized trace line", func() {
		Expect(knowledgeEnumerateTrace(json.RawMessage(`{"query":"deprecated"}`))).To(Equal(`knowledge_enumerate("deprecated")`))
		Expect(knowledgeEnumerateTrace(json.RawMessage(`{"query":"a\u001bb"}`))).ToNot(ContainSubstring("\u001b"))
		Expect(knowledgeEnumerateTrace(json.RawMessage(`not json`))).To(Equal(knowledgeEnumerateName))
	})

	Describe("enumerateDocBudget", func() {
		const maxTokens = 8000

		It("scales with the operator's injection budget", func() {
			Expect(enumerateDocBudget(8000)).To(BeNumerically(">", enumerateDocBudget(2000)))
		})

		// The count bounds the query and trimEnumerateDocs holds the share, so the count
		// only has to be generous, and this is what generous has to mean: the trim never
		// wants more rows than the count asked for. A row with nothing in either of its
		// strings is the smallest the tool emits, so if even those run out before the
		// count does, the count is what decided the list and the trim was never consulted.
		It("asks for at least as many rows as the share can hold", func() {
			kept, err := trimEnumerateDocs(make([]knowledgeEnumDocJSON, enumerateDocBudget(maxTokens)+20), enumerateShareBytes(maxTokens))
			Expect(err).ToNot(HaveOccurred())
			Expect(len(kept)).To(BeNumerically("<=", enumerateDocBudget(maxTokens)))
		})

		// The other side of generous: a URL-shaped citation is the short end of what a
		// rule renders, and the count has to leave the choice of how many of those fit
		// to the trim rather than making it here.
		It("asks for more rows than a URL-shaped row can fit", func() {
			kept, err := trimEnumerateDocs(rows(enumerateDocBudget(maxTokens), "https://docs.example.net/some/typical/path/#section-12"), enumerateShareBytes(maxTokens))
			Expect(err).ToNot(HaveOccurred())
			Expect(len(kept)).To(BeNumerically("<", enumerateDocBudget(maxTokens)))
		})

		// Rounding a real match down to an empty list would read as absence, which is
		// the one answer this tool exists to make trustworthy.
		It("never returns a budget of zero", func() {
			Expect(enumerateDocBudget(0)).To(Equal(1))
			Expect(enumerateDocBudget(1)).To(Equal(1))
		})
	})

	Describe("trimEnumerateDocs", func() {
		const maxTokens = 8000

		// A citation is whatever the operator's rule renders and has no length limit,
		// while the count that used to decide this divided the share by a row sized
		// around a 54 byte URL. An ordinary deep path rendered to a URL is already half
		// as long again as that, and a longer one runs to several times the share.
		DescribeTable("keeps the marshaled list inside the share", func(citationLen int, atMost int) {
			kept, err := trimEnumerateDocs(rows(400, strings.Repeat("a", citationLen)), enumerateShareBytes(maxTokens))
			Expect(err).ToNot(HaveOccurred())
			Expect(kept).ToNot(BeEmpty())
			Expect(len(kept)).To(BeNumerically("<=", atMost))

			listed, err := json.Marshal(kept)
			Expect(err).ToNot(HaveOccurred())
			Expect(len(listed)).To(BeNumerically("<=", enumerateShareBytes(maxTokens)))
		},
			Entry("a deep path rendered to a URL", 103, 40),
			Entry("a citation twice that length", 200, 26),
			Entry("a citation long enough that a handful of rows fill the share", 1500, 5),
		)

		It("leaves a list that already fits alone", func() {
			kept, err := trimEnumerateDocs(rows(5, "docs/some/typical/path.md#12"), enumerateShareBytes(maxTokens))
			Expect(err).ToNot(HaveOccurred())
			Expect(kept).To(HaveLen(5))
		})

		// A document that matched and is not listed reads as absence, so the first row
		// is kept whatever it costs.
		It("returns the single matching row even when it alone exceeds the share", func() {
			kept, err := trimEnumerateDocs(rows(1, strings.Repeat("a", 4000)), enumerateShareBytes(1000))
			Expect(err).ToNot(HaveOccurred())
			Expect(kept).To(HaveLen(1))

			listed, err := json.Marshal(kept)
			Expect(err).ToNot(HaveOccurred())
			Expect(len(listed)).To(BeNumerically(">", enumerateShareBytes(1000)))
		})
	})

	Describe("enumerateNote", func() {
		It("says an unbuilt or empty index is not an answer about the documents", func() {
			for _, s := range []rag.EnumerateStatus{rag.EnumIndexNotBuilt, rag.EnumCorpusEmpty} {
				note := enumerateNote(&rag.EnumerateResult{Status: s})
				Expect(note).To(ContainSubstring("not an answer about the operator's documents"))
			}
		})

		It("explains a query with nothing searchable in it", func() {
			note := enumerateNote(&rag.EnumerateResult{Status: rag.EnumQueryEmpty})
			Expect(note).To(ContainSubstring("nothing was searched for"))
		})

		// A model does not read the absence of a warning as a signal, so a complete
		// set has to declare itself complete as plainly as a truncated one declares
		// itself partial.
		It("states completeness when the whole set is returned", func() {
			note := enumerateNote(&rag.EnumerateResult{Status: rag.EnumOK, Matched: 3, Returned: 3, IndexedDocuments: 40})
			Expect(note).To(ContainSubstring("complete set"))
			Expect(note).To(ContainSubstring("all 3 matching documents of 40"))
		})

		It("marks a zero result as a genuine absence rather than a cutoff", func() {
			note := enumerateNote(&rag.EnumerateResult{Status: rag.EnumOK, Matched: 0, IndexedDocuments: 40})
			Expect(note).To(ContainSubstring("not a ranking cutoff"))
			Expect(note).To(ContainSubstring("index of 40"))
		})

		It("says plainly when the list was cut short", func() {
			note := enumerateNote(&rag.EnumerateResult{Status: rag.EnumOK, Matched: 90, Returned: 10, Truncated: true, IndexedDocuments: 200})
			Expect(note).To(ContainSubstring("90 documents match"))
			Expect(note).To(ContainSubstring("not the whole set"))
			Expect(note).ToNot(ContainSubstring("complete set"))
		})
	})

	Describe("enumerateStemNotes", func() {
		notesFor := func(t rag.TermReport) []string {
			return enumerateStemNotes(&rag.EnumerateResult{Terms: []rag.TermReport{t}})
		}

		It("says a dropped term was never searched for", func() {
			Expect(notesFor(rag.TermReport{Surface: "of", Dropped: true})[0]).To(ContainSubstring("too short"))
		})

		It("says a term absent in every form is absent", func() {
			Expect(notesFor(rag.TermReport{Surface: "kafka"})[0]).To(ContainSubstring("in any form"))
		})

		It("names the related forms behind a count larger than the literal one", func() {
			note := notesFor(rag.TermReport{Surface: "deprecated", Docs: 9, Literal: 4, Related: []string{"deprecation", "deprecate"}})[0]
			Expect(note).To(ContainSubstring("deprecation, deprecate"))
			Expect(note).To(ContainSubstring("4 contain it as written"))
		})

		It("still explains the gap when no related form could be named", func() {
			note := notesFor(rag.TermReport{Surface: "deprecated", Docs: 9, Literal: 4})[0]
			Expect(note).To(ContainSubstring("or a related form"))
		})

		It("stays silent when the counts agree", func() {
			Expect(notesFor(rag.TermReport{Surface: "sharding", Docs: 4, Literal: 4})).To(BeEmpty())
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
			Expect(os.WriteFile(filepath.Join(docs, "shard.md"), []byte("# Sharding\n\nKeys are hashed to shards for horizontal scale.\n"), 0o644)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(docs, "retire.md"), []byte("# Retirement\n\nThe old endpoint is deprecated and will be removed.\n"), 0o644)).To(Succeed())

			w, err := rag.OpenWriter(cfg, "")
			Expect(err).ToNot(HaveOccurred())
			_, err = w.Index(ctx, []string{docs}, rag.IndexOptions{Reconcile: true})
			Expect(err).ToNot(HaveOccurred())
			w.Close()
		}

		buildManyIndex := func(n int) {
			GinkgoHelper()

			docs := filepath.Join(tmp, "docs")
			Expect(os.MkdirAll(docs, 0o755)).To(Succeed())
			for i := 0; i < n; i++ {
				body := fmt.Sprintf("# Retention %d\n\nThe retention policy for shard %d is written down here.\n", i, i)
				Expect(os.WriteFile(filepath.Join(docs, fmt.Sprintf("policy-%02d.md", i)), []byte(body), 0o644)).To(Succeed())
			}

			w, err := rag.OpenWriter(cfg, "")
			Expect(err).ToNot(HaveOccurred())
			_, err = w.Index(ctx, []string{docs}, rag.IndexOptions{Reconcile: true})
			Expect(err).ToNot(HaveOccurred())
			w.Close()
		}

		call := func(query string) knowledgeEnumerateOutcome {
			GinkgoHelper()

			store, err := rag.Open(cfg, "")
			Expect(err).ToNot(HaveOccurred())
			DeferCleanup(store.Close)
			tools = RAGTools(cfg, store)

			body, err := callTool(ragToolNamed(tools, knowledgeEnumerateName), ctx, json.RawMessage(`{"query":`+strconv.Quote(query)+`}`), nil)
			Expect(err).ToNot(HaveOccurred())

			var out knowledgeEnumerateOutcome
			Expect(json.Unmarshal([]byte(body), &out)).To(Succeed())
			return out
		}

		BeforeEach(func() {
			tmp = GinkgoT().TempDir()
			cfg = enabled(filepath.Join(tmp, "knowledge"))
		})

		It("reports index_not_built before any index exists", func() {
			out := call("anything")
			Expect(out.Status).To(Equal(string(rag.EnumIndexNotBuilt)))
			Expect(out.Note).ToNot(BeEmpty())
			Expect(out.Documents).To(BeEmpty())
		})

		It("returns the matching documents with counts and no text", func() {
			buildIndex()

			out := call("shards")
			Expect(out.Status).To(Equal(string(rag.EnumOK)))
			Expect(out.Tier).To(Equal(rag.EnumerateTierLine))
			Expect(out.Matched).To(Equal(1))
			Expect(out.Returned).To(Equal(1))
			Expect(out.Truncated).To(BeFalse())
			Expect(out.Indexed).To(Equal(2))
			Expect(out.Documents).To(HaveLen(1))
			Expect(out.Documents[0].Citation).To(ContainSubstring("shard.md#"))
			Expect(out.Documents[0].BodyMatches).To(BeNumerically(">", 0))
			Expect(out.Note).To(ContainSubstring("complete set"))
		})

		It("cites a mapped document by the operator's rule and keeps the index key in index_ref", func() {
			// Unanchored because the indexer stores the absolute path it walked.
			cfg.Harness.RAG.Citations = []config.RAGCitationRule{{
				Pattern:         `docs/(.*)\.md$`,
				Replace:         "https://docs.example.net/$1",
				PatternCompiled: regexp.MustCompile(`docs/(.*)\.md$`),
			}}
			buildIndex()

			out := call("shards")
			Expect(out.Documents).To(HaveLen(1))
			Expect(out.Documents[0].Citation).To(Equal("https://docs.example.net/shard"))
			Expect(out.Documents[0].IndexRef).To(ContainSubstring("shard.md#"))
		})

		// A corpus that is published nowhere is the default, and the model is told to
		// cite the citation field whatever the operator configured, so that field has to
		// hold something citable even then.
		It("puts the index key in both fields for a document no rule matches", func() {
			buildIndex()

			out := call("shards")
			Expect(out.Documents).To(HaveLen(1))
			Expect(out.Documents[0].Citation).To(ContainSubstring("shard.md#"))
			Expect(out.Documents[0].Citation).To(Equal(out.Documents[0].IndexRef))
		})

		// The empty answer is the reason the tool exists, so it has to arrive with the
		// note that makes it safe to act on rather than as a bare empty list.
		It("returns an empty set that says it is complete", func() {
			buildIndex()

			out := call("kafka")
			Expect(out.Status).To(Equal(string(rag.EnumOK)))
			Expect(out.Matched).To(Equal(0))
			Expect(out.Documents).To(BeEmpty())
			Expect(out.Note).To(ContainSubstring("not a ranking cutoff"))
			Expect(out.Terms).To(HaveLen(1))
			Expect(out.Terms[0].Term).To(Equal("kafka"))
			Expect(out.Terms[0].Documents).To(Equal(0))
		})

		It("requires every word of a multi-word query in the same document", func() {
			buildIndex()

			Expect(call("shards deprecated").Matched).To(Equal(0))
			Expect(call("shards hashed").Matched).To(Equal(1))
		})

		It("reports stem reach so a model can tell absent from spelled differently", func() {
			buildIndex()

			out := call("deprecate")
			Expect(out.Matched).To(Equal(1))
			Expect(out.Terms).To(HaveLen(1))
			Expect(out.Terms[0].Documents).To(Equal(1))
			// "deprecate" is not written anywhere; only "deprecated" is.
			Expect(out.Terms[0].AsWritten).To(Equal(0))
			Expect(out.Note).To(ContainSubstring("related form"))
		})

		// The share is spent on rows whose citations the operator's rule renders, and a
		// rule can render anything. These measure the list the tool marshals rather than
		// a count derived from a sample citation.
		Describe("a corpus with long mapped citations", func() {
			const maxTokens = 1000

			mappedCall := func(pad int) knowledgeEnumerateOutcome {
				GinkgoHelper()

				cfg.Harness.RAG.MaxInjectedTokens = maxTokens
				cfg.Harness.RAG.Citations = []config.RAGCitationRule{{
					Pattern:         `docs/(.*)\.md$`,
					Replace:         "https://docs.example.net/" + strings.Repeat("x", pad) + "/$1",
					PatternCompiled: regexp.MustCompile(`docs/(.*)\.md$`),
				}}
				buildManyIndex(8)

				return call("retention")
			}

			DescribeTable("marshals its documents within its share of the injection budget", func(pad int) {
				out := mappedCall(pad)
				Expect(out.Matched).To(Equal(8))
				Expect(out.Documents).ToNot(BeEmpty())

				listed, err := json.Marshal(out.Documents)
				Expect(err).ToNot(HaveOccurred())
				Expect(len(listed)).To(BeNumerically("<=", enumerateShareBytes(maxTokens)))
			},
				Entry("a deep path rendered to a URL", 68),
				Entry("a citation twice that length", 165),
				Entry("a citation long enough that one row fills the share", 465),
			)

			// The description tells the model to always read the note, so a list the
			// trim shortened and the note still calls complete is the one error it has no
			// way to catch.
			It("announces a shortened list as truncated and counts the rows it returned", func() {
				out := mappedCall(68)

				Expect(out.Matched).To(Equal(8))
				Expect(out.Truncated).To(BeTrue())
				Expect(out.Returned).To(Equal(len(out.Documents)))
				Expect(out.Returned).To(BeNumerically("<", out.Matched))
				Expect(out.Note).To(ContainSubstring("8 documents match"))
				Expect(out.Note).To(ContainSubstring(fmt.Sprintf("the %d listed here", out.Returned)))
				Expect(out.Note).To(ContainSubstring("not the whole set"))
				Expect(out.Note).ToNot(ContainSubstring("complete set"))

				listed, err := json.Marshal(out.Documents)
				Expect(err).ToNot(HaveOccurred())
				Expect(len(listed)).To(BeNumerically("<=", enumerateShareBytes(maxTokens)))
			})
		})

		// A corpus no rule matches repeats the index reference in both fields, and a
		// list of those that fits is returned whole and said to be complete.
		It("leaves an unmapped corpus that fits its share alone", func() {
			buildManyIndex(8)

			out := call("retention")
			Expect(out.Matched).To(Equal(8))
			Expect(out.Returned).To(Equal(8))
			Expect(out.Documents).To(HaveLen(8))
			Expect(out.Truncated).To(BeFalse())
			Expect(out.Note).To(ContainSubstring("complete set"))
			for _, d := range out.Documents {
				Expect(d.Citation).To(Equal(d.IndexRef))
			}

			listed, err := json.Marshal(out.Documents)
			Expect(err).ToNot(HaveOccurred())
			Expect(len(listed)).To(BeNumerically("<=", enumerateShareBytes(6000)))
		})

		It("refuses a boolean query with a fix the model can act on", func() {
			buildIndex()

			store, err := rag.Open(cfg, "")
			Expect(err).ToNot(HaveOccurred())
			DeferCleanup(store.Close)

			_, err = callTool(ragToolNamed(RAGTools(cfg, store), knowledgeEnumerateName), ctx, json.RawMessage(`{"query":"shards OR deprecated"}`), nil)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("OR"))
		})
	})
})
