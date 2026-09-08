//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// Package file is the file-backed session store: each run is a JSON-lines journal
// (<id>.json) under a directory, guarded by a per-run lock file (<id>.lock).
// Importing this package registers the backend under runstate.BackendFile, so the
// program links it in by importing it (usually for its side effect). It holds no
// exported API beyond that registration.
package file

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/choria-io/fisk-ai/internal/runstate"
)

func init() {
	runstate.Register(runstate.BackendFile, newStore)
}

// options is the typed shape of the file backend's session options.
type options struct {
	// Directory is where run journals live. It defaults to runstate.DefaultDir()
	// (the absolute XDG state path), never the working directory, so runs never leak
	// into a repository.
	Directory string `json:"directory"`
}

// newStore is the runstate.Factory for the file backend: it decodes the options
// block, resolves the directory, and opens the store. A construction failure (bad
// options, an unwritable directory) surfaces here at run start.
//
// Resolution: a configured directory wins (relative ones rebased under env.StoreDir);
// otherwise, with a run-store base set, journals root under env.StoreDir/runs so a
// run's state sits alongside its memory and knowledge, and without one they default
// to the absolute XDG path. Either way runs never land in the working directory.
func newStore(env runstate.RuntimeEnv, raw json.RawMessage) (runstate.Store, error) {
	opts, err := decodeOptions(raw)
	if err != nil {
		return nil, err
	}

	dir := opts.Directory
	switch {
	case dir != "":
		if env.StoreDir != "" && !filepath.IsAbs(dir) {
			dir = filepath.Join(env.StoreDir, dir)
		}
	case env.StoreDir != "":
		dir = filepath.Join(env.StoreDir, "runs")
	default:
		dir, err = runstate.DefaultDir()
		if err != nil {
			return nil, err
		}
	}

	return NewFileStore(dir)
}

// decodeOptions strictly decodes the backend options. A stdlib decoder with
// DisallowUnknownFields catches a mistyped option key the same way the config
// layer catches a mistyped top-level key.
func decodeOptions(raw json.RawMessage) (options, error) {
	var opts options
	if len(raw) == 0 {
		return opts, nil
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&opts); err != nil {
		return opts, fmt.Errorf("invalid file session options: %w", err)
	}

	return opts, nil
}

// FileStore stores each run as a JSON-lines journal (<id>.json) under a directory,
// guarded by a per-run lock file (<id>.lock). Conversation and tool IO are
// sensitive, so the directory is 0700 and journals are 0600.
//
// Every method that reaches the filesystem returns the context's error before it opens
// anything, and List checks again before each run it reads, since a large store reads
// one journal per run. The individual syscalls behind a single record are not
// cancellable, so a call that has started its write finishes it.
type FileStore struct {
	dir string
}

// NewFileStore returns a FileStore rooted at dir, creating it 0700 if needed.
func NewFileStore(dir string) (*FileStore, error) {
	err := os.MkdirAll(dir, 0o700)
	if err != nil {
		return nil, fmt.Errorf("creating run store %q: %w", dir, err)
	}

	return &FileStore{dir: dir}, nil
}

func (s *FileStore) journalPath(id string) string {
	return filepath.Join(s.dir, id+".json")
}

func (s *FileStore) lockPath(id string) string {
	return filepath.Join(s.dir, id+".lock")
}

// journalExists reports whether a run's journal is on disk. It is advisory: the
// authoritative create-guard is the O_EXCL open in openJournal, and this only
// disambiguates a lock held by a live run from one held by a concurrent creator.
func (s *FileStore) journalExists(id string) bool {
	_, err := os.Stat(s.journalPath(id))

	return err == nil
}

// Info implements runstate.Store, reporting the backend and, deliberately, no location.
//
// This store's container is its directory, an absolute local filesystem path so for now not exposed for security reasons
func (s *FileStore) Info() runstate.Info {
	return runstate.Info{Backend: runstate.BackendFile}
}

