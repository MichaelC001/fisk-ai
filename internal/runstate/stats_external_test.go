//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package runstate_test

import (
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// undated strips the time from a record, the shape of one written before Record.Time.
func undated(rec runstate.Record) runstate.Record {
	rec.Time = time.Time{}

	return rec
}

// costing sets the tokens an assistant record reports.
func costing(rec runstate.Record, in, out, cacheRead, cacheCreate, thinking int64) runstate.Record {
	rec.Assistant.InTokens = in
	rec.Assistant.OutTokens = out
	rec.Assistant.CacheReadTokens = cacheRead
	rec.Assistant.CacheCreateTokens = cacheCreate
	rec.Assistant.ThinkingTokens = thinking

	return rec
}

var _ = Describe("Stats", func() {
	meta := runstate.MetaRecord{
		RunID:       "2ZqL",
		Prompt:      "tidy the streams",
		Created:     jt(0),
		Agent:       "ops",
		Caller:      "peer1",
		Fingerprint: runstate.Fingerprint{Model: "claude-opus-4-8", MaxTokens: 100000, MaxIterations: 25},
	}

	It("Should frame the run and sum the tokens of each turn with the cache tiers apart", func() {
		s := runstate.Stats(journalOf(meta,
			costing(assistantAt(jt(1), 0, toolUse("tu_1", "stream_ls", `{}`)), 100, 20, 1000, 500, 5),
			resultAt(jt(2), "tu_1", "ORDERS"),
			costing(assistantAt(jt(3), 1, textOnly("done")), 30, 40, 1500, 0, 10),
			terminalAt(jt(4), runstate.ReasonCompleted),
		))

		Expect(s.RunID).To(Equal("2ZqL"))
		Expect(s.Agent).To(Equal("ops"))
		Expect(s.Model).To(Equal("claude-opus-4-8"))
		Expect(s.Caller).To(Equal("peer1"))
		Expect(s.Created).To(Equal(jt(0)))
		Expect(s.Updated).To(Equal(jt(4)))
		Expect(s.Terminal).To(Equal(runstate.ReasonCompleted))
		Expect(s.Ended).To(Equal(jt(4)))
		Expect(s.Iterations).To(Equal(int64(2)))
		Expect(s.MaxIterations).To(Equal(int64(25)))
		Expect(s.MaxTokens).To(Equal(int64(100000)))

		Expect(s.Responses).To(Equal([]runstate.ResponseTokens{
			{Iteration: 0, Time: jt(1), Tokens: runstate.Tokens{In: 100, Out: 20, CacheRead: 1000, CacheCreate: 500, Thinking: 5}},
			{Iteration: 1, Time: jt(3), Tokens: runstate.Tokens{In: 30, Out: 40, CacheRead: 1500, Thinking: 10}},
		}))
		Expect(s.Tokens).To(Equal(runstate.Tokens{In: 130, Out: 60, CacheRead: 2500, CacheCreate: 500, Thinking: 15}))
		Expect(s.Tokens.Total()).To(Equal(int64(3190)))

		wall, ok := s.WallClock()
		Expect(ok).To(BeTrue())
		Expect(wall).To(Equal(4 * time.Second))
	})

	It("Should attribute each tool's calls to the kind that served them, by dispatch and by error", func() {
		denied := resultAt(jt(4), "tu_3", "denied by policy")
		denied.ToolResult.Dispatched = false
		denied.ToolResult.Result.IsError = true

		mcpFailed := resultAt(jt(6), "tu_4", "server said no")
		mcpFailed.ToolResult.Kind = toolkit.KindMCP.String()
		mcpFailed.ToolResult.Result.IsError = true

		s := runstate.Stats(journalOf(meta,
			assistantAt(jt(1), 0, toolUse("tu_1", "stream_ls", `{}`), toolUse("tu_2", "stream_ls", `{}`), toolUse("tu_3", "stream_rm", `{}`)),
			resultAt(jt(2), "tu_1", "a"),
			resultAt(jt(3), "tu_2", "b"),
			denied,
			assistantAt(jt(5), 1, toolUse("tu_4", "jira_search", `{}`), toolUse("tu_5", "jira_search", `{}`)),
			mcpFailed,
			terminalAt(jt(7), runstate.ReasonSuspended),
		))

		Expect(s.Tools).To(Equal([]runstate.ToolStats{
			{Tool: "jira_search", Kinds: []string{"mcp"}, Calls: 2, Dispatched: 1, Unanswered: 1, Errors: 1, Duration: time.Second, Timed: 1},
			{Tool: "stream_ls", Kinds: []string{"application"}, Calls: 2, Dispatched: 2, Duration: 2 * time.Second, Timed: 2},
			{Tool: "stream_rm", Kinds: []string{"application"}, Calls: 1, Undispatched: 1, Errors: 1, Duration: time.Second, Timed: 1},
		}))
	})

	It("Should list every kind a tool's answers carry when the records disagree", func() {
		older := undated(resultAt(jt(2), "tu_1", "a"))
		older.ToolResult.Kind = ""
		older.ToolResult.Dispatched = false

		s := runstate.Stats(journalOf(meta,
			undated(assistantAt(jt(1), 0, toolUse("tu_1", "stream_ls", `{}`))),
			older,
			assistantAt(jt(3), 1, toolUse("tu_2", "stream_ls", `{}`)),
			resultAt(jt(4), "tu_2", "b"),
		))

		Expect(s.Tools).To(HaveLen(1))
		Expect(s.Tools[0].Kinds).To(Equal([]string{"application", "unknown"}))
		Expect(s.Tools[0].Dispatched).To(Equal(int64(1)))
		Expect(s.Tools[0].Undispatched).To(BeZero())
	})

	// A result written before the kind existed cannot say whether its call ran. Fold
	// buckets it as unknown and not dispatched, and Stats counts it as neither, since
	// calling it answered without running would claim what the record does not say.
	It("Should count a result written before the kind was recorded as neither dispatched nor undispatched", func() {
		preKind := func(t time.Time, id string, remote bool) runstate.Record {
			return runstate.Record{Protocol: runstate.ToolResultProtocol, Time: t, ToolResult: &runstate.ToolResultRecord{
				ToolUseID: id, Result: llm.ToolResultBlock{ToolUseID: id, Content: "ok"}, Remote: remote,
			}}
		}

		s := runstate.Stats(journalOf(meta,
			assistantAt(jt(1), 0, toolUse("tu_1", "stream_ls", `{}`), toolUse("tu_2", "stream_ls", `{}`), toolUse("tu_3", "ask_peer", `{}`)),
			preKind(jt(2), "tu_1", false),
			preKind(jt(3), "tu_2", false),
			preKind(jt(4), "tu_3", true),
			terminalAt(jt(5), runstate.ReasonCompleted),
		))

		Expect(s.Tools).To(Equal([]runstate.ToolStats{
			{Tool: "ask_peer", Kinds: []string{"remote"}, Calls: 1, Dispatched: 1, Duration: time.Second, Timed: 1},
			{Tool: "stream_ls", Kinds: []string{"unknown"}, Calls: 2, Duration: 2 * time.Second, Timed: 2},
		}))
	})

	It("Should time a deferred call to its deferral rather than to the answer supplied later", func() {
		s := runstate.Stats(journalOf(meta,
			assistantAt(jt(1), 0, toolUse("tu_1", "change_request", `{}`)),
			runstate.Record{Protocol: runstate.DeferredProtocol, Time: jt(3), Deferred: &runstate.DeferredRecord{ToolUseID: "tu_1", ToolName: "change_request"}},
			terminalAt(jt(4), runstate.ReasonSuspended),
			resultAt(jt(500), "tu_1", "approved"),
		))

		Expect(s.Tools).To(HaveLen(1))
		Expect(s.Tools[0].Duration).To(Equal(2 * time.Second))
		Expect(s.Tools[0].Timed).To(Equal(int64(1)))
		Expect(s.Tools[0].Dispatched).To(Equal(int64(1)))
	})

	Describe("the status", func() {
		It("Should report a run with no terminal record as open", func() {
			s := runstate.Stats(journalOf(meta,
				assistantAt(jt(1), 0, toolUse("tu_1", "stream_ls", `{}`)),
			))

			Expect(s.Terminal).To(BeEmpty())
			Expect(s.Ended).To(BeZero())
			Expect(s.Iterations).To(Equal(int64(1)))
			Expect(s.Tools[0].Unanswered).To(Equal(int64(1)))
		})

		It("Should report a run that journaled a turn after its terminal record as open", func() {
			s := runstate.Stats(journalOf(meta,
				assistantAt(jt(1), 0, textOnly("one")),
				terminalAt(jt(2), runstate.ReasonCompleted),
				runstate.Record{Protocol: runstate.UserProtocol, Time: jt(3), User: &runstate.UserRecord{Message: llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Text: &llm.TextBlock{Text: "again"}}}}}},
			))

			Expect(s.Terminal).To(BeEmpty())
			Expect(s.Ended).To(BeZero())
			Expect(s.Updated).To(Equal(jt(3)))
		})

		It("Should report the reason of a run that ended on its budget", func() {
			s := runstate.Stats(journalOf(meta,
				assistantAt(jt(1), 0, textOnly("one")),
				terminalAt(jt(2), runstate.ReasonBudget),
			))

			Expect(s.Terminal).To(Equal(runstate.ReasonBudget))
			Expect(s.Ended).To(Equal(jt(2)))
		})

		It("Should pass over the memory revisions record a turn writes before its terminal record", func() {
			s := runstate.Stats(journalOf(meta,
				assistantAt(jt(1), 0, textOnly("one")),
				runstate.Record{Protocol: runstate.MemoryRevisionsProtocol, Optional: true, Time: jt(2), MemoryRevisions: &runstate.MemoryRevisionsRecord{Revisions: map[string]uint64{"notes": 7}}},
				terminalAt(jt(3), runstate.ReasonCompleted),
				runstate.Record{Protocol: runstate.Protocol("io.choria.fisk-ai.v1.session.future"), Optional: true, Time: jt(0)},
			))

			Expect(s.Iterations).To(Equal(int64(1)))
			Expect(s.Terminal).To(BeEmpty(), "a record after the terminal one reopens the run, whatever it is")

			s = runstate.Stats(journalOf(meta,
				assistantAt(jt(1), 0, textOnly("one")),
				runstate.Record{Protocol: runstate.MemoryRevisionsProtocol, Optional: true, Time: jt(2), MemoryRevisions: &runstate.MemoryRevisionsRecord{Revisions: map[string]uint64{"notes": 7}}},
				terminalAt(jt(3), runstate.ReasonCompleted),
			))

			Expect(s.Terminal).To(Equal(runstate.ReasonCompleted))
			Expect(s.Iterations).To(Equal(int64(1)))
		})
	})

	Describe("the timing", func() {
		It("Should report no durations for a journal that carries no times", func() {
			m := meta
			m.Created = time.Time{}

			s := runstate.Stats(journalOf(m,
				undated(assistantAt(jt(1), 0, toolUse("tu_1", "stream_ls", `{}`), toolUse("tu_2", "stream_ls", `{}`))),
				undated(resultAt(jt(2), "tu_1", "a")),
				undated(resultAt(jt(3), "tu_2", "b")),
				undated(terminalAt(jt(4), runstate.ReasonCompleted)),
			))

			Expect(s.Updated).To(BeZero())
			Expect(s.Ended).To(BeZero())
			Expect(s.Terminal).To(Equal(runstate.ReasonCompleted))
			Expect(s.Responses[0].Time).To(BeZero())

			_, ok := s.WallClock()
			Expect(ok).To(BeFalse())

			Expect(s.Tools).To(HaveLen(1))
			Expect(s.Tools[0].Calls).To(Equal(int64(2)))
			Expect(s.Tools[0].Timed).To(BeZero())
			Expect(s.Tools[0].Untimed).To(Equal(int64(2)))
			Expect(s.Tools[0].Duration).To(BeZero())
		})

		It("Should count a call with no result and no deferral as neither timed nor untimed", func() {
			s := runstate.Stats(journalOf(meta,
				assistantAt(jt(1), 0, toolUse("tu_1", "shell", `{}`), toolUse("tu_2", "shell", `{}`)),
				resultAt(jt(2), "tu_1", "a"),
				terminalAt(jt(3), runstate.ReasonSuspended),
			))

			Expect(s.Tools).To(HaveLen(1))
			Expect(s.Tools[0].Timed).To(Equal(int64(1)))
			Expect(s.Tools[0].Untimed).To(BeZero())
			Expect(s.Tools[0].Unanswered).To(Equal(int64(1)))
		})

		It("Should time only the calls whose two records both carry a time", func() {
			m := meta
			m.Created = time.Time{}

			s := runstate.Stats(journalOf(m,
				undated(assistantAt(jt(1), 0, toolUse("tu_1", "shell", `{}`), toolUse("tu_2", "shell", `{}`))),
				undated(resultAt(jt(2), "tu_1", "a")),
				runstate.Record{Protocol: runstate.ClaimProtocol, Time: jt(10), Claim: &runstate.ClaimRecord{By: "worker-2"}},
				resultAt(jt(13), "tu_2", "b"),
				assistantAt(jt(14), 1, toolUse("tu_3", "shell", `{}`)),
				resultAt(jt(16), "tu_3", "c"),
			))

			Expect(s.Tools).To(HaveLen(1))
			Expect(s.Tools[0].Calls).To(Equal(int64(3)))
			Expect(s.Tools[0].Timed).To(Equal(int64(2)))
			Expect(s.Tools[0].Untimed).To(Equal(int64(1)))
			Expect(s.Tools[0].Duration).To(Equal(5 * time.Second))
			Expect(s.Updated).To(Equal(jt(16)))

			_, ok := s.WallClock()
			Expect(ok).To(BeFalse(), "the meta record carries no time")
		})
	})

	It("Should marshal to the documented JSON shape", func() {
		s := runstate.Stats(journalOf(meta,
			costing(assistantAt(jt(1), 0, toolUse("tu_1", "stream_ls", `{}`)), 100, 20, 1000, 500, 5),
			resultAt(jt(3), "tu_1", "ORDERS"),
			terminalAt(jt(4), runstate.ReasonCompleted),
		))

		out, err := json.Marshal(s)
		Expect(err).ToNot(HaveOccurred())

		Expect(out).To(MatchJSON(`{
			"run_id": "2ZqL",
			"agent": "ops",
			"model": "claude-opus-4-8",
			"caller": "peer1",
			"created": "2023-11-14T22:13:20Z",
			"updated": "2023-11-14T22:13:24Z",
			"terminal": "completed",
			"ended": "2023-11-14T22:13:24Z",
			"iterations": 1,
			"max_iterations": 25,
			"max_tokens": 100000,
			"tokens": {"in_tokens": 100, "out_tokens": 20, "cache_read_tokens": 1000, "cache_create_tokens": 500, "thinking_tokens": 5},
			"responses": [
				{"iteration": 0, "time": "2023-11-14T22:13:21Z", "tokens": {"in_tokens": 100, "out_tokens": 20, "cache_read_tokens": 1000, "cache_create_tokens": 500, "thinking_tokens": 5}}
			],
			"tools": [
				{"tool": "stream_ls", "kinds": ["application"], "calls": 1, "dispatched": 1, "undispatched": 0, "unanswered": 0, "errors": 0, "duration_ns": 2000000000, "timed": 1, "untimed": 0}
			]
		}`))
	})

	It("Should render empty lists for a run that made no call", func() {
		out, err := json.Marshal(runstate.Stats(journalOf(meta)))
		Expect(err).ToNot(HaveOccurred())

		var doc map[string]any
		Expect(json.Unmarshal(out, &doc)).To(Succeed())
		Expect(doc).To(HaveKeyWithValue("responses", BeEmpty()))
		Expect(doc).To(HaveKeyWithValue("tools", BeEmpty()))
		Expect(doc["responses"]).ToNot(BeNil())
		Expect(doc["tools"]).ToNot(BeNil())
	})
})

// textOnly is a content block carrying text alone, for an assistant turn that calls no
// tool.
func textOnly(text string) llm.ContentBlock {
	return llm.ContentBlock{Text: &llm.TextBlock{Text: text}}
}
