//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agenttest

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/choria-io/fisk-ai/internal/runstate"
)

// FakeSessionStore is an in-memory runstate.Store for tests: it keeps each run's
// journal in a map guarded by a mutex, so a checkpoint+resume pair can be handed one
// store through agent.Options.SessionStore without a file backend or a NATS
// connection. Because the store lives only in this instance, a resume finds its
// session only if the injected store was actually borrowed across both runs, which
// is what the shared-session example asserts. It is one of the separate-package
// fakes proving each injectable interface can be implemented from outside its own
// package, and it is safe for the concurrent use runs sharing one store make of it.
//
// It honors the cancellation contract on runstate.Store and runstate.Journal: every
// method that takes a context returns that context's error before it touches the map,
// so a test can cancel a caller and see the store refuse.
type FakeSessionStore struct {
	mu   sync.Mutex
	runs map[string]*fakeJournal
	info runstate.Info
	// created counts the runs this store has made, so each journal keeps the place it
	// was created in. A map has no order, and ListPage enumerates oldest first.
	created uint64
}

// FakeSessionStore implements runstate.Store and fakeJournal implements
// runstate.Journal; the assertions are the separate-package interface audit,
// failing to compile if either interface stops being implementable from outside its
// own package.
var (
	_ runstate.Store   = (*FakeSessionStore)(nil)
	_ runstate.Journal = (*fakeJournal)(nil)
)

// NewFakeSessionStore returns an empty in-memory session store.
func NewFakeSessionStore(tb testing.TB) *FakeSessionStore {
	tb.Helper()
	return BuildFakeSessionStore()
}

// BuildFakeSessionStore is NewFakeSessionStore without a testing.TB, for a func Example
// or any other caller outside a test.
func BuildFakeSessionStore() *FakeSessionStore {
	return &FakeSessionStore{runs: map[string]*fakeJournal{}, info: runstate.Info{Backend: "fake"}}
}

// SetInfo overrides what the store reports about its backend.
//
// It exists so a test can prove an injected store is asked what it is rather than the
// config being asked what was requested. Those two agree for every configured backend, so
// nothing else can tell them apart. The default backend name is not a registered one, so
// a test that does not care cannot be mistaken for a real backend.
func (s *FakeSessionStore) SetInfo(info runstate.Info) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.info = info
}

// Info implements runstate.Store.
func (s *FakeSessionStore) Info() runstate.Info {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.info
}

// Create implements runstate.Store.
func (s *FakeSessionStore) Create(ctx context.Context, id string, meta runstate.MetaRecord) (runstate.Journal, error) {
	err := ctx.Err()
	if err != nil {
		return nil, err
	}

	err = runstate.ValidateID(id)
	if err != nil {
		return nil, err
	}

	// meta is this store's copy, so stamping the version leaves the caller's struct
	// alone. A fake that skipped this would journal a run at version 0 and fail the
	// resume it exists to exercise.
	err = runstate.PrepareMeta(&meta)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.runs[id]; ok {
		return nil, fmt.Errorf("%w: %q", runstate.ErrExists, id)
	}

	s.created++
	j := &fakeJournal{held: true, created: s.created, createdAt: meta.Created}
	// The Meta record is seq 1 and frames the run, mirroring the file backend.
	err = j.append(1, runstate.Record{Seq: 1, Protocol: runstate.MetaProtocol, Meta: &meta})
	if err != nil {
		return nil, err
	}
	s.runs[id] = j

	return j, nil
}

// Open implements runstate.Store.
func (s *FakeSessionStore) Open(ctx context.Context, id string) (runstate.Journal, error) {
	err := ctx.Err()
	if err != nil {
		return nil, err
	}

	err = runstate.ValidateID(id)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	j, ok := s.runs[id]
	if !ok {
		return nil, fmt.Errorf("%w: %q", runstate.ErrNotFound, id)
	}
	if !j.acquire() {
		return nil, runstate.ErrLocked
	}

	return j, nil
}

// Evict makes the journal for id report that it no longer holds its run, and refuse
// further appends, as a journal on a shared store does once another worker has taken the
// run over. It is how a test reaches the take-over path with one writer.
//
// Evicting an id the store never created does nothing. A run whose journal was closed is
// still held here, so evicting one marks it taken over: a later Open succeeds and that
// journal's CheckHeld and Append then report ErrLocked.
func (s *FakeSessionStore) Evict(id string) {
	s.mu.Lock()
	j, ok := s.runs[id]
	s.mu.Unlock()

	if ok {
		j.Evict()
	}
}

