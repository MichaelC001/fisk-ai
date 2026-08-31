//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package rag

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
)

// wordsFixture separates the two numbers that look alike. "widget" is concentrated:
// five chunks of one document. "gizmo" is spread: one chunk in each of five. The
// vocabulary table reports the same doc value for both, because it counts chunks, so
// any code path that filters, sorts or displays on that value cannot tell them
// apart. Every ordering spec below rests on this pair.
var wordsFixture = func() map[string]string {
	out := map[string]string{}

	var concentrated strings.Builder
	concentrated.WriteString("# Widgets\n")
	for i := range 5 {
		fmt.Fprintf(&concentrated, "\n## Section %c\n\nThe widget is described here.\n", rune('A'+i))
	}
	out["concentrated.md"] = concentrated.String()

	for i := range 5 {
		out[fmt.Sprintf("spread%c.md", rune('a'+i))] = "# Gizmos\n\nA gizmo appears once in this document.\n"
	}

	// Deliberate vocabulary shapes: a stem family split across two documents so the
	// stemmed count can exceed the literal one, a mid-word target for the prefix
	// regression, reserved words, and an over-long token.
	out["forms.md"] = "# Deprecation\n\nInterfaces are deprecated before removal, and deprecating one starts the clock.\n"
	out["notice.md"] = "# Notice\n\nEvery deprecation is announced two releases ahead.\n"
	out["mid.md"] = "# Testing\n\nThe testing harness and the indexing pass both run here.\n"
	out["reserved.md"] = "# Logic\n\nThis and that, or the other, but not the last one.\n"
	out["long.md"] = "# Blob\n\nQm9ndXNiYXNlNjRibG9idGhhdGlzdmVyeWxvbmdpbmRlZWQxMjM0NTY3ODkw text.\n"

	return out
}()

