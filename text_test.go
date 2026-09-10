//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
	"unicode/utf8"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("wrapText", func() {
	It("Should break a line wider than width at a space", func() {
		out := wrapText("You are an assistant that can assume nothing about the host", 30)
		Expect(out).To(Equal("You are an assistant that can\nassume nothing about the host"))
	})

	It("Should leave every line the author already fitted where it is", func() {
		prompt := "You are careful.\nYou never guess.\n\n- first rule\n- second rule"
		Expect(wrapText(prompt, 30)).To(Equal(prompt))
	})

	It("Should keep a blank line between paragraphs when it wraps around it", func() {
		out := wrapText("The first paragraph runs past the width given.\n\nThe second one does too, easily.", 20)
		Expect(out).To(Equal("The first paragraph\nruns past the width\ngiven.\n\nThe second one does\ntoo, easily."))
	})

	It("Should put a word wider than width on a line of its own rather than split it", func() {
		out := wrapText("see /very/long/path/that/nobody/can/break now", 20)
		Expect(out).To(Equal("see\n/very/long/path/that/nobody/can/break\nnow"))
	})

	It("Should count runes so a multibyte line is measured as it is displayed", func() {
		Expect(wrapText("héllo wörld", 11)).To(Equal("héllo wörld"))
		Expect(wrapText("héllo wörld", 10)).To(Equal("héllo\nwörld"))
	})

	It("Should return the text unchanged for a width of zero or less", func() {
		prompt := "a line that is much wider than nothing"
		Expect(wrapText(prompt, 0)).To(Equal(prompt))
		Expect(wrapText(prompt, -8)).To(Equal(prompt))
	})
})

var _ = Describe("truncateString", func() {
	It("Should return a short string unchanged", func() {
		Expect(truncateString("hello", 10)).To(Equal("hello"))
	})

	It("Should keep a string of exactly max runes unchanged", func() {
		Expect(truncateString("hello", 5)).To(Equal("hello"))
	})

	It("Should cut and append an ellipsis when longer than max", func() {
		Expect(truncateString("hello world", 5)).To(Equal("hello..."))
	})

	It("Should count runes so multibyte text is never split mid-character", func() {
		out := truncateString("héllo wörld", 5)
		Expect(out).To(Equal("héllo..."))
		Expect(utf8.ValidString(out)).To(BeTrue())
	})
})

var _ = Describe("truncateLine", func() {
	It("Should collapse runs of whitespace to single spaces", func() {
		Expect(truncateLine("a\n\tb   c", 20)).To(Equal("a b c"))
	})

	It("Should collapse first, then truncate on the collapsed length", func() {
		Expect(truncateLine("one  two  three  four", 7)).To(Equal("one two..."))
	})

	It("Should trim leading and trailing whitespace", func() {
		Expect(truncateLine("   spaced   ", 20)).To(Equal("spaced"))
	})
})
