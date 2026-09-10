//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package rag

import (
	"strings"
	"unicode/utf8"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Chunking", func() {
	It("builds a heading breadcrumb and keeps it out of the body", func() {
		md := "# Design\n\n## Backpressure\n\nThe buffer fills and producers slow down.\n"
		chunks := ChunkDocument(md)

		Expect(chunks).To(HaveLen(1))
		Expect(chunks[0].HeadingPath).To(Equal("Design > Backpressure"))
		Expect(chunks[0].Body).To(Equal("The buffer fills and producers slow down."))

		// The breadcrumb lives in one column only. Folding it back into the body is
		// what made body-only questions unanswerable, let a phrase match across the
		// join between a heading and a body, and rendered the breadcrumb twice on
		// every surface that prints both.
		Expect(chunks[0].Body).ToNot(ContainSubstring("Design"))
		Expect(chunks[0].Body).ToNot(ContainSubstring("Backpressure"))
	})

	It("keeps a fenced code block intact even when it exceeds the chunk size", func() {
		var b strings.Builder
		b.WriteString("# Code\n\n```go\n")
		for i := 0; i < 200; i++ {
			b.WriteString("line of code that is fairly long to push past the target size\n")
		}
		b.WriteString("```\n")

		chunks := ChunkDocument(b.String())

		fenced := 0
		for _, c := range chunks {
			if strings.Contains(c.Body, "```go") {
				fenced++
				Expect(strings.Count(c.Body, "```")).To(Equal(2), "the fence must open and close in the same chunk")
			}
		}
		Expect(fenced).To(Equal(1))
	})

	It("hard-splits an oversized paragraph on a rune boundary", func() {
		// The arrow's three bytes sit at 1199, 1200 and 1201, so a split at the target
		// size lands inside it. Cut there, both halves are invalid UTF-8 and the
		// embedder refuses the whole batch, which is what failed an index of the NATS
		// wiki on one table.
		block := strings.Repeat("a", targetChunkBytes-1) + "→" + strings.Repeat("b", maxChunkBytes)
		chunks := ChunkDocument("# Wide\n\n" + block + "\n")

		Expect(len(chunks)).To(BeNumerically(">", 1))

		var joined strings.Builder
		for _, c := range chunks {
			Expect(utf8.ValidString(c.Body)).To(BeTrue(), "a chunk body reaches the embedder and must be valid UTF-8")
			joined.WriteString(c.Body)
		}

		// Backing off to the boundary must move the split, not drop the bytes before it.
		Expect(joined.String()).To(Equal(block))
	})

	It("does not treat a # inside a code fence as a heading", func() {
		md := "# Real\n\n```\n# not a heading\n```\n\nbody\n"
		chunks := ChunkDocument(md)

		for _, c := range chunks {
			Expect(c.HeadingPath).ToNot(ContainSubstring("not a heading"))
		}
	})

	It("packs multiple small sections and returns the first heading as the title", func() {
		md := "# Top\n\nintro paragraph\n\n## A\n\nalpha\n\n## B\n\nbravo\n"
		Expect(DocumentTitle(md)).To(Equal("Top"))
		Expect(ChunkDocument(md)).ToNot(BeEmpty())
	})

	It("returns no chunks for empty or whitespace-only input", func() {
		Expect(ChunkDocument("")).To(BeEmpty())
		Expect(ChunkDocument("   \n\n  \n")).To(BeEmpty())
	})
})
