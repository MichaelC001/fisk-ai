//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// These tests cover llm.DeltaAssembler from outside the package, which is where every
// consumer of it sits: the adapters that render a delta stream and an embedder's own
// sink.
package llm_test

import (
	"strings"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/llm"
)

var _ = Describe("DeltaAssembler", func() {
	It("Should assemble the fragments of an index in the order they arrive", func() {
		var a llm.DeltaAssembler

		a.AddDelta(llm.Delta{Kind: llm.DeltaText, Index: 0, Text: "the answer "})
		a.AddDelta(llm.Delta{Kind: llm.DeltaText, Index: 0, Text: "is "})
		a.AddDelta(llm.Delta{Kind: llm.DeltaText, Index: 0, Text: "42", Final: true})

		Expect(a.Blocks()).To(Equal([]llm.AssembledBlock{
			{Kind: llm.DeltaText, Index: 0, Text: "the answer is 42", Source: llm.SourceFragments},
		}))
	})

	It("Should key on the index when two blocks interleave, and report them in index order", func() {
		var a llm.DeltaAssembler

		a.AddDelta(llm.Delta{Kind: llm.DeltaThinking, Index: 1, Text: "second "})
		a.AddDelta(llm.Delta{Kind: llm.DeltaText, Index: 0, Text: "first "})
		a.AddDelta(llm.Delta{Kind: llm.DeltaThinking, Index: 1, Text: "block", Final: true})
		a.AddDelta(llm.Delta{Kind: llm.DeltaText, Index: 0, Text: "block", Final: true})

		Expect(a.Blocks()).To(Equal([]llm.AssembledBlock{
			{Kind: llm.DeltaText, Index: 0, Text: "first block", Source: llm.SourceFragments},
			{Kind: llm.DeltaThinking, Index: 1, Text: "second block", Source: llm.SourceFragments},
		}))
	})

	It("Should take the whole block over the fragments it was streamed as", func() {
		var a llm.DeltaAssembler

		a.AddDelta(llm.Delta{Kind: llm.DeltaText, Index: 0, Text: "the answer is 4"})

		got := a.AddBlock(llm.WholeBlock{Kind: llm.DeltaText, Index: 0, Text: "the answer is 42"})
		Expect(got).To(Equal(llm.AssembledBlock{Kind: llm.DeltaText, Index: 0, Text: "the answer is 42", Source: llm.SourceBlock}))
		Expect(a.Blocks()).To(Equal([]llm.AssembledBlock{got}))
	})

	It("Should keep the fragments when the whole block arrived trimmed", func() {
		var a llm.DeltaAssembler

		a.AddDelta(llm.Delta{Kind: llm.DeltaText, Index: 0, Text: "the whole answer, every word of it"})

		got := a.AddBlock(llm.WholeBlock{Kind: llm.DeltaText, Index: 0, Text: "the whole answer [trimmed]", Trimmed: true})
		Expect(got).To(Equal(llm.AssembledBlock{
			Kind:   llm.DeltaText,
			Index:  0,
			Text:   "the whole answer, every word of it",
			Source: llm.SourceKeptFragments,
		}))
	})

	// The rule does not compare lengths, and cannot: a trimmed block is the cut text plus
	// a marker this package does not know the size of, so a trimmed copy of an answer
	// just over the limit is longer than the complete one. One fragment is enough to hold
	// the index.
	It("Should keep one fragment over a trimmed block holding far more text", func() {
		var a llm.DeltaAssembler

		a.AddDelta(llm.Delta{Kind: llm.DeltaText, Index: 0, Text: "the"})

		got := a.AddBlock(llm.WholeBlock{Kind: llm.DeltaText, Index: 0, Text: strings.Repeat("x", 4096), Trimmed: true})
		Expect(got.Text).To(Equal("the"))
		Expect(got.Source).To(Equal(llm.SourceKeptFragments))
	})

	It("Should take a block for an index that streamed no fragments, trimmed or not", func() {
		var a llm.DeltaAssembler

		whole := a.AddBlock(llm.WholeBlock{Kind: llm.DeltaText, Index: 0, Text: "unstreamed"})
		Expect(whole).To(Equal(llm.AssembledBlock{Kind: llm.DeltaText, Index: 0, Text: "unstreamed", Source: llm.SourceBlock}))

		// Nothing streamed for this index, so the cut copy is the only copy and reporting
		// the fragments would report nothing at all.
		trimmed := a.AddBlock(llm.WholeBlock{Kind: llm.DeltaThinking, Index: 1, Text: "cut [trimmed]", Trimmed: true})
		Expect(trimmed).To(Equal(llm.AssembledBlock{Kind: llm.DeltaThinking, Index: 1, Text: "cut [trimmed]", Source: llm.SourceBlock}))
	})

	It("Should report an index whose whole block never arrived as fragments", func() {
		var a llm.DeltaAssembler

		a.AddDelta(llm.Delta{Kind: llm.DeltaText, Index: 0, Text: "half an ans"})
		a.AddBlock(llm.WholeBlock{Kind: llm.DeltaThinking, Index: 1, Text: "reasoning"})

		Expect(a.Blocks()).To(Equal([]llm.AssembledBlock{
			{Kind: llm.DeltaText, Index: 0, Text: "half an ans", Source: llm.SourceFragments},
			{Kind: llm.DeltaThinking, Index: 1, Text: "reasoning", Source: llm.SourceBlock},
		}))
	})

	It("Should let the whole block name the kind of an index its fragments called something else", func() {
		var a llm.DeltaAssembler

		a.AddDelta(llm.Delta{Kind: llm.DeltaThinking, Index: 0, Text: "reasoning"})

		got := a.AddBlock(llm.WholeBlock{Kind: llm.DeltaText, Index: 0, Text: "answer"})
		Expect(got.Kind).To(Equal(llm.DeltaText))
	})

	It("Should hold a whole block against a fragment that arrives after it", func() {
		var a llm.DeltaAssembler

		a.AddBlock(llm.WholeBlock{Kind: llm.DeltaText, Index: 0, Text: "the answer is 42"})
		a.AddDelta(llm.Delta{Kind: llm.DeltaText, Index: 0, Text: " and then some"})

		Expect(a.Blocks()).To(Equal([]llm.AssembledBlock{
			{Kind: llm.DeltaText, Index: 0, Text: "the answer is 42", Source: llm.SourceBlock},
		}))
	})

	It("Should drop every index on Reset so the next call starts at 0 again", func() {
		var a llm.DeltaAssembler

		a.AddDelta(llm.Delta{Kind: llm.DeltaText, Index: 0, Text: "first call"})
		a.AddBlock(llm.WholeBlock{Kind: llm.DeltaText, Index: 0, Text: "first call"})
		a.Reset()

		Expect(a.Blocks()).To(BeEmpty())

		a.AddDelta(llm.Delta{Kind: llm.DeltaText, Index: 0, Text: "second call"})
		Expect(a.Blocks()).To(Equal([]llm.AssembledBlock{
			{Kind: llm.DeltaText, Index: 0, Text: "second call", Source: llm.SourceFragments},
		}))
	})

	It("Should serve a reader on another goroutine while fragments arrive", func() {
		var (
			a  llm.DeltaAssembler
			wg sync.WaitGroup
		)

		wg.Add(2)

		go func() {
			defer GinkgoRecover()
			defer wg.Done()

			for range 500 {
				a.AddDelta(llm.Delta{Kind: llm.DeltaText, Index: 0, Text: "x"})
			}
		}()

		go func() {
			defer GinkgoRecover()
			defer wg.Done()

			for range 500 {
				for _, b := range a.Blocks() {
					Expect(len(b.Text)).To(BeNumerically("<=", 500))
				}
			}
		}()

		wg.Wait()

		blocks := a.Blocks()
		Expect(blocks).To(HaveLen(1))
		Expect(blocks[0].Text).To(HaveLen(500))
	})
})
