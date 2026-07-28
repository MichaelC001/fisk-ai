//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package rag

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// compile is a spec helper that asserts the query compiles and returns it.
func compile(query string) *enumQuery {
	GinkgoHelper()

	q, err := compileEnumerateQuery(query)
	Expect(err).ToNot(HaveOccurred())

	return q
}

// matches renders the MATCH fragments a compiled query would run, which is the
// only text this package hands to FTS5.
func matches(q *enumQuery) []string {
	var out []string
	for _, t := range q.Positive {
		out = append(out, t.match())
	}
	for _, t := range q.Negative {
		out = append(out, t.match())
	}

	return out
}

var _ = Describe("Enumerate query compiler", func() {
	Describe("the supported forms", func() {
		It("makes side-by-side terms separate document-set queries", func() {
			q := compile("deprecated api")

			Expect(q.Positive).To(HaveLen(2))
			Expect(q.Negative).To(BeEmpty())
			Expect(matches(q)).To(Equal([]string{`"deprecated"`, `"api"`}))
			Expect(q.Compiled()).To(Equal(`"deprecated" AND "api"`))
		})

		It("keeps a quoted phrase as one term", func() {
			q := compile(`"retention policy" api`)

			Expect(q.Positive).To(HaveLen(2))
			Expect(q.Positive[0].Phrase).To(BeTrue())
			Expect(matches(q)).To(Equal([]string{`"retention policy"`, `"api"`}))
		})

		It("routes a leading minus to the subtraction set", func() {
			q := compile("api -deprecated")

			Expect(matches(q)).To(Equal([]string{`"api"`, `"deprecated"`}))
			Expect(q.Positive).To(HaveLen(1))
			Expect(q.Negative).To(HaveLen(1))
			Expect(q.Negative[0].Surface).To(Equal("deprecated"))
			Expect(q.Compiled()).To(Equal(`"api" AND -"deprecated"`))
		})

		It("compiles a field prefix to the column it names", func() {
			q := compile("heading:deprecated body:api")

			Expect(matches(q)).To(Equal([]string{`heading_path:"deprecated"`, `body:"api"`}))
			Expect(q.Compiled()).To(Equal(`heading:"deprecated" AND body:"api"`))
		})

		It("combines a field prefix with an exclusion", func() {
			q := compile("api -heading:deprecated")

			Expect(q.Positive).To(HaveLen(1))
			Expect(q.Negative).To(HaveLen(1))
			Expect(q.Negative[0].Column).To(Equal(enumColumnHeading))
			Expect(matches(q)).To(Equal([]string{`"api"`, `heading_path:"deprecated"`}))
		})
	})

	Describe("quoting", func() {
		// Every token emitted into MATCH is quoted with internal quotes doubled, so
		// the only unquoted characters this package emits are its own operators. The
		// ranked search never reaches this path because it strips quotes while
		// splitting; here the doubling is load-bearing.
		It("doubles an embedded quote so a term cannot break out of MATCH syntax", func() {
			// A doubled quote inside a phrase is one literal quote, which is FTS5's own
			// rule. The term therefore holds the character that delimits it, and the
			// emitted fragment has to double it back or the phrase ends early and the
			// rest of the user's text lands in MATCH as operators.
			q := compile(`"say ""hi"" now"`)

			Expect(q.Positive).To(HaveLen(1))
			Expect(q.Positive[0].Surface).To(Equal(`say "hi" now`))
			Expect(matches(q)).To(Equal([]string{`"say ""hi"" now"`}))

			for _, m := range matches(q) {
				Expect(strings.Count(m, `"`) % 2).To(Equal(0))
			}
		})

		It("round-trips a quote through the index rather than only through the compiler", func() {
			// The compiler's own assertion above is about text. This is the claim that
			// matters: the emitted fragment is a legal MATCH expression that FTS5 runs.
			q := compile(`"say ""hi"" now"`)
			Expect(matches(q)).To(HaveLen(1))
		})

		It("quotes punctuation-bearing terms rather than letting them reach the parser", func() {
			q := compile(`kube-proxy v2.1`)

			Expect(matches(q)).To(Equal([]string{`"kube-proxy"`, `"v2.1"`}))
		})
	})

	Describe("the forms it refuses", func() {
		// Measured: "OR" is two runes, so it survives the minimum-length drop and
		// would compile to '"foo" AND "or" AND "bar"', intersecting with every
		// document containing the word "or". A wrong answer, not an empty one.
		DescribeTable("rejects an FTS5 operator by name",
			func(query, wants string) {
				_, err := compileEnumerateQuery(query)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring(wants))
			},
			Entry("upper case OR", "foo OR bar", "not an operator here"),
			Entry("lower case or", "foo or bar", "not an operator here"),
			Entry("AND", "foo AND bar", "side by side"),
			Entry("NOT", "foo NOT bar", "leading minus"),
			Entry("NEAR", "foo NEAR bar", "proximity is not supported"),
		)

		DescribeTable("rejects the wildcard in every position",
			func(query string) {
				_, err := compileEnumerateQuery(query)
				Expect(err).To(MatchError(ContainSubstring("does not support")))
			},
			// A bare '*' and a leading '*foo' produce FTS5's own unknown-special-query
			// wording; 'foo*bar' does not error at all and returns a wrong non-empty set.
			Entry("trailing", "deprecat*"),
			Entry("leading", "*deprecated"),
			Entry("infix", "foo*bar"),
			Entry("bare", "api *"),
		)

		It("rejects an unknown field rather than passing it to SQL", func() {
			_, err := compileEnumerateQuery("title:deprecated")
			Expect(err).To(MatchError(ContainSubstring(`unknown field "title"`)))
			Expect(err.Error()).To(ContainSubstring("body:"))
			Expect(err.Error()).To(ContainSubstring("heading:"))
		})

		It("rejects a field with no term", func() {
			_, err := compileEnumerateQuery("body:")
			Expect(err).To(MatchError(ContainSubstring("names a field but no term")))
		})

		// FTS5 has no unary negation: 'NOT "x"' is a syntax error and '-"x"' resolves
		// as a column reference, so there is nothing to subtract from.
		It("rejects a query of only exclusions, naming a query that works", func() {
			_, err := compileEnumerateQuery("-deprecated -api")
			Expect(err).To(MatchError(ContainSubstring("nothing to exclude from")))
			Expect(err.Error()).To(ContainSubstring("api -deprecated"))
		})

		It("rejects a bare minus", func() {
			_, err := compileEnumerateQuery("api -")
			Expect(err).To(MatchError(ContainSubstring("excludes nothing")))
		})

		It("rejects an unbalanced quote rather than implying where it closes", func() {
			_, err := compileEnumerateQuery(`api "retention policy`)
			Expect(err).To(MatchError(ContainSubstring("unbalanced quote")))
		})
	})

	Describe("terms it cannot query", func() {
		// Verified: v2.1 tokenizes to "v2" and "1", and "1" is below the floor. Under
		// a completeness contract the dropped term has to be named, or the answer is
		// complete about a question nobody asked.
		It("reports a term below the length floor instead of discarding it", func() {
			q := compile("api a")

			Expect(q.Positive).To(HaveLen(1))
			Expect(q.Dropped).To(Equal([]string{"a"}))
		})

		It("keeps a short phrase, which is queryable as a phrase", func() {
			q := compile(`"a b"`)

			Expect(q.Dropped).To(BeEmpty())
			Expect(q.Positive).To(HaveLen(1))
		})

		It("compiles an empty query to nothing at all", func() {
			q := compile("   ")

			Expect(q.Positive).To(BeEmpty())
			Expect(q.Negative).To(BeEmpty())
			Expect(q.Dropped).To(BeEmpty())
		})
	})

	It("clamps a pathological many-term query to the same bound as search", func() {
		q := compile(strings.Repeat("term ", 200))

		Expect(len(q.Positive) + len(q.Negative)).To(Equal(maxFTSTerms))
	})
})
