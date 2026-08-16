//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package serve_test

import (
	"context"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/serve"
)

var _ = Describe("Hosting a service", func() {
	var (
		ctx    context.Context
		cancel context.CancelFunc
	)

	BeforeEach(func() {
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		DeferCleanup(cancel)
	})

	newServer := func(channels []serve.Channel, services []serve.Service) *serve.Server {
		GinkgoHelper()

		srv, err := serve.New(serve.Options{
			Channels: channels,
			Services: services,
			Config:   servedConfig(),
			Logger:   quietLogger(),
		})
		Expect(err).ToNot(HaveOccurred())

		return srv
	}

	// A service produces no work, so there is no puller to end Serve. Without the hold
	// a worker serving only tools would return from Serve at once and its caller would
	// release the surface out from under whoever was calling it.
	It("Should hold Serve open for a server with no channels", func() {
		svc := agenttest.NewService(GinkgoTB(), "a2a")
		srv := newServer(nil, []serve.Service{svc})

		served := make(chan error, 1)
		go func() { served <- srv.Serve(ctx) }()

		Consistently(served, 100*time.Millisecond).ShouldNot(Receive(),
			"a service answers for as long as it is registered")

		Expect(srv.Drain()).To(Succeed())
		Eventually(served).Should(Receive(BeNil()))
		Expect(svc.Closes()).To(Equal(1))
		Expect(ctx.Err()).ToNot(HaveOccurred(), "the drain ended it rather than the context")
	})

	// A surface that has stopped answering cannot be restarted from here, and a worker
	// whose surfaces are gone keeps running while doing nothing, so the fault ends the
	// server and the error is what makes a supervisor restart the process.
	It("Should end Serve with the error when a hosted surface reports a fault", func() {
		svc := agenttest.NewService(GinkgoTB(), "a2a")
		srv := newServer(nil, []serve.Service{svc})

		served := make(chan error, 1)
		go func() { served <- srv.Serve(ctx) }()

		Consistently(served, 100*time.Millisecond).ShouldNot(Receive())

		svc.Fault(errors.New("the a2a service stopped"))

		Eventually(served).Should(Receive(MatchError(ContainSubstring("the a2a service stopped"))))
		Expect(svc.Closes()).To(Equal(1), "the fault drains the surfaces on its way out")
		Expect(ctx.Err()).ToNot(HaveOccurred(), "the fault ended it rather than the context")
	})

	It("Should end a held Serve when its context is canceled", func() {
		srv := newServer(nil, []serve.Service{agenttest.NewService(GinkgoTB(), "a2a")})

		served := make(chan error, 1)
		go func() { served <- srv.Serve(ctx) }()

		cancel()
		Eventually(served).Should(Receive(BeNil()))
	})

	// The hold is for a server that has nothing else to wait on. A worker whose queue
	// engine died has to end so its supervisor hears about it, rather than staying up
	// answering tool calls and taking no jobs.
	It("Should end when its only channel is finished even though a service is hosted", func() {
		svc := agenttest.NewService(GinkgoTB(), "a2a")
		srv := newServer([]serve.Channel{agenttest.NewScriptedChannel(GinkgoTB(), "jobs")}, []serve.Service{svc})

		served := make(chan error, 1)
		go func() { served <- srv.Serve(ctx) }()

		Eventually(served).Should(Receive(BeNil()))
		Expect(svc.Closes()).To(Equal(0), "Serve returning is not what releases a surface")

		Expect(srv.Stop()).To(Succeed())
		Expect(svc.Closes()).To(Equal(1))
	})

	// A program draining on one signal and stopping on the next reaches every surface
	// twice. The service is what makes those two calls one, and the hold is closed once
	// however often it is released.
	It("Should survive a drain followed by a stop", func() {
		svc := agenttest.NewService(GinkgoTB(), "a2a")
		srv := newServer(nil, []serve.Service{svc})

		served := make(chan error, 1)
		go func() { served <- srv.Serve(ctx) }()

		Expect(srv.Drain()).To(Succeed())
		Eventually(served).Should(Receive(BeNil()))
		Expect(srv.Stop()).To(Succeed())

		Expect(svc.Closes()).To(Equal(2), "both calls reach the service; it is the service that makes them one")
	})

	Describe("Validation", func() {
		It("Should accept a server with a service and no channel", func() {
			srv, err := serve.New(serve.Options{
				Services: []serve.Service{agenttest.NewService(GinkgoTB(), "a2a")},
				Config:   servedConfig(),
				Logger:   quietLogger(),
			})
			Expect(err).ToNot(HaveOccurred())
			Expect(srv).ToNot(BeNil())
		})

		It("Should refuse a server with neither", func() {
			_, err := serve.New(serve.Options{Config: servedConfig(), Logger: quietLogger()})
			Expect(err).To(MatchError(ContainSubstring("at least one channel or service is required")))
		})

		It("Should refuse a nil or unnamed service", func() {
			_, err := serve.New(serve.Options{
				Services: []serve.Service{nil},
				Config:   servedConfig(),
				Logger:   quietLogger(),
			})
			Expect(err).To(MatchError(ContainSubstring("service 0 is nil")))

			_, err = serve.New(serve.Options{
				Services: []serve.Service{agenttest.NewService(GinkgoTB(), "")},
				Config:   servedConfig(),
				Logger:   quietLogger(),
			})
			Expect(err).To(MatchError(ContainSubstring("service 0 has no name")))
		})

		// A service is answering by the time it reaches here, so a refused set is a live
		// surface in a process that will serve nothing.
		It("Should release both kinds when it refuses its options", func() {
			queue := agenttest.NewQueue(GinkgoTB(), "jobs")
			svc := agenttest.NewService(GinkgoTB(), "a2a")

			_, err := serve.New(serve.Options{
				Channels: []serve.Channel{queue},
				Services: []serve.Service{svc},
				Logger:   quietLogger(),
			})
			Expect(err).To(MatchError(ContainSubstring("a configuration is required")))
			Expect(queue.Closes()).To(Equal(1))
			Expect(svc.Closes()).To(Equal(1))
		})
	})
})
