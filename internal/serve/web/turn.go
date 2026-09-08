//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"

	"github.com/segmentio/ksuid"

	wire "github.com/choria-io/fisk-ai/internal/a2a/wire/v1"
	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/mcpclient"
	"github.com/choria-io/fisk-ai/internal/remotetools"
	"github.com/choria-io/fisk-ai/internal/serve"
)

// turn is one admitted request on its way to being a run: the journal its thread runs
// in, the response it is written to, and the answer it carried.
type turn struct {
	ch         *Channel
	id         string
	session    string
	prompt     string
	caller     string
	checkpoint agent.Checkpoint
	log        *slog.Logger

	// w is the response the run is written to, held here for the endings that answer
	// with a status code and never reach the writer.
	w      http.ResponseWriter
	writer TurnWriter

	// events is what the run reports into, and prompter what it puts its questions to.
	events   *turnEvents
	prompter *prompter

	// done is closed once the ending has been written, which lets the request return.
	done chan struct{}
}

// newTurn builds the turn one request becomes. An answer the request carried goes to
// the prompter, which answers the question the resumed run puts about that call.
func (c *Channel) newTurn(in Turn, session string, held bool, w http.ResponseWriter, writer TurnWriter, log *slog.Logger) *turn {
	id := ksuid.New().String()

	t := &turn{
		ch:         c,
		id:         id,
		session:    session,
		prompt:     in.Prompt,
		caller:     in.Caller,
		checkpoint: checkpointFor(session, held, in.Prompt),
		log:        log.With("turn", id),
		w:          w,
		writer:     writer,
		events:     &turnEvents{w: writer},
		prompter:   newPrompter(log.With("turn", id)),
		done:       make(chan struct{}),
	}

	if in.Answer != nil {
		t.prompter.hold(*in.Answer)
	}

	return t
}

// work is the unit the server runs.
//
// PromptsMayBlock is false and PromptWait is unset, since no question is ever held: the
// prompter answers from the request or ends the turn at once. HumanPaced is set because
// the next turn of a thread arrives when somebody types it. The caller is the request's
// own claim and is recorded unverified, since this transport authenticates nobody.
func (t *turn) work() *serve.Work {
	return &serve.Work{
		ID:               t.id,
		Prompt:           t.prompt,
		Checkpoint:       t.checkpoint,
		ClaimedBy:        channelName + "/" + t.id,
		Caller:           serve.Caller{Name: t.caller},
		Events:           t.events,
		Prompter:         t.prompter,
		SuspendRequested: t.ch.suspendRequested,
		HumanPaced:       true,
		RunContext:       t.runContext,
		Done:             t.finish,
	}
}

// runContext leaves the run on the server's own context rather than the request's. A
// closed tab, a navigation away or React StrictMode's double mount would otherwise
// cancel the run and record ReasonError for a conversation the person has not abandoned.
func (t *turn) runContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return ctx, nil
}

// finish writes the ending and lets the request return. The server calls it exactly
// once, on a context that is not the run's.
//
// A run that reached no event has claimed nothing and streamed nothing, so a refusal is
// answered with a status code the page can act on. Everything else goes through the
// writer: the question the turn ended on, if any, and then the outcome.
func (t *turn) finish(_ context.Context, out serve.Outcome) error {
	defer close(t.done)

	status, reason := refusalFor(out)
	if status != 0 && !t.events.opened() {
		t.log.Info("Refusing a turn with a status code", "status", status, "reason", out.Reason, "error", out.Err)
		http.Error(t.w, reason, status)

		return nil
	}

	t.events.open()

	q := t.prompter.question()
	if q != nil {
		t.log.Info("Ending the turn on a question", "kind", q.Kind, "tool_use", q.ToolUseID)
		t.writer.Ask(*q)
	}

	notTaken := t.checkpoint.FollowUp && !out.FollowUpTaken
	if notTaken {
		t.log.Info("The conversation did not take the prompt", "reason", out.Reason, "deferred", len(out.Deferred))
	}

	t.log.Info("A turn ended", "reason", out.Reason, "error", out.Err, "asked", q != nil)

	t.writer.Close(Ending{Outcome: out, PromptNotTaken: notTaken})

	return nil
}

