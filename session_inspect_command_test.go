//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/choria-io/fisk"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// useSessionInspectFlags points the session commands at a fresh file store and resets
// every flag the inspection commands read, restoring the previous values when the spec
// ends.
func useSessionInspectFlags() {
	GinkgoHelper()

	origConfig, origStateDir, origID, origJSON := sessionConfigFile, stateDirFlag, sessionArgID, sessionJSON
	origTools, origIter, origIterSet, origErrors := queryTools, queryIteration, queryIterationSet, queryErrors
	origCallsOnly, origResultsOnly, origIn, origOut := queryCallsOnly, queryResultsOnly, queryInputMatch, queryOutputMatch
	origIdentity, origModel, origCaller, origStatus := searchIdentity, searchModel, searchCaller, searchStatus
	origSince, origUntil, origMatch := searchSince, searchUntil, searchMatch
	DeferCleanup(func() {
		sessionConfigFile, stateDirFlag, sessionArgID, sessionJSON = origConfig, origStateDir, origID, origJSON
		queryTools, queryIteration, queryIterationSet, queryErrors = origTools, origIter, origIterSet, origErrors
		queryCallsOnly, queryResultsOnly, queryInputMatch, queryOutputMatch = origCallsOnly, origResultsOnly, origIn, origOut
		searchIdentity, searchModel, searchCaller, searchStatus = origIdentity, origModel, origCaller, origStatus
		searchSince, searchUntil, searchMatch = origSince, origUntil, origMatch
	})

	searchIdentity, searchModel, searchCaller, searchStatus = "", "", "", ""
	searchSince, searchUntil, searchMatch = "", "", ""

	sessionConfigFile = ""
	stateDirFlag = GinkgoT().TempDir()
	sessionArgID = ""
	sessionJSON = false
	queryTools, queryIteration, queryIterationSet, queryErrors = nil, 0, false, false
	queryCallsOnly, queryResultsOnly, queryInputMatch, queryOutputMatch = false, false, "", ""
}

// writeSessionJournal stores a run under id in the store the session flags name, with
// recs appended after its meta record in order.
func writeSessionJournal(id string, meta runstate.MetaRecord, recs ...runstate.Record) {
	GinkgoHelper()

	ctx := context.Background()
	store, cleanup, err := openSessionStore(ctx)
	Expect(err).ToNot(HaveOccurred())
	defer cleanup()

	meta.RunID = id
	j, err := store.Create(ctx, id, meta)
	Expect(err).ToNot(HaveOccurred())

	for i, rec := range recs {
		seq := uint64(i + 2)
		rec.Seq = seq
		Expect(j.Append(ctx, seq, rec)).To(Succeed())
	}

	Expect(j.Close()).To(Succeed())
}

// sessionTime is the nth of a run of distinct times.
func sessionTime(n int) time.Time {
	return time.Unix(1700000000+int64(n), 0).UTC()
}

func sessionAssistant(t time.Time, iter int64, calls ...*llm.ToolUseBlock) runstate.Record {
	content := []llm.ContentBlock{{Text: &llm.TextBlock{Text: "working"}}}
	for _, c := range calls {
		content = append(content, llm.ContentBlock{ToolUse: c})
	}

	return runstate.Record{Protocol: runstate.AssistantProtocol, Time: t, Assistant: &runstate.AssistantRecord{
		Iteration: iter,
		Message:   llm.Message{Role: llm.RoleAssistant, Content: content},
		InTokens:  10,
		OutTokens: 5,
	}}
}

func sessionResult(t time.Time, id string, content string, isError bool) runstate.Record {
	return runstate.Record{Protocol: runstate.ToolResultProtocol, Time: t, ToolResult: &runstate.ToolResultRecord{
		ToolUseID:  id,
		Result:     llm.ToolResultBlock{ToolUseID: id, Content: content, IsError: isError},
		Kind:       toolkit.KindApplication.String(),
		Dispatched: true,
	}}
}

func sessionCall(id string, tool string, input string) *llm.ToolUseBlock {
	return &llm.ToolUseBlock{ID: id, Name: tool, Input: json.RawMessage(input)}
}

// tableRow is a pattern for adjacent table cells holding the given patterns. A table
// separates its cells with a box-drawing bar on a terminal and a pipe where it renders
// Markdown, and the pattern takes either.
func tableRow(cells ...string) string {
	return strings.Join(cells, `\s*[|\x{2502}]\s*`)
}

