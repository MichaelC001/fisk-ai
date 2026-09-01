//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agenttest_test

import (
	"encoding/json"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/llm"
)

var _ = Describe("RecordingEvents", func() {
	It("Should record each kind of event in the order it arrived", func() {
		e := agenttest.NewRecordingEvents()

		e.Starting(agent.RunInfo{Tools: 3})
		e.Warn(agent.Warning{Kind: agent.WarnUnknownTool, Name: "nope"})
		e.ToolCall(agent.ToolTrace{ID: "call-1", Name: "echo"})
		e.ToolResult(agent.ToolResultTrace{CallID: "call-1", Output: "hi"})
		e.Message(*agenttest.TextResponse("thinking out loud"), false)
		e.Message(*agenttest.TextResponse("the answer"), true)
		e.Panicked("boom", []byte("stack"))

		Expect(e.Starts()).To(HaveLen(1))
		Expect(e.Starts()[0].Tools).To(Equal(3))

		Expect(e.Warnings()).To(HaveLen(1))
		Expect(e.Warnings()[0].Name).To(Equal("nope"))
		Expect(e.HasWarning(agent.WarnUnknownTool)).To(BeTrue())
		Expect(e.HasWarning(agent.WarnMissingRequired)).To(BeFalse())

		Expect(e.ToolCalls()).To(HaveLen(1))
		Expect(e.ToolCalls()[0].ID).To(Equal("call-1"))
		Expect(e.ToolResults()).To(HaveLen(1))
		Expect(e.ToolResults()[0].Output).To(Equal("hi"))

		Expect(e.Messages()).To(HaveLen(2))
		Expect(e.Messages()[0].Terminal).To(BeFalse())
		Expect(e.Messages()[1].Terminal).To(BeTrue())

		Expect(e.Panics()).To(HaveLen(1))
		Expect(e.Panics()[0].Value).To(Equal("boom"))
		Expect(e.Panics()[0].Stack).To(Equal([]byte("stack")))
	})

	It("Should record the session a context reset left resumable", func() {
		e := agenttest.NewRecordingEvents()

		Expect(e.SessionRotations()).To(BeEmpty())

		e.SessionRotated("session-1")
		e.SessionRotated("session-2")

		Expect(e.SessionRotations()).To(Equal([]string{"session-1", "session-2"}))

		e.SessionRotations()[0] = "rewritten"
		Expect(e.SessionRotations()[0]).To(Equal("session-1"))
	})

	It("Should report the last terminal turn as the final message", func() {
		e := agenttest.NewRecordingEvents()

		_, ok := e.FinalMessage()
		Expect(ok).To(BeFalse(), "a run that reached no terminal turn has no final message")

		e.Message(*agenttest.TextResponse("first answer"), true)
		e.Message(*agenttest.TextResponse("narration"), false)
		e.Message(*agenttest.TextResponse("second answer"), true)

		final, ok := e.FinalMessage()
		Expect(ok).To(BeTrue())
		Expect(final.Content[0].Text.Text).To(Equal("second answer"))
	})

	It("Should accept an LLMRequest summary and record nothing for it", func() {
		e := agenttest.NewRecordingEvents()

		e.LLMRequest("2 messages, 4 tools")

		Expect(e.Warnings()).To(BeEmpty())
		Expect(e.Messages()).To(BeEmpty())
		Expect(e.Starts()).To(BeEmpty())
		Expect(e.ToolCalls()).To(BeEmpty())
		Expect(e.ToolResults()).To(BeEmpty())
	})

	It("Should return copies a caller may write to", func() {
		e := agenttest.NewRecordingEvents()

		e.Warn(agent.Warning{Name: "original"})
		e.Message(*agenttest.TextResponse("original"), true)
		e.ToolCall(agent.ToolTrace{Name: "original"})
		e.ToolResult(agent.ToolResultTrace{Output: "original"})
		e.Starting(agent.RunInfo{SessionID: "original"})
		e.Panicked("original", nil)

		e.Warnings()[0].Name = "rewritten"
		e.Messages()[0].Terminal = false
		e.ToolCalls()[0].Name = "rewritten"
		e.ToolResults()[0].Output = "rewritten"
		e.Starts()[0].SessionID = "rewritten"
		e.Panics()[0].Value = "rewritten"

		Expect(e.Warnings()[0].Name).To(Equal("original"))
		Expect(e.Messages()[0].Terminal).To(BeTrue())
		Expect(e.ToolCalls()[0].Name).To(Equal("original"))
		Expect(e.ToolResults()[0].Output).To(Equal("original"))
		Expect(e.Starts()[0].SessionID).To(Equal("original"))
		Expect(e.Panics()[0].Value).To(Equal("original"))
	})

	// The recorder is the fixture for a sink that opts into nothing, so a spec asserting
	// what a run does for a sink with no optional half has one to hand it.
	It("Should implement none of the four optional halves of agent.Events", func() {
		var e agent.Events = agenttest.NewRecordingEvents()

		_, streamer := e.(agent.MessageStreamer)
		Expect(streamer).To(BeFalse())

		_, remote := e.(agent.RemoteHostReporter)
		Expect(remote).To(BeFalse())

		_, mcp := e.(agent.MCPServerReporter)
		Expect(mcp).To(BeFalse())

		_, transcript := e.(agent.TranscriptReplayer)
		Expect(transcript).To(BeFalse())
	})

	It("Should aggregate several runs reporting at once", func() {
		const runs = 8
		const each = 10

		e := agenttest.NewRecordingEvents()

		var wg sync.WaitGroup
		for i := 0; i < runs; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer GinkgoRecover()

				e.Starting(agent.RunInfo{Tools: 1})
				for j := 0; j < each; j++ {
					e.Warn(agent.Warning{Kind: agent.WarnUnknownTool})
					e.ToolCall(agent.ToolTrace{Name: "echo"})
					e.ToolResult(agent.ToolResultTrace{Output: "ok"})
					e.Message(*agenttest.TextResponse("turn"), false)
				}
				e.Message(*agenttest.TextResponse("answer"), true)
			}()
		}

		// The accessors are read while the runs are still writing.
		Eventually(e.Starts).Should(HaveLen(runs))
		wg.Wait()

		Expect(e.Warnings()).To(HaveLen(runs * each))
		Expect(e.ToolCalls()).To(HaveLen(runs * each))
		Expect(e.ToolResults()).To(HaveLen(runs * each))
		Expect(e.Messages()).To(HaveLen(runs * (each + 1)))
	})
})

