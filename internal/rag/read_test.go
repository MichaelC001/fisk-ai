//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package rag

import (
	"context"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
)

var _ = Describe("ParseCitation", func() {
	It("splits a citation into its path and ordinal", func() {
		path, ordinal, err := ParseCitation("docs/design.md#3")
		Expect(err).ToNot(HaveOccurred())
		Expect(path).To(Equal("docs/design.md"))
		Expect(ordinal).To(Equal(3))
	})

	// A path may hold a "#" of its own, so the last one is the separator.
	It("splits on the last separator", func() {
		path, ordinal, err := ParseCitation("docs/c#sharp.md#7")
		Expect(err).ToNot(HaveOccurred())
		Expect(path).To(Equal("docs/c#sharp.md"))
		Expect(ordinal).To(Equal(7))
	})

	It("names the accepted form for a token it cannot split", func() {
		_, _, err := ParseCitation("docs/design.md")
		Expect(err).To(MatchError(ContainSubstring("<relpath>#<ordinal>")))

		for _, bad := range []string{"#3", "docs/design.md#", "docs/design.md#x", "docs/design.md#-1"} {
			_, _, err := ParseCitation(bad)
			Expect(err).To(MatchError(ContainSubstring("malformed")), bad)
		}
	})

	It("is the inverse of Citation", func() {
		path, ordinal, err := ParseCitation(Citation("docs/a b.md", 12))
		Expect(err).ToNot(HaveOccurred())
		Expect(path).To(Equal("docs/a b.md"))
		Expect(ordinal).To(Equal(12))
	})
})