var _ = Describe("session query", func() {
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

	BeforeEach(func() {
		useSessionInspectFlags()
	})

	Describe("newSessionQueryFilter", func() {
		It("Should build a zero filter with no flag given", func() {
			f, err := newSessionQueryFilter()
			Expect(err).ToNot(HaveOccurred())
			Expect(f).To(Equal(runstate.CallFilter{}))
		})

		It("Should carry each flag into the filter", func() {
			queryTools = []string{"stream_ls", "stream_rm"}
			queryErrors = true
			queryInputMatch = `"stream":"X"`
			queryOutputMatch = `(?i)^orders`

			f, err := newSessionQueryFilter()
			Expect(err).ToNot(HaveOccurred())
			Expect(f.Tools).To(Equal([]string{"stream_ls", "stream_rm"}))
			Expect(f.Errors).To(BeTrue())
			Expect(f.Input.String()).To(Equal(`"stream":"X"`))
			Expect(f.Output.String()).To(Equal(`(?i)^orders`))
			Expect(f.Answered).To(BeFalse())
		})

		It("Should select by iteration only when the flag was given, iteration 0 included", func() {
			queryIteration = 0

			f, err := newSessionQueryFilter()
			Expect(err).ToNot(HaveOccurred())
			Expect(f.Iteration).To(BeNil())

			queryIterationSet = true
			f, err = newSessionQueryFilter()
			Expect(err).ToNot(HaveOccurred())
			Expect(f.Iteration).To(HaveValue(Equal(int64(0))))
		})

		It("Should select the answered calls under --results-only and not under --calls-only", func() {
			queryResultsOnly = true
			f, err := newSessionQueryFilter()
			Expect(err).ToNot(HaveOccurred())
			Expect(f.Answered).To(BeTrue())

			queryResultsOnly = false
			queryCallsOnly = true
			f, err = newSessionQueryFilter()
			Expect(err).ToNot(HaveOccurred())
			Expect(f.Answered).To(BeFalse())
		})

		It("Should refuse a pattern that does not compile, naming the flag", func() {
			queryOutputMatch = "(unclosed"

			_, err := newSessionQueryFilter()
			Expect(err).To(MatchError(ContainSubstring("invalid --output-match pattern")))
		})

		It("Should refuse --calls-only with --results-only", func() {
			queryCallsOnly = true
			queryResultsOnly = true

			_, err := newSessionQueryFilter()
			Expect(err).To(MatchError(ContainSubstring("cannot both be set")))
		})
	})

	Describe("printSessionCall", func() {
		render := func(c runstate.Call, callsOnly bool, resultsOnly bool) string {
			var buf bytes.Buffer
			printSessionCall(&buf, c, callsOnly, resultsOnly)
			return buf.String()
		}

		It("Should print the tool, the id and the iteration, the input indented and the answer", func() {
			out := render(calls[1], false, false)

			Expect(out).To(Equal("stream_info tu_2 (iteration 0)\nInput:\n{\n  \"stream\": \"ORDERS\"\n}\nAnswer:\n{\n  \"messages\": 10\n}\n"))
		})

		It("Should print an answer that is not JSON verbatim", func() {
			stdout := "total 8\n-rw-r--r--  1 rip  staff  12 Sep 22 10:00 a.txt\n\tindented \"quoted\" line\n"
			c := answered(0, "tu_9", "shell", `{"cmd":"ls -l"}`, stdout, false)

			Expect(render(c, false, false)).To(HaveSuffix("Answer:\n" + stdout))
		})

		It("Should mark an error answer and say when there is no answer yet", func() {
			Expect(render(calls[2], false, false)).To(ContainSubstring("Answer (error):\nstream not found\n"))
			Expect(render(calls[3], false, false)).To(HaveSuffix("Answer: none yet\n"))
		})

		It("Should show the deferral of a call waiting on its answer", func() {
			c := calls[3]
			c.Deferred = &runstate.DeferredRecord{ToolUseID: "tu_4", ToolName: "stream_rm", Note: "waiting on approval", Handle: "CHG-1"}

			Expect(render(c, false, false)).To(ContainSubstring("Deferred: stream_rm: waiting on approval (CHG-1)\n"))
		})

		It("Should leave out the answer under --calls-only and the input under --results-only", func() {
			Expect(render(calls[0], true, false)).ToNot(ContainSubstring("Answer"))
			Expect(render(calls[0], false, true)).ToNot(ContainSubstring("Input"))
		})

		It("Should strip terminal control sequences and keep newlines", func() {
			c := answered(0, "tu_\x1b[31m1", "\x1b]0;pwned\x07shell", `{"cmd":"echo \u001b[31m"}`, "line \x1b[31mone\x1b[0m\nline two\x08", false)

			out := render(c, false, false)
			Expect(out).ToNot(ContainSubstring("\x1b"))
			Expect(out).ToNot(ContainSubstring("pwned"))
			Expect(out).ToNot(ContainSubstring("\x07"))
			Expect(out).ToNot(ContainSubstring("\x08"))
			Expect(out).To(ContainSubstring("line one\nline two"))
		})
	})

	Describe("sessionQueryAction", func() {
		BeforeEach(func() {
			writeSessionJournal("2ZqLquery", runstate.MetaRecord{Prompt: "tidy the streams", Created: sessionTime(0)},
				sessionAssistant(sessionTime(1), 0, sessionCall("tu_1", "stream_ls", `{}`), sessionCall("tu_2", "stream_info", `{"stream":"X"}`)),
				sessionResult(sessionTime(2), "tu_1", "X\nY", false),
				sessionResult(sessionTime(3), "tu_2", "no such stream", true),
				runstate.Record{Protocol: runstate.TerminalProtocol, Time: sessionTime(4), Terminal: &runstate.TerminalRecord{Reason: runstate.ReasonCompleted}},
			)
			sessionArgID = "2ZqLquery"
		})

		It("Should print the selected calls", func() {
			queryErrors = true

			out := captureStdout(func() {
				Expect(sessionQueryAction(nil)).To(Succeed())
			})

			Expect(out).To(Equal("stream_info tu_2 (iteration 0)\nInput:\n{\n  \"stream\": \"X\"\n}\nAnswer (error):\nno such stream\n"))
		})

		It("Should render the selected calls as one JSON document in the library's shape", func() {
			sessionJSON = true
			queryTools = []string{"stream_ls"}

			out := captureStdout(func() {
				Expect(sessionQueryAction(nil)).To(Succeed())
			})

			var doc struct {
				RunID string           `json:"run_id"`
				Calls []map[string]any `json:"calls"`
			}
			Expect(json.Unmarshal([]byte(out), &doc)).To(Succeed())
			Expect(doc.RunID).To(Equal("2ZqLquery"))
			Expect(doc.Calls).To(HaveLen(1))

			call := doc.Calls[0]
			Expect(call).To(HaveKeyWithValue("tool", "stream_ls"))
			Expect(call).To(HaveKeyWithValue("tool_use_id", "tu_1"))
			Expect(call).To(HaveKeyWithValue("iteration", BeNumerically("==", 0)))
			Expect(call).To(HaveKeyWithValue("time", "2023-11-14T22:13:21Z"))
			Expect(call).To(HaveKeyWithValue("answer_time", "2023-11-14T22:13:22Z"))
			Expect(call).To(HaveKey("answer"))

			answer := call["answer"].(map[string]any)
			Expect(answer).To(HaveKeyWithValue("kind", "application"))
			Expect(answer).To(HaveKeyWithValue("dispatched", true))
			Expect(answer["result"]).To(HaveKeyWithValue("content", "X\nY"))
		})

		It("Should leave the answer out of the JSON under --calls-only and the input under --results-only", func() {
			sessionJSON = true
			queryTools = []string{"stream_ls"}
			queryCallsOnly = true

			out := captureStdout(func() {
				Expect(sessionQueryAction(nil)).To(Succeed())
			})
			Expect(out).To(ContainSubstring(`"input"`))
			Expect(out).ToNot(ContainSubstring(`"answer`))

			queryCallsOnly = false
			queryResultsOnly = true

			out = captureStdout(func() {
				Expect(sessionQueryAction(nil)).To(Succeed())
			})
			Expect(out).ToNot(ContainSubstring(`"input"`))
			Expect(out).To(ContainSubstring(`"answer"`))
		})

		It("Should render an empty list when nothing matches", func() {
			sessionJSON = true
			queryTools = []string{"nothing"}

			out := captureStdout(func() {
				Expect(sessionQueryAction(nil)).To(Succeed())
			})

			Expect(out).To(MatchJSON(`{"run_id":"2ZqLquery","calls":[]}`))
		})

		It("Should report a session the store does not hold", func() {
			sessionArgID = "2ZqLmissing"

			Expect(sessionQueryAction(nil)).To(MatchError(runstate.ErrNotFound))
		})
	})
})

