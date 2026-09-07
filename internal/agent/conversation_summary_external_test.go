//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// These specs drive the summary a run writes on its terminal record through the
// exported agent.Run API, and check it against a fold of the journal it was written
// into. The fold is the authority on every number in it; the summary is those numbers
// copied to the tail so a listing reads them without the fold.
package agent_test

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
	runstatefile "github.com/choria-io/fisk-ai/internal/runstate/file"
	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// billedText is a completing reply that cost something, so a spec can assert the token
// counters and the context size rather than reading zeros back.
func billedText(text string, usage llm.Usage) *llm.Response {
	resp := agenttest.TextResponse(text)
	resp.Usage = usage

	return resp
}

// tailSummary is the summary on the last terminal record of a stored run, and nil where
// that record carries none.
func tailSummary(records []runstate.Record) *runstate.ConversationSummary {
	var out *runstate.ConversationSummary
	for _, rec := range records {
		if rec.Terminal != nil {
			out = rec.Terminal.Summary
		}
	}

	return out
}

// stripTerminalSummary rewrites a stored run with the summary taken off every terminal
// record, which is the shape of a conversation journaled before the field existed. It
// goes through the store's own interface, so what it leaves behind is a journal the
// store wrote.
func stripTerminalSummary(tb testing.TB, store runstate.Store, id string) {
	tb.Helper()

	ctx := context.Background()

	records := journalRecords(tb, store, id)
	Expect(records).NotTo(BeEmpty())
	Expect(records[0].Meta).NotTo(BeNil())

	Expect(store.Delete(ctx, id)).To(Succeed())

	j, err := store.Create(ctx, id, *records[0].Meta)
	Expect(err).NotTo(HaveOccurred())

	for _, rec := range records[1:] {
		if rec.Terminal != nil {
			rec.Terminal = &runstate.TerminalRecord{Reason: rec.Terminal.Reason, Message: rec.Terminal.Message}
		}
		Expect(j.Append(ctx, rec.Seq, rec)).To(Succeed())
	}

	Expect(j.Close()).To(Succeed())
}

