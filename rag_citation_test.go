//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/ui/columns"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/rag"
)

var _ = Describe("knowledge citation rendering", func() {
	ctx := context.Background()

	var (
		docsD string
		cfg   *config.Config
		store *rag.Store
	)

	writeDoc := func(rel string, content string) {
		GinkgoHelper()

		path := filepath.Join(docsD, rel)
		Expect(os.MkdirAll(filepath.Dir(path), 0o755)).To(Succeed())
		Expect(os.WriteFile(path, []byte(content), 0o644)).To(Succeed())
	}

	// The corpus is half published: one document a rule reaches and one it does not,
	// which is the ordinary state the blank column and the count exist to report. The
	// rule is unanchored because the indexer stores the absolute path it walked.
	BeforeEach(func() {
		tmp := GinkgoT().TempDir()
		docsD = filepath.Join(tmp, "docs")

		cfg = &config.Config{
			Identity: "test",
			Harness: config.HarnessConfig{
				RAG: &config.RAGConfig{
					Enabled:   true,
					Directory: filepath.Join(tmp, "knowledge"),
					Citations: []config.RAGCitationRule{{
						Pattern:         `published/(.*)\.md$`,
						Replace:         "https://docs.example.net/$1#${anchor}",
						PatternCompiled: regexp.MustCompile(`published/(.*)\.md$`),
					}},
				},
			},
		}

		writeDoc("published/guide.md", "# Guide\n\n## Backpressure\n\nThe queue applies backpressure when the buffer is full so producers slow down.\n")
		writeDoc("private/notes.md", "# Notes\n\nThe rollout notes mention backpressure once, in passing.\n")

		w, err := rag.OpenWriter(cfg, "")
		Expect(err).ToNot(HaveOccurred())
		_, err = w.Index(ctx, []string{docsD}, rag.IndexOptions{Reconcile: true})
		Expect(err).ToNot(HaveOccurred())
		Expect(w.Close()).To(Succeed())

		store, err = rag.Open(cfg, "")
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(store.Close)
	})

	Describe("knowledge search", func() {
		hits := func() []rag.Hit {
			GinkgoHelper()

			res, err := store.Search(ctx, "backpressure", 5)
			Expect(err).ToNot(HaveOccurred())
			Expect(res.Hits).To(HaveLen(2))

			return res.Hits
		}

		render := func(hits []rag.Hit) string {
			c := columns.New()
			renderSearchHits(c, hits, false)

			return c.String()
		}

		It("keeps the raw citation as the heading and prints the address under it", func() {
			out := render(hits())

			// The heading is the token knowledge show accepts, for both documents.
			Expect(out).To(ContainSubstring("guide.md#0"))
			Expect(out).To(ContainSubstring("notes.md#0"))

			Expect(out).To(ContainSubstring("https://docs.example.net/guide#backpressure"))
			Expect(strings.Count(out, "Address:")).To(Equal(1), "the unmapped document gets no address line")
		})

		It("prints no address at all when no rule is configured", func() {
			plain, err := rag.Open(&config.Config{
				Identity: "test",
				Harness:  config.HarnessConfig{RAG: &config.RAGConfig{Enabled: true, Directory: cfg.Harness.RAG.Directory}},
			}, "")
			Expect(err).ToNot(HaveOccurred())
			DeferCleanup(plain.Close)

			res, err := plain.Search(ctx, "backpressure", 5)
			Expect(err).ToNot(HaveOccurred())

			Expect(render(res.Hits)).ToNot(ContainSubstring("Address:"))
		})

		// knowledge show accepts the whole token and nothing else, so a heading cut
		// short would be a heading nobody can paste anywhere.
		It("leaves a long citation and a long address whole", func() {
			citation := "docs/" + strings.Repeat("a", 300) + ".md#7"
			address := "https://docs.example.net/" + strings.Repeat("b", 300)

			out := render([]rag.Hit{{Citation: citation, Address: address, AddressMapped: true, Content: "body"}})

			Expect(out).To(ContainSubstring(citation))
			Expect(out).To(ContainSubstring(address))
		})

		// A citation carries a corpus path and an address can carry a heading, so both
		// are document content on their way to a terminal. The newline is what makes
		// this spec load-bearing: the table library already drops raw escapes, and
		// collapsing whitespace is what SanitizeForTerminal adds over it, so a value
		// reaching the terminal unsanitized breaks the line rather than colors it.
		It("strips a control sequence and a newline from the citation and the address", func() {
			out := render([]rag.Hit{{
				Citation:      "docs/gui\x1b[31mde\nnotes.md#1",
				Address:       "https://docs.example.net/gui\x1b[31mde#head\ning",
				AddressMapped: true,
				Content:       "body",
			}})

			Expect(out).ToNot(ContainSubstring("\x1b"))
			Expect(out).To(ContainSubstring("docs/guide notes.md#1"))
			Expect(out).To(ContainSubstring("https://docs.example.net/guide#head ing"))
		})
	})

	Describe("knowledge match", func() {
		docs := func() []rag.MatchedDoc {
			GinkgoHelper()

			res, err := store.Enumerate(ctx, "backpressure", rag.EnumerateOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(res.Docs).To(HaveLen(2))

			return res.Docs
		}

		It("adds an address column that is blank for the document no rule matched", func() {
			out := matchTable(docs(), true).Render()

			Expect(out).To(ContainSubstring("Address"))
			Expect(out).To(ContainSubstring("https://docs.example.net/guide"))
			Expect(strings.Count(out, "https://")).To(Equal(1), "the unmapped document has a blank cell")
		})

		It("leaves the column off when no rule is configured", func() {
			out := matchTable(docs(), false).Render()

			Expect(out).ToNot(ContainSubstring("Address"))
			Expect(out).ToNot(ContainSubstring("https://"))
		})

		It("strips a control sequence and a newline from the address", func() {
			out := matchTable([]rag.MatchedDoc{{
				Path:          "docs/guide.md",
				Citation:      "docs/guide.md#1",
				Address:       "https://docs.example.net/gui\x1b[31mde#head\ning",
				AddressMapped: true,
			}}, true).Render()

			Expect(out).ToNot(ContainSubstring("\x1b"))
			Expect(out).To(ContainSubstring("https://docs.example.net/guide#head ing"))
		})
	})

	Describe("knowledge sources", func() {
		sources := func() []rag.Source {
			GinkgoHelper()

			out, err := store.Sources(ctx)
			Expect(err).ToNot(HaveOccurred())
			Expect(out).To(HaveLen(2))

			return out
		}

		// The count is the diagnostic: a rule that matches nothing sends raw paths to
		// the model and reports no error anywhere.
		It("renders the document-level address and counts what no rule reached", func() {
			tbl, unmapped := sourcesTable(sources(), store.CitationMapper())

			Expect(unmapped).To(Equal(1))

			out := tbl.Render()
			Expect(out).To(ContainSubstring("Address"))
			Expect(out).To(ContainSubstring("https://docs.example.net/guide"))
			Expect(out).ToNot(ContainSubstring("#"), "no chunk is being addressed, so the anchor renders empty")
			Expect(strings.Count(out, "https://")).To(Equal(1))
		})

		It("leaves the column off and the count at zero with no mapper", func() {
			tbl, unmapped := sourcesTable(sources(), nil)

			Expect(unmapped).To(Equal(0))
			Expect(tbl.Render()).ToNot(ContainSubstring("Address"))
		})

		// The mapper percent-encodes what it substitutes, so a control sequence in a
		// path reaches the column as text rather than as an escape.
		It("keeps a control sequence in a path out of the address it renders", func() {
			mapper := rag.NewCitationMapper([]config.RAGCitationRule{{
				Pattern:         `^(.*)$`,
				Replace:         "https://docs.example.net/$1",
				PatternCompiled: regexp.MustCompile(`^(.*)$`),
			}})

			tbl, unmapped := sourcesTable([]rag.Source{{Path: "gui\x1b[31mde.md"}}, mapper)

			Expect(unmapped).To(Equal(0))
			Expect(tbl.Render()).To(ContainSubstring("https://docs.example.net/gui%1B%5B31mde.md"))
		})

		// The operator's own literal template text is not substituted, so nothing
		// percent-encodes it and the surface is the only thing that can. A newline is
		// what separates this from the table library's own stripping.
		It("collapses a newline the operator wrote into a replacement", func() {
			mapper := rag.NewCitationMapper([]config.RAGCitationRule{{
				Pattern:         `^(.*)\.md$`,
				Replace:         "https://docs.example.net/\n$1",
				PatternCompiled: regexp.MustCompile(`^(.*)\.md$`),
			}})

			tbl, unmapped := sourcesTable([]rag.Source{{Path: "guide.md"}}, mapper)

			Expect(unmapped).To(Equal(0))
			Expect(tbl.Render()).To(ContainSubstring("https://docs.example.net/ guide"))
		})

		// A rule that matches and renders nothing leaves the same blank cell as one
		// that never matched, so the count has to agree with the column.
		It("counts a document whose rule renders an empty address as unmapped", func() {
			mapper := rag.NewCitationMapper([]config.RAGCitationRule{{
				Pattern:         `^(.*)\.md$`,
				Replace:         "#${anchor}",
				PatternCompiled: regexp.MustCompile(`^(.*)\.md$`),
			}})

			_, unmapped := sourcesTable([]rag.Source{{Path: "guide.md"}}, mapper)

			Expect(unmapped).To(Equal(1))
		})
	})
})
