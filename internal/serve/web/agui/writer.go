//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agui

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/events"
	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/types"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/serve"
	"github.com/choria-io/fisk-ai/internal/serve/web"
)

// The channel drives the writer through both halves, and the runner asserts the
// streaming one at runtime, so a sink that stopped satisfying either would stop
// streaming with no error raised anywhere.
var (
	_ web.TurnWriter        = (*turnWriter)(nil)
	_ agent.MessageStreamer = (*turnWriter)(nil)
)

// The custom events this format sends. AG-UI has an event for everything a run reports
// except an operator advisory and a message the conversation did not take, and CUSTOM
// carries a name a client branches on.
const (
	customCapabilities   = "fisk.capabilities"
	customWarning        = "fisk.warning"
	customPromptNotTaken = "fisk.prompt_not_taken"
)

// promptNotTakenText is what a page is told when its message did not enter the
// conversation: the turn resumed on an unanswered question, put the question again, and
// suspended without reaching a boundary that takes a user message.
const promptNotTakenText = "your message was not delivered: this conversation is waiting on a question, so answer that first and send the message again"

// emptyOutputText is the result of a call whose tool printed nothing.
const emptyOutputText = "the tool produced no output"

// capabilities is what this agent declares it can do with an interrupt, sent once at
// the head of every run.
//
// AG-UI has no manifest on the run endpoint, so the declaration goes where a client
// reading the stream sees it. ApproveWithEdits is false and stays false: a resumed run
// dispatches the model's original call again and there is no path to substitute the
// arguments a person edited.
type capabilities struct {
	HumanInTheLoop humanInTheLoop `json:"humanInTheLoop"`
}

type humanInTheLoop struct {
	Interrupts       bool `json:"interrupts"`
	ApproveWithEdits bool `json:"approveWithEdits"`
}

// warningValue is one advisory the run raised.
//
// The kind and its fields travel rather than a sentence about them, so the wording
// belongs to whatever renders it and a client that does not know a kind still has
// something to show. The kind's stable name is sent rather than its value, which is a
// position in a list.
type warningValue struct {
	Kind   string   `json:"kind"`
	Name   string   `json:"name,omitempty"`
	Count  int      `json:"count,omitempty"`
	Params []string `json:"params,omitempty"`
	Error  string   `json:"error,omitempty"`
}

// noticeValue is one line this channel says for itself rather than something the run
// produced.
type noticeValue struct {
	Message string `json:"message"`
}

// runResult is what a finished run reports beside its outcome: the terminal reason the
// journal recorded, which tells a conversation that answered from one that stopped
// waiting on something.
type runResult struct {
	Reason string `json:"reason,omitempty"`
}

// turnWriter renders one turn as AG-UI events.
//
// Every method runs on the run's goroutine in loop order, and the ending runs on the
// same goroutine after the run, so nothing here locks.
type turnWriter struct {
	stream *stream
	opened bool

	// threadID and runID are the run frame every event sits in, taken from the request
	// so the client's own ids come back to it.
	threadID string
	runID    string

	// blocks holds the open message of each content block of the model call running
	// now. llm.Delta.Index restarts at zero on every call, so Message clears them, and
	// ids are minted rather than taken from the index because a message id has to be
	// distinct from every other message of the thread.
	blocks map[int]*blockPart
	ids    int

	// interrupts are the questions the turn ended on, carried on the run-finished
	// event. A Fisk run raises one at a time, its prompter running on the loop's own
	// goroutine, and the protocol takes a list.
	interrupts []types.Interrupt

	// resent is the call the run that raised the interrupt already sent, which this
	// run dispatches again. It comes from the request rather than from anything
	// remembered, so any process can serve the run that resumes, and it is spent on
	// the first call that matches.
	resent string
}

// blockPart is one content block's message: whether it is prose or reasoning, the id it
// was started under, and whether the client has been told it ended.
type blockPart struct {
	reasoning bool
	id        string
	open      bool
}

