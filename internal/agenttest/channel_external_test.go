//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agenttest_test

import (
	"context"
	"errors"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/serve"
)

var _ = Describe("ScriptedChannel", func() {
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		DeferCleanup(cancel)
	})

	It("Should hand out its work in order and then report the channel done", func() {
		c := agenttest.NewScriptedChannel(GinkgoTB(), "scripted",
			&serve.Work{ID: "one", Prompt: "first"},
			&serve.Work{ID: "two", Prompt: "second"})

		Expect(c.Name()).To(Equal("scripted"))

		first, err := c.Next(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(first.ID).To(Equal("one"))

		second, err := c.Next(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(second.ID).To(Equal("two"))

		_, err = c.Next(ctx)
		Expect(err).To(MatchError(serve.ErrChannelDone))

		_, err = c.Next(ctx)
		Expect(err).To(MatchError(serve.ErrChannelDone), "a spent channel stays spent")
	})

	It("Should record every outcome in the order it was reported", func() {
		c := agenttest.NewScriptedChannel(GinkgoTB(), "scripted",
			&serve.Work{ID: "one"}, &serve.Work{ID: "two"})

		first, err := c.Next(ctx)
		Expect(err).ToNot(HaveOccurred())
		second, err := c.Next(ctx)
		Expect(err).ToNot(HaveOccurred())

		Expect(second.Done(ctx, serve.Outcome{ID: "two", Text: "later"})).To(Succeed())
		Expect(first.Done(ctx, serve.Outcome{ID: "one", Text: "earlier"})).To(Succeed())

		Expect(c.Outcomes()).To(HaveLen(2))
		Expect(c.Outcomes()[0].ID).To(Equal("two"))
		Expect(c.Outcomes()[1].ID).To(Equal("one"))
	})

	// The recorder appends the outcome and then calls the Done the work carried, so a spec
	// watching outcomes land through a Done of its own reads the outcome off Outcomes while
	// that Done is still running. The wrapped Done's error reaches the server.
	It("Should record an outcome before calling the Done the work already carried", func() {
		refused := errors.New("the caller could not be told")

		var (
			c        *agenttest.ScriptedChannel
			recorded []serve.Outcome
		)

		w := &serve.Work{ID: "one"}
		w.Done = func(_ context.Context, _ serve.Outcome) error {
			recorded = c.Outcomes()
			return refused
		}

		c = agenttest.NewScriptedChannel(GinkgoTB(), "scripted", w)

		next, err := c.Next(ctx)
		Expect(err).ToNot(HaveOccurred())

		Expect(next.Done(ctx, serve.Outcome{ID: "one", Text: "done"})).To(MatchError(refused))
		Expect(recorded).To(HaveLen(1), "the outcome was recorded before the wrapped Done ran")
		Expect(recorded[0].Text).To(Equal("done"))
		Expect(c.Outcomes()).To(HaveLen(1), "the recorder ran even though the wrapped Done failed")
	})

	It("Should refuse nil work by position", func() {
		c, err := agenttest.BuildScriptedChannel("scripted", &serve.Work{ID: "one"}, nil)
		Expect(c).To(BeNil())
		Expect(err).To(MatchError(ContainSubstring("scripted work 1 is nil")))
	})

	It("Should accept an empty name so a spec can drive a server's own name check", func() {
		c, err := agenttest.BuildScriptedChannel("")
		Expect(err).ToNot(HaveOccurred())
		Expect(c.Name()).To(BeEmpty())
	})

	// A scripted list holds nothing, so a server draining or stopping one has nothing to
	// release. A Close appearing here would take the untested shape away.
	It("Should not be releasable", func() {
		var c serve.Channel = agenttest.NewScriptedChannel(GinkgoTB(), "scripted")

		_, releasable := c.(serve.ReleasableChannel)
		Expect(releasable).To(BeFalse())
	})

	It("Should return a copy of the outcomes", func() {
		c := agenttest.NewScriptedChannel(GinkgoTB(), "scripted", &serve.Work{ID: "one"})

		w, err := c.Next(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(w.Done(ctx, serve.Outcome{ID: "one"})).To(Succeed())

		out := c.Outcomes()
		out[0].ID = "rewritten"

		Expect(c.Outcomes()[0].ID).To(Equal("one"))
	})

	It("Should record outcomes reported from several goroutines at once", func() {
		const runs = 8

		work := make([]*serve.Work, 0, runs)
		for i := 0; i < runs; i++ {
			work = append(work, &serve.Work{ID: "w"})
		}

		c := agenttest.NewScriptedChannel(GinkgoTB(), "scripted", work...)

		var wg sync.WaitGroup
		for i := 0; i < runs; i++ {
			w, err := c.Next(ctx)
			Expect(err).ToNot(HaveOccurred())

			wg.Add(1)
			go func() {
				defer wg.Done()
				defer GinkgoRecover()

				Expect(w.Done(ctx, serve.Outcome{ID: "w"})).To(Succeed())
			}()
		}

		// Outcomes is read while the reporters are still running, which is what makes it
		// usable with Eventually.
		Eventually(c.Outcomes).Should(HaveLen(runs))
		wg.Wait()
	})
})

var _ = Describe("Queue", func() {
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		DeferCleanup(cancel)
	})

	// Next is called on a goroutine because it blocks; the channel reports what it
	// returned so a spec can assert on both halves.
	type pull struct {
		work *serve.Work
		err  error
	}

	nextOn := func(q *agenttest.Queue, c context.Context) chan pull {
		pulled := make(chan pull, 1)
		go func() {
			defer GinkgoRecover()

			w, err := q.Next(c)
			pulled <- pull{work: w, err: err}
		}()

		return pulled
	}

	It("Should be releasable, which is what a drain reaches", func() {
		var c serve.Channel = agenttest.NewQueue(GinkgoTB(), "queue")

		_, releasable := c.(serve.ReleasableChannel)
		Expect(releasable).To(BeTrue())
	})

	It("Should block in Next until work is submitted", func() {
		q := agenttest.NewQueue(GinkgoTB(), "queue")
		Expect(q.Name()).To(Equal("queue"))

		pulled := nextOn(q, ctx)
		Consistently(pulled, 100*time.Millisecond).ShouldNot(Receive(), "an open queue with no work waits")

		Expect(q.Submit(&serve.Work{ID: "one"})).To(Succeed())

		var got pull
		Eventually(pulled).Should(Receive(&got))
		Expect(got.err).ToNot(HaveOccurred())
		Expect(got.work.ID).To(Equal("one"))
	})

	It("Should unblock a waiting Next on Close and stay done afterward", func() {
		q := agenttest.NewQueue(GinkgoTB(), "queue")

		pulled := nextOn(q, ctx)
		Consistently(pulled, 100*time.Millisecond).ShouldNot(Receive())

		Expect(q.Close()).To(Succeed())

		var got pull
		Eventually(pulled).Should(Receive(&got))
		Expect(got.err).To(MatchError(serve.ErrChannelDone))

		_, err := q.Next(ctx)
		Expect(err).To(MatchError(serve.ErrChannelDone))
	})

	It("Should return the context's error when the caller's context ends", func() {
		q := agenttest.NewQueue(GinkgoTB(), "queue")

		pullCtx, cancel := context.WithCancel(ctx)
		pulled := nextOn(q, pullCtx)
		Consistently(pulled, 100*time.Millisecond).ShouldNot(Receive())

		cancel()

		var got pull
		Eventually(pulled).Should(Receive(&got))
		Expect(got.err).To(MatchError(context.Canceled))
	})

	It("Should hold work submitted while nothing is serving", func() {
		q := agenttest.NewQueue(GinkgoTB(), "queue")

		Expect(q.Submit(&serve.Work{ID: "one"}, &serve.Work{ID: "two"})).To(Succeed())
		Expect(q.Pending()).To(Equal(2))

		w, err := q.Next(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(w.ID).To(Equal("one"))
		Expect(q.Pending()).To(Equal(1))
	})

	It("Should record nil work as a scripting fault and submit the rest of the batch", func() {
		q := agenttest.NewQueue(GinkgoTB(), "queue")

		err := q.Submit(&serve.Work{ID: "one"}, nil, &serve.Work{ID: "three"})
		Expect(err).To(MatchError(agenttest.ErrNotScripted))
		Expect(err).To(MatchError(ContainSubstring("work 1 is nil")))

		Expect(q.Pending()).To(Equal(2), "the real work in the batch was still submitted")

		faults := q.ScriptingFaults()
		Expect(faults).To(HaveLen(1))
		Expect(faults[0].Call).To(Equal("Submit"))
		Expect(faults[0].Subject).To(Equal("queue"))
		Expect(faults[0].Missing).To(Equal("work 1 is nil"))
	})

	It("Should return a copy of the scripting faults", func() {
		q := agenttest.NewQueue(GinkgoTB(), "queue")

		Expect(q.Submit(nil)).To(HaveOccurred())

		faults := q.ScriptingFaults()
		faults[0].Call = "rewritten"

		Expect(q.ScriptingFaults()[0].Call).To(Equal("Submit"))
	})

	It("Should accept work after Close and never deliver it", func() {
		q := agenttest.NewQueue(GinkgoTB(), "queue")

		Expect(q.Close()).To(Succeed())
		Expect(q.Submit(&serve.Work{ID: "one"})).To(Succeed())
		Expect(q.Pending()).To(Equal(1))

		_, err := q.Next(ctx)
		Expect(err).To(MatchError(serve.ErrChannelDone))
		Expect(q.Pending()).To(Equal(1), "a closed queue hands nothing over")
	})

	It("Should count every Close, including the second one", func() {
		q := agenttest.NewQueue(GinkgoTB(), "queue")

		Expect(q.Closes()).To(Equal(0))
		Expect(q.Close()).To(Succeed())
		Expect(q.Close()).To(Succeed())
		Expect(q.Closes()).To(Equal(2), "a drain and then a stop reach the channel twice")
	})

	It("Should record an outcome before calling the Done the work already carried", func() {
		q := agenttest.NewQueue(GinkgoTB(), "queue")

		var recorded []serve.Outcome

		w := &serve.Work{ID: "one"}
		w.Done = func(_ context.Context, _ serve.Outcome) error {
			recorded = q.Outcomes()
			return nil
		}

		Expect(q.Submit(w)).To(Succeed())

		next, err := q.Next(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(next.Done(ctx, serve.Outcome{ID: "one", Text: "done"})).To(Succeed())

		Expect(recorded).To(HaveLen(1), "the outcome was recorded before the wrapped Done ran")
		Expect(recorded[0].Text).To(Equal("done"))
		Expect(q.Outcomes()).To(HaveLen(1))
		Expect(q.Outcomes()[0].Text).To(Equal("done"))
	})

	It("Should serve one puller while several goroutines submit and report", func() {
		const items = 20

		q := agenttest.NewQueue(GinkgoTB(), "queue")

		var submitters sync.WaitGroup
		for i := 0; i < items; i++ {
			submitters.Add(1)
			go func() {
				defer submitters.Done()
				defer GinkgoRecover()

				Expect(q.Submit(&serve.Work{ID: "w"})).To(Succeed())
			}()
		}

		var reporters sync.WaitGroup
		for i := 0; i < items; i++ {
			w, err := q.Next(ctx)
			Expect(err).ToNot(HaveOccurred())

			reporters.Add(1)
			go func() {
				defer reporters.Done()
				defer GinkgoRecover()

				Expect(w.Done(ctx, serve.Outcome{ID: "w"})).To(Succeed())
			}()
		}

		submitters.Wait()
		Eventually(q.Outcomes).Should(HaveLen(items))
		reporters.Wait()

		Expect(q.Pending()).To(Equal(0))
		Expect(q.ScriptingFaults()).To(BeEmpty())
	})
})
