//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/runstate"
)

// SessionFor is the journal a thread runs in, derived from the serving identity and
// the browser's thread id.
//
// The thread id is hashed rather than used as the key, so the only journals a page can
// reach are the ones this identity minted for its threads. A session id is not a secret:
// it is logged and it appears in a session listing, and handing the store a page's own
// bytes would let one page name another channel's journal. The identity is first so two
// agents serving the same page keep their conversations apart.
//
// It is exported for a caller that also holds the store and wants the journal a thread
// reached. It says nothing about a journal existing.
func SessionFor(identity, threadID string) string {
	sum := sha256.Sum256([]byte(identity + "\x00" + threadID))

	return "w-" + hex.EncodeToString(sum[:])
}

// held reports whether the store holds a conversation under sessionID, which is what
// separates a request that opens one from a request that continues it.
//
// The store answers rather than a map in memory. A follow-up mistaken for an opening
// turn resumes without FollowUp, which replaces the conversation with the journaled one
// and discards the prompt, so the person's message would vanish. A map would give that
// wrong answer after every restart and in every process but the one that opened the
// thread.
func (c *Channel) held(ctx context.Context, sessionID string) (bool, error) {
	_, err := c.sessions.Load(ctx, sessionID)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, runstate.ErrNotFound), errors.Is(err, runstate.ErrInvalidID):
		return false, nil
	default:
		return false, fmt.Errorf("reading the stored conversation: %w", err)
	}
}

// checkpointFor is how a request joins its thread's conversation, in one of three
// shapes.
//
// A thread the store does not hold creates the journal and the prompt is its first
// turn. A held thread carrying a prompt resumes with FollowUp, which requires ResumeID,
// refuses CreateIfMissing and needs the prompt; the request was refused before this
// when it carried none. A held thread carrying only an answer resumes with neither: the
// answer seeds the prompter and the resume dispatches the call the question guards
// again, so nothing goes on the checkpoint for it.
//
// Force is set on every resuming shape. An operator restarting with a different model
// moves the stored configuration under every open thread, and a resume across that is
// otherwise refused with ErrConfigDrift. The run still drops the standing approvals it
// can no longer vouch for.
func checkpointFor(sessionID string, held bool, prompt string) agent.Checkpoint {
	switch {
	case !held:
		return agent.Checkpoint{ResumeID: sessionID, CreateIfMissing: true}
	case prompt != "":
		return agent.Checkpoint{ResumeID: sessionID, FollowUp: true, Force: true}
	default:
		return agent.Checkpoint{ResumeID: sessionID, Force: true}
	}
}
