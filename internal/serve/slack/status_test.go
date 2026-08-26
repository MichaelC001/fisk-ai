//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package slack

import (
	"context"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	slackgo "github.com/slack-go/slack"
)

// testClock is the time a limiter measures with when a spec is driving it, so a throttle
// is asserted on rather than waited for.
type testClock struct {
	mu sync.Mutex

	at time.Time
	// released stops the clock holding anybody, which is what lets a channel close while
	// its status messages are parked on an allowance that will never refill on its own.
	released bool
	waiters  []*clockWaiter
}

// clockWaiter is one caller parked until the clock reaches due.
type clockWaiter struct {
	due time.Time
	c   chan time.Time
}

func newTestClock() *testClock {
	return &testClock{at: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.at
}

func (c *testClock) after(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Buffered by one so nothing waiting on this clock has to be read from for the
	// clock to move.
	out := make(chan time.Time, 1)

	if c.released {
		// A released clock runs freely, so a caller asking to wait an hour has an hour
		// credited to it: a limiter that woke to a clock standing still would find its
		// bucket as empty as it left it and ask for the same wait again.
		c.at = c.at.Add(d)
		out <- c.at

		return out
	}

	c.waiters = append(c.waiters, &clockWaiter{due: c.at.Add(d), c: out})

	return out
}

// advance moves time on and wakes whatever was waiting for it.
func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.at = c.at.Add(d)
	c.fireLocked()
}

// waiting is how many callers are parked, which is how a spec knows a throttle has taken
// hold before it moves time: a spec that advanced first would leave the next caller
// measuring its wait from the time it had already been given.
func (c *testClock) waiting() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.waiters)
}

// release lets everything through, now and from now on.
//
// Time moves on here and keeps moving in after, because a limiter that woke on a clock that
// stood still would find its bucket as empty as it left it and wait again. A drain writes
// the ending of every turn that never ran, so there is something spending tokens at the end
// of every spec that uses this.
func (c *testClock) release() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.released = true
	c.at = c.at.Add(24 * time.Hour)
	c.fireLocked()
}

func (c *testClock) fireLocked() {
	kept := c.waiters[:0]

	for _, w := range c.waiters {
		if c.released || !w.due.After(c.at) {
			w.c <- w.due

			continue
		}

		kept = append(kept, w)
	}

	c.waiters = kept
}

// roomyChannel serves a channel whose allowance never holds anything up, for the specs
// that assert on what a status message says rather than on what got through.
func roomyChannel(opts Options, a api, s *fakeSocket) *Channel {
	GinkgoHelper()

	ch := newTestChannel(opts, a, s)
	ch.limit = newLimiter(time.Hour, 1000, newTestClock())
	ch.start()

	Eventually(s.ran).Should(BeClosed())
	DeferCleanup(func() { Expect(ch.Close()).To(Succeed()) })

	return ch
}

// throttledChannel serves a channel whose allowance the spec spends and refills itself.
//
// The clock is released before the channel is closed, Ginkgo running cleanups in reverse:
// a publisher parked on an allowance nobody will refill would otherwise hold the drain
// until its own deadline.
func throttledChannel(opts Options, a api, s *fakeSocket, cl *testClock, interval time.Duration, burst int) *Channel {
	GinkgoHelper()

	ch := newTestChannel(opts, a, s)
	ch.limit = newLimiter(interval, burst, cl)
	ch.start()

	Eventually(s.ran).Should(BeClosed())
	DeferCleanup(func() { Expect(ch.Close()).To(Succeed()) })
	DeferCleanup(cl.release)

	return ch
}

// pressingStop is the press of the Stop button on one status message, built from what that
// message is carrying rather than from a value a spec wrote.
func pressingStop(m *fakeMessage, user string) pressEvent {
	GinkgoHelper()

	for _, b := range m.Buttons {
		if b.ActionID != stopActionID {
			continue
		}

		return pressEvent{
			Team:      "T1",
			Channel:   m.ChannelID,
			ThreadTS:  m.ThreadTS,
			MessageTS: m.TS,
			User:      user,
			Value:     b.Value,
		}
	}

	Fail("the message carries no Stop button")

	return pressEvent{}
}

