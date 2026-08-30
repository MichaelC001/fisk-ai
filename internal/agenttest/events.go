//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agenttest

import (
	"sync"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/llm"
)

// RecordedMessage pairs an assistant turn with whether the loop reported it as the
// terminal answer, so a spec can distinguish the final message from intermediate
// narration.
type RecordedMessage struct {
	Response llm.Response
	Terminal bool
}

// RecordingEvents is an agent.Events that records every event for later assertion.
// It is safe for concurrent use, so a single instance can aggregate several runs at
// once (the N-concurrent-runs acceptance test); accessors return copies so a caller
// never races the run goroutines still writing.
//
// It implements none of the four optional halves of agent.Events, so it is the fixture
// for a sink that opts into nothing, and an external test in the agent package holds it
// to that. RecordingStreamEvents is the fixture for a sink that opts into the streaming
// half and receives an assistant turn in fragments.
type RecordingEvents struct {
	mu          sync.Mutex
	warnings    []agent.Warning
	messages    []RecordedMessage
	starts      []agent.RunInfo
	toolCalls   []agent.ToolTrace
	toolResults []agent.ToolResultTrace
	rotated     []string
	panics      []RecordedPanic
}

// RecordedPanic is a crash reported through Panicked: the recovered value and the
// captured goroutine stack.
type RecordedPanic struct {
	Value any
	Stack []byte
}

// NewRecordingEvents returns an empty recorder.
func NewRecordingEvents() *RecordingEvents { return &RecordingEvents{} }

func (e *RecordingEvents) Warn(w agent.Warning) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.warnings = append(e.warnings, w)
}

func (e *RecordingEvents) Starting(info agent.RunInfo) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.starts = append(e.starts, info)
}

func (e *RecordingEvents) Message(resp llm.Response, terminal bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.messages = append(e.messages, RecordedMessage{Response: resp, Terminal: terminal})
}

func (e *RecordingEvents) ToolCall(t agent.ToolTrace) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.toolCalls = append(e.toolCalls, t)
}

func (e *RecordingEvents) ToolResult(t agent.ToolResultTrace) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.toolResults = append(e.toolResults, t)
}

func (e *RecordingEvents) SessionRotated(prevID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rotated = append(e.rotated, prevID)
}

func (e *RecordingEvents) Panicked(value any, stack []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.panics = append(e.panics, RecordedPanic{Value: value, Stack: stack})
}

// Panics returns a copy of the crashes reported through Panicked.
func (e *RecordingEvents) Panics() []RecordedPanic {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]RecordedPanic, len(e.panics))
	copy(out, e.panics)
	return out
}

// LLMRequest carries nothing a spec asserts on today, so it is accepted and dropped
// rather than recorded.
func (e *RecordingEvents) LLMRequest(string) {}

// Warnings returns a copy of the warnings emitted, in order.
func (e *RecordingEvents) Warnings() []agent.Warning {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]agent.Warning, len(e.warnings))
	copy(out, e.warnings)
	return out
}

// Starts returns a copy of the run parameters reported at start, in order. A run
// reports once; a spec that drives several runs through one recorder reads them in
// the order the runs happened.
func (e *RecordingEvents) Starts() []agent.RunInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]agent.RunInfo, len(e.starts))
	copy(out, e.starts)
	return out
}

// HasWarning reports whether a warning of the given kind was emitted.
func (e *RecordingEvents) HasWarning(kind agent.WarningKind) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, w := range e.warnings {
		if w.Kind == kind {
			return true
		}
	}
	return false
}

// Messages returns a copy of the assistant turns recorded, in order.
func (e *RecordingEvents) Messages() []RecordedMessage {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]RecordedMessage, len(e.messages))
	copy(out, e.messages)
	return out
}

// FinalMessage returns the terminal assistant turn and true, or a zero message and
// false when no terminal turn was recorded.
func (e *RecordingEvents) FinalMessage() (llm.Response, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := len(e.messages) - 1; i >= 0; i-- {
		if e.messages[i].Terminal {
			return e.messages[i].Response, true
		}
	}
	return llm.Response{}, false
}

// ToolCalls returns a copy of the tool call traces recorded, in order.
func (e *RecordingEvents) ToolCalls() []agent.ToolTrace {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]agent.ToolTrace, len(e.toolCalls))
	copy(out, e.toolCalls)
	return out
}

// ToolResults returns a copy of the tool result traces recorded, in order.
func (e *RecordingEvents) ToolResults() []agent.ToolResultTrace {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]agent.ToolResultTrace, len(e.toolResults))
	copy(out, e.toolResults)
	return out
}

