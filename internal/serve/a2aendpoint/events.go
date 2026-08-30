//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2aendpoint

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"time"
	"unicode/utf8"

	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/a2a/transcript"
	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/remotetools"
	"github.com/choria-io/fisk-ai/internal/runstate"
)

// maxReplayBlocks bounds a replay however much a caller asks for. A conversation has
// no size limit and a replay is sent as fast as the transport takes it, so this is what
// stops one turn's resume from being thousands of messages. A caller is told when older
// blocks were left behind and can read the rest from the store, where the journal is.
const maxReplayBlocks = 200

// maxDeltaText is the most fragment text one delta message carries, and the size the
// coalescing buffer flushes at.
//
// ReplyStream refuses a message larger than a2a.MaxMessageSize and does not advance the
// sequence when it does, so an oversized delta is dropped and the caller sees no gap
// where it was. The text it assembles is then wrong and nothing says so. A whole block
// avoids that by trimming, which the reader can see; a trimmed fragment corrupts the
// text instead.
//
// The value is a fraction of the limit so the two move together. A sixty-fourth leaves
// room for the message header and for JSON escaping, which can cost six bytes for one
// byte of text.
const maxDeltaText = a2a.MaxMessageSize / 64

// deltaFlushWindow is how long buffered fragment text waits for more before it is sent.
//
// Without it a provider writing a word at a time would put one message on the wire per
// word. The window is read when a fragment arrives, never on a timer, so it delays a
// fragment only when another follows it.
const deltaFlushWindow = 100 * time.Millisecond

// maxDeltaBlocks caps how many content blocks the sink buffers fragments for at once.
//
// A backend checks a fragment's index against the blocks it has started, so a provider
// bounds its own indexes. This sink sees only fragments, and without a cap it would hold
// a buffer for every index a broken or hostile upstream sent, for the length of the call.
// No model call writes 32 blocks of text and reasoning.
const maxDeltaBlocks = 32

// The runner asserts this half at runtime, so a sink that stopped satisfying it would
// stop streaming with no error raised anywhere.
var _ agent.MessageStreamer = (*eventSink)(nil)

// eventSink turns a run's narration into blocks on the task's reply set.
//
// Every method runs on the run goroutine in loop order, so it needs no locking of its
// own. A send that fails is logged and the run continues: events are advisory and the
// answer travels in the terminal message, so a caller that missed one has lost less
// than a run stopped over it.
type eventSink struct {
	stream    *a2a.ReplyStream
	log       *slog.Logger
	iteration int

	// replay is how many blocks of the stored conversation this caller asked to be
	// sent before the run's own events. Zero, which is what a caller that asked for
	// nothing has, makes the sink no replayer at all.
	replay int

	// deltas is whether this caller asked for the fragments of an assistant turn as the
	// model writes it. It is set from the task request, and StreamDeltas reports it to
	// the runner.
	deltas bool
	// buffered coalesces fragments per content block index, so the sink paces what
	// reaches the wire instead of sending a message per fragment.
	buffered map[int]*deltaBuffer
	// bufferedCall is the model call buffered holds the fragments of. Index restarts at
	// 0 on every call, so without this a block left open by one call would collect the
	// next call's first fragment.
	bufferedCall int
	// cappedLogged is whether this call has already logged hitting maxDeltaBlocks, so an
	// upstream inventing an index per fragment is logged once and not per fragment.
	cappedLogged bool
}

// deltaBuffer is the unsent fragment text of one content block.
type deltaBuffer struct {
	kind llm.DeltaKind
	// pending is the text that has arrived and not yet been sent.
	pending []byte
	// since is when the oldest byte in pending arrived. deltaFlushWindow is measured
	// from it.
	since time.Time
}

