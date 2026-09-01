//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agenttest_test

import (
	"context"
	"encoding/json"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/agenttest"
)

var _ = Describe("FakeTransport", func() {
	var (
		ctx       context.Context
		card      a2a.AgentCard
		transport *agenttest.FakeTransport
	)

	BeforeEach(func() {
		ctx = context.Background()
		card = a2a.AgentCard{
			Name:    "peer",
			Version: "1.2.3",
			Tools:   []a2a.ToolDescriptor{{Name: "echo", Description: "echoes its input"}},
		}
		transport = agenttest.NewFakeTransport(GinkgoTB(), card)
	})

	// The client validates every reply against the message schema, so a fake driven through
	// it stamps replies an engine accepts.
	newClient := func() *a2a.Client {
		GinkgoHelper()

		c, err := a2a.NewClient(transport, "caller")
		Expect(err).ToNot(HaveOccurred())

		return c
	}

	It("Should implement the transport a tool call needs", func() {
		var t a2a.Transport = agenttest.BuildFakeTransport(card)

		_, replySet := t.(a2a.ReplySetTransport)
		Expect(replySet).To(BeTrue())
	})

	It("Should answer discovery from the card it was given", func() {
		discovered, err := newClient().Discover(ctx, "peer")
		Expect(err).ToNot(HaveOccurred())
		Expect(discovered.Name).To(Equal("peer"))
		Expect(discovered.Version).To(Equal("1.2.3"))
		Expect(discovered.Tools).To(HaveLen(1))
		Expect(discovered.Tools[0].Name).To(Equal("echo"))

		Expect(transport.RoundTrips()).To(Equal(1))
	})

	It("Should answer a tool call with ok until a spec says otherwise", func() {
		reply, err := newClient().InvokeTool(ctx, "peer", "echo", json.RawMessage(`{"text":"hi"}`))
		Expect(err).ToNot(HaveOccurred())
		Expect(reply.Output).To(Equal("ok"))
		Expect(reply.IsError).To(BeFalse())

		transport.SetToolReply("it went wrong", true)

		reply, err = newClient().InvokeTool(ctx, "peer", "echo", json.RawMessage(`{"text":"hi"}`))
		Expect(err).ToNot(HaveOccurred())
		Expect(reply.Output).To(Equal("it went wrong"))
		Expect(reply.IsError).To(BeTrue())

		Expect(transport.RoundTrips()).To(Equal(2), "a tool call is a round trip like discovery")
	})

	It("Should refuse an operation it has no answer for", func() {
		req := a2a.NewDiscoveryRequest()
		a2a.StampRequest(ctx, &req.Header, "caller", "peer")
		body, err := json.Marshal(req)
		Expect(err).ToNot(HaveOccurred())

		_, err = transport.RoundTrip(ctx, "peer", a2a.OpTool, body)
		Expect(err).To(MatchError(ContainSubstring("unexpected op")))

		_, err = transport.Stream(ctx, "peer", a2a.OpDiscovery, body)
		Expect(err).To(MatchError(ContainSubstring("unexpected streaming op")))
	})

	It("Should refuse a request whose header it cannot decode", func() {
		_, err := transport.RoundTrip(ctx, "peer", a2a.OpDiscovery, []byte("not json"))
		Expect(err).To(MatchError(ContainSubstring("could not decode request header")))
	})

	// A borrowed transport is the caller's to close, so a spec asserts this stays false
	// after a run that used it.
	It("Should report that nothing closed it", func() {
		Expect(transport.Closed()).To(BeFalse())

		Expect(transport.Close()).To(Succeed())

		Expect(transport.Closed()).To(BeTrue())
	})

	It("Should describe no addresses", func() {
		Expect(transport.Describe("peer")).To(BeNil())
	})

	// A run imports remote tools through the client half of a borrowed transport and
	// registers no handlers on it, so a spec asserts this stays zero.
	It("Should count a call that asked it to serve", func() {
		Expect(transport.ServeCalls()).To(Equal(0))

		Expect(transport.Serve(a2a.OpTool, nil)).To(Succeed())

		Expect(transport.ServeCalls()).To(Equal(1))
	})

	It("Should answer requests from several goroutines at once", func() {
		const callers = 8

		client := newClient()

		var wg sync.WaitGroup
		for i := 0; i < callers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer GinkgoRecover()

				_, err := client.Discover(ctx, "peer")
				Expect(err).ToNot(HaveOccurred())

				_, err = client.InvokeTool(ctx, "peer", "echo", json.RawMessage(`{"text":"hi"}`))
				Expect(err).ToNot(HaveOccurred())

				transport.RoundTrips()
			}()
		}

		wg.Wait()

		Expect(transport.RoundTrips()).To(Equal(callers * 2))
	})
})