var _ = Describe("RecordingStreamEvents", func() {
	It("Should implement the streaming half of agent.Events", func() {
		var e agent.Events = agenttest.NewRecordingStreamEvents(true)

		_, streamer := e.(agent.MessageStreamer)
		Expect(streamer).To(BeTrue())
	})

	It("Should answer StreamDeltas with what it was built with and count every ask", func() {
		declines := agenttest.NewRecordingStreamEvents(false)
		Expect(declines.StreamDeltas()).To(BeFalse())
		Expect(declines.StreamDeltas()).To(BeFalse())
		Expect(declines.Asked()).To(Equal(2), "the runner asks once per model call whatever the answer")

		accepts := agenttest.NewRecordingStreamEvents(true)
		Expect(accepts.StreamDeltas()).To(BeTrue())
		Expect(accepts.Asked()).To(Equal(1))
	})

	// A run that ends between the provider returning and Message leaves fragments with no
	// turn to reconcile them against. The next call drops them, so its record carries its
	// own fragments and its own block indexes alone.
	It("Should drop the fragments of a call that never reached Message", func() {
		e := agenttest.NewRecordingStreamEvents(true)

		e.StreamDeltas()
		e.MessageDelta(llm.Delta{Kind: llm.DeltaText, Index: 1, Text: "abandoned"})

		e.StreamDeltas()
		e.MessageDelta(llm.Delta{Kind: llm.DeltaText, Index: 0, Text: "kept"})
		e.Message(*agenttest.TextResponse("kept"), true)

		calls := e.Calls()
		Expect(calls).To(HaveLen(1), "a call the run abandoned is not among them")
		Expect(calls[0].Deltas).To(HaveLen(1))
		Expect(calls[0].Deltas[0].Text).To(Equal("kept"))
		Expect(calls[0].Blocks).To(HaveLen(1), "the abandoned call's index 1 went with it")
		Expect(calls[0].Blocks[0].Index).To(Equal(0))
		Expect(calls[0].Blocks[0].Text).To(Equal("kept"))

		Expect(e.Deltas()).To(HaveLen(2), "every fragment the recorder was sent is still here")
		Expect(e.Deltas()[0].Text).To(Equal("abandoned"))
	})

	It("Should reconcile a whole block over the fragments of its index", func() {
		e := agenttest.NewRecordingStreamEvents(true)

		e.StreamDeltas()
		e.MessageDelta(llm.Delta{Kind: llm.DeltaText, Index: 0, Text: "the ans"})
		e.MessageDelta(llm.Delta{Kind: llm.DeltaText, Index: 0, Text: "wer", Final: true})
		e.Message(*agenttest.TextResponse("the answer"), true)

		calls := e.Calls()
		Expect(calls).To(HaveLen(1))
		Expect(calls[0].Terminal).To(BeTrue())
		Expect(calls[0].Blocks[0].Source).To(Equal(llm.SourceBlock), "a provider cuts nothing")
		Expect(calls[0].Blocks[0].Text).To(Equal("the answer"))

		// The turn reaches the embedded recorder too, so a spec reads it the way it does
		// for a sink that streams nothing.
		final, ok := e.FinalMessage()
		Expect(ok).To(BeTrue())
		Expect(final.Content[0].Text.Text).To(Equal("the answer"))
		Expect(e.Messages()).To(HaveLen(1))
	})

	It("Should keep the fragments over a whole block that arrived trimmed", func() {
		e := agenttest.NewRecordingStreamEvents(true)

		e.StreamDeltas()
		e.MessageDelta(llm.Delta{Kind: llm.DeltaText, Index: 0, Text: "the whole long answer"})

		block := e.AddBlock(llm.WholeBlock{Kind: llm.DeltaText, Index: 0, Text: "the whole", Trimmed: true})
		Expect(block.Source).To(Equal(llm.SourceKeptFragments))
		Expect(block.Text).To(Equal("the whole long answer"))
	})

	It("Should leave a tool call's index out of the blocks", func() {
		e := agenttest.NewRecordingStreamEvents(true)

		e.StreamDeltas()
		e.MessageDelta(llm.Delta{Kind: llm.DeltaText, Index: 0, Text: "running it"})

		resp := llm.Response{
			StopReason: llm.StopToolUse,
			Content: []llm.ContentBlock{
				{Text: &llm.TextBlock{Text: "running it"}},
				{ToolUse: &llm.ToolUseBlock{ID: "call-1", Name: "echo", Input: json.RawMessage(`{}`)}},
			},
		}
		e.Message(resp, false)

		blocks := e.Calls()[0].Blocks
		Expect(blocks).To(HaveLen(1), "a tool call streams no fragments and is no text block")
		Expect(blocks[0].Index).To(Equal(0))
	})

	It("Should report what a call assembles to while it is still being written", func() {
		e := agenttest.NewRecordingStreamEvents(true)

		e.StreamDeltas()
		e.MessageDelta(llm.Delta{Kind: llm.DeltaThinking, Index: 0, Text: "half a "})

		mid := e.Assembled()
		Expect(mid).To(HaveLen(1))
		Expect(mid[0].Kind).To(Equal(llm.DeltaThinking))
		Expect(mid[0].Text).To(Equal("half a "))
		Expect(mid[0].Source).To(Equal(llm.SourceFragments))

		e.MessageDelta(llm.Delta{Kind: llm.DeltaThinking, Index: 0, Text: "thought"})
		Expect(e.Assembled()[0].Text).To(Equal("half a thought"))
	})

	It("Should return copies of the deltas and the calls", func() {
		e := agenttest.NewRecordingStreamEvents(true)

		e.StreamDeltas()
		e.MessageDelta(llm.Delta{Kind: llm.DeltaText, Index: 0, Text: "original"})
		e.Message(*agenttest.TextResponse("original"), true)

		e.Deltas()[0].Text = "rewritten"
		e.Calls()[0].Terminal = false

		Expect(e.Deltas()[0].Text).To(Equal("original"))
		Expect(e.Calls()[0].Terminal).To(BeTrue())
	})

	It("Should lock every method against concurrent readers", func() {
		e := agenttest.NewRecordingStreamEvents(true)

		var wg sync.WaitGroup

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer GinkgoRecover()

			for i := 0; i < 200; i++ {
				e.StreamDeltas()
				e.MessageDelta(llm.Delta{Kind: llm.DeltaText, Index: 0, Text: "fragment"})
				e.Message(*agenttest.TextResponse("turn"), false)
			}
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer GinkgoRecover()

			for i := 0; i < 200; i++ {
				e.Deltas()
				e.Calls()
				e.Assembled()
				e.Asked()
			}
		}()

		wg.Wait()

		Expect(e.Calls()).To(HaveLen(200))
	})
})
