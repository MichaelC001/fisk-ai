//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package sanitize

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("MapText", func() {
	upper := func(s string) string { return strings.ToUpper(s) }

	It("applies the function to a string holding no escape sequences", func() {
		Expect(MapText("plain text", upper)).To(Equal("PLAIN TEXT"))
	})

	It("leaves the escape sequences untouched", func() {
		Expect(MapText("a\x1b[31mb\x1b[0mc", upper)).To(Equal("A\x1b[31mB\x1b[0mC"))
	})

	It("passes the empty run between two sequences", func() {
		var runs []string
		MapText("\x1b[31m\x1b[0mx", func(s string) string {
			runs = append(runs, s)

			return s
		})

		Expect(runs).To(Equal([]string{"", "", "x"}))
	})

	It("keeps a rewrite out of the sequence it sits beside", func() {
		// The case this exists for: a rewrite that inserts a bracket before a "]" would
		// otherwise treat "\x1b[31mgateway]" as one bracketed run and edit the sequence.
		bracket := func(s string) string { return strings.ReplaceAll(s, "]", "[]") }

		Expect(MapText("\x1b[31mgateway]", bracket)).To(Equal("\x1b[31mgateway[]"))
		Expect(MapText("gateway]", bracket)).To(Equal("gateway[]"))
	})
})