// newTurnWriter holds the response for one turn, written under the thread and run the
// request named. resent is the call the run that raised the interrupt already sent to
// the client, empty on a request that starts a turn.
func newTurnWriter(w http.ResponseWriter, threadID string, runID string, resent string) *turnWriter {
	return &turnWriter{
		stream:   newStream(w),
		threadID: threadID,
		runID:    runID,
		blocks:   map[int]*blockPart{},
		resent:   resent,
	}
}

// Open sends the response head, starts the run and declares what this agent does with
// an interrupt. It is idempotent: the channel calls it before the first event, and
// Close calls it when the outcome arrived before any event did.
func (t *turnWriter) Open() {
	if t.opened {
		return
	}
	t.opened = true

	t.stream.open()
	t.stream.event(events.NewRunStartedEvent(t.threadID, t.runID))
	t.stream.event(events.NewCustomEvent(customCapabilities, events.WithValue(capabilities{
		HumanInTheLoop: humanInTheLoop{Interrupts: true, ApproveWithEdits: false},
	})))
}

// StreamDeltas asks for the fragments of each assistant turn, which is what makes the
// page type rather than wait.
func (t *turnWriter) StreamDeltas() bool { return true }

// MessageDelta renders one fragment on the message its block index is open under,
// starting that message on the index's first fragment and ending it on its last.
func (t *turnWriter) MessageDelta(d llm.Delta) {
	reasoning, drawn := blockKind(d.Kind)
	if !drawn {
		return
	}

	b, held := t.blocks[d.Index]
	if !held {
		b = &blockPart{reasoning: reasoning, id: t.mintID(), open: true}
		t.blocks[d.Index] = b

		t.startBlock(b)
	}

	// A fragment after the index's last one is a backend contradicting itself. The
	// message has been ended and the client would take the text as a new one, so it is
	// dropped and the assembled turn is what the index says.
	if !b.open {
		return
	}

	// A final fragment with nothing left to send carries no text, and a content event
	// with an empty delta is not a valid one, so it is not sent at all.
	if d.Text != "" {
		t.stream.event(contentEvent(b, d.Text))
	}

	if d.Final {
		t.endBlock(b)
	}
}

// Message renders the assistant turn a model call produced and ends that call's
// messages.
//
// A block whose fragments were streamed is ended rather than sent again, which is the
// reconcile rule llm.DeltaAssembler holds: the whole block replaces the fragments of
// its index, and the client already has that text. A block that streamed nothing is
// written whole, which covers a run against a provider that does not stream.
func (t *turnWriter) Message(resp llm.Response, _ bool) {
	for i, block := range resp.Content {
		reasoning, text, drawn := blockText(block)
		if !drawn {
			continue
		}

		b, streamed := t.blocks[i]
		if streamed {
			t.endBlock(b)

			continue
		}

		if text == "" {
			continue
		}

		t.whole(reasoning, text)
	}

	clear(t.blocks)
}

// whole writes one prose or reasoning block as its start, its text and its end
// together, for a block no fragment arrived for.
func (t *turnWriter) whole(reasoning bool, text string) {
	b := &blockPart{reasoning: reasoning, id: t.mintID(), open: true}

	t.startBlock(b)
	t.stream.event(contentEvent(b, text))
	t.endBlock(b)
}

// ToolCall renders a dispatched call as the call it is and the arguments it was
// dispatched with, which AG-UI streams and this run reports whole.
//
// The call a resumed question was about is left out. Its tool ran on the turn that
// asked, so the client already holds the call, and a client accumulates a call's
// argument fragments rather than replacing them: sending it again writes the arguments
// into the thread twice. It is spent on the first call that matches, so a later call of
// the same tool is sent.
func (t *turnWriter) ToolCall(trace agent.ToolTrace) {
	if t.resent != "" && t.resent == trace.ID {
		t.resent = ""

		return
	}

	t.call(trace.ID, trace.Name, trace.Input)
}

