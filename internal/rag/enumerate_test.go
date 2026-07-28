//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package rag

import (
	"context"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
)

// enumerateFixture is a corpus built for the composition rules rather than for
// realism. split.md is the important one: it holds "retention" and "policy" in
// separate chunks, which is the case a single MATCH expression gets wrong.
var enumerateFixture = map[string]string{
	"split.md": "# Storage\n\n## Retention\n\nRecords are held for ninety days and then removed.\n\n" +
		"## Limits\n\nThe policy is set per account and cannot be raised by a request.\n",

	"together.md": "# Retention policy\n\n" +
		"The retention policy is reviewed each release. A retention policy change is announced ahead of time.\n",

	"api.md": "# API\n\n" +
		"The API is versioned per endpoint. A deprecated endpoint keeps answering until it is removed.\n",

	"deprecation.md": "# Deprecation\n\n" +
		"Interfaces are deprecated two releases before removal. Deprecating an interface starts the clock.\n",

	"unrelated.md": "# Authentication\n\nTokens are validated against the issuer.\n",
}

// paths returns the matched document paths in result order.
func paths(res *EnumerateResult) []string {
	out := make([]string, 0, len(res.Docs))
	for _, d := range res.Docs {
		out = append(out, filepath.Base(d.Path))
	}

	return out
}

