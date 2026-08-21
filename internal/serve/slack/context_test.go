//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package slack

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/runstate"
)

// said builds one person's message in a thread or a channel.
func said(user, text, ts string) message {
	return message{UserID: user, Text: text, TS: ts}
}

// botSaid builds a message this bot posted, as Slack reports it back.
func botSaid(text, ts string) message {
	return message{UserID: "U0BOT", BotID: "B0BOT", Text: text, TS: ts}
}

var _ = Describe("preload", func() {
	var (
		api *fakeAPI
		ch  *Channel
	)

	BeforeEach(func() {
		api = newFakeAPI()
		api.names = map[string]string{"U1": "ana", "U2": "ben"}

		ch = newTestChannel(testOptions(), api, newFakeSocket())
	})

	It("Should read the channel when the mention started a thread", func() {
		api.history["C1"] = []message{
			said("U1", "node3 is full again", "1700000000.000010"),
			said("U2", "same as last week", "1700000000.000020"),
		}

		m := &mention{TeamID: "T1", ChannelID: "C1", ThreadTS: "1700000000.000100", TS: "1700000000.000100", UserID: "U1"}

		msgs, err := ch.preload(context.Background(), m)
		Expect(err).ToNot(HaveOccurred())
		Expect(ch.render(context.Background(), msgs)).To(Equal("ana: node3 is full again\nben: same as last week"))
	})

	// The case the design cares about: a bot pulled into a discussion people have been
	// having reads that discussion, not the channel around it.
	It("Should read the thread when the mention arrived inside one", func() {
		api.history["C1"] = []message{said("U2", "unrelated channel chatter", "1700000000.000010")}
		api.replies["C1/1699999999.000900"] = []message{
			said("U1", "the deploy went out at four", "1699999999.000900"),
			said("U2", "and disk climbed right after", "1699999999.000950"),
		}

		m := &mention{TeamID: "T1", ChannelID: "C1", ThreadTS: "1699999999.000900", TS: "1700000000.000100", UserID: "U1"}

		msgs, err := ch.preload(context.Background(), m)
		Expect(err).ToNot(HaveOccurred())
		Expect(ch.render(context.Background(), msgs)).To(Equal("ana: the deploy went out at four\nben: and disk climbed right after"))
	})

	It("Should read nothing when the allowance is zero", func() {
		ch.lines = 0

		api.history["C1"] = []message{said("U1", "something", "1700000000.000010")}

		msgs, err := ch.preload(context.Background(), &mention{ChannelID: "C1", ThreadTS: "1", TS: "1"})
		Expect(err).ToNot(HaveOccurred())
		Expect(msgs).To(BeEmpty())
	})

	It("Should report a read that failed", func() {
		api.historyErr = fmt.Errorf("channel_not_found")

		_, err := ch.preload(context.Background(), &mention{ChannelID: "C1", ThreadTS: "1", TS: "1"})
		Expect(err).To(MatchError(ContainSubstring("channel_not_found")))
	})
})

