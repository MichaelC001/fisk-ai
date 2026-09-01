//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package embedded

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// These specs assert the options this package asks for. A running server reports its
// addresses but not the options it was given, so this is the one claim an external
// spec cannot make; everything else about a Broker is pinned from outside.
var _ = Describe("serverOptions", func() {
	// The claim this package makes is that nothing outside the process can reach it, so
	// every listener is asserted rather than trusted to the literal being right.
	It("Should ask for no listener of any kind", func() {
		opts := serverOptions()

		Expect(opts.DontListen).To(BeTrue(), "a client port would put this agent on the network")
		Expect(opts.Port).To(BeZero())
		Expect(opts.HTTPPort).To(BeZero())
		Expect(opts.HTTPSPort).To(BeZero())
		Expect(opts.Cluster.Port).To(BeZero())
		Expect(opts.LeafNode.Port).To(BeZero())
		Expect(opts.Websocket.Port).To(BeZero())
		Expect(opts.Gateway.Port).To(BeZero())
	})

	It("Should ask for no store", func() {
		Expect(serverOptions().JetStream).To(BeFalse(), "there is no store here and nothing should think there is")
	})

	// A server that installed its own handler would take an interrupt away from the
	// program hosting it and exit the process, where an interrupt to a terminal running
	// an agent means stop the run at a boundary.
	It("Should leave signals and the terminal to the process", func() {
		opts := serverOptions()

		Expect(opts.NoSigs).To(BeTrue())
		Expect(opts.NoLog).To(BeTrue(), "a server writing to stdout would corrupt a piped run")
	})
})
