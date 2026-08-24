//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package tui

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("RenderAnswer", func() {
	It("Should return the markdown raw when color is off", func() {
		Expect(RenderAnswer("# Title\n\nhello", true)).To(Equal("# Title\n\nhello"))
	})

	It("Should strip terminal escapes the model emitted", func() {
		Expect(RenderAnswer("safe \x1b[31mred\x1b[0m tail", true)).To(Equal("safe red tail"))
	})
})
