//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package multiplex

import (
	"context"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// deliveries records what reached the multiplexer, in the order it did.
type deliveries struct {
	mu   sync.Mutex
	sent []report

	// hold, when set, keeps each delivery open until it is closed, which is how a spec
	// posts a state while an earlier one is still being delivered.
	hold chan struct{}
	// started is closed by the first delivery.
	started chan struct{}
	// panics makes a delivery panic.
	panics bool

	released bool
}

func (d *deliveries) deliver(_ context.Context, rep report) error {
	d.mu.Lock()
	d.sent = append(d.sent, rep)
	first := len(d.sent) == 1
	hold := d.hold
	panics := d.panics
	d.mu.Unlock()

	if first && d.started != nil {
		close(d.started)
	}
	if hold != nil {
		<-hold
	}
	if panics {
		panic("the multiplexer went away")
	}

	return nil
}

// setPanics changes what the next delivery does, from a spec while the worker may be in
// one.
func (d *deliveries) setPanics(p bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.panics = p
}

func (d *deliveries) release(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.released = true
	d.sent = append(d.sent, report{state: "released"})

	return nil
}

func (d *deliveries) states() []string {
	d.mu.Lock()
	defer d.mu.Unlock()

	out := make([]string, 0, len(d.sent))
	for _, rep := range d.sent {
		out = append(out, string(rep.state))
	}

	return out
}

func (d *deliveries) last() report {
	d.mu.Lock()
	defer d.mu.Unlock()

	if len(d.sent) == 0 {
		return report{}
	}

	return d.sent[len(d.sent)-1]
}

var _ = Describe("Reporter", func() {
	var del *deliveries

	BeforeEach(func() {
		del = &deliveries{}
	})

	// A process that has just started is waiting on the person who started it, so the
	// pane says so rather than claiming work nobody has asked for yet.
	It("Should open the pane at idle", func() {
		r := newReporter("herdr", del.deliver, del.release)
		defer r.Close()

		Eventually(del.states).Should(HaveExactElements("idle"))
	})

	It("Should report each state it is told", func() {
		r := newReporter("herdr", del.deliver, del.release)
		defer r.Close()

		Eventually(del.states).Should(ContainElement("idle"))

		r.Working()
		Eventually(del.states).Should(ContainElement("working"))

		r.Blocked("run this?")
		Eventually(del.states).Should(ContainElement("blocked"))
		Expect(del.last().message).To(Equal("run this?"))
	})

	// The reason is model-supplied and herdr renders it in a list beside other panes, so
	// what goes is one plain line of a length a column can hold.
	It("Should sanitize and cut the reason of a block", func() {
		r := newReporter("herdr", del.deliver, del.release)
		defer r.Close()

		r.Blocked("drop \x1b[31mthe\x1b[0m table\nfor " + strings.Repeat("a", 200) + "?")
		Eventually(func() string { return del.last().message }).ShouldNot(BeEmpty())

		msg := del.last().message
		Expect(msg).ToNot(ContainSubstring("\x1b"), "no escape sequence reaches another program's screen")
		Expect(msg).ToNot(ContainSubstring("\n"))
		Expect(msg).To(HavePrefix("drop the table for "))
		Expect([]rune(msg)).To(HaveLen(reasonRunes+1), "cut, with the mark that says it was")
	})

	// A pane shows a state rather than a history, so a run that changes state faster than
	// the multiplexer can be told does not build a queue of states nobody will see.
	It("Should deliver the newest state and drop what it overtook", func() {
		del.hold = make(chan struct{})
		del.started = make(chan struct{})

		r := newReporter("herdr", del.deliver, del.release)

		// The first delivery is held open, so everything below queues behind it.
		Eventually(del.started).Should(BeClosed())

		r.Working()
		r.Blocked("run this?")
		r.Working()
		r.Idle()

		// Closing rather than clearing it: a closed channel lets every later delivery
		// through, and clearing the field would race the worker reading it.
		close(del.hold)
		Eventually(del.states).Should(HaveExactElements("idle", "idle"))
		Consistently(del.states).Should(HaveExactElements("idle", "idle"))

		r.Close()
	})

	Describe("Close", func() {
		// A report landing after the release would claim the pane again, and herdr would
		// show a state for a process that has exited.
		It("Should release last, with nothing after it", func() {
			del.hold = make(chan struct{})
			del.started = make(chan struct{})

			r := newReporter("herdr", del.deliver, del.release)
			Eventually(del.started).Should(BeClosed())

			r.Working()
			close(del.hold)
			r.Close()

			states := del.states()
			Expect(states[len(states)-1]).To(Equal("released"))
			Consistently(del.states).Should(HaveExactElements(states))
		})

		It("Should ignore what it is told after it", func() {
			r := newReporter("herdr", del.deliver, del.release)
			Eventually(del.states).Should(ContainElement("idle"))

			r.Close()
			r.Working()
			r.Blocked("run this?")

			Consistently(del.states).ShouldNot(ContainElement("working"))
		})

		It("Should release once however often it is called", func() {
			r := newReporter("herdr", del.deliver, del.release)
			r.Close()
			r.Close()

			released := 0
			for _, s := range del.states() {
				if s == "released" {
					released++
				}
			}
			Expect(released).To(Equal(1))
		})
	})

	// The worker runs beside a full-screen view holding the terminal in raw mode, so a
	// panic on it would leave a terminal nobody can type into.
	It("Should survive a delivery that panics", func() {
		del.panics = true

		r := newReporter("herdr", del.deliver, del.release)
		defer r.Close()

		r.Working()
		Eventually(del.states).Should(ContainElement("working"))

		del.setPanics(false)
		r.Blocked("still here?")
		Eventually(del.states).Should(ContainElement("blocked"), "the worker kept going")
	})

	It("Should not make the run wait for a slow multiplexer", func() {
		del.hold = make(chan struct{})
		del.started = make(chan struct{})

		r := newReporter("herdr", del.deliver, del.release)
		Eventually(del.started).Should(BeClosed())

		done := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			defer close(done)

			r.Working()
			r.Blocked("run this?")
			r.Idle()
		}()

		Eventually(done, time.Second).Should(BeClosed())

		close(del.hold)
		r.Close()
	})
})
