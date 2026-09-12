//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// Package transcript renders a stored run as the blocks a live one produces, so that
// a conversation read back from a journal and a conversation watched as it happens
// reach a renderer in the same shape.
//
// It exists because a run has three audiences and had three renderings: the terminal
// watching it, the caller streaming it, and whoever opens the journal afterwards. Each
// derived its own lines from whatever it held, so the three disagreed. One shape, one
// renderer, and what differs between them is only what each one knows.
//
// It maps runstate onto a2a and depends on both, which is why it is here rather than
// in either: runstate does not import a2a, and a client that renders blocks should not
// have to link the storage layer to read one.
package transcript

import (
	"bytes"
	"encoding/json"
	"time"

	wire "github.com/choria-io/fisk-ai/internal/a2a/wire/v1"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
)

// Turns is a stored conversation as blocks, grouped by the message that produced
// them and in the order a live run would have sent them.
//
// The grouping is what makes a partial replay coherent: a caller asking for the last
// so many blocks gets whole turns, so a result never arrives without the call it
// answers.
type Turns [][]wire.Block

// Blocks is every block of the conversation, oldest first.
func (t Turns) Blocks() []wire.Block {
	var out []wire.Block
	for _, turn := range t {
		out = append(out, turn...)
	}

	return out
}

// Tail is the most recent blocks of the conversation, at least n of them where the
// conversation holds that many, rounded outwards so that no turn is cut in half. It
// reports whether anything older was left behind.
//
// An n of zero returns nothing, which is what a caller that asked for no history
// gets. A negative n returns everything.
func (t Turns) Tail(n int) (blocks []wire.Block, truncated bool) {
	if n == 0 {
		return nil, len(t) > 0
	}
	if n < 0 {
		return t.Blocks(), false
	}

	first := len(t)
	held := 0
	for first > 0 && held < n {
		first--
		held += len(t[first])
	}

	return Turns(t[first:]).Blocks(), first > 0
}

// Of renders a folded run as turns of blocks.
//
// What it leaves behind is everything a journal holds that describes the run rather
// than the conversation: the fingerprint, the caller, the conversation token, the
// approvals, and the deferral notes a tool wrote. A thinking block keeps its text and
// loses its signature, which is the provider payload a live run also never sends and
// which no reader can do anything with.
//
// Text is trimmed to what one block carries, so a replayed tool result that a live one
// would have trimmed is trimmed the same way rather than exceeding the message cap and
// being dropped.
//
// Each block carries the time of the record it came from, and carries none where that
// record predates runstate.Record.Time.
//
// A nil run returns nil, which is the same answer a run holding no messages gives: a
// caller that looked for a stored run and found none renders an empty conversation
// rather than checking first.
func Of(rs *runstate.RunState) Turns {
	if rs == nil {
		return nil
	}

	var turns Turns

	for i := 0; i < len(rs.Messages); i++ {
		msg := rs.Messages[i]

		if msg.Role != llm.RoleAssistant {
			turns = turns.add(blocksOf(msg, messageTime(rs, i), rs.ResultTimes))

			continue
		}

		turn := blocksOf(msg, messageTime(rs, i), rs.ResultTimes)

		// A journal stores every call of a turn in the assistant message and every
		// result in the message after it, while a run dispatches its tools one at a
		// time and sends each result behind the call it answers. So the results are
		// lifted into the turn holding their calls: without that a replayed turn reads
		// in an order no run ever produced, and a tail rounded to a turn boundary could
		// open on a result whose call was left behind.
		if i+1 < len(rs.Messages) {
			results, rest := resultsOf(rs.Messages[i+1], messageTime(rs, i+1), rs.ResultTimes)
			if len(results) > 0 {
				i++
				turns = turns.add(interleave(turn, results))
				// Whatever else that message held is a turn of its own, which is a
				// follow-up typed while the results were still being appended.
				turns = turns.add(rest)

				continue
			}
		}

		// A turn that called no tool ended on its answer, which is what a live run marks
		// as the terminal message and what a client sets the answer apart by.
		turns = turns.add(markFinal(turn))
	}

	// The turn a run left unfinished is held apart from the conversation, and it is
	// the one a resume is about to continue, so it is the last thing a person needs
	// to see rather than the one thing missing. It is not marked final: it is the turn
	// that did not finish.
	if rs.Pending != nil {
		results := make([]toolResult, 0, len(rs.Pending.Results))
		for _, res := range rs.Pending.Results {
			results = append(results, toolResult{call: res.ToolUseID, block: resultBlock(res, rs.ResultTimes[res.ToolUseID])})
		}

		turns = turns.add(interleave(blocksOf(rs.Pending.Assistant, rs.Pending.Time, rs.ResultTimes), results))
	}

	return turns
}

// messageTime is when the message at i was journaled, zero where the run carries no time
// for it. RunState.Times is index aligned with Messages where Fold built it, and a
// RunState assembled by hand may carry none at all.
func messageTime(rs *runstate.RunState, i int) time.Time {
	if i >= len(rs.Times) {
		return time.Time{}
	}

	return rs.Times[i]
}

// add appends a turn, dropping an empty one so a message that rendered nothing does
// not become a turn a tail has to count.
func (t Turns) add(turn []wire.Block) Turns {
	if len(turn) == 0 {
		return t
	}

	return append(t, turn)
}

