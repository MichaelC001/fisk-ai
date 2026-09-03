//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package rag

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
)

// lexicalConfig builds a lexical-only (no embeddings) config whose store lives
// under dir.
func lexicalConfig(dir string) *config.Config {
	return &config.Config{
		Identity: "test",
		Harness: config.HarnessConfig{
			RAG: &config.RAGConfig{Enabled: true, Directory: dir},
		},
	}
}

var _ = Describe("resolveDir", func() {
	ragCfg := func(dir string) *config.Config {
		return &config.Config{
			Identity: "agent",
			Harness:  config.HarnessConfig{RAG: &config.RAGConfig{Enabled: true, Directory: dir}},
		}
	}

	It("rebases the default knowledge directory under the store base", func() {
		Expect(resolveDir(ragCfg(""), "/srv/base")).To(Equal(filepath.Join("/srv/base", "knowledge", "agent")))
	})

	It("rebases a relative configured directory under the store base", func() {
		Expect(resolveDir(ragCfg("kb"), "/srv/base")).To(Equal(filepath.Join("/srv/base", "kb")))
	})

	It("honors an absolute configured directory regardless of the base", func() {
		Expect(resolveDir(ragCfg("/abs/kb"), "/srv/base")).To(Equal("/abs/kb"))
	})

	It("resolves relative to the working directory when no base is set", func() {
		Expect(resolveDir(ragCfg(""), "")).To(Equal(filepath.Join("knowledge", "agent")))
	})

	// StorePath names the file without opening or creating anything, which is what
	// lets a caller report an index it could not open.
	It("names the index file under the resolved directory", func() {
		Expect(StorePath(ragCfg("kb"), "/srv/base")).To(Equal(filepath.Join("/srv/base", "kb", dbFileName)))
	})
})

var _ = Describe("the Store present doctor check", func() {
	ctx := context.Background()

	detail := func(cfg *config.Config) string {
		GinkgoHelper()

		s, err := Open(cfg, "", Options{})
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(s.Close)

		r, err := s.Doctor(ctx, nil)
		Expect(err).ToNot(HaveOccurred())

		for _, c := range r.Checks {
			if c.Name == "Store present" {
				Expect(c.State).To(Equal(DoctorFail))
				return c.Detail
			}
		}

		Fail("the store check is absent from the report")
		return ""
	}

	// A relative dbPath in the report resolves against whichever directory the reader
	// is standing in, and standing in the wrong one is what produced the missing file.
	It("names the absolute path it found nothing at", func() {
		abs, err := filepath.Abs(filepath.Join("kb", dbFileName))
		Expect(err).ToNot(HaveOccurred())

		d := detail(lexicalConfig("kb"))
		Expect(d).To(ContainSubstring("no index file at " + abs))
		Expect(d).To(ContainSubstring("the configured knowledge directory is relative"))
	})

	It("says nothing about the working directory for an absolute directory", func() {
		dir := GinkgoT().TempDir()

		d := detail(lexicalConfig(dir))
		Expect(d).To(ContainSubstring("no index file at " + filepath.Join(dir, dbFileName)))
		Expect(d).ToNot(ContainSubstring("relative"))
	})
})

