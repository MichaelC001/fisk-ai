//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agent_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
)

var _ = Describe("the Events sink", func() {
	// This pins what the split is for: a sink can render a run without naming agent's
	// remote-import helper or the application-command layer's tool registry. The
	// recording fake implements neither and is the sink every resume spec in this
	// package runs against, so the run path is exercised without them throughout rather
	// than only here.
	It("Should let a sink implement neither optional half", func() {
		var sink agent.Events = agenttest.NewRecordingEvents()

		_, reports := sink.(agent.RemoteHostReporter)
		Expect(reports).To(BeFalse(), "a sink that renders no advisories should not have to")

		_, replays := sink.(agent.TranscriptReplayer)
		Expect(replays).To(BeFalse(), "nor should one that replays no transcript")
	})

	// This guards the other direction. SlogEvents is the sink an operator reads a run
	// back from, so losing either half there would drop the import notes and the resumed
	// conversation from the log with nothing to notice it.
	It("Should keep both optional halves on the renderers", func() {
		var sink agent.Events = agent.NewSlogEvents(nil, false)

		_, reports := sink.(agent.RemoteHostReporter)
		Expect(reports).To(BeTrue())

		_, replays := sink.(agent.TranscriptReplayer)
		Expect(replays).To(BeTrue())
	})
})
