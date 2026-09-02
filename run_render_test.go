//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/a2a/transcript"
	wire "github.com/choria-io/fisk-ai/internal/a2a/wire/v1"
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
		live := []wire.Block{
			wire.NewToolCallBlock("toolu_1", "stream_ls", json.RawMessage(`{"all":true}`)),
			wire.NewToolResultBlock("toolu_1", "ORDERS\nEVENTS", false),
			wire.NewToolCallBlock("toolu_2", "stream_info", json.RawMessage(`{"stream":"ORDERS"}`)),
			wire.NewToolResultBlock("toolu_2", "12 messages", false),
			wire.NewFinalTextBlock("there are two streams"),
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

		lines := r.Lines(wire.NewTextBlock("looking that up"))
		Expect(lines).To(HaveLen(1))
		Expect(lines[0].Kind).To(Equal(tui.LineNarration))
		Expect(r.answer).To(BeEmpty(), "narration is not the answer")

		lines = r.Lines(wire.NewFinalTextBlock("there are two"))
		Expect(lines).To(HaveLen(2))
		Expect(lines[0]).To(Equal(tui.Line{Kind: tui.LineMeta, Text: "--- answer ---"}))
		Expect(lines[1].Text).To(Equal("there are two"))
		Expect(r.answer).To(Equal("there are two"))
	})

	It("Should keep a warning for the reprint and drop one it cannot word", func() {
		r := &blockRenderer{}

		lines := r.Lines(wire.NewBlock(wire.WarningBlock{Kind: "unknown_tool", Name: "nosuchtool"}))
		Expect(lines).To(HaveLen(1))
		Expect(lines[0].Kind).To(Equal(tui.LineWarning))
		Expect(r.warnings).To(HaveLen(1))
		Expect(r.warnings[0]).To(ContainSubstring("nosuchtool"))

		// A kind this build does not know still says something: the run raised it, and
		// its fields are what the wire carried.
		lines = r.Lines(wire.NewBlock(wire.WarningBlock{Kind: "from_a_newer_peer", Name: "thing"}))
		Expect(lines).To(HaveLen(1))
		Expect(r.warnings).To(HaveLen(2))
	})

	It("Should show reasoning only when it was asked for", func() {
		Expect((&blockRenderer{}).Lines(wire.NewThinkingBlock("hmm"))).To(BeEmpty())

		lines := (&blockRenderer{showThinking: true}).Lines(wire.NewThinkingBlock("hmm"))
		Expect(lines).To(HaveLen(1))
		Expect(lines[0].Kind).To(Equal(tui.LineThinking))
	})

	// A progress status paces a caller and has nothing to show a person; a replay
	// marker says which part of the view already happened.
	It("Should mark a replay and show nothing for progress", func() {
		Expect(statusLines(wire.StatusBlock{Iteration: 3})).To(BeEmpty())

		Expect(statusLines(wire.StatusBlock{Phase: wire.PhaseReplayStart})).To(HaveLen(1))
		Expect(statusLines(wire.StatusBlock{Phase: wire.PhaseReplayEnd})).To(HaveLen(1))

		truncated := statusLines(wire.StatusBlock{Phase: wire.PhaseReplayEnd, Count: 40, Truncated: true})
		Expect(truncated).To(HaveLen(2))
		Expect(truncated[0].Text).To(ContainSubstring("last 40 blocks"))
	})
})

var _ = Describe("warningLead", func() {
	// Every warning arrives over a2a whether the agent is behind the embedded broker or
	// on somebody else's machine, so the wording is what tells a reader which failed.
	It("Should name a hosted agent", func() {
		Expect(warningLead("cowsay", "")).To(Equal("warning from agent cowsay"))
	})

	It("Should mark an agent on a bus as remote", func() {
		Expect(warningLead("cowsay", "ngs_user")).To(Equal("warning from remote agent cowsay"))
	})

	It("Should fall back to a bare lead for a run with no identity", func() {
		Expect(warningLead("", "ngs_user")).To(Equal("warning"))
	})

	It("Should keep an identity from carrying control characters to the terminal", func() {
		Expect(warningLead("cow\x1b[31msay", "")).NotTo(ContainSubstring("\x1b"))
	})
})