var _ = Describe("Enumerate", func() {
	ctx := context.Background()

	var (
		docsD string
		cfg   *config.Config
	)

	BeforeEach(func() {
		tmp := GinkgoT().TempDir()
		docsD = filepath.Join(tmp, "docs")
		cfg = lexicalConfig(filepath.Join(tmp, "knowledge"))

		for rel, body := range enumerateFixture {
			writeDoc(docsD, rel, body)
		}
	})

	// reader indexes the fixture and returns a read-only store, which is how both
	// the CLI and the agent reach this path.
	reader := func() *Store {
		w, err := OpenWriter(cfg, "")
		Expect(err).ToNot(HaveOccurred())
		_, err = w.Index(ctx, []string{docsD}, IndexOptions{Reconcile: true})
		Expect(err).ToNot(HaveOccurred())
		w.Close()

		r, err := Open(cfg, "")
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(r.Close)

		return r
	}

	enumerate := func(r *Store, query string) *EnumerateResult {
		GinkgoHelper()

		res, err := r.Enumerate(ctx, query, EnumerateOptions{})
		Expect(err).ToNot(HaveOccurred())

		return res
	}

	Describe("composition", func() {
		// The finding that restructured the feature: FTS5 booleans evaluate within one
		// row, which is one chunk, so a document holding each term in a different
		// chunk is invisible to '"retention" AND "policy"'. Composing over document
		// sets in Go is what finds it, and a command claiming completeness cannot ship
		// without this.
		It("finds a document whose terms are in different chunks", func() {
			r := reader()

			// Each term alone finds it, which is what makes the AND case a real miss
			// rather than an absent document.
			Expect(paths(enumerate(r, "retention"))).To(ContainElement("split.md"))
			Expect(paths(enumerate(r, "policy"))).To(ContainElement("split.md"))
			Expect(paths(enumerate(r, "retention policy"))).To(ContainElement("split.md"))

			// The same expression inside one MATCH, which is what the naive
			// implementation would have run.
			single, err := r.documentsMatching(ctx, ftsTablePorter, `"retention" AND "policy"`)
			Expect(err).ToNot(HaveOccurred())

			var splitID int64
			Expect(r.db.QueryRowContext(ctx, `SELECT id FROM documents WHERE path LIKE '%split.md'`).Scan(&splitID)).To(Succeed())
			Expect(single).ToNot(HaveKey(splitID), "chunk-level AND should miss it, or this spec is not testing anything")
		})

		It("intersects terms rather than unioning them", func() {
			r := reader()

			res := enumerate(r, "retention policy")
			Expect(paths(res)).To(ConsistOf("split.md", "together.md"))
			Expect(res.Matched).To(Equal(2))
		})

		It("subtracts an excluded term", func() {
			r := reader()

			Expect(paths(enumerate(r, "deprecated"))).To(ConsistOf("api.md", "deprecation.md"))
			Expect(paths(enumerate(r, "deprecated -endpoint"))).To(ConsistOf("deprecation.md"))
		})

		It("scopes a term to one column", func() {
			r := reader()

			// "Retention" is a heading in both, but only together.md says it in a body.
			Expect(paths(enumerate(r, "heading:retention"))).To(ConsistOf("split.md", "together.md"))
			Expect(paths(enumerate(r, "body:retention"))).To(ConsistOf("together.md"))
		})

		It("matches a phrase only where the words are adjacent", func() {
			r := reader()

			Expect(paths(enumerate(r, `"retention policy"`))).To(ConsistOf("together.md"))
		})
	})

	Describe("what it reports", func() {
		It("counts body and heading matches separately", func() {
			r := reader()

			res := enumerate(r, "retention")
			byPath := map[string]MatchedDoc{}
			for _, d := range res.Docs {
				byPath[filepath.Base(d.Path)] = d
			}

			// split.md has the word in a heading only; together.md has it in both.
			Expect(byPath["split.md"].HeadingMatches).To(BeNumerically(">", 0))
			Expect(byPath["split.md"].BodyMatches).To(Equal(0))
			Expect(byPath["together.md"].BodyMatches).To(BeNumerically(">", 0))
		})

		It("cites a chunk that matched rather than the top of the file", func() {
			r := reader()

			res := enumerate(r, "limits")
			Expect(res.Docs).To(HaveLen(1))
			Expect(res.Docs[0].Citation).To(Equal(Citation(res.Docs[0].Path, 1)))
		})

		It("reports the indexed document count alongside the match count", func() {
			r := reader()

			res := enumerate(r, "retention")
			Expect(res.IndexedDocuments).To(Equal(len(enumerateFixture)))
			Expect(res.Matched).To(Equal(len(res.Docs)))
		})

		It("reports each term against both indexes and names its stem", func() {
			r := reader()

			res := enumerate(r, "deprecated")
			Expect(res.Terms).To(HaveLen(1))

			term := res.Terms[0]
			Expect(term.Surface).To(Equal("deprecated"))
			Expect(term.Stem).To(Equal("deprec"))

			// Both documents hold a form of the word; only two hold it as written, and
			// deprecation.md holds "deprecated" too, so the interesting gap is that the
			// stemmed count also reaches "deprecating".
			Expect(term.Docs).To(Equal(2))
			Expect(term.Literal).To(BeNumerically("<=", term.Docs))
		})

		It("names a term it could not query rather than dropping it silently", func() {
			r := reader()

			res := enumerate(r, "retention a")
			Expect(res.Status).To(Equal(EnumOK))

			var dropped []TermReport
			for _, t := range res.Terms {
				if t.Dropped {
					dropped = append(dropped, t)
				}
			}
			Expect(dropped).To(HaveLen(1))
			Expect(dropped[0].Surface).To(Equal("a"))
		})

		It("shows the expression it ran", func() {
			r := reader()

			Expect(enumerate(r, "retention -policy").Compiled).To(Equal(`"retention" AND -"policy"`))
		})
	})

	Describe("ordering and budget", func() {
		It("orders by matching chunks by default and by path on request", func() {
			r := reader()

			byMatches, err := r.Enumerate(ctx, "retention", EnumerateOptions{Sort: SortByMatches})
			Expect(err).ToNot(HaveOccurred())
			Expect(paths(byMatches)).To(Equal([]string{"together.md", "split.md"}))

			byPath, err := r.Enumerate(ctx, "retention", EnumerateOptions{Sort: SortByPath})
			Expect(err).ToNot(HaveOccurred())
			Expect(paths(byPath)).To(Equal([]string{"split.md", "together.md"}))
		})

		// A limit applied before the order is resolved returns an arbitrary subset,
		// which is indistinguishable from a ranking to the reader.
		It("resolves the order before applying the limit", func() {
			r := reader()

			res, err := r.Enumerate(ctx, "retention", EnumerateOptions{Limit: 1, Sort: SortByMatches})
			Expect(err).ToNot(HaveOccurred())
			Expect(paths(res)).To(Equal([]string{"together.md"}))
			Expect(res.Matched).To(Equal(2), "the total must survive truncation")
			Expect(res.Returned).To(Equal(1))
			Expect(res.Truncated).To(BeTrue())
		})

		It("filters on body matches, and the total reflects the filter", func() {
			r := reader()

			res, err := r.Enumerate(ctx, "retention", EnumerateOptions{MinBodyMatches: 1})
			Expect(err).ToNot(HaveOccurred())
			Expect(paths(res)).To(ConsistOf("together.md"))
			Expect(res.Matched).To(Equal(1))
			Expect(res.Truncated).To(BeFalse())
		})
	})

	Describe("the states that are not failures", func() {
		It("reports an unbuilt index rather than panicking on a nil handle", func() {
			r, err := Open(lexicalConfig(filepath.Join(GinkgoT().TempDir(), "absent")), "")
			Expect(err).ToNot(HaveOccurred())
			defer r.Close()

			res, err := r.Enumerate(ctx, "retention", EnumerateOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(res.Status).To(Equal(EnumIndexNotBuilt))
		})

		It("distinguishes an empty corpus from an empty result", func() {
			w, err := OpenWriter(cfg, "")
			Expect(err).ToNot(HaveOccurred())
			w.Close()

			r, err := Open(cfg, "")
			Expect(err).ToNot(HaveOccurred())
			defer r.Close()

			res, err := r.Enumerate(ctx, "retention", EnumerateOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(res.Status).To(Equal(EnumCorpusEmpty))
		})

		// The two states a single "0 results" cannot tell apart, and they call for
		// opposite next actions.
		It("distinguishes a query that reduced to nothing from a complete empty answer", func() {
			r := reader()

			empty := enumerate(r, "a b")
			Expect(empty.Status).To(Equal(EnumQueryEmpty))
			Expect(empty.Terms).To(HaveLen(2))

			absent := enumerate(r, "kubernetes")
			Expect(absent.Status).To(Equal(EnumOK))
			Expect(absent.Docs).To(BeEmpty())
			Expect(absent.Matched).To(Equal(0))
			Expect(absent.IndexedDocuments).To(Equal(len(enumerateFixture)))
		})

		It("returns the compiler's error, with its fix, rather than an FTS5 one", func() {
			r := reader()

			_, err := r.Enumerate(ctx, "retention OR policy", EnumerateOptions{})
			Expect(err).To(MatchError(ContainSubstring("not an operator here")))
			Expect(err.Error()).ToNot(ContainSubstring("fts5"))
		})
	})
})

var _ = Describe("Related forms", func() {
	ctx := context.Background()

	It("names the other forms behind a stemmed count", func() {
		tmp := GinkgoT().TempDir()
		docsD := filepath.Join(tmp, "docs")
		cfg := lexicalConfig(filepath.Join(tmp, "knowledge"))

		writeDoc(docsD, "a.md", "# A\n\nThis interface is deprecated.\n")
		writeDoc(docsD, "b.md", "# B\n\nThe deprecation is announced early.\n")
		writeDoc(docsD, "c.md", "# C\n\nWe deprecate interfaces slowly.\n")

		w, err := OpenWriter(cfg, "")
		Expect(err).ToNot(HaveOccurred())
		_, err = w.Index(ctx, []string{docsD}, IndexOptions{Reconcile: true})
		Expect(err).ToNot(HaveOccurred())
		w.Close()

		r, err := Open(cfg, "")
		Expect(err).ToNot(HaveOccurred())
		defer r.Close()

		res, err := r.Enumerate(ctx, "deprecated", EnumerateOptions{})
		Expect(err).ToNot(HaveOccurred())
		Expect(res.Matched).To(Equal(3))

		term := res.Terms[0]
		Expect(term.Docs).To(Equal(3))
		Expect(term.Literal).To(Equal(1))
		Expect(term.Related).To(ConsistOf("deprecation", "deprecate"))
	})

	It("names nothing when the counts agree, so the scan is not paid for", func() {
		tmp := GinkgoT().TempDir()
		docsD := filepath.Join(tmp, "docs")
		cfg := lexicalConfig(filepath.Join(tmp, "knowledge"))
		writeDoc(docsD, "a.md", "# A\n\nThis interface is deprecated.\n")

		w, err := OpenWriter(cfg, "")
		Expect(err).ToNot(HaveOccurred())
		_, err = w.Index(ctx, []string{docsD}, IndexOptions{Reconcile: true})
		Expect(err).ToNot(HaveOccurred())
		w.Close()

		r, err := Open(cfg, "")
		Expect(err).ToNot(HaveOccurred())
		defer r.Close()

		res, err := r.Enumerate(ctx, "deprecated", EnumerateOptions{})
		Expect(err).ToNot(HaveOccurred())
		Expect(res.Terms[0].Related).To(BeEmpty())
	})
})
