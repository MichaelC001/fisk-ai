//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package slack

import (
	"context"
	"strconv"
	"strings"
	"sync"
)

// Only mentions are turns, which keeps spend predictable and keeps this bot out of
// conversations nobody addressed to it. The cost is that it is deaf between mentions, and
// "ok do that then" refers to a discussion it never saw. Two reads close that.
//
// The preload runs on the turn that opens a conversation and reads the container the
// mention arrived in. The gap runs on every turn after it and reads what was said in the
// thread since this bot last spoke. Both are capped by context_lines and both drop what
// nobody said.

// preload is the surrounding conversation an opening turn is given, oldest first.
//
// It reads the container the mention arrived in: the channel for a mention that started a
// thread, and the thread itself for a mention somebody made inside one people had been
// talking in already. The second is the case that matters, a bot pulled into an incident
// thread being asked about what is above it rather than about the channel around it.
//
// Nothing here stops at a message of this bot's own, and it cannot: every message this bot
// posts is a threaded reply, and threaded replies are absent from a channel's history. So
// an opening turn in a channel reads the full allowance every time, where the gap read
// below usually reads a few lines.
func (c *Channel) preload(ctx context.Context, m *mention) ([]message, error) {
	if c.lines <= 0 {
		return nil, nil
	}

	if m.startedThread() {
		msgs, err := c.api.channelHistory(ctx, m.ChannelID, c.lines)
		if err != nil {
			return nil, err
		}

		return usable(msgs, m), nil
	}

	msgs, err := c.api.threadReplies(ctx, m.ChannelID, m.ThreadTS, c.lines)
	if err != nil {
		return nil, err
	}

	return usable(msgs, m), nil
}

// gap is what was said in the thread since this bot last spoke, oldest first.
//
// It is what makes mention-only turns usable rather than deaf: the people in the thread
// carry on talking between mentions, and a follow-up of "ok do that then" means nothing
// without it.
//
// The walk finds this bot's own last message in the thread's tail and takes what follows.
// A tail that holds no message of ours means this bot last spoke further back than the
// allowance reaches, so every message in the window is one it has not seen, and the whole
// window is the answer.
func (c *Channel) gap(ctx context.Context, m *mention) ([]message, error) {
	if c.lines <= 0 {
		return nil, nil
	}

	// One more than the allowance, so a window whose oldest message is this bot's own
	// still has the allowance left after it.
	msgs, err := c.api.threadReplies(ctx, m.ChannelID, m.ThreadTS, c.lines+1)
	if err != nil {
		return nil, err
	}

	after := msgs
	for i := len(msgs) - 1; i >= 0; i-- {
		if c.spoke(msgs[i]) {
			after = msgs[i+1:]
			break
		}
	}

	return usable(after, m), nil
}

// spoke reports whether this bot posted a message. A bot id alone is not enough: another
// bot in the same thread has one too, and its messages are conversation this bot has not
// seen rather than a mark of where it last spoke.
func (c *Channel) spoke(msg message) bool {
	return msg.UserID != "" && msg.UserID == c.workspace.UserID
}

// startedThread reports whether this mention opened its thread, which is what decides
// which container the preload reads.
func (m *mention) startedThread() bool { return m.ThreadTS == m.TS }

// usable drops what is not somebody talking, and anything from the mention onwards.
//
// The mention itself is in whatever was read, and so is anything said after it while the
// read was in flight. Both reach the model as the prompt, so including them here would
// send them twice.
//
// A thread_broadcast survives. It carries a subtype, which is what the joins and the
// leaves carry, but it is a person speaking and one who asked to be heard more widely
// than the rest of the thread.
func usable(msgs []message, m *mention) []message {
	out := make([]message, 0, len(msgs))

	for _, msg := range msgs {
		switch {
		case msg.BotID != "", msg.UserID == "":
			continue
		case msg.Subtype != "" && msg.Subtype != subtypeBroadcast:
			continue
		case strings.TrimSpace(msg.Text) == "":
			continue
		case !before(msg.TS, m.TS):
			continue
		}

		out = append(out, msg)
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

// subtypeBroadcast is the reply somebody sent to the thread and the channel at once.
const subtypeBroadcast = "thread_broadcast"

// before reports whether one Slack timestamp precedes another.
//
// They are compared as numbers rather than as strings. The seconds part grows a digit
// eventually and the fractional part is not guaranteed a fixed width, either of which
// makes a string comparison quietly wrong. A timestamp that will not parse sorts as
// earliest, which keeps a message rather than dropping it.
func before(a, b string) bool {
	return tsValue(a) < tsValue(b)
}

func tsValue(ts string) float64 {
	v, err := strconv.ParseFloat(ts, 64)
	if err != nil {
		return 0
	}

	return v
}

// render turns messages into the lines a prompt carries, resolving each speaker's display
// name.
//
// The name rather than the user id, because the model is reading a conversation and
// "U024BE7LH: restart it" is not one. A lookup that fails renders the id, which is worse
// to read and better than losing the line.
func (c *Channel) render(ctx context.Context, msgs []message) string {
	if len(msgs) == 0 {
		return ""
	}

	lines := make([]string, 0, len(msgs))
	for _, msg := range msgs {
		lines = append(lines, c.names.of(ctx, c.api, msg.UserID)+": "+strings.TrimSpace(msg.Text))
	}

	return strings.Join(lines, "\n")
}

// names resolves a Slack user id to what a person reading the thread sees, once per user.
//
// A conversation is mostly the same few people, so the cache turns a read of twenty lines
// into two or three calls rather than twenty. It is never invalidated: a display name a
// person changes mid-conversation is stale in the prompt and nowhere else, and a worker
// that answers for weeks holds one entry per person it has heard from.
type names struct {
	mu sync.Mutex
	by map[string]string
}

func newNames() *names { return &names{by: map[string]string{}} }

func (n *names) of(ctx context.Context, api api, userID string) string {
	if userID == "" {
		return "unknown"
	}

	n.mu.Lock()
	cached, ok := n.by[userID]
	n.mu.Unlock()

	if ok {
		return cached
	}

	name, err := api.userDisplayName(ctx, userID)
	if err != nil || name == "" {
		// Not cached: a lookup that failed for a reason that passes should be tried again
		// on the next turn rather than leaving the id in every line for as long as this
		// worker runs.
		return userID
	}

	n.mu.Lock()
	n.by[userID] = name
	n.mu.Unlock()

	return name
}
