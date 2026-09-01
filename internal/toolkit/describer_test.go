//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package toolkit

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("CallInfo", func() {
	// A tool that does not implement Describer yields this value, so its Kind has to be
	// the sentinel rather than whichever provider a reordered const block put first.
	It("Should account the zero value under the unknown provider", func() {
		var zero CallInfo
		Expect(zero.Kind).To(Equal(KindUnknown))
	})
})