var _ = Describe("the summary a turn writes on its terminal record", func() {
	var (
		ctx   context.Context
		store *runstatefile.FileStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		s, err := runstatefile.NewFileStore(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())
		store = s
	})

	opts := func(provider *agenttest.ScriptedProvider, prompt string, cp agent.Checkpoint) agent.Options {
		return agent.Options{
			Config:       agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), exampleApp())),
			ConfigFile:   "agent.yaml",
			Prompt:       []string{prompt},
			Provider:     provider,
			SessionStore: store,
			Checkpoint:   cp,
		}
	}

	// The claim Counters makes, held across the whole item: the journal is where the
	// numbers come from, and the row a listing shows is a copy of them.
	expectFoldAgrees := func(id string) *runstate.ConversationSummary {
		GinkgoHelper()

		rs, err := store.Load(ctx, id)
		Expect(err).NotTo(HaveOccurred())

		infos, err := store.List(ctx, runstate.ListFilter{})
		Expect(err).NotTo(HaveOccurred())

		var summary *runstate.ConversationSummary
		for _, info := range infos {
			if info.RunID == id {
				summary = info.Summary
			}
		}

		Expect(summary).NotTo(BeNil(), "run %q", id)
		Expect(summary.Counters).To(Equal(rs.Counters))
		Expect(summary.Turns).To(Equal(rs.Turns))
		Expect(summary.ContextTokens).To(Equal(rs.ContextTokens))

		return summary
	}

	It("Should carry what one turn cost, and agree with a fold of the journal", func() {
		res, err := agent.Run(ctx, opts(
			agenttest.NewScriptedProvider(GinkgoTB(),
				billedText("there are three streams", llm.Usage{In: 120, Out: 20, CacheRead: 4000, CacheCreate: 300}),
			),
			"how many streams are there",
			agent.Checkpoint{Enabled: true},
		), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())

		summary := expectFoldAgrees(res.SessionID)
		Expect(summary.Turns).To(Equal(int64(1)))
		Expect(summary.ContextTokens).To(Equal(int64(4420)), "the call's whole input, both cache tiers included")
		Expect(summary.Counters.LlmCalls).To(Equal(int64(1)))
		Expect(summary.Counters.ToolCalls).To(BeZero())
		Expect(summary.Counters.InTokens).To(Equal(int64(120)))
		Expect(summary.Counters.OutTokens).To(Equal(int64(20)))

		// The run wrote it onto the terminal record itself, which is what the listing
		// reads rather than folding the journal.
		Expect(tailSummary(journalRecords(GinkgoTB(), store, res.SessionID))).To(Equal(summary))
	})

	// Every turn of a served conversation is a resumed run, so the count has to climb
	// with the conversation rather than restart with the process serving it.
	It("Should continue the turn count on the next turn of the conversation", func() {
		res1, err := agent.Run(ctx, opts(
			agenttest.NewScriptedProvider(GinkgoTB(),
				billedText("there are three streams", llm.Usage{In: 100, Out: 20}),
			),
			"how many streams are there",
			agent.Checkpoint{Enabled: true},
		), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(expectFoldAgrees(res1.SessionID).Turns).To(Equal(int64(1)))

		res2, err := agent.Run(ctx, opts(
			agenttest.NewScriptedProvider(GinkgoTB(),
				billedText("the first one is ORDERS", llm.Usage{In: 260, Out: 30}),
			),
			"what is the first one called",
			agent.Checkpoint{ResumeID: res1.SessionID, FollowUp: true},
		), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res2.SessionID).To(Equal(res1.SessionID))

		summary := expectFoldAgrees(res1.SessionID)
		Expect(summary.Turns).To(Equal(int64(2)))
		Expect(summary.ContextTokens).To(Equal(int64(260)), "the second turn's call, which is what a third sends again")
		Expect(summary.Counters.LlmCalls).To(Equal(int64(2)), "both turns, since the resume seeded from the journal")
		Expect(summary.Counters.InTokens).To(Equal(int64(360)))
	})

	It("Should count the tool calls a turn made, partitioned by the provider that served them", func() {
		res, err := agent.Run(ctx, opts(
			agenttest.NewScriptedProvider(GinkgoTB(),
				agenttest.ToolUseResponse("c1", "do", json.RawMessage(`{"subject":"widgets"}`)),
				billedText("there are three streams", llm.Usage{In: 300, Out: 20}),
			),
			"list the streams",
			agent.Checkpoint{Enabled: true},
		), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())

		summary := expectFoldAgrees(res.SessionID)
		Expect(summary.Counters.ToolCalls).To(Equal(int64(1)))
		Expect(summary.Counters.ToolCallsByKind).To(HaveKeyWithValue(toolkit.KindApplication, int64(1)))
	})

	// A context reset rotates to a fresh journal, and each journal's terminal record
	// describes the conversation it holds. The run's own counters keep climbing to report
	// the whole sitting, which is what the outgoing conversation's numbers are taken
	// before and the incoming one's are taken after.
	It("Should summarize each journal a context reset leaves behind on its own", func() {
		turns := 0
		next := func(context.Context) agent.Continuation {
			turns++
			if turns == 1 {
				return agent.Continuation{Text: "start again", Reset: true, Continue: true}
			}

			return agent.Continuation{Continue: false}
		}

		o := opts(
			agenttest.NewScriptedProvider(GinkgoTB(),
				billedText("there are three streams", llm.Usage{In: 100, Out: 20}),
				billedText("the first is orders", llm.Usage{In: 700, Out: 40}),
			),
			"how many streams are there",
			agent.Checkpoint{Enabled: true},
		)
		o.NextPrompt = next

		res, err := agent.Run(ctx, o, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())

		infos, err := store.List(ctx, runstate.ListFilter{})
		Expect(err).NotTo(HaveOccurred())
		Expect(infos).To(HaveLen(2))

		for _, info := range infos {
			summary := expectFoldAgrees(info.RunID)
			Expect(summary.Turns).To(Equal(int64(1)), "each journal opens on its own first prompt")
			Expect(summary.Counters.LlmCalls).To(Equal(int64(1)), "run %q", info.RunID)
		}

		// The rotated-to journal is the one the run ended on, and it carries only its own
		// call rather than the sitting's two.
		rotated := expectFoldAgrees(res.SessionID)
		Expect(rotated.ContextTokens).To(Equal(int64(700)))
		Expect(rotated.Counters.InTokens).To(Equal(int64(700)))
	})

	// A deferred call has no result record until somebody answers it, and the two ends of
	// the wait fail in opposite directions if the run counts the dispatch instead: the
	// suspend claims a call the fold has not got, and the run that finishes on the answer
	// reports one fewer than the fold, under the wrong provider.
	It("Should agree with the fold across a deferral and its answer", func() {
		var calls atomic.Int64
		tool := deferringTool(GinkgoTB(), "change_request", &calls)

		withTool := func(provider *agenttest.ScriptedProvider, prompt string, cp agent.Checkpoint) agent.Options {
			o := opts(provider, prompt, cp)
			o.CustomTools = []toolkit.Tool{tool}

			return o
		}

		res1, err := agent.Run(ctx, withTool(
			agenttest.NewScriptedProvider(GinkgoTB(), agenttest.ToolUseResponse("c1", "change_request", json.RawMessage(`{}`))),
			"raise a change",
			agent.Checkpoint{Enabled: true},
		), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res1.Reason).To(Equal(runstate.ReasonSuspended))
		Expect(res1.Deferred).To(HaveLen(1))
		useID := res1.Deferred[0].ToolUseID

		suspended := expectFoldAgrees(res1.SessionID)
		Expect(suspended.Counters.ToolCalls).To(BeZero(), "the call is waiting on an answer, so no result record counts it yet")
		Expect(suspended.Counters.ToolCallsByKind).To(BeEmpty())

		// The answer arrives from outside the run and the next turn finishes on it.
		res2, err := agent.Run(ctx, withTool(
			agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("the change was approved")),
			"raise a change",
			agent.Checkpoint{ResumeID: res1.SessionID, Answer: &agent.DeferredAnswer{ToolUseID: useID, Content: `{"approved":true}`}},
		), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res2.Reason).To(Equal(runstate.ReasonCompleted))
		Expect(calls.Load()).To(Equal(int64(1)), "a deferred call is never dispatched again")

		answered := expectFoldAgrees(res1.SessionID)
		Expect(answered.Counters.ToolCalls).To(Equal(int64(1)))
		Expect(answered.Counters.ToolCallsByKind).To(HaveKeyWithValue(toolkit.KindCustom, int64(1)),
			"the answering record takes the kind off the deferral, so the call stays with the provider that served it")
	})

	// A conversation whose last turn ended before this build wrote summaries. It lists
	// with none rather than with zeros, so a rail draws an empty slot for it.
	It("Should report no summary for a conversation whose terminal record predates the field", func() {
		res, err := agent.Run(ctx, opts(
			agenttest.NewScriptedProvider(GinkgoTB(),
				billedText("there are three streams", llm.Usage{In: 100, Out: 20}),
			),
			"how many streams are there",
			agent.Checkpoint{Enabled: true},
		), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())

		stripTerminalSummary(GinkgoTB(), store, res.SessionID)

		infos, err := store.List(ctx, runstate.ListFilter{})
		Expect(err).NotTo(HaveOccurred())
		Expect(infos).To(HaveLen(1))
		Expect(infos[0].Terminal).To(Equal(runstate.ReasonCompleted))
		Expect(infos[0].Summary).To(BeNil())

		// The next turn of it still counts from the journal, so the conversation's history
		// is not lost with the summary.
		res2, err := agent.Run(ctx, opts(
			agenttest.NewScriptedProvider(GinkgoTB(),
				billedText("the first one is ORDERS", llm.Usage{In: 260, Out: 30}),
			),
			"what is the first one called",
			agent.Checkpoint{ResumeID: res.SessionID, FollowUp: true},
		), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())

		summary := expectFoldAgrees(res2.SessionID)
		Expect(summary.Turns).To(Equal(int64(2)))
		Expect(summary.Counters.LlmCalls).To(Equal(int64(2)))
	})
})
