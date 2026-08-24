//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package runstate

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/segmentio/ksuid"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/toolkit"
)

func TestRunState(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "RunState")
}

func newID() string {
	return ksuid.New().String()
}

func textBlock(s string) llm.ContentBlock {
	return llm.ContentBlock{Text: &llm.TextBlock{Text: s}}
}

func assistantMessage(blocks ...llm.ContentBlock) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: blocks}
}

func assistantWithTools(iter int64, ids ...string) *AssistantRecord {
	content := []llm.ContentBlock{textBlock("working")}
	for _, id := range ids {
		content = append(content, llm.ContentBlock{ToolUse: &llm.ToolUseBlock{ID: id, Name: "shell", Input: json.RawMessage(`{"x":1}`)}})
	}
	return &AssistantRecord{
		Iteration: iter,
		Message:   assistantMessage(content...),
		InTokens:  10,
		OutTokens: 5,
	}
}

func toolResult(id string) *ToolResultRecord {
	return toolResultKind(id, toolkit.KindApplication)
}

// toolResultKind is a result a named provider served and the call was dispatched to,
// for the per-kind accounting the fold recomputes. Every record a run writes carries a
// kind, so the shared helper does too and the schema is exercised with the field
// present.
func toolResultKind(id string, kind toolkit.Kind) *ToolResultRecord {
	rec := toolResultDenied(id, kind)
	rec.Remote = kind == toolkit.KindRemote
	rec.Dispatched = true

	return rec
}

// toolResultDenied is a result for a call that was answered without reaching its
// provider, which a run records with a kind and no dispatch flag.
func toolResultDenied(id string, kind toolkit.Kind) *ToolResultRecord {
	return &ToolResultRecord{ToolUseID: id, Result: llm.ToolResultBlock{ToolUseID: id, Content: "ok"}, Kind: kind.String()}
}

func userRecord(text string) *UserRecord {
	return &UserRecord{Message: llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{textBlock(text)}}}
}

func assistantText(iter int64, stop, text string) *AssistantRecord {
	return &AssistantRecord{
		Iteration:  iter,
		StopReason: stop,
		Message:    assistantMessage(textBlock(text)),
		InTokens:   3,
		OutTokens:  4,
	}
}

// userTexts returns the concatenated text blocks of every user message in the folded
// conversation, so a test can assert the reconstructed follow-ups without depending on
// block ordering within a message.
func userTexts(rs *RunState) []string {
	var out []string
	for _, msg := range rs.Messages {
		if msg.Role != llm.RoleUser {
			continue
		}
		var text string
		for _, block := range msg.Content {
			if block.Text != nil {
				text += block.Text.Text
			}
		}
		out = append(out, text)
	}
	return out
}

