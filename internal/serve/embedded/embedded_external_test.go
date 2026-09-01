//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package embedded_test

import (
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/serve/embedded"
)

var _ = Describe("Broker", func() {
	start := func() *embedded.Broker {
		GinkgoHelper()

		broker, err := embedded.Start("test", nil)
		Expect(err).NotTo(HaveOccurred())

		return broker
	}

	// The observable half of "nothing outside the process can reach it": the connection
	// runs over the in-process pipe rather than a socket, so there is no address for
	// anything else to dial. The options behind that are asserted in-package.
	It("Should connect over the in-process pipe rather than a socket", func() {
		broker := start()
		DeferCleanup(func() { Expect(broker.Close()).To(Succeed()) })

		nc := broker.Conns().Nats()
		Expect(nc).NotTo(BeNil())
		Expect(nc.ConnectedAddr()).To(Equal("pipe"))
	})

	// The connection is what everything hosted here is given, so a message published on
	// it has to reach a subscriber on it.
	It("Should carry messages", func() {
		broker := start()
		DeferCleanup(func() { Expect(broker.Close()).To(Succeed()) })

		nc := broker.Conns().Nats()

		sub, err := nc.SubscribeSync("greeting")
		Expect(err).NotTo(HaveOccurred())

		Expect(nc.Publish("greeting", []byte("hello"))).To(Succeed())
		Expect(nc.Flush()).To(Succeed())

		msg, err := sub.NextMsg(time.Second)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(msg.Data)).To(Equal("hello"))
	})

	// The reason Close waits for the drain rather than only starting it: a run that has
	// just ended publishes its terminal message and the process closes immediately
	// after, and that message has to arrive.
	//
	// The handler sleeps, so the drain has to wait for the subscription's pending
	// messages to reach zero and cannot win the race by being quick. A volume of
	// unflushed publishes does not pin this: with the wait removed, 5000 of them still
	// all arrived in about 1 run in 20. Handler time is what a drain that only starts
	// cannot skip, and it measured 0 of 100 delivered on every run without the wait and
	// 100 of 100 on every run with it.
	It("Should deliver what a slow subscriber has not finished when it closes", func() {
		const count = 100

		broker := start()
		nc := broker.Conns().Nats()

		var delivered atomic.Int64
		_, err := nc.Subscribe("farewell", func(*nats.Msg) {
			time.Sleep(2 * time.Millisecond)
			delivered.Add(1)
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(nc.Flush()).To(Succeed())

		for range count {
			Expect(nc.Publish("farewell", []byte("run finished"))).To(Succeed())
		}

		Expect(broker.Close()).To(Succeed())
		Expect(delivered.Load()).To(BeEquivalentTo(count))
	})

	// Closing twice is what a deferred close and an explicit one add up to on an error
	// path. It must not panic or hang, and the second call must report what the first
	// one did rather than a fresh answer about an already closed connection.
	It("Should be safe to close twice and report the same outcome", func() {
		broker := start()

		first := broker.Close()
		Expect(first).To(Succeed())
		Expect(broker.Close()).To(Succeed())
	})

	// Close is the release verb, and every other one in this tree returns an error. A
	// caller that ignores it is choosing to; one that cannot see it has no choice.
	It("Should report the outcome of the drain", func() {
		broker := start()

		var closer interface{ Close() error } = broker
		Expect(closer.Close()).To(Succeed())
	})
})
