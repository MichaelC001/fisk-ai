//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package slack

import (
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/runstate"
)

// mentionEvent builds the events envelope Slack delivers for an app_mention, so a spec
// varies one field of a real payload rather than a hand-made struct.
type mentionEvent struct {
	Team     string
	Channel  string
	User     string
	BotID    string
	Text     string
	TS       string
	ThreadTS string

	// EnvelopeID and Retry are the delivery rather than the message. Slack mints a fresh
	// envelope id for each delivery of one message, which is why a redelivery is
	// recognized by the message rather than by these.
	EnvelopeID string
	Retry      int
}

func (m mentionEvent) envelope() envelope {
	inner := map[string]any{
		"type":    "app_mention",
		"user":    m.User,
		"text":    m.Text,
		"ts":      m.TS,
		"channel": m.Channel,
	}
	if m.ThreadTS != "" {
		inner["thread_ts"] = m.ThreadTS
	}
	if m.BotID != "" {
		inner["bot_id"] = m.BotID
	}

	body, err := json.Marshal(map[string]any{
		"type":    "event_callback",
		"team_id": m.Team,
		"event":   inner,
	})
	Expect(err).ToNot(HaveOccurred())

	id := m.EnvelopeID
	if id == "" {
		id = "Ev1"
	}

	return envelope{ID: id, Kind: envelopeMention, Payload: body, RetryAttempt: m.Retry}
}

// aMention is a valid mention a spec varies.
func aMention() mentionEvent {
	return mentionEvent{
		Team:    "T1",
		Channel: "C1",
		User:    "U1",
		Text:    "<@U0BOT> what is eating disk on node3",
		TS:      "1700000000.000100",
	}
}

