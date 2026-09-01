//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package rag

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	// maxIndexFileBytes skips a source file larger than this, bounding memory and
	// keeping a single chunk within the embedding input cap.
	maxIndexFileBytes = 512 * 1024

	// memoryDirName is the sibling store excluded from every walk, alongside this
	// feature's own store directory.
	memoryDirName = "memory"
)

// DefaultExtensions returns the file extensions the walk indexes when a caller
// names none: markdown and plain text only. Every call builds a new map, so a
// caller that adds to the one it received changes only its own copy.
func DefaultExtensions() map[string]bool {
	return map[string]bool{
		".md":       true,
		".markdown": true,
		".txt":      true,
		".text":     true,
	}
}

// IndexOptions controls one index run.
type IndexOptions struct {
	// Reindex forces a full rebuild: existing data and the vector table are dropped
	// (allowing a dimension change) and everything is re-embedded from scratch.
	Reindex bool
	// DryRun lists what would be done and estimates chunk and embedding-call counts
	// without writing or embedding anything.
	DryRun bool
	// Reconcile enables orphan deletion after the walk. It is set only for a
	// full-corpus walk (no explicit path given); a subpath walk never deletes.
	Reconcile bool
	// Extensions is the allowed extension set, DefaultExtensions() when nil. Index
	// copies it once before the first root, so every root walks the set the call was
	// made with however the caller edits its own map meanwhile.
	Extensions map[string]bool
	// Progress, when set, receives human-readable progress notes (skipped files,
	// counts) for the CLI to print. It is never called with model-facing data.
	Progress func(string)
	// OnFile, when set, is called once per file the walk processed, after the file
	// is committed, so a caller can drive a progress display. It is called
	// synchronously on the walk goroutine and must not block. A dry run never calls
	// it: nothing is committed, so there is no progress to report.
	OnFile func(IndexEvent)
}

// IndexAction is what an index run did with one file.
type IndexAction string

const (
	// IndexAdded reports a file the index did not hold before.
	IndexAdded IndexAction = "added"
	// IndexUpdated reports a file whose content hash changed.
	IndexUpdated IndexAction = "updated"
	// IndexUnchanged reports a file whose content hash matched, which is re-used as
	// it stands. Files the walk rejects outright (oversized, not UTF-8) are reported
	// through Progress and produce no event.
	IndexUnchanged IndexAction = "unchanged"
)

// IndexEvent reports one processed file to IndexOptions.OnFile.
type IndexEvent struct {
	// Path is the file, as the index keys it.
	Path string
	// Action is what the run did with it.
	Action IndexAction
	// Chunks is how many chunks the file holds, whether or not this run produced
	// them.
	Chunks int
	// Embeddings is how many chunks this run sent to the embedder, which is zero for
	// an unchanged file and for a run with no vector tier. A caller measuring
	// embedding progress counts this, never Chunks: an unchanged file carries the
	// chunk count it was indexed with, and charging that to a total covering only
	// the files being embedded completes the display before the work does.
	Embeddings int
}

// IndexStats summarizes an index run.
type IndexStats struct {
	Files      int
	Added      int
	Updated    int
	Skipped    int
	Removed    int
	Chunks     int
	Embeddings int
	// FirstBuild reports that the index held no documents before this run, the one
	// time the operator has no cost intuition for what is about to be embedded.
	FirstBuild bool
}

// exts is the extension set this run walks with, copied so the walk reads a map
// nobody else holds.
func (o IndexOptions) exts() map[string]bool {
	if o.Extensions == nil {
		return DefaultExtensions()
	}

	out := make(map[string]bool, len(o.Extensions))
	for ext, allowed := range o.Extensions {
		out[ext] = allowed
	}

	return out
}

func (o IndexOptions) note(msg string) {
	if o.Progress != nil {
		o.Progress(msg)
	}
}

func (o IndexOptions) event(path string, action IndexAction, chunks, embeddings int) {
	if o.OnFile == nil || o.DryRun {
		return
	}

	o.OnFile(IndexEvent{Path: path, Action: action, Chunks: chunks, Embeddings: embeddings})
}

