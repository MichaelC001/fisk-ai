//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/rag"
)

var _ = Describe("knowledgeAdvice", func() {
	It("Should pass a nil error through", func() {
		Expect(knowledgeAdvice(nil)).To(BeNil())
	})

	It("Should return an error carrying no knowledge sentinel unchanged", func() {
		err := errors.New("the disk is full")
		Expect(knowledgeAdvice(err)).To(BeIdenticalTo(err))
	})

	// The sentinel has to survive the wrapping: the CLI adds a command and the callers
	// above it still classify on what the library said.
	DescribeTable("Should add the command that repairs what a sentinel reports",
		func(sentinel error, advice string) {
			err := knowledgeAdvice(fmt.Errorf("%w: the index at /tmp/idx", sentinel))

			Expect(err).To(MatchError(sentinel))
			Expect(err.Error()).To(ContainSubstring("the index at /tmp/idx"))
			Expect(err.Error()).To(ContainSubstring(advice))
		},
		Entry("a stale embedding identity", rag.ErrMetaMismatch, "fisk knowledge index --reindex"),
		Entry("a changed dimension", rag.ErrDimensionMismatch, "fisk knowledge index --reindex"),
		Entry("a substituted model", rag.ErrModelMismatch, "knowledge.embeddings.model"),
		Entry("a later format generation", rag.ErrFormatTooNew, "upgrade fisk"),
		Entry("an earlier format generation", rag.ErrFormatTooOld, "fisk knowledge reset --force"),
	)

	// Every command this product ships is fisk, and the binary named in the advice is
	// the one the operator has to type.
	It("Should name no binary the product does not install", func() {
		for _, sentinel := range []error{rag.ErrMetaMismatch, rag.ErrDimensionMismatch, rag.ErrModelMismatch, rag.ErrFormatTooNew, rag.ErrFormatTooOld} {
			Expect(knowledgeAdvice(sentinel).Error()).ToNot(ContainSubstring("fisk-ai"))
		}
	})
})