var _ = Describe("gap", func() {
	var (
		api *fakeAPI
		ch  *Channel
	)

	BeforeEach(func() {
		api = newFakeAPI()
		api.names = map[string]string{"U1": "ana", "U2": "ben"}

		ch = newTestChannel(testOptions(), api, newFakeSocket())
	})

	// This is what makes mention-only turns usable rather than deaf.
	It("Should read what was said after this bot last spoke", func() {
		api.replies["C1/1700000000.000100"] = []message{
			said("U1", "what is eating disk", "1700000000.000100"),
			botSaid("it is the journal", "1700000000.000200"),
			said("U2", "we could rotate it", "1700000000.000300"),
			said("U1", "that would work", "1700000000.000400"),
		}

		m := &mention{TeamID: "T1", ChannelID: "C1", ThreadTS: "1700000000.000100", TS: "1700000000.000500", UserID: "U1"}

		msgs, err := ch.gap(context.Background(), m)
		Expect(err).ToNot(HaveOccurred())
		Expect(ch.render(context.Background(), msgs)).To(Equal("ben: we could rotate it\nana: that would work"))
	})

	It("Should take the whole window when this bot spoke further back than it reaches", func() {
		api.replies["C1/1700000000.000100"] = []message{
			said("U1", "one", "1700000000.000300"),
			said("U2", "two", "1700000000.000400"),
		}

		m := &mention{TeamID: "T1", ChannelID: "C1", ThreadTS: "1700000000.000100", TS: "1700000000.000500", UserID: "U1"}

		msgs, err := ch.gap(context.Background(), m)
		Expect(err).ToNot(HaveOccurred())
		Expect(msgs).To(HaveLen(2))
	})

	It("Should read nothing when nobody spoke since", func() {
		api.replies["C1/1700000000.000100"] = []message{
			said("U1", "what is eating disk", "1700000000.000100"),
			botSaid("it is the journal", "1700000000.000200"),
		}

		m := &mention{TeamID: "T1", ChannelID: "C1", ThreadTS: "1700000000.000100", TS: "1700000000.000500", UserID: "U1"}

		msgs, err := ch.gap(context.Background(), m)
		Expect(err).ToNot(HaveOccurred())
		Expect(msgs).To(BeEmpty())
	})

	// Another bot in the thread is conversation this one has not seen, not a mark of
	// where it last spoke.
	It("Should not treat another bot's message as its own", func() {
		api.replies["C1/1700000000.000100"] = []message{
			botSaid("it is the journal", "1700000000.000200"),
			{UserID: "U9", BotID: "B9", Text: "deploy finished", TS: "1700000000.000300"},
			said("U1", "so it was the deploy", "1700000000.000400"),
		}

		m := &mention{TeamID: "T1", ChannelID: "C1", ThreadTS: "1700000000.000100", TS: "1700000000.000500", UserID: "U1"}

		msgs, err := ch.gap(context.Background(), m)
		Expect(err).ToNot(HaveOccurred())
		Expect(msgs).To(HaveLen(1), "the other bot's own message is still dropped as a bot's")
		Expect(msgs[0].Text).To(Equal("so it was the deploy"))
	})
})

var _ = Describe("usable", func() {
	m := &mention{ChannelID: "C1", TS: "1700000000.000500"}

	It("Should drop bots, the joins and the leaves, and anything empty", func() {
		msgs := usable([]message{
			said("U1", "kept", "1700000000.000100"),
			botSaid("dropped", "1700000000.000110"),
			{UserID: "U2", Text: "joined", TS: "1700000000.000120", Subtype: "channel_join"},
			{UserID: "U2", Text: "   ", TS: "1700000000.000130"},
			{Text: "nobody said it", TS: "1700000000.000140"},
		}, m, "U0BOT")

		Expect(msgs).To(HaveLen(1))
		Expect(msgs[0].Text).To(Equal("kept"))
	})

	// It carries a subtype like the joins do, and it is a person speaking who asked to be
	// heard more widely than the rest of the thread.
	It("Should keep a thread_broadcast", func() {
		msgs := usable([]message{
			{UserID: "U1", Text: "everybody should see this", TS: "1700000000.000100", Subtype: subtypeBroadcast},
		}, m, "U0BOT")

		Expect(msgs).To(HaveLen(1))
	})

	// A channel where two people ask two things a minute apart produced exactly this: the
	// second turn read the first question out of the channel's history and answered both.
	It("Should drop another mention of this bot, which is a different conversation", func() {
		msgs := usable([]message{
			said("U1", "I wonder how atomicity works in nats", "1700000000.000100"),
			said("U1", "<@U0BOT> please help", "1700000000.000110"),
			said("U2", "ask <@U9> instead", "1700000000.000120"),
		}, m, "U0BOT")

		Expect(msgs).To(HaveLen(2))
		Expect(msgs[0].Text).To(Equal("I wonder how atomicity works in nats"))
		Expect(msgs[1].Text).To(Equal("ask <@U9> instead"), "somebody else's mention is part of what was said")
	})

	// Both reach the model as the prompt, so including them here sends them twice.
	It("Should drop the mention itself and anything said after it", func() {
		msgs := usable([]message{
			said("U1", "before", "1700000000.000100"),
			said("U1", "the mention", "1700000000.000500"),
			said("U2", "landed while the read was in flight", "1700000000.000600"),
		}, m, "U0BOT")

		Expect(msgs).To(HaveLen(1))
		Expect(msgs[0].Text).To(Equal("before"))
	})
})

var _ = Describe("before", func() {
	// A string comparison is quietly wrong when the seconds part grows a digit or the
	// fraction is not a fixed width.
	It("Should compare timestamps as numbers", func() {
		Expect(before("999999999.000100", "1700000000.000100")).To(BeTrue())
		Expect(before("1700000000.5", "1700000000.000100")).To(BeFalse())
		Expect(before("1700000000.000100", "1700000000.000100")).To(BeFalse())
	})

	It("Should sort an unparseable timestamp earliest, so the message is kept", func() {
		Expect(before("nonsense", "1700000000.000100")).To(BeTrue())
	})
})

