//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package serve_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/serve"
)

// A drain needs a channel that stays open until it is told to stop, which is why these
// specs use the fake queue rather than a scripted list: a channel reporting it is
// finished the moment its list is spent has already ended Serve before there is
// anything to drain.
var _ = Describe("Draining", func() {
	var (
		ctx    context.Context
		cancel context.CancelFunc
	)

	BeforeEach(func() {
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		DeferCleanup(cancel)
	})

	newServer := func(q *agenttest.Queue) *serve.Server {
		GinkgoHelper()

		srv, err := serve.New(serve.Options{
			Channels:    []serve.Channel{q},
			Config:      servedConfig(),
			Provider:    agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("ok")),
			Concurrency: 1,
			Logger:      quietLogger(),
		})
		Expect(err).ToNot(HaveOccurred())

		return srv
	}

	// The whole point of a drain over a cancellation: Serve ends on its own, and the run
	// that was already going runs to its natural end rather than being interrupted at
	// whatever point the signal arrived.
	It("Should end Serve by itself and let the run in flight finish", func() {
		q := agenttest.NewQueue(GinkgoTB(), "jobs")

		started := make(chan struct{})
		release := make(chan struct{})
		q.Submit(&serve.Work{ID: "first", Prompt: "go", Events: newStartProbe(func() {
			close(started)
			<-release
		})})

		srv := newServer(q)

		served := make(chan error, 1)
		go func() { served <- srv.Serve(ctx) }()

		Eventually(started).Should(BeClosed())
		Expect(srv.Drain()).To(Succeed())

		Consistently(served, 100*time.Millisecond).ShouldNot(Receive(),
			"the run in flight is waited for rather than abandoned")

		close(release)
		Eventually(served).Should(Receive(BeNil()))

		Expect(ctx.Err()).ToNot(HaveOccurred(),
			"Serve returned because the channels were drained, not because it was canceled")

		outcomes := q.Outcomes()
		Expect(outcomes).To(HaveLen(1))
		Expect(outcomes[0].ID).To(Equal("first"))
		Expect(outcomes[0].Reason).To(Equal(runstate.ReasonCompleted))
		Expect(outcomes[0].Abandoned).To(BeFalse())
		Expect(outcomes[0].Err).ToNot(HaveOccurred())
	})

	// The other half of the contract: once drained, the server stops taking work. What
	// is left on the queue is left there for another worker rather than claimed and
	// dropped.
	It("Should stop taking work once it is drained", func() {
		q := agenttest.NewQueue(GinkgoTB(), "jobs")

		ran := make(chan struct{})
		q.Submit(&serve.Work{ID: "first", Prompt: "go", Events: newStartProbe(func() { close(ran) })})

		srv := newServer(q)

		served := make(chan error, 1)
		go func() { served <- srv.Serve(ctx) }()

		Eventually(ran).Should(BeClosed())

		// An empty queue does not end anything: the puller is blocked in Next waiting
		// for more work, which is what a worker against a real queue is doing almost all
		// of the time. The drain is the only thing that ends it.
		Expect(srv.Drain()).To(Succeed())
		Eventually(served).Should(Receive(BeNil()))

		q.Submit(&serve.Work{ID: "second", Prompt: "go"})
		Expect(q.Pending()).To(Equal(1))
		Expect(q.Outcomes()).To(HaveLen(1), "nothing was reported for work that was never taken")
	})

	// A program draining on one signal and stopping on the next releases every channel
	// twice, so a channel that cannot tolerate the second call turns an orderly shutdown
	// into an error.
	It("Should survive a drain followed by a stop", func() {
		q := agenttest.NewQueue(GinkgoTB(), "jobs")
		srv := newServer(q)

		served := make(chan error, 1)
		go func() { served <- srv.Serve(ctx) }()

		Expect(srv.Drain()).To(Succeed())
		Eventually(served).Should(Receive(BeNil()))
		Expect(srv.Stop()).To(Succeed())

		Expect(q.Closes()).To(Equal(2), "both calls reach the channel; it is the channel that makes them one")
	})
})
