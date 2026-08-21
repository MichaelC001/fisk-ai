//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package slack

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/runstate"
)

// SessionFor is the journal a thread runs in, derived from the serving identity and the
// three identifiers Slack minted for the thread rather than taken from anything a person
// wrote.
//
// A thread creates a journal or resumes the one an earlier turn of that same thread made,
// and nothing else. Handing the store bytes a caller chose would let one person name
// another's journal, and a journal id is not a secret: it is logged, and a deferred run's
// terminal message carries it.
//
// All three Slack identifiers are hashed. The thread timestamp alone is unique only
// within a channel, so a worker answering in two workspaces, or in two channels that
// happened to mint the same timestamp, would otherwise merge their conversations. The
// identity is first so that two agents sharing a workspace keep theirs apart.
//
// threadTS is the thread's own root: the mention's thread_ts when it arrived inside a
// thread, and the mention's own ts when it started one. Passing an empty threadTS is a
// caller's mistake this cannot detect, and it would derive one journal for every
// top-level mention in a channel, so mentionOf resolves it rather than leaving it to a
// call site.
//
// It is exported for a caller that also holds the store and wants the journal a thread
// reached. It says nothing about a journal existing.
func SessionFor(identity, teamID, channelID, threadTS string) string {
	sum := sha256.Sum256([]byte(identity + "\x00" + teamID + "\x00" + channelID + "\x00" + threadTS))

	return "s-" + hex.EncodeToString(sum[:])
}

// held reports whether this worker already holds a conversation under sessionID, which is
// what separates a turn that opens one from a turn that continues it.
//
// The store answers rather than a map in memory, and it has to: a follow-up mistaken for
// an opening turn resumes without FollowUp, which replaces the conversation with the
// journaled one and discards the prompt, so the person's message would vanish. A map
// would give that wrong answer after every restart.
func (c *Channel) held(sessionID string) (bool, error) {
	_, err := c.sessions.Load(sessionID)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, runstate.ErrNotFound), errors.Is(err, runstate.ErrInvalidID):
		return false, nil
	default:
		return false, fmt.Errorf("reading the stored conversation: %w", err)
	}
}

// checkpointFor is how a turn joins its thread's conversation.
//
// There are three cases and two shapes. A mention in a thread nobody has answered yet
// creates the journal and its own text is the first prompt, whether the mention opened
// the thread or arrived in one people had been talking in for an hour. A mention in a
// thread this worker holds adds a turn to what is stored.
//
// FollowUp and CreateIfMissing cannot both be set, which agent.Run refuses by name: a
// caller that may deliver the same work twice would append its prompt on every
// redelivery. That is why held above asks the store instead of assuming, and why the
// intake deduplicates before it gets here.
//
// Force belongs to the resuming shape and to no other. A thread's next turn may arrive
// days after the last one and across a deploy, and a resume is otherwise refused whenever
// the model, the system prompt, the thinking mode or the reasoning effort has moved since
// the journal was written. Refusing there would kill every open thread in the workspace on
// one configuration edit. The run drops the standing approvals it can no longer vouch for,
// which is the part of that refusal worth keeping.
func checkpointFor(sessionID string, held bool) agent.Checkpoint {
	if held {
		return agent.Checkpoint{ResumeID: sessionID, FollowUp: true, Force: true}
	}

	return agent.Checkpoint{ResumeID: sessionID, CreateIfMissing: true}
}
