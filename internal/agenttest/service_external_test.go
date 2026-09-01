//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agenttest_test

import (
	"errors"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/serve"
)

var _ = Describe("Service", func() {
	It("Should implement a service that can fault", func() {
		var svc serve.Service = agenttest.NewService(GinkgoTB(), "a2a")
		Expect(svc.Name()).To(Equal("a2a"))

		_, faulting := svc.(serve.FaultingEndpoint)
		Expect(faulting).To(BeTrue())
	})

	It("Should count every Close rather than making the second one silent", func() {
		svc := agenttest.BuildService("a2a")

		Expect(svc.Closes()).To(Equal(0))
		Expect(svc.Close()).To(Succeed())
		Expect(svc.Close()).To(Succeed())
		Expect(svc.Closes()).To(Equal(2), "a drain and then a stop reach the service twice")
	})

	It("Should report a fault on the Faults channel", func() {
		svc := agenttest.BuildService("a2a")
		stopped := errors.New("the a2a service stopped")

		svc.Fault(stopped)

		Expect(svc.Faults()).To(Receive(MatchError(stopped)))
	})

	// The buffer holds one and Serve returns the first fault and ends. A blocking send
	// would park the spec goroutine until the suite timed out rather than fail an
	// assertion.
	It("Should drop a second fault rather than blocking the goroutine that reported it", func() {
		svc := agenttest.BuildService("a2a")

		reported := make(chan struct{})
		go func() {
			defer GinkgoRecover()

			svc.Fault(errors.New("first"))
			svc.Fault(errors.New("second"))
			close(reported)
		}()

		Eventually(reported).Should(BeClosed())
		Expect(svc.Faults()).To(Receive(MatchError("first")))
		Expect(svc.Faults()).ToNot(Receive(), "the second fault was dropped, not queued")
	})

	It("Should count closes reported from several goroutines at once", func() {
		const closers = 8

		svc := agenttest.BuildService("a2a")

		var wg sync.WaitGroup
		for i := 0; i < closers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer GinkgoRecover()

				Expect(svc.Close()).To(Succeed())
			}()
		}

		Eventually(svc.Closes).Should(Equal(closers))
		wg.Wait()
	})
})
