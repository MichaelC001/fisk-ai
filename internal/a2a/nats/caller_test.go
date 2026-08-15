//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package nats

import (
	"context"
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/conns"
)

var _ = Describe("Integration: caller context", func() {
	// serveCaller stands a transport up and captures the caller it hands its handler for
	// one request carrying body.
	serveCaller := func(body []byte) a2a.Caller {
		GinkgoHelper()

		nc := runNATS()

		transport, err := a2a.NewTransport("nats", conns.New(conns.WithNats(nc)), a2a.TransportConfig{Identity: "svc"})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(transport.Close)

		seen := make(chan a2a.Caller, 1)
		err = transport.Serve(a2a.OpTool, func(_ context.Context, caller a2a.Caller, _ []byte, reply a2a.Replier) {
			seen <- caller
			Expect(reply.Respond([]byte(`{}`))).To(Succeed())
		})
		Expect(err).NotTo(HaveOccurred())

		_, err = nc.Request(ToolSubject("svc"), body, 5*time.Second)
		Expect(err).NotTo(HaveOccurred())

		var got a2a.Caller
		Eventually(seen).Should(Receive(&got))

		return got
	}

	// NATS authenticates a connection to the server rather than a publisher to a
	// subscriber, so this transport can vouch for nobody and says so.
	It("Should hand the handler a caller it vouches for nothing about", func() {
		caller := serveCaller([]byte(`{"protocol":"probe"}`))

		Expect(caller.Verified).To(BeFalse())
		Expect(caller.Name).To(BeEmpty(), "a name with nothing vouching for it is worse than none")
	})

	// Header.Sender is what a caller meant to say and Caller is what something vouches
	// for. Reading the claim out of the body and presenting it as the transport's answer
	// is the mistake supplying the caller from the transport exists to prevent.
	It("Should not promote a claimed sender into the transport's caller", func() {
		req := a2a.NewToolRequest("ping", nil)
		req.Sender = a2a.Identity{Name: "trust-me"}

		body, err := json.Marshal(req)
		Expect(err).NotTo(HaveOccurred())

		caller := serveCaller(body)

		Expect(caller.Name).ToNot(Equal("trust-me"))
		Expect(caller.Verified).To(BeFalse())
	})
})
