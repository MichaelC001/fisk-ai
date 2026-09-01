//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package runstate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Backend names for the session store, selected by the session config backend.
const (
	// BackendFile stores each run as a JSON-lines journal under a directory. It is
	// the name the file subpackage registers under and the config default.
	BackendFile = "file"
	// BackendJetStream stores each run as messages on a pre-existing JetStream stream,
	// keyed by subject (<prefix>.<run>.<seq>) with MaxMsgsPerSubject=1. It is the name
	// the jetstream subpackage registers under.
	BackendJetStream = "jetstream"
)

var (
	// ErrNotFound is returned when a run id has no stored journal.
	ErrNotFound = errors.New("run not found")
	// ErrExists is returned by Create when a journal for the id already exists.
	ErrExists = errors.New("run already exists")
	// ErrInvalidID is returned when a run id is not a valid KSUID; it must be
	// validated before it is ever used as a path component.
	ErrInvalidID = errors.New("invalid run id")
	// ErrLocked is returned when the run's lock is already held. The flock is per open
	// file description and each acquire opens its own, so it is held either by another
	// process or by another run of the same id in this process (a server running many
	// runs at once); the message names neither, since a caller cannot tell which.
	ErrLocked = errors.New("run is already locked (by another process or a concurrent run of the same id)")
	// ErrSeqGap is returned by Append when a seq skips ahead of the journal.
	ErrSeqGap = errors.New("record seq gap")
)

// RunInfo is a summary of a stored run, for listing.
type RunInfo struct {
	RunID   string
	Created time.Time
	Updated time.Time
	Model   string
	Prompt  string
	// Terminal is the reason the run ended, or empty if it is still open (was
	// suspended or crashed).
	Terminal TerminalReason
}

// Journal is an append-only record log for a single run. Append is idempotent on
// a duplicate seq so a crash-retry of the most recent event is a no-op. A Journal
// is not safe for concurrent use; the Store guards each run with a lock.
//
// Cancellation contract. Every method that can block on I/O takes a context, and a
// backend that cannot reach its storage in time fails that call. A failed call leaves
// the journal open and its lock held, so a caller may retry with a fresh context.
//
// No backend writes a record in pieces, so a canceled Append cannot leave a torn record
// behind. It can leave a complete one. The
// JetStream backend publishes a record and waits for the acknowledgement, and a context
// that expires between those two returns an error over a record that is already stored;
// appending the same seq again adopts that record rather than writing a second one, and
// LastSeq still reports the seq before it. The file backend writes and fsyncs the line
// without consulting the context again, so an Append it starts either lands or fails on
// its own terms.
type Journal interface {
	// Append writes rec at seq. A seq equal to or below the last written seq is
	// treated as an already-recorded duplicate and ignored; a seq more than one
	// beyond the last is an ErrSeqGap.
	Append(ctx context.Context, seq uint64, rec Record) error
	// Records returns every record in order, dropping an unparsable final line
	// (a torn tail from a crash mid-write) but erroring on interior corruption.
	Records(ctx context.Context) ([]Record, error)
	// LastSeq returns the highest seq written, so a resuming writer continues the
	// sequence rather than colliding with existing records.
	//
	// It takes no context because it reads what this journal has durably stored,
	// which every backend advances in memory after an acknowledged write.
	LastSeq() uint64
	// CheckHeld reports whether this journal still holds its run, returning
	// ErrLocked when another writer has taken it over.
	//
	// It exists so a holder can find out before doing something it cannot undo,
	// rather than at its next append, which is after. A caller running work with
	// effects outside this process should call it immediately before each such step
	// on a store it does not exclusively own.
	//
	// It is a point-in-time answer and a backend may have to ask the network for it.
	// A nil return means no other writer had taken the run as of this call, never
	// that none can before the next one. An error that is not ErrLocked means the
	// question could not be answered, which is not evidence that the run was lost.
	CheckHeld(ctx context.Context) error
	// Close releases the journal and its lock.
	//
	// It takes no context: a release a caller could cancel would strand the lock and
	// whatever handle the backend holds, so Close runs to completion.
	Close() error
}

