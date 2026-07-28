//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"unicode/utf8"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/rag"
)

var _ = Describe("knowledge words", func() {
	BeforeEach(func() {
		knowledgeWordsPattern = ""
		knowledgeWordsLimit = wordsScreenLimit
		knowledgeWordsMinDocs = 0
		knowledgeWordsMaxDocs = 0
		knowledgeWordsOnly = false
		knowledgeWordsCount = false
	})

	Describe("displayWord", func() {
		It("leaves an ordinary word alone", func() {
			Expect(displayWord("deprecated")).To(Equal("deprecated"))
		})

		// A token is any run of alphanumerics, so one base64 blob in an indexed
		// document is a single enormous word that would otherwise break the layout.
		It("elides a long word to its head and tail", func() {
			long := strings.Repeat("a", 20) + strings.Repeat("z", 20)
			out := displayWord(long)

			Expect(utf8.RuneCountInString(out)).To(Equal(wordsMaxRunes))
			Expect(out).To(ContainSubstring("..."))
			Expect(out).To(HavePrefix("a"))
			Expect(out).To(HaveSuffix("z"))
		})

		It("keeps a word exactly at the bound whole", func() {
			exact := strings.Repeat("a", wordsMaxRunes)
			Expect(displayWord(exact)).To(Equal(exact))
		})

		// The tokenizer cannot currently emit an escape sequence, but the rule the rest
		// of the CLI follows is unconditional, so a later tokenizer change cannot
		// quietly reopen this.
		It("strips terminal control sequences", func() {
			Expect(displayWord("a\x1b[31mb")).ToNot(ContainSubstring("\x1b"))
		})
	})

	Describe("validateWordsFlags", func() {
		It("accepts the defaults", func() {
			Expect(validateWordsFlags()).To(Succeed())
		})

		It("refuses negative bounds", func() {
			knowledgeWordsMinDocs = -1
			Expect(validateWordsFlags()).To(MatchError(ContainSubstring("cannot be negative")))
		})

		// Both flags are legal on their own, so the contradiction is only visible when
		// they cross, and silently returning nothing would read as an absent word.
		It("refuses bounds that cross", func() {
			knowledgeWordsMinDocs = 5
			knowledgeWordsMaxDocs = 2
			Expect(validateWordsFlags()).To(MatchError(ContainSubstring("no word can satisfy both")))
		})

		It("refuses two flags that both own the output", func() {
			knowledgeWordsCount = true
			knowledgeWordsOnly = true
			Expect(validateWordsFlags()).To(MatchError(ContainSubstring("pick one")))
		})
	})

	Describe("wordsSuggestion", func() {
		// The word is not in the index, so only its opening letters are worth keeping:
		// suggesting the whole of it would send the reader to another empty result.
		It("keeps a short prefix of the absent word", func() {
			Expect(wordsSuggestion("kafka")).To(Equal("fisk-ai knowledge words kaf"))
		})

		It("leaves a word shorter than the prefix alone", func() {
			Expect(wordsSuggestion("db")).To(Equal("fisk-ai knowledge words db"))
		})

		It("folds case, because the vocabulary is folded", func() {
			Expect(wordsSuggestion("Kafka")).To(Equal("fisk-ai knowledge words kaf"))
		})
	})

	Describe("wordsStatusError", func() {
		// A pipe must never be able to read an unbuilt index as an empty vocabulary,
		// which is why the machine modes fail rather than print nothing.
		It("names the fix for an index that was never built", func() {
			Expect(wordsStatusError(rag.EnumIndexNotBuilt)).To(MatchError(ContainSubstring("knowledge index")))
		})

		It("distinguishes an empty index from a missing one", func() {
			Expect(wordsStatusError(rag.EnumCorpusEmpty)).To(MatchError(ContainSubstring("0 documents")))
		})
	})
})
