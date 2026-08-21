//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package slack

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/serve"
)

// servingChannel builds a channel over the fakes and starts it reading, which is what the
// server's first Next does. It is released when the spec ends.
func servingChannel(opts Options, a api, s *fakeSocket) *Channel {
	GinkgoHelper()

	ch := newTestChannel(opts, a, s)
	ch.start()

	Eventually(s.ran).Should(BeClosed())
	DeferCleanup(func() { Expect(ch.Close()).To(Succeed()) })

	return ch
}

// nextWork takes the work a spec expects to be there, failing rather than hanging when it
// is not.
func nextWork(ch *Channel) *serve.Work {
	GinkgoHelper()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	w, err := ch.Next(ctx)
	Expect(err).ToNot(HaveOccurred())
	Expect(w).ToNot(BeNil())

	return w
}

// noWork asserts that nothing is handed over, which is how a spec says a mention was
// folded, queued behind or refused rather than admitted.
func noWork(ch *Channel) {
	GinkgoHelper()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	_, err := ch.Next(ctx)
	Expect(err).To(MatchError(context.DeadlineExceeded))
}

// ended reports a turn finished at a boundary a follow-up can join.
func ended(w *serve.Work) {
	GinkgoHelper()

	Expect(w.Done(context.Background(), serve.Outcome{ID: w.ID, Reason: runstate.ReasonCompleted})).To(Succeed())
}

