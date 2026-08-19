//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package transcript_test

import (
	"encoding/json"
	"strings"
	"testing"

	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/a2a/transcript"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
)

// conversation is a stored run of three turns: a prompt, an assistant turn that
// thinks and calls a tool, the result of that call, and the answer.
func conversation() *runstate.RunState {
	return &runstate.RunState{
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{
				{Text: &llm.TextBlock{Text: "how many streams are there"}},
			}},
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
				{Thinking: &llm.ThinkingBlock{Text: "list them first", Signature: []byte("provider-payload")}},
				{ToolUse: &llm.ToolUseBlock{ID: "toolu_1", Name: "stream_ls", Input: json.RawMessage(`{"all":true}`)}},
			}},
			{Role: llm.RoleUser, Content: []llm.ContentBlock{
				{ToolResult: &llm.ToolResultBlock{ToolUseID: "toolu_1", Content: "ORDERS\nEVENTS\nJOBS"}},
			}},
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
				{Text: &llm.TextBlock{Text: "there are three streams"}},
			}},
		},
	}
}

func TestOf_RendersTheConversationAsBlocks(t *testing.T) {
	g := NewWithT(t)

	turns := transcript.Of(conversation())
	g.Expect(turns).To(HaveLen(3), "the results join the turn that called them")

	blocks := turns.Blocks()
	g.Expect(blocks).To(HaveLen(5))

	types := make([]a2a.BlockType, len(blocks))
	for i, b := range blocks {
		types[i] = b.Type()
	}
	g.Expect(types).To(Equal([]a2a.BlockType{
		a2a.BlockPrompt, a2a.BlockThinking, a2a.BlockToolCall, a2a.BlockToolResult, a2a.BlockText,
	}))

	g.Expect(blocks[0].Content().(a2a.PromptBlock).Text).To(Equal("how many streams are there"))
	call := blocks[2].Content().(a2a.ToolCallBlock)
	g.Expect(call.Name).To(Equal("stream_ls"))
	g.Expect(string(call.Input)).To(Equal(`{"all":true}`))
	g.Expect(blocks[3].Content().(a2a.ToolResultBlock).Output).To(Equal("ORDERS\nEVENTS\nJOBS"))
}

// A run dispatches its tools one at a time and sends each result behind the call it
// answers; a journal stores every call together and every result together. A replay
// that walked the journal in stored order would render an order no run produced, and a
// client cannot pair them itself without knowing what a turn was.
func TestOf_PutsEachResultBehindItsCall(t *testing.T) {
	g := NewWithT(t)

	rs := &runstate.RunState{
		Messages: []llm.Message{
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
				{ToolUse: &llm.ToolUseBlock{ID: "toolu_1", Name: "first"}},
				{ToolUse: &llm.ToolUseBlock{ID: "toolu_2", Name: "second"}},
			}},
			// Stored out of call order, which is what a batch that finished out of order
			// leaves behind.
			{Role: llm.RoleUser, Content: []llm.ContentBlock{
				{ToolResult: &llm.ToolResultBlock{ToolUseID: "toolu_2", Content: "second answered"}},
				{ToolResult: &llm.ToolResultBlock{ToolUseID: "toolu_1", Content: "first answered"}},
			}},
		},
	}

	blocks := transcript.Of(rs).Blocks()
	g.Expect(blocks).To(HaveLen(4))

	g.Expect(blocks[0].Content().(a2a.ToolCallBlock).Name).To(Equal("first"))
	g.Expect(blocks[1].Content().(a2a.ToolResultBlock).Output).To(Equal("first answered"))
	g.Expect(blocks[2].Content().(a2a.ToolCallBlock).Name).To(Equal("second"))
	g.Expect(blocks[3].Content().(a2a.ToolResultBlock).Output).To(Equal("second answered"))
}

// A follow-up typed while the previous turn's results were still being appended folds
// into one stored message. The results belong to the turn that called them and the
// prompt is the next turn, so the two halves separate.
func TestOf_SeparatesAFollowUpFoldedInWithResults(t *testing.T) {
	g := NewWithT(t)

	rs := &runstate.RunState{
		Messages: []llm.Message{
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
				{ToolUse: &llm.ToolUseBlock{ID: "toolu_1", Name: "first"}},
			}},
			{Role: llm.RoleUser, Content: []llm.ContentBlock{
				{ToolResult: &llm.ToolResultBlock{ToolUseID: "toolu_1", Content: "answered"}},
				{Text: &llm.TextBlock{Text: "actually, never mind"}},
			}},
		},
	}

	turns := transcript.Of(rs)
	g.Expect(turns).To(HaveLen(2))
	g.Expect(turns[0]).To(HaveLen(2))
	g.Expect(turns[1]).To(HaveLen(1))
	g.Expect(turns[1][0].Content().(a2a.PromptBlock).Text).To(Equal("actually, never mind"))
}

