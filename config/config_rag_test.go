// Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Knowledge (RAG) config", func() {
	It("gates on the block and the enabled flag", func() {
		off := &Config{}
		Expect(off.RAGEnabled()).To(BeFalse())
		Expect(off.RAGVectorEnabled()).To(BeFalse())

		present := &Config{Harness: HarnessConfig{RAG: &RAGConfig{Enabled: false}}}
		Expect(present.RAGEnabled()).To(BeFalse())

		on := &Config{Harness: HarnessConfig{RAG: &RAGConfig{Enabled: true}}}
		Expect(on.RAGEnabled()).To(BeTrue())
		Expect(on.RAGVectorEnabled()).To(BeFalse(), "no embeddings block means lexical-only")
	})

	It("turns on the vector tier only when the embeddings block is present", func() {
		cfg := &Config{Harness: HarnessConfig{RAG: &RAGConfig{Enabled: true, Embeddings: &RAGEmbeddingsConfig{BaseURL: "http://127.0.0.1:1234/v1", Model: "m"}}}}
		Expect(cfg.RAGVectorEnabled()).To(BeTrue())
	})

	It("parses the embeddings timeout and defaults it when unset", func() {
		data := []byte(`
application_path: /bin/ls
identity: kb
system_prompt: hello
llm:
  model: claude-opus-4-8
harness:
  knowledge:
    enabled: true
    embeddings:
      base_url: http://127.0.0.1:1234/v1
      model: text-embedding-test
`)
		cfg, err := ParseConfig(data)
		Expect(err).ToNot(HaveOccurred())
		Expect(cfg.RAGVectorEnabled()).To(BeTrue())
		Expect(cfg.Harness.RAG.Embeddings.TimeoutParsed).To(Equal(30 * time.Second))
	})

	It("rejects a malformed embeddings timeout", func() {
		data := []byte(`
application_path: /bin/ls
identity: kb
system_prompt: hello
llm:
  model: claude-opus-4-8
harness:
  knowledge:
    enabled: true
    embeddings:
      base_url: http://127.0.0.1:1234/v1
      model: m
      timeout: not-a-duration
`)
		_, err := ParseConfig(data)
		Expect(err).To(HaveOccurred())
	})

	Describe("Citation rules", func() {
		citationConfig := func(rules string) []byte {
			return []byte(`
application_path: /bin/ls
identity: kb
system_prompt: hello
llm:
  model: claude-opus-4-8
harness:
  knowledge:
    enabled: true
    citations:
` + rules)
		}

		It("keeps the rules in order and compiles each pattern", func() {
			cfg, err := ParseConfig(citationConfig(`      - pattern: '^docs/content/(.+)/_index\.md$'
        replace: 'https://docs.example.net/$1/'
      - pattern: '^docs/content/(?P<page>.+)\.md$'
        replace: 'https://docs.example.net/${page}/#${anchor}'
`))
			Expect(err).ToNot(HaveOccurred())

			rules := cfg.RAGCitationRules()
			Expect(rules).To(HaveLen(2))
			Expect(rules[0].Pattern).To(Equal(`^docs/content/(.+)/_index\.md$`))
			Expect(rules[0].Replace).To(Equal("https://docs.example.net/$1/"))
			Expect(rules[1].Pattern).To(Equal(`^docs/content/(?P<page>.+)\.md$`))
			Expect(rules[1].Replace).To(Equal("https://docs.example.net/${page}/#${anchor}"))

			Expect(rules[0].PatternCompiled).ToNot(BeNil())
			Expect(rules[0].PatternCompiled.MatchString("docs/content/dev/_index.md")).To(BeTrue())
			Expect(rules[1].PatternCompiled).ToNot(BeNil())
			Expect(rules[1].PatternCompiled.SubexpNames()).To(ContainElement("page"))
		})

		It("leaves the rules nil when the block is absent", func() {
			cfg, err := ParseConfig([]byte(`
application_path: /bin/ls
identity: kb
system_prompt: hello
llm:
  model: claude-opus-4-8
harness:
  knowledge:
    enabled: true
`))
			Expect(err).ToNot(HaveOccurred())
			Expect(cfg.Harness.RAG.Citations).To(BeNil())
			Expect(cfg.RAGCitationRules()).To(BeNil())
		})

		It("reports no rules for a config built as a struct literal", func() {
			cfg := &Config{Harness: HarnessConfig{RAG: &RAGConfig{
				Enabled:   true,
				Citations: []RAGCitationRule{{Pattern: `^docs/(.+)$`, Replace: "https://docs.example.net/$1"}},
			}}}
			Expect(cfg.RAGCitationRules()).To(BeNil(), "an uncompiled rule cannot match, so the citation passes through")

			bare := &Config{}
			Expect(bare.RAGCitationRules()).To(BeNil())
		})

		It("rejects a pattern that does not compile, naming the rule", func() {
			_, err := ParseConfig(citationConfig(`      - pattern: '^docs/(.+)$'
        replace: 'https://docs.example.net/$1'
      - pattern: '^docs/(unclosed'
        replace: 'https://docs.example.net/'
`))
			Expect(err).To(MatchError(ContainSubstring("harness.knowledge.citations[1] pattern \"^docs/(unclosed\"")))
		})

		It("rejects a replacement naming something that is neither a group nor a supplied value", func() {
			_, err := ParseConfig(citationConfig(`      - pattern: '^docs/(?P<page>.+)\.md$'
        replace: 'https://docs.example.net/${section}/'
`))
			Expect(err).To(MatchError(ContainSubstring("harness.knowledge.citations[0] replace")))
			Expect(err).To(MatchError(ContainSubstring("$section is neither a named capture group")))
		})

		It("rejects $1x, which Go reads as a group named 1x and expands to nothing", func() {
			_, err := ParseConfig(citationConfig(`      - pattern: '^docs/(.+)\.md$'
        replace: 'https://docs.example.net/$1x/'
`))
			Expect(err).To(MatchError(ContainSubstring("$1x is neither a named capture group")))
		})

		It("rejects a numbered group beyond what the pattern captures", func() {
			_, err := ParseConfig(citationConfig(`      - pattern: '^docs/(.+)\.md$'
        replace: 'https://docs.example.net/$1/$2/'
`))
			Expect(err).To(MatchError(ContainSubstring("harness.knowledge.citations[0] replace")))
			Expect(err).To(MatchError(ContainSubstring("$2 refers to capture group 2 but the pattern")))
			Expect(err).To(MatchError(ContainSubstring("has 1")))
		})

		It("rejects a rule with no pattern, which would match every document path", func() {
			_, err := ParseConfig(citationConfig(`      - replace: 'https://docs.example.net/'
`))
			Expect(err).To(MatchError(ContainSubstring("harness.knowledge.citations[0]: pattern is required")))

			_, err = ParseConfig(citationConfig(`      - pattern: ''
        replace: 'https://docs.example.net/'
`))
			Expect(err).To(MatchError(ContainSubstring("harness.knowledge.citations[0]: pattern is required")))
		})

		It("rejects a rule with no replacement, which would map its paths to nothing", func() {
			_, err := ParseConfig(citationConfig(`      - pattern: '^docs/(.+)$'
`))
			Expect(err).To(MatchError(ContainSubstring("harness.knowledge.citations[0]: replace is required")))
			Expect(err).To(MatchError(ContainSubstring(`^docs/(.+)$`)))
		})

		It("accepts the supplied names and a literal dollar", func() {
			cfg, err := ParseConfig(citationConfig(`      - pattern: '^docs/(.+)\.md$'
        replace: 'https://docs.example.net/$1/#${anchor} ${heading} ${ordinal} $$anchor'
`))
			Expect(err).ToNot(HaveOccurred())
			Expect(cfg.RAGCitationRules()).To(HaveLen(1))
		})

		It("supplies ordinal, heading and anchor as the reserved names", func() {
			Expect(RAGCitationReservedNames()).To(Equal([]string{"ordinal", "heading", "anchor"}))

			names := RAGCitationReservedNames()
			names[0] = "mutated"
			Expect(RAGCitationReservedNames()[0]).To(Equal("ordinal"), "the caller gets a copy")
		})
	})
})
