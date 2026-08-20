//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/serve/a2aendpoint"
)

var _ = Describe("resumeHint", func() {
	const (
		identity = "joker"
		token    = "2fJ8kQwLmNpRsTvXyZaBcDeFgHi"
	)

	It("Should print nothing for a run that left no conversation", func() {
		Expect(resumeHint(identity, "", "")).To(BeEmpty())
		Expect(resumeHint(identity, "ngs_user", "")).To(BeEmpty())
	})

	// Locally the store holds the token beside the conversation, so the session id
	// resolves and is the handle fisk-ai session names.
	It("Should name the session id for an agent hosted here", func() {
		Expect(resumeHint(identity, "", token)).To(Equal("resume with: fisk run --resume " + a2aendpoint.SessionFor(identity, token)))
	})

	// Remotely there is no store to resolve an id against: it travels as a token, the
	// worker hashes it again, and it names no journal. So the hint carries the token, and
	// the context and identity without which the command reaches a different agent, or
	// none at all in a directory holding no configuration.
	It("Should name the token, the context and the agent for one somewhere else", func() {
		Expect(resumeHint(identity, "ngs_user", token)).To(Equal(
			"resume with: fisk run --nats-context ngs_user --identity " + identity + " --resume " + token))
	})

	It("Should not print a session id remotely, which a remote resume refuses", func() {
		Expect(resumeHint(identity, "ngs_user", token)).ToNot(ContainSubstring(a2aendpoint.SessionFor(identity, token)))
	})
})