// Info describes the session store a run bound. It describes the store, not a run;
// RunInfo is the per-run summary List returns.
//
// It exists because the configuration says what was asked for while an injected store
// says what ran, and only the store itself knows which.
//
// Its fields are intended for telemetry and for display to an operator, which is what
// bounds what may go in one: a value here may leave the process and cannot be un-sent.
type Info struct {
	// Backend is the registered backend name, in the registry's own vocabulary, so it
	// stays correct however many backends are added. It is never empty.
	Backend string

	// Location names the container this store is bound to, in whatever term the backend
	// uses: a JetStream stream today, a table or an index later. It must be an
	// operator-configured identifier. Never a filesystem path, never a URL carrying
	// userinfo, never a credential. A backend with nothing safe to name returns "".
	Location string
}

// Store persists run journals. The interface is append-oriented and seq-keyed so
// that both backends can honor it: the file implementation appends to a JSON-lines
// journal, and the jetstream implementation puts each record on its own subject
// (<prefix>.<run>.<seq>, MaxMsgsPerSubject=1 with discard-new-per-subject for an
// unbounded dedup window).
//
// Cancellation contract. Every method that reaches storage takes a context, and the
// caller's deadline governs the call: a backend applies a timeout of its own only when
// the context carries no deadline. Load, List and Delete are all-or-nothing. A canceled
// Load or List returns no partial result, and a canceled Delete has either removed the
// whole run or not started.
//
// Create is not. It takes the id before it writes the meta record, so a cancellation
// between those two returns an error and no journal over an id that is now taken: a
// second Create of it returns ErrExists. What is behind that id differs by backend. The
// file backend has created an empty journal and its lock file, which Load reports as
// ErrEmpty and Open hands back at seq 0, so a caller resuming it writes the meta record
// itself or calls Delete and starts again. The JetStream backend has either stored the
// meta record, in which case the run reads and resumes like any other, or stored nothing,
// in which case the id is free and Create succeeds. A caller that cannot tell which it
// got calls Load.
//
// A canceled Open returns no journal and takes no lock, so nothing has to be released.
type Store interface {
	// Info describes the active store.
	//
	// It reports what the store resolved when it was built. It must not block, perform
	// I/O, or fail, and it must be safe to call from any goroutine, which is why it
	// takes no context.
	Info() Info
	// Create starts a new run, writing meta as seq 1, and returns the locked
	// journal. It fails with ErrExists if the id is already present.
	//
	// It stamps meta.Version, so a caller leaves that field zero; a meta record
	// carrying a version this build does not write fails with ErrVersion. An
	// implementation gets both from PrepareMeta, which it calls before the append.
	Create(ctx context.Context, id string, meta MetaRecord) (Journal, error)
	// Open locks an existing run's journal for appending (resume). It fails with
	// ErrNotFound if the id is unknown.
	Open(ctx context.Context, id string) (Journal, error)
	// Load reads and folds a run without locking it, for inspection and listing.
	Load(ctx context.Context, id string) (*RunState, error)
	// List summarizes all stored runs.
	List(ctx context.Context) ([]RunInfo, error)
	// Delete removes a run's journal and lock.
	Delete(ctx context.Context, id string) error
}

// New builds the session store for the named backend, handing the factory the
// per-run environment and the raw per-backend options block. env carries what a
// backend needs beyond its options: the file backend roots journals under
// env.StoreDir/runs when set (empty keeps the XDG default), and a connection-backed
// backend borrows env.Nats. It returns an error for an unknown backend or malformed
// backend options, so an operator's mistake surfaces at run start rather than on the
// first operation. An unknown backend most often means the backend package was not
// imported into this build; the error lists the backends that are linked in.
func New(backend string, options json.RawMessage, env RuntimeEnv) (Store, error) {
	reg, ok := lookup(backend)
	if !ok {
		return nil, fmt.Errorf("unknown session backend %q: known backends are %v", backend, Backends())
	}

	return reg.factory(env, options)
}

// DefaultDir returns the default run store directory, honoring XDG_STATE_HOME and
// falling back to ~/.local/state. Runs are never stored in the working directory,
// where they would leak into repositories, and they are never namespaced by
// identity, so a resume finds its run regardless of the active identity. It lives
// in the core, not the file backend, so the never-in-CWD contract stays visible to
// every backend that resolves a default location.
func DefaultDir() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolving home directory: %w", err)
		}
		base = filepath.Join(home, ".local", "state")
	}

	return filepath.Join(base, "fisk-ai", "runs"), nil
}
