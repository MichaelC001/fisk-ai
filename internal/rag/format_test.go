//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package rag

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
)

// pinFormatVersion rewrites the pinned format version of an existing index,
// standing in for an index written by another fisk-ai without needing that build.
func pinFormatVersion(dbPath string, version int) {
	db, err := openDB(dbPath, false)
	Expect(err).ToNot(HaveOccurred())
	defer db.Close()

	_, err = db.Exec(`INSERT INTO rag_meta(key, value) VALUES('format_version', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, strconv.Itoa(version))
	Expect(err).ToNot(HaveOccurred())
}

// writeForeignShapeStore builds an index whose chunks table carries a column set
// from a different format generation, with nothing pinned in rag_meta. It is the
// state the shipped reset left behind (it cleared the manifest but kept the table
// shape), which is why the format check alone cannot identify it.
func writeForeignShapeStore(dir string) string {
	Expect(os.MkdirAll(dir, dirFileMode)).To(Succeed())
	dbPath := filepath.Join(dir, dbFileName)
	Expect(ensureFileMode(dbPath)).To(Succeed())

	db, err := openDB(dbPath, false)
	Expect(err).ToNot(HaveOccurred())
	defer db.Close()

	for _, stmt := range []string{
		`CREATE TABLE documents (id INTEGER PRIMARY KEY, path TEXT NOT NULL UNIQUE, title TEXT, mtime INTEGER, hash TEXT)`,
		`CREATE TABLE chunks (id INTEGER PRIMARY KEY, document_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE, heading_path TEXT, ordinal INTEGER, text TEXT NOT NULL)`,
		`CREATE TABLE rag_meta (key TEXT PRIMARY KEY, value TEXT)`,
	} {
		_, err := db.Exec(stmt)
		Expect(err).ToNot(HaveOccurred())
	}

	return dbPath
}

var _ = Describe("Index format gate", func() {
	ctx := context.Background()

	var (
		tmp    string
		storeD string
		docsD  string
		dbPath string
		cfg    *config.Config
	)

	BeforeEach(func() {
		tmp = GinkgoT().TempDir()
		storeD = filepath.Join(tmp, "knowledge")
		docsD = filepath.Join(tmp, "docs")
		dbPath = filepath.Join(storeD, dbFileName)
		cfg = lexicalConfig(storeD)

		writeDoc(docsD, "backpressure.md", "# Design\n\n## Backpressure\n\nThe queue applies backpressure when the buffer is full so producers slow down.\n")
		writeDoc(docsD, "auth.md", "# Authentication\n\nTokens are validated against the issuer before any request proceeds.\n")
	})

	// buildIndex writes a current-format index and returns how many documents it
	// added.
	buildIndex := func() int {
		w, err := OpenWriter(cfg, "", Options{})
		Expect(err).ToNot(HaveOccurred())
		defer w.Close()

		stats, err := w.Index(ctx, []string{docsD}, IndexOptions{Reconcile: true})
		Expect(err).ToNot(HaveOccurred())

		return stats.Added
	}

	Describe("an index built by an older format", func() {
		BeforeEach(func() {
			buildIndex()
			pinFormatVersion(dbPath, formatVersion-1)
		})

		It("is refused by a reader, which names the discard and the rebuild", func() {
			_, err := Open(cfg, "", Options{})
			Expect(err).To(MatchError(ErrFormatTooOld))
			Expect(err.Error()).To(ContainSubstring(storeD))
			Expect(err.Error()).To(ContainSubstring("knowledge reset --force"))
			Expect(err.Error()).To(ContainSubstring("knowledge index"))
		})

		It("is refused by a writer before the schema can half-migrate", func() {
			_, err := OpenWriter(cfg, "", Options{})
			Expect(err).To(MatchError(ErrFormatTooOld))
			Expect(err.Error()).To(ContainSubstring("knowledge reset --force"))
		})

		// The refusal names a command, so that command has to work against the state
		// it is named for: a message naming a fix that also refuses is worse than one
		// naming none.
		It("is discarded by the command the refusal names, leaving a rebuildable store", func() {
			// The writer refusing must not leave the advisory lock held, or the fix it
			// names would fail with ErrLocked instead of running.
			removed, err := Destroy(cfg, "")
			Expect(err).ToNot(HaveOccurred())
			Expect(removed).To(Equal(dbPath))

			exists, err := StoreExists(cfg, "")
			Expect(err).ToNot(HaveOccurred())
			Expect(exists).To(BeFalse())

			Expect(buildIndex()).To(Equal(2))

			r, err := Open(cfg, "", Options{})
			Expect(err).ToNot(HaveOccurred())
			defer r.Close()

			res, err := r.Search(ctx, "backpressure buffer", 5)
			Expect(err).ToNot(HaveOccurred())
			Expect(res.Status).To(Equal(StatusOK))
			Expect(res.Hits).ToNot(BeEmpty())
		})
	})

	Describe("an index whose manifest was cleared but whose table shape is from another format", func() {
		BeforeEach(func() {
			writeForeignShapeStore(storeD)
		})

		It("is refused by a reader, naming both column sets", func() {
			_, err := Open(cfg, "", Options{})
			Expect(err).To(MatchError(ErrFormatTooOld))

			// The shape found on disk, then the shape this build needs. The fixture's
			// column is "text" so it stays foreign whatever this build's own shape is.
			Expect(err.Error()).To(ContainSubstring("ordinal, text"))
			Expect(err.Error()).To(ContainSubstring(strings.Join(chunksColumns, ", ")))
		})

		// The format check waves this state through: there is no pinned format to
		// compare, and an unpinned manifest is also what a freshly created schema has.
		It("is refused by a writer", func() {
			_, err := OpenWriter(cfg, "", Options{})
			Expect(err).To(MatchError(ErrFormatTooOld))
		})
	})

	// A schema can gain an object without any column changing, and such an index has
	// the right pinned format and the right columns. Reporting zero counts from a
	// table that does not exist, as though they had been measured, is the failure the
	// object check exists to prevent.
	Describe("an index missing a table this build's schema creates", func() {
		BeforeEach(func() {
			buildIndex()

			db, err := openDB(dbPath, false)
			Expect(err).ToNot(HaveOccurred())
			defer db.Close()

			// The state an index written before the unstemmed table existed is in.
			for _, stmt := range []string{`DROP TABLE chunks_vocab`, `DROP TABLE chunks_fts_exact`} {
				_, err := db.Exec(stmt)
				Expect(err).ToNot(HaveOccurred())
			}
		})

		It("is refused by a reader, naming what is missing", func() {
			_, err := Open(cfg, "", Options{})
			Expect(err).To(MatchError(ErrFormatTooOld))
			Expect(err.Error()).To(ContainSubstring("chunks_fts_exact"))
			Expect(err.Error()).To(ContainSubstring("chunks_vocab"))
		})

		// A writer could create the missing table, but the rows already in chunks would
		// not be in it, so it would answer with silent partial coverage.
		It("is refused by a writer rather than repaired underneath the existing rows", func() {
			_, err := OpenWriter(cfg, "", Options{})
			Expect(err).To(MatchError(ErrFormatTooOld))
		})
	})

	Describe("an index built by a newer format", func() {
		BeforeEach(func() {
			buildIndex()
			pinFormatVersion(dbPath, formatVersion+1)
		})

		It("is refused by a reader, naming the upgrade rather than a rebuild", func() {
			_, err := Open(cfg, "", Options{})
			Expect(err).To(MatchError(ErrFormatTooNew))
			Expect(err.Error()).To(ContainSubstring("upgrade fisk-ai"))
		})

		// The writer read the manifest nowhere at all before the gate, so an older
		// binary would happily index into a layout it does not understand.
		It("is refused by a writer", func() {
			_, err := OpenWriter(cfg, "", Options{})
			Expect(err).To(MatchError(ErrFormatTooNew))
		})
	})

	Describe("an index this build wrote", func() {
		It("opens on both paths and pins the current format", func() {
			Expect(buildIndex()).To(Equal(2))

			r, err := Open(cfg, "", Options{})
			Expect(err).ToNot(HaveOccurred())
			r.Close()

			w, err := OpenWriter(cfg, "", Options{})
			Expect(err).ToNot(HaveOccurred())
			defer w.Close()

			st, err := w.Stats(ctx)
			Expect(err).ToNot(HaveOccurred())
			Expect(st.Documents).To(Equal(2))
			Expect(st.Meta.FormatVersion).To(Equal(formatVersion))
		})

		// Reset clears rag_meta, leaving nothing pinned. That state is a current index,
		// not an old one, and must not be refused on the next open.
		It("is not refused after a reset has cleared the manifest", func() {
			buildIndex()

			w, err := OpenWriter(cfg, "", Options{})
			Expect(err).ToNot(HaveOccurred())
			Expect(w.Reset(ctx)).To(Succeed())
			w.Close()

			r, err := Open(cfg, "", Options{})
			Expect(err).ToNot(HaveOccurred())
			defer r.Close()

			res, err := r.Search(ctx, "backpressure", 5)
			Expect(err).ToNot(HaveOccurred())
			Expect(res.Status).To(Equal(StatusIndexEmpty))
		})
	})

	Describe("a store with no schema yet", func() {
		// ensureFileMode creates an empty file before openDB, so file existence is not
		// evidence of an index and the shape check has to tolerate a database with no
		// tables at all.
		It("is not mistaken for an index from another format", func() {
			w, err := OpenWriter(cfg, "", Options{})
			Expect(err).ToNot(HaveOccurred())
			defer w.Close()

			stats, err := w.Index(ctx, []string{docsD}, IndexOptions{Reconcile: true})
			Expect(err).ToNot(HaveOccurred())
			Expect(stats.Added).To(Equal(2))
		})
	})

	Describe("Destroy", func() {
		It("removes the index and any WAL sidecars left behind", func() {
			buildIndex()

			// A clean close checkpoints the sidecars away, but an interrupted writer
			// leaves them on disk, and one left behind would let a later open resurrect
			// pages from the index just discarded.
			for _, suffix := range []string{"-wal", "-shm"} {
				Expect(os.WriteFile(dbPath+suffix, nil, dbFileMode)).To(Succeed())
			}

			_, err := Destroy(cfg, "")
			Expect(err).ToNot(HaveOccurred())

			for _, suffix := range []string{"", "-wal", "-shm"} {
				Expect(dbPath + suffix).ToNot(BeAnExistingFile())
			}
		})

		It("is not an error against a store that is already absent", func() {
			Expect(os.MkdirAll(storeD, dirFileMode)).To(Succeed())

			removed, err := Destroy(cfg, "")
			Expect(err).ToNot(HaveOccurred())
			Expect(removed).To(Equal(dbPath))
		})

		It("refuses to delete through a symlink planted at the index path", func() {
			Expect(os.MkdirAll(storeD, dirFileMode)).To(Succeed())
			target := filepath.Join(tmp, "elsewhere.db")
			Expect(os.WriteFile(target, []byte("not the index"), dbFileMode)).To(Succeed())
			Expect(os.Symlink(target, dbPath)).To(Succeed())

			_, err := Destroy(cfg, "")
			Expect(err).To(MatchError(ContainSubstring("symlink")))
			Expect(target).To(BeAnExistingFile())
		})

		It("cannot run while a writer holds the advisory lock", func() {
			buildIndex()

			w, err := OpenWriter(cfg, "", Options{})
			Expect(err).ToNot(HaveOccurred())
			defer w.Close()

			_, err = Destroy(cfg, "")
			Expect(err).To(MatchError(ErrLocked))
			Expect(dbPath).To(BeAnExistingFile())
		})
	})
})