// A live run marks the text of the message that ended a turn, so a client can tell the
// answer from the narration on the way to it. A replay that marked nothing would render
// every answer as narration.
func TestOf_MarksTheAnswerOfEachTurn(t *testing.T) {
	g := NewWithT(t)

	blocks := transcript.Of(conversation()).Blocks()
	g.Expect(blocks[4].Content().(a2a.TextBlock).Final).To(BeTrue())

	// A turn that called a tool meant to continue, so its text is narration however the
	// journal stored the results.
	rs := &runstate.RunState{
		Messages: []llm.Message{
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
				{Text: &llm.TextBlock{Text: "let me look"}},
				{ToolUse: &llm.ToolUseBlock{ID: "toolu_1", Name: "first"}},
			}},
		},
	}

	narration := transcript.Of(rs).Blocks()
	g.Expect(narration[0].Content().(a2a.TextBlock).Final).To(BeFalse())
}

// A thinking signature is the provider payload that lets a turn be replayed to the
// provider that produced it. A live run never sends one and neither does a replay: no
// reader can do anything with the bytes, and they leave the process that holds them.
func TestOf_DropsTheThinkingSignature(t *testing.T) {
	g := NewWithT(t)

	blocks := transcript.Of(conversation()).Blocks()
	thinking := blocks[1].Content().(a2a.ThinkingBlock)

	g.Expect(thinking.Text).To(Equal("list them first"))
	g.Expect(thinking.Signature).To(BeEmpty())
	g.Expect(thinking.Provider).To(BeEmpty())
}

// A journal holds whatever a tool returned, and a block has a size cap. Trimming here
// is what stops a replayed result exceeding the message cap, where it would be dropped
// without advancing the sequence and leave the caller a hole it cannot see.
func TestOf_TrimsWhatWillNotFit(t *testing.T) {
	g := NewWithT(t)

	rs := conversation()
	huge := strings.Repeat("x", a2a.MaxBlockText*2)
	rs.Messages[2].Content[0].ToolResult.Content = huge

	blocks := transcript.Of(rs).Blocks()
	output := blocks[3].Content().(a2a.ToolResultBlock).Output

	g.Expect(output).To(Equal(a2a.TrimBlockText(huge)))
	g.Expect(len(output)).To(BeNumerically("<", len(huge)))
}

// The turn a run left unfinished is what a resume continues, so it belongs at the end
// of the transcript rather than being the one turn a person cannot see.
func TestOf_IncludesTheUnfinishedTurn(t *testing.T) {
	g := NewWithT(t)

	rs := conversation()
	rs.Pending = &runstate.PendingTurn{
		Assistant: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{ToolUse: &llm.ToolUseBlock{ID: "toolu_2", Name: "stream_rm", Input: json.RawMessage(`{"stream":"ORDERS"}`)}},
		}},
	}

	turns := transcript.Of(rs)
	last := turns[len(turns)-1]

	g.Expect(last).To(HaveLen(1))
	g.Expect(last[0].Content().(a2a.ToolCallBlock).ID).To(Equal("toolu_2"))
}

// A caller asking for the last so many blocks gets whole turns, because a result
// without the call it answers cannot be rendered as anything.
func TestTail_RoundsOutToWholeTurns(t *testing.T) {
	g := NewWithT(t)

	turns := transcript.Of(conversation())

	blocks, truncated := turns.Tail(1)
	g.Expect(truncated).To(BeTrue())
	g.Expect(blocks).To(HaveLen(1), "the last turn is the answer")
	g.Expect(blocks[0].Type()).To(Equal(a2a.BlockText))

	// One block short of a turn boundary takes the whole turn it lands in, and the turn
	// holding a result holds the call it answers, so a tail never opens on a result.
	blocks, truncated = turns.Tail(2)
	g.Expect(truncated).To(BeTrue())
	g.Expect(blocks).To(HaveLen(4))
	g.Expect(blocks[0].Type()).To(Equal(a2a.BlockThinking))

	blocks, truncated = turns.Tail(3)
	g.Expect(truncated).To(BeTrue())
	g.Expect(blocks).To(HaveLen(4))

	blocks, truncated = turns.Tail(99)
	g.Expect(truncated).To(BeFalse())
	g.Expect(blocks).To(HaveLen(5))

	blocks, truncated = turns.Tail(0)
	g.Expect(blocks).To(BeEmpty())
	g.Expect(truncated).To(BeTrue(), "a caller that asked for nothing is still told there is history")

	blocks, truncated = turns.Tail(-1)
	g.Expect(blocks).To(HaveLen(5))
	g.Expect(truncated).To(BeFalse())
}

func TestOf_ToleratesAnEmptyRun(t *testing.T) {
	g := NewWithT(t)

	g.Expect(transcript.Of(nil)).To(BeEmpty())
	g.Expect(transcript.Of(&runstate.RunState{}).Blocks()).To(BeEmpty())

	blocks, truncated := transcript.Of(nil).Tail(10)
	g.Expect(blocks).To(BeEmpty())
	g.Expect(truncated).To(BeFalse())
}
