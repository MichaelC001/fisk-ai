//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package rag

import (
	"context"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
)

var _ = Describe("FTS integrity", func() {
	ctx := context.Background()

	var (
		docsD string
		cfg   *config.Config
		dir   string
	)

	BeforeEach(func() {
		tmp := GinkgoT().TempDir()
		docsD = filepath.Join(tmp, "docs")
		dir = filepath.Join(tmp, "knowledge")
		cfg = lexicalConfig(dir)

		writeDoc(docsD, "a.md", "# Retention\n\nRecords are held for ninety days.\n")
	})

	build := func() {
		GinkgoHelper()

		w, err := OpenWriter(cfg, "")
		Expect(err).ToNot(HaveOccurred())
		_, err = w.Index(ctx, []string{docsD}, IndexOptions{Reconcile: true})
		Expect(err).ToNot(HaveOccurred())
		Expect(w.Close()).To(Succeed())
	}

	reader := func() *Store {
		GinkgoHelper()

		s, err := Open(cfg, "")
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(s.Close)

		return s
	}

	// desync writes a chunk with the insert trigger off and puts the trigger back, so
	// the schema stays whole and only the derived index is stale. That is the state
	// the check exists for, and it is invisible to every other check.
	desync := func() {
		GinkgoHelper()

		w, err := OpenWriter(cfg, "")
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(func() {})

		for _, stmt := range []string{
			`DROP TRIGGER IF EXISTS chunks_ai`,
			`INSERT INTO chunks(document_id, heading_path, ordinal, body)
			 SELECT document_id, 'Ghost', 9999, 'zzqqxx ghosted text' FROM chunks LIMIT 1`,
			`CREATE TRIGGER IF NOT EXISTS chunks_ai AFTER INSERT ON chunks BEGIN
				INSERT INTO chunks_fts(rowid, body, heading_path)
				VALUES (new.id, new.body, new.heading_path);
				INSERT INTO chunks_fts_exact(rowid, body, heading_path)
				VALUES (new.id, new.body, new.heading_path);
			END`,
		} {
			_, execErr := w.db.ExecContext(ctx, stmt)
			Expect(execErr).ToNot(HaveOccurred())
		}

		Expect(w.Close()).To(Succeed())
	}

	It("passes on a healthy index", func() {
		build()
		Expect(reader().CheckFTSIntegrity(ctx)).To(Succeed())
	})

	// The reason the check is worth a write lock: a drifted index answers MATCH with
	// fewer documents than the corpus holds, so every search silently under-reports
	// while the command that promises a complete set keeps promising it.
	It("catches a drift that MATCH cannot see", func() {
		build()
		desync()

		set, err := reader().documentsMatching(ctx, ftsTableExact, `"zzqqxx"`)
		Expect(err).ToNot(HaveOccurred())
		Expect(set).To(BeEmpty())

		err = reader().CheckFTSIntegrity(ctx)
		Expect(err).To(MatchError(ErrFTSDesynced))
		Expect(err).To(MatchError(ContainSubstring("knowledge rebuild")))
	})

	It("repairs the drift without touching the stored text", func() {
		build()

		before, err := reader().Stats(ctx)
		Expect(err).ToNot(HaveOccurred())

		desync()
		Expect(reader().RebuildFTS(ctx)).To(Succeed())
		Expect(reader().CheckFTSIntegrity(ctx)).To(Succeed())

		// The text that was invisible is now reachable, and no document was lost.
		set, err := reader().documentsMatching(ctx, ftsTableExact, `"zzqqxx"`)
		Expect(err).ToNot(HaveOccurred())
		Expect(set).To(HaveLen(1))

		after, err := reader().Stats(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(after.Documents).To(Equal(before.Documents))
	})

	// The writer entry point creates an index as a side effect of opening one, so a
	// diagnostic built on it would manufacture the thing it was asked to inspect.
	It("refuses rather than creating an index that does not exist", func() {
		s := reader()

		Expect(s.CheckFTSIntegrity(ctx)).To(MatchError(ErrIndexNotBuilt))
		Expect(s.RebuildFTS(ctx)).To(MatchError(ErrIndexNotBuilt))

		_, err := os.Stat(filepath.Join(dir, dbFileName))
		Expect(os.IsNotExist(err)).To(BeTrue(), "the diagnostic created an index file")
	})

	It("reports a concurrent writer rather than blocking", func() {
		build()

		w, err := OpenWriter(cfg, "")
		Expect(err).ToNot(HaveOccurred())
		defer w.Close()

		Expect(reader().CheckFTSIntegrity(ctx)).To(MatchError(ErrLocked))
	})

	Describe("the doctor check", func() {
		report := func() *DoctorReport {
			GinkgoHelper()

			r, err := reader().Doctor(ctx, nil)
			Expect(err).ToNot(HaveOccurred())

			return r
		}

		check := func(r *DoctorReport) DoctorCheck {
			GinkgoHelper()

			for _, c := range r.Checks {
				if c.Name == "Search index integrity" {
					return c
				}
			}

			Fail("the integrity check is absent from the report")
			return DoctorCheck{}
		}

		It("passes on a healthy index", func() {
			build()

			r := report()
			Expect(check(r).State).To(Equal(DoctorPass))
			Expect(r.HasFatal()).To(BeFalse())
			Expect(r.HasUnrun()).To(BeFalse())
		})

		It("fails the run on a drifted index", func() {
			build()
			desync()

			r := report()
			Expect(check(r).State).To(Equal(DoctorFail))
			Expect(check(r).Fatal).To(BeTrue())
			Expect(r.HasFatal()).To(BeTrue())
			Expect(check(r).Detail).To(ContainSubstring("knowledge rebuild"))
		})

		// Not knowing is not the same as knowing something is wrong, which is why the
		// state is three-valued and why HasFatal ignores a check that did not run.
		It("skips rather than fails when there is no index", func() {
			r := report()
			Expect(check(r).State).To(Equal(DoctorNotRun))
			Expect(r.HasFatal()).To(BeFalse())
			Expect(r.HasUnrun()).To(BeTrue())
		})

		It("skips rather than fails when another writer holds the lock", func() {
			build()

			w, err := OpenWriter(cfg, "")
			Expect(err).ToNot(HaveOccurred())
			defer w.Close()

			r := report()
			Expect(check(r).State).To(Equal(DoctorNotRun))
			Expect(check(r).Detail).To(ContainSubstring("holds the index lock"))
			Expect(r.HasFatal()).To(BeFalse())
		})
	})
})
