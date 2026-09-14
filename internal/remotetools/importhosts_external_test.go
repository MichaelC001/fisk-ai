//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package remotetools_test

import (
	"context"
	"encoding/json"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/a2a"
	wire "github.com/choria-io/fisk-ai/internal/a2a/wire/v1"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/remotetools"
)

// peerCard is a card advertising the named tools.
func peerCard(tools ...string) wire.AgentCard {
	card := wire.AgentCard{Name: "peer", Version: "1.0.0"}
	for _, name := range tools {
		card.Tools = append(card.Tools, wire.ToolDescriptor{
			Name:        name,
			Description: name + " on the peer",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		})
	}

	return card
}

var _ = Describe("ImportHosts", func() {
	var (
		ctx context.Context
		cfg *config.Config
	)

	BeforeEach(func() {
		ctx = context.Background()
		cfg = &config.Config{
			Identity:    "agent",
			NatsContext: "lab",
			RemoteTools: []config.RemoteToolHost{{Name: "live"}, {Name: "dead"}},
		}
	})

	// The dead host comes first, where ImportForRun would return before resolving
	// anything.
	It("Should resolve the hosts that answered and return the first failure", func() {
		cfg.RemoteTools = []config.RemoteToolHost{{Name: "dead"}, {Name: "live"}}

		transport := agenttest.NewFakeTransport(GinkgoTB(), peerCard("forecast"))
		transport.SetFaults(agenttest.TransportFault{Agent: "dead", Err: a2a.ErrNoResponders})
		client, err := a2a.NewClient(transport, "agent")
		Expect(err).NotTo(HaveOccurred())

		imports, byName, err := remotetools.ImportHosts(ctx, client, cfg, map[string]bool{})
		Expect(err).To(MatchError(ContainSubstring(`importing tools from remote agent "dead" on context "lab"`)))
		Expect(errors.Is(err, a2a.ErrNoResponders)).To(BeTrue())

		Expect(imports).To(HaveLen(2))
		Expect(imports[0].Host.Name).To(Equal("dead"))
		Expect(imports[0].Err).To(HaveOccurred())
		Expect(imports[0].Tools).To(BeEmpty())
		Expect(imports[1].Host.Name).To(Equal("live"))
		Expect(imports[1].Err).NotTo(HaveOccurred())
		Expect(imports[1].Tools).To(HaveLen(1))
		Expect(imports[1].Tools[0].Name()).To(Equal("forecast"))

		Expect(byName).To(HaveKey("forecast"))
	})

	It("Should return no error when every host answered", func() {
		client, err := a2a.NewClient(agenttest.NewFakeTransport(GinkgoTB(), peerCard("forecast")), "agent")
		Expect(err).NotTo(HaveOccurred())

		imports, byName, err := remotetools.ImportHosts(ctx, client, cfg, map[string]bool{})
		Expect(err).NotTo(HaveOccurred())

		// Both hosts advertise the bare name, so both are prefixed.
		Expect(imports).To(HaveLen(2))
		Expect(imports[0].Tools[0].Name()).To(Equal("live_forecast"))
		Expect(imports[1].Tools[0].Name()).To(Equal("dead_forecast"))
		Expect(byName).To(HaveLen(2))
	})

	It("Should report a collision after a failed host as the failed host", func() {
		cfg.RemoteTools = []config.RemoteToolHost{{Name: "dead"}, {Name: "live"}}

		transport := agenttest.NewFakeTransport(GinkgoTB(), peerCard("forecast"))
		transport.SetFaults(agenttest.TransportFault{Agent: "dead", Err: a2a.ErrNoResponders})
		client, err := a2a.NewClient(transport, "agent")
		Expect(err).NotTo(HaveOccurred())

		// The bare name is taken, so it is prefixed to live_forecast, which is taken too.
		taken := map[string]bool{"forecast": true, "live_forecast": true}

		imports, byName, err := remotetools.ImportHosts(ctx, client, cfg, taken)
		Expect(err).To(MatchError(ContainSubstring(`importing tools from remote agent "dead"`)))
		Expect(imports[1].Skipped).To(ConsistOf(`forecast (name "live_forecast" collides)`))
		Expect(byName).To(BeEmpty())
	})

	It("Should return the collision when every host answered", func() {
		cfg.RemoteTools = []config.RemoteToolHost{{Name: "live"}}

		client, err := a2a.NewClient(agenttest.NewFakeTransport(GinkgoTB(), peerCard("forecast")), "agent")
		Expect(err).NotTo(HaveOccurred())

		imports, _, err := remotetools.ImportHosts(ctx, client, cfg, map[string]bool{"forecast": true, "live_forecast": true})
		Expect(err).To(MatchError(ContainSubstring("imported tool name collision")))
		Expect(imports[0].Skipped).To(HaveLen(1))
	})
})
