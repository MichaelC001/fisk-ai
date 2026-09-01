//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agenttest_test

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/llm"
)

// goroutineID reads the id off the header of the calling goroutine's own stack, so a spec
// can tell which goroutine a delta function ran on.
func goroutineID() uint64 {
	var buf [64]byte

	// The header is "goroutine N [running]:", the only place the runtime publishes the id.
	header := string(buf[:runtime.Stack(buf[:], false)])
	header = strings.TrimPrefix(header, "goroutine ")

	id, err := strconv.ParseUint(header[:strings.IndexByte(header, ' ')], 10, 64)
	Expect(err).ToNot(HaveOccurred())

	return id
}

var _ = Describe("ScriptedProvider", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("Should implement llm.Provider", func() {
		var p llm.Provider = agenttest.NewScriptedProvider(GinkgoTB())
		Expect(p).ToNot(BeNil())
	})

	It("Should implement llm.StreamingProvider", func() {
		var p llm.StreamingProvider = agenttest.NewScriptedProvider(GinkgoTB())
		Expect(p).ToNot(BeNil())
	})

	It("Should answer successive calls with the scripted responses in order", func() {
		p := agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("call-1", "echo", json.RawMessage(`{"text":"hi"}`)),
			agenttest.TextResponse("done"))

		first, err := p.Call(ctx, llm.Request{})
		Expect(err).ToNot(HaveOccurred())
		Expect(first.StopReason).To(Equal(llm.StopToolUse))
		Expect(first.Content[0].ToolUse.Name).To(Equal("echo"))

		second, err := p.Call(ctx, llm.Request{})
		Expect(err).ToNot(HaveOccurred())
		Expect(second.StopReason).To(Equal(llm.StopEndTurn))
		Expect(second.Content[0].Text.Text).To(Equal("done"))
	})

	It("Should fail a call past the end of the script", func() {
		p := agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done"))

		_, err := p.Call(ctx, llm.Request{})
		Expect(err).ToNot(HaveOccurred())

		resp, err := p.Call(ctx, llm.Request{})
		Expect(resp).To(BeNil())
		Expect(err).To(MatchError(ContainSubstring("call 2 exceeds 1 scripted responses")))
	})

	It("Should record every request it was called with, in order", func() {
		p := agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.TextResponse("one"), agenttest.TextResponse("two"))

		_, err := p.Call(ctx, llm.Request{Model: "first"})
		Expect(err).ToNot(HaveOccurred())
		_, err = p.Call(ctx, llm.Request{Model: "second"})
		Expect(err).ToNot(HaveOccurred())

		requests := p.Requests()
		Expect(requests).To(HaveLen(2))
		Expect(requests[0].Model).To(Equal("first"))
		Expect(requests[1].Model).To(Equal("second"))

		requests[0].Model = "rewritten"
		Expect(p.Requests()[0].Model).To(Equal("first"), "Requests returns a copy")
	})

	It("Should record the request of a call that ran off the end of the script", func() {
		p := agenttest.NewScriptedProvider(GinkgoTB())

		_, err := p.Call(ctx, llm.Request{Model: "unanswered"})
		Expect(err).To(HaveOccurred())

		Expect(p.Requests()).To(HaveLen(1))
		Expect(p.Requests()[0].Model).To(Equal("unanswered"))
	})

	It("Should declare an anthropic-shaped provider until a spec says otherwise", func() {
		p := agenttest.NewScriptedProvider(GinkgoTB())

		Expect(p.Capabilities().Provider).To(Equal("anthropic"))
		Expect(p.Capabilities().SupportsToolSearch).To(BeTrue())

		p.SetCapabilities(llm.Caps{Provider: "openai"})
		Expect(p.Capabilities().Provider).To(Equal("openai"))
		Expect(p.Capabilities().SupportsToolSearch).To(BeFalse())
	})

	It("Should refuse a nil response by position", func() {
		p, err := agenttest.BuildScriptedProvider(agenttest.TextResponse("one"), nil)
		Expect(p).To(BeNil())
		Expect(err).To(MatchError(ContainSubstring("scripted response 1 is nil")))
	})

	It("Should build a terminal text turn and a tool call turn", func() {
		text := agenttest.TextResponse("the answer")
		Expect(text.StopReason).To(Equal(llm.StopEndTurn))
		Expect(text.Content).To(HaveLen(1))
		Expect(text.Content[0].Text.Text).To(Equal("the answer"))

		call := agenttest.ToolUseResponse("call-1", "echo", json.RawMessage(`{"text":"hi"}`))
		Expect(call.StopReason).To(Equal(llm.StopToolUse))
		Expect(call.Content).To(HaveLen(1))
		Expect(call.Content[0].ToolUse.ID).To(Equal("call-1"))
		Expect(call.Content[0].ToolUse.Name).To(Equal("echo"))
		Expect(string(call.Content[0].ToolUse.Input)).To(Equal(`{"text":"hi"}`))
	})

	It("Should serve calls made from several goroutines at once", func() {
		const calls = 32

		responses := make([]*llm.Response, 0, calls)
		for i := 0; i < calls; i++ {
			responses = append(responses, agenttest.TextResponse("done"))
		}

		p := agenttest.NewScriptedProvider(GinkgoTB(), responses...)

		var wg sync.WaitGroup
		for i := 0; i < calls; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer GinkgoRecover()

				_, err := p.Call(ctx, llm.Request{Model: "test-model"})
				Expect(err).ToNot(HaveOccurred())

				p.Capabilities()
				p.Requests()
			}()
		}

		wg.Wait()

		Expect(p.Requests()).To(HaveLen(calls))
	})

	Describe("Faults", func() {
		DescribeTable("Should return the sentinel a spec injected on both call paths",
			func(sentinel error) {
				p := agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("never reached"))
				p.SetEveryCallFault(agenttest.Fault{Err: sentinel})

				_, err := p.Call(ctx, llm.Request{})
				Expect(errors.Is(err, sentinel)).To(BeTrue(), "Call returned %v", err)

				_, err = p.CallStream(ctx, llm.Request{}, func(llm.Delta) {})
				Expect(errors.Is(err, sentinel)).To(BeTrue(), "CallStream returned %v", err)
			},
			Entry("rate limited", llm.ErrRateLimited),
			Entry("overloaded", llm.ErrOverloaded),
			Entry("authentication", llm.ErrAuthentication),
			Entry("context length exceeded", llm.ErrContextLengthExceeded),
			Entry("invalid request", llm.ErrInvalidRequest),
			Entry("model not found", llm.ErrModelNotFound),
			Entry("request too large", llm.ErrRequestTooLarge),
			Entry("backend failure", llm.ErrBackendFailure),
		)

		It("Should fail the nth call and answer the ones before it from the script", func() {
			p := agenttest.NewScriptedProvider(GinkgoTB(),
				agenttest.TextResponse("one"), agenttest.TextResponse("two"), agenttest.TextResponse("three"))
			p.SetCallFault(2, agenttest.Fault{Err: llm.ErrRateLimited})

			first, err := p.Call(ctx, llm.Request{})
			Expect(err).ToNot(HaveOccurred())
			Expect(first.Content[0].Text.Text).To(Equal("one"))

			_, err = p.Call(ctx, llm.Request{})
			Expect(err).To(MatchError(llm.ErrRateLimited))

			third, err := p.Call(ctx, llm.Request{})
			Expect(err).ToNot(HaveOccurred())
			Expect(third.Content[0].Text.Text).To(Equal("three"), "the failed call spent its script position")

			Expect(p.Requests()).To(HaveLen(3))
		})

		It("Should let a call number override the fault set for every call", func() {
			p := agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("one"), agenttest.TextResponse("two"))
			p.SetEveryCallFault(agenttest.Fault{Err: llm.ErrOverloaded})
			p.SetCallFault(2, agenttest.Fault{})

			_, err := p.Call(ctx, llm.Request{})
			Expect(err).To(MatchError(llm.ErrOverloaded))

			second, err := p.Call(ctx, llm.Request{})
			Expect(err).ToNot(HaveOccurred())
			Expect(second.Content[0].Text.Text).To(Equal("two"))
		})

		It("Should fail a call the script has no response for", func() {
			p := agenttest.NewScriptedProvider(GinkgoTB())
			p.SetEveryCallFault(agenttest.Fault{Err: llm.ErrAuthentication})

			_, err := p.Call(ctx, llm.Request{})
			Expect(err).To(MatchError(llm.ErrAuthentication))
			Expect(err).ToNot(MatchError(ContainSubstring("exhausted")))
		})

		It("Should hand a delay to the waiter rather than sleeping through it", func() {
			p := agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("slow"))
			p.SetCallFault(1, agenttest.Fault{Delay: time.Hour})

			var waited []time.Duration
			p.SetWaiter(func(_ context.Context, d time.Duration) error {
				waited = append(waited, d)
				return nil
			})

			started := time.Now()
			resp, err := p.Call(ctx, llm.Request{})
			Expect(err).ToNot(HaveOccurred())
			Expect(resp.Content[0].Text.Text).To(Equal("slow"))

			Expect(waited).To(Equal([]time.Duration{time.Hour}))
			Expect(time.Since(started)).To(BeNumerically("<", time.Second))
		})

		It("Should return what the waiter returned instead of the scripted response", func() {
			p := agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("never reached"))
			p.SetEveryCallFault(agenttest.Fault{Delay: time.Hour})
			p.SetWaiter(func(context.Context, time.Duration) error { return context.DeadlineExceeded })

			resp, err := p.Call(ctx, llm.Request{})
			Expect(resp).To(BeNil())
			Expect(err).To(MatchError(context.DeadlineExceeded))

			_, err = p.CallStream(ctx, llm.Request{}, func(llm.Delta) {
				Fail("a call the deadline ended sent a fragment")
			})
			Expect(err).To(MatchError(context.DeadlineExceeded))
		})

		It("Should wait on the default timer again when a spec sets a nil waiter", func() {
			p := agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("slow"))
			p.SetCallFault(1, agenttest.Fault{Delay: time.Hour})
			p.SetWaiter(func(context.Context, time.Duration) error { return nil })
			p.SetWaiter(nil)

			// The default waiter ends on the context, so a canceled one answers the hour at
			// once and the spec neither waits it out nor calls a nil waiter.
			callCtx, cancel := context.WithCancel(ctx)
			cancel()

			resp, err := p.Call(callCtx, llm.Request{})
			Expect(resp).To(BeNil())
			Expect(err).To(MatchError(context.Canceled))
		})

		It("Should leave the waiter out of a call with no delay", func() {
			p := agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("prompt"))
			p.SetWaiter(func(context.Context, time.Duration) error {
				Fail("the waiter was called for a call with no delay")
				return nil
			})

			_, err := p.Call(ctx, llm.Request{})
			Expect(err).ToNot(HaveOccurred())
		})
	})

	Describe("CallStream", func() {
		It("Should refuse a nil delta function without spending a script position", func() {
			p := agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("one"))

			resp, err := p.CallStream(ctx, llm.Request{}, nil)
			Expect(resp).To(BeNil())
			Expect(err).To(MatchError(ContainSubstring("requires a delta function")))
			Expect(p.Requests()).To(BeEmpty())

			answered, err := p.CallStream(ctx, llm.Request{}, func(llm.Delta) {})
			Expect(err).ToNot(HaveOccurred())
			Expect(answered.Content[0].Text.Text).To(Equal("one"))
		})

		It("Should send fragments in order and return the response Call returns for the position", func() {
			script := []*llm.Response{{
				StopReason: llm.StopEndTurn,
				Content: []llm.ContentBlock{
					{Thinking: &llm.ThinkingBlock{Text: "it is 42"}},
					{ToolUse: &llm.ToolUseBlock{ID: "call-1", Name: "echo", Input: json.RawMessage(`{}`)}},
					{Text: &llm.TextBlock{Text: "the answer is 42"}},
				},
			}}

			called, err := agenttest.NewScriptedProvider(GinkgoTB(), script...).Call(ctx, llm.Request{})
			Expect(err).ToNot(HaveOccurred())

			var seen []llm.Delta
			var assembler llm.DeltaAssembler

			streamed, err := agenttest.NewScriptedProvider(GinkgoTB(), script...).CallStream(ctx, llm.Request{}, func(d llm.Delta) {
				seen = append(seen, d)
				assembler.AddDelta(d)
			})
			Expect(err).ToNot(HaveOccurred())
			Expect(streamed).To(Equal(called))

			Expect(seen).To(Equal([]llm.Delta{
				{Kind: llm.DeltaThinking, Index: 0, Text: "it "},
				{Kind: llm.DeltaThinking, Index: 0, Text: "is "},
				{Kind: llm.DeltaThinking, Index: 0, Text: "42"},
				{Kind: llm.DeltaThinking, Index: 0, Final: true},
				{Kind: llm.DeltaText, Index: 2, Text: "the "},
				{Kind: llm.DeltaText, Index: 2, Text: "answer "},
				{Kind: llm.DeltaText, Index: 2, Text: "is "},
				{Kind: llm.DeltaText, Index: 2, Text: "42"},
				{Kind: llm.DeltaText, Index: 2, Final: true},
			}), "the tool call streams nothing and its index is absent")

			// The fragments of every index carry the text the returned turn has in that
			// block, which is what a consumer reconciles against.
			Expect(assembler.Blocks()).To(Equal([]llm.AssembledBlock{
				{Kind: llm.DeltaThinking, Index: 0, Text: "it is 42", Source: llm.SourceFragments},
				{Kind: llm.DeltaText, Index: 2, Text: "the answer is 42", Source: llm.SourceFragments},
			}))
		})

		// agent.runner writes and reads the timestamp of the first fragment without a lock
		// because llm.StreamingProvider promises this, so a fake that moved fn to another
		// goroutine would turn the runner's streaming path into a data race.
		It("Should call fn on the calling goroutine, in order, and not after it returns", func() {
			p := agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("one two three"))

			caller := goroutineID()

			// Appended without a lock, so a fn running anywhere else is a race the detector
			// reports as well as an id these assertions catch.
			var ids []uint64
			var text []string
			returned := false

			resp, err := p.CallStream(ctx, llm.Request{}, func(d llm.Delta) {
				Expect(returned).To(BeFalse(), "fn ran after CallStream returned")

				ids = append(ids, goroutineID())
				text = append(text, d.Text)
			})
			returned = true

			Expect(err).ToNot(HaveOccurred())
			Expect(resp.Content[0].Text.Text).To(Equal("one two three"))

			Expect(text).To(Equal([]string{"one ", "two ", "three", ""}))
			for _, id := range ids {
				Expect(id).To(Equal(caller))
			}
		})

		It("Should send the fragments a spec wrote for a call in place of the derived ones", func() {
			p := agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("one two"))
			p.SetCallDeltas(1,
				llm.Delta{Kind: llm.DeltaThinking, Index: 0, Text: "think"},
				llm.Delta{Kind: llm.DeltaText, Index: 1, Text: "one "},
				llm.Delta{Kind: llm.DeltaThinking, Index: 0, Final: true},
				llm.Delta{Kind: llm.DeltaText, Index: 1, Text: "two"},
				llm.Delta{Kind: llm.DeltaText, Index: 1, Final: true})

			var seen []llm.Delta
			_, err := p.CallStream(ctx, llm.Request{}, func(d llm.Delta) { seen = append(seen, d) })
			Expect(err).ToNot(HaveOccurred())

			Expect(seen).To(HaveLen(5))
			Expect(seen[0].Index).To(Equal(0))
			Expect(seen[1].Index).To(Equal(1), "two blocks interleave, which the derived fragments never do")
		})

		It("Should report the same exhaustion as Call", func() {
			p := agenttest.NewScriptedProvider(GinkgoTB())

			resp, err := p.CallStream(ctx, llm.Request{Model: "unanswered"}, func(llm.Delta) {
				Fail("an exhausted call sent a fragment")
			})
			Expect(resp).To(BeNil())
			Expect(err).To(MatchError(ContainSubstring("call 1 exceeds 0 scripted responses")))
			Expect(p.Requests()).To(HaveLen(1))
		})

		It("Should keep the fragments it already sent when a fault fires mid-stream", func() {
			p := agenttest.NewScriptedProvider(GinkgoTB(),
				agenttest.TextResponse("one two three"), agenttest.TextResponse("done"))
			p.SetCallFault(1, agenttest.Fault{Err: llm.ErrBackendFailure, AfterFragments: 2})

			rec := agenttest.NewRecordingStreamEvents(true)

			Expect(rec.StreamDeltas()).To(BeTrue())
			resp, err := p.CallStream(ctx, llm.Request{}, rec.MessageDelta)
			Expect(resp).To(BeNil())
			Expect(err).To(MatchError(llm.ErrBackendFailure))

			Expect(rec.Deltas()).To(HaveLen(2))
			Expect(rec.Deltas()[0].Text).To(Equal("one "))
			Expect(rec.Deltas()[1].Text).To(Equal("two "))
			Expect(rec.Calls()).To(BeEmpty(), "a call that failed never reached Message")

			// The next call starts and the recorder drops the failed call's fragments.
			Expect(rec.StreamDeltas()).To(BeTrue())
			Expect(rec.Assembled()).To(BeEmpty())

			resp, err = p.CallStream(ctx, llm.Request{}, rec.MessageDelta)
			Expect(err).ToNot(HaveOccurred())
			rec.Message(*resp, true)

			calls := rec.Calls()
			Expect(calls).To(HaveLen(1))
			Expect(calls[0].Deltas).To(HaveLen(2))
			Expect(calls[0].Deltas[0].Text).To(Equal("done"))
			Expect(calls[0].Blocks).To(Equal([]llm.AssembledBlock{
				{Kind: llm.DeltaText, Index: 0, Text: "done", Source: llm.SourceBlock},
			}))

			Expect(rec.Deltas()).To(HaveLen(4), "Deltas keeps the fragments of the failed call")
		})

		It("Should send every fragment before a fault that fires past the end of the stream", func() {
			p := agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("one two"))
			p.SetCallFault(1, agenttest.Fault{Err: llm.ErrBackendFailure, AfterFragments: 100})

			var seen []llm.Delta
			_, err := p.CallStream(ctx, llm.Request{}, func(d llm.Delta) { seen = append(seen, d) })
			Expect(err).To(MatchError(llm.ErrBackendFailure))
			Expect(seen).To(HaveLen(3))
			Expect(seen[2].Final).To(BeTrue())
		})
	})
})