var _ = Describe("newStore", func() {
	// A reader and a writer are built from one config by one helper, so every value
	// that config decides has to be the same on both. Before the helper existed each
	// constructor filled these itself, and a field written into one alone was a store
	// that searched differently depending on which one opened it.
	It("gives a reader and a writer the same config-derived fields", func() {
		storeD := filepath.Join(GinkgoT().TempDir(), "knowledge")

		cfg := &config.Config{
			Identity: "test",
			Harness: config.HarnessConfig{
				RAG: &config.RAGConfig{
					Enabled:           true,
					Directory:         storeD,
					TopK:              7,
					MaxInjectedTokens: 4242,
					Embeddings: &config.RAGEmbeddingsConfig{
						BaseURL: "http://127.0.0.1:11434/v1",
						Model:   "bge-small",
					},
					Citations: []config.RAGCitationRule{{
						Pattern:         `^docs/(.+)\.md$`,
						Replace:         "https://example.net/$1#${anchor}",
						PatternCompiled: regexp.MustCompile(`^docs/(.+)\.md$`),
					}},
				},
			},
		}

		// The reader opens first, while no index file exists: a configured vector tier
		// against the writer's freshly created, unpinned manifest is a meta mismatch,
		// which says nothing about the fields under test.
		r, err := Open(cfg, "", Options{})
		Expect(err).ToNot(HaveOccurred())
		defer r.Close()

		w, err := OpenWriter(cfg, "", Options{})
		Expect(err).ToNot(HaveOccurred())
		defer w.Close()

		Expect(r.dir).To(Equal(storeD))
		Expect(w.dir).To(Equal(storeD))

		Expect(r.dbPath).To(Equal(filepath.Join(storeD, dbFileName)))
		Expect(w.dbPath).To(Equal(filepath.Join(storeD, dbFileName)))

		Expect(r.topK).To(Equal(7))
		Expect(w.topK).To(Equal(7))

		Expect(r.maxInjectedTokens).To(Equal(4242))
		Expect(w.maxInjectedTokens).To(Equal(4242))

		Expect(r.emb).ToNot(BeNil())
		Expect(w.emb).ToNot(BeNil())
		Expect(r.emb.Model()).To(Equal("bge-small"))
		Expect(w.emb.Model()).To(Equal("bge-small"))

		// The writer answers searches too, so its mapper has to rewrite the same paths
		// the reader's does rather than hand the model a raw token.
		readerAddr, readerOK := r.citations.Render("docs/guide.md", 3, "Design > Backpressure")
		writerAddr, writerOK := w.citations.Render("docs/guide.md", 3, "Design > Backpressure")
		Expect(readerOK).To(BeTrue())
		Expect(writerOK).To(BeTrue())
		Expect(readerAddr).To(Equal("https://example.net/guide#backpressure"))
		Expect(writerAddr).To(Equal("https://example.net/guide#backpressure"))

		// readOnly comes from the helper's parameter, and is the one field the two are
		// meant to disagree on.
		Expect(r.readOnly).To(BeTrue())
		Expect(w.readOnly).To(BeFalse())
	})
})

// writeDoc writes a document under root, creating parent directories.
func writeDoc(root, rel, content string) string {
	path := filepath.Join(root, rel)
	Expect(os.MkdirAll(filepath.Dir(path), 0o755)).To(Succeed())
	Expect(os.WriteFile(path, []byte(content), 0o644)).To(Succeed())

	return path
}

