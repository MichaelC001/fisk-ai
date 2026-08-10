//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// Package tasks stores the record of a unit of work: what was asked, and what came
// back.
//
// It exists because an asynchronous caller submits work and returns later for the
// answer, with no connection, no shared process and possibly no shared machine in
// between. What it can rely on is what was written down, and this is that record.
//
// It is deliberately not the run journal. internal/runstate holds how the work was
// done, which is this software's private working state: the conversation, the tool
// calls, the fingerprint and the resume position. A caller is never sent there for an
// answer, both because it is not an interface anyone should depend on and because it
// would make every internal change a breaking one. A Task record is the contract; the
// journal behind it is not.
//
// A record holds the a2a messages themselves rather than a translation of them, so a
// stored task is self-describing, validates through the same schemas a message on a
// wire does, and does not depend on this version of this program to be read.
package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/choria-io/fisk-ai/internal/a2a"
)

var (
	// ErrExists is returned by Submit when a record already exists for the request's
	// id, so a resubmission is reported rather than silently written twice.
	ErrExists = errors.New("task already exists")

	// ErrNotFound is returned by Load and Complete for an id with no record.
	ErrNotFound = errors.New("task not found")

	// ErrCompleted is returned by Complete when the record already carries an
	// answer.
	ErrCompleted = errors.New("task is already completed")

	// ErrInvalidID is returned for an id that is not a safe, bounded single path
	// component.
	ErrInvalidID = errors.New("invalid task id")

	// ErrInvalidRequest is returned by Submit when the body is not a usable request
	// message.
	ErrInvalidRequest = errors.New("invalid task request")

	// ErrUnknownBackend is returned when no backend is registered under a name.
	ErrUnknownBackend = errors.New("unknown task store backend")
)

// State is what the store knows about a record.
//
// It reports only what a store can observe: a record exists, and later it has an
// answer. Whether an existing task is queued, claimed, running or being retried is
// the queue's to report, since only the queue knows about claims, attempts and
// leases, and duplicating that here would give two answers to one question.
type State string

const (
	// StateSubmitted is a record with no answer yet.
	StateSubmitted State = "submitted"
	// StateCompleted is a record whose answer has been written.
	StateCompleted State = "completed"
)

// Task is a stored unit of work: what was asked, and what came back.
type Task struct {
	// ID is the request's own correlation id, which names this task everywhere: the
	// queue item, this record, the trace, and the session its run journals under.
	ID string `json:"id"`

	// State is what the store knows, which is whether an answer has been written.
	State State `json:"state"`

	// Request is the io.choria.fisk-ai.v1.request message as submitted.
	//
	// It is the same message, not the same bytes. Nesting it in this record
	// re-encodes it: insignificant whitespace is dropped and <, > and & become
	// escapes. Property order, meaning, schema validity and decoding are unaffected.
	// A digest or signature over the submitted body is therefore not something this
	// format can support.
	Request json.RawMessage `json:"request"`

	// Submitted is when this record was written. The submitter's own claim about who
	// it is and when it asked is inside the request, on the message header.
	Submitted time.Time `json:"submitted"`

	// Result is the io.choria.fisk-ai.v1.result message, or an
	// io.choria.fisk-ai.v1.error where there is no answer to give. It is absent
	// until the task completes.
	Result json.RawMessage `json:"result,omitempty"`

	// Completed is when the answer was written, zero while there is none.
	Completed time.Time `json:"completed,omitzero"`
}

// Info describes the store a caller is bound to, for telemetry and for display to an
// operator. That is what bounds what may go in one: a value here may leave the
// process and cannot be un-sent.
type Info struct {
	// Backend is the registered backend name, in the registry's own vocabulary. It is
	// never empty.
	Backend string

	// Location names the container this store is bound to, in whatever term the
	// backend uses. It must be an operator-configured identifier: never a filesystem
	// path, never a URL carrying userinfo, never a credential. A backend with nothing
	// safe to name returns "".
	Location string
}

// Store holds task records.
//
// An implementation must be safe for concurrent use by independent processes sharing
// one backing store. That is not a nicety here: submitting and executing are
// different processes by definition, and a queue may deliver one task to two workers.
type Store interface {
	// Info describes this store. It must not block, perform I/O, or fail, and must be
	// safe to call from any goroutine.
	Info() Info

	// Submit writes a new record, taking its id from the request's own header, and
	// returns the record as stored.
	//
	// It returns ErrInvalidRequest when the body is not a request message carrying a
	// usable id, and ErrExists when a record for that id is already present.
	Submit(ctx context.Context, request json.RawMessage) (*Task, error)

	// Load returns a record by id, or ErrNotFound.
	Load(ctx context.Context, id string) (*Task, error)

	// Complete attaches an answer and moves the record to StateCompleted.
	//
	// It returns ErrNotFound for an unknown id and ErrCompleted when an answer is
	// already present. Refusing the second write is deliberate: with at-least-once
	// delivery two workers can run one task, and the loser is usually the one that
	// failed, either ejected by the journal's fence or refused a resume because the
	// session had already completed. Last write wins would let such a failure replace
	// an answer that succeeded.
	Complete(ctx context.Context, id string, result json.RawMessage) error
}

// idPattern constrains a task id to a safe, single path component. It is also a
// valid NATS subject token, so the same ids carry to a stream backend. It matches
// the run-id rule in runstate, since a task id becomes a session id.
var idPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// maxIDLen caps a task id so it stays a safe filename and subject token. The charset
// is ASCII, so runes equal bytes.
const maxIDLen = 128

// ValidateID rejects an id that is not a safe, bounded single path component. Every
// backend calls it before an id is used as a key or a path component, so it is a
// path-traversal defense as well as a format rule, and the format cannot drift
// between backends.
func ValidateID(id string) error {
	if len(id) > maxIDLen || !idPattern.MatchString(id) {
		return fmt.Errorf("%w: %q (use letters, digits, '-' or '_')", ErrInvalidID, id)
	}

	return nil
}

// RequestID reads the id a request message carries, which is the id its task is
// stored under.
//
// A request's header sets both id and request to the same value, and the submitter
// has already chosen it, so taking the id from the message rather than minting one is
// what keeps a single identifier threading the request, the record, the trace and the
// session. Minting here would leave the record carrying a second id beside the one
// the message already had.
//
// It checks that the body is a request message with a usable id, which is what the
// store needs to key a record. It is not a schema check: validating a message against
// the v1 schemas belongs to the surface that accepts it from a caller, which is where
// a rejection can still be reported to whoever sent it.
func RequestID(request json.RawMessage) (string, error) {
	var hdr struct {
		Protocol string `json:"protocol"`
		ID       string `json:"id"`
	}

	err := json.Unmarshal(request, &hdr)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}

	if hdr.Protocol != a2a.RequestProtocol {
		return "", fmt.Errorf("%w: protocol is %q, want %q", ErrInvalidRequest, hdr.Protocol, a2a.RequestProtocol)
	}

	err = ValidateID(hdr.ID)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}

	return hdr.ID, nil
}
