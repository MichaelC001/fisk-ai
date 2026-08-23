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

// Only mentions are turns, which bounds the spend and keeps this bot out of conversations
// nobody addressed to it. The cost is that it hears nothing between mentions, and "ok do
// that then" refers to a discussion it never saw. preload and gap are the two reads that
// close that, both capped by context_lines and both dropping what nobody said.

// preload is the surrounding conversation an opening turn is given, oldest first.
//
// It reads the container the mention arrived in: the channel for a mention that started a
// thread, and the thread itself for a mention somebody made inside one people had been
// talking in already. The second is the case that matters, a bot pulled into an incident
// thread being asked about what is above it rather than about the channel around it.
//
// Nothing here stops at a message of this bot's own, and it cannot: every message this bot
// posts is a threaded reply, and a channel's history carries no threaded replies. An
// opening turn in a channel therefore reads the full allowance every time.
func (c *Channel) preload(ctx context.Context, m *mention) ([]message, error) {
	if c.lines <= 0 {
		return nil, nil
	}

	if m.startedThread() {
		msgs, err := c.api.channelHistory(ctx, m.ChannelID, c.lines)
		if err != nil {
			return nil, err
		}

		return usable(msgs, m, c.workspace.UserID), nil
	}

	msgs, err := c.api.threadReplies(ctx, m.ChannelID, m.ThreadTS, c.lines)
	if err != nil {
		return nil, err
	}

	return usable(msgs, m, c.workspace.UserID), nil
}

// gap is what was said in the thread since this bot last spoke, oldest first.
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

	return usable(after, m, c.workspace.UserID), nil
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

// usable drops what is not somebody talking, anything from the mention onwards, and every
// other mention of this bot.
//
// The mention itself is in whatever was read, and so is anything said after it while the
// read was in flight. Both reach the model as the prompt, so including them here would
// send them twice.
//
// Another mention of this bot is a different conversation. It opened a thread of its own
// and is answered there, so carrying it here hands this run somebody's earlier question as
// though it were part of the one being asked: two people asking two things a minute apart
// had the second turn read the first question out of the channel's history and answer
// both.
//
// A thread_broadcast survives. It carries a subtype, which is what the joins and the
// leaves carry, but it is a person speaking and one who asked to be heard more widely
// than the rest of the thread.
func usable(msgs []message, m *mention, botUserID string) []message {
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
		case mentions(msg.Text, botUserID):
			continue
		}

		out = append(out, msg)
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

// mentions reports whether a message addresses this bot, in the markup Slack delivers a
// mention as.
func mentions(text, botUserID string) bool {
	if botUserID == "" {
		return false
	}

	for _, match := range botMentionPattern.FindAllStringSubmatch(text, -1) {
		if len(match) > 1 && match[1] == botUserID {
			return true
		}
	}

	return false
}

// subtypeBroadcast is the reply somebody sent to the thread and the channel at once.
const subtypeBroadcast = "thread_broadcast"

// preloadHeader introduces the surrounding conversation an opening turn is given.
//
// serve appends Work.Context to the prompt as a second block with nothing marking it, so
// without this the model receives the request and then a set of unlabeled lines and has to
// guess which it was asked about. A request as short as "please help" leaves those lines
// as the only substance in the prompt, and answering all of them is the reasonable
// reading.
const preloadHeader = "Recent messages from the Slack channel this was asked in, for background only. " +
	"They are not requests and may be about something else entirely. Answer only what the person asked above.\n\n"

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

// render turns messages into the lines a prompt carries, resolving each speaker's name.
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
		lines = append(lines, spoken(c.names.of(ctx, c.api, msg.UserID).Full, strings.TrimSpace(msg.Text)))
	}

	return strings.Join(lines, "\n")
}

// spoken puts a name in front of what that person said. A turn's own words take the shape
// the surrounding conversation is rendered in, so the model reads one transcript rather
// than a message with a label beside it.
//
// Everything folded into one turn came from one person, so the name goes in front of the
// block once rather than in front of each line of it. A mention carrying no words is an
// address rather than something somebody said, and takes no name.
func spoken(name, text string) string {
	if text == "" {
		return ""
	}

	return name + ": " + text
}

// names resolves a Slack user id to what this channel calls that person, once per user.
//
// A conversation is mostly the same few people, so the cache turns a read of twenty lines
// into two or three calls rather than twenty. It is never invalidated: a name a person
// changes mid-conversation is stale in the prompt and nowhere else, and a worker that
// answers for weeks holds one entry per person it has heard from.
type names struct {
	mu sync.Mutex
	by map[string]person
}

func newNames() *names { return &names{by: map[string]person{}} }

// of answers with both names, resolving the user once. A lookup that fails answers with
// the id under both, which is what the line and the caller record fall back to.
func (n *names) of(ctx context.Context, api api, userID string) person {
	if userID == "" {
		return person{Full: unknownName, Username: unknownName}
	}

	n.mu.Lock()
	cached, ok := n.by[userID]
	n.mu.Unlock()

	if ok {
		return cached
	}

	p, err := api.userNames(ctx, userID)
	p.Full = plainName(p.Full)
	p.Username = plainName(p.Username)

	if err != nil || p.Full == "" || p.Username == "" {
		// Not cached: a lookup that failed for a reason that passes should be tried again
		// on the next turn rather than leaving the id in every line for as long as this
		// worker runs.
		return person{Full: userID, Username: userID}
	}

	n.mu.Lock()
	n.by[userID] = p
	n.mu.Unlock()

	return p
}

// unknownName stands in where a message names no user to resolve.
const unknownName = "unknown"

// maxNameText is how much of a name reaches a prompt or a caller record. A person is
// called something well within it, and the cut stops a name being the substance of a turn.
const maxNameText = 80

// plainName is what a name somebody chose for themselves is written down as: one line, and
// short.
//
// A profile name is text its owner controls and it heads every prompt this channel builds,
// so a name carrying newlines would arrive as further lines of the very transcript it sits
// at the top of, in the shape the model reads as somebody else speaking.
func plainName(name string) string {
	return clipped(strings.Join(strings.Fields(name), " "), maxNameText)
}