// Create implements runstate.Store.
//
// A meta record that fails to write, the canceled context among its causes, leaves the
// journal file and the lock file this made to hold the id. That is what runstate.Store
// documents: Create is not all-or-nothing, a second Create of the id returns ErrExists,
// and Load reports the empty journal as runstate.ErrEmpty.
func (s *FileStore) Create(ctx context.Context, id string, meta runstate.MetaRecord) (runstate.Journal, error) {
	err := ctx.Err()
	if err != nil {
		return nil, err
	}

	err = runstate.ValidateID(id)
	if err != nil {
		return nil, err
	}

	// meta is this store's copy, so stamping the version leaves the caller's struct alone.
	err = runstate.PrepareMeta(&meta)
	if err != nil {
		return nil, err
	}

	// The existence check lives inside openJournal, which holds the run's lock and
	// opens O_EXCL, so checking and creating are one atomic step rather than a stat
	// followed by an open that a second creator can interleave with.
	//
	// A lock held by someone else means the id is either already live or being
	// created right now. Only the first of those is ErrExists, so the journal is
	// stat'd to tell them apart; a caller racing an established run reports the id
	// as present rather than merely busy.
	j, err := s.openJournal(id, true)
	if err != nil {
		if errors.Is(err, runstate.ErrLocked) && s.journalExists(id) {
			return nil, fmt.Errorf("%w: %q", runstate.ErrExists, id)
		}
		return nil, err
	}

	err = j.Append(ctx, 1, runstate.Record{Seq: 1, Protocol: runstate.MetaProtocol, Meta: &meta})
	if err != nil {
		j.Close()
		return nil, err
	}

	return j, nil
}

// Open implements runstate.Store.
func (s *FileStore) Open(ctx context.Context, id string) (runstate.Journal, error) {
	err := ctx.Err()
	if err != nil {
		return nil, err
	}

	err = runstate.ValidateID(id)
	if err != nil {
		return nil, err
	}

	_, err = os.Stat(s.journalPath(id))
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("%w: %q", runstate.ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}

	return s.openJournal(id, false)
}

// openJournal locks the run and opens its journal for appending. With created set
// the journal must not already exist: the open adds O_EXCL so the create cannot
// race another creator holding no lock yet, and an existing journal surfaces as
// ErrExists rather than being silently appended to.
func (s *FileStore) openJournal(id string, created bool) (*fileJournal, error) {
	lock, err := acquireLock(s.lockPath(id))
	if err != nil {
		return nil, err
	}

	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if created {
		flags |= os.O_EXCL
	}

	f, err := os.OpenFile(s.journalPath(id), flags, 0o600)
	if err != nil {
		lock.release()
		if created && errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("%w: %q", runstate.ErrExists, id)
		}
		return nil, err
	}

	j := &fileJournal{path: s.journalPath(id), f: f, lock: lock, dirCreated: created, dir: s.dir}

	last, err := lastSeq(s.journalPath(id))
	if err != nil {
		j.Close()
		return nil, err
	}
	j.lastSeq = last

	return j, nil
}

// Load implements runstate.Store.
func (s *FileStore) Load(ctx context.Context, id string) (*runstate.RunState, error) {
	err := ctx.Err()
	if err != nil {
		return nil, err
	}

	err = runstate.ValidateID(id)
	if err != nil {
		return nil, err
	}

	recs, err := readRecords(s.journalPath(id))
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("%w: %q", runstate.ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}

	return runstate.Fold(recs)
}

// List implements runstate.Store. It reads and folds one journal per run, so it checks
// the context before each of them as well as at the start. The filter is applied through
// runstate.ListFilter: the prefix against the journal's name, before the journal is read,
// and the agent against the folded run, so a run with no agent is listed under any agent.
func (s *FileStore) List(ctx context.Context, filter runstate.ListFilter) ([]runstate.RunInfo, error) {
	err := ctx.Err()
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}

	var out []runstate.RunInfo
	for _, e := range entries {
		err = ctx.Err()
		if err != nil {
			return nil, err
		}

		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".json" {
			continue
		}
		id := name[:len(name)-len(".json")]
		if runstate.ValidateID(id) != nil {
			continue
		}
		if !filter.MatchesID(id) {
			continue
		}

		info, err := s.summarize(id)
		if err != nil {
			continue
		}
		if !filter.MatchesAgent(info.Agent) {
			continue
		}

		out = append(out, *info)
	}

	return out, nil
}