var _ = Describe("The walk's extension set", func() {
	ctx := context.Background()

	var (
		docsD string
		cfg   *config.Config
	)

	BeforeEach(func() {
		tmp := GinkgoT().TempDir()
		docsD = filepath.Join(tmp, "docs")
		cfg = lexicalConfig(filepath.Join(tmp, "knowledge"))
	})

	index := func(opts IndexOptions) *IndexStats {
		w, err := OpenWriter(cfg, "", Options{})
		Expect(err).ToNot(HaveOccurred())
		defer w.Close()

		stats, err := w.Index(ctx, []string{docsD}, opts)
		Expect(err).ToNot(HaveOccurred())

		return stats
	}

	It("builds a new default map on every call", func() {
		first := DefaultExtensions()
		first[".rst"] = true
		delete(first, ".md")

		Expect(DefaultExtensions()).To(Equal(map[string]bool{
			".md":       true,
			".markdown": true,
			".txt":      true,
			".text":     true,
		}))
	})

	It("indexes the default extensions after a caller edits the map it was handed", func() {
		handed := DefaultExtensions()
		handed[".rst"] = true

		writeDoc(docsD, "design.md", "# Design\n\nThe queue applies backpressure.\n")
		writeDoc(docsD, "guide.rst", "Guide\n=====\n\nRestructured text the walk does not index.\n")

		stats := index(IndexOptions{Reconcile: true})
		Expect(stats.Files).To(Equal(1))
		Expect(stats.Added).To(Equal(1))
	})

	// Index copies the caller's map before the first root, so a write to it while the
	// walk is running cannot change which files the rest of the walk accepts. OnFile
	// runs on the walk goroutine between files, which is where the race would land.
	It("walks the extension set the call was made with, not a write made during the walk", func() {
		writeDoc(docsD, "a.md", "# A\n\nThe queue applies backpressure.\n")
		writeDoc(docsD, "b.txt", "Tokens are validated against the issuer.\n")

		exts := map[string]bool{".md": true, ".txt": true}
		stats := index(IndexOptions{
			Reconcile:  true,
			Extensions: exts,
			OnFile:     func(IndexEvent) { delete(exts, ".txt") },
		})

		Expect(stats.Files).To(Equal(2))
	})

	// knowledge.paths is a list, so a walk over several roots is the ordinary shape. A
	// copy taken per root would let a write made while the first root walks reach the
	// second.
	It("walks every root with the set the call was made with", func() {
		tmp := GinkgoT().TempDir()
		one := filepath.Join(tmp, "one")
		two := filepath.Join(tmp, "two")

		writeDoc(one, "a.md", "# A\n\nThe queue applies backpressure.\n")
		writeDoc(two, "b.txt", "Tokens are validated against the issuer.\n")

		w, err := OpenWriter(cfg, "", Options{})
		Expect(err).ToNot(HaveOccurred())
		defer w.Close()

		exts := map[string]bool{".md": true, ".txt": true}
		stats, err := w.Index(ctx, []string{one, two}, IndexOptions{
			Reconcile:  true,
			Extensions: exts,
			OnFile:     func(IndexEvent) { delete(exts, ".txt") },
		})
		Expect(err).ToNot(HaveOccurred())

		Expect(stats.Files).To(Equal(2))
	})
})

var _ = Describe("ChunkText", func() {
	ctx := context.Background()

	var (
		docsD string
		cfg   *config.Config
	)

	BeforeEach(func() {
		tmp := GinkgoT().TempDir()
		docsD = filepath.Join(tmp, "docs")
		cfg = lexicalConfig(filepath.Join(tmp, "knowledge"))
	})

	It("reports ErrIndexNotBuilt when no index file exists", func() {
		r, err := Open(cfg, "", Options{})
		Expect(err).ToNot(HaveOccurred())
		defer r.Close()

		_, _, err = r.ChunkText(ctx, "docs/design.md", 0)
		Expect(errors.Is(err, ErrIndexNotBuilt)).To(BeTrue())
	})

	It("reports ErrCitationNotFound for a citation that names no chunk", func() {
		path := writeDoc(docsD, "design.md", "# Design\n\nThe queue applies backpressure.\n")

		w, err := OpenWriter(cfg, "", Options{})
		Expect(err).ToNot(HaveOccurred())
		_, err = w.Index(ctx, []string{docsD}, IndexOptions{Reconcile: true})
		Expect(err).ToNot(HaveOccurred())
		w.Close()

		r, err := Open(cfg, "", Options{})
		Expect(err).ToNot(HaveOccurred())
		defer r.Close()

		// Ordinal 0 of the indexed file resolves, so the miss is the ordinal alone.
		_, _, err = r.ChunkText(ctx, filepath.ToSlash(path), 0)
		Expect(err).ToNot(HaveOccurred())

		_, _, err = r.ChunkText(ctx, filepath.ToSlash(path), 4242)
		Expect(errors.Is(err, ErrCitationNotFound)).To(BeTrue())
		Expect(errors.Is(err, sql.ErrNoRows)).To(BeFalse())

		_, _, err = r.ChunkText(ctx, "no/such/file.md", 0)
		Expect(errors.Is(err, ErrCitationNotFound)).To(BeTrue())
	})
})

