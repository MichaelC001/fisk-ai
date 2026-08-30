//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package serve

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/llm"
)

// quietEvents is the sink a channel that renders nothing supplies: it implements
// agent.Events and none of its optional halves. agenttest holds a recording one and
// this package cannot import it, since the fakes import serve.
type quietEvents struct{}

func (quietEvents) Warn(agent.Warning)               {}
func (quietEvents) Starting(agent.RunInfo)           {}
func (quietEvents) LLMRequest(string)                {}
func (quietEvents) ToolCall(agent.ToolTrace)         {}
func (quietEvents) ToolResult(agent.ToolResultTrace) {}
func (quietEvents) Message(llm.Response, bool)       {}
func (quietEvents) SessionRotated(string)            {}
func (quietEvents) Panicked(any, []byte)             {}

// deltaEvents is a channel's sink that hears fragments, answering as the spec sets it.
type deltaEvents struct {
	quietEvents

	wants  bool
	deltas []llm.Delta
}

func (e *deltaEvents) StreamDeltas() bool { return e.wants }

func (e *deltaEvents) MessageDelta(d llm.Delta) { e.deltas = append(e.deltas, d) }

var _ = Describe("eventRecorder", func() {
	// The recorder implements the half for every run, so the runner's assertion on it
	// always succeeds. What the run reads is the answer, and the answer is the channel's.
	It("Should answer StreamDeltas from the channel's sink", func() {
		var sink agent.Events = newEventRecorder(nil, quietLogger())
		streamer, ok := sink.(agent.MessageStreamer)
		Expect(ok).To(BeTrue(), "the recorder implements the half unconditionally")
		Expect(streamer.StreamDeltas()).To(BeFalse(), "a channel supplying no sink wants nothing")

		streamer = newEventRecorder(quietEvents{}, quietLogger())
		Expect(streamer.StreamDeltas()).To(BeFalse(), "a sink that does not implement the half wants nothing")

		streamer = newEventRecorder(&deltaEvents{wants: false}, quietLogger())
		Expect(streamer.StreamDeltas()).To(BeFalse(), "a sink implementing the half decides for itself")

		streamer = newEventRecorder(&deltaEvents{wants: true}, quietLogger())
		Expect(streamer.StreamDeltas()).To(BeTrue())
	})

	// Fragments are forwarded to a sink that hears them and dropped for one that does
	// not, which is the same transparency the other three halves have.
	It("Should forward fragments only to a sink that hears them", func() {
		inner := &deltaEvents{wants: true}
		recorder := newEventRecorder(inner, quietLogger())

		recorder.MessageDelta(llm.Delta{Kind: llm.DeltaText, Text: "hello"})
		recorder.MessageDelta(llm.Delta{Kind: llm.DeltaText, Final: true})
		Expect(inner.deltas).To(Equal([]llm.Delta{
			{Kind: llm.DeltaText, Text: "hello"},
			{Kind: llm.DeltaText, Final: true},
		}))

		Expect(func() {
			newEventRecorder(quietEvents{}, quietLogger()).MessageDelta(llm.Delta{Kind: llm.DeltaText, Text: "hello"})
			newEventRecorder(nil, quietLogger()).MessageDelta(llm.Delta{Kind: llm.DeltaText, Text: "hello"})
		}).NotTo(Panic())
	})
})
