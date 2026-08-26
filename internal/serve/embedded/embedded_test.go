//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package embedded

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Broker", func() {
	// The claim this package makes is that nothing outside the process can reach it, so it
	// is asserted rather than trusted to the options being right.
	It("Should listen nowhere", func() {
		broker, err := Start("test", nil)
		Expect(err).NotTo(HaveOccurred())
		defer broker.Close()

		Expect(broker.server.Addr()).To(BeNil(), "a client port would put this agent on the network")
		Expect(broker.server.MonitorAddr()).To(BeNil())
		Expect(broker.server.ClusterAddr()).To(BeNil())
		Expect(broker.opts.LeafNode.Port).To(BeZero())
		Expect(broker.opts.Websocket.Port).To(BeZero())
		Expect(broker.opts.Gateway.Port).To(BeZero())

		Expect(broker.opts.DontListen).To(BeTrue())
		Expect(broker.opts.JetStream).To(BeFalse(), "there is no store here and nothing should think there is")
	})

	// A server that installed its own handler would take an interrupt away from the program
	// hosting it and exit the process, where an interrupt to a terminal running an agent
	// means stop the run at a boundary.
	It("Should leave signals to the process", func() {
		broker, err := Start("test", nil)
		Expect(err).NotTo(HaveOccurred())
		defer broker.Close()

		Expect(broker.opts.NoSigs).To(BeTrue())
		Expect(broker.opts.NoLog).To(BeTrue(), "a server writing to stdout would corrupt a piped run")
	})

	// The connection is what everything hosted here is given, so a message published on it
	// has to reach a subscriber on it.
	It("Should carry messages", func() {
		broker, err := Start("test", nil)
		Expect(err).NotTo(HaveOccurred())
		defer broker.Close()

		nc := broker.Conns().Nats()
		Expect(nc).NotTo(BeNil())

		sub, err := nc.SubscribeSync("greeting")
		Expect(err).NotTo(HaveOccurred())

		Expect(nc.Publish("greeting", []byte("hello"))).To(Succeed())
		Expect(nc.Flush()).To(Succeed())

		msg, err := sub.NextMsg(time.Second)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(msg.Data)).To(Equal("hello"))
	})

	// Closing twice is what a deferred close and an explicit one add up to on an error
	// path, and it must not panic or hang.
	It("Should be safe to close twice", func() {
		broker, err := Start("test", nil)
		Expect(err).NotTo(HaveOccurred())

		broker.Close()
		broker.Close()
	})
})
