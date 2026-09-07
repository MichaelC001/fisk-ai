//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package vercel

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/segmentio/ksuid"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/serve/web"
)

// The channel drives the writer through both halves, and the runner asserts the
// streaming one at runtime, so a sink that stopped satisfying either would stop
// streaming with no error raised anywhere.
var (
	_ web.TurnWriter        = (*turnWriter)(nil)
	_ agent.MessageStreamer = (*turnWriter)(nil)
)

// promptNotTakenText is what a page is told when its message did not enter the
// conversation: the turn resumed on an unanswered question, put the question again,
// and suspended without reaching a boundary that takes a user message.
const promptNotTakenText = "your message was not delivered: this conversation is waiting on a question, so answer that first and send the message again"

// turnWriter renders one turn as the UI message stream.
//
// Every method runs on the run's goroutine in loop order, and the ending runs on the
// same goroutine after the run, so nothing here locks.
type turnWriter struct {
	stream *stream
	opened bool

	// blocks holds the open stream part of each content block of the model call
	// running now. llm.Delta.Index restarts at zero on every call, so Message clears
	// them, and ids are minted rather than taken from the index because a part id has
	// to be unique across the whole message.
	blocks map[int]*blockPart
	ids    int

	// suppress is the call an answered question named, whose traced
	// tool-input-available the resuming turn would send a second time under the same
	// toolCallId. It comes from the request rather than from anything remembered, so
	// any process can serve the turn that answers, and it is spent on the first call
	// that matches.
	suppress string

	// asked reports that the turn ended on a question, which is the ending rather
	// than the suspend the run reports it as.
	asked bool

	// continues is the assistant message the client is still building, which this
	// response head names so the client writes into it rather than appending a second
	// message beside it. Empty starts a message under a new id.
	continues string
}

// blockPart is one content block's stream part: which kind it is, the id it was
// opened under, and whether the client has been told it ended.
type blockPart struct {
	kind string
	id   string
	open bool
}

// newTurnWriter holds the response for one turn. answered is the call the request
// carried an answer for, empty on a request that only prompts, and continues the
// assistant message the client is still building, empty on a request that starts one.
func newTurnWriter(w http.ResponseWriter, answered string, continues string) *turnWriter {
	return &turnWriter{
		stream:    newStream(w),
		blocks:    map[int]*blockPart{},
		suppress:  answered,
		continues: continues,
	}
}

// Open sends the response head and starts the assistant message. It is idempotent:
// the channel calls it before the first event, and Close calls it when the outcome
// arrived before any event did.
//
// A turn answering a question names the message the client is still building, so the
// parts of this turn go into it. A turn the page started with a message of its own
// opens a new one.
func (t *turnWriter) Open() {
	if t.opened {
		return
	}
	t.opened = true

	id := t.continues
	if id == "" {
		id = ksuid.New().String()
	}

	t.stream.open()
	t.stream.part(startPart{Type: "start", MessageID: id})
}

// StreamDeltas asks for the fragments of each assistant turn, which is what makes the
// page type rather than wait.
func (t *turnWriter) StreamDeltas() bool { return true }

// MessageDelta renders one fragment on the part its block index is open under,
// opening that part on the index's first fragment and ending it on its last.
func (t *turnWriter) MessageDelta(d llm.Delta) {
	kind := partKind(d.Kind)
	if kind == "" {
		return
	}

	b, held := t.blocks[d.Index]
	if !held {
		b = &blockPart{kind: kind, id: t.mintID(), open: true}
		t.blocks[d.Index] = b

		t.stream.part(boundaryPart{Type: b.kind + "-start", ID: b.id})
	}

	// A fragment after the index's last one is a backend contradicting itself. The
	// part has been ended and the client would take the text as a new one, so it is
	// dropped and the assembled turn is what the index says.
	if !b.open {
		return
	}

	// A final fragment with nothing left to send carries no text, and delta is a
	// required field, so an empty one is not sent at all.
	if d.Text != "" {
		t.stream.part(deltaPart{Type: b.kind + "-delta", ID: b.id, Delta: d.Text})
	}

	if d.Final {
		t.endBlock(b)
	}
}