var _ = Describe("Words", func() {
	ctx := context.Background()

	var (
		docsD string
		cfg   *config.Config
	)

	BeforeEach(func() {
		tmp := GinkgoT().TempDir()
		docsD = filepath.Join(tmp, "docs")
		cfg = lexicalConfig(filepath.Join(tmp, "knowledge"))

		for rel, body := range wordsFixture {
			writeDoc(docsD, rel, body)
		}
	})

	reader := func() *Store {
		w, err := OpenWriter(cfg, "", Options{})
		Expect(err).ToNot(HaveOccurred())
		_, err = w.Index(ctx, []string{docsD}, IndexOptions{Reconcile: true})
		Expect(err).ToNot(HaveOccurred())
		Expect(w.Close()).To(Succeed())

		s, err := Open(cfg, "", Options{})
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(s.Close)

		return s
	}

	// find returns the one word by surface form, failing rather than returning a
	// zero value so a missing word does not read as a wrong count.
	find := func(res *WordsResult, word string) Word {
		GinkgoHelper()

		for _, w := range res.Words {
			if w.Word == word {
				return w
			}
		}

		Fail("no word " + word + " in the result")
		return Word{}
	}

	counted := func(pattern string) *WordsResult {
		GinkgoHelper()

		res, err := reader().Words(ctx, WordsOptions{Pattern: pattern, CountThreshold: 50})
		Expect(err).ToNot(HaveOccurred())
		Expect(res.Status).To(Equal(EnumOK))

		return res
	}

	Describe("document counts", func() {
		// The whole reason the vocabulary table's own count is not used.
		It("counts documents rather than chunks", func() {
			res := counted("^(widget|gizmo)$")
			Expect(find(res, "widget").AsWritten).To(Equal(1))
			Expect(find(res, "gizmo").AsWritten).To(Equal(5))
		})

		// Any form is what knowledge match reports, and the two commands showing
		// different numbers for one word is the confusion these columns prevent.
		It("reports the stemmed count that match would report", func() {
			res := counted("deprecat")

			for _, word := range []string{"deprecated", "deprecating"} {
				w := find(res, word)
				set, err := reader().documentsMatching(ctx, ftsTablePorter, enumTerm{Surface: word}.match())
				Expect(err).ToNot(HaveOccurred())
				Expect(w.AnyForm).To(Equal(len(set)), word)
			}
		})

		It("reaches other forms through the stem", func() {
			w := find(counted("deprecat"), "deprecating")
			Expect(w.AsWritten).To(Equal(1))
			Expect(w.AnyForm).To(BeNumerically(">", w.AsWritten))
		})
	})

	Describe("the pattern", func() {
		// The bug the LiteralPrefix optimization would have shipped: an unanchored
		// pattern must return words that contain it without starting with it.
		It("matches anywhere in a word, not only at the start", func() {
			res := counted("ing")
			words := []string{}
			for _, w := range res.Words {
				words = append(words, w.Word)
			}
			Expect(words).To(ContainElement("testing"))
			Expect(words).To(ContainElement("indexing"))
		})

		It("anchors when asked to", func() {
			res := counted("^index")
			for _, w := range res.Words {
				Expect(w.Word).To(HavePrefix("index"))
			}
		})

		// The stored vocabulary is folded, so a case-sensitive match could only ever
		// return nothing for a capitalized word an operator would naturally type.
		It("ignores case", func() {
			Expect(counted("WIDGET").Matched).To(Equal(counted("widget").Matched))
			Expect(counted("WIDGET").Matched).To(BeNumerically(">", 0))
		})

		It("rejects a malformed pattern with a fix", func() {
			_, err := reader().Words(ctx, WordsOptions{Pattern: "c++"})
			Expect(err).To(MatchError(ContainSubstring("not a valid regular expression")))
			Expect(err).To(MatchError(ContainSubstring("escape any special characters")))
		})

		It("returns the whole vocabulary with no pattern", func() {
			res := counted("")
			Expect(res.Matched).To(Equal(res.Vocabulary))
			Expect(res.Vocabulary).To(BeNumerically(">", 20))
		})
	})

	Describe("bounds and ordering", func() {
		// The defect the review caught: bounding on the cheap chunk count would keep
		// "widget", which is in one document, under --min-docs 2.
		It("bounds on the document count it displays", func() {
			res, err := reader().Words(ctx, WordsOptions{Pattern: "^(widget|gizmo)$", MinDocs: 2})
			Expect(err).ToNot(HaveOccurred())
			Expect(res.Words).To(HaveLen(1))
			Expect(res.Words[0].Word).To(Equal("gizmo"))
		})

		It("removes the words that appear everywhere", func() {
			res, err := reader().Words(ctx, WordsOptions{MaxDocs: 1, CountThreshold: 0})
			Expect(err).ToNot(HaveOccurred())
			for _, w := range res.Words {
				Expect(w.AsWritten).To(BeNumerically("<=", 1))
			}
		})

		It("refuses nothing but returns nothing when the bounds cross", func() {
			res, err := reader().Words(ctx, WordsOptions{MinDocs: 5, MaxDocs: 1})
			Expect(err).ToNot(HaveOccurred())
			Expect(res.Words).To(BeEmpty())
		})

		// Sorting has to resolve before the limit or a truncated list is an arbitrary
		// subset, which is the invariant Enumerate already states for itself.
		It("sorts by document count before limiting", func() {
			res, err := reader().Words(ctx, WordsOptions{Pattern: "^(widget|gizmo)$", Sort: SortWordsByDocs, Limit: 1, CountThreshold: 50})
			Expect(err).ToNot(HaveOccurred())
			Expect(res.Words).To(HaveLen(1))
			Expect(res.Words[0].Word).To(Equal("gizmo"))
			Expect(res.Truncated).To(BeTrue())
			Expect(res.Matched).To(Equal(2))
		})

		It("sorts alphabetically when asked", func() {
			res, err := reader().Words(ctx, WordsOptions{Sort: SortWordsByWord, CountThreshold: 0})
			Expect(err).ToNot(HaveOccurred())
			for i := 1; i < len(res.Words); i++ {
				Expect(res.Words[i-1].Word < res.Words[i].Word).To(BeTrue())
			}
		})

		It("groups the forms of one word together", func() {
			res, err := reader().Words(ctx, WordsOptions{Pattern: "deprecat", Sort: SortWordsByStem, CountThreshold: 50})
			Expect(err).ToNot(HaveOccurred())
			Expect(len(res.Words)).To(BeNumerically(">", 1))
			for i := 1; i < len(res.Words); i++ {
				Expect(res.Words[i-1].Stem <= res.Words[i].Stem).To(BeTrue())
			}
		})
	})

	Describe("counting is skipped for a listing", func() {
		// The counts are two queries per word, and a vocabulary dump is scanned rather
		// than compared, so a big result is not made to pay for them.
		It("does not count a set above the threshold", func() {
			res, err := reader().Words(ctx, WordsOptions{CountThreshold: 5})
			Expect(err).ToNot(HaveOccurred())
			Expect(res.Counted).To(BeFalse())
			Expect(res.Words[0].AsWritten).To(Equal(0))
		})

		It("counts a set small enough to compare", func() {
			res, err := reader().Words(ctx, WordsOptions{Pattern: "^widget$", CountThreshold: 5})
			Expect(err).ToNot(HaveOccurred())
			Expect(res.Counted).To(BeTrue())
		})

		// A bound is stated in the number that only exists when counting, so asking for
		// one has to buy the counts however large the set is.
		It("counts regardless when a bound is given", func() {
			res, err := reader().Words(ctx, WordsOptions{MinDocs: 1, CountThreshold: 0})
			Expect(err).ToNot(HaveOccurred())
			Expect(res.Counted).To(BeTrue())
		})
	})

	// The vocabulary spans both indexed columns, and this feature already established
	// that conflating body and heading text inverts results, which is why match has
	// body: and heading:. The table cannot say which column a word came from, so the
	// scope is applied to the counts instead.
	Describe("field scoping", func() {
		scoped := func(pattern, field string) *WordsResult {
			GinkgoHelper()

			res, err := reader().Words(ctx, WordsOptions{Pattern: pattern, Field: field})
			Expect(err).ToNot(HaveOccurred())

			return res
		}

		It("counts only the chosen column", func() {
			// "gizmo" is body text under a "Gizmos" heading, so it is in one column only.
			Expect(scoped("^gizmo$", "body").Words).To(HaveLen(1))
			Expect(scoped("^gizmo$", "heading").Words).To(BeEmpty())
		})

		It("finds a word that only appears in a heading", func() {
			res := scoped("^gizmos$", "heading")
			Expect(res.Words).To(HaveLen(1))
			Expect(res.Words[0].AsWritten).To(Equal(5))
		})

		// An empty scoped result must not read like an absent word, so the pattern count
		// survives the scope to say which of the two happened.
		It("reports what the pattern found even when the scope removes it all", func() {
			res := scoped("^gizmo$", "heading")
			Expect(res.Matched).To(Equal(0))
			Expect(res.PatternMatched).To(Equal(1))
			Expect(res.Scoped).To(BeTrue())
		})

		It("forces the counts, since the scope is expressed through them", func() {
			res, err := reader().Words(ctx, WordsOptions{Field: "body", CountThreshold: 0})
			Expect(err).ToNot(HaveOccurred())
			Expect(res.Counted).To(BeTrue())
			Expect(res.Scoped).To(BeTrue())
		})

		It("is unscoped by default", func() {
			res, err := reader().Words(ctx, WordsOptions{Pattern: "^gizmo$", CountThreshold: 50})
			Expect(err).ToNot(HaveOccurred())
			Expect(res.Scoped).To(BeFalse())
			Expect(res.Words).To(HaveLen(1))
		})

		It("names the two fields it knows", func() {
			_, err := reader().Words(ctx, WordsOptions{Field: "title"})
			Expect(err).To(MatchError(ContainSubstring("unknown field")))
			Expect(err).To(MatchError(ContainSubstring("heading")))
		})
	})

	Describe("queryability", func() {
		// The vocabulary holds words knowledge match refuses, so a count is stated for
		// words that command will not produce one for.
		It("marks the reserved words", func() {
			res := counted("^(and|or|not)$")
			Expect(res.Words).ToNot(BeEmpty())
			for _, w := range res.Words {
				Expect(w.Queryable).To(BeFalse(), w.Word)
			}
		})

		It("marks words below the minimum term length", func() {
			Expect(wordIsQueryable("a")).To(BeFalse())
			Expect(wordIsQueryable("ab")).To(BeTrue())
		})

		It("accepts an ordinary word", func() {
			Expect(find(counted("^widget$"), "widget").Queryable).To(BeTrue())
		})
	})

	Describe("index states", func() {
		It("reports an index that was never built", func() {
			s, err := Open(cfg, "", Options{})
			Expect(err).ToNot(HaveOccurred())
			DeferCleanup(s.Close)

			res, err := s.Words(ctx, WordsOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(res.Status).To(Equal(EnumIndexNotBuilt))
			Expect(res.Words).To(BeEmpty())
		})

		It("reports an index holding no documents", func() {
			w, err := OpenWriter(cfg, "", Options{})
			Expect(err).ToNot(HaveOccurred())
			Expect(w.Close()).To(Succeed())

			s, err := Open(cfg, "", Options{})
			Expect(err).ToNot(HaveOccurred())
			DeferCleanup(s.Close)

			res, err := s.Words(ctx, WordsOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(res.Status).To(Equal(EnumCorpusEmpty))
		})
	})
})