var _ = Describe("session stats", func() {
	BeforeEach(func() {
		useSessionInspectFlags()
	})

	Describe("printSessionStats", func() {
		stats := func(recs ...runstate.Record) string {
			all := []runstate.Record{{Protocol: runstate.MetaProtocol, Time: sessionTime(0), Meta: &runstate.MetaRecord{
				Version: runstate.Version, RunID: "2ZqL", Agent: "ops", Created: sessionTime(0),
				Fingerprint: runstate.Fingerprint{Model: "claude-opus-4-8", MaxTokens: 1000, MaxIterations: 10},
			}}}
			all = append(all, recs...)
			for i := range all {
				all[i].Seq = uint64(i + 1)
			}

			var buf bytes.Buffer
			printSessionStats(&buf, runstate.Stats(all))

			return buf.String()
		}

		It("Should frame the run, measure the tokens against the budget and say what the tool time measured", func() {
			out := stats(
				sessionAssistant(sessionTime(1), 0, sessionCall("tu_1", "stream_ls", `{}`)),
				sessionResult(sessionTime(3), "tu_1", "X", true),
				runstate.Record{Protocol: runstate.TerminalProtocol, Time: sessionTime(4), Terminal: &runstate.TerminalRecord{Reason: runstate.ReasonBudget}},
			)

			// The document renders as Markdown where the environment asks for it, so the
			// assertions stay on values that read the same both ways.
			Expect(out).To(ContainSubstring("ops"))
			Expect(out).To(MatchRegexp(`Status:(\*\*)? budget`))
			Expect(out).To(ContainSubstring("1 (at most 10 per turn)"))
			Expect(out).To(ContainSubstring("15 of 1000 budget (10 in / 5 out)"))
			Expect(out).To(MatchRegexp(`4(\.00)?s \(creation to the last journal record\)`))
			Expect(out).To(MatchRegexp(tableRow("stream_ls", "application", "1", "1", "0", "0", "1", `2(\.00)?s`)))
			Expect(out).To(ContainSubstring("Tool time is the gap between the record that ended each call"))
		})

		It("Should report an open run and say which calls the time covers", func() {
			out := stats(
				runstate.Record{Protocol: runstate.AssistantProtocol, Assistant: &runstate.AssistantRecord{Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{ToolUse: sessionCall("tu_1", "shell", `{}`)}, {ToolUse: sessionCall("tu_2", "shell", `{}`)}}}}},
				runstate.Record{Protocol: runstate.ToolResultProtocol, ToolResult: &runstate.ToolResultRecord{ToolUseID: "tu_1", Result: llm.ToolResultBlock{ToolUseID: "tu_1", Content: "a"}}},
				runstate.Record{Protocol: runstate.ClaimProtocol, Time: sessionTime(10), Claim: &runstate.ClaimRecord{By: "worker-2"}},
				sessionResult(sessionTime(12), "tu_2", "b", false),
			)

			Expect(out).To(MatchRegexp(`Status:(\*\*)? open`))
			Expect(out).To(ContainSubstring("(1 of 2 calls)"))
			Expect(out).To(ContainSubstring("1 of 2 calls ended with a record that carries no time and are not timed.\n"))
			Expect(out).ToNot(ContainSubstring("no answer yet"))
		})

		It("Should report a call a suspended run left unanswered as unanswered, not as undated", func() {
			out := stats(
				sessionAssistant(sessionTime(1), 0, sessionCall("tu_1", "shell", `{}`), sessionCall("tu_2", "shell", `{}`)),
				sessionResult(sessionTime(3), "tu_1", "a", false),
				runstate.Record{Protocol: runstate.TerminalProtocol, Time: sessionTime(4), Terminal: &runstate.TerminalRecord{Reason: runstate.ReasonSuspended}},
			)

			Expect(out).To(ContainSubstring("Tool time is the gap between the record that ended each call"))
			Expect(out).To(HaveSuffix("1 of 2 calls have no answer yet, so they are not timed.\n"))
			Expect(out).ToNot(ContainSubstring("carries no time"))
			Expect(out).ToNot(ContainSubstring("records no times"))
		})

		It("Should report the only call of a dated run that stopped during it as unanswered", func() {
			out := stats(
				sessionAssistant(sessionTime(1), 0, sessionCall("tu_1", "shell", `{}`)),
			)

			Expect(out).To(HaveSuffix("\n1 of 1 calls have no answer yet, so they are not timed.\n"))
			Expect(out).ToNot(ContainSubstring("records no times"))
			Expect(out).ToNot(ContainSubstring("Tool time is the gap"))
		})

		It("Should show no timing for a journal that carries no times", func() {
			var buf bytes.Buffer
			printSessionStats(&buf, runstate.Stats([]runstate.Record{
				{Seq: 1, Protocol: runstate.MetaProtocol, Meta: &runstate.MetaRecord{Version: runstate.Version, RunID: "2ZqL"}},
				{Seq: 2, Protocol: runstate.AssistantProtocol, Assistant: &runstate.AssistantRecord{Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{ToolUse: sessionCall("tu_1", "shell", `{}`)}}}}},
				{Seq: 3, Protocol: runstate.ToolResultProtocol, ToolResult: &runstate.ToolResultRecord{ToolUseID: "tu_1", Result: llm.ToolResultBlock{ToolUseID: "tu_1", Content: "a"}}},
			}))

			out := buf.String()
			Expect(out).To(ContainSubstring("not recorded in this journal"))
			Expect(out).To(ContainSubstring("The journal records no times for these calls"))
			Expect(out).ToNot(ContainSubstring("0s"))
		})

		It("Should strip terminal control sequences from what it prints of the journal", func() {
			out := stats(
				sessionAssistant(sessionTime(1), 0, sessionCall("tu_1", "\x1b]0;pwned\x07shell\x1b[31m", `{}`)),
				sessionResult(sessionTime(2), "tu_1", "a", false),
				runstate.Record{Protocol: runstate.TerminalProtocol, Time: sessionTime(3), Terminal: &runstate.TerminalRecord{Reason: "\x1b[31mdone\x1b]0;pwned\x07"}},
			)

			// The heading is bold on a terminal, so what is asserted is that the journal's
			// own sequences are gone rather than that no escape is printed at all.
			Expect(out).ToNot(ContainSubstring("[31m"))
			Expect(out).ToNot(ContainSubstring("]0;"))
			Expect(out).ToNot(ContainSubstring("pwned"))
			Expect(out).ToNot(ContainSubstring("\x07"))
			Expect(out).To(MatchRegexp(tableRow("shell", "application")))
			Expect(out).To(MatchRegexp(`Status:(\*\*)? done\n`))
		})
	})

	Describe("sessionStatsAction", func() {
		BeforeEach(func() {
			writeSessionJournal("2ZqLstats", runstate.MetaRecord{Prompt: "tidy the streams", Created: sessionTime(0), Fingerprint: runstate.Fingerprint{Model: "claude-opus-4-8"}},
				sessionAssistant(sessionTime(1), 0, sessionCall("tu_1", "stream_ls", `{}`)),
				sessionResult(sessionTime(2), "tu_1", "X", false),
				runstate.Record{Protocol: runstate.MemoryRevisionsProtocol, Optional: true, Time: sessionTime(3), MemoryRevisions: &runstate.MemoryRevisionsRecord{Revisions: map[string]uint64{"notes": 7}}},
				runstate.Record{Protocol: runstate.TerminalProtocol, Time: sessionTime(4), Terminal: &runstate.TerminalRecord{Reason: runstate.ReasonCompleted}},
			)
			sessionArgID = "2ZqLstats"
		})

		It("Should render the library's statistics as one JSON document", func() {
			sessionJSON = true

			out := captureStdout(func() {
				Expect(sessionStatsAction(nil)).To(Succeed())
			})

			var got runstate.Statistics
			Expect(json.Unmarshal([]byte(out), &got)).To(Succeed())
			Expect(got.RunID).To(Equal("2ZqLstats"))
			Expect(got.Terminal).To(Equal(runstate.ReasonCompleted))
			Expect(got.Tokens.Total()).To(Equal(int64(15)))
			Expect(got.Tools).To(HaveLen(1))
			Expect(got.Tools[0].Duration).To(Equal(time.Second))
		})

		It("Should report a session the store does not hold", func() {
			sessionArgID = "2ZqLmissing"

			Expect(sessionStatsAction(nil)).To(MatchError(runstate.ErrNotFound))
		})
	})
})

