//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package rag

import (
	"context"
	"errors"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// dimFailEmbedder fails the dimension probe, standing in for an embeddings server
// that is unreachable when a reindex is asked for. Everything else behaves as the
// fake does, so a spec reaches the probe the same way a real run would.
type dimFailEmbedder struct {
	fakeEmbedder
}

func (d *dimFailEmbedder) Dim(context.Context) (int, error) {
	return 0, errors.New("connection refused")
}

var _ = Describe("Schema changes are atomic", func() {
	ctx := context.Background()

	var (
		tmp    string
		storeD string
		docsD  string
	)

	BeforeEach(func() {
		tmp = GinkgoT().TempDir()
		storeD = filepath.Join(tmp, "knowledge")
		docsD = filepath.Join(tmp, "docs")

		writeDoc(docsD, "backpressure.md", "# Backpressure\n\nThe queue applies backpressure when the buffer is full.\n")
		writeDoc(docsD, "auth.md", "# Authentication\n\nTokens are validated against the issuer.\n")
	})

	// intact asserts the store is one this build can open and query: every base schema
	// object present, and the corpus still there.
	intact := func(s *Store, docs int) {
		GinkgoHelper()

		missing, err := s.missingSchemaObjects(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(missing).To(BeEmpty())

		Expect(s.refuseUnusableIndex(ctx)).To(Succeed())

		n, err := scanCount(ctx, s.db, `SELECT count(*) FROM documents`)
		Expect(err).ToNot(HaveOccurred())
		Expect(n).To(Equal(docs))

		chunks, err := scanCount(ctx, s.db, `SELECT count(*) FROM chunks`)
		Expect(err).ToNot(HaveOccurred())
		Expect(chunks).To(BeNumerically(">", 0))
	}

	Describe("the schema rebuild", func() {
		// The drop and the recreate used to be twenty autocommitted statements, so a
		// Ctrl-C between any two of them left a schema that was neither the old one nor
		// the new one, and every later open refused it.
		It("leaves the index whole when its transaction rolls back", func() {
			w, err := OpenWriter(lexicalConfig(storeD), "", Options{})
			Expect(err).ToNot(HaveOccurred())
			defer w.Close()

			_, err = w.Index(ctx, []string{docsD}, IndexOptions{Reconcile: true})
			Expect(err).ToNot(HaveOccurred())
			intact(w, 2)

			tx, err := w.db.BeginTx(ctx, nil)
			Expect(err).ToNot(HaveOccurred())
			Expect(rebuildSchema(ctx, tx)).To(Succeed())
			Expect(tx.Rollback()).To(Succeed())

			intact(w, 2)
		})

		It("empties the index when its transaction commits", func() {
			w, err := OpenWriter(lexicalConfig(storeD), "", Options{})
			Expect(err).ToNot(HaveOccurred())
			defer w.Close()

			_, err = w.Index(ctx, []string{docsD}, IndexOptions{Reconcile: true})
			Expect(err).ToNot(HaveOccurred())

			Expect(w.Reset(ctx)).To(Succeed())

			missing, err := w.missingSchemaObjects(ctx)
			Expect(err).ToNot(HaveOccurred())
			Expect(missing).To(BeEmpty())

			n, err := scanCount(ctx, w.db, `SELECT count(*) FROM documents`)
			Expect(err).ToNot(HaveOccurred())
			Expect(n).To(Equal(0))
		})
	})

	Describe("a canceled reset", func() {
		It("leaves the index it was clearing", func() {
			w, err := OpenWriter(lexicalConfig(storeD), "", Options{})
			Expect(err).ToNot(HaveOccurred())
			defer w.Close()

			_, err = w.Index(ctx, []string{docsD}, IndexOptions{Reconcile: true})
			Expect(err).ToNot(HaveOccurred())

			canceled, cancel := context.WithCancel(ctx)
			cancel()

			Expect(w.Reset(canceled)).To(MatchError(context.Canceled))
			intact(w, 2)
		})
	})

	Describe("a reindex that cannot reach the embeddings server", func() {
		// The probe used to run after the reset, so a reindex against a server that was
		// down dropped the whole index and only then reported that it could not rebuild
		// it.
		It("refuses before anything is dropped", func() {
			cfg := vectorConfig(storeD, "m1")

			w := openWriterMock(cfg, &fakeEmbedder{model: "m1", dim: 8})
			_, err := w.Index(ctx, []string{docsD}, IndexOptions{Reconcile: true})
			Expect(err).ToNot(HaveOccurred())
			w.Close()

			w = openWriterMock(cfg, &dimFailEmbedder{fakeEmbedder{model: "m1", dim: 8}})
			defer w.Close()

			_, err = w.Index(ctx, []string{docsD}, IndexOptions{Reindex: true, Reconcile: true})
			Expect(err).To(MatchError(ContainSubstring("connection refused")))

			intact(w, 2)

			// The manifest is still the one the vectors on disk were built with, so the
			// index the probe failed to replace is still queryable.
			meta, err := w.readMeta(ctx)
			Expect(err).ToNot(HaveOccurred())
			Expect(meta.Model).To(Equal("m1"))
			Expect(meta.Dimension).To(Equal(8))
		})
	})

	Describe("the vector table and the manifest", func() {
		// They were created and pinned in separate transactions, so an interrupt between
		// them left a vector table with nothing pinned, which every later read reported
		// as a mismatch demanding a reindex.
		It("are committed together, or not at all", func() {
			cfg := vectorConfig(storeD, "m1")
			w := openWriterMock(cfg, &fakeEmbedder{model: "m1", dim: 8})
			defer w.Close()

			plan, err := w.planVectorTier(ctx, false)
			Expect(err).ToNot(HaveOccurred())
			Expect(plan.vector).To(BeTrue())

			tx, err := w.db.BeginTx(ctx, nil)
			Expect(err).ToNot(HaveOccurred())
			Expect(applyVectorPlan(ctx, tx, plan)).To(Succeed())
			Expect(tx.Rollback()).To(Succeed())

			cols, err := w.tableColumns(ctx, "chunks_vec")
			Expect(err).ToNot(HaveOccurred())
			Expect(cols).To(BeEmpty())

			meta, err := w.readMeta(ctx)
			Expect(err).ToNot(HaveOccurred())
			Expect(meta.Model).To(BeEmpty())
		})
	})

	Describe("planning the vector tier", func() {
		It("writes nothing, so a refusal leaves the manifest alone", func() {
			cfg := vectorConfig(storeD, "m1")

			w := openWriterMock(cfg, &fakeEmbedder{model: "m1", dim: 8})
			_, err := w.Index(ctx, []string{docsD}, IndexOptions{Reconcile: true})
			Expect(err).ToNot(HaveOccurred())
			w.Close()

			// A different model against the same index is the mismatch a reindex exists to
			// resolve, so it is refused here and the pinned identity is untouched.
			w = openWriterMock(vectorConfig(storeD, "m2"), &fakeEmbedder{model: "m2", dim: 8})
			defer w.Close()

			_, err = w.planVectorTier(ctx, false)
			Expect(err).To(MatchError(ErrMetaMismatch))

			meta, err := w.readMeta(ctx)
			Expect(err).ToNot(HaveOccurred())
			Expect(meta.Model).To(Equal("m1"))
		})

		It("skips the manifest checks on a reindex, which is the command that resolves them", func() {
			cfg := vectorConfig(storeD, "m1")

			w := openWriterMock(cfg, &fakeEmbedder{model: "m1", dim: 8})
			_, err := w.Index(ctx, []string{docsD}, IndexOptions{Reconcile: true})
			Expect(err).ToNot(HaveOccurred())
			w.Close()

			w = openWriterMock(vectorConfig(storeD, "m2"), &fakeEmbedder{model: "m2", dim: 16})
			defer w.Close()

			plan, err := w.planVectorTier(ctx, true)
			Expect(err).ToNot(HaveOccurred())
			Expect(plan.meta.Model).To(Equal("m2"))
			Expect(plan.dim).To(Equal(16))
		})
	})
})
