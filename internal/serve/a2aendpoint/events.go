//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2aendpoint

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"

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
func (e *eventSink) Message(resp llm.Response, terminal bool) {
	for _, block := range resp.Content {
		switch {
		case block.Text != nil:
			// The text of the terminal turn is the answer and travels again in the
			// result. Only the run knows which message ended it, so it says: without
			// this a caller cannot tell the answer from the narration on the way to it,
			// and renders it twice.
			if terminal {
				e.send(a2a.NewFinalTextBlock(trimForWire(block.Text.Text)))

				continue
			}

			e.send(a2a.NewTextBlock(trimForWire(block.Text.Text)))
		case block.Thinking != nil:
			// The signature stays local. It is the opaque payload that lets a turn be
			// replayed to the provider that produced it, it is never replayed across an
			// agent boundary, and no peer can do anything with the bytes.
			e.send(a2a.NewBlock(a2a.ThinkingBlock{Text: trimForWire(block.Thinking.Text)}))
		}
	}

	if terminal {
		return
	}

	e.iteration++
	e.send(a2a.NewBlock(a2a.StatusBlock{Iteration: e.iteration, Usage: callUsage(resp.Usage)}))
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
