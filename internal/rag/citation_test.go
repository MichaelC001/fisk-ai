//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package rag

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
)

// citationRule builds one rule the way config hands it over, with the pattern
// already compiled. A rule reaching the mapper any other way carries no compiled
// pattern and is skipped.
func citationRule(pattern string, replace string) config.RAGCitationRule {
	return config.RAGCitationRule{
		Pattern:         pattern,
		Replace:         replace,
		PatternCompiled: regexp.MustCompile(pattern),
	}
}

// docsRule is the shape most of the table uses: a markdown file under docs/,
// captured without its extension.
func docsRule(replace string) []config.RAGCitationRule {
	return []config.RAGCitationRule{citationRule(`^docs/(.*)\.md$`, replace)}
}

var _ = Describe("CitationMapper", func() {
	DescribeTable("rendering a citation",
		func(rules []config.RAGCitationRule, path string, ordinal int, headingPath string, citation string, matched bool) {
			got, ok := NewCitationMapper(rules).Render(path, ordinal, headingPath)

			Expect(got).To(Equal(citation))
			Expect(ok).To(Equal(matched))
		},

		Entry("keeps the raw token when no rule is configured",
			nil, "docs/guide.md", 3, "Guide > Setup",
			"docs/guide.md#3", false),

		Entry("keeps the raw token when no rule matches",
			docsRule("https://docs.example.net/$1"), "notes/todo.md", 1, "Todo",
			"notes/todo.md#1", false),

		Entry("skips a rule carrying no compiled pattern",
			[]config.RAGCitationRule{{Pattern: `^docs/`, Replace: "https://docs.example.net/"}}, "docs/guide.md", 1, "Guide",
			"docs/guide.md#1", false),

		// ReplaceAllString would keep the part of the path outside the match and
		// render /home/rip/https://docs.example.net/guide/setup/.
		Entry("drops the part of the path an unanchored rule did not match",
			[]config.RAGCitationRule{citationRule(`docs/(.*)\.md$`, "https://docs.example.net/$1/")},
			"/home/rip/docs/guide/setup.md", 2, "Guide > Setup",
			"https://docs.example.net/guide/setup/", true),

		// The file is named ${anchor}.md on disk, and a single pass never fills that
		// captured placeholder from the document's own heading.
		Entry("never rescans a placeholder that arrived through a capture",
			docsRule("https://docs.example.net/$1#${anchor}"),
			"docs/${anchor}.md", 1, "Read Me",
			"https://docs.example.net/%24%7Banchor%7D#read-me", true),

		// url.PathEscape leaves @ alone, which would put evil.example in the
		// authority of the rendered URL.
		Entry("keeps a captured directory out of the authority",
			[]config.RAGCitationRule{citationRule(`^(?P<host>[^/]+)/`, "https://${host}.docs.example.net/")},
			"a@evil.example/guide.md", 1, "Guide",
			"https://a%40evil.example.docs.example.net/", true),

		Entry("percent-encodes a heading holding a space, an ampersand and a question mark",
			docsRule("https://docs.example.net/$1?h=${heading}"),
			"docs/guide.md", 1, "Guide > Why & When?",
			"https://docs.example.net/guide?h=Why%20%26%20When%3F", true),

		Entry("fills every reserved name",
			docsRule("https://docs.example.net/$1/${ordinal}?h=${heading}#${anchor}"),
			"docs/guide.md", 7, "Guide > Setup",
			"https://docs.example.net/guide/7?h=Setup#setup", true),

		Entry("takes the deepest crumb as the heading",
			docsRule("https://docs.example.net/$1#${anchor}"),
			"docs/guide.md", 1, "Guide > Setup > Running It",
			"https://docs.example.net/guide#running-it", true),

		Entry("deletes an apostrophe rather than collapsing it",
			docsRule("https://docs.example.net/$1#${anchor}"),
			"docs/guide.md", 1, "Guide > Don't Panic",
			"https://docs.example.net/guide#dont-panic", true),

		Entry("deletes a period in a version heading",
			docsRule("https://docs.example.net/$1#${anchor}"),
			"docs/guide.md", 1, "v1.2.3 Release",
			"https://docs.example.net/guide#v123-release", true),

		Entry("deletes an ampersand and keeps both spaces around it",
			docsRule("https://docs.example.net/$1#${anchor}"),
			"docs/guide.md", 1, "Tips & Tricks",
			"https://docs.example.net/guide#tips--tricks", true),

		Entry("keeps an underscore, which is a word character",
			docsRule("https://docs.example.net/$1#${anchor}"),
			"docs/guide.md", 1, "snake_case Names",
			"https://docs.example.net/guide#snake_case-names", true),

		Entry("drops leading punctuation from the slug",
			docsRule("https://docs.example.net/$1#${anchor}"),
			"docs/guide.md", 1, ".NET Guide",
			"https://docs.example.net/guide#net-guide", true),

		Entry("trims a bare hash when the chunk has no heading",
			docsRule("https://docs.example.net/$1#${anchor}"),
			"docs/guide.md", 1, "",
			"https://docs.example.net/guide", true),

		Entry("trims a bare hash when the heading is entirely punctuation",
			docsRule("https://docs.example.net/$1#${anchor}"),
			"docs/guide.md", 1, "Guide > !!!",
			"https://docs.example.net/guide", true),

		// A literal between the hash and an empty value is the operator's to get
		// right, so the hash stays.
		Entry("keeps a hash carrying a literal before an empty value",
			docsRule("https://docs.example.net/$1#section-${heading}"),
			"docs/guide.md", 1, "",
			"https://docs.example.net/guide#section-", true),

		Entry("renders a group that took part in no match as empty",
			[]config.RAGCitationRule{citationRule(`^docs/(?:(a)|(b))\.md$`, "https://docs.example.net/$1$2")},
			"docs/b.md", 1, "B",
			"https://docs.example.net/b", true),

		// A pattern group wins over the reserved name it shares a spelling with,
		// which is the order config validated the replacement in. A renderer that
		// checked the reserved names first would accept the same config and render
		// something else.
		Entry("prefers a capture group to the reserved name it shares a spelling with",
			[]config.RAGCitationRule{citationRule(`^docs/(?P<heading>.*)\.md$`, "https://docs.example.net/${heading}")},
			"docs/guide.md", 1, "Design > Backpressure",
			"https://docs.example.net/guide", true),

		Entry("renders a doubled dollar as a literal dollar",
			docsRule("https://docs.example.net/$$$1"),
			"docs/guide.md", 1, "Guide",
			"https://docs.example.net/$guide", true),

		Entry("takes the first of three rules that matches",
			[]config.RAGCitationRule{
				citationRule(`^docs/api/(.*)\.md$`, "https://api.example.net/$1"),
				citationRule(`^docs/(.*)\.md$`, "https://docs.example.net/$1"),
				citationRule(`^(.*)$`, "https://catchall.example.net/$1"),
			},
			"docs/api/v1.md", 1, "V1",
			"https://api.example.net/v1", true),

		Entry("falls past a rule that does not match to the next one",
			[]config.RAGCitationRule{
				citationRule(`^docs/api/(.*)\.md$`, "https://api.example.net/$1"),
				citationRule(`^docs/(.*)\.md$`, "https://docs.example.net/$1"),
				citationRule(`^(.*)$`, "https://catchall.example.net/$1"),
			},
			"docs/guide.md", 1, "Guide",
			"https://docs.example.net/guide", true),

		Entry("reaches the last rule when the earlier two miss",
			[]config.RAGCitationRule{
				citationRule(`^docs/api/(.*)\.md$`, "https://api.example.net/$1"),
				citationRule(`^docs/(.*)\.md$`, "https://docs.example.net/$1"),
				citationRule(`^(.*)$`, "https://catchall.example.net/$1"),
			},
			"notes/todo.txt", 1, "Todo",
			"https://catchall.example.net/notes/todo.txt", true),
	)

	DescribeTable("rendering a document-level citation",
		func(rules []config.RAGCitationRule, path string, citation string, matched bool) {
			got, ok := NewCitationMapper(rules).RenderDocument(path)

			Expect(got).To(Equal(citation))
			Expect(ok).To(Equal(matched))
		},

		// The bare path, not Citation(path, 0): no chunk is cited, so there is no
		// ordinal to invent.
		Entry("keeps the bare path when no rule is configured",
			nil, "docs/guide.md", "docs/guide.md", false),

		Entry("keeps the bare path when no rule matches",
			docsRule("https://docs.example.net/$1"), "notes/todo.md", "notes/todo.md", false),

		Entry("fills the capture groups",
			docsRule("https://docs.example.net/$1/"), "docs/guide/setup.md",
			"https://docs.example.net/guide/setup/", true),

		Entry("renders ordinal, heading and anchor empty",
			docsRule("https://docs.example.net/$1/${ordinal}?h=${heading}#${anchor}"), "docs/guide.md",
			"https://docs.example.net/guide/?h=", true),

		Entry("trims the bare hash a chunk rule leaves behind",
			docsRule("https://docs.example.net/$1#${anchor}"), "docs/guide.md",
			"https://docs.example.net/guide", true),

		Entry("takes the first rule that matches",
			[]config.RAGCitationRule{
				citationRule(`^docs/api/(.*)\.md$`, "https://api.example.net/$1"),
				citationRule(`^docs/(.*)\.md$`, "https://docs.example.net/$1"),
			},
			"docs/api/v1.md", "https://api.example.net/v1", true),
	)

	// The mapper is optional to its callers, so a caller with no rules to hand passes
	// nil rather than building one.
	It("passes every citation through a nil mapper", func() {
		var m *CitationMapper

		citation, ok := m.Render("docs/guide.md", 3, "Guide")
		Expect(citation).To(Equal("docs/guide.md#3"))
		Expect(ok).To(BeFalse())

		citation, ok = m.RenderDocument("docs/guide.md")
		Expect(citation).To(Equal("docs/guide.md"))
		Expect(ok).To(BeFalse())
	})

	Describe("the replacement grammar", func() {
		// One scanner reads a replacement: config.RAGCitationRule.ReplaceRefs, which
		// config validates the references against and which this expander walks the
		// ranges of. Expanding through it holds both against regexp's own expand, so a
		// replacement accepted at load cannot render something else at run time.
		It("resolves the same references as regexp.Regexp.ExpandString", func() {
			re := regexp.MustCompile(`(?P<sec>[a-z]+)/(\d+)`)
			path := "guide/42"

			match := re.FindStringSubmatchIndex(path)
			Expect(match).ToNot(BeNil())

			// Every template up to five characters over an alphabet that spells the
			// whole grammar: the dollar, both braces, a digit, an underscore, a
			// character no name can hold, and the letters of this pattern's group
			// name. It cannot spell ordinal, heading or anchor, so no reserved name is
			// reachable and ExpandString answers for every template it builds, and
			// every group it can name captures alphanumerics, which percent-encoding
			// leaves alone.
			alphabet := "${}1_.sec"

			checked := 0
			for length := 1; length <= 5; length++ {
				total := 1
				for range length {
					total *= len(alphabet)
				}

				buf := make([]byte, length)
				for n := range total {
					v := n
					for i := range length {
						buf[i] = alphabet[v%len(alphabet)]
						v /= len(alphabet)
					}

					template := string(buf)
					rule := config.RAGCitationRule{Replace: template, PatternCompiled: re}
					got := expandCitation(rule, path, match, citationReserved{ordinal: "7", headingPath: "Heading"})
					want := string(re.ExpandString(nil, template, path, match))
					if got != want {
						Fail(fmt.Sprintf("template %q expanded to %q, ExpandString gives %q", template, got, want))
					}
					checked++
				}
			}

			Expect(checked).To(BeNumerically(">=", 60000))
		})
	})
})

