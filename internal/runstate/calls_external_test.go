//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package runstate_test

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// jt is the nth of a run of distinct times, so a spec can say which record a time came
// from and what the gap between two records is.
func jt(n int) time.Time {
	return time.Unix(1700000000+int64(n), 0).UTC()
}

// journalOf numbers recs after a meta record the way a store does, so a spec lists only
// the records it is about.
func journalOf(meta runstate.MetaRecord, recs ...runstate.Record) []runstate.Record {
	meta.Version = runstate.Version
	all := []runstate.Record{{Protocol: runstate.MetaProtocol, Meta: &meta, Time: meta.Created}}
	all = append(all, recs...)
	for i := range all {
		all[i].Seq = uint64(i + 1)
	}

	return all
}

// toolUse is one tool_use block naming tool with the given input.
func toolUse(id string, tool string, input string) llm.ContentBlock {
	return llm.ContentBlock{ToolUse: &llm.ToolUseBlock{ID: id, Name: tool, Input: json.RawMessage(input)}}
}

// assistantAt is an assistant record at iteration iter journaled at t, carrying blocks.
func assistantAt(t time.Time, iter int64, blocks ...llm.ContentBlock) runstate.Record {
	return runstate.Record{Protocol: runstate.AssistantProtocol, Time: t, Assistant: &runstate.AssistantRecord{
		Iteration: iter,
		Message:   llm.Message{Role: llm.RoleAssistant, Content: blocks},
	}}
}

// resultAt is a result for id journaled at t, served by an application tool that ran.
func resultAt(t time.Time, id string, content string) runstate.Record {
	return runstate.Record{Protocol: runstate.ToolResultProtocol, Time: t, ToolResult: &runstate.ToolResultRecord{
		ToolUseID:  id,
		Result:     llm.ToolResultBlock{ToolUseID: id, Content: content},
		Kind:       toolkit.KindApplication.String(),
		Dispatched: true,
	}}
}

// terminalAt is a terminal record journaled at t.
func terminalAt(t time.Time, reason runstate.TerminalReason) runstate.Record {
	return runstate.Record{Protocol: runstate.TerminalProtocol, Time: t, Terminal: &runstate.TerminalRecord{Reason: reason}}
}