// call writes one tool call.
func (t *turnWriter) call(id string, name string, input json.RawMessage) {
	t.stream.event(events.NewToolCallStartEvent(id, name))
	t.stream.event(events.NewToolCallArgsEvent(id, toolInput(input)))
	t.stream.event(events.NewToolCallEndEvent(id))
}

// ToolResult renders what a call returned.
//
// A call that failed carries its error text as the result. The protocol's result event
// has no field marking one, where the message a snapshot carries does, so a live turn
// tells the model and the page the same thing the tool said.
func (t *turnWriter) ToolResult(trace agent.ToolResultTrace) {
	t.stream.event(events.NewToolCallResultEvent(t.mintID(), trace.CallID, toolOutput(trace.Output)))
}

// toolOutput is what a call returned as a result event carries it.
//
// The protocol requires content on that event, so a tool that printed nothing is
// reported as having printed nothing. Leaving the event out instead would leave the
// call reading as one that never returned, and sending empty content writes an event
// the protocol's own reader refuses.
func toolOutput(output string) string {
	if output == "" {
		return emptyOutputText
	}

	return output
}

// Warn sends what the run raised: a tool stopped on its timeout, a tool the model
// called that does not exist, approvals dropped by a forced resume. It is the one thing
// a run reports that AG-UI has no event for, so it goes as a custom event under a name
// a client can branch on.
func (t *turnWriter) Warn(w agent.Warning) {
	value := warningValue{Kind: w.Kind.String(), Name: w.Name, Count: w.Count, Params: w.Params}
	if w.Err != nil {
		value.Error = w.Err.Error()
	}

	t.stream.event(events.NewCustomEvent(customWarning, events.WithValue(value)))
}

// Ask holds the question the turn ended on. It is carried on the run-finished event
// Close sends, since an AG-UI interrupt is what a run finishes with rather than
// something sent during it.
//
// Nothing is synthesized before it. The gate runs before the run traces the call, so no
// tool call has been sent for the command being approved, and the interrupt carries the
// rendered command line and the tag that gated it. The turn that resumes traces that
// call and sends it the way it sends any other.
func (t *turnWriter) Ask(q web.Question) {
	t.Open()

	t.interrupts = append(t.interrupts, interruptFor(q))
}

// Close ends the run with what the turn ended on.
//
// Every message still open is ended first: a run that died between the model call and
// its assembled turn leaves fragments whose message the client would otherwise hold in
// its streaming state for the rest of the thread.
//
// A turn that ended on a question finishes on the interrupt rather than on the error
// the suspend would otherwise report. The suspend and the question are the same fact,
// and a run error would end the thread where the client is meant to start another one
// carrying the answer.
func (t *turnWriter) Close(e web.Ending) {
	t.Open()
	t.closeBlocks()

	if e.PromptNotTaken {
		t.stream.event(events.NewCustomEvent(customPromptNotTaken, events.WithValue(noticeValue{Message: promptNotTakenText})))
	}

	switch {
	case len(t.interrupts) > 0:
		t.stream.event(events.NewRunFinishedEventWithOptions(t.threadID, t.runID,
			events.WithInterruptOutcome(t.interrupts)))

	case e.Outcome.Err != nil:
		t.stream.event(t.runError(e))

	default:
		t.stream.event(events.NewRunFinishedEventWithOptions(t.threadID, t.runID,
			events.WithSuccessOutcome(),
			events.WithResult(runResult{Reason: string(e.Outcome.Reason)})))
	}
}

// runError is the run's failure as the client reads it. The code is the one
// serve.ErrorCode places on the outcome, so a page can tell a provider that is busy
// from a request the model refused.
func (t *turnWriter) runError(e web.Ending) events.Event {
	options := []events.RunErrorOption{events.WithRunID(t.runID)}

	code := serve.ErrorCode(e.Outcome)
	if code != "" {
		options = append(options, events.WithErrorCode(code))
	}

	return events.NewRunErrorEvent(e.Outcome.Err.Error(), options...)
}

