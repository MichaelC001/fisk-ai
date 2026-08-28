//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package rag

import (
	"fmt"
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
		func(rules []config.RAGCitationRule, path string, ordinal int, headingPath string, address string, matched bool) {
			got, ok := NewCitationMapper(rules).Render(path, ordinal, headingPath)

			Expect(got).To(Equal(address))
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

		// A single pass is what stops the corpus writing the template: the file is
		// named ${anchor}.md on disk and the capture must not be filled from the
		// document's own heading.
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

	Describe("the replacement grammar", func() {
		// config validates a replacement with a transcription of regexp's own
		// expand, verified against ExpandString. The expander has to resolve exactly
		// what that scanner validated, or a config accepted at load renders wrong at
		// run time, so hold it against the same reference.
		It("resolves the same references as regexp.Regexp.ExpandString", func() {
			re := regexp.MustCompile(`(?P<sec>[a-z]+)/(\d+)`)
			path := "guide/42"

			match := re.FindStringSubmatchIndex(path)
			Expect(match).ToNot(BeNil())

			// Every template up to five characters over an alphabet that spells the
			// whole grammar: the dollar, both braces, a digit, an underscore, a
			// character no name can hold, and the letters of this pattern's group
			// name. It cannot spell ordinal, heading or anchor, so no reserved name is
			// reachable and ExpandString is the whole answer, and every group it can
			// name captures alphanumerics, which percent-encoding leaves alone.
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
					got := expandCitation(re, template, path, match, 7, "Heading")
					want := string(re.ExpandString(nil, template, path, match))
					if got != want {
						Fail(fmt.Sprintf("template %q expanded to %q, ExpandString gives %q", template, got, want))
					}
					checked++
				}
			}

			Expect(checked).To(Equal(66429))
		})
	})
})
