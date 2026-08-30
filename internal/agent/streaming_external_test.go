//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// These tests cover the MessageStreamer half of agent.Events: which call the runner
// makes, and which sink receives the fragments when it streams.
package agent_test

import (
	"context"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/telemetry"
)

// deltaProvider answers one terminal turn on either call path and records which path
// was taken, so a spec can tell a streamed call from an ordinary one. CallStream
// reports the fragments it was built with before returning the same turn Call returns,
// which is the equivalence llm.StreamingProvider requires of a backend.
type deltaProvider struct {
	deltas []llm.Delta
	// pause is held once, after the first fragment, so a spec asserting on the time to
	// the first one can tell it from the time to the last: the two are microseconds
	// apart otherwise and either value would pass.
	pause time.Duration

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

	for i, d := range p.deltas {
		if i == 1 {
			time.Sleep(p.pause)
		}

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

func streamingRun(provider llm.Provider, events agent.Events) (*agent.Result, error) {
	GinkgoHelper()

	return tracedStreamingRun(provider, events, nil)
}

// tracedStreamingRun is streamingRun with a telemetry provider, for the specs that read
// what the run's chat span recorded about the call it made.
func tracedStreamingRun(provider llm.Provider, events agent.Events, tel *telemetry.Provider) (*agent.Result, error) {
	GinkgoHelper()

	app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())

	return agent.Run(context.Background(), agent.Options{
		Config:     agenttest.Config(GinkgoTB(), app),
		ConfigFile: "agent.yaml",
		Prompt:     []string{"go"},
		Provider:   provider,
		Telemetry:  tel,
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
		Expect(streamed).To(BeZero(), "a sink that implements no streaming half should not put the run on the streaming path")

		final, ok := sink.FinalMessage()
		Expect(ok).To(BeTrue())
		Expect(final.Content[0].Text.Text).To(Equal("the answer is 42"))
	})

	// The recorder every hosted run sits behind implements the half unconditionally, so
	// the assertion succeeding is not the opt-in; the sink's answer decides.
	It("Should not stream for a sink that answers false", func() {
		provider := &deltaProvider{deltas: fragments}
		sink := agenttest.NewRecordingStreamEvents(false)

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
		sink := agenttest.NewRecordingStreamEvents(true)

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

	// How long a person waits before an answer starts is the number streaming exists to
	// change, and the two durations already on the model path both end elsewhere: the
	// span covers the whole call, and the HTTP attempt ends at the response headers.
	//
	// The provider holds its second fragment back, so a value taken from the last one
	// rather than the first would be past the pause and fail the assertion.
	It("Should record the wait for the first fragment on the chat span", func() {
		pause := 200 * time.Millisecond
		provider := &deltaProvider{deltas: fragments, pause: pause}
		sink := agenttest.NewRecordingStreamEvents(true)
		tel, exp := recordingTelemetry()

		res, err := tracedStreamingRun(provider, sink, tel)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))
		Expect(sink.Deltas()).To(Equal(fragments))

		chat := spanNamed(exp, "chat ")

		streamed, ok := spanAttr(chat, "fisk.llm.streamed")
		Expect(ok).To(BeTrue())
		Expect(streamed.AsBool()).To(BeTrue())

		ttft, ok := spanAttr(chat, "gen_ai.server.time_to_first_token")
		Expect(ok).To(BeTrue())
		Expect(ttft.AsFloat64()).To(BeNumerically(">", 0))
		Expect(ttft.AsFloat64()).To(BeNumerically("<", pause.Seconds()),
			"the value must be the first fragment's arrival, not the last's")
	})

	// A batched call has no first fragment to time and records the flag as false, which
	// tells the two meanings of the HTTP attempt duration apart on a dashboard that mixes
	// streamed and batched runs.
	It("Should record the flag as false and no first-token time for a call that did not stream", func() {
		provider := &deltaProvider{deltas: fragments}
		sink := agenttest.NewRecordingStreamEvents(false)
		tel, exp := recordingTelemetry()

		res, err := tracedStreamingRun(provider, sink, tel)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

		chat := spanNamed(exp, "chat ")

		streamed, ok := spanAttr(chat, "fisk.llm.streamed")
		Expect(ok).To(BeTrue())
		Expect(streamed.AsBool()).To(BeFalse())

		_, ok = spanAttr(chat, "gen_ai.server.time_to_first_token")
		Expect(ok).To(BeFalse())
	})

	// A sink asking for fragments from a backend that has none gets the ordinary call
	// and the whole turn, and no warning.
	It("Should make the ordinary call when the provider cannot stream", func() {
		provider := &plainProvider{}
		sink := agenttest.NewRecordingStreamEvents(true)

		res, err := streamingRun(provider, sink)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

		Expect(provider.Calls()).To(Equal(1))
		Expect(sink.Deltas()).To(BeEmpty())
		Expect(sink.Warnings()).To(BeEmpty())
	})
})