// citationConfig builds the lexical-only config of lexicalConfig with citation
// rules attached, compiled the way a parsed config hands them over.
func citationConfig(dir string, rules ...config.RAGCitationRule) *config.Config {
	cfg := lexicalConfig(dir)
	cfg.Harness.RAG.Citations = rules

	return cfg
}

var _ = Describe("Store citation rendering", func() {
	ctx := context.Background()

	var (
		docsD  string
		storeD string
		cfg    *config.Config
	)

	BeforeEach(func() {
		tmp := GinkgoT().TempDir()
		docsD = filepath.Join(tmp, "docs")
		storeD = filepath.Join(tmp, "knowledge")

		// The one rule fills both the ordinal and the anchor, so a single corpus pins
		// what a chunk-level citation carries and what a document-level one leaves
		// empty. It is unanchored because the indexer stores the absolute path it
		// walked, which here is under a temporary directory.
		cfg = citationConfig(storeD, citationRule(`published/(.*)\.md$`, "https://docs.example.net/$1?c=${ordinal}#${anchor}"))

		writeDoc(docsD, "published/guide.md", "# Guide\n\n## Getting Started\n\nInstall the binary and run it once to write a configuration file.\n\n"+
			"## Backpressure\n\nThe queue applies backpressure when the buffer is full so producers slow down.\n")

		// Outside the published tree, so no rule reaches it, and holding the same word
		// so one query returns both.
		writeDoc(docsD, "private/notes.md", "# Notes\n\nThe rollout notes mention backpressure once, in passing.\n")
	})

	// reader indexes the fixture and opens it through readCfg, so a spec can read the
	// same index through a config carrying no rules.
	reader := func(readCfg *config.Config) *Store {
		GinkgoHelper()

		w, err := OpenWriter(cfg, "", Options{})
		Expect(err).ToNot(HaveOccurred())
		_, err = w.Index(ctx, []string{docsD}, IndexOptions{Reconcile: true})
		Expect(err).ToNot(HaveOccurred())
		w.Close()

		r, err := Open(readCfg, "", Options{})
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(r.Close)

		return r
	}

	// The two corpus documents are named rather than ranked, so a spec asserts about
	// the document it means.
	hitsByDoc := func(r *Store) map[string]Hit {
		GinkgoHelper()

		res, err := r.Search(ctx, "backpressure buffer producers", 5)
		Expect(err).ToNot(HaveOccurred())

		out := map[string]Hit{}
		for _, h := range res.Hits {
			out[filepath.Base(h.DocPath)] = h
		}

		return out
	}

	docsByDoc := func(r *Store) map[string]MatchedDoc {
		GinkgoHelper()

		res, err := r.Enumerate(ctx, "backpressure", EnumerateOptions{})
		Expect(err).ToNot(HaveOccurred())

		out := map[string]MatchedDoc{}
		for _, d := range res.Docs {
			out[filepath.Base(d.Path)] = d
		}

		return out
	}

	It("renders a mapped citation for a search hit, anchored at the chunk's heading", func() {
		hits := hitsByDoc(reader(cfg))
		Expect(hits).To(HaveKey("guide.md"))

		hit := hits["guide.md"]
		Expect(hit.Ordinal).To(Equal(1))
		Expect(hit.HeadingPath).To(Equal("Guide > Backpressure"))
		Expect(hit.MappedCitation).To(Equal("https://docs.example.net/guide?c=1#backpressure"))
		Expect(hit.Mapped).To(BeTrue())

		// The raw token is kept alongside it rather than replaced.
		Expect(hit.Citation).To(Equal(Citation(hit.DocPath, 1)))
	})

	// Enumeration never loads a heading, so the anchor renders empty and the mapped
	// citation names the document at its first matching chunk.
	It("renders a document-level citation for an enumerate row", func() {
		docs := docsByDoc(reader(cfg))
		Expect(docs).To(HaveKey("guide.md"))

		doc := docs["guide.md"]
		Expect(doc.Citation).To(Equal(Citation(doc.Path, 1)))
		Expect(doc.MappedCitation).To(Equal("https://docs.example.net/guide?c=1"))
		Expect(doc.Mapped).To(BeTrue())
	})

	It("keeps the raw token for a document in the same corpus that no rule matches", func() {
		r := reader(cfg)

		hits := hitsByDoc(r)
		Expect(hits).To(HaveKey("notes.md"))
		Expect(hits["notes.md"].MappedCitation).To(Equal(hits["notes.md"].Citation))
		Expect(hits["notes.md"].Mapped).To(BeFalse())

		docs := docsByDoc(r)
		Expect(docs).To(HaveKey("notes.md"))
		Expect(docs["notes.md"].MappedCitation).To(Equal(docs["notes.md"].Citation))
		Expect(docs["notes.md"].Mapped).To(BeFalse())
	})

	It("leaves every mapped citation at the raw citation when no rules are configured", func() {
		r := reader(lexicalConfig(storeD))

		hits := hitsByDoc(r)
		Expect(hits).To(HaveLen(2))
		for _, h := range hits {
			Expect(h.MappedCitation).To(Equal(h.Citation))
			Expect(h.Mapped).To(BeFalse())
		}

		docs := docsByDoc(r)
		Expect(docs).To(HaveLen(2))
		for _, d := range docs {
			Expect(d.MappedCitation).To(Equal(d.Citation))
			Expect(d.Mapped).To(BeFalse())
		}
	})

	// A listing has no ordinal to cite a document at, so the same rule renders its
	// document-level form: knowledge sources differs from knowledge match here, and
	// only because this rule uses ${ordinal}.
	It("renders the document-level form of the same rule for a listing", func() {
		m := reader(cfg).CitationMapper()

		citation, mapped := m.RenderDocument(filepath.Join(docsD, "published/guide.md"))
		Expect(citation).To(Equal("https://docs.example.net/guide?c="))
		Expect(mapped).To(BeTrue())

		unpublished := filepath.Join(docsD, "private/notes.md")
		citation, mapped = m.RenderDocument(unpublished)
		Expect(citation).To(Equal(unpublished))
		Expect(mapped).To(BeFalse())
	})
})
