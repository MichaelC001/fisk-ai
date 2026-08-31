//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package rag

import (
	"context"
	"fmt"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
)

// recordingEmbedder captures the documents handed to it so a spec can assert on the
// exact text the vectors are built from. It embeds through fakeEmbedder so the rest
// of the vector tier behaves normally.
type recordingEmbedder struct {
	fakeEmbedder

	seen []Document
}

func (r *recordingEmbedder) EmbedDocuments(ctx context.Context, docs []Document) ([][]float32, error) {
	r.seen = append(r.seen, docs...)

	return r.fakeEmbedder.EmbedDocuments(ctx, docs)
}

// ftsRowCount counts rows in an FTS table for a MATCH expression, which is how a
// spec sees terms the index still holds for a chunk that no longer exists. A join
// back to chunks would hide exactly that, since search.go drops unjoinable rows.
func ftsRowCount(ctx context.Context, s *Store, table, match string) int {
	n, err := scanCount(ctx, s.db, fmt.Sprintf(`SELECT count(*) FROM %s WHERE %[1]s MATCH ?`, table), match)
	Expect(err).ToNot(HaveOccurred())

	return n
}

// integrityCheck runs the FTS5 integrity check in the rank form against every
// full-text table. The bare form returns clean against an index that no longer
// matches its content table, so it is not evidence; only ('integrity-check', 1)
// compares the index against the content. Both tables are checked because the
// commands are per table, not trigger-driven, so a trigger that keeps one in step
// and not the other passes any check that looks at one.
func integrityCheck(ctx context.Context, s *Store) error {
	for _, table := range []string{"chunks_fts", "chunks_fts_exact"} {
		_, err := s.db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s(%[1]s, rank) VALUES('integrity-check', 1)`, table))
		if err != nil {
			return fmt.Errorf("%s: %w", table, err)
		}
	}

	return nil
}

var _ = Describe("Schema and triggers", func() {
	ctx := context.Background()

	var (
		tmp    string
		storeD string
		docsD  string
		cfg    *config.Config
	)

	BeforeEach(func() {
		tmp = GinkgoT().TempDir()
		storeD = filepath.Join(tmp, "knowledge")
		docsD = filepath.Join(tmp, "docs")
		cfg = lexicalConfig(storeD)

		writeDoc(docsD, "backpressure.md", "# Design\n\n## Backpressure\n\nThe queue applies backpressure when the buffer is full so producers slow down.\n")
		writeDoc(docsD, "auth.md", "# Authentication\n\nTokens are validated against the issuer before any request proceeds.\n")
	})

	index := func(w *Store) {
		_, err := w.Index(ctx, []string{docsD}, IndexOptions{Reconcile: true})
		Expect(err).ToNot(HaveOccurred())
	}

	Describe("the stored columns", func() {
		It("keeps the breadcrumb out of the body and answers each column separately", func() {
			w, err := OpenWriter(cfg, "", Options{})
			Expect(err).ToNot(HaveOccurred())
			defer w.Close()
			index(w)

			// "Design" and "Backpressure" are heading words only. Under the folded
			// schema both were in the body column too, so no body-only question could
			// be asked at all.
			Expect(ftsRowCount(ctx, w, "chunks_fts", `heading_path:"backpressure"`)).To(Equal(1))
			Expect(ftsRowCount(ctx, w, "chunks_fts", `body:"backpressure"`)).To(Equal(1))
			Expect(ftsRowCount(ctx, w, "chunks_fts", `body:"design"`)).To(Equal(0))
			Expect(ftsRowCount(ctx, w, "chunks_fts", `heading_path:"queue"`)).To(Equal(0))
		})

		// Measured on the folded schema: a phrase whose first word ended the
		// breadcrumb and whose second began the body matched, because the two sat
		// adjacent in one indexed column. Nothing in the document said it.
		It("does not match a phrase spanning the heading and the body", func() {
			writeDoc(docsD, "retention.md", "# Retention Policy\n\nchanges are announced a release ahead.\n")

			w, err := OpenWriter(cfg, "", Options{})
			Expect(err).ToNot(HaveOccurred())
			defer w.Close()
			index(w)

			Expect(ftsRowCount(ctx, w, "chunks_fts", `"policy changes"`)).To(Equal(0))
			Expect(ftsRowCount(ctx, w, "chunks_fts", `"retention policy"`)).To(Equal(1))
		})

		It("returns the body alone to every reader, so no surface renders the breadcrumb twice", func() {
			w, err := OpenWriter(cfg, "", Options{})
			Expect(err).ToNot(HaveOccurred())
			index(w)
			w.Close()

			r, err := Open(cfg, "", Options{})
			Expect(err).ToNot(HaveOccurred())
			defer r.Close()

			res, err := r.Search(ctx, "backpressure buffer", 5)
			Expect(err).ToNot(HaveOccurred())
			Expect(res.Hits).ToNot(BeEmpty())

			hit := res.Hits[0]
			Expect(hit.HeadingPath).To(Equal("Design > Backpressure"))
			Expect(hit.Content).To(HavePrefix("The queue applies"))
			Expect(hit.Content).ToNot(ContainSubstring("Design >"))

			// knowledge show reads the same column by citation.
			heading, body, err := r.ChunkText(ctx, hit.DocPath, hit.Ordinal)
			Expect(err).ToNot(HaveOccurred())
			Expect(heading).To(Equal("Design > Backpressure"))
			Expect(body).ToNot(ContainSubstring("Design >"))
		})
	})

	Describe("the text handed to the embedder", func() {
		// The vectors are built from this string and nothing detects a change to it:
		// the manifest pins model, dimension, normalization and prefixes, none of
		// which is a function of the text. Unfolding the lexical index deliberately
		// did not unfold this, so it is pinned here.
		It("is the breadcrumb folded into the body, unchanged by the column split", func() {
			emb := &recordingEmbedder{fakeEmbedder: fakeEmbedder{model: "m1", dim: 32}}

			w, err := OpenWriter(vectorConfig(storeD, "m1"), "", Options{})
			Expect(err).ToNot(HaveOccurred())
			defer w.Close()
			w.emb = emb
			index(w)

			Expect(emb.seen).ToNot(BeEmpty())

			var found bool
			for _, d := range emb.seen {
				if d.Title != "Design > Backpressure" {
					continue
				}
				found = true
				Expect(d.Text).To(Equal("Design > Backpressure\n\nThe queue applies backpressure when the buffer is full so producers slow down."))
			}
			Expect(found).To(BeTrue(), "the chunk under the Backpressure heading was never embedded")
		})

		It("carries no breadcrumb for a chunk that has none", func() {
			writeDoc(docsD, "plain.txt", "just a paragraph with no headings at all\n")

			emb := &recordingEmbedder{fakeEmbedder: fakeEmbedder{model: "m1", dim: 32}}
			w, err := OpenWriter(vectorConfig(storeD, "m1"), "", Options{})
			Expect(err).ToNot(HaveOccurred())
			defer w.Close()
			w.emb = emb
			index(w)

			for _, d := range emb.seen {
				if d.Title == "" {
					Expect(d.Text).To(Equal("just a paragraph with no headings at all"))
				}
			}
		})
	})

	Describe("the two tokenizers", func() {
		// One document, one word. Every count below is about how that single word is
		// stored, not about which documents exist.
		BeforeEach(func() {
			writeDoc(docsD, "policy.md", "# Policy\n\nThis interface is deprecated and will be removed.\n")
		})

		withIndex := func(fn func(w *Store)) {
			w, err := OpenWriter(cfg, "", Options{})
			Expect(err).ToNot(HaveOccurred())
			defer w.Close()
			index(w)
			fn(w)
		}

		It("stems in the matcher and does not in the second table", func() {
			withIndex(func(w *Store) {
				// The stemmed table is what every query runs against, so a zero from it
				// means no document holds the word in any form. That is the claim, and it
				// is only true because these all match.
				for _, form := range []string{"deprecated", "deprecate", "deprecation", "deprecating"} {
					Expect(ftsRowCount(ctx, w, "chunks_fts", `"`+form+`"`)).
						To(Equal(1), "the stemmed table should match %q", form)
				}

				// The unstemmed table matches only the form the document uses, which is
				// what lets a stemmed count say how many of its documents contain the word
				// as it was typed.
				Expect(ftsRowCount(ctx, w, "chunks_fts_exact", `"deprecated"`)).To(Equal(1))
				for _, form := range []string{"deprecate", "deprecation", "deprecating"} {
					Expect(ftsRowCount(ctx, w, "chunks_fts_exact", `"`+form+`"`)).
						To(Equal(0), "the unstemmed table should not match %q", form)
				}
			})
		})

		// The reason the unstemmed table exists rather than the stemmed one growing a
		// prefix operator. Against an index of stems, a prefix that is longer than the
		// stem but shorter than the word matches nothing, so lengthening a query can
		// take a result set from non-empty to empty and back again.
		It("is only monotonic under a prefix search in the unstemmed table", func() {
			withIndex(func(w *Store) {
				porter := map[string]int{}
				exact := map[string]int{}
				for _, prefix := range []string{"deprec", "depreca", "deprecat", "deprecate", "deprecated"} {
					porter[prefix] = ftsRowCount(ctx, w, "chunks_fts", prefix+`*`)
					exact[prefix] = ftsRowCount(ctx, w, "chunks_fts_exact", prefix+`*`)
				}

				Expect(porter).To(Equal(map[string]int{
					"deprec": 1, "depreca": 0, "deprecat": 0, "deprecate": 1, "deprecated": 1,
				}), "the stemmed table's prefix behavior is not monotonic, which is why * is not offered against it")

				Expect(exact).To(Equal(map[string]int{
					"deprec": 1, "depreca": 1, "deprecat": 1, "deprecate": 1, "deprecated": 1,
				}), "every prefix of a stored word should match it")
			})
		})

		It("exposes real words rather than stems through the vocabulary", func() {
			withIndex(func(w *Store) {
				terms := map[string]int{}
				rows, err := w.db.QueryContext(ctx, `SELECT term, doc FROM chunks_vocab`)
				Expect(err).ToNot(HaveOccurred())
				defer rows.Close()
				for rows.Next() {
					var term string
					var doc int
					Expect(rows.Scan(&term, &doc)).To(Succeed())
					terms[term] = doc
				}
				Expect(rows.Err()).ToNot(HaveOccurred())

				// A vocabulary over the stemmed table would hand an operator "deprec",
				// which is not a word and cannot be typed back into a query.
				Expect(terms).To(HaveKey("deprecated"))
				Expect(terms).ToNot(HaveKey("deprec"))
				Expect(terms["deprecated"]).To(Equal(1))
			})
		})

		// The vocabulary is writer-created because a read-only connection cannot create
		// a virtual table at all. Once created it is fully readable, which is what makes
		// it usable from the CLI without a writer lock.
		It("keeps the vocabulary readable through a read-only handle", func() {
			withIndex(func(w *Store) {})

			r, err := Open(cfg, "", Options{})
			Expect(err).ToNot(HaveOccurred())
			defer r.Close()

			n, err := scanCount(ctx, r.db, `SELECT count(*) FROM chunks_vocab WHERE term = 'deprecated'`)
			Expect(err).ToNot(HaveOccurred())
			Expect(n).To(Equal(1))
		})
	})

	Describe("Reset against a corrupt index", func() {
		// The repair a reset exists to perform, against the state it could not repair
		// while it cleared rows: the cascade fires the delete trigger into the broken
		// index and fails before any rebuild statement can run.
		It("drops and recreates rather than failing on the corruption", func() {
			w, err := OpenWriter(cfg, "", Options{})
			Expect(err).ToNot(HaveOccurred())
			defer w.Close()
			index(w)

			_, err = w.db.ExecContext(ctx, `DELETE FROM chunks_fts_data WHERE id > 1`)
			Expect(err).ToNot(HaveOccurred())

			// What the old reset did first, and why it could not get to its own repair.
			_, err = w.db.ExecContext(ctx, `DELETE FROM documents`)
			Expect(err).To(MatchError(ContainSubstring("malformed")))

			Expect(w.Reset(ctx)).To(Succeed())

			st, err := w.Stats(ctx)
			Expect(err).ToNot(HaveOccurred())
			Expect(st.Documents).To(Equal(0))

			index(w)
			Expect(integrityCheck(ctx, w)).To(Succeed())

			res, err := w.Search(ctx, "backpressure buffer", 5)
			Expect(err).ToNot(HaveOccurred())
			Expect(res.Hits).ToNot(BeEmpty())
		})
	})

	Describe("the FTS sync triggers", func() {
		// Two of the three ways to get the trigger set wrong pass a bare
		// integrity-check and then wedge every later write, so the rank form runs
		// after each of the three operations rather than only at the end.
		It("keeps the index consistent with its content table across insert, update and delete", func() {
			w, err := OpenWriter(cfg, "", Options{})
			Expect(err).ToNot(HaveOccurred())
			defer w.Close()

			index(w)
			Expect(integrityCheck(ctx, w)).To(Succeed())

			// An update: same path, changed content, which purges and re-inserts chunks.
			writeDoc(docsD, "auth.md", "# Authentication\n\nTokens are checked against the issuer, and a rotated key invalidates them.\n")
			index(w)
			Expect(integrityCheck(ctx, w)).To(Succeed())

			removed, err := w.DeleteDocument(ctx, filepath.Join(docsD, "auth.md"))
			Expect(err).ToNot(HaveOccurred())
			Expect(removed).To(BeTrue())
			Expect(integrityCheck(ctx, w)).To(Succeed())
		})

		// Omitting a column from a delete leaves that column's terms in the index
		// against a rowid that no longer exists. Search hides it, because hydration
		// drops rows it cannot join, so this asks the index directly.
		It("leaves no terms behind for a deleted chunk, in either column", func() {
			w, err := OpenWriter(cfg, "", Options{})
			Expect(err).ToNot(HaveOccurred())
			defer w.Close()
			index(w)

			Expect(ftsRowCount(ctx, w, "chunks_fts", `heading_path:"authentication"`)).To(Equal(1))
			Expect(ftsRowCount(ctx, w, "chunks_fts", `body:"issuer"`)).To(Equal(1))

			removed, err := w.DeleteDocument(ctx, filepath.Join(docsD, "auth.md"))
			Expect(err).ToNot(HaveOccurred())
			Expect(removed).To(BeTrue())

			Expect(ftsRowCount(ctx, w, "chunks_fts", `heading_path:"authentication"`)).To(Equal(0))
			Expect(ftsRowCount(ctx, w, "chunks_fts", `body:"issuer"`)).To(Equal(0))

			// A later delete is what a leftover term wedges, so exercise one.
			_, err = w.DeleteDocument(ctx, filepath.Join(docsD, "backpressure.md"))
			Expect(err).ToNot(HaveOccurred())
			Expect(integrityCheck(ctx, w)).To(Succeed())
		})
	})
})
