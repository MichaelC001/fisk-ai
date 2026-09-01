//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agenttest_test

import (
	"context"
	"encoding/json"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/llm"
)

var _ = Describe("ScriptedProvider", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("Should implement llm.Provider", func() {
		var p llm.Provider = agenttest.NewScriptedProvider(GinkgoTB())
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
})
