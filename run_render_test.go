//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/a2a/transcript"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/tui"
)

var _ = Describe("blockRenderer", func() {
	// One conversation, told twice: as the blocks a run sends while it happens, and as
	// the blocks the journal of that run produces afterwards. This is the defect the
	// whole shape exists to remove, so it is the first thing the suite asserts.
	Describe("One renderer, two sources", func() {
		// live is what the worker's sink sends for a turn that called two tools and then
		// answered. The prompt is not among them: the caller wrote it and knows it.
		live := []a2a.Block{
			a2a.NewToolCallBlock("toolu_1", "stream_ls", json.RawMessage(`{"all":true}`)),
			a2a.NewToolResultBlock("toolu_1", "ORDERS\nEVENTS", false),
			a2a.NewToolCallBlock("toolu_2", "stream_info", json.RawMessage(`{"stream":"ORDERS"}`)),
			a2a.NewToolResultBlock("toolu_2", "12 messages", false),
			a2a.NewFinalTextBlock("there are two streams"),
		}

		// stored is the journal that run left: every call in the assistant message and
		// every result in the message after it, which is not the order they happened in.
		stored := &runstate.RunState{
			Messages: []llm.Message{
				{Role: llm.RoleUser, Content: []llm.ContentBlock{
					{Text: &llm.TextBlock{Text: "how many streams"}},
				}},
				{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
					{ToolUse: &llm.ToolUseBlock{ID: "toolu_1", Name: "stream_ls", Input: json.RawMessage(`{"all":true}`)}},
					{ToolUse: &llm.ToolUseBlock{ID: "toolu_2", Name: "stream_info", Input: json.RawMessage(`{"stream":"ORDERS"}`)}},
				}},
				{Role: llm.RoleUser, Content: []llm.ContentBlock{
					{ToolResult: &llm.ToolResultBlock{ToolUseID: "toolu_1", Content: "ORDERS\nEVENTS"}},
					{ToolResult: &llm.ToolResultBlock{ToolUseID: "toolu_2", Content: "12 messages"}},
				}},
				{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
					{Text: &llm.TextBlock{Text: "there are two streams"}},
				}},
			},
		}

		It("Should render a run and its journal as the same lines", func() {
			replayed := renderBlocks(transcript.Of(stored).Blocks(), false)

			// The stored conversation opens with the prompt, which a live caller wrote
			// rather than received. Everything after it is the same run.
			Expect(replayed[0].Kind).To(Equal(tui.LinePrompt))
			Expect(replayed[1:]).To(Equal(renderBlocks(live, false)))
		})
	})

	It("Should set the answer apart and keep its text for the reprint", func() {
		r := &blockRenderer{}

		lines := r.Lines(a2a.NewTextBlock("looking that up"))
		Expect(lines).To(HaveLen(1))
		Expect(lines[0].Kind).To(Equal(tui.LineNarration))
		Expect(r.answer).To(BeEmpty(), "narration is not the answer")

		lines = r.Lines(a2a.NewFinalTextBlock("there are two"))
		Expect(lines).To(HaveLen(2))
		Expect(lines[0]).To(Equal(tui.Line{Kind: tui.LineMeta, Text: "--- answer ---"}))
		Expect(lines[1].Text).To(Equal("there are two"))
		Expect(r.answer).To(Equal("there are two"))
	})

	It("Should keep a warning for the reprint and drop one it cannot word", func() {
		r := &blockRenderer{}

		lines := r.Lines(a2a.NewBlock(a2a.WarningBlock{Kind: "unknown_tool", Name: "nosuchtool"}))
		Expect(lines).To(HaveLen(1))
		Expect(lines[0].Kind).To(Equal(tui.LineWarning))
		Expect(r.warnings).To(HaveLen(1))
		Expect(r.warnings[0]).To(ContainSubstring("nosuchtool"))

		// A kind this build does not know still says something: the run raised it, and
		// its fields are what the wire carried.
		lines = r.Lines(a2a.NewBlock(a2a.WarningBlock{Kind: "from_a_newer_peer", Name: "thing"}))
		Expect(lines).To(HaveLen(1))
		Expect(r.warnings).To(HaveLen(2))
	})

	It("Should show reasoning only when it was asked for", func() {
		Expect((&blockRenderer{}).Lines(a2a.NewThinkingBlock("hmm"))).To(BeEmpty())

		lines := (&blockRenderer{showThinking: true}).Lines(a2a.NewThinkingBlock("hmm"))
		Expect(lines).To(HaveLen(1))
		Expect(lines[0].Kind).To(Equal(tui.LineThinking))
	})

	// A progress status paces a caller and has nothing to show a person; a replay
	// marker says which part of the view already happened.
	It("Should mark a replay and show nothing for progress", func() {
		Expect(statusLines(a2a.StatusBlock{Iteration: 3})).To(BeEmpty())

		Expect(statusLines(a2a.StatusBlock{Phase: a2a.PhaseReplayStart})).To(HaveLen(1))
		Expect(statusLines(a2a.StatusBlock{Phase: a2a.PhaseReplayEnd})).To(HaveLen(1))

		truncated := statusLines(a2a.StatusBlock{Phase: a2a.PhaseReplayEnd, Count: 40, Truncated: true})
		Expect(truncated).To(HaveLen(2))
		Expect(truncated[0].Text).To(ContainSubstring("last 40 blocks"))
	})
})