// RecordedCall is one model call a RecordingStreamEvents saw: the fragments it was sent
// and what they reconcile to against the assembled turn.
type RecordedCall struct {
	// Deltas are this call's fragments, in the order the provider produced them.
	Deltas []llm.Delta
	// Blocks is what each content block index of the call assembles to, ordered by index,
	// with the source saying which copy of the text that is. A tool call has no delta
	// kind and streams no fragments, so its index is absent.
	Blocks []llm.AssembledBlock
	// Terminal is what Message reported for the call. True means the turn is the answer
	// the run ended on.
	Terminal bool
}

// RecordingStreamEvents is a RecordingEvents that also implements
// agent.MessageStreamer, so a delta sink has a recorder to assert against: it holds the
// fragments a run reported and the text those fragments reconcile to.
//
// It reconciles through llm.DeltaAssembler. The rule for a whole block that diverges from
// the fragments of its index has one implementation, and a buffer here would be a second
// copy of it in a test package.
//
// StreamDeltas starts a model call and Message ends one, which is how the runner drives a
// sink: it asks once per call, before the call. A test driving this recorder by hand calls
// StreamDeltas for each call the same way; without it a later call's fragments join the
// blocks of an earlier one. A call the run abandoned between the provider returning and
// Message has its fragments dropped at the next call rather than recorded.
//
// Use one per run. Its methods lock, but the fragments, the assembler and the call it is
// building carry no run identity, so two runs sharing an instance interleave into one
// call. The embedded recorder aggregates several runs; this half does not.
type RecordingStreamEvents struct {
	*RecordingEvents

	stream bool

	mu        sync.Mutex
	asked     int
	deltas    []llm.Delta
	pending   []llm.Delta
	calls     []RecordedCall
	assembler llm.DeltaAssembler
}

// NewRecordingStreamEvents returns an empty recorder that answers StreamDeltas with
// stream.
func NewRecordingStreamEvents(stream bool) *RecordingStreamEvents {
	return &RecordingStreamEvents{RecordingEvents: NewRecordingEvents(), stream: stream}
}

// StreamDeltas answers what the recorder was constructed with and starts a model call,
// dropping whatever a call that never reached Message left behind.
func (e *RecordingStreamEvents) StreamDeltas() bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.asked++
	e.pending = nil
	e.assembler.Reset()

	return e.stream
}

// MessageDelta records one fragment and adds it to the call's assembler.
func (e *RecordingStreamEvents) MessageDelta(d llm.Delta) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.deltas = append(e.deltas, d)
	e.pending = append(e.pending, d)
	e.assembler.AddDelta(d)
}

// AddBlock reconciles one whole block against the fragments its index holds and returns
// what the index now assembles to.
//
// Message calls it for every text and thinking block of a turn with Trimmed false, since
// a provider cuts nothing. A producer between a run and a sink does cut, a2a.TrimBlockText
// at a2a.MaxBlockText, so a test for a sink on that path calls this directly with Trimmed
// set and gets SourceKeptFragments back.
func (e *RecordingStreamEvents) AddBlock(w llm.WholeBlock) llm.AssembledBlock {
	e.mu.Lock()
	defer e.mu.Unlock()

	return e.assembler.AddBlock(w)
}

// Message reconciles the turn's text and thinking blocks against the fragments of the
// call, records the call, and ends it. The turn also reaches the embedded recorder, so
// Messages and FinalMessage report it as they do for a sink that streams nothing.
func (e *RecordingStreamEvents) Message(resp llm.Response, terminal bool) {
	e.mu.Lock()

	for i, block := range resp.Content {
		switch {
		case block.Text != nil:
			e.assembler.AddBlock(llm.WholeBlock{Kind: llm.DeltaText, Index: i, Text: block.Text.Text})
		case block.Thinking != nil:
			e.assembler.AddBlock(llm.WholeBlock{Kind: llm.DeltaThinking, Index: i, Text: block.Thinking.Text})
		}
	}

	e.calls = append(e.calls, RecordedCall{Deltas: e.pending, Blocks: e.assembler.Blocks(), Terminal: terminal})
	e.pending = nil

	e.mu.Unlock()

	e.RecordingEvents.Message(resp, terminal)
}

// Deltas returns a copy of every fragment the recorder was sent across the run, in order,
// including those of a call that ended without a Message.
func (e *RecordingStreamEvents) Deltas() []llm.Delta {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]llm.Delta, len(e.deltas))
	copy(out, e.deltas)
	return out
}

// Calls returns a copy of the model calls that reached Message, in order. A call the run
// abandoned before Message is not among them and its fragments are in Deltas alone.
func (e *RecordingStreamEvents) Calls() []RecordedCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]RecordedCall, len(e.calls))
	copy(out, e.calls)
	return out
}

// Assembled returns what the call being written assembles to, or the last call to end,
// ordered by index. Read it mid-call to see what a sink rendering a live turn would show.
func (e *RecordingStreamEvents) Assembled() []llm.AssembledBlock {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.assembler.Blocks()
}

// Asked returns how many times StreamDeltas was called. The runner calls it once per
// model call, whatever the answer.
func (e *RecordingStreamEvents) Asked() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.asked
}
