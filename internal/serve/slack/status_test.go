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
func (c *testClock) release() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.released = true
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

	It("Should post one message when the turn is admitted and edit that message as the run reports", func() {
		ch := roomyChannel(opts, api, socket)

		socket.deliver(aMention().envelope())
		Eventually(socket.acked).Should(HaveLen(1))

		Eventually(textIn(api, "C1")).Should(Equal(hintThinking), "a turn with a worker to start on is thinking rather than queued")
		Expect(api.messages()[0].ThreadTS).To(Equal("1700000000.000100"))

		s := statusOf(ch, session)

		s.note(hintFor("memory_search"))
		Eventually(textIn(api, "C1")).Should(Equal(hintMemory))

		s.note(hintFor("knowledge_search"))
		Eventually(textIn(api, "C1")).Should(Equal(hintKnowledge))

		s.note(hintFor("restart_node"))
		Eventually(textIn(api, "C1")).Should(Equal(hintTools))

		Expect(api.messages()).To(HaveLen(1), "one message per turn, edited in place")
	})

	// Two minutes of the same string reads as a hang. The count is movement without
	// saying what is moving.
	It("Should count a hint that repeats and start again when it changes", func() {
		ch := roomyChannel(opts, api, socket)

		socket.deliver(aMention().envelope())
		Eventually(textIn(api, "C1")).Should(Equal(hintThinking))

		s := statusOf(ch, session)

		s.note(hintTools)
		Eventually(textIn(api, "C1")).Should(Equal("Calling tools..."))

		s.note(hintTools)
		Eventually(textIn(api, "C1")).Should(Equal("Calling tools... (2)"))

		s.note(hintTools)
		Eventually(textIn(api, "C1")).Should(Equal("Calling tools... (3)"))

		s.note(hintMemory)
		Eventually(textIn(api, "C1")).Should(Equal("Accessing memory..."), "a different hint counts from one again")
	})

	// The first thing a run reports is that it started, and a turn already reading as
	// thinking has nothing to say about that.
	It("Should spend no call moving from the state it was posted in to the first hint", func() {
		ch := roomyChannel(opts, api, socket)

		socket.deliver(aMention().envelope())
		Eventually(textIn(api, "C1")).Should(Equal(hintThinking))

		statusOf(ch, session).note(hintThinking)

		Consistently(editsIn(api, "C1"), 100*time.Millisecond).Should(BeEmpty())
	})

	It("Should post nothing at all where progress is turned off", func() {
		opts.Progress = false

		ch := roomyChannel(opts, api, socket)

		socket.deliver(aMention().envelope())
		Eventually(socket.acked).Should(HaveLen(1))

		w := nextWork(ch)
		Expect(w.Prompt).To(Equal("what is eating disk on node3"))

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

		Eventually(textIn(api, "C1")).Should(Equal(hintThinking))
		Eventually(textIn(api, "C2")).Should(Equal(hintQueued))

		// The first turn takes the worker and the second is handed over behind it, which
		// is not the same as running: serve's puller takes an item before a slot frees.
		nextWork(ch)
		queued := nextWork(ch)

		Consistently(textIn(api, "C2"), 100*time.Millisecond).Should(Equal(hintQueued))

		runCtx, cancel := queued.RunContext(context.Background())
		Expect(runCtx).ToNot(BeNil())
		Expect(cancel).To(BeNil(), "this channel cancels no run of its own")

		Eventually(textIn(api, "C2")).Should(Equal(hintThinking))
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
			Eventually(textIn(api, "C1")).Should(Equal(hintThinking), "the one call the bucket held")

			s := statusOf(ch, session)

			s.note(hintTools)
			Eventually(clock.waiting).Should(Equal(1))

			s.note(hintMemory)
			s.note(hintKnowledge)

			Consistently(editsIn(api, "C1"), 100*time.Millisecond).Should(BeEmpty())

			clock.advance(interval)

			Eventually(editsIn(api, "C1")).Should(Equal([]string{hintKnowledge}),
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
