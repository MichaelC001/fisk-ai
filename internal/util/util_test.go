//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package util

import (
	"os"
	"unicode/utf8"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("RenderMarkdownTo", func() {
	// The test process writes to a pipe or a file rather than a terminal, so this is
	// also the path a piped or redirected run takes.
	It("Should return the markdown unchanged when the destination is not a terminal", func() {
		Expect(RenderMarkdownTo("# Title\n\nhello", os.Stdout, false)).To(Equal("# Title\n\nhello"))
	})

	It("Should return the markdown unchanged when color is off", func() {
		Expect(RenderMarkdownTo("# Title\n\nhello", os.Stdout, true)).To(Equal("# Title\n\nhello"))
	})

	It("Should strip terminal escapes the model emitted so a style cannot outlive the message", func() {
		Expect(RenderMarkdownTo("safe \x1b[31mred-injection\x1b[0m tail", os.Stdout, true)).To(Equal("safe red-injection tail"))
	})
})

var _ = Describe("TruncateString", func() {
	It("Should return a short string unchanged", func() {
		Expect(TruncateString("hello", 10)).To(Equal("hello"))
	})

	It("Should keep a string of exactly max runes unchanged", func() {
		Expect(TruncateString("hello", 5)).To(Equal("hello"))
	})

	It("Should cut and append an ellipsis when longer than max", func() {
		Expect(TruncateString("hello world", 5)).To(Equal("hello..."))
	})

	It("Should count runes so multibyte text is never split mid-character", func() {
		out := TruncateString("héllo wörld", 5)
		Expect(out).To(Equal("héllo..."))
		Expect(utf8.ValidString(out)).To(BeTrue())
	})
})

var _ = Describe("TruncateLine", func() {
	It("Should collapse runs of whitespace to single spaces", func() {
		Expect(TruncateLine("a\n\tb   c", 20)).To(Equal("a b c"))
	})

	It("Should collapse first, then truncate on the collapsed length", func() {
		Expect(TruncateLine("one  two  three  four", 7)).To(Equal("one two..."))
	})

	It("Should trim leading and trailing whitespace", func() {
		Expect(TruncateLine("   spaced   ", 20)).To(Equal("spaced"))
	})
})
