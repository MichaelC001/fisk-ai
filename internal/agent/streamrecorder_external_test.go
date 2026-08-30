//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// These tests cover agenttest.RecordingStreamEvents, the recorder an embedder writing a
// delta sink asserts against. The agenttest package has no suite of its own, so its fakes
// are pinned from here, where a real run drives them, as RecordingEvents is in
// events_external_test.go.
package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
)

// callScript is one model call a scriptedDeltaProvider answers: the fragments it reports
// as the turn is written, and the turn it returns.
type callScript struct {
	deltas   []llm.Delta
	response *llm.Response
}

// scriptedDeltaProvider answers a queue of calls, one per CallStream, so a spec can read
// a recorder across a run of several. Call fails: these specs assert on what the streaming
// path recorded, and a run that took the ordinary one would leave them nothing to read.
type scriptedDeltaProvider struct {
	mu     sync.Mutex
	script []callScript
	idx    int
}

func (p *scriptedDeltaProvider) Capabilities() llm.Caps { return llm.Caps{Provider: "anthropic"} }

func (p *scriptedDeltaProvider) Call(context.Context, llm.Request) (*llm.Response, error) {
	return nil, fmt.Errorf("scriptedDeltaProvider: the run made the ordinary call")
}

func (p *scriptedDeltaProvider) CallStream(_ context.Context, _ llm.Request, fn func(llm.Delta)) (*llm.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.idx >= len(p.script) {
		return nil, fmt.Errorf("scriptedDeltaProvider: exhausted at call %d of %d", p.idx+1, len(p.script))
	}

	call := p.script[p.idx]
	p.idx++

	for _, d := range call.deltas {
		fn(d)
	}

	return call.response, nil
}

