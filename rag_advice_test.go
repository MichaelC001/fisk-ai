//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
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
		Entry("a later format generation", rag.ErrFormatTooNew, "fisk knowledge reset --force"),
		Entry("an earlier format generation", rag.ErrFormatTooOld, "fisk knowledge reset --force"),
	)

	// A reindex from a config with no embeddings block rebuilds the index lexical-only
	// and discards the vectors, so the advice that names it alone destroys what the
	// operator wanted back.
	It("Should offer both routes out of an index whose embeddings block is gone", func() {
		err := fmt.Errorf("%w: %w (%q)", rag.ErrMetaMismatch, rag.ErrEmbeddingsAbsent, "m1")
		advice := knowledgeAdvice(err).Error()

		Expect(advice).To(ContainSubstring("knowledge.embeddings"))
		Expect(advice).To(ContainSubstring("fisk knowledge index --reindex"))
		Expect(advice).To(ContainSubstring("discard its vectors"))
		Expect(advice).To(ContainSubstring(`("m1")`))
	})

	// The two travel wrapped together, so the case that matches only ErrMetaMismatch
	// has to keep answering the mismatches that carry no embeddings sentinel.
	It("Should still send a bare embedding mismatch to a reindex", func() {
		advice := knowledgeAdvice(fmt.Errorf("%w: dim 32 vs 64", rag.ErrMetaMismatch)).Error()

		Expect(advice).To(ContainSubstring("run: fisk knowledge index --reindex"))
		Expect(advice).ToNot(ContainSubstring("knowledge.embeddings"))
	})

	// Reset discards the file rather than clearing rows for either of these, and it
	// covering only the earlier one is what left an operator with no working command
	// after the format pin was lowered.
	It("Should treat both format refusals as reset's to answer", func() {
		Expect(formatRefusal(rag.ErrFormatTooOld)).To(BeTrue())
		Expect(formatRefusal(rag.ErrFormatTooNew)).To(BeTrue())
		Expect(formatRefusal(fmt.Errorf("wrapped: %w", rag.ErrFormatTooNew))).To(BeTrue())

		Expect(formatRefusal(nil)).To(BeFalse())
		Expect(formatRefusal(rag.ErrLocked)).To(BeFalse())
		Expect(formatRefusal(rag.ErrMetaMismatch)).To(BeFalse())
	})

	// A pin that was lowered leaves an index this build reads as later than its own,
	// and there is no newer build to reach for, so the advice that names only one of the
	// two routes names the one that does not apply.
	It("Should offer both routes out of a later format generation", func() {
		advice := knowledgeAdvice(rag.ErrFormatTooNew).Error()

		Expect(advice).To(ContainSubstring("run a build that reads that format"))
		Expect(advice).To(ContainSubstring("fisk knowledge reset --force"))
	})

	// Every command this product ships is fisk, and the binary named in the advice is
	// the one the operator has to type.
	It("Should name no binary the product does not install", func() {
		for _, sentinel := range []error{rag.ErrMetaMismatch, rag.ErrEmbeddingsAbsent, rag.ErrDimensionMismatch, rag.ErrModelMismatch, rag.ErrFormatTooNew, rag.ErrFormatTooOld} {
			Expect(knowledgeAdvice(sentinel).Error()).ToNot(ContainSubstring("fisk-ai"))
		}
	})
})

var _ = Describe("knowledgeStoreDetail", func() {
	cfg := func(dir string) *config.Config {
		return &config.Config{
			Identity: "agent",
			Harness:  config.HarnessConfig{RAG: &config.RAGConfig{Enabled: true, Directory: dir}},
		}
	}

	// A relative directory resolves against wherever the process is standing, which is
	// the case where an index exists and this command searched somewhere else, so the
	// report has to carry the path it searched rather than the one the config holds.
	It("Should resolve a relative configured directory and name it", func() {
		detail := knowledgeStoreDetail(cfg("kb"), "")
		Expect(detail).To(HaveLen(2))

		abs, err := filepath.Abs(filepath.Join("kb", "knowledge.db"))
		Expect(err).ToNot(HaveOccurred())
		Expect(detail[0]).To(Equal("looked for: " + abs))
		Expect(detail[1]).To(Equal(`knowledge.directory "kb" is relative, so it resolved against the current directory`))
	})

	// With nothing configured there is no value to quote, so the line says where the
	// default puts the index instead.
	It("Should name the default layout when no directory is configured", func() {
		detail := knowledgeStoreDetail(cfg(""), "")
		Expect(detail).To(HaveLen(2))

		abs, err := filepath.Abs(filepath.Join("knowledge", "agent", "knowledge.db"))
		Expect(err).ToNot(HaveOccurred())
		Expect(detail[0]).To(Equal("looked for: " + abs))
		Expect(detail[1]).To(Equal("knowledge.directory is not set, so the index defaults to knowledge/agent under the current directory"))
	})

	It("Should say nothing about the working directory for an absolute configured directory", func() {
		detail := knowledgeStoreDetail(cfg("/srv/kb"), "")

		Expect(detail).To(Equal([]string{"looked for: " + filepath.Join("/srv/kb", "knowledge.db")}))
	})

	// Reset prints the store detail alone: there is nothing to reset, and building an
	// index is not what the operator asked for.
	It("Should carry the build command only in the not-built detail", func() {
		store := knowledgeStoreDetail(cfg("/srv/kb"), "")
		notBuilt := knowledgeNotBuiltDetail(cfg("/srv/kb"), "")

		Expect(notBuilt[len(notBuilt)-1]).To(Equal("run: fisk knowledge index"))
		Expect(notBuilt).To(HaveLen(len(store) + 1))
		for _, line := range store {
			Expect(line).ToNot(ContainSubstring("fisk knowledge index"))
		}
	})

	It("Should render the headline and every detail line as an error", func() {
		err := knowledgeReportError(knowledgeNotBuiltHeadline, knowledgeNotBuiltDetail(cfg("/srv/kb"), ""))

		Expect(err.Error()).To(Equal(knowledgeNotBuiltHeadline +
			"\n  looked for: " + filepath.Join("/srv/kb", "knowledge.db") +
			"\n  run: fisk knowledge index"))
	})
})
