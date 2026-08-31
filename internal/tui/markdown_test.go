//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package tui

import (
	"os"
	"strings"
	"unicode/utf8"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("RenderMarkdownTo", func() {
	// The destination is a pipe of the spec's own rather than os.Stdout, which is a
	// terminal or not depending on how the suite was invoked: ginkgo intercepts stdout
	// unless it is run with -v. This is the path a piped or redirected run takes.
	It("Should return the markdown unchanged when the destination is not a terminal", func() {
		r, w, err := os.Pipe()
		Expect(err).NotTo(HaveOccurred())
		defer r.Close()
		defer w.Close()

		Expect(RenderMarkdownTo("# Title\n\nhello", w, false)).To(Equal("# Title\n\nhello"))
	})

	It("Should return the markdown unchanged when color is off", func() {
		Expect(RenderMarkdownTo("# Title\n\nhello", os.Stdout, true)).To(Equal("# Title\n\nhello"))
	})

	It("Should strip terminal escapes the model emitted so a style cannot outlive the message", func() {
		Expect(RenderMarkdownTo("safe \x1b[31mred-injection\x1b[0m tail", os.Stdout, true)).To(Equal("safe red-injection tail"))
	})
})

var _ = Describe("RenderMarkdownWidth", func() {
	It("Should render markdown to styled ANSI when color is enabled", func() {
		out := RenderMarkdownWidth("# Heading\n\nsome **bold** prose", 80, false)
		Expect(out).To(ContainSubstring("Heading"))
		Expect(out).To(ContainSubstring("bold"))
		Expect(out).To(ContainSubstring("\x1b["), "expected ANSI styling escapes in the colored output")
	})

	It("Should emit no ANSI escapes when noColor is set", func() {
		out := RenderMarkdownWidth("# Heading\n\nsome **bold** prose", 80, true)
		Expect(out).To(ContainSubstring("Heading"))
		Expect(out).NotTo(ContainSubstring("\x1b"), "the notty style must not emit escapes")
	})

	It("Should wrap prose near the requested width", func() {
		para := strings.Repeat("word ", 80)
		out := RenderMarkdownWidth(para, 30, true)

		for _, line := range strings.Split(out, "\n") {
			Expect(utf8.RuneCountInString(line)).To(BeNumerically("<=", 30))
		}
	})

	It("Should not panic on an absurdly small width", func() {
		Expect(RenderMarkdownWidth("hello world", 1, true)).To(ContainSubstring("hello"))
	})
})
