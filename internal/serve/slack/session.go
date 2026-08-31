//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package slack

import (
	"context"
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
// thread, and the mention's own ts when it started one. mentionOf resolves it, since an
// empty one derives a single journal for every top-level mention in a channel and nothing
// here can tell that from a thread.
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

// checkpointFor is how a turn joins its thread's conversation. A mention in a thread
// nobody has answered yet creates the journal and its own text is the first prompt,
// whether the mention opened the thread or arrived in one people had been talking in for
// an hour. A mention in a thread this worker holds adds a turn to what is stored.
//
// FollowUp and CreateIfMissing cannot both be set, which agent.Run refuses by name: a
// caller that may deliver the same work twice would append its prompt on every
// redelivery. That is why held asks the store instead of assuming, and why the intake
// deduplicates before it gets here.
//
// Force belongs to the resuming shape and to no other. A thread's next turn may arrive
// days after the last one and across a deploy, and a resume is otherwise refused whenever
// the model, the system prompt, the thinking mode or the reasoning effort has moved since
// the journal was written, which would end every open thread in the workspace on one
// configuration edit. The run still drops the standing approvals it can no longer vouch
// for.
func checkpointFor(sessionID string, held bool) agent.Checkpoint {
	if held {
		return agent.Checkpoint{ResumeID: sessionID, FollowUp: true, Force: true}
	}

	return agent.Checkpoint{ResumeID: sessionID, CreateIfMissing: true}
}

// resumeCheckpoint is how a click joins its thread's conversation: the journal that thread
// runs in, and the result of the call the conversation is waiting on.
//
// A click adds no turn, so neither FollowUp nor CreateIfMissing is set. CreateIfMissing
// would invent a conversation under the name of one that is gone and answer a call nobody
// in it ever made; a journal that is not there is the run's to refuse.
//
// answer is nil for the confirm gate. Its call was never dispatched, so the resume
// dispatches it and the gate asks again rather than the approval arriving as the guarded
// command's own result.
//
// Force is set for the reason checkpointFor sets it: a click may land days after the
// question and across a deploy that moved the model or the system prompt.
func resumeCheckpoint(sessionID string, answer *agent.DeferredAnswer) agent.Checkpoint {
	return agent.Checkpoint{ResumeID: sessionID, Answer: answer, Force: true}
}