// ListPage implements runstate.Store.
//
// Ordering a directory of journals needs each run's creation time, which is on the meta
// record every journal opens with, so this reads the first line of each and orders on
// what it finds. Only the page's runs are then folded. The page is spared the
// whole-journal read List makes of every run, which is the read that grows with a
// conversation.
//
// Two runs can carry the same creation time, so the run id orders those and the cursor
// holds the pair.
//
// The creation time is the one the caller stamped on the meta record and no store
// controls it, so this cursor does not carry RunPage.Cursor's guarantee that a run
// created after a page is enumerated after it. A run stamped earlier than the run a page
// ended on sorts before that page's cursor, and a walk resumed there never reaches it.
// Two agents whose clocks disagree, or one clock stepped backwards, both produce it, and
// a caller that must see every run walks from an empty cursor. Nothing on disk is
// monotonic: a run id is whatever the caller chose, and a journal's modification time is
// its last turn rather than its first.
func (s *FileStore) ListPage(ctx context.Context, filter runstate.ListFilter, limit int, cursor string) (runstate.RunPage, error) {
	err := runstate.CheckPageLimit(limit)
	if err != nil {
		return runstate.RunPage{}, err
	}

	after, err := parseCursor(cursor)
	if err != nil {
		return runstate.RunPage{}, err
	}

	err = ctx.Err()
	if err != nil {
		return runstate.RunPage{}, err
	}

	order, err := s.creationOrder(ctx, filter)
	if err != nil {
		return runstate.RunPage{}, err
	}

	var runs []runstate.RunInfo
	var last position
	for _, p := range order {
		if len(runs) == limit {
			break
		}

		err = ctx.Err()
		if err != nil {
			return runstate.RunPage{}, err
		}

		if after != nil && !after.precedes(p) {
			continue
		}
		last = p

		// The agent came off the meta record with the creation time, so an excluded run
		// is dropped before its journal is folded. Folding it first would cost the
		// whole-journal read the page exists to avoid, once per run another agent wrote.
		if !filter.MatchesAgent(p.agent) {
			continue
		}

		info, err := s.summarize(p.id)
		if err != nil {
			continue
		}

		runs = append(runs, *info)
	}

	page := runstate.RunPage{Runs: runs}
	// A short page ran out of runs. A full one stops where it stops, and the caller
	// finds out whether anything follows by asking for the next page.
	if len(runs) == limit {
		page.Cursor = last.cursor()
	}

	return page, nil
}

// position is where a page ended: the run it ended on, in the order runs are enumerated.
type position struct {
	created time.Time
	id      string
	// agent is MetaRecord.Agent, read off the same line as the creation time so a page
	// answers the filter without folding the run. It is not part of the cursor.
	agent string
}

// precedes reports whether other comes after p in creation order, which a page resuming
// at p takes.
func (p position) precedes(other position) bool {
	if !other.created.Equal(p.created) {
		return other.created.After(p.created)
	}

	return other.id > p.id
}

// cursorSeparator divides the two halves of a cursor. A run id is letters, digits, '-'
// and '_', and a timestamp holds none of it, so neither half can carry one.
const cursorSeparator = "|"

// cursor writes the position a page resumes after. It is base64 so that a caller reads
// it as the store's own value rather than as a run id and a date to take apart, and the
// JetStream backend refuses it.
func (p position) cursor() string {
	return base64.RawURLEncoding.EncodeToString([]byte(p.created.UTC().Format(time.RFC3339Nano) + cursorSeparator + p.id))
}