var _ = Describe("Intake", func() {
	var (
		api    *fakeAPI
		socket *fakeSocket
		opts   Options
	)

	BeforeEach(func() {
		api = newFakeAPI()
		socket = newFakeSocket()
		opts = testOptions()
	})

	It("Should acknowledge a mention and hand it over as work", func() {
		ch := servingChannel(opts, api, socket)

		socket.deliver(aMention().envelope())
		Eventually(socket.acked).Should(Equal([]string{"Ev1"}))

		w := nextWork(ch)

		Expect(w.ID).To(Equal("C1/1700000000.000100"), "the mention's channel and timestamp, which Slack minted rather than anybody choosing")
		Expect(w.ClaimedBy).To(Equal(w.ID))
		Expect(w.Prompt).To(Equal("what is eating disk on node3"))
		Expect(w.Caller).To(Equal(serve.Caller{Name: "T1/U1", Verified: true}))
		Expect(w.HumanPaced).To(BeTrue(), "a thread moves at the pace of whoever is typing in it")
		Expect(w.SuspendRequested).ToNot(BeNil())
		Expect(w.Done).ToNot(BeNil())

		id := SessionFor(opts.Identity, "T1", "C1", "1700000000.000100")
		Expect(w.Checkpoint).To(Equal(agent.Checkpoint{ResumeID: id, CreateIfMissing: true}))
	})

	// Slack redelivers anything it was not acknowledged for inside three seconds, with a
	// fresh envelope id each time, so a second turn on one message is the failure this
	// prevents.
	It("Should acknowledge a redelivered message and take no second turn on it", func() {
		ch := servingChannel(opts, api, socket)

		socket.deliver(aMention().envelope())
		Eventually(socket.acked).Should(HaveLen(1))

		again := aMention()
		again.EnvelopeID = "Ev2"
		again.Retry = 1

		socket.deliver(again.envelope())
		Eventually(socket.acked).Should(Equal([]string{"Ev1", "Ev2"}), "an envelope Slack is waiting on is answered whether or not it earns a turn")

		w := nextWork(ch)
		Expect(w.Prompt).To(Equal("what is eating disk on node3"))

		noWork(ch)
	})

	It("Should acknowledge and drop what it does not answer", func() {
		ch := servingChannel(opts, api, socket)

		// Only app_mention is subscribed, so an interactive envelope here is the click
		// path's rather than intake's, and anything else is a subscription somebody added.
		socket.deliver(envelope{ID: "Ev1", Kind: envelopeInteractive, Payload: []byte(`{}`)})
		Eventually(socket.acked).Should(Equal([]string{"Ev1"}))

		fromBot := aMention()
		fromBot.EnvelopeID = "Ev2"
		fromBot.BotID = "B1"

		socket.deliver(fromBot.envelope())
		Eventually(socket.acked).Should(Equal([]string{"Ev1", "Ev2"}))

		noWork(ch)
		Expect(api.messages()).To(BeEmpty())
	})

	Describe("Coalescing", func() {
		// Three lines typed in ten seconds are one thought, and the run they were typed
		// at has already been handed over.
		It("Should fold further mentions from the same person into one follow-up turn", func() {
			ch := servingChannel(opts, api, socket)

			socket.deliver(aMention().envelope())
			Eventually(socket.acked).Should(HaveLen(1))

			first := nextWork(ch)

			// The conversation the run would have written, so the follow-up is asked to
			// continue it rather than to create it.
			id := SessionFor(opts.Identity, "T1", "C1", "1700000000.000100")
			j, err := opts.Sessions.Create(id, runstate.MetaRecord{Version: runstate.Version, RunID: id})
			Expect(err).ToNot(HaveOccurred())
			Expect(j.Close()).To(Succeed())

			for i, text := range []string{"and check node4", "and node5"} {
				m := aMention()
				m.EnvelopeID = "EvF"
				m.ThreadTS = "1700000000.000100"
				m.TS = "1700000010.00010" + string(rune('0'+i))
				m.Text = "<@U0BOT> " + text

				socket.deliver(m.envelope())
			}

			Eventually(socket.acked).Should(HaveLen(3))
			noWork(ch)

			ended(first)

			follow := nextWork(ch)
			Expect(follow.Prompt).To(Equal("and check node4\nand node5"), "one turn carrying both lines")
			Expect(follow.Caller.Name).To(Equal("T1/U1"))
			Expect(follow.Checkpoint).To(Equal(agent.Checkpoint{ResumeID: id, FollowUp: true, Force: true}))
			Expect(api.messages()).To(BeEmpty())
		})

		// A line typed while the turn was still waiting for a worker costs nothing extra
		// to answer in that turn.
		It("Should join what folded in before the handover onto the prompt", func() {
			ch := servingChannel(opts, api, socket)

			socket.deliver(aMention().envelope())
			Eventually(socket.acked).Should(HaveLen(1))

			second := aMention()
			second.EnvelopeID = "Ev2"
			second.ThreadTS = "1700000000.000100"
			second.TS = "1700000001.000100"
			second.Text = "<@U0BOT> the disk one, not the memory one"

			socket.deliver(second.envelope())
			Eventually(socket.acked).Should(HaveLen(2))

			w := nextWork(ch)
			Expect(w.Prompt).To(Equal("what is eating disk on node3\nthe disk one, not the memory one"))
		})

		// Work.Caller, the Stop button and the next question each have exactly one owner,
		// which two people folded into one turn would not.
		It("Should queue somebody else's mention behind rather than folding it in", func() {
			ch := servingChannel(opts, api, socket)

			socket.deliver(aMention().envelope())
			Eventually(socket.acked).Should(HaveLen(1))

			first := nextWork(ch)

			other := aMention()
			other.EnvelopeID = "Ev2"
			other.User = "U2"
			other.ThreadTS = "1700000000.000100"
			other.TS = "1700000002.000100"
			other.Text = "<@U0BOT> while you are there, check node4"

			socket.deliver(other.envelope())
			Eventually(socket.acked).Should(HaveLen(2))

			noWork(ch)

			ended(first)

			w := nextWork(ch)
			Expect(w.Prompt).To(Equal("while you are there, check node4"))
			Expect(w.Caller.Name).To(Equal("T1/U2"))
			Expect(w.ID).To(Equal("C1/1700000002.000100"), "its own turn with its own id")
		})

		DescribeTable("Should not deliver the folded lines to a run that reached no user boundary",
			func(out serve.Outcome) {
				ch := servingChannel(opts, api, socket)

				socket.deliver(aMention().envelope())
				Eventually(socket.acked).Should(HaveLen(1))

				first := nextWork(ch)

				more := aMention()
				more.EnvelopeID = "Ev2"
				more.ThreadTS = "1700000000.000100"
				more.TS = "1700000003.000100"
				more.Text = "<@U0BOT> and node4 as well"

				socket.deliver(more.envelope())
				Eventually(socket.acked).Should(HaveLen(2))

				out.ID = first.ID
				Expect(first.Done(context.Background(), out)).To(Succeed())

				noWork(ch)

				var posted []fakeMessage
				Eventually(func() []fakeMessage { posted = api.messages(); return posted }).Should(HaveLen(1), "one message, not one per line")

				Expect(posted[0].Text).To(ContainSubstring("I did not get to"))
				Expect(posted[0].Text).To(ContainSubstring("and node4 as well"))
				Expect(posted[0].ThreadTS).To(Equal("1700000000.000100"))
			},
			// The prompt would be neither journaled nor answered, which is what
			// Outcome.FollowUpTaken reports and nothing here can repair.
			Entry("a run waiting on a deferred call", serve.Outcome{Reason: runstate.ReasonSuspended, Deferred: []agent.DeferredCall{{ToolUseID: "tu1"}}}),
			// Starting a fresh run seconds after somebody asked for a stop is not what
			// they asked for.
			Entry("a run that suspended", serve.Outcome{Reason: runstate.ReasonSuspended}),
		)
	})

	// A person told to come back is better served than one watching a queued message for
	// three minutes.
	It("Should refuse a mention once the backlog is full, saying so in the thread", func() {
		opts.MaxWaiting = 1

		ch := servingChannel(opts, api, socket)

		socket.deliver(aMention().envelope())
		Eventually(socket.acked).Should(HaveLen(1))

		full := aMention()
		full.EnvelopeID = "Ev2"
		full.Channel = "C2"
		full.TS = "1700000004.000100"
		full.Text = "<@U0BOT> and me"

		socket.deliver(full.envelope())
		Eventually(socket.acked).Should(Equal([]string{"Ev1", "Ev2"}), "a refusal is still an answer Slack is waiting for")

		var posted []fakeMessage
		Eventually(func() []fakeMessage { posted = api.messages(); return posted }).Should(HaveLen(1))

		Expect(posted[0].Text).To(Equal(backlogRefusal))
		Expect(posted[0].ChannelID).To(Equal("C2"))
		Expect(posted[0].ThreadTS).To(Equal("1700000004.000100"))

		w := nextWork(ch)
		Expect(w.ID).To(Equal("C1/1700000000.000100"), "the turn that was admitted is unaffected")

		noWork(ch)
	})

	// The in-flight entry is the only mutual exclusion between two resumes of one
	// conversation, so a thread whose turn never released it would answer nothing again.
	It("Should let a thread take another turn once the last one reported its outcome", func() {
		ch := servingChannel(opts, api, socket)

		socket.deliver(aMention().envelope())
		Eventually(socket.acked).Should(HaveLen(1))

		first := nextWork(ch)
		ended(first)

		next := aMention()
		next.EnvelopeID = "Ev2"
		next.ThreadTS = "1700000000.000100"
		next.TS = "1700000005.000100"
		next.Text = "<@U0BOT> ok do that then"

		socket.deliver(next.envelope())
		Eventually(socket.acked).Should(HaveLen(2))

		w := nextWork(ch)
		Expect(w.Prompt).To(Equal("ok do that then"))
		Expect(w.ID).To(Equal("C1/1700000005.000100"))
	})

	// A worker that exited while a refusal was in flight would leave the person who was
	// refused with nothing at all.
	It("Should not close while a message it started is still in flight", func() {
		release, arrivals := api.hold()

		opts.MaxWaiting = 1
		ch := newTestChannel(opts, api, socket)
		ch.start()
		Eventually(socket.ran).Should(BeClosed())

		socket.deliver(aMention().envelope())
		Eventually(socket.acked).Should(HaveLen(1))

		full := aMention()
		full.EnvelopeID = "Ev2"
		full.Channel = "C2"
		full.TS = "1700000004.000100"

		socket.deliver(full.envelope())
		Eventually(arrivals).Should(Receive())

		closed := make(chan error, 1)
		go func() { closed <- ch.Close() }()

		Consistently(closed, 100*time.Millisecond).ShouldNot(Receive())

		release()

		Eventually(closed).Should(Receive(BeNil()))
		Expect(api.messages()).To(HaveLen(1))
	})

	It("Should refuse a mention that arrives once it is draining", func() {
		ch := newTestChannel(opts, api, socket)
		ch.start()
		Eventually(socket.ran).Should(BeClosed())

		m := aMention()

		Expect(ch.admit(mentionFrom(m))).To(Equal(""))
		Expect(ch.Close()).To(Succeed())
		Expect(ch.admit(mentionFrom(m))).To(Equal(drainingRefusal))
	})
})

// mentionFrom decodes an event a spec wrote, for the specs that call admission directly
// rather than through the socket.
func mentionFrom(ev mentionEvent) *mention {
	GinkgoHelper()

	m, ok, err := mentionOf(ev.envelope(), "U0BOT")
	Expect(err).ToNot(HaveOccurred())
	Expect(ok).To(BeTrue())

	return m
}