// ResumeTranscript sends the stored conversation a resume is continuing, so a caller
// that was not there for the earlier turns has them before the new ones arrive.
//
// It is bracketed by a status block at each end, since a client renders what already
// happened differently from what is happening, and the closing one says how much was
// sent and whether the conversation began before it. The count is the caller's, capped
// here and rounded outwards to a turn so that a result never arrives without its call.
func (e *eventSink) ResumeTranscript(rs *runstate.RunState) {
	if e.replay == 0 {
		return
	}

	want := min(e.replay, maxReplayBlocks)

	blocks, truncated := transcript.Of(rs).Tail(want)
	if len(blocks) == 0 {
		return
	}

	// The closing block carries what the conversation has consumed so far, which is the
	// one number a caller cannot work out from the replay: a client keeping a running
	// total would otherwise start a resumed conversation at zero and jump when the turn
	// ends. It is the whole conversation rather than one call, which is the other
	// meaning this field carries and which the schema separates.
	e.send(a2a.NewBlock(a2a.StatusBlock{Phase: a2a.PhaseReplayStart}))
	for _, block := range blocks {
		e.send(block)
	}
	e.send(a2a.NewBlock(a2a.StatusBlock{
		Phase:     a2a.PhaseReplayEnd,
		Count:     len(blocks),
		Truncated: truncated,
		Usage:     a2a.UsageFromCounters(rs.Counters),
	}))

	e.log.Info("Replayed a stored conversation", "blocks", len(blocks), "asked", e.replay, "truncated", truncated)
}

// Message sends the model's text and its reasoning, then marks the end of an iteration
// with a status block a caller can pace itself against.
//
// Tool-use content is not sent here. ToolCall carries the same call with the tool's own
// description of it, and a caller reading both would render each call twice.
//
// Each block carries its position in the call that produced it, counted over the whole
// turn including the blocks that are not sent, so a caller that asked for fragments
// knows which buffer this block replaces. Counting only the blocks that reach the wire
// would give a different number as soon as a tool call sat between two of them.
func (e *eventSink) Message(resp llm.Response, terminal bool) {
	for index, block := range resp.Content {
		switch {
		case block.Text != nil:
			// The text of the terminal turn is the answer and travels again in the
			// result. Only the run knows which message ended it, so it says: without
			// this a caller cannot tell the answer from the narration on the way to it,
			// and renders it twice.
			text, trimmed := trimmedForWire(block.Text.Text)
			e.send(a2a.NewBlock(a2a.TextBlock{Text: text, Final: terminal, Index: index, Trimmed: trimmed}))
		case block.Thinking != nil:
			// The signature stays local. It is the opaque payload that lets a turn be
			// replayed to the provider that produced it, it is never replayed across an
			// agent boundary, and no peer can do anything with the bytes.
			text, trimmed := trimmedForWire(block.Thinking.Text)
			e.send(a2a.NewBlock(a2a.ThinkingBlock{Text: text, Index: index, Trimmed: trimmed}))
		}
	}

	if terminal {
		return
	}

	e.iteration++
	e.send(a2a.NewBlock(a2a.StatusBlock{Iteration: e.iteration, Usage: callUsage(resp.Usage)}))
}

// StreamDeltas reports whether this caller asked for the fragments of an assistant turn
// as the model writes it. The value comes from the request's deltas property. The runner
// asks once per model call, and the answer is a field the sink already holds.
func (e *eventSink) StreamDeltas() bool { return e.deltas }