var _ = Describe("Store (lexical tier)", func() {
	ctx := context.Background()

	var (
		tmp    string
		storeD string
		docsD  string
		cfg    *config.Config
	)

	BeforeEach(func() {
		tmp = GinkgoT().TempDir()
		storeD = filepath.Join(tmp, "knowledge")
		docsD = filepath.Join(tmp, "docs")
		cfg = lexicalConfig(storeD)

		writeDoc(docsD, "backpressure.md", "# Design\n\n## Backpressure\n\nThe queue applies backpressure when the buffer is full so producers slow down.\n")
		writeDoc(docsD, "auth.md", "# Authentication\n\nTokens are validated against the issuer before any request proceeds.\n")
	})

	index := func() *IndexStats {
		w, err := OpenWriter(cfg, "", Options{})
		Expect(err).ToNot(HaveOccurred())
		defer w.Close()
		stats, err := w.Index(ctx, []string{docsD}, IndexOptions{Reconcile: true})
		Expect(err).ToNot(HaveOccurred())
		return stats
	}

	It("builds a lexical index and retrieves the on-topic section", func() {
		stats := index()
		Expect(stats.Added).To(Equal(2))
		Expect(stats.Chunks).To(BeNumerically(">=", 2))

		r, err := Open(cfg, "", Options{})
		Expect(err).ToNot(HaveOccurred())
		defer r.Close()
		Expect(r.Built()).To(BeTrue())

		res, err := r.Search(ctx, "how does backpressure work", 5)
		Expect(err).ToNot(HaveOccurred())
		Expect(res.Status).To(Equal(StatusOK))
		Expect(res.Hits).ToNot(BeEmpty())
		Expect(res.Hits[0].DocPath).To(ContainSubstring("backpressure.md"))
		Expect(res.Hits[0].Citation).To(MatchRegexp(`backpressure\.md#\d+`))
	})

	It("reports index_not_built before any index exists", func() {
		r, err := Open(cfg, "", Options{})
		Expect(err).ToNot(HaveOccurred())
		defer r.Close()
		Expect(r.Built()).To(BeFalse())

		res, err := r.Search(ctx, "anything", 5)
		Expect(err).ToNot(HaveOccurred())
		Expect(res.Status).To(Equal(StatusIndexNotBuilt))
	})

	It("skips unchanged files and reconciles deletions on a full-root walk", func() {
		index()

		// Remove one file, re-index the whole root: it should be reconciled away.
		Expect(os.Remove(filepath.Join(docsD, "auth.md"))).To(Succeed())

		w, err := OpenWriter(cfg, "", Options{})
		Expect(err).ToNot(HaveOccurred())
		stats, err := w.Index(ctx, []string{docsD}, IndexOptions{Reconcile: true})
		Expect(err).ToNot(HaveOccurred())
		Expect(stats.Removed).To(Equal(1))
		Expect(stats.Skipped).To(Equal(1)) // backpressure.md unchanged
		w.Close()

		r, err := Open(cfg, "", Options{})
		Expect(err).ToNot(HaveOccurred())
		defer r.Close()
		res, err := r.Search(ctx, "authentication tokens issuer", 5)
		Expect(err).ToNot(HaveOccurred())
		for _, h := range res.Hits {
			Expect(h.DocPath).ToNot(ContainSubstring("auth.md"))
		}
	})

	It("does not reconcile-delete on a subpath (non-reconciling) walk", func() {
		index()
		Expect(os.Remove(filepath.Join(docsD, "auth.md"))).To(Succeed())

		w, err := OpenWriter(cfg, "", Options{})
		Expect(err).ToNot(HaveOccurred())
		stats, err := w.Index(ctx, []string{docsD}, IndexOptions{Reconcile: false})
		Expect(err).ToNot(HaveOccurred())
		Expect(stats.Removed).To(Equal(0))
		w.Close()

		st, err := statsFor(cfg)
		Expect(err).ToNot(HaveOccurred())
		Expect(st.Documents).To(Equal(2)) // auth.md still indexed despite deletion
	})

	It("refuses a second concurrent writer with the advisory lock", func() {
		w1, err := OpenWriter(cfg, "", Options{})
		Expect(err).ToNot(HaveOccurred())
		defer w1.Close()

		_, err = OpenWriter(cfg, "", Options{})
		Expect(err).To(MatchError(ErrLocked))
	})

	It("keeps the DB file private (0600) after a write", func() {
		index()
		fi, err := os.Stat(filepath.Join(storeD, dbFileName))
		Expect(err).ToNot(HaveOccurred())
		Expect(fi.Mode().Perm()).To(Equal(os.FileMode(0o600)))
	})

	// A mode=ro reader must read correctly while a separate writer commits, and see
	// each committed snapshot without torn state. This is the multi-process WAL
	// validation gate exercised in-process (separate connections behave identically
	// to separate processes under SQLite WAL).
	It("serves a read-only reader concurrently with a live writer", func() {
		index() // establishes the file + WAL

		reader, err := Open(cfg, "", Options{})
		Expect(err).ToNot(HaveOccurred())
		defer reader.Close()

		writer, err := OpenWriter(cfg, "", Options{})
		Expect(err).ToNot(HaveOccurred())
		defer writer.Close()

		// The reader sees the current committed state.
		res, err := reader.Search(ctx, "backpressure buffer", 5)
		Expect(err).ToNot(HaveOccurred())
		Expect(res.Hits).ToNot(BeEmpty())

		// The writer commits a brand-new document while the reader stays open.
		writeDoc(docsD, "sharding.md", "# Sharding\n\nKeys are hashed to shards for horizontal scale.\n")
		_, err = writer.Index(ctx, []string{docsD}, IndexOptions{Reconcile: true})
		Expect(err).ToNot(HaveOccurred())

		// A fresh per-query read transaction sees the new commit; no torn state.
		res, err = reader.Search(ctx, "sharding horizontal scale keys", 5)
		Expect(err).ToNot(HaveOccurred())
		found := false
		for _, h := range res.Hits {
			if filepath.Base(h.DocPath) == "sharding.md" {
				found = true
			}
		}
		Expect(found).To(BeTrue())
	})
})

