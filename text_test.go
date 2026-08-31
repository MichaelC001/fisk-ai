//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
	"unicode/utf8"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

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
