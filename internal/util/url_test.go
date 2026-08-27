//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package util

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("ValidateBaseURL", func() {
	It("accepts https to any host", func() {
		Expect(ValidateBaseURL("base", "https://example.com/v1")).To(Succeed())
	})

	It("accepts http to a loopback host", func() {
		Expect(ValidateBaseURL("base", "http://127.0.0.1:1234/v1")).To(Succeed())
		Expect(ValidateBaseURL("base", "http://localhost:1234/v1")).To(Succeed())
		Expect(ValidateBaseURL("base", "http://[::1]:1234/v1")).To(Succeed())
	})

	It("accepts http to a non-loopback host", func() {
		Expect(ValidateBaseURL("base", "http://10.0.0.5:8080")).To(Succeed())
		Expect(ValidateBaseURL("base", "http://host.docker.internal:1234/v1")).To(Succeed())
	})

	It("rejects a non-http scheme", func() {
		Expect(ValidateBaseURL("base", "ftp://example.com")).To(MatchError(ContainSubstring("scheme must be http or https")))
	})

	It("rejects embedded userinfo credentials", func() {
		Expect(ValidateBaseURL("base", "https://user:pass@example.com")).To(MatchError(ContainSubstring("userinfo")))
	})

	It("rejects an unparseable URL", func() {
		Expect(ValidateBaseURL("base", "http://[::1")).ToNot(Succeed())
	})

	It("rejects http with an empty host", func() {
		Expect(ValidateBaseURL("base", "http://")).To(MatchError(ContainSubstring("must name a host")))
	})
})
