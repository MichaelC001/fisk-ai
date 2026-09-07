//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	"context"
	"encoding/json"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	wire "github.com/choria-io/fisk-ai/internal/a2a/wire/v1"
)

// cardTransport answers every round trip with one canned card, so a spec drives what a
// client does with what a peer sent it.
type cardTransport struct {
	card wire.AgentCard
}

func (t *cardTransport) RoundTrip(_ context.Context, _ string, _ RouteHint, body []byte) ([]byte, error) {
	GinkgoHelper()

	req, err := wire.ExpectProtocol[*wire.DiscoveryRequest](body, wire.DiscoveryRequestProtocol)
	Expect(err).ToNot(HaveOccurred())

	reply := &wire.DiscoveryReply{AgentCard: t.card}
	reply.Protocol = wire.DiscoveryReplyProtocol
	wire.StampReply(&reply.Header, &req.Header, "peer")

	out, err := json.Marshal(reply)
	Expect(err).ToNot(HaveOccurred())

	return out, nil
}

func (t *cardTransport) Serve(RouteHint, Handler) error { return nil }
func (t *cardTransport) Close() error                   { return nil }

var _ = Describe("Agent card", func() {
	discover := func(card wire.AgentCard) (*wire.AgentCard, error) {
		GinkgoHelper()

		client, err := NewClient(&cardTransport{card: card}, "caller")
		Expect(err).ToNot(HaveOccurred())

		return client.Discover(context.Background(), "peer")
	}

	It("Should refuse to serve a card whose icon url a browser must not be handed", func() {
		_, err := NewServer(newFakeTransport(), nil, ServerOptions{Identity: "svc", IconURL: "javascript:alert(1)"})
		Expect(err).To(MatchError(wire.ErrIconURL))

		_, err = NewServer(newFakeTransport(), nil, ServerOptions{Identity: "svc", IconURL: "https://example.net/" + strings.Repeat("a", wire.MaxIconURLLength)})
		Expect(err).To(MatchError(wire.ErrIconURL))
	})

	// The discovery reply schema limits both, so a card over either is one every caller
	// fails to validate. Refusing at construction puts that in front of the operator who
	// wrote the value rather than leaving an agent that starts and cannot be discovered.
	It("Should refuse to serve a card whose display name or icon the schema will not carry", func() {
		_, err := NewServer(newFakeTransport(), nil, ServerOptions{
			Identity:    "svc",
			DisplayName: strings.Repeat("a", wire.MaxDisplayNameLength+1),
		})
		Expect(err).To(MatchError(wire.ErrCardField))

		_, err = NewServer(newFakeTransport(), nil, ServerOptions{
			Identity: "svc",
			Icon:     strings.Repeat("a", wire.MaxIconLength+1),
		})
		Expect(err).To(MatchError(wire.ErrCardField))
	})

	It("Should serve what the operator wrote about the agent", func() {
		s, err := NewServer(newFakeTransport(), nil, ServerOptions{
			Identity:    "nats-auth-prod-eu",
			Version:     "1.2.3",
			Description: "manages nats auth",
			DisplayName: "NATS Auth",
			Icon:        "\U0001f510",
			IconURL:     "https://example.net/agent.png",
		})
		Expect(err).ToNot(HaveOccurred())

		Expect(s.card.Description).To(Equal("manages nats auth"))
		Expect(s.card.DisplayName).To(Equal("NATS Auth"))
		Expect(s.card.Icon).To(Equal("\U0001f510"))
		Expect(s.card.IconURL).To(Equal("https://example.net/agent.png"))
	})

	// The card is a peer's claim about itself and a caller may put it in front of a
	// person. A scheme outside https never reaches here, since the discovery reply
	// schema states the same rule and the reply is validated first; what does reach
	// here is an https URL naming no host.
	It("Should clear an icon url the peer's card names no host in", func() {
		card, err := discover(wire.AgentCard{Name: "peer", Version: "1", IconURL: "https:///agent.png"})
		Expect(err).ToNot(HaveOccurred())
		Expect(card.IconURL).To(BeEmpty())
	})

	It("Should refuse a card whose icon url carries a scheme a browser must not be handed", func() {
		for _, icon := range []string{"javascript:alert(1)", "http://example.net/a.png"} {
			_, err := discover(wire.AgentCard{Name: "peer", Version: "1", IconURL: icon})
			Expect(err).To(MatchError(ErrToolImport), icon)
		}
	})

	It("Should refuse a card whose icon url is over the length limit", func() {
		_, err := discover(wire.AgentCard{Name: "peer", Version: "1", IconURL: "https://example.net/" + strings.Repeat("a", wire.MaxIconURLLength)})
		Expect(err).To(MatchError(ErrToolImport))
	})

	It("Should carry what the peer said about itself", func() {
		card, err := discover(wire.AgentCard{
			Name:        "peer",
			Version:     "1",
			DisplayName: "NATS Auth",
			Icon:        "\U0001f510",
			IconURL:     "https://example.net/agent.png",
			Notes:       []string{"the tools of one source could not be listed"},
		})
		Expect(err).ToNot(HaveOccurred())
		Expect(card.DisplayName).To(Equal("NATS Auth"))
		Expect(card.Icon).To(Equal("\U0001f510"))
		Expect(card.IconURL).To(Equal("https://example.net/agent.png"))
		Expect(card.Notes).To(ConsistOf("the tools of one source could not be listed"))
	})
})
