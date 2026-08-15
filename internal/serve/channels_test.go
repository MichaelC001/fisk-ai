//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package serve

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Per-channel concurrency", func() {
	It("Should take a channel's own bound when it states one", func() {
		srv, err := New(Options{
			Channels:    []Channel{&boundedChannel{idleChannel: idleChannel{name: "bounded"}, concurrency: 9}},
			Config:      servedConfig(),
			Concurrency: 2,
			Logger:      quietLogger(),
		})
		Expect(err).ToNot(HaveOccurred())

		Expect(srv.concurrencyFor(srv.opts.Channels[0])).To(Equal(9))
	})

	It("Should fall back to the configured default", func() {
		srv, err := New(Options{
			Channels:    []Channel{&idleChannel{name: "plain"}},
			Config:      servedConfig(),
			Concurrency: 3,
			Logger:      quietLogger(),
		})
		Expect(err).ToNot(HaveOccurred())

		Expect(srv.concurrencyFor(srv.opts.Channels[0])).To(Equal(3))

		// A channel that answers with nothing useful is the same as one that does not
		// answer at all, rather than a server with no slots that never runs anything.
		zero := &boundedChannel{idleChannel: idleChannel{name: "zero"}, concurrency: 0}
		Expect(srv.concurrencyFor(zero)).To(Equal(3))
	})
})