// refusalFor is the status code and the line for an outcome that ran nothing, and zero
// for one that has to be written by the format.
//
// The busy answer comes from the claim record: the first append of a resume is written
// before any model call, so the loser fails inside agent.Run with nothing streamed and
// serve.ErrorCode places the wrapped runstate.ErrLocked.
func refusalFor(out serve.Outcome) (int, string) {
	switch {
	case out.Abandoned:
		return http.StatusServiceUnavailable, stoppedRefusal
	case serve.ErrorCode(out) == wire.CodeConversationBusy:
		return http.StatusConflict, busyRefusal
	case errors.Is(out.Err, agent.ErrConversationNotFound):
		return http.StatusNotFound, unknownRefusal
	default:
		return 0, ""
	}
}

// The runner asserts this half at runtime, so a sink that stopped satisfying it would
// silently stop streaming.
var _ agent.MessageStreamer = (*turnEvents)(nil)

// turnEvents stands between the server and the format's writer, and writes the response
// head at the first event that proves the run holds its journal.
//
// That event is Starting. The run raises advisories while it assembles its tool set,
// before the journal is opened and claimed, so a head written on the first event of any
// kind would answer a turn that then loses the claim with a 200 and an empty body.
// Everything before Starting is held and forwarded after Open; a run that never reaches
// Starting is answered with a status code and what was held is dropped with the writer.
//
// Every method runs on the run goroutine in loop order, and the ending runs on the same
// goroutine after the run, so the lock covers the contract rather than a race this
// package produces.
type turnEvents struct {
	w TurnWriter

	mu      sync.Mutex
	isOpen  bool
	pending []func()
}

// open writes the head once and forwards what arrived before it.
func (e *turnEvents) open() {
	e.mu.Lock()
	if e.isOpen {
		e.mu.Unlock()

		return
	}
	e.isOpen = true
	held := e.pending
	e.pending = nil
	e.mu.Unlock()

	e.w.Open()

	for _, f := range held {
		f()
	}
}

// opened reports whether the head has been written.
func (e *turnEvents) opened() bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	return e.isOpen
}

// forward passes one event to the writer, or holds it until the head is written.
func (e *turnEvents) forward(f func()) {
	e.mu.Lock()
	if !e.isOpen {
		e.pending = append(e.pending, f)
		e.mu.Unlock()

		return
	}
	e.mu.Unlock()

	f()
}

// Starting is the first event after the journal is claimed on a resume and created on
// a first turn, so it opens the response.
func (e *turnEvents) Starting(info agent.RunInfo) {
	e.open()
	e.w.Starting(info)
}

func (e *turnEvents) Warn(w agent.Warning)               { e.forward(func() { e.w.Warn(w) }) }
func (e *turnEvents) LLMRequest(summary string)          { e.forward(func() { e.w.LLMRequest(summary) }) }
func (e *turnEvents) ToolCall(t agent.ToolTrace)         { e.forward(func() { e.w.ToolCall(t) }) }
func (e *turnEvents) ToolResult(t agent.ToolResultTrace) { e.forward(func() { e.w.ToolResult(t) }) }
func (e *turnEvents) Message(resp llm.Response, terminal bool) {
	e.forward(func() { e.w.Message(resp, terminal) })
}
func (e *turnEvents) SessionRotated(prevID string) { e.forward(func() { e.w.SessionRotated(prevID) }) }
func (e *turnEvents) Panicked(value any, stack []byte) {
	e.forward(func() { e.w.Panicked(value, stack) })
}

// The optional halves of agent.Events are implemented unconditionally and forwarded
// only to a writer that wants them, so a format that renders for a person still receives
// them and one that does not is not made to implement them by sitting behind this.
func (e *turnEvents) RemoteHostNotes(notes []remotetools.HostImport) {
	reporter, ok := e.w.(agent.RemoteHostReporter)
	if ok {
		e.forward(func() { reporter.RemoteHostNotes(notes) })
	}
}

func (e *turnEvents) MCPServerNotes(imports []mcpclient.ServerImport) {
	reporter, ok := e.w.(agent.MCPServerReporter)
	if ok {
		e.forward(func() { reporter.MCPServerNotes(imports) })
	}
}

// StreamDeltas asks the writer. A writer that does not stream answers false, so the run
// makes the ordinary call and the turn arrives through Message alone.
func (e *turnEvents) StreamDeltas() bool {
	streamer, ok := e.w.(agent.MessageStreamer)
	if !ok {
		return false
	}

	return streamer.StreamDeltas()
}

func (e *turnEvents) MessageDelta(d llm.Delta) {
	streamer, ok := e.w.(agent.MessageStreamer)
	if ok {
		e.forward(func() { streamer.MessageDelta(d) })
	}
}