var _ = Describe("Store rm and reset", func() {
	ctx := context.Background()

	var (
		tmp    string
		storeD string
		docsD  string
		cfg    *config.Config
	)

	BeforeEach(func() {
		tmp = GinkgoT().TempDir()
		storeD = filepath.Join(tmp, "knowledge")
		docsD = filepath.Join(tmp, "docs")
		cfg = lexicalConfig(storeD)

		writeDoc(docsD, "backpressure.md", "# Design\n\n## Backpressure\n\nThe queue applies backpressure when the buffer is full so producers slow down.\n")
		writeDoc(docsD, "auth.md", "# Authentication\n\nTokens are validated against the issuer before any request proceeds.\n")

		w, err := OpenWriter(cfg, "", Options{})
		Expect(err).ToNot(HaveOccurred())
		defer w.Close()
		_, err = w.Index(ctx, []string{docsD}, IndexOptions{Reconcile: true})
		Expect(err).ToNot(HaveOccurred())
	})

	It("reports StoreExists only once an index file is present", func() {
		empty := lexicalConfig(filepath.Join(tmp, "empty"))
		exists, err := StoreExists(empty, "")
		Expect(err).ToNot(HaveOccurred())
		Expect(exists).To(BeFalse())

		exists, err = StoreExists(cfg, "")
		Expect(err).ToNot(HaveOccurred())
		Expect(exists).To(BeTrue())
	})

	It("removes a known document and reports it, leaving others intact", func() {
		w, err := OpenWriter(cfg, "", Options{})
		Expect(err).ToNot(HaveOccurred())
		defer w.Close()

		removed, err := w.DeleteDocument(ctx, filepath.Join(docsD, "auth.md"))
		Expect(err).ToNot(HaveOccurred())
		Expect(removed).To(BeTrue())

		st, err := w.Stats(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(st.Documents).To(Equal(1))

		res, err := w.Search(ctx, "authentication tokens issuer", 5)
		Expect(err).ToNot(HaveOccurred())
		for _, h := range res.Hits {
			Expect(h.DocPath).ToNot(ContainSubstring("auth.md"))
		}
	})

	It("reports a miss for an unknown document without erroring", func() {
		w, err := OpenWriter(cfg, "", Options{})
		Expect(err).ToNot(HaveOccurred())
		defer w.Close()

		removed, err := w.DeleteDocument(ctx, filepath.Join(docsD, "does-not-exist.md"))
		Expect(err).ToNot(HaveOccurred())
		Expect(removed).To(BeFalse())

		st, err := w.Stats(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(st.Documents).To(Equal(2))
	})

	It("wipes all data on Reset, leaving a clean empty index", func() {
		w, err := OpenWriter(cfg, "", Options{})
		Expect(err).ToNot(HaveOccurred())

		Expect(w.Reset(ctx)).To(Succeed())

		st, err := w.Stats(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(st.Documents).To(Equal(0))
		Expect(st.Chunks).To(Equal(0))
		w.Close()

		// The file remains and a fresh search reports an empty (not unbuilt) index.
		r, err := Open(cfg, "", Options{})
		Expect(err).ToNot(HaveOccurred())
		defer r.Close()
		res, err := r.Search(ctx, "backpressure buffer", 5)
		Expect(err).ToNot(HaveOccurred())
		Expect(res.Status).To(Equal(StatusIndexEmpty))
	})

	It("returns freed pages to the OS on Reset (auto_vacuum=FULL)", func() {
		dbPath := filepath.Join(storeD, dbFileName)

		// Grow the index well past its initial size so a failure to compact is
		// unmistakable, then checkpoint so the pages land in the main file.
		w, err := OpenWriter(cfg, "", Options{})
		Expect(err).ToNot(HaveOccurred())
		for i := range 200 {
			writeDoc(docsD, fmt.Sprintf("bulk/doc%d.md", i), "# Doc\n\n"+strings.Repeat("padding content for the index ", 200)+"\n")
		}
		_, err = w.Index(ctx, []string{docsD}, IndexOptions{Reconcile: true})
		Expect(err).ToNot(HaveOccurred())
		_, err = w.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
		Expect(err).ToNot(HaveOccurred())

		full, err := os.Stat(dbPath)
		Expect(err).ToNot(HaveOccurred())

		Expect(w.Reset(ctx)).To(Succeed())
		_, err = w.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
		Expect(err).ToNot(HaveOccurred())
		w.Close()

		after, err := os.Stat(dbPath)
		Expect(err).ToNot(HaveOccurred())
		Expect(after.Size()).To(BeNumerically("<", full.Size()/2))
	})
})

var _ = Describe("Store DeleteDocumentsUnder", func() {
	ctx := context.Background()

	var (
		docsD string
		cfg   *config.Config
		w     *Store
	)

	// The corpus pairs each directory with a sibling that a LIKE pattern built from
	// its name would also match: "ab.md" against "a", "axb" against "a_b", "aZZb"
	// against "a%b".
	BeforeEach(func() {
		tmp := GinkgoT().TempDir()
		docsD = filepath.Join(tmp, "docs")
		cfg = lexicalConfig(filepath.Join(tmp, "knowledge"))

		writeDoc(docsD, "a/one.md", "# One\n\nthe first document body\n")
		writeDoc(docsD, "a/deep/two.md", "# Two\n\nthe second document body\n")
		writeDoc(docsD, "ab.md", "# Sibling\n\nthe sibling document body\n")
		writeDoc(docsD, "a_b/under.md", "# Under\n\nthe underscore document body\n")
		writeDoc(docsD, "axb/plain.md", "# Plain\n\nthe plain document body\n")
		writeDoc(docsD, "a%b/pct.md", "# Percent\n\nthe percent document body\n")
		writeDoc(docsD, "aZZb/other.md", "# Other\n\nthe other document body\n")

		var err error
		w, err = OpenWriter(cfg, "", Options{})
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(w.Close)

		_, err = w.Index(ctx, []string{docsD}, IndexOptions{Reconcile: true})
		Expect(err).ToNot(HaveOccurred())
	})

	doc := func(rel string) string {
		return filepath.ToSlash(filepath.Join(docsD, rel))
	}

	orphanChunks := func() int {
		var n int
		err := w.db.QueryRowContext(ctx, `SELECT count(*) FROM chunks WHERE document_id NOT IN (SELECT id FROM documents)`).Scan(&n)
		Expect(err).ToNot(HaveOccurred())

		return n
	}

	It("removes every document under the directory along with its chunks", func() {
		before, err := w.Stats(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(before.Chunks).To(BeNumerically(">=", 7))

		removed, err := w.DeleteDocumentsUnder(ctx, filepath.Join(docsD, "a"))
		Expect(err).ToNot(HaveOccurred())
		Expect(removed).To(Equal(2))

		Expect(documentPaths(w)).ToNot(ContainElements(doc("a/one.md"), doc("a/deep/two.md")))

		after, err := w.Stats(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(after.Documents).To(Equal(before.Documents - 2))
		Expect(after.Chunks).To(BeNumerically("<", before.Chunks))
		Expect(orphanChunks()).To(Equal(0))
	})

	It("leaves a sibling whose name starts with the directory name", func() {
		_, err := w.DeleteDocumentsUnder(ctx, filepath.Join(docsD, "a"))
		Expect(err).ToNot(HaveOccurred())

		Expect(documentPaths(w)).To(ContainElement(doc("ab.md")))
	})

	It("matches a directory holding an underscore or a percent literally", func() {
		removed, err := w.DeleteDocumentsUnder(ctx, filepath.Join(docsD, "a_b"))
		Expect(err).ToNot(HaveOccurred())
		Expect(removed).To(Equal(1))

		removed, err = w.DeleteDocumentsUnder(ctx, filepath.Join(docsD, "a%b"))
		Expect(err).ToNot(HaveOccurred())
		Expect(removed).To(Equal(1))

		Expect(documentPaths(w)).To(ContainElements(doc("axb/plain.md"), doc("aZZb/other.md")))
	})

	It("removes nothing for a directory that holds no documents", func() {
		removed, err := w.DeleteDocumentsUnder(ctx, filepath.Join(docsD, "absent"))
		Expect(err).ToNot(HaveOccurred())
		Expect(removed).To(Equal(0))

		st, err := w.Stats(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(st.Documents).To(Equal(7))
	})

	It("ignores a trailing slash on the directory", func() {
		removed, err := w.DeleteDocumentsUnder(ctx, filepath.Join(docsD, "a")+"/")
		Expect(err).ToNot(HaveOccurred())
		Expect(removed).To(Equal(2))
	})
})

// documentPaths returns every indexed document path, sorted.
func documentPaths(s *Store) []string {
	GinkgoHelper()

	rows, err := s.db.QueryContext(context.Background(), `SELECT path FROM documents ORDER BY path`)
	Expect(err).ToNot(HaveOccurred())
	defer rows.Close()

	var out []string
	for rows.Next() {
		var p string
		Expect(rows.Scan(&p)).To(Succeed())
		out = append(out, p)
	}
	Expect(rows.Err()).ToNot(HaveOccurred())

	return out
}

// statsFor opens a read-only store and returns its stats.
func statsFor(cfg *config.Config) (*Stats, error) {
	r, err := Open(cfg, "", Options{})
	if err != nil {
		return nil, err
	}
	defer r.Close()

	return r.Stats(context.Background())
}