// Replay writes a stored conversation as one messages snapshot: the user turns, the
// assistant turns, and the calls with the results that answered them, in the order the
// journal holds them.
//
// A snapshot is what AG-UI has for a conversation that is already over, so nothing is
// re-narrated as the events a live turn produced and a client that renders a thread
// renders this one without knowing it was read back.
//
// A conversation the run left waiting on a question is followed by Ask, which puts the
// interrupt on the run-finished event. The pending turn is in the snapshot, so the call
// the interrupt names is a call the client holds.
func (t *turnWriter) Replay(rs *runstate.RunState) {
	if rs == nil {
		return
	}

	t.stream.event(events.NewMessagesSnapshotEvent(t.snapshot(rs)))
}

// The run reports these for an operator watching a terminal. AG-UI has no event for the
// resolved run parameters, a verbose request summary or a rotated session, and a
// panic's stack reaches this sink and no further: the run's error carries what the page
// is told.
func (t *turnWriter) Starting(agent.RunInfo) {}
func (t *turnWriter) LLMRequest(string)      {}
func (t *turnWriter) SessionRotated(string)  {}
func (t *turnWriter) Panicked(any, []byte)   {}

// startBlock tells the client a message started.
func (t *turnWriter) startBlock(b *blockPart) {
	if b.reasoning {
		t.stream.event(events.NewReasoningMessageStartEvent(b.id, string(types.RoleAssistant)))

		return
	}

	t.stream.event(events.NewTextMessageStartEvent(b.id, events.WithRole(string(types.RoleAssistant))))
}

// contentEvent is one message's text, on the message its block is open under.
func contentEvent(b *blockPart, text string) events.Event {
	if b.reasoning {
		return events.NewReasoningMessageContentEvent(b.id, text)
	}

	return events.NewTextMessageContentEvent(b.id, text)
}

// closeBlocks ends every message still open and forgets them.
func (t *turnWriter) closeBlocks() {
	for _, b := range t.blocks {
		t.endBlock(b)
	}

	clear(t.blocks)
}

// endBlock tells the client a message ended, once.
func (t *turnWriter) endBlock(b *blockPart) {
	if !b.open {
		return
	}
	b.open = false

	if b.reasoning {
		t.stream.event(events.NewReasoningMessageEndEvent(b.id))

		return
	}

	t.stream.event(events.NewTextMessageEndEvent(b.id))
}

// mintID names one message of this thread. The run id is in it because a client keeps
// the messages of every run of a thread, and two runs numbering from one would have the
// second run's first message written into the first run's.
func (t *turnWriter) mintID() string {
	t.ids++

	return t.runID + "-" + strconv.Itoa(t.ids)
}

// blockKind reports whether a fragment's block is reasoning rather than prose, and
// whether this format draws it at all.
func blockKind(k llm.DeltaKind) (bool, bool) {
	switch k {
	case llm.DeltaText:
		return false, true
	case llm.DeltaThinking:
		return true, true
	default:
		return false, false
	}
}

// blockText is one content block as a message: whether it is reasoning, the text it
// carries, and whether it is drawn here at all. A tool call is rendered from the trace
// the run emits when it dispatches it, and a tool result and a provider block have no
// message of their own.
func blockText(b llm.ContentBlock) (bool, string, bool) {
	switch {
	case b.Text != nil:
		return false, b.Text.Text, true
	case b.Thinking != nil:
		return true, b.Thinking.Text, true
	default:
		return false, "", false
	}
}

// toolInput is a call's arguments as the client parses them. A call with no arguments
// is an empty object rather than an empty string, which is not JSON and which the
// protocol's own args event refuses.
func toolInput(input json.RawMessage) string {
	if len(input) == 0 {
		return "{}"
	}

	return string(input)
}