var _ = Describe("Calls", func() {
	meta := runstate.MetaRecord{RunID: "2ZqL", Prompt: "list the streams", Created: jt(0)}

	It("Should pair two calls of one assistant turn with their results in call order", func() {
		calls := runstate.Calls(journalOf(meta,
			assistantAt(jt(1), 0, toolUse("tu_1", "stream_ls", `{"all":true}`), toolUse("tu_2", "stream_info", `{"stream":"ORDERS"}`)),
			resultAt(jt(2), "tu_1", "ORDERS"),
			resultAt(jt(3), "tu_2", `{"messages":10}`),
			terminalAt(jt(4), runstate.ReasonCompleted),
		))

		Expect(calls).To(HaveLen(2))

		Expect(calls[0].Iteration).To(Equal(int64(0)))
		Expect(calls[0].ToolUseID).To(Equal("tu_1"))
		Expect(calls[0].Tool).To(Equal("stream_ls"))
		Expect(string(calls[0].Input)).To(Equal(`{"all":true}`))
		Expect(calls[0].Time).To(Equal(jt(1)))
		Expect(calls[0].Answer.Result.Content).To(Equal("ORDERS"))
		Expect(calls[0].AnswerTime).To(Equal(jt(2)))

		Expect(calls[1].ToolUseID).To(Equal("tu_2"))
		Expect(calls[1].Tool).To(Equal("stream_info"))
		Expect(calls[1].Answer.Result.Content).To(Equal(`{"messages":10}`))
		Expect(calls[1].AnswerTime).To(Equal(jt(3)))
	})

	It("Should carry the error flag, the kind and the dispatch flag of the answer", func() {
		failed := resultAt(jt(2), "tu_1", "no such stream")
		failed.ToolResult.Result.IsError = true
		failed.ToolResult.Kind = toolkit.KindMCP.String()
		failed.ToolResult.Dispatched = false

		calls := runstate.Calls(journalOf(meta, assistantAt(jt(1), 0, toolUse("tu_1", "stream_info", `{}`)), failed))

		Expect(calls).To(HaveLen(1))
		Expect(calls[0].Answer.Result.IsError).To(BeTrue())
		Expect(calls[0].Answer.Kind).To(Equal("mcp"))
		Expect(calls[0].Answer.Dispatched).To(BeFalse())
	})

	It("Should keep a call a suspended run left unanswered, with no answer", func() {
		calls := runstate.Calls(journalOf(meta,
			assistantAt(jt(1), 3, toolUse("tu_1", "stream_ls", `{}`), toolUse("tu_2", "stream_rm", `{"stream":"X"}`)),
			resultAt(jt(2), "tu_1", "X"),
			terminalAt(jt(3), runstate.ReasonSuspended),
		))

		Expect(calls).To(HaveLen(2))
		Expect(calls[1].ToolUseID).To(Equal("tu_2"))
		Expect(calls[1].Iteration).To(Equal(int64(3)))
		Expect(calls[1].Answer).To(BeNil())
		Expect(calls[1].AnswerTime).To(BeZero())
		Expect(calls[1].Deferred).To(BeNil())
	})

	It("Should pair a deferred call with the answer supplied after the run suspended", func() {
		deferred := runstate.Record{Protocol: runstate.DeferredProtocol, Time: jt(2), Deferred: &runstate.DeferredRecord{
			ToolUseID: "tu_1", ToolName: "change_request", Note: "waiting on approval", Handle: "CHG-1",
		}}

		calls := runstate.Calls(journalOf(meta,
			assistantAt(jt(1), 0, toolUse("tu_1", "change_request", `{}`)),
			deferred,
			terminalAt(jt(3), runstate.ReasonSuspended),
			runstate.Record{Protocol: runstate.ClaimProtocol, Time: jt(4), Claim: &runstate.ClaimRecord{By: "worker-2"}},
			resultAt(jt(5), "tu_1", "approved"),
		))

		Expect(calls).To(HaveLen(1))
		Expect(calls[0].Deferred).ToNot(BeNil())
		Expect(calls[0].Deferred.Handle).To(Equal("CHG-1"))
		Expect(calls[0].Answer.Result.Content).To(Equal("approved"))
		Expect(calls[0].AnswerTime).To(Equal(jt(5)))
	})

	It("Should keep a deferred call nothing has answered, with its deferral and no answer", func() {
		calls := runstate.Calls(journalOf(meta,
			assistantAt(jt(1), 0, toolUse("tu_1", "change_request", `{}`)),
			runstate.Record{Protocol: runstate.DeferredProtocol, Time: jt(2), Deferred: &runstate.DeferredRecord{ToolUseID: "tu_1", ToolName: "change_request", Note: "waiting"}},
			terminalAt(jt(3), runstate.ReasonSuspended),
		))

		Expect(calls).To(HaveLen(1))
		Expect(calls[0].Deferred.Note).To(Equal("waiting"))
		Expect(calls[0].Answer).To(BeNil())
	})

	// A transcript block carries 64 KiB of text. The pairing reads the record, so an
	// answer longer than that comes back as the tool returned it.
	It("Should return an answer longer than a transcript block whole", func() {
		huge := strings.Repeat("0123456789abcdef", 16*1024)

		calls := runstate.Calls(journalOf(meta,
			assistantAt(jt(1), 0, toolUse("tu_1", "dump", `{}`)),
			resultAt(jt(2), "tu_1", huge),
		))

		Expect(calls).To(HaveLen(1))
		Expect(calls[0].Answer.Result.Content).To(HaveLen(len(huge)))
		Expect(calls[0].Answer.Result.Content).To(Equal(huge))
	})

	It("Should keep the first answer of a call answered twice and pass over a result that answers no call", func() {
		calls := runstate.Calls(journalOf(meta,
			assistantAt(jt(1), 0, toolUse("tu_1", "stream_ls", `{}`)),
			resultAt(jt(2), "tu_1", "first"),
			resultAt(jt(3), "tu_1", "second"),
			resultAt(jt(4), "tu_9", "stray"),
		))

		Expect(calls).To(HaveLen(1))
		Expect(calls[0].Answer.Result.Content).To(Equal("first"))
	})

	It("Should answer the latest open call when a provider reuses a tool_use id across turns", func() {
		calls := runstate.Calls(journalOf(meta,
			assistantAt(jt(1), 0, toolUse("call_0", "stream_ls", `{}`)),
			resultAt(jt(2), "call_0", "one"),
			assistantAt(jt(3), 1, toolUse("call_0", "stream_info", `{}`)),
			resultAt(jt(4), "call_0", "two"),
		))

		Expect(calls).To(HaveLen(2))
		Expect(calls[0].Answer.Result.Content).To(Equal("one"))
		Expect(calls[1].Answer.Result.Content).To(Equal("two"))
		Expect(calls[1].Iteration).To(Equal(int64(1)))
	})

	It("Should return nothing for a run that made no tool call", func() {
		Expect(runstate.Calls(journalOf(meta, terminalAt(jt(1), runstate.ReasonCompleted)))).To(BeEmpty())
		Expect(runstate.Calls(nil)).To(BeEmpty())
	})
})

