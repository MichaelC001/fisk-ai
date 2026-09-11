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

// expandParaBytes is the size of every paragraph these specs write. Two of them
// come to 1404 bytes, past targetChunkBytes, so the packer gives each paragraph a
// chunk of its own and an ordinal is a paragraph.
const expandParaBytes = 700

// expandPara returns a paragraph of exactly expandParaBytes bytes opening with
// word, so the chunk count and the quota arithmetic are both exact and each chunk
// carries a word the specs can look for.
func expandPara(word string) string {
	return word + " " + strings.Repeat("x", expandParaBytes-len(word)-1)
}

// expandParas returns n paragraphs named <prefix>0 to <prefix>n-1, separated by the
// blank line that delimits a block.
func expandParas(prefix string, n int) string {
	out := make([]string, n)
	for i := range out {
		out[i] = expandPara(fmt.Sprintf("%s%d", prefix, i))
	}

	return strings.Join(out, "\n\n")
}

var _ = Describe("inSection", func() {
	It("takes the hit's own section and the subsections under it", func() {
		Expect(inSection("Setup", "Setup")).To(BeTrue())
		Expect(inSection("Setup > Notes", "Setup")).To(BeTrue())
		Expect(inSection("Setup > Notes > Detail", "Setup")).To(BeTrue())
	})

	It("leaves a sibling and an ancestor out", func() {
		Expect(inSection("Setup", "Setup > Notes")).To(BeFalse())
		Expect(inSection("Setup > Other", "Setup > Notes")).To(BeFalse())
		Expect(inSection("Guide", "Guide > Setup")).To(BeFalse())
	})

	// The separator is part of the prefix, so a heading that merely starts with the
	// hit's text is a different section.
	It("does not read a heading sharing a prefix as nested", func() {
		Expect(inSection("Setup Notes", "Setup")).To(BeFalse())
		Expect(inSection("Setupery", "Setup")).To(BeFalse())
	})

	// Every breadcrumb in a document with no headings is empty, so the quota is the
	// only limit on a walk through one.
	It("takes every chunk of a document with no headings", func() {
		Expect(inSection("", "")).To(BeTrue())
		Expect(inSection("Setup", "")).To(BeFalse())
	})
})

var _ = Describe("resolvedExpandToSection", func() {
	on, off := true, false

	It("takes the configured key when the caller passes no override", func() {
		Expect(resolvedExpandToSection(&config.RAGConfig{}, nil)).To(BeFalse())
		Expect(resolvedExpandToSection(&config.RAGConfig{ExpandToSection: true}, nil)).To(BeTrue())
	})

	It("takes the override in both directions", func() {
		Expect(resolvedExpandToSection(&config.RAGConfig{}, &on)).To(BeTrue())
		Expect(resolvedExpandToSection(&config.RAGConfig{ExpandToSection: true}, &off)).To(BeFalse())
	})
})

