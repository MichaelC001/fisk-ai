//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package serve

import (
	"errors"

	wire "github.com/choria-io/fisk-ai/internal/a2a/wire/v1"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
)

// ErrorCode is the wire code for an outcome whose class decides what a caller does
// next, and the empty string for one this does not place, where the caller supplies its
// own code.
//
// The classes are the model provider refusing the call and the journal being held by
// another writer. A run reports them through Outcome.Err with the sentinel wrapped, so
// every channel reporting an outcome has the same error to read.
//
// It takes the whole outcome rather than the error because one of the classes depends
// on how far the run got. It lives here rather than in a channel because more than one
// channel answers with a wire code: a2aendpoint sends one on a terminal message and
// asyncjobs stores one on the task record. Two of these, one per channel, would have to
// agree on which class wins, and somebody adding a class to one would leave the other
// answering the same failure differently.
func ErrorCode(out Outcome) string {
	switch {
	case errors.Is(out.Err, llm.ErrRateLimited), errors.Is(out.Err, llm.ErrOverloaded):
		return wire.CodeProviderBusy

	case errors.Is(out.Err, llm.ErrAuthentication), errors.Is(out.Err, llm.ErrModelNotFound):
		return wire.CodeProviderRefused

	case errors.Is(out.Err, llm.ErrContextLengthExceeded):
		return wire.CodeContextExceeded

	// Only where the run reached no terminal outcome, which is the claim it takes
	// before anything runs. A lock the run meets later has already executed part of the
	// turn, and CodeConversationBusy tells a caller its work never started: an
	// interactive client reads that code and sends a held approve reply again, which
	// would run a gated command a second time.
	case out.Reason == "" && errors.Is(out.Err, runstate.ErrLocked):
		return wire.CodeConversationBusy
	}

	return ""
}