// statusIn is this bot's message in one channel, which for these specs is the status
// message, and nil until it has been posted.
func statusIn(a *fakeAPI, channelID string) func() *fakeMessage {
	return func() *fakeMessage {
		msgs := a.messages()

		for i := range msgs {
			if msgs[i].ChannelID == channelID {
				return &msgs[i]
			}
		}

		return nil
	}
}

// statusOf is the status message of the turn holding a thread, which is what the events
// sink drives once a run is reporting into it.
func statusOf(ch *Channel, session string) *status {
	ch.mu.Lock()
	defer ch.mu.Unlock()

	t := ch.inFlight[session]
	Expect(t).ToNot(BeNil(), "the thread is holding no turn")

	return t.status
}

// textIn is what this bot's message in one channel says, empty where it has posted none,
// so a spec waits for a status message rather than for a count.
func textIn(a *fakeAPI, channelID string) func() string {
	return func() string {
		for _, m := range a.messages() {
			if m.ChannelID == channelID {
				return m.Text
			}
		}

		return ""
	}
}

// editsIn is every text one channel's message has been given since it was posted.
func editsIn(a *fakeAPI, channelID string) func() []string {
	return func() []string {
		for _, m := range a.messages() {
			if m.ChannelID == channelID {
				return m.Edits
			}
		}

		return nil
	}
}