var _ = Describe("Expanding a hit to its section", func() {
	ctx := context.Background()

	var (
		tmp   string
		docsD string
		cfg   *config.Config
	)

	BeforeEach(func() {
		tmp = GinkgoT().TempDir()
		docsD = filepath.Join(tmp, "docs")
	})

	// topK divides the injection budget into the per-hit quota, so topK 4 with
	// maxTokens 2400 gives each hit 2400 characters: its own 700-byte chunk and two
	// more, since a fourth would come to 2800.
	newConfig := func(topK, maxTokens int) *config.Config {
		return &config.Config{
			Identity: "test",
			Harness: config.HarnessConfig{
				RAG: &config.RAGConfig{
					Enabled:           true,
					Directory:         filepath.Join(tmp, "knowledge"),
					TopK:              topK,
					MaxInjectedTokens: maxTokens,
				},
			},
		}
	}

	index := func() {
		GinkgoHelper()

		w, err := OpenWriter(cfg, "", Options{})
		Expect(err).ToNot(HaveOccurred())
		defer w.Close()

		_, err = w.Index(ctx, []string{docsD}, IndexOptions{Reconcile: true})
		Expect(err).ToNot(HaveOccurred())
	}

	search := func(query string, expand bool) []Hit {
		GinkgoHelper()

		r, err := Open(cfg, "", Options{ExpandToSection: &expand})
		Expect(err).ToNot(HaveOccurred())
		defer r.Close()

		res, err := r.Search(ctx, query, 0)
		Expect(err).ToNot(HaveOccurred())
		Expect(res.Status).To(Equal(StatusOK))

		return res.Hits
	}

	Describe("the section boundary", func() {
		var path string

		BeforeEach(func() {
			cfg = newConfig(4, 6000)

			// Five chunks: the prose under Guide, two under Guide > Setup, one under the
			// subsection Guide > Setup > Details, and one under the sibling Guide > Other.
			doc := strings.Join([]string{
				"# Guide", "", expandPara("intro"), "",
				"## Setup", "", expandPara("setupone"), "", expandPara("needle"), "",
				"### Details", "", expandPara("detail"), "",
				"## Other", "", expandPara("other"), "",
			}, "\n")

			chunks := ChunkDocument(doc)
			Expect(chunks).To(HaveLen(5))
			Expect(chunks[0].HeadingPath).To(Equal("Guide"))
			Expect(chunks[2].HeadingPath).To(Equal("Guide > Setup"))
			Expect(chunks[3].HeadingPath).To(Equal("Guide > Setup > Details"))
			Expect(chunks[4].HeadingPath).To(Equal("Guide > Other"))

			path = filepath.ToSlash(writeDoc(docsD, "guide.md", doc))
			index()
		})

		// The quota here is 6000 characters against a document of 3500, so what the
		// walk stops at is the boundary alone.
		It("walks through a subsection and stops at the sibling and the parent's prose", func() {
			hits := search("needle", true)
			Expect(hits).To(HaveLen(1))

			Expect(hits[0].Citation).To(Equal(path + "#2"))
			Expect(hits[0].Span).To(Equal(path + "#1..#3"))

			Expect(hits[0].Content).To(ContainSubstring("setupone "))
			Expect(hits[0].Content).To(ContainSubstring("needle "))
			Expect(hits[0].Content).To(ContainSubstring("detail "))
			Expect(hits[0].Content).ToNot(ContainSubstring("intro "))
			Expect(hits[0].Content).ToNot(ContainSubstring("other "))
		})

		// The span is a claim about the text beside it, so it is checked against the
		// size of what came back: three chunks joined by a blank line.
		It("spans exactly what the content covers", func() {
			hits := search("needle", true)
			Expect(hits[0].Content).To(HaveLen(3*expandParaBytes + 2*len("\n\n")))
		})

		It("returns the chunk that ranked and no span with expansion off", func() {
			hits := search("needle", false)
			Expect(hits).To(HaveLen(1))

			Expect(hits[0].Span).To(BeEmpty())
			Expect(hits[0].Content).To(Equal(expandPara("needle")))
		})
	})

	Describe("the quota", func() {
		// Every breadcrumb in a document with no headings is empty, so every chunk is
		// in the hit's section and only the quota stops the walk.
		It("limits a walk through a document with no headings", func() {
			cfg = newConfig(4, 2400)

			doc := expandParas("m", 7)
			Expect(ChunkDocument(doc)).To(HaveLen(7))

			path := filepath.ToSlash(writeDoc(docsD, "notes.md", strings.Replace(doc, "m3 ", "m3 middleneedle ", 1)))
			index()

			hits := search("middleneedle", true)
			Expect(hits).To(HaveLen(1))
			Expect(hits[0].Span).To(Equal(path + "#2..#4"))
			Expect(hits[0].Content).To(ContainSubstring("m2 "))
			Expect(hits[0].Content).To(ContainSubstring("m4 "))
			Expect(hits[0].Content).ToNot(ContainSubstring("m1 "))
			Expect(hits[0].Content).ToNot(ContainSubstring("m5 "))
		})

		// A hit at ordinal 0 has nothing above it, so the two chunks the quota affords
		// are both taken below.
		It("spends downward for a hit at ordinal 0", func() {
			cfg = newConfig(4, 2400)

			doc := expandParas("f", 7)
			path := filepath.ToSlash(writeDoc(docsD, "notes.md", strings.Replace(doc, "f0 ", "f0 firstneedle ", 1)))
			index()

			hits := search("firstneedle", true)
			Expect(hits).To(HaveLen(1))
			Expect(hits[0].Citation).To(Equal(path + "#0"))
			Expect(hits[0].Span).To(Equal(path + "#0..#2"))
			Expect(hits[0].Content).To(ContainSubstring("f2 "))
			Expect(hits[0].Content).ToNot(ContainSubstring("f3 "))
		})

		// The chunk that ranked is what the model was given the result for, so a
		// section the quota cannot hold is truncated around it rather than from the top.
		It("truncates a section longer than the quota around the hit", func() {
			cfg = newConfig(4, 2400)

			body := expandParas("s", 9)
			doc := "# Big\n\n## Long\n\n" + strings.Replace(body, "s4 ", "s4 longneedle ", 1) + "\n"
			path := filepath.ToSlash(writeDoc(docsD, "long.md", doc))
			index()

			hits := search("longneedle", true)
			Expect(hits).To(HaveLen(1))
			Expect(hits[0].HeadingPath).To(Equal("Big > Long"))
			Expect(hits[0].Span).To(Equal(path + "#3..#5"))
			Expect(hits[0].Content).To(ContainSubstring("longneedle "))
			Expect(hits[0].Content).ToNot(ContainSubstring("s2 "))
			Expect(hits[0].Content).ToNot(ContainSubstring("s6 "))
		})
	})

	Describe("two hits in one section", func() {
		It("returns one block carrying no chunk twice", func() {
			cfg = newConfig(4, 2400)

			doc := expandParas("p", 7)
			doc = strings.Replace(doc, "p2 ", "p2 pairneedle ", 1)
			doc = strings.Replace(doc, "p3 ", "p3 pairneedle ", 1)
			writeDoc(docsD, "pair.md", doc)
			index()

			// Both chunks rank, so without the merge the block around each would return
			// the other's text a second time.
			Expect(search("pairneedle", false)).To(HaveLen(2))

			hits := search("pairneedle", true)
			Expect(hits).To(HaveLen(1))
			Expect(strings.Count(hits[0].Content, "p2 ")).To(Equal(1))
			Expect(strings.Count(hits[0].Content, "p3 ")).To(Equal(1))
			Expect(strings.Count(hits[0].Content, "pairneedle ")).To(Equal(2))
		})
	})
})
