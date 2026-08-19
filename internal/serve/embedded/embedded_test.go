//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package embedded

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"
)

// The claim this package makes is that nothing outside the process can reach it, so it
// is asserted rather than trusted to the options being right.
func TestStart_ListensNowhere(t *testing.T) {
	g := NewWithT(t)

	broker, err := Start("test", nil)
	g.Expect(err).NotTo(HaveOccurred())
	defer broker.Close()

	g.Expect(broker.server.Addr()).To(BeNil(), "a client port would put this agent on the network")
	g.Expect(broker.server.MonitorAddr()).To(BeNil())
	g.Expect(broker.server.ClusterAddr()).To(BeNil())
	g.Expect(broker.opts.LeafNode.Port).To(BeZero())
	g.Expect(broker.opts.Websocket.Port).To(BeZero())
	g.Expect(broker.opts.Gateway.Port).To(BeZero())

	g.Expect(broker.opts.DontListen).To(BeTrue())
	g.Expect(broker.opts.JetStream).To(BeFalse(), "there is no store here and nothing should think there is")
}

// A server that installed its own handler would take an interrupt away from the program
// hosting it and exit the process, where an interrupt to a terminal running an agent
// means stop the run at a boundary.
func TestStart_LeavesSignalsToTheProcess(t *testing.T) {
	g := NewWithT(t)

	broker, err := Start("test", nil)
	g.Expect(err).NotTo(HaveOccurred())
	defer broker.Close()

	g.Expect(broker.opts.NoSigs).To(BeTrue())
	g.Expect(broker.opts.NoLog).To(BeTrue(), "a server writing to stdout would corrupt a piped run")
}

// The connection is what everything hosted here is given, so a message published on it
// has to reach a subscriber on it.
func TestStart_CarriesMessages(t *testing.T) {
	g := NewWithT(t)

	broker, err := Start("test", nil)
	g.Expect(err).NotTo(HaveOccurred())
	defer broker.Close()

	nc := broker.Conns().Nats()
	g.Expect(nc).NotTo(BeNil())

	sub, err := nc.SubscribeSync("greeting")
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(nc.Publish("greeting", []byte("hello"))).To(Succeed())
	g.Expect(nc.Flush()).To(Succeed())

	msg, err := sub.NextMsg(time.Second)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(msg.Data)).To(Equal("hello"))
}

// Closing twice is what a deferred close and an explicit one add up to on an error
// path, and it must not panic or hang.
func TestClose_IsSafeTwice(t *testing.T) {
	g := NewWithT(t)

	broker, err := Start("test", nil)
	g.Expect(err).NotTo(HaveOccurred())

	broker.Close()
	broker.Close()
}