// parseCursor reads a cursor back into the position a page resumes after, returning nil
// for an empty cursor, which starts at the oldest run in the store.
func parseCursor(cursor string) (*position, error) {
	invalid := func() error {
		return fmt.Errorf("%w: %q is not a position this store minted", runstate.ErrInvalidCursor, cursor)
	}

	if cursor == "" {
		return nil, nil
	}

	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, invalid()
	}

	stamp, id, ok := strings.Cut(string(raw), cursorSeparator)
	if !ok {
		return nil, invalid()
	}
	created, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return nil, invalid()
	}
	err = runstate.ValidateID(id)
	if err != nil {
		return nil, invalid()
	}

	return &position{created: created, id: id}, nil
}

// creationOrder enumerates the runs the filter's prefix selects, oldest first, reading
// each journal's meta record and nothing else. That record carries the agent as well as
// the creation time, so a filtered page answers the agent from it. The prefix is answered
// from the journal's name, before the file is opened. A journal whose first line is
// missing or unreadable is left out, as List leaves it out: the meta record is written
// first, so a journal without one holds no run to name.
func (s *FileStore) creationOrder(ctx context.Context, filter runstate.ListFilter) ([]position, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}

	out := make([]position, 0, len(entries))
	for _, e := range entries {
		err = ctx.Err()
		if err != nil {
			return nil, err
		}

		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".json" {
			continue
		}
		id := name[:len(name)-len(".json")]
		if runstate.ValidateID(id) != nil {
			continue
		}
		if !filter.MatchesID(id) {
			continue
		}

		meta, err := readMeta(s.journalPath(id))
		if err != nil {
			continue
		}

		out = append(out, position{created: meta.Created, id: id, agent: meta.Agent})
	}

	slices.SortFunc(out, func(a, b position) int {
		if !a.created.Equal(b.created) {
			return a.created.Compare(b.created)
		}

		return strings.Compare(a.id, b.id)
	})

	return out, nil
}

// summarize folds one run into a listing row. Both listings build a row here, so the two
// cannot describe the same run differently.
func (s *FileStore) summarize(id string) (*runstate.RunInfo, error) {
	path := s.journalPath(id)

	recs, err := readRecords(path)
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 || recs[0].Meta == nil {
		return nil, fmt.Errorf("%w: run %q opens on no meta record", runstate.ErrCorrupt, id)
	}

	rs, err := runstate.Fold(recs)
	if err != nil {
		return nil, err
	}

	info := runstate.RunInfo{RunID: rs.RunID, Created: recs[0].Meta.Created, Model: rs.Fingerprint.Model, Prompt: rs.Prompt, Agent: rs.Agent}
	fi, err := os.Stat(path)
	if err == nil {
		info.Updated = fi.ModTime()
	}
	if rs.Terminal != nil {
		info.Terminal = rs.Terminal.Reason
		info.Summary = rs.Terminal.Summary
	}

	return &info, nil
}

// readMeta reads a journal's first record, the meta record every run is created with.
// It reads the head of the file rather than the whole journal, so ordering a store of
// long conversations costs one short read per run.
func readMeta(path string) (*runstate.MetaRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	line, err := bufio.NewReader(f).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}

	var rec runstate.Record
	err = json.Unmarshal(line, &rec)
	if err != nil {
		return nil, fmt.Errorf("%w: run at %q opens on an unreadable record: %w", runstate.ErrCorrupt, filepath.Base(path), err)
	}
	if rec.Meta == nil {
		return nil, fmt.Errorf("%w: run at %q opens on no meta record", runstate.ErrCorrupt, filepath.Base(path))
	}

	return rec.Meta, nil
}

// Delete implements runstate.Store. The context is read once, before the first removal,
// so the journal and its lock file go together.
func (s *FileStore) Delete(ctx context.Context, id string) error {
	err := ctx.Err()
	if err != nil {
		return err
	}

	err = runstate.ValidateID(id)
	if err != nil {
		return err
	}

	err = os.Remove(s.journalPath(id))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	err = os.Remove(s.lockPath(id))
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	return nil
}