// MessageDelta sends one fragment of the assistant turn being written, coalesced with
// the fragments around it.
//
// A provider emits a fragment every few tokens, so one message each would tie the wire
// cost of a run to the rate the backend writes at. Fragments are buffered per content
// block index and sent when the buffer reaches maxDeltaText or its oldest byte has
// waited deltaFlushWindow, whichever comes first. The Final fragment of a block sends
// what is left along with the mark that closes the block.
//
// The window is read here and never on a timer. ReplyStream is owned by one goroutine at
// a time, its sequence counter is unguarded, and every method of this sink runs on the
// run goroutine, so a timer publishing a partial buffer would race the run's own events
// and corrupt the numbering. Nothing is left behind: a block ends with a Final fragment,
// and a call that wrote nothing buffered nothing.
func (e *eventSink) MessageDelta(d llm.Delta) {
	if !e.deltas {
		return
	}

	if d.Kind != llm.DeltaText && d.Kind != llm.DeltaThinking {
		return
	}

	// e.iteration counts the status blocks Message has sent, so the call being written is
	// the next one. Reading it here keeps the fragments and the status block that ends
	// their call on the same number.
	call := e.iteration + 1
	if call != e.bufferedCall {
		clear(e.buffered)
		e.bufferedCall = call
		e.cappedLogged = false
	}

	buf, held := e.buffered[d.Index]
	if !held {
		if len(e.buffered) >= maxDeltaBlocks {
			if !e.cappedLogged {
				e.log.Warn("Dropping fragments of a model call writing more blocks than the sink buffers", "iteration", call, "blocks", maxDeltaBlocks)
				e.cappedLogged = true
			}

			return
		}

		if e.buffered == nil {
			e.buffered = make(map[int]*deltaBuffer, 4)
		}

		buf = &deltaBuffer{kind: d.Kind, since: time.Now()}
		e.buffered[d.Index] = buf
	}

	buf.pending = append(buf.pending, d.Text...)

	// A fragment larger than the flush cap is split across messages. A caller assembling
	// the text cannot notice a missing one, so what does not fit in this message goes in
	// the next.
	for len(buf.pending) >= maxDeltaText {
		cut := runeCut(buf.pending, maxDeltaText)
		e.sendDelta(d.Index, call, buf.kind, string(buf.pending[:cut]), false)
		buf.pending = buf.pending[cut:]
		buf.since = time.Now()
	}

	if d.Final {
		e.sendDelta(d.Index, call, buf.kind, string(buf.pending), true)
		delete(e.buffered, d.Index)

		return
	}

	if len(buf.pending) == 0 || time.Since(buf.since) < deltaFlushWindow {
		return
	}

	e.sendDelta(d.Index, call, buf.kind, string(buf.pending), false)
	buf.pending = buf.pending[:0]
	buf.since = time.Now()
}

// sendDelta publishes one fragment of the block at index, as the block kind it is a
// fragment of.
func (e *eventSink) sendDelta(index, iteration int, kind llm.DeltaKind, text string, final bool) {
	if kind == llm.DeltaThinking {
		e.send(a2a.NewBlock(a2a.ThinkingDeltaBlock{Index: index, Iteration: iteration, Text: text, Final: final}))

		return
	}

	e.send(a2a.NewBlock(a2a.TextDeltaBlock{Index: index, Iteration: iteration, Text: text, Final: final}))
}

// runeCut is the largest cut of b at or below limit that does not split a rune, since
// half a rune reaches a caller as a replacement character in the middle of an answer.
// Bytes that begin no rune at all are cut at the limit, so text that is not valid UTF-8
// still moves rather than stalling the buffer.
func runeCut(b []byte, limit int) int {
	if len(b) <= limit {
		return len(b)
	}

	cut := limit
	for cut > 0 && !utf8.RuneStart(b[cut]) {
		cut--
	}

	if cut == 0 {
		return limit
	}

	return cut
}

// ToolCall sends what the run is about to do to the world, with the id a result answers
// and the arguments it was dispatched with.
func (e *eventSink) ToolCall(t agent.ToolTrace) {
	e.send(a2a.NewToolCallBlock(t.ID, t.Name, objectInput(t.Input)))
}

// ToolResult sends what a call returned, against the id of the call it answers.
//
// Not every call produces one. A denied confirmation, a call missing required
// arguments, a tool that answers later and an aborted run each end without a result, so
// a caller pairing the two tolerates a call that is never answered.
func (e *eventSink) ToolResult(t agent.ToolResultTrace) {
	e.send(a2a.NewToolResultBlock(t.CallID, trimForWire(t.Output), t.IsError))
}