// Load implements runstate.Store.
func (s *FakeSessionStore) Load(ctx context.Context, id string) (*runstate.RunState, error) {
	err := ctx.Err()
	if err != nil {
		return nil, err
	}

	err = runstate.ValidateID(id)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	j, ok := s.runs[id]
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q", runstate.ErrNotFound, id)
	}

	return runstate.Fold(j.snapshot())
}

// List implements runstate.Store. The filter is answered through
// runstate.ListFilter.MatchesID and MatchesAgent, the calls both real backends make, so a
// test written against this fake sees a run with no agent listed under any agent, and a
// run outside the prefix dropped before it is folded, as it would be in a file or
// JetStream store. The row's summary comes off the terminal record the same way, and stays
// nil for a conversation whose last turn wrote none.
func (s *FakeSessionStore) List(ctx context.Context, filter runstate.ListFilter) ([]runstate.RunInfo, error) {
	err := ctx.Err()
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]runstate.RunInfo, 0, len(s.runs))
	for id, j := range s.runs {
		if !filter.MatchesID(id) {
			continue
		}

		rs, err := runstate.Fold(j.snapshot())
		if err != nil {
			return nil, err
		}
		if !filter.MatchesAgent(rs.Agent) {
			continue
		}

		created, updated := j.stamps()
		out = append(out, runInfoFor(id, rs, created, updated))
	}

	return out, nil
}

// ListPage implements runstate.Store, enumerating the runs in the order Create made
// them, which is the creation order the real backends read off a journal's meta record
// or a record's stream sequence. The cursor is that place in the order, so a caller that
// parsed one from a file or JetStream store is refused here as it would be there.
//
// A row is built by the same call List builds one with, so an embedder paging this fake
// sees the rows a full listing would have handed it.
func (s *FakeSessionStore) ListPage(ctx context.Context, filter runstate.ListFilter, limit int, cursor string) (runstate.RunPage, error) {
	err := ctx.Err()
	if err != nil {
		return runstate.RunPage{}, err
	}

	err = runstate.CheckPageLimit(limit)
	if err != nil {
		return runstate.RunPage{}, err
	}

	after, err := parseCursor(cursor)
	if err != nil {
		return runstate.RunPage{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	ids := make([]string, 0, len(s.runs))
	for id, j := range s.runs {
		if j.created <= after {
			continue
		}
		// The prefix goes here rather than beside the agent below, since both real
		// backends answer it from the id they enumerate on and never fold the run.
		if !filter.MatchesID(id) {
			continue
		}
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b string) int {
		return cmp.Compare(s.runs[a].created, s.runs[b].created)
	})

	var runs []runstate.RunInfo
	last := after
	for _, id := range ids {
		j := s.runs[id]
		last = j.created

		rs, err := runstate.Fold(j.snapshot())
		if err != nil {
			return runstate.RunPage{}, err
		}
		if !filter.MatchesAgent(rs.Agent) {
			continue
		}

		created, updated := j.stamps()
		runs = append(runs, runInfoFor(id, rs, created, updated))
		if len(runs) == limit {
			break
		}
	}

	page := runstate.RunPage{Runs: runs}
	if len(runs) == limit {
		page.Cursor = cursorPrefix + strconv.FormatUint(last, 10)
	}

	return page, nil
}

// runInfoFor builds one listing row, so List and ListPage cannot describe the same run
// differently. The ending and the summary come off the terminal record the way both real
// backends read them, and stay absent for a conversation whose last turn wrote none.
//
// It fills every field a real backend fills. created is the time the caller stamped on
// the meta record, as it is in both real stores, and updated is when this journal last
// took a record, which is the file store's modification time and the JetStream store's
// stored time. A rail built against this fake would otherwise read year-one dates and no
// model from rows a real store names.
func runInfoFor(id string, rs *runstate.RunState, created, updated time.Time) runstate.RunInfo {
	info := runstate.RunInfo{
		RunID:   id,
		Created: created,
		Updated: updated,
		Model:   rs.Fingerprint.Model,
		Prompt:  rs.Prompt,
		Agent:   rs.Agent,
	}
	if rs.Terminal != nil {
		info.Terminal = rs.Terminal.Reason
		info.Summary = rs.Terminal.Summary
	}

	return info
}

// cursorPrefix marks a cursor as this store's own. The JetStream store's cursor is a
// stream sequence written as decimal, so a bare number here would parse and answer a page
// from a place in this store's creation order that the sequence never meant.
const cursorPrefix = "fake-"

// parseCursor reads a cursor back into the place in the creation order a page resumes
// after. An empty cursor starts at the oldest run the store holds.
func parseCursor(cursor string) (uint64, error) {
	invalid := func() error {
		return fmt.Errorf("%w: %q is not a position this store minted", runstate.ErrInvalidCursor, cursor)
	}

	if cursor == "" {
		return 0, nil
	}

	rest, ok := strings.CutPrefix(cursor, cursorPrefix)
	if !ok {
		return 0, invalid()
	}

	after, err := strconv.ParseUint(rest, 10, 64)
	if err != nil {
		return 0, invalid()
	}

	return after, nil
}

// Delete implements runstate.Store.
func (s *FakeSessionStore) Delete(ctx context.Context, id string) error {
	err := ctx.Err()
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.runs, id)

	return nil
}