// Message renders the assistant turn a model call produced and ends that call's
// parts.
//
// A block whose fragments were streamed is ended rather than sent again, which is the
// reconcile rule llm.DeltaAssembler holds: the whole block replaces the fragments of
// its index, and the client already has that text. A block that streamed nothing is
// written whole, which covers a run against a provider that does not stream.
func (t *turnWriter) Message(resp llm.Response, _ bool) {
	for i, block := range resp.Content {
		kind, text := blockText(block)
		if kind == "" {
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

		id := t.mintID()

		t.stream.part(boundaryPart{Type: kind + "-start", ID: id})
		t.stream.part(deltaPart{Type: kind + "-delta", ID: id, Delta: text})
		t.stream.part(boundaryPart{Type: kind + "-end", ID: id})
	}

	clear(t.blocks)
}

// ToolCall renders a dispatched call with the arguments it was dispatched with.
//
// The call an answered question named is left out: the turn that asked already sent
// its part, and a second one under the same toolCallId draws a second card for the
// call the person is watching.
func (t *turnWriter) ToolCall(trace agent.ToolTrace) {
	if t.suppress != "" && t.suppress == trace.ID {
		t.suppress = ""

		return
	}

	t.stream.part(toolInputPart{
		Type:       "tool-input-available",
		ToolCallID: trace.ID,
		ToolName:   trace.Name,
		Input:      toolInput(trace.Input),
		Dynamic:    true,
	})
}

// ToolResult renders what a call returned, on the part the call opened.
func (t *turnWriter) ToolResult(trace agent.ToolResultTrace) {
	if trace.IsError {
		t.stream.part(toolErrorPart{
			Type:       "tool-output-error",
			ToolCallID: trace.CallID,
			ErrorText:  trace.Output,
			Dynamic:    true,
		})

		return
	}

	t.stream.part(toolOutputPart{
		Type:       "tool-output-available",
		ToolCallID: trace.CallID,
		Output:     trace.Output,
		Dynamic:    true,
	})
}

// Ask puts the question the turn ended on.
//
// An approval carries no part of its own: tool-approval-request mutates the tool part
// its toolCallId names, and the client drops one naming a part it does not hold. The
// gate runs before the call is traced, so that part is sent here, carrying the gate's
// rendered command line and the tag that gated it.
//
// The other three questions have no shape in the AI SDK, so each goes as a data part
// the page reads without a schema and answers in fiskAnswer.
func (t *turnWriter) Ask(q web.Question) {
	t.Open()
	t.asked = true

	if q.Kind == web.KindApprove {
		input, err := json.Marshal(approvalInput{Command: q.Display, Tag: q.Tag})
		if err != nil {
			input = json.RawMessage("{}")
		}

		t.stream.part(toolInputPart{
			Type:       "tool-input-available",
			ToolCallID: q.ToolUseID,
			ToolName:   q.Command,
			Input:      input,
			Dynamic:    true,
		})

		t.stream.part(approvalPart{
			Type:       "tool-approval-request",
			ApprovalID: approvalID(q.ToolUseID),
			ToolCallID: q.ToolUseID,
			Reason:     q.Display,
		})

		return
	}

	t.stream.part(dataPart{
		Type: "data-question",
		ID:   q.ToolUseID,
		Data: questionData{
			ToolUseID: q.ToolUseID,
			Kind:      string(q.Kind),
			Question:  q.Text,
			Options:   q.Options,
			Default:   q.Default,
		},
	})
}

// Close ends the body with what the turn ended on.
//
// Every part still open is ended first: a run that died between the model call and
// its assembled turn leaves fragments whose part the client would otherwise hold in
// its streaming state for the rest of the message.
//
// A turn that ended on a question sends no error part. The suspend the run reports is
// the same fact as the question, and sending it too would put "the run was suspended"
// on the page beside the thing it is asking the person.
func (t *turnWriter) Close(e web.Ending) {
	t.Open()
	t.closeBlocks()

	switch {
	case e.PromptNotTaken:
		t.stream.part(errorPart{Type: "error", ErrorText: promptNotTakenText})

	case !t.asked && e.Outcome.Err != nil:
		t.stream.part(errorPart{Type: "error", ErrorText: e.Outcome.Err.Error()})
	}

	t.stream.part(finishPart{Type: "finish", FinishReason: t.finishReason(e)})
	t.stream.done()
}

// Replay writes a stored conversation as the parts the page would have seen while it
// ran. It renders nothing: reading a runstate.RunState into UI message stream parts
// is the work of the item that opens a stored session, and no caller replays yet.
func (t *turnWriter) Replay(*runstate.RunState) {}

// The run reports these for an operator watching a terminal. The AI SDK client has no
// part for an advisory, a request summary or a rotated session, and a panic's stack
// reaches this sink and no further: the run's error carries what the page is told.
func (t *turnWriter) Warn(agent.Warning)     {}
func (t *turnWriter) Starting(agent.RunInfo) {}
func (t *turnWriter) LLMRequest(string)      {}
func (t *turnWriter) SessionRotated(string)  {}
func (t *turnWriter) Panicked(any, []byte)   {}

// finishReason maps how the turn ended onto the AI SDK's six reasons. A run names
// more endings than the SDK does, so several collapse onto other.
func (t *turnWriter) finishReason(e web.Ending) string {
	if t.asked {
		return "tool-calls"
	}

	switch e.Outcome.Reason {
	case runstate.ReasonCompleted:
		return "stop"
	case runstate.ReasonBudget, runstate.ReasonMaxIterations:
		return "length"
	case runstate.ReasonError:
		return "error"
	case runstate.ReasonSuspended:
		return "other"
	}

	if e.Outcome.Err != nil {
		return "error"
	}

	return "other"
}

// closeBlocks ends every part still open and forgets them.
func (t *turnWriter) closeBlocks() {
	for _, b := range t.blocks {
		t.endBlock(b)
	}

	clear(t.blocks)
}

// endBlock tells the client a part ended, once.
func (t *turnWriter) endBlock(b *blockPart) {
	if !b.open {
		return
	}
	b.open = false

	t.stream.part(boundaryPart{Type: b.kind + "-end", ID: b.id})
}

// mintID names one text or reasoning part within this message.
func (t *turnWriter) mintID() string {
	t.ids++

	return strconv.Itoa(t.ids)
}

// approvalID names one approval. The page answers by naming the call, so this is
// derived from the call rather than minted and remembered, which is what lets a
// second process serve the turn that answers.
func approvalID(toolUseID string) string { return "approval-" + toolUseID }

// partKind is the stream part a fragment's block kind is rendered as, empty for a
// kind this format does not draw.
func partKind(k llm.DeltaKind) string {
	switch k {
	case llm.DeltaText:
		return "text"
	case llm.DeltaThinking:
		return "reasoning"
	default:
		return ""
	}
}

// blockText is the stream part one content block is rendered as and the text it
// carries. A tool call is rendered from the trace the run emits when it dispatches
// it, and a tool result and a provider block have no part here at all.
func blockText(b llm.ContentBlock) (string, string) {
	switch {
	case b.Text != nil:
		return "text", b.Text.Text
	case b.Thinking != nil:
		return "reasoning", b.Thinking.Text
	default:
		return "", ""
	}
}

// toolInput is a call's arguments as the client parses them. It parses input as
// unknown, so an absent one passes as null and leaves the page rendering that word;
// an empty object is what a call with no arguments means.
func toolInput(input json.RawMessage) json.RawMessage {
	if len(input) == 0 {
		return json.RawMessage("{}")
	}

	return input
}
