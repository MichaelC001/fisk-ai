//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agenttest_test

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/a2a"
	wire "github.com/choria-io/fisk-ai/internal/a2a/wire/v1"
	"github.com/choria-io/fisk-ai/internal/agenttest"
)

var _ = Describe("FakeTransport", func() {
	var (
		ctx       context.Context
		card      wire.AgentCard
		transport *agenttest.FakeTransport
	)

	BeforeEach(func() {
		ctx = context.Background()
		card = wire.AgentCard{
			Name:    "peer",
			Version: "1.2.3",
			Tools:   []wire.ToolDescriptor{{Name: "echo", Description: "echoes its input"}},
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
		req := wire.NewDiscoveryRequest()
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

	Describe("Faults", func() {
		It("Should report a peer nothing is listening for", func() {
			transport.SetFaults(agenttest.TransportFault{Err: a2a.ErrNoResponders})

			client := newClient()

			_, err := client.Discover(ctx, "peer")
			Expect(err).To(MatchError(a2a.ErrNoResponders))
			Expect(err).To(MatchError(a2a.ErrAgentUnavailable), "the narrow sentinel wraps the wide one")

			_, err = client.InvokeTool(ctx, "peer", "echo", json.RawMessage(`{"text":"hi"}`))
			Expect(err).To(MatchError(a2a.ErrNoResponders))

			Expect(transport.RoundTrips()).To(Equal(2), "a request a fault failed is a request it was given")
		})

		It("Should report a peer that accepted the call and never answered", func() {
			transport.SetFaults(agenttest.TransportFault{Err: a2a.ErrAgentUnavailable})

			_, err := newClient().InvokeTool(ctx, "peer", "echo", json.RawMessage(`{"text":"hi"}`))
			Expect(err).To(MatchError(a2a.ErrAgentUnavailable))
			Expect(err).ToNot(MatchError(a2a.ErrNoResponders), "somebody is listening, they were just silent")
		})

		It("Should report a reply the caller cannot use", func() {
			transport.SetFaults(agenttest.TransportFault{Err: a2a.ErrToolImport})

			_, err := newClient().Discover(ctx, "peer")
			Expect(err).To(MatchError(a2a.ErrToolImport))
		})

		It("Should fail one peer and answer another", func() {
			transport.SetFaults(agenttest.TransportFault{Agent: "gone", Err: a2a.ErrNoResponders})

			client := newClient()

			_, err := client.Discover(ctx, "gone")
			Expect(err).To(MatchError(a2a.ErrNoResponders))

			discovered, err := client.Discover(ctx, "peer")
			Expect(err).ToNot(HaveOccurred())
			Expect(discovered.Name).To(Equal("peer"))

			reply, err := client.InvokeTool(ctx, "peer", "echo", json.RawMessage(`{"text":"hi"}`))
			Expect(err).ToNot(HaveOccurred())
			Expect(reply.Output).To(Equal("ok"))
		})

		It("Should answer discovery for a peer whose tool calls fail", func() {
			transport.SetFaults(agenttest.TransportFault{
				Ops: []a2a.RouteHint{a2a.OpTool},
				Err: a2a.ErrAgentUnavailable,
			})

			client := newClient()

			discovered, err := client.Discover(ctx, "peer")
			Expect(err).ToNot(HaveOccurred())
			Expect(discovered.Tools).To(HaveLen(1))

			_, err = client.InvokeTool(ctx, "peer", "echo", json.RawMessage(`{"text":"hi"}`))
			Expect(err).To(MatchError(a2a.ErrAgentUnavailable))
		})

		It("Should take the first fault a request matches", func() {
			transport.SetFaults(
				agenttest.TransportFault{Agent: "peer", Ops: []a2a.RouteHint{a2a.OpTool}, Err: a2a.ErrToolImport},
				agenttest.TransportFault{Err: a2a.ErrNoResponders},
			)

			client := newClient()

			_, err := client.InvokeTool(ctx, "peer", "echo", json.RawMessage(`{"text":"hi"}`))
			Expect(err).To(MatchError(a2a.ErrToolImport))

			_, err = client.Discover(ctx, "peer")
			Expect(err).To(MatchError(a2a.ErrNoResponders))
		})

		It("Should answer everything again once a spec clears the faults", func() {
			transport.SetFaults(agenttest.TransportFault{Err: a2a.ErrNoResponders})
			transport.SetFaults()

			_, err := newClient().Discover(ctx, "peer")
			Expect(err).ToNot(HaveOccurred())
		})

		It("Should hand a delay to the waiter rather than sleeping through it", func() {
			transport.SetFaults(agenttest.TransportFault{Delay: time.Hour})

			var waited []time.Duration
			transport.SetWaiter(func(_ context.Context, d time.Duration) error {
				waited = append(waited, d)
				return nil
			})

			client := newClient()
			started := time.Now()

			_, err := client.Discover(ctx, "peer")
			Expect(err).ToNot(HaveOccurred())

			reply, err := client.InvokeTool(ctx, "peer", "echo", json.RawMessage(`{"text":"hi"}`))
			Expect(err).ToNot(HaveOccurred())
			Expect(reply.Output).To(Equal("ok"), "a delay the fault names alone answers once it has passed")

			Expect(waited).To(Equal([]time.Duration{time.Hour, time.Hour}))
			Expect(time.Since(started)).To(BeNumerically("<", time.Second))
		})

		It("Should fail a call the waiter ended", func() {
			transport.SetFaults(agenttest.TransportFault{Delay: time.Hour})
			transport.SetWaiter(func(context.Context, time.Duration) error { return context.DeadlineExceeded })

			client := newClient()

			_, err := client.Discover(ctx, "peer")
			Expect(err).To(MatchError(context.DeadlineExceeded))

			_, err = client.InvokeTool(ctx, "peer", "echo", json.RawMessage(`{"text":"hi"}`))
			Expect(err).To(MatchError(context.DeadlineExceeded))
		})

		It("Should wait on the default timer again when a spec sets a nil waiter", func() {
			transport.SetFaults(agenttest.TransportFault{Delay: time.Hour})
			transport.SetWaiter(func(context.Context, time.Duration) error { return nil })
			transport.SetWaiter(nil)

			// The default waiter selects on the context, so an already-canceled one returns
			// from an hour's delay immediately.
			callCtx, cancel := context.WithCancel(ctx)
			cancel()

			_, err := newClient().Discover(callCtx, "peer")
			Expect(err).To(MatchError(context.Canceled))
		})

		It("Should leave the waiter out of a request with no delay", func() {
			transport.SetWaiter(func(context.Context, time.Duration) error {
				Fail("the waiter was called for a request with no delay")
				return nil
			})

			_, err := newClient().Discover(ctx, "peer")
			Expect(err).ToNot(HaveOccurred())
		})

		It("Should count a request as it arrives rather than once its delay has passed", func() {
			transport.SetFaults(agenttest.TransportFault{Delay: time.Hour, Err: a2a.ErrAgentUnavailable})

			counted := 0
			transport.SetWaiter(func(context.Context, time.Duration) error {
				counted = transport.RoundTrips()
				return nil
			})

			_, err := newClient().Discover(ctx, "peer")
			Expect(err).To(MatchError(a2a.ErrAgentUnavailable))

			Expect(counted).To(Equal(1), "the transport answers a waiter that reads it back mid-wait")
			Expect(transport.RoundTrips()).To(Equal(1))
		})
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
