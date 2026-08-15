//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package nats

import (
	"encoding/json"
	"testing"

	"github.com/nats-io/nats.go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/conns"
)

func TestNats(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Internal/A2A/Nats")
}

var _ = Describe("Subjects", func() {
	It("Should namespace every subject under the prefix and identity", func() {
		Expect(DiscoverySubject("nats")).To(Equal("choria.fisk-ai.discovery.nats"))
		Expect(ToolSubject("orders-db")).To(Equal("choria.fisk-ai.tool.orders-db"))
		Expect(TaskSubject("orders-db")).To(Equal("choria.fisk-ai.task.orders-db"))
	})

	It("Should put the request id in the cancel subject, so only the worker running it hears", func() {
		Expect(CancelSubject("orders-db", "2abc_1")).To(Equal("choria.fisk-ai.cancel.orders-db.2abc_1"))
	})
})

var _ = Describe("endpointName", func() {
	It("Should name each route hint", func() {
		for hint, want := range map[a2a.RouteHint]string{
			a2a.OpDiscovery: "discovery",
			a2a.OpTool:      "tool",
			a2a.OpTask:      "task",
		} {
			name, err := endpointName(hint)
			Expect(err).ToNot(HaveOccurred())
			Expect(name).To(Equal(want))
		}
	})

	// micro does not refuse a duplicate endpoint name, so a hint that fell through to a
	// name already registered would corrupt INFO and STATS without saying anything.
	It("Should error on a hint it does not know rather than falling back to a name in use", func() {
		_, err := endpointName(a2a.RouteHint(99))
		Expect(err).To(MatchError(ContainSubstring("unknown a2a route hint")))
	})
})

var _ = Describe("newTransport", func() {
	It("Should fail when the provider carries no NATS connection", func() {
		tr, err := newTransport(conns.New(), a2a.TransportConfig{Identity: "svc"})
		Expect(err).To(MatchError(ContainSubstring("requires a NATS connection")))
		Expect(tr).To(BeNil())
	})

	It("Should reject unknown transport options strictly", func() {
		p := conns.New(conns.WithNats(&nats.Conn{}))
		_, err := newTransport(p, a2a.TransportConfig{Identity: "svc", Options: json.RawMessage(`{"nope":true}`)})
		Expect(err).To(MatchError(ContainSubstring("decoding nats transport options")))
	})

	It("Should register itself under the nats name", func() {
		Expect(a2a.Transports()).To(ContainElement("nats"))
	})
})