var _ = Describe("runstate", func() {
	Describe("Fold", func() {
		meta := func() Record {
			return Record{Seq: 1, Protocol: MetaProtocol, Meta: &MetaRecord{
				Version: Version, RunID: newID(), Prompt: "start here",
				Fingerprint: Fingerprint{Model: "claude-opus-4-8"},
			}}
		}

		It("rebuilds the initial prompt as the first user message", func() {
			rs, err := Fold([]Record{meta()})
			Expect(err).NotTo(HaveOccurred())
			Expect(rs.Messages).To(HaveLen(1))
			Expect(rs.Pending).To(BeNil())
			Expect(rs.NextIteration).To(Equal(int64(0)))
		})

		It("restores the conversation token and the caller", func() {
			rec := meta()
			rec.Meta.ConversationToken = "3Hzmp8VqrKL42NmXcPd7bTgWfR1"
			rec.Meta.Caller = "peer1"

			rs, err := Fold([]Record{rec})
			Expect(err).NotTo(HaveOccurred())
			Expect(rs.ConversationToken).To(Equal("3Hzmp8VqrKL42NmXcPd7bTgWfR1"))
			Expect(rs.Caller).To(Equal("peer1"))
		})

		// A journal written before either field existed folds with both empty rather
		// than failing, which is what keeps the record version at 3.
		It("folds a journal that carries neither", func() {
			rs, err := Fold([]Record{meta()})
			Expect(err).NotTo(HaveOccurred())
			Expect(rs.ConversationToken).To(BeEmpty())
			Expect(rs.Caller).To(BeEmpty())
		})

		claim := func(seq uint64) Record {
			return Record{Seq: seq, Protocol: ClaimProtocol, Claim: &ClaimRecord{By: "worker-a", Claimed: time.Now().UTC()}}
		}

		// A claim is written on resume, so it lands wherever a takeover happened,
		// including between an assistant turn and the tool results answering it. It must
		// change nothing at all: a fold that committed the turn there would destroy the
		// Pending batch the resume exists to finish.
		It("folds identically with a claim record anywhere in the journal", func() {
			base := []Record{
				meta(),
				{Seq: 2, Protocol: AssistantProtocol, Assistant: assistantWithTools(0, "tu_1", "tu_2")},
				{Seq: 3, Protocol: ToolResultProtocol, ToolResult: toolResult("tu_1")},
			}

			want, err := Fold(base)
			Expect(err).NotTo(HaveOccurred())
			Expect(want.Pending).NotTo(BeNil(), "tu_2 is unanswered, so this run has an in-flight batch")

			// Every position a claim can occupy, with the following records renumbered.
			positions := map[string][]Record{
				"before the first turn": {
					meta(), claim(2),
					{Seq: 3, Protocol: AssistantProtocol, Assistant: assistantWithTools(0, "tu_1", "tu_2")},
					{Seq: 4, Protocol: ToolResultProtocol, ToolResult: toolResult("tu_1")},
				},
				"between a turn and its results": {
					meta(),
					{Seq: 2, Protocol: AssistantProtocol, Assistant: assistantWithTools(0, "tu_1", "tu_2")},
					claim(3),
					{Seq: 4, Protocol: ToolResultProtocol, ToolResult: toolResult("tu_1")},
				},
				"as the trailing record": {
					meta(),
					{Seq: 2, Protocol: AssistantProtocol, Assistant: assistantWithTools(0, "tu_1", "tu_2")},
					{Seq: 3, Protocol: ToolResultProtocol, ToolResult: toolResult("tu_1")},
					claim(4),
				},
			}

			for name, recs := range positions {
				got, err := Fold(recs)
				Expect(err).NotTo(HaveOccurred(), name)
				Expect(got.Messages).To(Equal(want.Messages), name)
				Expect(got.Pending).To(Equal(want.Pending), name)
				Expect(got.Counters).To(Equal(want.Counters), name)
				Expect(got.NextIteration).To(Equal(want.NextIteration), name)
				Expect(got.Terminal).To(Equal(want.Terminal), name)
			}
		})

		It("counts every tool result under the kind its record carries", func() {
			recs := []Record{
				meta(),
				{Seq: 2, Protocol: AssistantProtocol, Assistant: assistantWithTools(0, "tu_1", "tu_2", "tu_3")},
				{Seq: 3, Protocol: ToolResultProtocol, ToolResult: toolResultKind("tu_1", toolkit.KindApplication)},
				{Seq: 4, Protocol: ToolResultProtocol, ToolResult: toolResultKind("tu_2", toolkit.KindMCP)},
				{Seq: 5, Protocol: ToolResultProtocol, ToolResult: toolResultKind("tu_3", toolkit.KindMCP)},
			}

			rs, err := Fold(recs)
			Expect(err).NotTo(HaveOccurred())
			Expect(rs.Counters.ToolCalls).To(Equal(int64(3)))
			Expect(rs.Counters.MCPToolCalls).To(Equal(int64(2)))
			Expect(rs.Counters.ToolCallsByKind).To(Equal(map[toolkit.Kind]int64{
				toolkit.KindApplication: 1,
				toolkit.KindMCP:         2,
			}))

			var summed int64
			for _, n := range rs.Counters.ToolCallsByKind {
				summed += n
			}
			Expect(summed).To(Equal(rs.Counters.ToolCalls), "per-kind buckets must partition tool_calls")
		})

		// The buckets take a call whatever became of it and the two dispatch counters take
		// only the calls that reached their provider, so a call a policy hook denied or the
		// operator refused folds back into its bucket and into neither counter.
		It("keeps a call that never reached its provider out of the dispatch counters", func() {
			recs := []Record{
				meta(),
				{Seq: 2, Protocol: AssistantProtocol, Assistant: assistantWithTools(0, "tu_1", "tu_2", "tu_3", "tu_4")},
				{Seq: 3, Protocol: ToolResultProtocol, ToolResult: toolResultKind("tu_1", toolkit.KindMCP)},
				{Seq: 4, Protocol: ToolResultProtocol, ToolResult: toolResultDenied("tu_2", toolkit.KindMCP)},
				{Seq: 5, Protocol: ToolResultProtocol, ToolResult: toolResultKind("tu_3", toolkit.KindRemote)},
				{Seq: 6, Protocol: ToolResultProtocol, ToolResult: toolResultDenied("tu_4", toolkit.KindRemote)},
			}

			rs, err := Fold(recs)
			Expect(err).NotTo(HaveOccurred())
			Expect(rs.Counters.ToolCalls).To(Equal(int64(4)))
			Expect(rs.Counters.MCPToolCalls).To(Equal(int64(1)), "the denied MCP call was never dispatched")
			Expect(rs.Counters.RemoteToolCalls).To(Equal(int64(1)), "the denied remote call was never dispatched")
			Expect(rs.Counters.ToolCallsByKind).To(Equal(map[toolkit.Kind]int64{
				toolkit.KindMCP:    2,
				toolkit.KindRemote: 2,
			}))

			var summed int64
			for _, n := range rs.Counters.ToolCallsByKind {
				summed += n
			}
			Expect(summed).To(Equal(rs.Counters.ToolCalls), "per-kind buckets must partition tool_calls")
		})

		// A record written before the kind field existed carries no token. Its remote flag
		// is what such a journal has always reported, so it still folds as the remote kind
		// and as a dispatch, and a record without one folds as the unknown kind and no
		// dispatch rather than being guessed at from the tool name.
		It("folds a record with no kind from its remote flag alone", func() {
			legacy := func(id string, remote bool) *ToolResultRecord {
				return &ToolResultRecord{ToolUseID: id, Result: llm.ToolResultBlock{ToolUseID: id, Content: "ok"}, Remote: remote}
			}

			rs, err := Fold([]Record{
				meta(),
				{Seq: 2, Protocol: AssistantProtocol, Assistant: assistantWithTools(0, "tu_1", "tu_2")},
				{Seq: 3, Protocol: ToolResultProtocol, ToolResult: legacy("tu_1", true)},
				{Seq: 4, Protocol: ToolResultProtocol, ToolResult: legacy("tu_2", false)},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(rs.Counters.ToolCalls).To(Equal(int64(2)))
			Expect(rs.Counters.RemoteToolCalls).To(Equal(int64(1)))
			Expect(rs.Counters.MCPToolCalls).To(BeZero())
			Expect(rs.Counters.ToolCallsByKind).To(Equal(map[toolkit.Kind]int64{
				toolkit.KindRemote:  1,
				toolkit.KindUnknown: 1,
			}))
		})

		// A token this build has no Kind for is what a journal written by a newer one
		// carries. The call is still counted, under the sentinel rather than under a
		// provider it was not.
		It("counts an unrecognized kind token as the unknown kind", func() {
			rec := &ToolResultRecord{ToolUseID: "tu_1", Result: llm.ToolResultBlock{ToolUseID: "tu_1", Content: "ok"}, Kind: "quantum"}

			rs, err := Fold([]Record{
				meta(),
				{Seq: 2, Protocol: AssistantProtocol, Assistant: assistantWithTools(0, "tu_1")},
				{Seq: 3, Protocol: ToolResultProtocol, ToolResult: rec},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(rs.Counters.ToolCallsByKind).To(Equal(map[toolkit.Kind]int64{toolkit.KindUnknown: 1}))
		})

		It("rejects a claim record with no payload", func() {
			_, err := Fold([]Record{meta(), {Seq: 2, Protocol: ClaimProtocol}})
			Expect(err).To(MatchError(ErrCorrupt))
		})

		It("folds decision records into the approvals in journal order", func() {
			recs := []Record{
				meta(),
				{Seq: 2, Protocol: AssistantProtocol, Assistant: assistantWithTools(0, "tu_1")},
				{Seq: 3, Protocol: ToolResultProtocol, ToolResult: toolResult("tu_1")},
				{Seq: 4, Protocol: DecisionProtocol, Optional: true, Decision: &DecisionRecord{Tool: "stream_rm"}},
				{Seq: 5, Protocol: DecisionProtocol, Optional: true, Decision: &DecisionRecord{Tool: "server_run"}},
			}
			rs, err := Fold(recs)
			Expect(err).NotTo(HaveOccurred())
			Expect(rs.Approvals).To(Equal([]string{"stream_rm", "server_run"}))
			// A grant lands after the result of the call that triggered it, so it must
			// leave the turn it sits inside exactly as it found it.
			Expect(rs.Pending).To(BeNil())
			Expect(rs.Messages).To(HaveLen(3))
			Expect(rs.Counters.ToolCalls).To(Equal(int64(1)))
		})

		// A terminal record ends a run rather than a conversation, so a turn delivered
		// on a later resume folds in behind one and the record that run writes replaces
		// it.
		It("folds a user turn that follows a terminal record", func() {
			recs := []Record{
				meta(),
				{Seq: 2, Protocol: AssistantProtocol, Assistant: assistantText(0, "end_turn", "first answer")},
				{Seq: 3, Protocol: TerminalProtocol, Terminal: &TerminalRecord{Reason: ReasonCompleted}},
				{Seq: 4, Protocol: ClaimProtocol, Claim: &ClaimRecord{By: "worker-2"}},
				{Seq: 5, Protocol: UserProtocol, User: userRecord("second question")},
				{Seq: 6, Protocol: AssistantProtocol, Assistant: assistantText(1, "end_turn", "second answer")},
				{Seq: 7, Protocol: TerminalProtocol, Terminal: &TerminalRecord{Reason: ReasonSuspended}},
			}
			rs, err := Fold(recs)
			Expect(err).NotTo(HaveOccurred())
			Expect(rs.Messages).To(HaveLen(4))
			Expect(userTexts(rs)).To(Equal([]string{"start here", "second question"}))
			Expect(rs.Terminal).To(Equal(&TerminalRecord{Reason: ReasonSuspended}))
			Expect(rs.Completed()).To(BeFalse())
			Expect(rs.NextIteration).To(Equal(int64(2)))
		})

		It("rejects a decision record with no payload", func() {
			_, err := Fold([]Record{meta(), {Seq: 2, Protocol: DecisionProtocol, Optional: true}})
			Expect(err).To(MatchError(ErrCorrupt))
		})

		It("folds call approvals in journal order and leaves the turn alone", func() {
			recs := []Record{
				meta(),
				{Seq: 2, Protocol: AssistantProtocol, Assistant: assistantWithTools(0, "tu_1")},
				{Seq: 3, Protocol: CallApprovalProtocol, Optional: true, CallApproval: &CallApprovalRecord{ToolUseID: "tu_1", ToolName: "stream_rm"}},
			}
			rs, err := Fold(recs)
			Expect(err).NotTo(HaveOccurred())
			Expect(rs.CallApprovals).To(Equal([]CallApprovalRecord{{ToolUseID: "tu_1", ToolName: "stream_rm"}}))
			// The call it names is unanswered, so the batch it approves is still open.
			Expect(rs.Pending).ToNot(BeNil())
			Expect(rs.Pending.Answered).To(BeEmpty())
		})

		// A one-shot approval authorizes the next dispatch of one call. A run that ended
		// before reaching it spends it, so a later question nobody answers cannot be
		// followed by a dispatch this authorizes.
		It("spends a call approval a terminal record follows", func() {
			recs := []Record{
				meta(),
				{Seq: 2, Protocol: AssistantProtocol, Assistant: assistantWithTools(0, "tu_1")},
				{Seq: 3, Protocol: CallApprovalProtocol, Optional: true, CallApproval: &CallApprovalRecord{ToolUseID: "tu_1", ToolName: "stream_rm"}},
				{Seq: 4, Protocol: TerminalProtocol, Terminal: &TerminalRecord{Reason: ReasonSuspended}},
				{Seq: 5, Protocol: CallApprovalProtocol, Optional: true, CallApproval: &CallApprovalRecord{ToolUseID: "tu_2", ToolName: "server_run"}},
			}
			rs, err := Fold(recs)
			Expect(err).NotTo(HaveOccurred())
			Expect(rs.CallApprovals).To(Equal([]CallApprovalRecord{{ToolUseID: "tu_2", ToolName: "server_run"}}))
			// A standing grant covers the conversation, so a suspend does not touch it.
			Expect(rs.Approvals).To(BeEmpty())
		})

		It("rejects a call approval record with no payload", func() {
			_, err := Fold([]Record{meta(), {Seq: 2, Protocol: CallApprovalProtocol, Optional: true}})
			Expect(err).To(MatchError(ErrCorrupt))
		})

		// The record is written as a run ends, which is after its terminal record, so it
		// folds behind one and must leave the turn it sits inside alone.
		It("folds memory revisions and leaves the turn alone", func() {
			recs := []Record{
				meta(),
				{Seq: 2, Protocol: AssistantProtocol, Assistant: assistantWithTools(0, "tu_1")},
				{Seq: 3, Protocol: MemoryRevisionsProtocol, Optional: true, MemoryRevisions: &MemoryRevisionsRecord{Revisions: map[string]uint64{"notes": 7}}},
			}
			rs, err := Fold(recs)
			Expect(err).NotTo(HaveOccurred())
			Expect(rs.MemoryRevisions).To(Equal(map[string]uint64{"notes": 7}))
			Expect(rs.Pending).ToNot(BeNil())
			Expect(rs.Pending.Answered).To(BeEmpty())
		})

		// A run that dropped a revision after a refused write records what it holds now,
		// so merging would restore what it deliberately dropped.
		It("keeps only the newest memory revisions record", func() {
			recs := []Record{
				meta(),
				{Seq: 2, Protocol: AssistantProtocol, Assistant: assistantText(0, "end_turn", "first answer")},
				{Seq: 3, Protocol: TerminalProtocol, Terminal: &TerminalRecord{Reason: ReasonCompleted}},
				{Seq: 4, Protocol: MemoryRevisionsProtocol, Optional: true, MemoryRevisions: &MemoryRevisionsRecord{Revisions: map[string]uint64{"notes": 7, "plans": 2}}},
				{Seq: 5, Protocol: UserProtocol, User: userRecord("second question")},
				{Seq: 6, Protocol: AssistantProtocol, Assistant: assistantText(1, "end_turn", "second answer")},
				{Seq: 7, Protocol: TerminalProtocol, Terminal: &TerminalRecord{Reason: ReasonCompleted}},
				{Seq: 8, Protocol: MemoryRevisionsProtocol, Optional: true, MemoryRevisions: &MemoryRevisionsRecord{Revisions: map[string]uint64{"notes": 9}}},
			}
			rs, err := Fold(recs)
			Expect(err).NotTo(HaveOccurred())
			Expect(rs.MemoryRevisions).To(Equal(map[string]uint64{"notes": 9}))
		})

		It("rejects a memory revisions record with no payload", func() {
			_, err := Fold([]Record{meta(), {Seq: 2, Protocol: MemoryRevisionsProtocol, Optional: true}})
			Expect(err).To(MatchError(ErrCorrupt))
		})

		// A newer build's record kind reaches this one as an unknown protocol. Skipping
		// is the writer's declaration that a reader without it is more conservative, not
		// this reader's guess.
		It("skips an unrecognized protocol only when it is marked optional", func() {
			recs := []Record{
				meta(),
				{Seq: 2, Protocol: AssistantProtocol, Assistant: assistantText(0, "end_turn", "done")},
				{Seq: 3, Protocol: Protocol("io.choria.fisk-ai.v1.session.from_the_future"), Optional: true},
			}
			rs, err := Fold(recs)
			Expect(err).NotTo(HaveOccurred())
			Expect(rs.Messages).To(HaveLen(2))

			recs[2].Optional = false
			_, err = Fold(recs)
			Expect(err).To(MatchError(ErrCorrupt))
		})

		It("commits a complete turn and derives counters", func() {
			recs := []Record{
				meta(),
				{Seq: 2, Protocol: AssistantProtocol, Assistant: assistantWithTools(0, "tu_1")},
				{Seq: 3, Protocol: ToolResultProtocol, ToolResult: toolResult("tu_1")},
			}
			rs, err := Fold(recs)
			Expect(err).NotTo(HaveOccurred())
			Expect(rs.Pending).To(BeNil())
			// user(prompt), assistant, user(results)
			Expect(rs.Messages).To(HaveLen(3))
			Expect(rs.NextIteration).To(Equal(int64(1)))
			Expect(rs.Counters.LlmCalls).To(Equal(int64(1)))
			Expect(rs.Counters.ToolCalls).To(Equal(int64(1)))
			Expect(rs.Counters.InTokens).To(Equal(int64(10)))
			Expect(rs.Counters.OutTokens).To(Equal(int64(5)))
		})

		It("sums the cache token split across assistant records", func() {
			recs := []Record{
				meta(),
				{Seq: 2, Protocol: AssistantProtocol, Assistant: &AssistantRecord{
					Iteration: 0, Message: assistantMessage(textBlock("a")),
					InTokens: 10, OutTokens: 5, CacheReadTokens: 100, CacheCreateTokens: 40,
				}},
				{Seq: 3, Protocol: AssistantProtocol, Assistant: &AssistantRecord{
					Iteration: 1, Message: assistantMessage(textBlock("b")),
					InTokens: 2, OutTokens: 3, CacheReadTokens: 200,
				}},
			}
			rs, err := Fold(recs)
			Expect(err).NotTo(HaveOccurred())
			Expect(rs.Counters.CacheReadTokens).To(Equal(int64(300)))
			Expect(rs.Counters.CacheCreateTokens).To(Equal(int64(40)))
		})

		It("folds a pre-caching record (no cache fields) as zero", func() {
			// A journal written before prompt caching omits the cache fields; they read as
			// zero, which is correct since caching was off and there were none.
			recs := []Record{
				meta(),
				{Seq: 2, Protocol: AssistantProtocol, Assistant: assistantText(0, "end_turn", "done")},
			}
			rs, err := Fold(recs)
			Expect(err).NotTo(HaveOccurred())
			Expect(rs.Counters.CacheReadTokens).To(BeZero())
			Expect(rs.Counters.CacheCreateTokens).To(BeZero())
		})

		It("leaves an unanswered tool batch as a pending turn", func() {
			recs := []Record{
				meta(),
				{Seq: 2, Protocol: AssistantProtocol, Assistant: assistantWithTools(0, "tu_1", "tu_2")},
				{Seq: 3, Protocol: ToolResultProtocol, ToolResult: toolResult("tu_1")},
			}
			rs, err := Fold(recs)
			Expect(err).NotTo(HaveOccurred())
			Expect(rs.Pending).NotTo(BeNil())
			Expect(rs.Pending.Answered).To(HaveKeyWithValue("tu_1", true))
			Expect(rs.Pending.Answered).NotTo(HaveKey("tu_2"))
			Expect(rs.Pending.Results).To(HaveLen(1))
			// The in-flight assistant turn is not committed to Messages.
			Expect(rs.Messages).To(HaveLen(1))
			Expect(rs.NextIteration).To(Equal(int64(1)))
			Expect(unansweredToolUses(rs.Pending.Assistant, rs.Pending.Answered)).To(Equal([]string{"tu_2"}))
		})

		It("surfaces a trailing paused turn's stop reason and commits it", func() {
			recs := []Record{
				meta(),
				{Seq: 2, Protocol: AssistantProtocol, Assistant: &AssistantRecord{
					Iteration:  0,
					StopReason: "pause_turn",
					Message:    assistantMessage(textBlock("searching")),
				}},
			}
			rs, err := Fold(recs)
			Expect(err).NotTo(HaveOccurred())
			Expect(rs.Pending).To(BeNil())
			Expect(rs.LastStopReason).To(Equal("pause_turn"))
			Expect(rs.Messages).To(HaveLen(2))
		})

		It("records terminal state and reports completion", func() {
			recs := []Record{
				meta(),
				{Seq: 2, Protocol: AssistantProtocol, Assistant: &AssistantRecord{Iteration: 0, Message: assistantMessage(textBlock("final"))}},
				{Seq: 3, Protocol: TerminalProtocol, Terminal: &TerminalRecord{Reason: ReasonCompleted}},
			}
			rs, err := Fold(recs)
			Expect(err).NotTo(HaveOccurred())
			Expect(rs.Completed()).To(BeTrue())
		})

		It("rejects any version other than the current one", func() {
			for _, v := range []int{Version - 1, Version + 1} {
				r := meta()
				r.Meta.Version = v
				_, err := Fold([]Record{r})
				Expect(err).To(MatchError(ErrVersion), "version %d must be rejected", v)
			}
		})

		It("rejects records that do not start with meta", func() {
			_, err := Fold([]Record{{Seq: 1, Protocol: AssistantProtocol, Assistant: assistantWithTools(0, "tu_1")}})
			Expect(err).To(MatchError(ErrNoMeta))
		})

		It("rejects a non-increasing seq", func() {
			recs := []Record{meta(), {Seq: 1, Protocol: AssistantProtocol, Assistant: assistantWithTools(0, "tu_1")}}
			_, err := Fold(recs)
			Expect(err).To(MatchError(ErrCorrupt))
		})

		It("restores the interactive flag from meta", func() {
			r := meta()
			r.Meta.Interactive = true
			rs, err := Fold([]Record{r})
			Expect(err).NotTo(HaveOccurred())
			Expect(rs.Interactive).To(BeTrue())
		})

		It("appends an interactive follow-up as a new user turn after a completed answer", func() {
			recs := []Record{
				meta(),
				{Seq: 2, Protocol: AssistantProtocol, Assistant: assistantText(0, "end_turn", "first answer")},
				{Seq: 3, Protocol: UserProtocol, User: userRecord("second question")},
				{Seq: 4, Protocol: AssistantProtocol, Assistant: assistantText(1, "end_turn", "second answer")},
			}
			rs, err := Fold(recs)
			Expect(err).NotTo(HaveOccurred())
			// user(prompt), assistant, user(follow-up), assistant
			Expect(rs.Messages).To(HaveLen(4))
			Expect(userTexts(rs)).To(Equal([]string{"start here", "second question"}))
			Expect(rs.NextIteration).To(Equal(int64(2)))
			Expect(rs.Counters.LlmCalls).To(Equal(int64(2)))
		})

		It("folds a follow-up into a trailing tool-results turn, mirroring the post-error runtime fold", func() {
			// A turn calls a tool (answered), then the next LLM call errored before a
			// reply, so the conversation rests on a dangling user(results) turn and the
			// follow-up must merge into it rather than open a second user turn in a row.
			recs := []Record{
				meta(),
				{Seq: 2, Protocol: AssistantProtocol, Assistant: assistantWithTools(0, "tu_1")},
				{Seq: 3, Protocol: ToolResultProtocol, ToolResult: toolResult("tu_1")},
				{Seq: 4, Protocol: UserProtocol, User: userRecord("carry on")},
			}
			rs, err := Fold(recs)
			Expect(err).NotTo(HaveOccurred())
			// user(prompt), assistant(tool call), user(results + follow-up merged)
			Expect(rs.Messages).To(HaveLen(3))
			last := rs.Messages[2]
			Expect(last.Role).To(Equal(llm.RoleUser))
			// The merged user turn carries both the tool result and the follow-up text.
			Expect(userTexts(rs)).To(Equal([]string{"start here", "carry on"}))
			hasResult := false
			for _, b := range last.Content {
				if b.ToolResult != nil {
					hasResult = true
				}
			}
			Expect(hasResult).To(BeTrue(), "the tool result must survive alongside the follow-up")
		})

		It("merges consecutive user records into one turn", func() {
			recs := []Record{
				meta(),
				{Seq: 2, Protocol: AssistantProtocol, Assistant: assistantWithTools(0, "tu_1")},
				{Seq: 3, Protocol: ToolResultProtocol, ToolResult: toolResult("tu_1")},
				{Seq: 4, Protocol: UserProtocol, User: userRecord("one")},
				{Seq: 5, Protocol: UserProtocol, User: userRecord("two")},
			}
			rs, err := Fold(recs)
			Expect(err).NotTo(HaveOccurred())
			Expect(rs.Messages).To(HaveLen(3))
			Expect(userTexts(rs)).To(Equal([]string{"start here", "onetwo"}))
		})

		It("preserves the resume position and stop reason when the journal ends on a follow-up", func() {
			// submit follow-up -> LLM call fails -> operator leaves: the journal's last
			// structural record is a user turn, but NextIteration must still point past the
			// last assistant so a resumed turn does not reuse an iteration index.
			recs := []Record{
				meta(),
				{Seq: 2, Protocol: AssistantProtocol, Assistant: assistantText(0, "end_turn", "answer")},
				{Seq: 3, Protocol: UserProtocol, User: userRecord("next")},
				{Seq: 4, Protocol: TerminalProtocol, Terminal: &TerminalRecord{Reason: ReasonSuspended}},
			}
			rs, err := Fold(recs)
			Expect(err).NotTo(HaveOccurred())
			Expect(rs.NextIteration).To(Equal(int64(1)))
			Expect(rs.LastStopReason).To(Equal("end_turn"))
			Expect(rs.Pending).To(BeNil())
			Expect(rs.Completed()).To(BeFalse())
			Expect(userTexts(rs)).To(Equal([]string{"start here", "next"}))
		})

		It("reconstructs an identical conversation whether follow-ups were merged at runtime or journaled separately", func() {
			// Two follow-ups journaled as separate user records after a dangling results
			// turn must fold to the same Messages as if the runtime had merged them.
			separate := []Record{
				meta(),
				{Seq: 2, Protocol: AssistantProtocol, Assistant: assistantWithTools(0, "tu_1")},
				{Seq: 3, Protocol: ToolResultProtocol, ToolResult: toolResult("tu_1")},
				{Seq: 4, Protocol: UserProtocol, User: userRecord("part one ")},
				{Seq: 5, Protocol: UserProtocol, User: userRecord("part two")},
			}
			merged := []Record{
				meta(),
				{Seq: 2, Protocol: AssistantProtocol, Assistant: assistantWithTools(0, "tu_1")},
				{Seq: 3, Protocol: ToolResultProtocol, ToolResult: toolResult("tu_1")},
				{Seq: 4, Protocol: UserProtocol, User: userRecord("part one part two")},
			}
			a, err := Fold(separate)
			Expect(err).NotTo(HaveOccurred())
			b, err := Fold(merged)
			Expect(err).NotTo(HaveOccurred())
			Expect(userTexts(a)).To(Equal(userTexts(b)))
			Expect(a.Messages).To(HaveLen(len(b.Messages)))
		})

		It("resumes a chat across a suspend terminal record", func() {
			// The normal resumed-chat shape: a suspended session, then more turns appended
			// after resume. The final terminal wins for completion, and the follow-up added
			// after the suspend is part of the conversation.
			recs := []Record{
				meta(),
				{Seq: 2, Protocol: AssistantProtocol, Assistant: assistantText(0, "end_turn", "answer one")},
				{Seq: 3, Protocol: TerminalProtocol, Terminal: &TerminalRecord{Reason: ReasonSuspended}},
				{Seq: 4, Protocol: UserProtocol, User: userRecord("again")},
				{Seq: 5, Protocol: AssistantProtocol, Assistant: assistantText(1, "end_turn", "answer two")},
			}
			rs, err := Fold(recs)
			Expect(err).NotTo(HaveOccurred())
			Expect(rs.Completed()).To(BeFalse())
			Expect(rs.NextIteration).To(Equal(int64(2)))
			Expect(userTexts(rs)).To(Equal([]string{"start here", "again"}))
		})
	})

	Describe("Fingerprint", func() {
		It("reports an actionable field-level diff", func() {
			a := Fingerprint{Model: "claude-opus-4-7", SystemHash: "h1", MaxTokens: 100}
			b := Fingerprint{Model: "claude-opus-4-8", SystemHash: "h2", MaxTokens: 100}
			Expect(a.Equal(b)).To(BeFalse())
			Expect(a.Diff(b)).To(ConsistOf("model: claude-opus-4-7 -> claude-opus-4-8", "system prompt: changed"))
		})

		It("keeps the budget bounds out of the drift a resume refuses", func() {
			a := Fingerprint{Model: "m", MaxTokens: 100, MaxIterations: 5}
			b := Fingerprint{Model: "m", MaxTokens: 200, MaxIterations: 9}
			Expect(a.BlockingDiff(b)).To(BeEmpty())
			Expect(a.BudgetDiff(b)).To(ConsistOf("max_tokens: 100 -> 200", "max_iterations: 5 -> 9"))
			Expect(a.Diff(b)).To(ConsistOf("max_tokens: 100 -> 200", "max_iterations: 5 -> 9"))
			Expect(a.Equal(b)).To(BeFalse())
		})

		// A provider reads a stored conversation whether or not it still holds every
		// tool the history names, so a changed tool set continues. It is its own diff
		// because it endangers a standing approval, which is keyed on a tool name.
		It("reports a changed tool set as drift a resume continues through", func() {
			a := Fingerprint{Model: "m", ToolsHash: "h1", MaxTokens: 100}
			b := Fingerprint{Model: "m", ToolsHash: "h2", MaxTokens: 100}
			Expect(a.ToolsDiff(b)).To(ConsistOf("tool set: changed"))
			Expect(a.BlockingDiff(b)).To(BeEmpty())
			Expect(a.BudgetDiff(b)).To(BeEmpty())

			// Equal is the strict comparison, so it still sees it.
			Expect(a.Equal(b)).To(BeFalse())
			Expect(a.Diff(b)).To(ConsistOf("tool set: changed"))
		})

		It("excludes the provider from Equal and Diff so it stays a separate hard gate", func() {
			a := Fingerprint{Provider: "anthropic", Model: "m"}
			b := Fingerprint{Provider: "openai", Model: "m"}
			Expect(a.Equal(b)).To(BeTrue())
			Expect(a.Diff(b)).To(BeEmpty())
		})

		// A run that asks for no effort computes it empty, and a journal written before
		// the field existed folds it empty, so an old session resumes against a build
		// that has the field.
		It("matches a fingerprint written before reasoning_effort existed", func() {
			var old Fingerprint
			Expect(json.Unmarshal([]byte(`{"model":"m","system_hash":"h","tools_hash":"t","thinking_mode":"off","max_tokens":1,"max_iterations":2}`), &old)).To(Succeed())

			now := Fingerprint{Model: "m", SystemHash: "h", ToolsHash: "t", ThinkingMode: "off", MaxTokens: 1, MaxIterations: 2}
			Expect(old.Equal(now)).To(BeTrue())
		})

		It("reports an effort change, naming an absent level rather than leaving a gap", func() {
			a := Fingerprint{Model: "m"}
			b := Fingerprint{Model: "m", ReasoningEffort: "xhigh"}
			Expect(a.Equal(b)).To(BeFalse())
			Expect(a.Diff(b)).To(ConsistOf("reasoning_effort: none -> xhigh"))
		})

		It("never stores the raw system prompt", func() {
			secret := "SENSITIVE-SYSTEM-PROMPT-TEXT"
			fp := Fingerprint{Model: "m", SystemHash: HashHex([]byte(secret))}
			data, err := json.Marshal(fp)
			Expect(err).NotTo(HaveOccurred())
			Expect(bytes.Contains(data, []byte(secret))).To(BeFalse())
		})
	})
})