var _ = Describe("The status message", func() {
	var (
		api     *fakeAPI
		socket  *fakeSocket
		opts    Options
		session string
	)

	BeforeEach(func() {
		api = newFakeAPI()
		socket = newFakeSocket()
		opts = testOptions()
		session = SessionFor(opts.Identity, "T1", "C1", "1700000000.000100")
	})

	// The hint is a family rather than a call: everybody in the channel reads the thread,
	// and which tool is being run against what is not theirs to publish.
	DescribeTable("Should name the family a tool belongs to rather than the tool",
		func(tool string, expected string) {
			Expect(hintFor(tool)).To(Equal(expected))
		},
		Entry("a memory tool", "memory_write", hintMemory),
		Entry("another memory tool", "memory_search", hintMemory),
		Entry("a knowledge search", "knowledge_search", hintKnowledge),
		Entry("a knowledge enumeration", "knowledge_enumerate", hintKnowledge),
		Entry("anything else", "restart_node", hintTools),
		Entry("a tool named after nothing in particular", "", hintTools),
	)

	// A thread nobody has read for an hour is scrolled through rather than read, so the
	// state of every turn in it has to survive being glanced at. The shortcodes are written
	// out here rather than built from the constants, since what a person sees is the emoji
	// Slack renders from them.
	DescribeTable("Should open the line with the emoji for the state the turn is in",
		func(reach func(s *status), expected string) {
			s := &status{}
			reach(s)

			s.mu.Lock()
			defer s.mu.Unlock()

			text, icon := s.lineLocked()
			Expect(statusText(icon, text)).To(Equal(expected))
		},
		Entry("waiting for a worker", func(s *status) { s.queued = true }, ":hourglass_flowing_sand: Queued..."),
		Entry("a run that has reported nothing yet", func(s *status) {}, ":thinking_face: Thinking..."),
		Entry("between tool calls", func(s *status) { s.note(hintThinking) }, ":thinking_face: Thinking..."),
		Entry("in the memory tools", func(s *status) { s.note(hintMemory) }, ":brain: Accessing memory..."),
		Entry("in the knowledge tools", func(s *status) { s.note(hintKnowledge) }, ":books: Searching knowledge..."),
		Entry("in any other tool", func(s *status) { s.note(hintTools) }, ":hammer: Calling tools..."),
		Entry("waiting for a person to answer", func(s *status) { s.asking(true) }, ":question: Waiting for an answer..."),
		Entry("waiting for a person after the turn deferred", func(s *status) { s.ends(deferredNote, emojiAsking) },
			":question: Waiting for your answer."),
		// The emoji goes in front of the whole line, so the link markup an answer ends on is
		// still a link.
		Entry("answered", func(s *status) {
			s.ends("Done: <https://example.slack.com/archives/C1/p17|see the answer>", emojiAnswered)
		},
			":white_check_mark: Done: <https://example.slack.com/archives/C1/p17|see the answer>"),
		Entry("stopped", func(s *status) { s.ends(stoppedNote, emojiStopped) },
			":octagonal_sign: Stopped. Mention me in this thread to carry on."),
		Entry("failed", func(s *status) { s.ends(crashedNote, emojiFailed) },
			":x: Something went wrong on my side. Mention me again to try it, and the worker log has the detail."),
	)

	// The text argument is what a notification and a client that renders no blocks show, and
	// a person reading either of those is the one furthest from the thread.
	It("Should carry the emoji in the blocks and in the text argument alike", func() {
		s := &status{}
		s.note(hintTools)

		s.mu.Lock()
		msg := s.stateLocked()
		s.mu.Unlock()

		Expect(msg.Text).To(Equal(":hammer: Calling tools..."), "postBlocks gives this to Slack as the text argument")

		section, ok := blocksOf(msg)[0].(*slackgo.SectionBlock)
		Expect(ok).To(BeTrue())
		Expect(section.Text.Text).To(Equal(msg.Text), "the blocks and the text argument are set from one string")
		Expect(section.Text.Type).To(Equal(slackgo.MarkdownType), "Slack renders a shortcode in mrkdwn")
	})

	It("Should post one message when the turn is admitted and edit that message as the run reports", func() {
		ch := roomyChannel(opts, api, socket)

		socket.deliver(aMention().envelope())
		Eventually(socket.acked).Should(HaveLen(1))

		Eventually(textIn(api, "C1")).Should(Equal(statusText(emojiThinking, hintThinking)), "a turn with a worker to start on is thinking rather than queued")
		Expect(api.messages()[0].ThreadTS).To(Equal("1700000000.000100"))

		s := statusOf(ch, session)

		s.note(hintFor("memory_search"))
		Eventually(textIn(api, "C1")).Should(Equal(statusText(emojiMemory, hintMemory)))

		s.note(hintFor("knowledge_search"))
		Eventually(textIn(api, "C1")).Should(Equal(statusText(emojiKnowledge, hintKnowledge)))

		s.note(hintFor("restart_node"))
		Eventually(textIn(api, "C1")).Should(Equal(statusText(emojiTools, hintTools)))

		Expect(api.messages()).To(HaveLen(1), "one message per turn, edited in place")
	})

	// Two minutes of the same string reads as a hang. The count is movement without
	// saying what is moving.
	It("Should count a hint that repeats and start again when it changes", func() {
		ch := roomyChannel(opts, api, socket)

		socket.deliver(aMention().envelope())
		Eventually(textIn(api, "C1")).Should(Equal(statusText(emojiThinking, hintThinking)))

		s := statusOf(ch, session)

		s.note(hintTools)
		Eventually(textIn(api, "C1")).Should(Equal(":hammer: Calling tools..."))

		s.note(hintTools)
		Eventually(textIn(api, "C1")).Should(Equal(":hammer: Calling tools... (2)"))

		s.note(hintTools)
		Eventually(textIn(api, "C1")).Should(Equal(":hammer: Calling tools... (3)"))

		s.note(hintMemory)
		Eventually(textIn(api, "C1")).Should(Equal(":brain: Accessing memory..."), "a different hint counts from one again")
	})

	// The first thing a run reports is that it started, and a turn already reading as
	// thinking has nothing to say about that.
	It("Should spend no call moving from the state it was posted in to the first hint", func() {
		ch := roomyChannel(opts, api, socket)

		socket.deliver(aMention().envelope())
		Eventually(textIn(api, "C1")).Should(Equal(statusText(emojiThinking, hintThinking)))

		statusOf(ch, session).note(hintThinking)

		Consistently(editsIn(api, "C1"), 100*time.Millisecond).Should(BeEmpty())
	})

	It("Should post nothing at all where progress is turned off", func() {
		opts.Progress = false

		ch := roomyChannel(opts, api, socket)

		socket.deliver(aMention().envelope())
		Eventually(socket.acked).Should(HaveLen(1))

		w := nextWork(ch)
		Expect(w.Prompt).To(Equal("<@U1>: what is eating disk on node3"))

		Expect(statusOf(ch, session)).To(BeNil())
		Consistently(api.messages, 100*time.Millisecond).Should(BeEmpty())
	})

	// A person watching a thread that says nothing cannot tell a bot that is busy from
	// one that is broken.
	It("Should show a queued line for a turn admitted with no worker free, until its run starts", func() {
		opts.Workers = 1

		ch := roomyChannel(opts, api, socket)

		socket.deliver(aMention().envelope())
		Eventually(socket.acked).Should(HaveLen(1))

		second := aMention()
		second.EnvelopeID = "Ev2"
		second.Channel = "C2"
		second.TS = "1700000009.000100"
		second.Text = "<@U0BOT> and my one"

		socket.deliver(second.envelope())
		Eventually(socket.acked).Should(HaveLen(2))

		Eventually(textIn(api, "C1")).Should(Equal(statusText(emojiThinking, hintThinking)))
		Eventually(textIn(api, "C2")).Should(Equal(statusText(emojiQueued, hintQueued)))

		// The first turn takes the worker and the second is handed over behind it, which
		// is not the same as running: serve's puller takes an item before a slot frees.
		nextWork(ch)
		queued := nextWork(ch)

		Consistently(textIn(api, "C2"), 100*time.Millisecond).Should(Equal(statusText(emojiQueued, hintQueued)))

		runCtx, cancel := queued.RunContext(context.Background())
		Expect(runCtx).ToNot(BeNil())
		Expect(cancel).To(BeNil(), "this channel cancels no run of its own")

		Eventually(textIn(api, "C2")).Should(Equal(statusText(emojiThinking, hintThinking)))
	})

	Describe("The workspace's allowance", func() {
		const interval = time.Hour

		var clock *testClock

		BeforeEach(func() {
			clock = newTestClock()
		})

		// The state a turn ended in is what item 8 turns into a link to the answer, so a
		// dropped last edit is a message that says Thinking... forever.
		It("Should skip the hints a run passed through and still write the one it reached", func() {
			ch := throttledChannel(opts, api, socket, clock, interval, 1)

			socket.deliver(aMention().envelope())
			Eventually(textIn(api, "C1")).Should(Equal(statusText(emojiThinking, hintThinking)), "the one call the bucket held")

			s := statusOf(ch, session)

			s.note(hintTools)
			Eventually(clock.waiting).Should(Equal(1))

			s.note(hintMemory)
			s.note(hintKnowledge)

			Consistently(editsIn(api, "C1"), 100*time.Millisecond).Should(BeEmpty())

			clock.advance(interval)

			Eventually(editsIn(api, "C1")).Should(Equal([]string{statusText(emojiKnowledge, hintKnowledge)}),
				"where the run is, in one call, rather than every state it passed through")
		})

		// Tier 3 is roughly fifty calls a minute for the app across the whole workspace,
		// so a second turn is not entitled to an allowance of its own.
		It("Should hold every turn to one bucket rather than one each", func() {
			throttledChannel(opts, api, socket, clock, interval, 1)

			socket.deliver(aMention().envelope())

			other := aMention()
			other.EnvelopeID = "Ev2"
			other.Channel = "C2"
			other.TS = "1700000009.000100"
			other.Text = "<@U0BOT> mine too"

			socket.deliver(other.envelope())
			Eventually(socket.acked).Should(HaveLen(2))

			Eventually(api.messages).Should(HaveLen(1))
			Eventually(clock.waiting).Should(Equal(1), "the second turn waits for the same allowance the first spent")
			Consistently(api.messages, 100*time.Millisecond).Should(HaveLen(1))

			clock.advance(interval)

			Eventually(api.messages).Should(HaveLen(2))
		})
	})
})