var _ = Describe("names", func() {
	It("Should resolve a user once and answer from the cache after", func() {
		api := newFakeAPI()
		api.names = map[string]string{"U1": "ana"}

		n := newNames()

		Expect(n.of(context.Background(), api, "U1")).To(Equal("ana"))
		Expect(n.of(context.Background(), api, "U1")).To(Equal("ana"))
		Expect(api.lookups).To(Equal(1), "a conversation is mostly the same few people")
	})

	It("Should render the id when a lookup fails, and try again next time", func() {
		api := newFakeAPI()
		api.nameErr = fmt.Errorf("ratelimited")

		n := newNames()

		Expect(n.of(context.Background(), api, "U1")).To(Equal("U1"))
		Expect(n.of(context.Background(), api, "U1")).To(Equal("U1"))
		Expect(api.lookups).To(Equal(2), "a failure that passes should not leave an id in every line for as long as this worker runs")
	})

	It("Should name a message nobody posted", func() {
		Expect(newNames().of(context.Background(), newFakeAPI(), "")).To(Equal("unknown"))
	})
})

var _ = Describe("What a turn carries", func() {
	var (
		api    *fakeAPI
		socket *fakeSocket
		opts   Options
	)

	BeforeEach(func() {
		api = newFakeAPI()
		api.names = map[string]string{"U1": "ana", "U2": "ben"}
		socket = newFakeSocket()
		opts = testOptions()
	})

	// Supporting material is what Work.Context is for, and the run offers it alongside the
	// prompt rather than as part of what the person said.
	It("Should carry an opening turn's surroundings as context", func() {
		api.history["C1"] = []message{
			said("U2", "node3 is full again", "1700000000.000010"),
		}

		ch := servingChannel(opts, api, socket)

		socket.deliver(aMention().envelope())

		w := nextWork(ch)
		Expect(w.Prompt).To(Equal("what is eating disk on node3"))
		Expect(w.Context).To(HaveSuffix("ben: node3 is full again"))
		Expect(w.Context).To(HavePrefix(preloadHeader), "serve appends this to the prompt unlabeled, so it says what it is")
	})

	// A short request leaves the surrounding lines as the only substance in the prompt, and
	// a turn asked to "please help" answered every question it found in the channel.
	It("Should say nothing about background when there is none to give", func() {
		ch := servingChannel(opts, api, socket)

		socket.deliver(aMention().envelope())

		Expect(nextWork(ch).Context).To(BeEmpty())
	})

	// A follow-up is only a sentence alongside the discussion it answers, so the gap
	// belongs in the prompt rather than beside it.
	It("Should carry a follow-up's gap above what the person said", func() {
		ch := servingChannel(opts, api, socket)

		socket.deliver(aMention().envelope())
		ended(nextWork(ch))

		// The run journals the conversation; the spec stands in for it, so the next
		// mention in this thread is a follow-up.
		id := SessionFor(opts.Identity, "T1", "C1", "1700000000.000100")
		j, err := opts.Sessions.Create(id, runstate.MetaRecord{Version: runstate.Version, RunID: id})
		Expect(err).ToNot(HaveOccurred())
		Expect(j.Close()).To(Succeed())

		api.replies["C1/1700000000.000100"] = []message{
			said("U1", "what is eating disk on node3", "1700000000.000100"),
			botSaid("the journal", "1700000000.000200"),
			said("U2", "we could rotate it", "1700000000.000300"),
		}

		second := aMention()
		second.ThreadTS = "1700000000.000100"
		second.TS = "1700000000.000400"
		second.Text = "<@U0BOT> ok do that then"
		second.EnvelopeID = "Ev2"

		socket.deliver(second.envelope())

		w := nextWork(ch)
		Expect(w.Checkpoint.FollowUp).To(BeTrue())
		Expect(w.Context).To(BeEmpty(), "a follow-up reads its thread rather than the channel around it")
		Expect(w.Prompt).To(Equal("Said in this thread since I last replied:\nben: we could rotate it\n\nok do that then"))
	})

	// A person who asked a question would rather have it answered narrowly than not at
	// all.
	It("Should run the turn anyway when the read fails", func() {
		api.historyErr = fmt.Errorf("channel_not_found")

		ch := servingChannel(opts, api, socket)

		socket.deliver(aMention().envelope())

		w := nextWork(ch)
		Expect(w.Prompt).To(Equal("what is eating disk on node3"))
		Expect(w.Context).To(BeEmpty())
	})
})
