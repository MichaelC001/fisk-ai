//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("knowledge match flags", func() {
	// The match flags are package globals; reset the ones this suite touches so
	// cases stay independent.
	AfterEach(func() {
		knowledgeMatchAll = false
		knowledgeMatchCount = false
		knowledgeMatchPathsOnly = false
		knowledgeMatchExplain = false
		knowledgeMatchJSON = false
		knowledgeMatchMinMatches = 0
		knowledgeMatchQuery = ""
	})

	It("accepts the defaults", func() {
		Expect(validateMatchFlags(false)).To(Succeed())
	})

	// --limit holds a default, so the conflict is with a limit the user actually
	// typed. Treating the default as a conflict would make --all unusable.
	It("rejects --all with an explicit --limit but not with the default", func() {
		knowledgeMatchAll = true
		Expect(validateMatchFlags(false)).To(Succeed())
		Expect(validateMatchFlags(true)).To(MatchError(ContainSubstring("contradict each other")))
	})

	DescribeTable("rejects output flags that contradict each other",
		func(setup func()) {
			setup()
			Expect(validateMatchFlags(false)).To(HaveOccurred())
		},
		Entry("count with paths-only", func() { knowledgeMatchCount = true; knowledgeMatchPathsOnly = true }),
		Entry("count with explain", func() { knowledgeMatchCount = true; knowledgeMatchExplain = true }),
		Entry("paths-only with explain", func() { knowledgeMatchPathsOnly = true; knowledgeMatchExplain = true }),
		Entry("json with count", func() { knowledgeMatchJSON = true; knowledgeMatchCount = true }),
		Entry("json with paths-only", func() { knowledgeMatchJSON = true; knowledgeMatchPathsOnly = true }),
		Entry("json with explain", func() { knowledgeMatchJSON = true; knowledgeMatchExplain = true }),
	)

	It("accepts --json with the flags that shape the set", func() {
		knowledgeMatchJSON = true
		knowledgeMatchAll = true
		Expect(validateMatchFlags(false)).To(Succeed())
	})

	It("rejects a negative --min-matches", func() {
		knowledgeMatchMinMatches = -1
		Expect(validateMatchFlags(false)).To(MatchError(ContainSubstring("cannot be negative")))
	})

	// The pointer knowledge search prints when it ranks nothing. It names a command
	// that exists and quotes the query, which is corpus-adjacent text on its way to
	// a terminal.
	It("suggests match with the query quoted and sanitized", func() {
		out := matchSuggestion("backpressure\x1b[31m")

		Expect(out).To(ContainSubstring(`fisk knowledge match "backpressure"`))
		Expect(out).ToNot(ContainSubstring("\x1b"))
	})
})