var _ = Describe("liveUsage", func() {
	// The wire counts cache with the rest of the input, since a caller reading a bill
	// wants what it was billed for. The status bar shows the uncached remainder beside a
	// cache figure of its own, so feeding the total through would count cache twice.
	It("Should split the cache back out of the input total", func() {
		in, out, cacheRead, cacheCreate, thinking := liveUsage(&a2a.Usage{
			InputTokens:       1000,
			OutputTokens:      200,
			CacheReadTokens:   700,
			CacheCreateTokens: 100,
			ThinkingTokens:    50,
		})

		Expect(in).To(Equal(int64(200)))
		Expect(out).To(Equal(int64(200)))
		Expect(cacheRead).To(Equal(int64(700)))
		Expect(cacheCreate).To(Equal(int64(100)))
		Expect(thinking).To(Equal(int64(50)))
	})

	It("Should not report a negative remainder from a peer that counts differently", func() {
		in, _, _, _, _ := liveUsage(&a2a.Usage{InputTokens: 10, CacheReadTokens: 99})
		Expect(in).To(BeZero())
	})
})

var _ = Describe("exportsFromCard", func() {
	// The process that exports is the one running the agent, so a client reading its own
	// provider would answer for the wrong machine. What the agent said is the answer.
	It("Should report what the agent said", func() {
		exports, content := exportsFromCard(&a2a.AgentCard{Telemetry: true, TelemetryContent: true})
		Expect(exports).To(BeTrue())
		Expect(content).To(Equal(tui.ContentExported))

		exports, content = exportsFromCard(&a2a.AgentCard{Telemetry: true})
		Expect(exports).To(BeTrue())
		Expect(content).To(Equal(tui.ContentNotExported))

		exports, content = exportsFromCard(&a2a.AgentCard{})
		Expect(exports).To(BeFalse())
		Expect(content).To(Equal(tui.ContentNotExported))
	})

	// Not knowing must not read as no. An agent that was reachable and did not answer in
	// time has told the client nothing, and a privacy marker that treated silence as
	// reassurance would be wrong in the direction that matters.
	It("Should report silence as unknown rather than as no", func() {
		exports, content := exportsFromCard(nil)
		Expect(exports).To(BeFalse())
		Expect(content).To(Equal(tui.ContentExportUnknown))
	})
})

var _ = Describe("endsSession", func() {
	// A turn that failed, was refused for want of capacity or was not taken leaves the
	// conversation where it was, so the input row reopens and the operator decides.
	It("Should keep the conversation open for an ending it can retry", func() {
		Expect(endsSession(a2a.CodeFailed)).To(BeFalse())
		Expect(endsSession(a2a.CodeCapacity)).To(BeFalse())
		Expect(endsSession(a2a.CodeConversationBusy)).To(BeFalse())
		Expect(endsSession(a2a.CodeTurnNotTaken)).To(BeFalse())
	})

	It("Should end it for an ending the conversation does not continue past", func() {
		Expect(endsSession(a2a.CodeSuspended)).To(BeTrue())
		Expect(endsSession(a2a.CodeDeferred)).To(BeTrue())
		Expect(endsSession(a2a.CodeCrashed)).To(BeTrue())
		Expect(endsSession(a2a.CodeUnknownConversation)).To(BeTrue())
	})
})