// Index walks roots and brings the index into line with them: it adds new files,
// re-ingests changed ones (by content hash), skips unchanged ones, and, on a
// reconciling full-corpus walk, deletes documents no longer present. The vector tier
// is prepared upfront and, on a reindex, the schema is rebuilt in the same
// transaction: see prepareIndex. Embedding happens outside the write transaction so
// the slow call never holds the single writer slot. It requires a writer store.
//
// A cancel during the preparation leaves the index untouched. A cancel during the
// walk keeps the files already committed, which the next run skips by content hash,
// except after a reindex, where the old index is gone and re-running without
// --reindex is what finishes the rebuild.
func (s *Store) Index(ctx context.Context, roots []string, opts IndexOptions) (*IndexStats, error) {
	if s.readOnly || s.db == nil {
		return nil, fmt.Errorf("index requires a writable knowledge store")
	}
	if len(roots) == 0 {
		return nil, fmt.Errorf("index requires at least one path")
	}

	stats := &IndexStats{}

	priorDocs, err := scanCount(ctx, s.db, `SELECT count(*) FROM documents`)
	if err != nil {
		return nil, err
	}
	stats.FirstBuild = priorDocs == 0

	if !opts.DryRun {
		if err := s.prepareIndex(ctx, opts.Reindex); err != nil {
			return nil, err
		}
		if opts.Reindex {
			stats.FirstBuild = true
		}
	}

	// One copy for the whole run rather than one per root, so every root walks the set
	// the call was made with even when the caller writes to its map while root one is
	// still going.
	exts := opts.exts()

	seen := map[string]bool{}
	for _, root := range roots {
		if err := s.walkRoot(ctx, root, exts, opts, stats, seen); err != nil {
			return nil, err
		}
	}

	// Orphan reconcile: only on a reconciling full-corpus walk, and never when the
	// walk saw zero files (a walk that errored early must not wipe the index).
	if opts.Reconcile && !opts.DryRun && len(seen) > 0 {
		if err := s.reconcileOrphans(ctx, seen, stats); err != nil {
			return nil, err
		}
	}

	return stats, nil
}

