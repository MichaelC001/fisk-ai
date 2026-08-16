//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2asurface

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/remotetools"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/toolkit/fisk"
)

// maxWireText bounds the text of one message on the reply set.
//
// A tool's output is the one unbounded thing on this path, and a ReplyStream refuses a
// message over the size cap without advancing the sequence, so an event dropped for
// size would leave no gap for a caller to notice. Trimming keeps the event and says
// what happened to the rest. The journal on this worker holds all of it.
//
// It is a constant rather than a setting: it exists so a message fits, not so an
// operator can choose how much of a run a caller sees.
const maxWireText = 64 * 1024

// trimMarker closes a value that was cut, so a caller renders a truncation rather than
// an answer that stops mid-sentence.
const trimMarker = "\n[trimmed for the event stream; the full text is in this worker's run journal]"

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

// Warn is not sent. No block type carries an advisory, so the run's warnings reach this
// worker's log and stop there, including the one explaining why every confirmation-gated
// tool was refused.
func (e *eventSink) Warn(agent.Warning) {}

// Panicked is not sent. The stack leaks absolute paths, module layout and frame
// arguments, and it reaches this sink and the worker's log and nowhere else; the caller
// is told the run crashed by the terminal message.
func (e *eventSink) Panicked(any, []byte) {}

// The rest describe a run to something rendering it locally and have no counterpart on
// the wire: the resolved run parameters, the remote import notes, the transcript replay
// of a resumed run, the verbose request summary, and a context reset naming the session
// it left behind.
func (e *eventSink) Starting(agent.RunInfo)                                                {}
func (e *eventSink) RemoteHostNotes([]remotetools.HostImport)                              {}
func (e *eventSink) ResumeTranscript(*runstate.RunState, map[string]*fisk.FiskCommandTool) {}
func (e *eventSink) LLMRequest(string)                                                     {}
func (e *eventSink) SessionRotated(string)                                                 {}

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

// trimForWire cuts a value to what one message can carry. It cuts on a rune boundary,
// since half a rune reaches a caller as a replacement character in the middle of an
// answer.
func trimForWire(s string) string {
	if len(s) <= maxWireText {
		return s
	}

	cut := maxWireText
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}

	return s[:cut] + trimMarker
}

// utf8Start reports whether b begins a rune rather than continuing one.
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
