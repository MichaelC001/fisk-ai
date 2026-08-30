//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// These tests cover the MessageStreamer half of agent.Events: which call the runner
// makes, and who hears the fragments when it streams.
package agent_test

import (
	"context"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
)

// deltaProvider answers one terminal turn on either call path and records which path
// was taken, so a spec can tell a streamed call from an ordinary one. CallStream
// reports the fragments it was built with before returning the same turn Call returns,
// which is the equivalence llm.StreamingProvider requires of a backend.
type deltaProvider struct {
	deltas []llm.Delta

	mu       sync.Mutex
	calls    int
	streamed int
}

func (p *deltaProvider) Capabilities() llm.Caps { return llm.Caps{Provider: "anthropic"} }

func (p *deltaProvider) Call(context.Context, llm.Request) (*llm.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++

	return agenttest.TextResponse("the answer is 42"), nil
}

func (p *deltaProvider) CallStream(_ context.Context, _ llm.Request, fn func(llm.Delta)) (*llm.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.streamed++

	for _, d := range p.deltas {
		fn(d)
	}

	return agenttest.TextResponse("the answer is 42"), nil
}

func (p *deltaProvider) Counts() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.calls, p.streamed
}

// plainProvider streams nothing, so a sink asking for fragments against it gets the
// ordinary call and no fragments.
type plainProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *plainProvider) Capabilities() llm.Caps { return llm.Caps{Provider: "anthropic"} }

func (p *plainProvider) Call(context.Context, llm.Request) (*llm.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++

	return agenttest.TextResponse("the answer is 42"), nil
}

func (p *plainProvider) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.calls
}

// streamingSink is a recording sink that also implements agent.MessageStreamer, with
// the answer it gives set per spec.
type streamingSink struct {
	*agenttest.RecordingEvents

	wants bool

	mu     sync.Mutex
	asked  int
	deltas []llm.Delta
}

func newStreamingSink(wants bool) *streamingSink {
	return &streamingSink{RecordingEvents: agenttest.NewRecordingEvents(), wants: wants}
}

func (s *streamingSink) StreamDeltas() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked++

	return s.wants
}

func (s *streamingSink) MessageDelta(d llm.Delta) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deltas = append(s.deltas, d)
}

func (s *streamingSink) Asked() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.asked
}

func (s *streamingSink) Deltas() []llm.Delta {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]llm.Delta, len(s.deltas))
	copy(out, s.deltas)

	return out
}

func streamingRun(provider llm.Provider, events agent.Events) (*agent.Result, error) {
	GinkgoHelper()

	app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())

	return agent.Run(context.Background(), agent.Options{
		Config:     agenttest.Config(GinkgoTB(), app),
		ConfigFile: "agent.yaml",
		Prompt:     []string{"go"},
		Provider:   provider,
	}, events, agenttest.NewScriptedPrompter(GinkgoTB()))
}

var _ = Describe("streaming an assistant turn", func() {
	// The fragments a streaming spec sends: two of one block, then its end.
	fragments := []llm.Delta{
		{Kind: llm.DeltaText, Index: 0, Text: "the answer "},
		{Kind: llm.DeltaText, Index: 0, Text: "is 42"},
		{Kind: llm.DeltaText, Index: 0, Final: true},
	}

	// Every sink that exists today implements no half, so this is the case that must not
	// move: a run against a backend that can stream still makes the ordinary call.
	It("Should not stream for a sink that does not implement the half", func() {
		provider := &deltaProvider{deltas: fragments}
		sink := agenttest.NewRecordingEvents()

		res, err := streamingRun(provider, sink)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

		calls, streamed := provider.Counts()
		Expect(calls).To(Equal(1))
		Expect(streamed).To(BeZero(), "a sink that cannot hear fragments should not put the run on the streaming path")

		final, ok := sink.FinalMessage()
		Expect(ok).To(BeTrue())
		Expect(final.Content[0].Text.Text).To(Equal("the answer is 42"))
	})

	// The recorder every hosted run sits behind implements the half unconditionally, so
	// the assertion succeeding is not the opt-in: the answer is.
	It("Should not stream for a sink that answers false", func() {
		provider := &deltaProvider{deltas: fragments}
		sink := newStreamingSink(false)

		res, err := streamingRun(provider, sink)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

		calls, streamed := provider.Counts()
		Expect(calls).To(Equal(1))
		Expect(streamed).To(BeZero())
		Expect(sink.Asked()).To(Equal(1), "the runner asks once per model call")
		Expect(sink.Deltas()).To(BeEmpty())
	})

	// Both sides agreeing is what streams, and the fragments arrive in the order the
	// provider produced them. Message still reports the whole turn afterwards.
	It("Should stream when the sink and the provider both agree", func() {
		provider := &deltaProvider{deltas: fragments}
		sink := newStreamingSink(true)

		res, err := streamingRun(provider, sink)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

		calls, streamed := provider.Counts()
		Expect(streamed).To(Equal(1))
		Expect(calls).To(BeZero())
		Expect(sink.Deltas()).To(Equal(fragments))

		final, ok := sink.FinalMessage()
		Expect(ok).To(BeTrue())
		Expect(final.Content[0].Text.Text).To(Equal("the answer is 42"))
	})

	// A sink asking for fragments from a backend that has none gets the ordinary call
	// and the whole turn, with nothing reported about the difference.
	It("Should make the ordinary call when the provider cannot stream", func() {
		provider := &plainProvider{}
		sink := newStreamingSink(true)

		res, err := streamingRun(provider, sink)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

		Expect(provider.Calls()).To(Equal(1))
		Expect(sink.Deltas()).To(BeEmpty())
		Expect(sink.Warnings()).To(BeEmpty())
	})
})
