//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package slack

import (
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/slack-go/slack/slackevents"

	"github.com/choria-io/fisk-ai/internal/serve"
)

// mention is one app_mention reduced to what this channel decides on.
type mention struct {
	// TeamID, ChannelID and ThreadTS identify the conversation. ThreadTS is already
	// resolved: the thread the mention arrived in, or the mention itself where it started
	// one.
	TeamID    string
	ChannelID string
	ThreadTS  string

	// TS is this message's own timestamp, unique within its channel. It is what a
	// redelivery is recognized by, and it equals ThreadTS on a mention that opened the
	// thread.
	TS string

	// UserID is who mentioned the bot.
	UserID string

	// Text is what they said, with the mention of this bot removed.
	Text string
}

// mentionOf decodes one envelope into the mention this channel acts on, reporting false
// for an envelope that is not one.
//
// An envelope carrying anything other than an app_mention is acknowledged and dropped:
// only app_mention is subscribed, so anything else here is a subscription somebody added
// in the Slack app configuration rather than something to answer.
//
// A mention from a bot is dropped as well, this one's own answers included. Two bots
// mentioning each other in a thread would otherwise take turns until a budget stopped
// them.
func mentionOf(env envelope, botUserID string) (*mention, bool, error) {
	if env.Kind != envelopeMention {
		return nil, false, nil
	}

	// The token is not verified because socket mode already authenticated the
	// connection: the envelope arrived over a websocket this process opened with its own
	// app-level token, so there is no shared secret left to check.
	outer, err := slackevents.ParseEvent(env.Payload, slackevents.OptionNoVerifyToken())
	if err != nil {
		return nil, false, fmt.Errorf("decoding an events envelope: %w", err)
	}

	inner, ok := outer.InnerEvent.Data.(*slackevents.AppMentionEvent)
	if !ok {
		return nil, false, nil
	}

	if inner.BotID != "" || inner.User == "" || inner.User == botUserID {
		return nil, false, nil
	}

	// team_id is on the outer envelope rather than the event, and a shared channel names
	// the sender's own team on the event. The conversation belongs to the workspace the
	// event was delivered for, which is the outer one.
	team := outer.TeamID
	if team == "" {
		team = inner.SourceTeam
	}

	m := &mention{
		TeamID:    team,
		ChannelID: inner.Channel,
		ThreadTS:  threadOf(inner),
		TS:        inner.TimeStamp,
		UserID:    inner.User,
		Text:      strings.TrimSpace(stripMention(inner.Text, botUserID)),
	}

	if m.TeamID == "" || m.ChannelID == "" || m.ThreadTS == "" || m.TS == "" {
		return nil, false, fmt.Errorf("an app_mention arrived without the identifiers a conversation is derived from")
	}

	return m, true, nil
}

// threadOf resolves which thread a mention belongs to. An app_mention inside a thread
// carries that thread's root; one that starts a thread carries none, and its own
// timestamp becomes the root the bot replies under.
//
// Getting this wrong does not misplace a reply, it merges or splits conversations: an
// empty thread_ts hashed into a session would give every top-level mention in a channel
// one journal, and a resume against a completed journal is answered from the journal
// with no model call, so one person would be served another's stored answer.
func threadOf(ev *slackevents.AppMentionEvent) string {
	if ev.ThreadTimeStamp != "" {
		return ev.ThreadTimeStamp
	}

	return ev.TimeStamp
}

// botMentionPattern matches a user mention in Slack's own markup, which is how this bot's
// name arrives in the text.
var botMentionPattern = regexp.MustCompile(`<@([A-Z0-9]+)(\|[^>]*)?>`)

// stripMention removes this bot's own mention from what a person wrote, so the prompt is
// the question rather than the address on the envelope. Mentions of anybody else are left
// alone: they are part of what was said.
func stripMention(text, botUserID string) string {
	if botUserID == "" {
		return text
	}

	return botMentionPattern.ReplaceAllStringFunc(text, func(match string) string {
		parts := botMentionPattern.FindStringSubmatch(match)
		if len(parts) > 1 && parts[1] == botUserID {
			return ""
		}

		return match
	})
}

// callerOf is what this channel knows about who asked, as rip/U024BE7LH. Slack
// authenticated the sender, so the claim is verified.
//
// It reaches a worker's log and the journal's Meta record, which somebody reads back
// months later, so it carries both names. The username is who a reader recognizes; the id
// is who it still was after they renamed themselves.
//
// A lookup that failed leaves the id on its own rather than the id written twice, which is
// the same fallback every other reader of a name takes.
func callerOf(m *mention, p person) serve.Caller {
	if p.Username == "" || p.Username == m.UserID {
		return serve.Caller{Name: m.UserID, Verified: true}
	}

	return serve.Caller{Name: p.Username + "/" + m.UserID, Verified: true}
}

// seen recognizes a message this worker already took, so a redelivery does not pay for
// the same turn twice.
//
// Slack delivers at-least-once: an envelope not acknowledged within three seconds, or
// acknowledged into a socket that had already dropped, arrives again with RetryAttempt
// set. The retry marker alone is not enough to decide on, since a first delivery this
// worker never finished acting on also arrives as a retry and does deserve a turn. What
// decides is whether this message was already taken.
//
// It holds message timestamps rather than envelope ids, because Slack mints a fresh
// envelope id for each delivery of one message.
type seen struct {
	mu sync.Mutex

	// ids is the set, and order the arrival sequence the oldest is evicted from. A
	// bounded set rather than an expiring one: nothing here needs a timer, and the memory
	// is what the bound says it is.
	//
	// Evicting an entry a retry then arrives for costs one duplicate turn, which is the
	// same cost as not deduplicating at all, so the bound only has to outlast Slack's
	// retry window under this worker's own traffic.
	ids   map[string]struct{}
	order []string
	limit int
}

// defaultSeenLimit is how many message timestamps are remembered. A worker answering a
// mention a minute would remember the last seventeen hours of them, against a retry
// window measured in minutes.
const defaultSeenLimit = 1024

func newSeen(limit int) *seen {
	if limit <= 0 {
		limit = defaultSeenLimit
	}

	return &seen{ids: make(map[string]struct{}, limit), limit: limit}
}

// take records a message and reports whether it is new. A false answer means this worker
// already took it, so the delivery is acknowledged and dropped.
//
// The key carries the channel because a message timestamp is unique only within one.
func (s *seen) take(channelID, ts string) bool {
	key := channelID + "/" + ts

	s.mu.Lock()
	defer s.mu.Unlock()

	_, already := s.ids[key]
	if already {
		return false
	}

	s.ids[key] = struct{}{}
	s.order = append(s.order, key)

	if len(s.order) > s.limit {
		delete(s.ids, s.order[0])
		s.order = s.order[1:]
	}

	return true
}
