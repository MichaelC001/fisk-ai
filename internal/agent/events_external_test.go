//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agent_test

import (
	"testing"

	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
)

// TestEvents_OptionalHalvesAreOptional pins what the split is for: a sink can render a
// run without naming agent's remote-import helper or the application-command layer's
// tool registry. The recording fake implements neither and is the sink every resume
// spec in this package runs against, so the run path is exercised without them
// throughout rather than only here.
func TestEvents_OptionalHalvesAreOptional(t *testing.T) {
	g := NewWithT(t)

	var sink agent.Events = agenttest.NewRecordingEvents()

	_, reports := sink.(agent.RemoteHostReporter)
	g.Expect(reports).To(BeFalse(), "a sink that renders no advisories should not have to")

	_, replays := sink.(agent.TranscriptReplayer)
	g.Expect(replays).To(BeFalse(), "nor should one that replays no transcript")
}

// TestEvents_RenderersKeepBothHalves guards the other direction. SlogEvents is the sink
// an operator reads a run back from, so losing either half there would drop the import
// notes and the resumed conversation from the log with nothing to notice it.
func TestEvents_RenderersKeepBothHalves(t *testing.T) {
	g := NewWithT(t)

	var sink agent.Events = agent.NewSlogEvents(nil, false)

	_, reports := sink.(agent.RemoteHostReporter)
	g.Expect(reports).To(BeTrue())

	_, replays := sink.(agent.TranscriptReplayer)
	g.Expect(replays).To(BeTrue())
}