// walkRoot walks one root, dispatching each eligible file to add / update / skip. exts
// is the extension set Index copied for the run, shared by every root it walks.
func (s *Store) walkRoot(ctx context.Context, root string, exts map[string]bool, opts IndexOptions, stats *IndexStats, seen map[string]bool) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Observe cancellation between files so an interrupt is prompt even across
		// the CPU-bound chunking that sits between the ctx-aware embed and DB calls.
		if err := ctx.Err(); err != nil {
			return err
		}

		if d.IsDir() {
			if shouldSkipDir(path, s.dir) {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip symlinks: canonicalize to one real key per file and never follow a
		// link out of the corpus.
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if !exts[strings.ToLower(filepath.Ext(path))] {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > maxIndexFileBytes {
			opts.note(fmt.Sprintf("skipping oversized file %q (%d bytes)", path, info.Size()))
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !utf8.Valid(data) {
			opts.note(fmt.Sprintf("skipping non-UTF-8 file %q", path))
			return nil
		}

		key := filepath.ToSlash(path)
		seen[key] = true
		stats.Files++

		return s.ingestOne(ctx, key, info.ModTime().Unix(), data, opts, stats)
	})
}

// shouldSkipDir decides whether a directory is excluded from both the index walk
// and the watcher, so the two agree on the corpus: the store's own directory,
// dotdirs like .git, and a sibling memory/ store are skipped. The walk root itself
// (name "." or "..") is never skipped.
func shouldSkipDir(path, storeDir string) bool {
	if filepath.Clean(path) == filepath.Clean(storeDir) {
		return true
	}
	name := filepath.Base(path)
	// Skip dotfiles like .git, but never skip the walk root itself when it is ".".
	if name != "." && name != ".." && strings.HasPrefix(name, ".") {
		return true
	}
	if name == memoryDirName {
		return true
	}

	return false
}

// ingestOne classifies a seen file by content hash and add/update/skips it,
// updating stats. In dry-run it only chunks to estimate work and writes nothing.
func (s *Store) ingestOne(ctx context.Context, key string, mtime int64, data []byte, opts IndexOptions, stats *IndexStats) error {
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])

	var have string
	err := s.db.QueryRowContext(ctx, `SELECT hash FROM documents WHERE path = ?`, key).Scan(&have)
	switch {
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return err
	case opts.Reindex:
		// A reindex drops and re-embeds every file, so a matching stale hash must not
		// short-circuit it as unchanged. The dry-run cost estimate skips the reset, so
		// without this it would see every file as unchanged and report zero work.
	case errors.Is(err, sql.ErrNoRows):
		// add
	case have == hash:
		stats.Skipped++
		n, err := scanCount(ctx, s.db, `SELECT count(*) FROM chunks c JOIN documents d ON d.id=c.document_id WHERE d.path=?`, key)
		if err != nil {
			return err
		}
		stats.Chunks += n
		// Nothing was embedded: these chunks are the ones a previous run produced.
		opts.event(key, IndexUnchanged, n, 0)
		return nil
	default:
		// update
	}

	// A reindex re-embeds every file from scratch, so it always counts as an add.
	isNew := opts.Reindex || errors.Is(err, sql.ErrNoRows)

	chunks := ChunkDocument(string(data))

	if opts.DryRun {
		if isNew {
			stats.Added++
		} else {
			stats.Updated++
		}
		stats.Chunks += len(chunks)
		if s.emb != nil {
			stats.Embeddings += len(chunks)
		}
		return nil
	}

	if err := s.ingestFile(ctx, key, mtime, hash, string(data), chunks); err != nil {
		return fmt.Errorf("indexing %q: %w", key, err)
	}
	action := IndexUpdated
	if isNew {
		stats.Added++
		action = IndexAdded
	} else {
		stats.Updated++
	}
	stats.Chunks += len(chunks)
	embedded := 0
	if s.emb != nil {
		embedded = len(chunks)
		stats.Embeddings += embedded
	}
	// After the commit, so the event never reports work still in the embedder.
	opts.event(key, action, len(chunks), embedded)

	return nil
}