var _ = Describe("The Stop button", func() {
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

	// The turn id is what the press is routed by. It reaches a run only after the team,
	// channel and thread on the envelope have been checked against the turn, so it is a name
	// rather than a capability.
	It("Should carry a button naming the turn and no call", func() {
		roomyChannel(opts, api, socket)

		socket.deliver(aMention().envelope())
		Eventually(textIn(api, "C1")).Should(Equal(statusText(emojiThinking, hintThinking)))

		msg := statusIn(api, "C1")()
		Expect(msg.Buttons).To(HaveLen(1))
		Expect(msg.Buttons[0].ActionID).To(Equal(stopActionID))
		Expect(msg.Buttons[0].Label).To(Equal(labelStop))

		v, err := decodeValue(msg.Buttons[0].Value)
		Expect(err).ToNot(HaveOccurred())
		Expect(v.Stop).To(Equal("C1/1700000000.000100"))
		Expect(v.ToolUse).To(BeEmpty(), "it answers no call, so nothing looks for a question under it")
		Expect(msg.Buttons[0].Value).ToNot(ContainSubstring(SessionFor(opts.Identity, "T1", "C1", "1700000000.000100")))
	})

	It("Should take the button off when the turn ends", func() {
		ch := roomyChannel(opts, api, socket)

		socket.deliver(aMention().envelope())
		Eventually(textIn(api, "C1")).Should(Equal(statusText(emojiThinking, hintThinking)))

		msg := statusIn(api, "C1")()
		Expect(msg.Buttons).To(HaveLen(1))

		ended(nextWork(ch))

		Eventually(buttonsOf(api, msg.TS)).Should(BeEmpty(), "a turn that has ended is not one anybody can park")
	})

	It("Should make the run report a suspend at its next boundary", func() {
		ch := roomyChannel(opts, api, socket)

		socket.deliver(aMention().envelope())
		Eventually(textIn(api, "C1")).Should(Equal(statusText(emojiThinking, hintThinking)))

		w := nextWork(ch)
		Expect(w.SuspendRequested()).To(BeFalse())

		socket.deliver(pressingStop(statusIn(api, "C1")(), "U2").envelope())
		Eventually(socket.acked).Should(HaveLen(2))

		Eventually(w.SuspendRequested).Should(BeTrue())
	})

	// Stopping means a conversation somebody can carry on with rather than a turn that died
	// half done, so the run finishes the step in hand and parks where a resume picks it up.
	It("Should leave the run's context alone", func() {
		ch := roomyChannel(opts, api, socket)

		socket.deliver(aMention().envelope())
		Eventually(textIn(api, "C1")).Should(Equal(statusText(emojiThinking, hintThinking)))

		w := nextWork(ch)

		runCtx, cancel := w.RunContext(context.Background())
		Expect(cancel).To(BeNil(), "this channel cancels no run of its own")

		socket.deliver(pressingStop(statusIn(api, "C1")(), "U2").envelope())
		Eventually(w.SuspendRequested).Should(BeTrue())

		Expect(runCtx.Err()).To(BeNil())
	})

	// Whoever can see the thread can press it, which is who may answer a question there.
	It("Should take a press from somebody other than the person who asked", func() {
		ch := roomyChannel(opts, api, socket)

		socket.deliver(aMention().envelope())
		Eventually(textIn(api, "C1")).Should(Equal(statusText(emojiThinking, hintThinking)))

		w := nextWork(ch)

		socket.deliver(pressingStop(statusIn(api, "C1")(), "U9").envelope())

		Eventually(w.SuspendRequested).Should(BeTrue())
	})

	It("Should ask for nothing further on a second press", func() {
		ch := roomyChannel(opts, api, socket)

		socket.deliver(aMention().envelope())
		Eventually(textIn(api, "C1")).Should(Equal(statusText(emojiThinking, hintThinking)))

		w := nextWork(ch)
		msg := statusIn(api, "C1")()

		socket.deliver(pressingStop(msg, "U2").envelope())
		Eventually(w.SuspendRequested).Should(BeTrue())

		socket.deliver(pressingStop(msg, "U9").envelope())
		Eventually(socket.acked).Should(HaveLen(3))

		Expect(w.SuspendRequested()).To(BeTrue())
		Consistently(api.messages, 100*time.Millisecond).Should(HaveLen(1), "the status message, and nothing said about the second press")
		noWork(ch)
	})

	// A turn queued behind another in its thread has a status message from the moment it is
	// admitted, so it has a button from then too. Pressing it parks the run at the first
	// boundary it reaches.
	It("Should park a turn that was still queued when it was pressed", func() {
		opts.Workers = 1

		ch := roomyChannel(opts, api, socket)

		socket.deliver(aMention().envelope())
		Eventually(socket.acked).Should(HaveLen(1))

		second := aMention()
		second.EnvelopeID = "Ev2"
		second.Channel = "C2"
		second.TS = "1700000009.000100"
		second.Text = "<@U0BOT> and my one"

		socket.deliver(second.envelope())
		Eventually(textIn(api, "C2")).Should(Equal(statusText(emojiQueued, hintQueued)))

		socket.deliver(pressingStop(statusIn(api, "C2")(), "U2").envelope())
		Eventually(socket.acked).Should(HaveLen(3))

		nextWork(ch)
		queued := nextWork(ch)

		Expect(queued.ID).To(Equal("C2/1700000009.000100"))
		Eventually(queued.SuspendRequested).Should(BeTrue())
	})

	Describe("A press this worker cannot act on", func() {
		It("Should tell whoever pressed that the turn has already finished", func() {
			ch := roomyChannel(opts, api, socket)

			socket.deliver(aMention().envelope())
			Eventually(textIn(api, "C1")).Should(Equal(statusText(emojiThinking, hintThinking)))

			msg := statusIn(api, "C1")()
			ended(nextWork(ch))
			Eventually(buttonsOf(api, msg.TS)).Should(BeEmpty())

			// The message a person is looking at is the one they had before the edit, buttons
			// and all, which is also what a stale message left by a worker that crashed is.
			socket.deliver(pressingStop(msg, "U2").envelope())

			Eventually(postedLine(api, stalePressRefusal)).Should(BeTrue())
		})

		It("Should tell whoever pressed that a turn it never held is not running", func() {
			roomyChannel(opts, api, socket)

			value, err := encodeValue(buttonValue{Stop: "C1/1699999999.000100"})
			Expect(err).ToNot(HaveOccurred())

			socket.deliver(pressEvent{
				Team:      "T1",
				Channel:   "C1",
				ThreadTS:  "1700000000.000100",
				MessageTS: "1699999999.000200",
				User:      "U2",
				Value:     value,
			}.envelope())

			Eventually(postedLine(api, stalePressRefusal)).Should(BeTrue())
		})

		// The conversation comes from the envelope, so a value presented against a thread it
		// was not minted in reaches no run.
		It("Should reach no run for a value presented against another thread", func() {
			ch := roomyChannel(opts, api, socket)

			socket.deliver(aMention().envelope())
			Eventually(textIn(api, "C1")).Should(Equal(statusText(emojiThinking, hintThinking)))

			w := nextWork(ch)

			elsewhere := pressingStop(statusIn(api, "C1")(), "U2")
			elsewhere.Channel = "C2"
			elsewhere.ThreadTS = "1700000030.000100"

			socket.deliver(elsewhere.envelope())

			Eventually(postedLine(api, stalePressRefusal)).Should(BeTrue())
			Expect(w.SuspendRequested()).To(BeFalse())
		})
	})

	It("Should leave no button to press where progress is turned off", func() {
		opts.Progress = false

		ch := roomyChannel(opts, api, socket)

		socket.deliver(aMention().envelope())
		Eventually(socket.acked).Should(HaveLen(1))

		w := nextWork(ch)
		Expect(w.SuspendRequested()).To(BeFalse())

		Consistently(api.messages, 100*time.Millisecond).Should(BeEmpty(), "no status message, so nothing carrying a Stop button")
	})

	// A press names a turn and no call, so nothing looks for a question under it and nobody
	// is told about one this worker does not hold.
	It("Should read a value that names a turn rather than a call", func() {
		v, err := decodeValue(`{"stop":"C1/1700000000.000100"}`)
		Expect(err).ToNot(HaveOccurred())
		Expect(v.Stop).To(Equal("C1/1700000000.000100"))
		Expect(v.Kind).To(BeEmpty())

		_, err = decodeValue(`{"kind":"confirm"}`)
		Expect(err).To(MatchError(ContainSubstring("without the call it answers")))
	})
})