var _ = Describe("CallFilter", func() {
	answered := func(iter int64, id string, tool string, input string, content string, isError bool) runstate.Call {
		return runstate.Call{
			Iteration: iter, ToolUseID: id, Tool: tool, Input: json.RawMessage(input),
			Answer: &runstate.ToolResultRecord{ToolUseID: id, Result: llm.ToolResultBlock{ToolUseID: id, Content: content, IsError: isError}},
		}
	}

	calls := []runstate.Call{
		answered(0, "tu_1", "stream_ls", `{"all":true}`, "ORDERS\nEVENTS", false),
		answered(0, "tu_2", "stream_info", `{"stream":"ORDERS"}`, `{"messages":10}`, false),
		answered(1, "tu_3", "stream_info", `{"stream":"MISSING"}`, "stream not found", true),
		{Iteration: 2, ToolUseID: "tu_4", Tool: "stream_rm", Input: json.RawMessage(`{"stream":"EVENTS"}`)},
	}

	selected := func(f runstate.CallFilter) []string {
		var out []string
		for _, c := range f.Select(calls) {
			out = append(out, c.ToolUseID)
		}
		return out
	}

	iteration := func(n int64) *int64 { return &n }

	It("Should select every call when zero", func() {
		Expect(selected(runstate.CallFilter{})).To(Equal([]string{"tu_1", "tu_2", "tu_3", "tu_4"}))
	})

	It("Should select by any of the named tools", func() {
		Expect(selected(runstate.CallFilter{Tools: []string{"stream_info"}})).To(Equal([]string{"tu_2", "tu_3"}))
		Expect(selected(runstate.CallFilter{Tools: []string{"stream_ls", "stream_rm"}})).To(Equal([]string{"tu_1", "tu_4"}))
	})

	It("Should select by iteration, iteration 0 included", func() {
		Expect(selected(runstate.CallFilter{Iteration: iteration(0)})).To(Equal([]string{"tu_1", "tu_2"}))
		Expect(selected(runstate.CallFilter{Iteration: iteration(2)})).To(Equal([]string{"tu_4"}))
	})

	It("Should select the calls answered with an error", func() {
		Expect(selected(runstate.CallFilter{Errors: true})).To(Equal([]string{"tu_3"}))
	})

	It("Should select only answered calls when asked, and for an output pattern", func() {
		Expect(selected(runstate.CallFilter{Answered: true})).To(Equal([]string{"tu_1", "tu_2", "tu_3"}))
		Expect(selected(runstate.CallFilter{Output: regexp.MustCompile(`.`)})).To(Equal([]string{"tu_1", "tu_2", "tu_3"}))
	})

	It("Should match the raw input JSON and the answer content", func() {
		Expect(selected(runstate.CallFilter{Input: regexp.MustCompile(`"stream":"(ORDERS|EVENTS)"`)})).To(Equal([]string{"tu_2", "tu_4"}))
		Expect(selected(runstate.CallFilter{Output: regexp.MustCompile(`(?i)^orders`)})).To(Equal([]string{"tu_1"}))
	})

	It("Should combine the filters as AND", func() {
		f := runstate.CallFilter{
			Tools:     []string{"stream_info"},
			Iteration: iteration(1),
			Errors:    true,
			Input:     regexp.MustCompile("MISSING"),
		}
		Expect(selected(f)).To(Equal([]string{"tu_3"}))

		f.Iteration = iteration(0)
		Expect(f.Select(calls)).To(BeNil())
	})
})