// ingestFile embeds the file's chunks OUTSIDE the write transaction (the slow call
// must not hold the single writer slot), then does the cheap upsert + purge +
// insert in a short transaction. Purge-then-insert covers both first ingest and
// update; the triggers keep chunks_fts and chunks_vec correct, including clearing
// ghost chunks when a file shrinks.
func (s *Store) ingestFile(ctx context.Context, key string, mtime int64, hash, contents string, chunks []Chunk) error {
	var vecs [][]float32
	if s.emb != nil && len(chunks) > 0 {
		docs := make([]Document, len(chunks))
		for i, c := range chunks {
			// The one place the breadcrumb is folded into the body. The lexical index
			// keeps the two apart, but the vector is built from both, so the section
			// title pulls the on-topic chunk to rank 1 rather than being invisible to
			// the model. This exact string is what the index was built from.
			docs[i] = Document{Title: c.HeadingPath, Text: foldHeading(c.HeadingPath, c.Body)}
		}
		raw, err := s.emb.EmbedDocuments(ctx, docs)
		if err != nil {
			return fmt.Errorf("embedding: %w", err)
		}
		if len(raw) != len(chunks) {
			return fmt.Errorf("embedder returned %d vectors for %d chunks", len(raw), len(chunks))
		}
		vecs = make([][]float32, len(raw))
		for i, v := range raw {
			vecs[i] = normalize(v)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var docID int64
	title := DocumentTitle(contents)
	err = tx.QueryRowContext(ctx,
		`INSERT INTO documents(path, title, mtime, hash) VALUES(?,?,?,?)
		 ON CONFLICT(path) DO UPDATE SET title=excluded.title, mtime=excluded.mtime, hash=excluded.hash
		 RETURNING id`, key, title, mtime, hash).Scan(&docID)
	if err != nil {
		return fmt.Errorf("upsert document: %w", err)
	}

	// Purge the document's existing chunks first; the triggers clear the matching
	// FTS and vec rows, so a shrinking file leaves no ghost chunks behind.
	if _, err := tx.ExecContext(ctx, `DELETE FROM chunks WHERE document_id = ?`, docID); err != nil {
		return fmt.Errorf("purge chunks: %w", err)
	}

	for i, c := range chunks {
		var chunkID int64
		err = tx.QueryRowContext(ctx,
			`INSERT INTO chunks(document_id, heading_path, ordinal, body) VALUES(?,?,?,?) RETURNING id`,
			docID, c.HeadingPath, i, c.Body).Scan(&chunkID)
		if err != nil {
			return fmt.Errorf("insert chunk: %w", err)
		}
		if s.emb != nil {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO chunks_vec(chunk_id, embedding) VALUES(?, ?)`, chunkID, vecJSON(vecs[i])); err != nil {
				return fmt.Errorf("insert vector: %w", err)
			}
		}
	}

	return tx.Commit()
}

// reconcileOrphans deletes documents whose path was not seen during a reconciling
// full-corpus walk, so files removed from disk drop out of the index.
func (s *Store) reconcileOrphans(ctx context.Context, seen map[string]bool, stats *IndexStats) error {
	rows, err := s.db.QueryContext(ctx, `SELECT path FROM documents`)
	if err != nil {
		return err
	}
	var orphans []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return err
		}
		if !seen[p] {
			orphans = append(orphans, p)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, p := range orphans {
		if _, err := s.DeleteDocument(ctx, p); err != nil {
			return err
		}
		stats.Removed++
	}

	return nil
}

// DeleteDocument removes one document and its chunks by path; the triggers clear
// the FTS and vec rows. It is idempotent: an absent path is not an error. It
// reports whether a matching document was found and removed.
func (s *Store) DeleteDocument(ctx context.Context, key string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var docID int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM documents WHERE path = ?`, key).Scan(&docID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM chunks WHERE document_id = ?`, docID); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM documents WHERE id = ?`, docID); err != nil {
		return false, err
	}

	if err := tx.Commit(); err != nil {
		return false, err
	}

	return true, nil
}

// vectorPlan is what prepareIndex resolved before it changed anything: the manifest
// to pin, and whether the run wants a vector table at the probed dimension.
type vectorPlan struct {
	meta   Meta
	vector bool
	dim    int
}

// prepareIndex resolves the vector tier, then applies it and, on a reindex, rebuilds
// the schema, in one transaction.
//
// Resolving runs first and writes nothing. The dimension probe contacts the
// embeddings server, and the manifest checks refuse a configuration this index cannot
// serve, so a reindex against an unreachable server leaves the index whole rather than
// dropping it and then reporting that it cannot rebuild it.
//
// One transaction covers the rest. A cancel anywhere in it rolls back to the index
// that was there before, and no store is left carrying a vector table with nothing
// pinned, or a schema that is neither the old one nor the new one.
func (s *Store) prepareIndex(ctx context.Context, reindex bool) error {
	plan, err := s.planVectorTier(ctx, reindex)
	if err != nil {
		return err
	}

	return s.withTx(ctx, func(tx *sql.Tx) error {
		if reindex {
			if err := rebuildSchema(ctx, tx); err != nil {
				return fmt.Errorf("resetting index for reindex: %w", err)
			}
		}

		return applyVectorPlan(ctx, tx, plan)
	})
}

// planVectorTier probes the live model's dimension and reconciles it with the
// manifest before any embedding spend (invariant 1), refusing upfront when the
// configured embedding identity differs from an existing index and naming the fix. It
// writes nothing: a lexical-only store plans the format version alone, and every
// refusal here leaves the index untouched.
//
// The manifest checks run only when reindex is false. A reindex exists to resolve the
// mismatch they report, so applying them to one would refuse the command that fixes
// it.
func (s *Store) planVectorTier(ctx context.Context, reindex bool) (vectorPlan, error) {
	if s.emb == nil {
		return vectorPlan{meta: Meta{FormatVersion: formatVersion}}, nil
	}

	dim, err := s.emb.Dim(ctx)
	if err != nil {
		return vectorPlan{}, err
	}

	desired := Meta{
		FormatVersion:  formatVersion,
		Model:          s.emb.Model(),
		Dimension:      dim,
		Normalized:     true,
		QueryPrefix:    s.emb.QueryPrefix(),
		DocumentPrefix: s.emb.DocumentPrefix(),
	}
	plan := vectorPlan{meta: desired, vector: true, dim: dim}

	if reindex {
		return plan, nil
	}

	meta, err := s.readMeta(ctx)
	if err != nil {
		return vectorPlan{}, err
	}

	if meta.Model != "" {
		if meta.Model != desired.Model || meta.Dimension != desired.Dimension ||
			meta.QueryPrefix != desired.QueryPrefix || meta.DocumentPrefix != desired.DocumentPrefix || !meta.Normalized {
			return vectorPlan{}, fmt.Errorf("%w: manifest built with model=%s dim=%d; config requests model=%s dim=%d - run 'fisk-ai knowledge index --reindex'",
				ErrMetaMismatch, meta.Model, meta.Dimension, desired.Model, desired.Dimension)
		}

		return plan, nil
	}

	chunkCount, err := scanCount(ctx, s.db, `SELECT count(*) FROM chunks`)
	if err != nil {
		return vectorPlan{}, err
	}
	if chunkCount > 0 {
		return vectorPlan{}, fmt.Errorf("%w: the existing index is lexical-only but config now requests embeddings model=%s - run 'fisk-ai knowledge index --reindex'", ErrMetaMismatch, desired.Model)
	}

	return plan, nil
}

// applyVectorPlan creates the vec0 table at the planned dimension and its delete
// trigger, then pins the manifest, all inside tx. A reindex drops the table earlier in
// the same transaction, so IF NOT EXISTS here is safe and never a silent dimension
// no-op.
//
// The pin shares the transaction with the CREATEs so the pinned identity and the
// vectors on disk are committed together. A lexical-only plan pins the format version
// alone, which is what lets the read path detect a later switch to the vector tier and
// require a reindex.
func applyVectorPlan(ctx context.Context, tx *sql.Tx, plan vectorPlan) error {
	if plan.vector {
		stmts := []string{
			fmt.Sprintf(`CREATE VIRTUAL TABLE IF NOT EXISTS chunks_vec USING vec0(
				chunk_id  INTEGER PRIMARY KEY,
				embedding FLOAT[%d]
			)`, plan.dim),
			`CREATE TRIGGER IF NOT EXISTS chunks_ad_vec AFTER DELETE ON chunks BEGIN
				DELETE FROM chunks_vec WHERE chunk_id = old.id;
			END`,
		}
		for _, stmt := range stmts {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("creating vector table (dimension %d): %w", plan.dim, err)
			}
		}
	}

	return writeMeta(ctx, tx, plan.meta)
}

// Reset wipes all indexed data from an open writer store, leaving a clean empty
// index: the file and base schema remain, ready for the next knowledge index. It
// works even against an index whose pinned embedding identity no longer matches the
// config, and even against one whose full-text index no longer matches its content
// table. An interrupted reset leaves the index it was clearing.
func (s *Store) Reset(ctx context.Context) error {
	return s.withTx(ctx, func(tx *sql.Tx) error { return rebuildSchema(ctx, tx) })
}

// rebuildSchema drops every schema object and recreates it inside tx, leaving a clean
// empty index for a full rebuild. The vector table is among them, so a later model or
// dimension change is unconstrained without that being a special case; so is the
// manifest, which is why a reset store has nothing pinned.
//
// It drops rather than clearing rows, which lets it repair an index whose full-text
// tables no longer match their content table: see dropSchema. Recreating is
// ensureBaseSchema alone, so the schema has one definition.
//
// Both halves run on the caller's transaction rather than opening their own. The
// writer pool holds a single connection, so a nested BeginTx would wait for the
// connection this transaction is holding.
func rebuildSchema(ctx context.Context, tx *sql.Tx) error {
	if err := dropSchema(ctx, tx); err != nil {
		return err
	}

	return ensureBaseSchema(ctx, tx)
}

// withTx runs fn in a transaction, committing on success and rolling back on error.
func (s *Store) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := fn(tx); err != nil {
		return err
	}

	return tx.Commit()
}