// toolResult is one stored result and the call it answers.
type toolResult struct {
	call  string
	block wire.Block
}

// resultsOf splits a stored message into the tool results it carries and everything
// else it holds, so the results can join the turn that called them.
//
// at is when this message was journaled and dates what is left over, which is a
// follow-up somebody typed while the results were still being appended. Each result is
// dated from resultTimes, since a batch is one message and each result is its own record.
func resultsOf(msg llm.Message, at time.Time, resultTimes map[string]time.Time) ([]toolResult, []wire.Block) {
	var (
		results []toolResult
		rest    llm.Message
	)

	rest.Role = msg.Role
	for _, block := range msg.Content {
		if block.ToolResult == nil {
			rest.Content = append(rest.Content, block)

			continue
		}

		id := block.ToolResult.ToolUseID
		results = append(results, toolResult{call: id, block: resultBlock(*block.ToolResult, resultTimes[id])})
	}

	return results, blocksOf(rest, at, resultTimes)
}

// interleave puts each result behind the call it answers.
//
// A result whose call is not in this turn is kept at the end rather than dropped: the
// journal is the record, and losing one would hide a call that was answered.
func interleave(turn []wire.Block, results []toolResult) []wire.Block {
	if len(results) == 0 {
		return turn
	}

	placed := make([]bool, len(results))
	out := make([]wire.Block, 0, len(turn)+len(results))

	for _, block := range turn {
		out = append(out, block)

		call, ok := block.Content().(wire.ToolCallBlock)
		if !ok {
			continue
		}

		for i, res := range results {
			if placed[i] || res.call != call.ID {
				continue
			}

			placed[i] = true
			out = append(out, res.block)
		}
	}

	for i, res := range results {
		if !placed[i] {
			out = append(out, res.block)
		}
	}

	return out
}

// markFinal marks the turn's last text as the answer it ended on. A turn holding no
// text is returned as it stands, which is a turn that only reasoned.
//
// A turn that called a tool is left alone however its results were stored. The model
// meant to continue, so its text is narration on the way to an answer rather than the
// answer, which is the distinction a live run's terminal flag draws.
//
// The flag is set on the block that was found rather than a new block being built from
// its text. That keeps the time the block was journaled at, and it keeps the Trimmed
// flag blocksOf set on text it cut.
func markFinal(turn []wire.Block) []wire.Block {
	for _, block := range turn {
		if _, ok := block.Content().(wire.ToolCallBlock); ok {
			return turn
		}
	}

	for i := len(turn) - 1; i >= 0; i-- {
		text, ok := turn[i].Content().(wire.TextBlock)
		if !ok {
			continue
		}

		text.Final = true
		turn[i] = wire.NewBlock(text)

		return turn
	}

	return turn
}

// blocksOf renders one stored message. A user message is a prompt, tool results, or
// both: a follow-up typed while the previous turn's results were still being appended
// folds into one message, and each half is rendered as what it is.
//
// at is when the record carrying this message was journaled and dates every block it
// produces, zero for a message the journal holds no time for. A tool result is dated
// from resultTimes instead, each result being its own record.
func blocksOf(msg llm.Message, at time.Time, resultTimes map[string]time.Time) []wire.Block {
	var out []wire.Block

	for _, block := range msg.Content {
		switch {
		case block.Text != nil:
			if block.Text.Text == "" {
				continue
			}
			if msg.Role == llm.RoleUser {
				out = append(out, wire.NewBlock(wire.PromptBlock{Text: wire.TrimBlockText(block.Text.Text), Time: at}))

				continue
			}

			text, cut := wire.TrimmedBlockText(block.Text.Text)
			out = append(out, wire.NewBlock(wire.TextBlock{Text: text, Trimmed: cut, Time: at}))

		case block.Thinking != nil:
			if block.Thinking.Text == "" {
				continue
			}

			text, cut := wire.TrimmedBlockText(block.Thinking.Text)
			out = append(out, wire.NewBlock(wire.ThinkingBlock{Text: text, Trimmed: cut, Time: at}))

		case block.ToolUse != nil:
			out = append(out, wire.NewBlock(wire.ToolCallBlock{
				ID:    block.ToolUse.ID,
				Name:  block.ToolUse.Name,
				Input: objectInput(block.ToolUse.Input),
				Time:  at,
			}))

		case block.ToolResult != nil:
			out = append(out, resultBlock(*block.ToolResult, resultTimes[block.ToolResult.ToolUseID]))
		}
	}

	return out
}

// resultBlock renders one tool result, dated at when the journal holds its time.
func resultBlock(res llm.ToolResultBlock, at time.Time) wire.Block {
	return wire.NewBlock(wire.ToolResultBlock{
		CallID: res.ToolUseID,
		Time:   at,
		ToolResult: wire.ToolResult{
			Output:  wire.TrimBlockText(res.Content),
			IsError: res.IsError,
		},
	})
}

// objectInput carries a call's arguments only when they are a JSON object, which is
// what the schema types the field as. A journal written by a provider that sent
// anything else costs the caller that block's arguments rather than the whole message,
// which validating a replayed block against the schema would otherwise do.
func objectInput(input json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(input)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return nil
	}

	return trimmed
}