var _ = Describe("mentionOf", func() {
	It("Should decode what a conversation is derived from", func() {
		m, ok, err := mentionOf(aMention().envelope(), "U0BOT")
		Expect(err).ToNot(HaveOccurred())
		Expect(ok).To(BeTrue())

		Expect(m.TeamID).To(Equal("T1"))
		Expect(m.ChannelID).To(Equal("C1"))
		Expect(m.UserID).To(Equal("U1"))
		Expect(m.TS).To(Equal("1700000000.000100"))
		Expect(m.Text).To(Equal("what is eating disk on node3"), "the bot's own mention is not part of the question")
	})

	// A mention that starts a thread carries no thread_ts, and hashing what it carries
	// would give every top-level mention in a channel one journal.
	It("Should use the mention's own timestamp as the thread when it started one", func() {
		m, _, err := mentionOf(aMention().envelope(), "U0BOT")
		Expect(err).ToNot(HaveOccurred())
		Expect(m.ThreadTS).To(Equal("1700000000.000100"))
		Expect(m.ThreadTS).To(Equal(m.TS))
	})

	It("Should use the thread it arrived in when there is one", func() {
		ev := aMention()
		ev.ThreadTS = "1699999999.000900"

		m, _, err := mentionOf(ev.envelope(), "U0BOT")
		Expect(err).ToNot(HaveOccurred())
		Expect(m.ThreadTS).To(Equal("1699999999.000900"))
		Expect(m.TS).To(Equal("1700000000.000100"), "its own timestamp is still what a redelivery is recognized by")
	})

	It("Should ignore an envelope that is not a mention", func() {
		_, ok, err := mentionOf(envelope{Kind: envelopeInteractive, Payload: []byte(`{}`)}, "U0BOT")
		Expect(err).ToNot(HaveOccurred())
		Expect(ok).To(BeFalse())
	})

	// Two bots mentioning each other in a thread would take turns until a budget stopped
	// them.
	It("Should ignore a mention from a bot", func() {
		ev := aMention()
		ev.BotID = "B1"

		_, ok, err := mentionOf(ev.envelope(), "U0BOT")
		Expect(err).ToNot(HaveOccurred())
		Expect(ok).To(BeFalse())
	})

	It("Should ignore its own mention", func() {
		ev := aMention()
		ev.User = "U0BOT"

		_, ok, err := mentionOf(ev.envelope(), "U0BOT")
		Expect(err).ToNot(HaveOccurred())
		Expect(ok).To(BeFalse())
	})

	It("Should refuse a mention missing what a conversation is derived from", func() {
		for _, missing := range []func(*mentionEvent){
			func(e *mentionEvent) { e.Team = "" },
			func(e *mentionEvent) { e.Channel = "" },
			func(e *mentionEvent) { e.TS = "" },
		} {
			ev := aMention()
			missing(&ev)

			_, _, err := mentionOf(ev.envelope(), "U0BOT")
			Expect(err).To(MatchError(ContainSubstring("identifiers a conversation is derived from")))
		}
	})

	It("Should refuse a payload it cannot decode", func() {
		_, _, err := mentionOf(envelope{Kind: envelopeMention, Payload: []byte(`not json`)}, "U0BOT")
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("stripMention", func() {
	It("Should remove this bot and leave everybody else", func() {
		Expect(stripMention("<@U0BOT> ask <@U2> about it", "U0BOT")).To(Equal(" ask <@U2> about it"))
	})

	It("Should remove the bot wherever it appears, including a labeled mention", func() {
		Expect(stripMention("hey <@U0BOT|bot> and <@U0BOT>", "U0BOT")).To(Equal("hey  and "))
	})

	It("Should leave the text alone when it knows no bot id", func() {
		Expect(stripMention("<@U0BOT> hello", "")).To(Equal("<@U0BOT> hello"))
	})
})

var _ = Describe("callerOf", func() {
	// A log line and a journal's Meta record are read by a person months later, and
	// T1/U024BE7LH tells them nothing about who it was.
	It("Should record the username with the user id and report it verified", func() {
		caller := callerOf(&mention{TeamID: "T1", UserID: "U024BE7LH"}, person{Full: "Roland Pienaar", Username: "rip"})

		Expect(caller.Name).To(Equal("rip/U024BE7LH"))
		Expect(caller.Verified).To(BeTrue(), "Slack authenticated the sender")
	})

	// The record is worth having with one name missing from it.
	It("Should record the id alone when the lookup gave no username", func() {
		Expect(callerOf(&mention{UserID: "U1"}, person{}).Name).To(Equal("U1"))
		Expect(callerOf(&mention{UserID: "U1"}, person{Full: "U1", Username: "U1"}).Name).To(Equal("U1"))
	})
})

var _ = Describe("A thread's turns", func() {
	// The three cases the design names, walked end to end: an opening mention creates,
	// the next mention in that thread adds a turn, and a redelivery of either is dropped.
	It("Should open a conversation, continue it, and refuse a redelivery", func() {
		opts := testOptions()
		ch := newTestChannel(opts, newFakeAPI(), newFakeSocket())

		first := aMention()
		m, ok, err := mentionOf(first.envelope(), "U0BOT")
		Expect(err).ToNot(HaveOccurred())
		Expect(ok).To(BeTrue())

		Expect(ch.taken.take(m.ChannelID, m.TS)).To(BeTrue())

		id := SessionFor(opts.Identity, m.TeamID, m.ChannelID, m.ThreadTS)

		held, err := ch.held(id)
		Expect(err).ToNot(HaveOccurred())
		Expect(held).To(BeFalse())
		Expect(checkpointFor(id, held).CreateIfMissing).To(BeTrue())

		// The run would have created it; the spec stands in for the run.
		j, err := opts.Sessions.Create(id, runstate.MetaRecord{Version: runstate.Version, RunID: id})
		Expect(err).ToNot(HaveOccurred())
		Expect(j.Close()).To(Succeed())

		second := aMention()
		second.ThreadTS = first.TS
		second.TS = "1700000001.000100"
		second.Text = "<@U0BOT> ok do that then"

		m2, _, err := mentionOf(second.envelope(), "U0BOT")
		Expect(err).ToNot(HaveOccurred())
		Expect(ch.taken.take(m2.ChannelID, m2.TS)).To(BeTrue())

		Expect(SessionFor(opts.Identity, m2.TeamID, m2.ChannelID, m2.ThreadTS)).To(Equal(id), "the same thread is the same conversation")

		held, err = ch.held(id)
		Expect(err).ToNot(HaveOccurred())
		Expect(held).To(BeTrue())

		cp := checkpointFor(id, held)
		Expect(cp.FollowUp).To(BeTrue())
		Expect(cp.CreateIfMissing).To(BeFalse())

		Expect(ch.taken.take(m2.ChannelID, m2.TS)).To(BeFalse(), "Slack redelivering it pays for no second turn")
	})
})