var _ = Describe("session search", func() {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.Local)

	infos := []runstate.RunInfo{
		{RunID: "a", Agent: "ops", Caller: "peer1", Model: "claude-opus-4-8", Prompt: "list the streams", Terminal: runstate.ReasonCompleted, Created: now.Add(-2 * time.Hour)},
		{RunID: "b", Agent: "ops", Caller: "peer2", Model: "claude-sonnet-4-6", Prompt: "Remove the ORDERS stream", Created: now.Add(-3 * 24 * time.Hour)},
		{RunID: "c", Agent: "billing", Caller: "peer1", Model: "claude-opus-4-8", Prompt: "raise an invoice", Terminal: runstate.ReasonBudget, Created: now.Add(-10 * 24 * time.Hour)},
		{RunID: "d", Model: "claude-opus-4-8", Prompt: "list the consumers", Terminal: runstate.ReasonCompleted, Created: now.Add(-30 * time.Minute)},
	}

	BeforeEach(func() {
		useSessionInspectFlags()
	})

	Describe("newSessionSearchFilter", func() {
		It("Should build a zero filter with no flag given", func() {
			f, err := newSessionSearchFilter(now)
			Expect(err).ToNot(HaveOccurred())
			Expect(f).To(Equal(runstate.SearchFilter{}))
		})

		It("Should carry each flag into the filter", func() {
			searchIdentity = "ops"
			searchModel = "claude-opus-4-8"
			searchCaller = "peer1"
			searchStatus = "open"
			searchMatch = "(?i)orders"

			f, err := newSessionSearchFilter(now)
			Expect(err).ToNot(HaveOccurred())
			Expect(f.Agent).To(Equal("ops"))
			Expect(f.Model).To(Equal("claude-opus-4-8"))
			Expect(f.Caller).To(Equal("peer1"))
			Expect(f.Status).To(Equal(runstate.StatusOpen))
			Expect(f.Prompt.String()).To(Equal("(?i)orders"))
		})

		It("Should take --since and --until as a duration ago", func() {
			searchSince = "24h"
			searchUntil = "7d"

			f, err := newSessionSearchFilter(now)
			Expect(err).ToNot(HaveOccurred())
			Expect(f.Since).To(Equal(now.Add(-24 * time.Hour)))
			Expect(f.Until).To(Equal(now.Add(-7 * 24 * time.Hour)))
		})

		It("Should take --since as a date at local midnight and --until as an RFC3339 time", func() {
			searchSince = "2026-09-18"
			searchUntil = "2026-09-22T11:00:00Z"

			f, err := newSessionSearchFilter(now)
			Expect(err).ToNot(HaveOccurred())
			Expect(f.Since).To(Equal(time.Date(2026, 9, 18, 0, 0, 0, 0, time.Local)))
			Expect(f.Until.Equal(time.Date(2026, 9, 22, 11, 0, 0, 0, time.UTC))).To(BeTrue())
		})

		It("Should select what the flags describe from a listing", func() {
			searchIdentity = "ops"
			searchSince = "7d"

			f, err := newSessionSearchFilter(now)
			Expect(err).ToNot(HaveOccurred())

			found, excluded := f.Select(infos)
			Expect(found).To(HaveLen(2))
			Expect(found[0].RunID).To(Equal("a"))
			Expect(found[1].RunID).To(Equal("b"))
			Expect(excluded).To(Equal(1))
		})

		It("Should refuse a time it cannot read and a pattern that does not compile", func() {
			searchSince = "last tuesday"
			_, err := newSessionSearchFilter(now)
			Expect(err).To(MatchError(ContainSubstring(`invalid --since "last tuesday"`)))

			searchSince = "-2h"
			_, err = newSessionSearchFilter(now)
			Expect(err).To(MatchError(ContainSubstring("invalid --since")))

			searchSince = ""
			searchMatch = "(unclosed"
			_, err = newSessionSearchFilter(now)
			Expect(err).To(MatchError(ContainSubstring("invalid --match pattern")))
		})

		It("Should refuse a status sessionStatus never prints, listing the valid ones", func() {
			app := fisk.New("fisk", "test")
			registerSessionCommand(app)

			_, err := app.Parse([]string{"session", "search", "--status", "done"})
			Expect(err).To(MatchError(ContainSubstring("open,completed,suspended,error,budget,max_iterations")))
		})
	})

	Describe("printSessionSearch", func() {
		It("Should add the columns the filters made relevant and say how many runs --identity left out", func() {
			searchIdentity = "ops"
			searchCaller = "peer1"
			searchSince = "1d"

			f, err := newSessionSearchFilter(now)
			Expect(err).ToNot(HaveOccurred())

			var buf bytes.Buffer
			printSessionSearch(&buf, f, infos[:1], 2)

			out := buf.String()
			Expect(out).To(MatchRegexp(tableRow("ID", "Agent", "Caller", "Model", "Status", "Created", "Updated", "Prompt")))
			Expect(out).To(ContainSubstring("2 more sessions matched every other filter but carry no agent"))
		})

		It("Should print the session ls columns with no filter that adds one", func() {
			f, err := newSessionSearchFilter(now)
			Expect(err).ToNot(HaveOccurred())

			var buf bytes.Buffer
			printSessionSearch(&buf, f, infos[:1], 0)

			out := buf.String()
			Expect(out).To(MatchRegexp(tableRow("ID", "Model", "Status", "Updated", "Prompt")))
			Expect(out).ToNot(ContainSubstring("carry no agent"))
		})

		It("Should strip terminal control sequences from what it prints of the journal", func() {
			searchIdentity = "\x1b[31mops"
			searchCaller = "peer\x1b]0;pwned\x07"

			f, err := newSessionSearchFilter(now)
			Expect(err).ToNot(HaveOccurred())

			var buf bytes.Buffer
			printSessionSearch(&buf, f, []runstate.RunInfo{{
				RunID: "run\x1b[31m-a\x07", Agent: "\x1b[31mops", Caller: "peer\x1b]0;pwned\x07", Model: hostileModel,
				Prompt: "list \x1b[31mthe\x1b[0m\nstreams\x08", Terminal: "\x1b]0;pwned\x07done\x1b[0m",
			}}, 0)

			out := buf.String()
			Expect(out).ToNot(ContainSubstring("\x1b"))
			Expect(out).ToNot(ContainSubstring("pwned"))
			Expect(out).ToNot(ContainSubstring("\x07"))
			Expect(out).ToNot(ContainSubstring("\x08"))
			Expect(out).To(ContainSubstring("list the streams"))
			Expect(out).To(MatchRegexp(tableRow("run-a", "ops", "peer")))
			Expect(out).To(MatchRegexp(tableRow("done")))
		})
	})

	Describe("sessionSearchAction", func() {
		It("Should render the matching runs and the excluded count as one JSON document", func() {
			created := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)

			writeSessionJournal("2ZqLsearch1", runstate.MetaRecord{Agent: "ops", Caller: "peer1", Prompt: "list the streams", Created: created, Fingerprint: runstate.Fingerprint{Model: "claude-opus-4-8"}},
				runstate.Record{Protocol: runstate.TerminalProtocol, Time: created.Add(time.Minute), Terminal: &runstate.TerminalRecord{Reason: runstate.ReasonCompleted}},
			)
			writeSessionJournal("2ZqLsearch2", runstate.MetaRecord{Agent: "billing", Prompt: "raise an invoice", Created: created})
			writeSessionJournal("2ZqLsearch3", runstate.MetaRecord{Prompt: "list the consumers", Created: created})

			sessionJSON = true
			searchIdentity = "ops"
			searchSince = "2h"

			out := captureStdout(func() {
				Expect(sessionSearchAction(nil)).To(Succeed())
			})

			Expect(out).To(MatchJSON(fmt.Sprintf(`{
				"runs": [{
					"run_id": "2ZqLsearch1",
					"agent": "ops",
					"caller": "peer1",
					"model": "claude-opus-4-8",
					"status": "completed",
					"created": %[1]q,
					"updated": %[2]q,
					"ended": %[2]q,
					"prompt": "list the streams"
				}],
				"no_agent_excluded": 1
			}`, created.Format(time.RFC3339), created.Add(time.Minute).Format(time.RFC3339))))
		})

		It("Should render an empty list when nothing matches", func() {
			sessionJSON = true
			searchModel = "nothing"

			out := captureStdout(func() {
				Expect(sessionSearchAction(nil)).To(Succeed())
			})

			Expect(out).To(MatchJSON(`{"runs":[],"no_agent_excluded":0}`))
		})
	})
})