var _ = Describe("liveUsage", func() {
	// The wire counts cache with the rest of the input, since a caller reading a bill
	// wants what it was billed for. The status bar shows the uncached remainder beside a
	// cache figure of its own, so feeding the total through would count cache twice.
	It("Should split the cache back out of the input total", func() {
		in, out, cacheRead, cacheCreate, thinking := liveUsage(&wire.Usage{
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
		in, _, _, _, _ := liveUsage(&wire.Usage{InputTokens: 10, CacheReadTokens: 99})
		Expect(in).To(BeZero())
	})
})

var _ = Describe("exportsFromCard", func() {
	// The process that exports is the one running the agent, so a client reading its own
	// provider would answer for the wrong machine. What the agent said is the answer.
	It("Should report what the agent said", func() {
		exports, content := exportsFromCard(&wire.AgentCard{Telemetry: true, TelemetryContent: true})
		Expect(exports).To(BeTrue())
		Expect(content).To(Equal(tui.ContentExported))

		exports, content = exportsFromCard(&wire.AgentCard{Telemetry: true})
		Expect(exports).To(BeTrue())
		Expect(content).To(Equal(tui.ContentNotExported))

		exports, content = exportsFromCard(&wire.AgentCard{})
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

var _ = Describe("modelFromCard", func() {
	// The process calling the model is the one running the agent, so this terminal's own
	// configuration answers for the wrong machine the moment the agent is elsewhere.
	It("Should report what the agent said", func() {
		Expect(modelFromCard(&wire.AgentCard{Model: "claude-sonnet-5"})).To(Equal("claude-sonnet-5"))
	})

	// Both surfaces that show it leave their row out when it is empty, which is what an
	// operator should see when nobody has told them what is answering.
	It("Should report nothing where the agent named none, or answered nothing", func() {
		Expect(modelFromCard(&wire.AgentCard{})).To(BeEmpty())
		Expect(modelFromCard(nil)).To(BeEmpty())
	})

	// A peer chooses this string, and the status bar escapes widget markup and stops
	// there. An escape sequence would write to a terminal it does not belong to, and a
	// long one would push the token count and the key hints off the bar.
	It("Should sanitize and cut what a peer supplied", func() {
		Expect(modelFromCard(&wire.AgentCard{Model: "opus\x1b[31m-5"})).To(Equal("opus-5"))

		long := modelFromCard(&wire.AgentCard{Model: strings.Repeat("m", 200)})
		Expect([]rune(long)).To(HaveLen(49), "cut, with the mark that says it was")
	})
})

var _ = Describe("endsSession", func() {
	// A turn that failed, was refused for want of capacity or was not taken leaves the
	// conversation where it was, so the input row reopens and the operator decides.
	It("Should keep the conversation open for an ending it can retry", func() {
		Expect(endsSession(wire.CodeFailed)).To(BeFalse())
		Expect(endsSession(wire.CodeCapacity)).To(BeFalse())
		Expect(endsSession(wire.CodeConversationBusy)).To(BeFalse())
		Expect(endsSession(wire.CodeTurnNotTaken)).To(BeFalse())
	})

	It("Should end it for an ending the conversation does not continue past", func() {
		Expect(endsSession(wire.CodeSuspended)).To(BeTrue())
		Expect(endsSession(wire.CodeDeferred)).To(BeTrue())
		Expect(endsSession(wire.CodeCrashed)).To(BeTrue())
		Expect(endsSession(wire.CodeUnknownConversation)).To(BeTrue())
	})
})

var _ = Describe("endingMessage", func() {
	// The agent states which parts of the configuration moved and names no flag, since
	// a peer reaching it over a2a has no command line. This program has --force, and the
	// code is what tells it the resume is available.
	It("Should name --force for a refused resume", func() {
		msg := endingMessage(&wire.ErrorMessage{
			Code: wire.CodeConfigDrift,
			Err:  "cannot resume \"run-4f2a\":\n  llm.model: a -> b\nre-run against the original configuration",
		})

		Expect(msg).To(ContainSubstring("llm.model: a -> b"))
		Expect(msg).To(ContainSubstring("re-run with --force"))
	})

	It("Should leave a code it does not know as the worker's own message", func() {
		msg := endingMessage(&wire.ErrorMessage{Code: "something_new", Err: "the worker said this"})

		Expect(msg).To(Equal("the worker said this"))
	})
})