// fileJournal is the JSON-lines journal handle returned by FileStore.
type fileJournal struct {
	path       string
	dir        string
	f          *os.File
	lock       *fileLock
	lastSeq    uint64
	dirCreated bool
}

// Append implements runstate.Journal. The dup/gap decision is the shared
// runstate.CheckAppend contract; the write, the fsync, and the last-seq advance
// stay here because they are file-specific and ordering-load-bearing: lastSeq is
// advanced only after a successful Sync, so a torn or failed write re-appends the
// same seq on retry rather than losing it.
//
// The context is read once, before the record is marshaled. Past that point the write
// and its fsync run to completion, so a caller that cancels mid-append gets a record
// that landed rather than a half-written line.
func (j *fileJournal) Append(ctx context.Context, seq uint64, rec runstate.Record) error {
	err := ctx.Err()
	if err != nil {
		return err
	}

	skip, err := runstate.CheckAppend(j.lastSeq, seq)
	if err != nil {
		return err
	}
	if skip {
		return nil
	}
	rec.Seq = seq

	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshaling record: %w", err)
	}
	line = append(line, '\n')

	_, err = j.f.Write(line)
	if err != nil {
		return fmt.Errorf("writing record: %w", err)
	}
	err = j.f.Sync()
	if err != nil {
		return fmt.Errorf("syncing record: %w", err)
	}

	// On the first write of a newly created journal, fsync the directory so the
	// new file entry itself survives a crash.
	if j.dirCreated {
		err = syncDir(j.dir)
		if err != nil {
			return fmt.Errorf("syncing store directory: %w", err)
		}
		j.dirCreated = false
	}

	j.lastSeq = seq

	return nil
}

// Records implements runstate.Journal.
func (j *fileJournal) Records(ctx context.Context) ([]runstate.Record, error) {
	err := ctx.Err()
	if err != nil {
		return nil, err
	}

	return readRecords(j.path)
}

// LastSeq implements runstate.Journal.
func (j *fileJournal) LastSeq() uint64 {
	return j.lastSeq
}

// CheckHeld implements runstate.Journal. This backend holds an exclusive flock on
// the run for the journal's lifetime, so holding is structural: while this journal
// is open no other process opened it, and the answer needs no I/O. On a platform
// without flock the lock excludes nobody, and this reports held anyway rather than
// inventing a guarantee the platform does not offer. LocksRuns says which of the two
// this build does, so a caller can tell an enforced answer from an assumed one.
//
// The context is still read, so a caller stepping through work on a canceled context
// is stopped here rather than at the append after it.
func (j *fileJournal) CheckHeld(ctx context.Context) error {
	return ctx.Err()
}

// Close implements runstate.Journal.
func (j *fileJournal) Close() error {
	err := j.f.Close()
	j.lock.release()

	return err
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()

	return d.Sync()
}

// readRecords parses a JSON-lines journal, dropping an unparsable final line as a
// torn tail (only the last append can be torn on an append-only, fsynced file)
// while treating interior parse failures as corruption.
func readRecords(path string) ([]runstate.Record, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	lines := bytes.Split(data, []byte{'\n'})
	// A well-formed file ends in a newline, so the final split element is empty.
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}

	out := make([]runstate.Record, 0, len(lines))
	for i, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}

		var rec runstate.Record
		err := json.Unmarshal(line, &rec)
		if err != nil {
			if i == len(lines)-1 {
				// Torn tail from a crash mid-write: drop it and resume from the
				// last complete record.
				break
			}
			return nil, fmt.Errorf("%w: line %d: %w", runstate.ErrCorrupt, i+1, err)
		}
		out = append(out, rec)
	}

	return out, nil
}

func lastSeq(path string) (uint64, error) {
	recs, err := readRecords(path)
	if err != nil {
		return 0, err
	}
	if len(recs) == 0 {
		return 0, nil
	}

	return recs[len(recs)-1].Seq, nil
}