var _ = Describe("the recording delta sink", func() {
	// One block written in two fragments, then its end.
	fragments := []llm.Delta{
		{Kind: llm.DeltaText, Index: 0, Text: "the answer "},
		{Kind: llm.DeltaText, Index: 0, Text: "is 42"},
		{Kind: llm.DeltaText, Index: 0, Final: true},
	}

	// A sink built to decline is the case an embedder reaches for to prove their adapter
	// does not stream, so the answer has to reach the runner and stop the streaming call.
	It("Should answer StreamDeltas with what it was constructed with", func() {
		provider := &deltaProvider{deltas: fragments}
		sink := agenttest.NewRecordingStreamEvents(false)

		res, err := streamingRun(provider, sink)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

		calls, streamed := provider.Counts()
		Expect(calls).To(Equal(1))
		Expect(streamed).To(BeZero())
		Expect(sink.Asked()).To(Equal(1))
		Expect(sink.Deltas()).To(BeEmpty())
	})

	// Both halves of what the recorder is for: the fragments as they arrived, and the text
	// the reconciler decided for the index once the turn ended it.
	It("Should record every fragment in order and what the call assembles to", func() {
		provider := &deltaProvider{deltas: fragments}
		sink := agenttest.NewRecordingStreamEvents(true)

		res, err := streamingRun(provider, sink)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

		_, streamed := provider.Counts()
		Expect(streamed).To(Equal(1))
		Expect(sink.Deltas()).To(Equal(fragments))

		calls := sink.Calls()
		Expect(calls).To(HaveLen(1))
		Expect(calls[0].Deltas).To(Equal(fragments))
		Expect(calls[0].Terminal).To(BeTrue())
		Expect(calls[0].Blocks).To(Equal([]llm.AssembledBlock{
			{Kind: llm.DeltaText, Index: 0, Text: "the answer is 42", Source: llm.SourceBlock},
		}))

		// The embedded recorder still hears the turn, so a spec reads the final answer the
		// way it does from a sink that streams nothing.
		final, ok := sink.FinalMessage()
		Expect(ok).To(BeTrue())
		Expect(final.Content[0].Text.Text).To(Equal("the answer is 42"))
	})

	// The outcome a delta sink behind a2a has to get right: the whole block is a cut copy
	// of text the fragments carried in full, so the fragments stand.
	It("Should keep the fragments over a block that arrived trimmed", func() {
		sink := agenttest.NewRecordingStreamEvents(true)

		Expect(sink.StreamDeltas()).To(BeTrue())
		sink.MessageDelta(llm.Delta{Kind: llm.DeltaText, Index: 0, Text: "the whole answer"})

		got := sink.AddBlock(llm.WholeBlock{Kind: llm.DeltaText, Index: 0, Text: "the whole", Trimmed: true})
		Expect(got.Source).To(Equal(llm.SourceKeptFragments))
		Expect(got.Text).To(Equal("the whole answer"))

		Expect(sink.Assembled()).To(Equal([]llm.AssembledBlock{
			{Kind: llm.DeltaText, Index: 0, Text: "the whole answer", Source: llm.SourceKeptFragments},
		}))
	})

	// Block indexes restart at 0 on every call, so a recorder that carried one call's
	// fragments into the next would report the two answers joined. StreamDeltas is the
	// boundary that prevents it, here driven by hand as the runner drives it.
	It("Should start a call's blocks empty rather than on the previous call's", func() {
		sink := agenttest.NewRecordingStreamEvents(true)

		sink.StreamDeltas()
		sink.MessageDelta(llm.Delta{Kind: llm.DeltaText, Index: 0, Text: "first call"})
		sink.AddBlock(llm.WholeBlock{Kind: llm.DeltaText, Index: 0, Text: "first", Trimmed: true})

		sink.StreamDeltas()
		sink.MessageDelta(llm.Delta{Kind: llm.DeltaText, Index: 0, Text: "second call"})

		got := sink.AddBlock(llm.WholeBlock{Kind: llm.DeltaText, Index: 0, Text: "second", Trimmed: true})
		Expect(got.Text).To(Equal("second call"))
	})

	// A journal write or a PostModelCall hook can end a run after the provider returned
	// and before Message, so the fragments of that call are never recorded as a call. They
	// stay in Deltas, which is the run rather than the calls that completed.
	It("Should drop the fragments of a call that never reached Message", func() {
		sink := agenttest.NewRecordingStreamEvents(true)

		sink.StreamDeltas()
		sink.MessageDelta(llm.Delta{Kind: llm.DeltaText, Index: 0, Text: "abandoned"})

		sink.StreamDeltas()
		sink.MessageDelta(llm.Delta{Kind: llm.DeltaText, Index: 0, Text: "kept"})
		sink.Message(llm.Response{Content: []llm.ContentBlock{{Text: &llm.TextBlock{Text: "kept"}}}}, true)

		Expect(sink.Calls()).To(HaveLen(1))
		Expect(sink.Calls()[0].Deltas).To(HaveLen(1))
		Expect(sink.Calls()[0].Deltas[0].Text).To(Equal("kept"))
		Expect(sink.Deltas()).To(HaveLen(2))
	})

	// A run of two calls: the narration that asks for a tool, then the answer. Deltas is
	// the whole run and each RecordedCall holds its own call's fragments, so an embedder
	// can assert either without reconstructing the split.
	It("Should record each model call's fragments and blocks on their own", func() {
		narration := []llm.Delta{
			{Kind: llm.DeltaText, Index: 0, Text: "looking "},
			{Kind: llm.DeltaText, Index: 0, Text: "it up", Final: true},
		}
		provider := &scriptedDeltaProvider{script: []callScript{
			{deltas: narration, response: &llm.Response{
				StopReason: llm.StopToolUse,
				Content: []llm.ContentBlock{
					{Text: &llm.TextBlock{Text: "looking it up"}},
					{ToolUse: &llm.ToolUseBlock{ID: "c1", Name: "do", Input: json.RawMessage(`{"subject":"widgets"}`)}},
				},
			}},
			{deltas: fragments, response: agenttest.TextResponse("the answer is 42")},
		}}
		sink := agenttest.NewRecordingStreamEvents(true)

		res, err := streamingRun(provider, sink)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))
		Expect(sink.Asked()).To(Equal(2))

		Expect(sink.Deltas()).To(Equal(append(append([]llm.Delta{}, narration...), fragments...)))

		calls := sink.Calls()
		Expect(calls).To(HaveLen(2))

		Expect(calls[0].Deltas).To(Equal(narration))
		Expect(calls[0].Terminal).To(BeFalse())
		// The tool call the turn also carried has no delta kind and streams nothing, so
		// index 1 assembles to nothing at all.
		Expect(calls[0].Blocks).To(Equal([]llm.AssembledBlock{
			{Kind: llm.DeltaText, Index: 0, Text: "looking it up", Source: llm.SourceBlock},
		}))

		Expect(calls[1].Deltas).To(Equal(fragments))
		Expect(calls[1].Terminal).To(BeTrue())
		Expect(calls[1].Blocks).To(Equal([]llm.AssembledBlock{
			{Kind: llm.DeltaText, Index: 0, Text: "the answer is 42", Source: llm.SourceBlock},
		}))
	})
})