// fakeJournal is one run's in-memory append-only record log. held models the file
// backend's per-run lock so a second Open of a still-open run fails with ErrLocked.
type fakeJournal struct {
	mu     sync.Mutex
	held   bool
	stolen bool
	// created is the place this run holds in the order the store made its runs, which
	// is what ListPage enumerates on and what its cursor carries.
	created uint64
	// createdAt is the time the caller stamped on the meta record and updatedAt is when
	// this journal last took a record. A listing row carries both, as it does from a
	// file or JetStream store.
	createdAt time.Time
	updatedAt time.Time
	records   []runstate.Record
	lastSeq   uint64
}

// stamps reports when this run was created and when it last took a record, for a
// listing row.
func (j *fakeJournal) stamps() (time.Time, time.Time) {
	j.mu.Lock()
	defer j.mu.Unlock()

	return j.createdAt, j.updatedAt
}

// acquire takes the lock, reporting false when it is already held.
func (j *fakeJournal) acquire() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.held {
		return false
	}
	j.held = true

	return true
}

// Append implements runstate.Journal. An evicted journal is refused here as well as in
// CheckHeld, since a store that let a taken-over writer keep appending would model no
// real backend.
func (j *fakeJournal) Append(ctx context.Context, seq uint64, rec runstate.Record) error {
	err := ctx.Err()
	if err != nil {
		return err
	}

	j.mu.Lock()
	defer j.mu.Unlock()

	if j.stolen {
		return fmt.Errorf("%w: the run was taken over", runstate.ErrLocked)
	}

	return j.append(seq, rec)
}

// append is the unlocked core, also called from Create while the journal is not yet
// published and so cannot be contended. It uses the same seq accounting as the file
// backend: runstate.CheckAppend folds a duplicate and rejects a gap, and the record's
// Seq is stamped from the seq argument so a fold sees a strictly increasing sequence.
func (j *fakeJournal) append(seq uint64, rec runstate.Record) error {
	skip, err := runstate.CheckAppend(j.lastSeq, seq)
	if err != nil {
		return err
	}
	if skip {
		return nil
	}
	rec.Seq = seq
	j.records = append(j.records, rec)
	j.lastSeq = seq
	j.updatedAt = time.Now()

	return nil
}

// Records implements runstate.Journal.
func (j *fakeJournal) Records(ctx context.Context) ([]runstate.Record, error) {
	err := ctx.Err()
	if err != nil {
		return nil, err
	}

	return j.snapshot(), nil
}

// LastSeq implements runstate.Journal.
func (j *fakeJournal) LastSeq() uint64 {
	j.mu.Lock()
	defer j.mu.Unlock()

	return j.lastSeq
}

// CheckHeld implements runstate.Journal. The fake excludes a second opener the way the
// file backend does, so an open journal holds its run unless a test has taken it away
// with Evict.
func (j *fakeJournal) CheckHeld(ctx context.Context) error {
	err := ctx.Err()
	if err != nil {
		return err
	}

	j.mu.Lock()
	defer j.mu.Unlock()

	if j.stolen {
		return fmt.Errorf("%w: the run was taken over", runstate.ErrLocked)
	}

	return nil
}

// Evict marks the run as taken over by somebody else, so this journal refuses to check
// held and refuses to append.
//
// It models the shared-store case rather than the locking one: a journal whose store
// cannot exclude a second opener discovers it lost the run only when it consults the
// store, so opening still succeeds and writing does not. That is what a test needs to
// reach, and no arrangement of the fake's own lock produces it.
func (j *fakeJournal) Evict() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.stolen = true
}

// Close implements runstate.Journal, releasing the per-run lock.
func (j *fakeJournal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.held = false

	return nil
}

// snapshot returns a copy of the records under lock, for Load and Records.
func (j *fakeJournal) snapshot() []runstate.Record {
	j.mu.Lock()
	defer j.mu.Unlock()

	out := make([]runstate.Record, len(j.records))
	copy(out, j.records)

	return out
}