// Warn sends what the run raised: a tool stopped on its timeout, a tool the model
// called that does not exist, approvals dropped by a forced resume.
//
// The kind and its fields travel rather than a sentence about them, so the wording
// belongs to whatever renders it and a caller that does not know a kind still has
// something to show. The kind's stable name is sent rather than its value, which is a
// position in a list.
func (e *eventSink) Warn(w agent.Warning) {
	block := a2a.WarningBlock{
		Kind:   w.Kind.String(),
		Name:   trimForWire(w.Name),
		Count:  w.Count,
		Params: w.Params,
	}
	if w.Err != nil {
		block.Error = trimForWire(w.Err.Error())
	}

	e.send(a2a.NewBlock(block))
}

// RemoteHostNotes sends what importing a peer's tools had to say about it: a filter
// that was ignored, tools that were skipped, a host that contributed nothing.
//
// They travel as warnings because that is what they are, and because the alternative is
// what happened before: agent.RemoteHostReporter is optional, this sink did not
// implement it, and so a mistyped include left a run quietly short of tools with nobody
// told. A caller cannot see the worker's configuration and is the only one who can act
// on it.
func (e *eventSink) RemoteHostNotes(imports []remotetools.HostImport) {
	for _, imp := range imports {
		for _, w := range agent.HostImportWarnings(imp) {
			e.Warn(w)
		}
	}
}

// Panicked is not sent. The stack leaks absolute paths, module layout and frame
// arguments, and it reaches this sink and the worker's log and nowhere else; the caller
// is told the run crashed by the terminal message.
func (e *eventSink) Panicked(any, []byte) {}

// The rest describe a run to something rendering it locally and have no counterpart on
// the wire: the resolved run parameters, the verbose request summary, and a context
// reset naming the session it left behind. The remote import notes and the transcript
// replay of a resumed run are not here at all, being the optional halves of
// agent.Events that a sink rendering for nobody does not implement.
func (e *eventSink) Starting(agent.RunInfo) {}
func (e *eventSink) LLMRequest(string)      {}
func (e *eventSink) SessionRotated(string)  {}

// send publishes one block, keeping the run going whatever the sink says. A message
// over the size cap is the one failure this can still provoke after trimming, and it is
// logged as the event a caller will not see.
func (e *eventSink) send(block a2a.Block) {
	err := e.stream.Event(block)
	if err == nil {
		return
	}

	if errors.Is(err, a2a.ErrMessageTooLarge) {
		e.log.Warn("Dropping an event that does not fit the size cap", "block", block.Type())

		return
	}

	e.log.Warn("Sending an event failed", "block", block.Type(), "error", err)
}

// callUsage reports what one model call cost. The call counts are left out: this
// describes a single call, and the run's totals travel in the terminal message.
func callUsage(u llm.Usage) *a2a.Usage {
	return &a2a.Usage{
		InputTokens:       u.In + u.CacheRead + u.CacheCreate,
		OutputTokens:      u.Out,
		CacheReadTokens:   u.CacheRead,
		CacheCreateTokens: u.CacheCreate,
		ThinkingTokens:    u.Thinking,
	}
}

// objectInput carries a tool call's arguments only when they are a JSON object, which
// is what the schema types the field as. A provider that sent anything else would cost
// the caller the whole event rather than one field, since a receiver validates every
// message of the set.
func objectInput(input json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(input)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return nil
	}

	return trimmed
}

// trimForWire cuts a value to what one message can carry. It is a2a.TrimBlockText
// under a local name, so this package's call sites read as they did when the bound
// lived here and the adapter that replays a stored conversation trims identically.
func trimForWire(s string) string { return a2a.TrimBlockText(s) }

// trimmedForWire is trimForWire for a block that reports whether it was cut. It compares
// the value that came back with the one that went in, so the limit and the cutting rule
// stay in a2a.TrimBlockText and nothing here restates them.
func trimmedForWire(s string) (string, bool) {
	out := trimForWire(s)

	return out, out != s
}
