//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package rag

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
)

// ErrFTSDesynced reports that an FTS index no longer matches the chunk text it is
// built from. SQLite reports this as a malformed database image, which reads as a
// failing disk and is not what happened: the stored text is intact and only the
// derived index is stale, which RebuildFTS repairs without touching the corpus.
var ErrFTSDesynced = errors.New("the search index does not match the stored text")

// ftsTables are the two full-text indexes over chunk text, both derived entirely
// from the chunks table and both rebuildable from it.
var ftsTables = []string{ftsTablePorter, ftsTableExact}

// withWriteAccess runs fn against a writable handle on an index that already
// exists, holding the advisory write lock for the duration.
//
// It deliberately does not use OpenWriter. That function is the index constructor:
// it creates the directory, creates the database file, sets persistent journal and
// vacuum modes, and creates the whole schema. Calling it from a diagnostic would
// mean a machine with no index acquires one as a side effect of being asked whether
// its index is healthy, and the very check that reported "no index file" would
// report a built store on the next run.
func (s *Store) withWriteAccess(ctx context.Context, fn func(context.Context, *sql.DB) error) error {
	if s.db == nil {
		return ErrIndexNotBuilt
	}

	lock, err := acquireWriteLock(filepath.Join(s.dir, lockFileName))
	if err != nil {
		return err
	}
	defer lock.release()

	db, err := openDB(s.dbPath, false)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	return fn(ctx, db)
}

// CheckFTSIntegrity verifies that each full-text index still matches the chunk text
// it is derived from, and names the table that failed.
//
// The rank form of the command is the one that checks this. The bare form, and the
// rank-0 form, both pass on an index that has drifted from its content table, and a
// drifted index answers MATCH with fewer rows than the corpus holds. That makes
// every search quietly incomplete, which is the one failure knowledge match exists
// to rule out, so it is worth the write lock this needs.
//
// It is a write: FTS5 commands are issued as inserts, so SQLite opens a write
// transaction even though no row changes.
func (s *Store) CheckFTSIntegrity(ctx context.Context) error {
	return s.withWriteAccess(ctx, func(ctx context.Context, db *sql.DB) error {
		for _, table := range ftsTables {
			q := fmt.Sprintf(`INSERT INTO %s(%[1]s, rank) VALUES('integrity-check', 1)`, table)
			if _, err := db.ExecContext(ctx, q); err != nil {
				return fmt.Errorf("%w: %s is stale; run: fisk knowledge rebuild", ErrFTSDesynced, table)
			}
		}

		return nil
	})
}

// RebuildFTS discards each full-text index and derives it again from the chunk
// text, which is what repairs the state CheckFTSIntegrity reports.
//
// It reads the chunks table and writes only the two FTS indexes. The documents, the
// chunk text and the vectors are untouched, so nothing is re-embedded and no
// embeddings spend is incurred, which is the whole difference between this and a
// reset followed by a fresh index.
//
// It repairs a derived index that drifted from intact text. It cannot repair
// damaged text: given a corrupt chunks table it will faithfully build a consistent
// index over the corruption, after which the integrity check passes and says
// nothing. That is why it is a verb an operator runs deliberately rather than
// something the doctor offers to do for them.
func (s *Store) RebuildFTS(ctx context.Context) error {
	return s.withWriteAccess(ctx, func(ctx context.Context, db *sql.DB) error {
		for _, table := range ftsTables {
			q := fmt.Sprintf(`INSERT INTO %s(%[1]s) VALUES('rebuild')`, table)
			if _, err := db.ExecContext(ctx, q); err != nil {
				return fmt.Errorf("rebuilding %s: %w", table, err)
			}
		}

		return nil
	})
}