var _ = Describe("Reading chunks by citation", func() {
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

	// readQuota is half the injection budget in characters, so maxTokens 1100 affords
	// 2200 characters: a 700-byte chunk and two more at 702 with their joins, where a
	// fourth would come to 2806.
	newConfig := func(maxTokens int) *config.Config {
		return &config.Config{
			Identity: "test",
			Harness: config.HarnessConfig{
				RAG: &config.RAGConfig{
					Enabled:           true,
					Directory:         filepath.Join(tmp, "knowledge"),
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

	read := func(citation string, before, after int) *ReadResult {
		GinkgoHelper()

		r, err := Open(cfg, "", Options{})
		Expect(err).ToNot(HaveOccurred())
		defer r.Close()

		res, err := r.ReadChunks(ctx, citation, ReadOptions{Before: before, After: after})
		Expect(err).ToNot(HaveOccurred())

		return res
	}

	readErr := func(citation string) error {
		GinkgoHelper()

		r, err := Open(cfg, "", Options{})
		Expect(err).ToNot(HaveOccurred())
		defer r.Close()

		_, err = r.ReadChunks(ctx, citation, ReadOptions{})

		return err
	}

	Describe("a citation the index does not hold", func() {
		var path string

		BeforeEach(func() {
			cfg = newConfig(6000)
			path = filepath.ToSlash(writeDoc(docsD, "notes.md", expandParas("n", 3)))
			index()
		})

		It("reports an ordinal past the end of the document", func() {
			Expect(readErr(path + "#9")).To(MatchError(ErrCitationNotFound))
		})

		It("reports a document the index never walked", func() {
			Expect(readErr("docs/absent.md#0")).To(MatchError(ErrCitationNotFound))
		})

		// A malformed token never reaches the index, and the error says what the form
		// is rather than that nothing matched it.
		It("reports a malformed token as malformed", func() {
			err := readErr("notes.md")
			Expect(err).ToNot(MatchError(ErrCitationNotFound))
			Expect(err).To(MatchError(ContainSubstring("<relpath>#<ordinal>")))
		})

		It("reports an index that was never built", func() {
			cfg = newConfig(6000)
			cfg.Harness.RAG.Directory = filepath.Join(tmp, "empty")

			Expect(readErr("notes.md#0")).To(MatchError(ErrIndexNotBuilt))
		})
	})

	Describe("the edges of a document", func() {
		var path string

		BeforeEach(func() {
			cfg = newConfig(6000)
			path = filepath.ToSlash(writeDoc(docsD, "notes.md", expandParas("n", 4)))
			index()
		})

		It("returns the cited chunk alone when nothing either side is asked for", func() {
			res := read(path+"#1", 0, 0)
			Expect(res.Content).To(Equal(expandPara("n1")))
			Expect(res.Span).To(BeEmpty())
			Expect(res.First).To(Equal(1))
			Expect(res.Last).To(Equal(1))
			Expect(res.Citation).To(Equal(path + "#1"))
			Expect(res.DocumentChunks).To(Equal(4))
		})

		It("stops at the start of the document and spends nothing upward", func() {
			res := read(path+"#0", 3, 1)
			Expect(res.First).To(Equal(0))
			Expect(res.Last).To(Equal(1))
			Expect(res.Span).To(Equal(path + "#0..#1"))
			Expect(res.Truncated).To(BeFalse())
			Expect(res.Content).To(ContainSubstring("n1 "))
			Expect(res.Content).ToNot(ContainSubstring("n2 "))
		})

		It("stops at the end of the document", func() {
			res := read(path+"#3", 1, 5)
			Expect(res.First).To(Equal(2))
			Expect(res.Last).To(Equal(3))
			Expect(res.Span).To(Equal(path + "#2..#3"))
			Expect(res.Truncated).To(BeFalse())
			Expect(res.Content).To(ContainSubstring("n2 "))
		})

		It("joins the chunks it took with a blank line", func() {
			res := read(path+"#1", 1, 1)
			Expect(res.Content).To(Equal(strings.Join([]string{expandPara("n0"), expandPara("n1"), expandPara("n2")}, "\n\n")))
		})
	})

	Describe("a heading boundary", func() {
		var path string

		BeforeEach(func() {
			cfg = newConfig(6000)

			// The five chunks the expansion specs use: prose under Guide, two under
			// Guide > Setup, one under Guide > Setup > Details, one under Guide > Other.
			doc := strings.Join([]string{
				"# Guide", "", expandPara("intro"), "",
				"## Setup", "", expandPara("setupone"), "", expandPara("needle"), "",
				"### Details", "", expandPara("detail"), "",
				"## Other", "", expandPara("other"), "",
			}, "\n")

			path = filepath.ToSlash(writeDoc(docsD, "guide.md", doc))
			index()
		})

		// Expansion stops at the sibling and at the parent's prose. A read asked for
		// them returns them: the model named the range, so nothing here is a walk
		// wandering out of the section it started in.
		It("crosses into the sibling section and the parent's prose", func() {
			res := read(path+"#2", 2, 2)
			Expect(res.First).To(Equal(0))
			Expect(res.Last).To(Equal(4))
			Expect(res.Span).To(Equal(path + "#0..#4"))
			Expect(res.Content).To(ContainSubstring("intro "))
			Expect(res.Content).To(ContainSubstring("other "))
		})

		// The breadcrumb answers "where does this citation sit", so it stays the cited
		// chunk's own even where the block reaches past it.
		It("carries the cited chunk's breadcrumb for the whole block", func() {
			res := read(path+"#2", 2, 2)
			Expect(res.HeadingPath).To(Equal("Guide > Setup"))
		})
	})

	Describe("the cap on one call", func() {
		// Nine chunks and a quota of 2200 characters, so the three the quota affords
		// are returned and the rest of the request is reported rather than dropped.
		It("stops at the quota and says so", func() {
			cfg = newConfig(1100)
			path := filepath.ToSlash(writeDoc(docsD, "long.md", expandParas("q", 9)))
			index()

			res := read(path+"#4", 3, 3)
			Expect(res.Truncated).To(BeTrue())
			Expect(res.First).To(Equal(3))
			Expect(res.Last).To(Equal(5))
			Expect(res.Span).To(Equal(path + "#3..#5"))
			Expect(res.Content).To(HaveLen(3*expandParaBytes + 2*len("\n\n")))
		})

		// A read that asked for nothing beyond the cited chunk is never reported as cut
		// short, whatever that chunk costs.
		It("reports a read of the cited chunk alone as whole", func() {
			cfg = newConfig(1)
			path := filepath.ToSlash(writeDoc(docsD, "long.md", expandParas("q", 3)))
			index()

			res := read(path+"#1", 0, 0)
			Expect(res.Content).To(Equal(expandPara("q1")))
			Expect(res.Truncated).To(BeFalse())
		})

		// A quota of 4 characters affords no neighbor, so the block is the cited chunk
		// on its own and carries no span to read on from.
		It("returns the cited chunk even when it alone exceeds the quota", func() {
			cfg = newConfig(1)
			path := filepath.ToSlash(writeDoc(docsD, "long.md", expandParas("q", 3)))
			index()

			res := read(path+"#1", 1, 1)
			Expect(res.Content).To(Equal(expandPara("q1")))
			Expect(res.Truncated).To(BeTrue())
			Expect(res.First).To(Equal(1))
			Expect(res.Last).To(Equal(1))
			Expect(res.Span).To(BeEmpty())
		})
	})
})
