//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package toolkit

import (
	"errors"
	"fmt"
)

// maxDeferralNoteRunes caps the note a deferring tool supplies. It is displayed to
// an operator reading a suspended run and travels in the journal, so it is bounded
// like every other tool-supplied display text.
const maxDeferralNoteRunes = 500

// maxDeferralHandleRunes caps the handle a deferring tool supplies. A handle is an
// external system's identifier, so it is short by nature; the cap exists to stop a
// tool putting a payload where an identifier belongs.
const maxDeferralHandleRunes = 200

// truncateRunes caps s at max runes. Length is bounded here because a deferral is
// built in this package; escape stripping is not, because util.SanitizeForTerminal
// lives in a package that imports this one. Every site that renders a deferral to a
// terminal sanitizes it there, as it must for anything read back from a journal.
func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}

	return string(runes[:max])
}

// ErrDeferredResult reports that a tool will answer later rather than now. It is the
// one error a tool may return that is not a harness failure: the run ends at a
// resumable boundary, releasing its goroutine and its worker, and the result is
// supplied to the journal whenever it arrives.
//
// Test for it with errors.Is, and reach the tool's own description of what it is
// waiting on with errors.As against *DeferredResult.
var ErrDeferredResult = errors.New("the tool will answer later")

// DeferredResult is what a tool returns when the work it started will finish later.
// It carries what an operator needs to see the call through by hand; the tool_use id
// the call answers is the correlation key and is supplied by the caller, never by the
// tool.
//
// A deferred call is never re-run. That is the whole point of it: the tool already
// filed the ticket or already put the question, so a resume that called it again
// would do the thing twice.
type DeferredResult struct {
	// Note describes what the call is waiting on, in one line, for an operator
	// reading a suspended run. It is sanitized and capped for display.
	Note string

	// Handle is the tool's own identifier for the outstanding work, so a person
	// reading a suspended run can see which ticket, request or message it belongs
	// to. It may be empty, it is never interpreted, and nothing resolves one back to
	// a session.
	Handle string
}

// Error describes the deferral for a log line, naming the handle when there is one.
func (d *DeferredResult) Error() string {
	if d.Handle == "" {
		return fmt.Sprintf("%s: %s", ErrDeferredResult.Error(), d.Note)
	}

	return fmt.Sprintf("%s: %s (%s)", ErrDeferredResult.Error(), d.Note, d.Handle)
}

// Unwrap reports ErrDeferredResult, so every caller tests one sentinel however the
// deferral was built.
func (d *DeferredResult) Unwrap() error { return ErrDeferredResult }

// DeferResult builds the error a tool returns when it cannot answer now. note says
// what the call is waiting on and handle names it in whatever system is doing the
// waiting; handle may be empty. Both are capped, since they are tool-supplied text
// that is journaled and later shown to an operator.
//
// It is named DeferResult rather than Defer because deferral already means hiding a
// tool behind tool search in this package (see Tool.Definition and
// llm.ToolDef.DeferLoading), and the two must not read as one thing.
func DeferResult(note, handle string) error {
	return &DeferredResult{
		Note:   truncateRunes(note, maxDeferralNoteRunes),
		Handle: truncateRunes(handle, maxDeferralHandleRunes),
	}
}

// ServedDeferralRefusal is what a serving surface answers a caller whose tool call
// deferred. A served call has no journal, no session and a peer waiting on a reply,
// so there is nowhere for a later answer to land and no resume that would collect
// it.
//
// It is a fixed message rather than the deferral's own text because the caller is
// being told its call cannot be completed here, which is a fact about the surface;
// what the tool is waiting on would read as though the answer were coming.
const ServedDeferralRefusal = "this tool defers its answer and cannot be called on a served surface: there is no session to resume and no way to deliver a later result"

// IsDeferred reports whether err is a tool deferring its answer, and returns the
// deferral when it is. It exists so a caller that must decide what to do about a
// deferral does not have to know that the detail travels as an error.
func IsDeferred(err error) (*DeferredResult, bool) {
	var d *DeferredResult
	if errors.As(err, &d) {
		return d, true
	}

	// A caller may have built the sentinel without the detail; report the deferral
	// with an empty description rather than missing it.
	if errors.Is(err, ErrDeferredResult) {
		return &DeferredResult{}, true
	}

	return nil, false
}
