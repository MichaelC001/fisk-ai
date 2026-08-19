//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package serve

import (
	"log/slog"
	"time"

	"github.com/choria-io/fisk-ai/internal/runstate"
)

// loggedStore reports what reaching a conversation costs, wrapping the session store
// every run on this server shares.
//
// Nothing is held between turns: a worker keeps no conversation in memory, so each turn
// reads the journal back from the store and folds it, and any worker of the identity can
// serve any turn. The run log named the session a turn resumed without saying that
// reaching it crossed the network, or what that took, which is the difference between a
// slow model and a slow store.
//
// What is reported is the pair a conversation is held between. Load reads and folds the
// records and Open takes the journal for appending, both of which a resume performs;
// Create is the same acquisition for a conversation that did not exist yet. Closing the
// journal is the release: the run is over, nothing here holds the conversation any more,
// and the next turn reads it back from the store. Appends are not reported, being one
// per record on a path that already logs the run.
type loggedStore struct {
	runstate.Store

	log     *slog.Logger
	backend string
}

// withStoreLogging wraps store so its reads are reported to log. A nil logger returns
// the store unchanged, so a caller that wants no output pays nothing for the decision.
func withStoreLogging(store runstate.Store, log *slog.Logger) runstate.Store {
	if log == nil {
		return store
	}

	return &loggedStore{Store: store, log: log, backend: store.Info().Backend}
}

// Load implements runstate.Store, reporting the read and the fold behind it.
//
// The message count is the fold's own size rather than the store's: it is what the next
// model call re-sends, so a conversation whose reads are getting slower says so here
// before it says it in the bill.
func (s *loggedStore) Load(id string) (*runstate.RunState, error) {
	started := time.Now()

	rs, err := s.Store.Load(id)
	if err != nil {
		s.log.Warn("Reading a conversation failed", "session", id, "backend", s.backend, "duration", time.Since(started), "error", err)

		return nil, err
	}

	s.log.Info("Read a conversation from the store", "session", id, "backend", s.backend, "duration", time.Since(started), "messages", len(rs.Messages))

	return rs, nil
}

// Open implements runstate.Store, reporting the journal lock a resume takes before
// anything runs.
func (s *loggedStore) Open(id string) (runstate.Journal, error) {
	started := time.Now()

	j, err := s.Store.Open(id)
	if err != nil {
		s.log.Warn("Opening a conversation journal failed", "session", id, "backend", s.backend, "duration", time.Since(started), "error", err)

		return nil, err
	}

	s.log.Info("Opened a conversation journal", "session", id, "backend", s.backend, "duration", time.Since(started))

	return s.hold(id, j), nil
}

// Create implements runstate.Store, reporting the journal a conversation starts on.
//
// It is reported so that every hold this logs has the release below to close it. A
// conversation created here is held for its first turn exactly as a resumed one is.
func (s *loggedStore) Create(id string, meta runstate.MetaRecord) (runstate.Journal, error) {
	started := time.Now()

	j, err := s.Store.Create(id, meta)
	if err != nil {
		s.log.Warn("Creating a conversation journal failed", "session", id, "backend", s.backend, "duration", time.Since(started), "error", err)

		return nil, err
	}

	s.log.Info("Created a conversation journal", "session", id, "backend", s.backend, "duration", time.Since(started))

	return s.hold(id, j), nil
}

// hold wraps a journal so that releasing it is reported against the run that took it.
func (s *loggedStore) hold(id string, j runstate.Journal) runstate.Journal {
	return &loggedJournal{Journal: j, store: s, id: id, taken: time.Now()}
}

// loggedJournal reports the release of a conversation this process was holding.
//
// A journal is what a run holds a conversation by: while it is open the folded
// conversation is in memory and the lock is this worker's, and closing it gives both up.
// So the close is the point after which the next turn reads the store again, wherever it
// is served, and how long it was held is how long this worker had the conversation to
// itself.
type loggedJournal struct {
	runstate.Journal

	store *loggedStore
	id    string
	taken time.Time
}

// Close implements runstate.Journal, reporting the release.
func (j *loggedJournal) Close() error {
	err := j.Journal.Close()
	if err != nil {
		j.store.log.Warn("Releasing a conversation failed", "session", j.id, "backend", j.store.backend, "held", time.Since(j.taken), "error", err)

		return err
	}

	j.store.log.Info("Released a conversation, the next turn reads it back", "session", j.id, "backend", j.store.backend, "held", time.Since(j.taken))

	return nil
}
