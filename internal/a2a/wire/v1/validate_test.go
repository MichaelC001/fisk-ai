//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package wire

import (
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("ExpectProtocol", func() {
	It("Should return the decoded message when the protocol matches", func() {
		req := NewToolRequest("ping", nil)
		StampRequest(&req.Header, "me", "you", "")
		data, err := json.Marshal(req)
		Expect(err).NotTo(HaveOccurred())

		msg, err := ExpectProtocol[*ToolRequest](data, ToolRequestProtocol)
		Expect(err).NotTo(HaveOccurred())
		Expect(msg.Name).To(Equal("ping"))
	})

	// The type argument names what the id decodes into, so a call naming another one is
	// written wrong. It is refused rather than returning a zero value the caller would
	// read as a message that arrived.
	It("Should reject a type argument the wanted protocol does not decode into", func() {
		req := NewToolRequest("ping", nil)
		StampRequest(&req.Header, "me", "you", "")
		data, err := json.Marshal(req)
		Expect(err).NotTo(HaveOccurred())

		msg, err := ExpectProtocol[*Cancel](data, ToolRequestProtocol)
		Expect(err).To(MatchError(ErrProtocolMismatch))
		Expect(err).To(MatchError(ContainSubstring("decodes into *wire.ToolRequest, not *wire.Cancel")))
		Expect(msg).To(BeNil())
	})

	It("Should reject a message whose protocol is not the one the path carries", func() {
		req := NewDiscoveryRequest()
		StampRequest(&req.Header, "me", "you", "")
		data, err := json.Marshal(req)
		Expect(err).NotTo(HaveOccurred())

		_, err = ExpectProtocol[*ToolRequest](data, ToolRequestProtocol)
		Expect(err).To(MatchError(ErrProtocolMismatch))
	})

	It("Should reject an undecodable body", func() {
		_, err := ExpectProtocol[*ToolRequest]([]byte(`{"protocol":"nope"}`), ToolRequestProtocol)
		Expect(err).To(MatchError(ErrProtocolMismatch))
	})
})
