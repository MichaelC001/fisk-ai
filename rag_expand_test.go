//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/choria-io/fisk"
	"github.com/choria-io/ui/columns"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/rag"
)

var _ = Describe("knowledge search expansion", func() {
	Describe("the --expand flag", func() {
		// The parse runs the action, which fails on the missing configuration file. The
		// flag values are set before that, so these specs read them off the parse and
		// never reach the index.
		parse := func(args ...string) {
			GinkgoHelper()

			knowledgeExpand = false
			knowledgeExpandSet = false

			app := fisk.New("fisk", "an agent")
			registerRAGCommand(app)

			_, err := app.Parse(append([]string{"knowledge", "--config", "/nonexistent/agent.yaml", "search", "backpressure"}, args...))
			Expect(err).To(HaveOccurred())
		}

		It("asks for expansion under --expand", func() {
			parse("--expand")

			opt := knowledgeExpandOption()
			Expect(opt).ToNot(BeNil())
			Expect(*opt).To(BeTrue())
		})

		// --no-expand exists so an operator who set the key can run the same query both
		// ways and compare.
		It("refuses expansion under --no-expand", func() {
			parse("--no-expand")

			opt := knowledgeExpandOption()
			Expect(opt).ToNot(BeNil())
			Expect(*opt).To(BeFalse())
		})

		// No override, so the store takes harness.knowledge.expand_to_section, whichever
		// way the operator set it.
		It("overrides nothing when neither is typed", func() {
			parse()

			Expect(knowledgeExpandOption()).To(BeNil())
		})
	})

	Describe("rendering an expanded hit", func() {
		render := func(h rag.Hit) string {
			c := columns.New()
			renderSearchHits(c, []rag.Hit{h}, false)

			return c.String()
		}

		It("gives the range an expanded hit covers its own line", func() {
			out := render(rag.Hit{Citation: "docs/design.md#7", Ordinal: 7, Span: "docs/design.md#5..#9"})

			Expect(out).To(ContainSubstring("docs/design.md#7"))
			// The label and the value are matched apart because columns renders the
			// item as markdown when it detects an LLM running the command and as
			// plain text otherwise.
			Expect(out).To(ContainSubstring("Span:"))
			Expect(out).To(ContainSubstring("docs/design.md#5..#9"))
		})

		It("prints no span line for a hit that did not grow", func() {
			out := render(rag.Hit{Citation: "docs/design.md#7", Ordinal: 7})

			Expect(out).To(ContainSubstring("docs/design.md#7"))
			Expect(out).ToNot(ContainSubstring("Span:"))
		})
	})
})
