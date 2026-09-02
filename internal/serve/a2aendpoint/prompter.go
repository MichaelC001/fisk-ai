//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2aendpoint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/choria-io/fisk-ai/internal/a2a"
	wire "github.com/choria-io/fisk-ai/internal/a2a/wire/v1"
	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// elicitPrompter puts a run's questions to the caller that submitted the task. It is
// what the prompts channel supplies on Work.Prompter when the elicit key is on, and it
// is the difference between a served run that refuses every confirmation-gated tool and
// one that can ask.
//
// A question goes out on the task's reply set and the answer arrives on the task's own
// inbound path, correlated by a question id this mints. The run goroutine calls one
// method at a time and blocks in it; the answer arrives on the transport's goroutine
// and is handed over through the channel the pending question holds.
//
// It bounds the question itself rather than leaving it to serve, which is what lets a
// caller hold one open: each window is expose.agent.a2a.request_timeout, and a caller
// with a person in front of the question restarts it by answering AnswerWaiting. So the
// channel sets Work.PromptsMayBlock and serve wraps nothing.
type elicitPrompter struct {
	task *task

	// window is how long one question is held before this gives up on it, restarted
	// each time the caller says somebody is still looking at it.
	window time.Duration

	mu      sync.Mutex
	pending map[string]*pending
	closed  bool

	// heldCall and heldChoice are an approval the request carried, for a caller that
	// was asked before its run gave up and is answering now. The gate asks about the
	// same call again on this resume, and that question is answered from here rather
	// than put back to the caller which already answered it.
	//
	// It is spent on the first question about that call, so a second call of the same
	// tool is asked about, and the run asks normally once it is gone.
	heldCall   string
	heldChoice toolkit.ConfirmChoice
}

// hold takes the approval a request carried, to answer the question the resumed run
// puts about that call.
func (p *elicitPrompter) hold(toolUseID string, choice toolkit.ConfirmChoice) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.heldCall = toolUseID
	p.heldChoice = choice
}

// heldFor reports the answer this task is holding for the named call, and spends it.
func (p *elicitPrompter) heldFor(toolUseID string) (toolkit.ConfirmChoice, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if toolUseID == "" || p.heldCall != toolUseID {
		return toolkit.ConfirmNo, false
	}

	p.heldCall = ""

	return p.heldChoice, true
}

// pending is one question this run is waiting on: the answer, and the evidence that
// somebody is still there to give one.
//
// The two are kept apart rather than sharing one channel. deliver reports a full buffer
// as an answer that reached nothing, so evidence queued in the answer's place would
// refuse the operator's real answer, and ask would take the evidence for the reply and
// fail the run on it, ApproveCommand by denying the gate.
type pending struct {
	// answer carries the reply that ends the question. It is buffered so a handler
	// never blocks on a waiter that has already given up.
	answer chan *wire.ElicitReply
	// alive carries the caller saying the question is still on somebody's screen. It
	// is buffered by one and written without blocking, since restarting a window twice
	// is restarting it once.
	alive chan struct{}
}

func newElicitPrompter(t *task) *elicitPrompter {
	return &elicitPrompter{task: t, window: t.ch.promptWait, pending: map[string]*pending{}}
}

// CanPrompt reports true: the caller is on the other end of the task's reply set and
// can be asked. Whether it has an operator behind it is the caller's own answer, which
// arrives as no_operator and reaches the gate as a denial.
func (p *elicitPrompter) CanPrompt() bool { return true }

// ApproveCommand puts a confirmation-gated command to the caller.
//
// An unanswered approval reports toolkit.ErrPromptAborted rather than a deferral. The
// gate guards a call that has not run, so the awaited thing is permission: a deferred
// call is never dispatched again, and a peer approving later would find nothing left to
// run. The abort leaves the call unanswered, so the resume dispatches it and asks again.
func (p *elicitPrompter) ApproveCommand(ctx context.Context, req toolkit.GateRequest) (toolkit.ConfirmChoice, error) {
	choice, held := p.heldFor(req.ToolUseID)
	if held {
		p.task.log.Info("Answering an approval from the answer its caller sent", "tool_use", req.ToolUseID, "command", req.Command)

		return choice, nil
	}

	ask := wire.NewElicitRequest(wire.ElicitApprove, wire.NewID())
	ask.ToolUseID = req.ToolUseID
	ask.Command = req.Command
	ask.Display = req.Display
	ask.Tag = req.Tag

	reply, err := p.ask(ctx, ask, unansweredAborts)
	if err != nil {
		return toolkit.ConfirmNo, err
	}

	switch reply.Answer {
	case wire.AnswerNoOperator:
		return toolkit.ConfirmNo, errNoOperatorThere

	case wire.AnswerChoice:
		switch reply.Choice {
		case wire.ChoiceAlways:
			return toolkit.ConfirmAlways, nil
		case wire.ChoiceOnce:
			return toolkit.ConfirmOnce, nil
		default:
			return toolkit.ConfirmNo, nil
		}

	default:
		return toolkit.ConfirmNo, fmt.Errorf("the caller answered an approval with %q", reply.Answer)
	}
}

// Confirm puts a yes/no question to the caller.
func (p *elicitPrompter) Confirm(ctx context.Context, question string) (bool, error) {
	ask := wire.NewElicitRequest(wire.ElicitConfirm, wire.NewID())
	ask.ToolUseID = toolkit.ToolUseIDFromContext(ctx)
	ask.Question = question

	reply, err := p.ask(ctx, ask, unansweredDefers)
	if err != nil {
		return false, err
	}

	switch reply.Answer {
	case wire.AnswerNoOperator:
		return false, errNoOperatorThere
	case wire.AnswerConfirmed:
		return reply.Confirmed, nil
	default:
		return false, fmt.Errorf("the caller answered a confirmation with %q", reply.Answer)
	}
}

// Select asks the caller to choose one of options and returns its index. An index
// outside the options is a choice nobody made, reported as such rather than clamped.
func (p *elicitPrompter) Select(ctx context.Context, question string, options []string) (int, error) {
	ask := wire.NewElicitRequest(wire.ElicitSelect, wire.NewID())
	ask.ToolUseID = toolkit.ToolUseIDFromContext(ctx)
	ask.Question = question
	ask.Options = options

	reply, err := p.ask(ctx, ask, unansweredDefers)
	if err != nil {
		return -1, err
	}

	switch reply.Answer {
	case wire.AnswerNoOperator:
		return -1, errNoOperatorThere

	case wire.AnswerIndex:
		if reply.Index < 0 || reply.Index >= len(options) {
			return -1, fmt.Errorf("the caller chose option %d of %d", reply.Index, len(options))
		}

		return reply.Index, nil

	default:
		return -1, fmt.Errorf("the caller answered a selection with %q", reply.Answer)
	}
}

// Input asks the caller for a free text value. An empty string is a valid answer,
// which is why the reply's own answer field says what was given.
func (p *elicitPrompter) Input(ctx context.Context, question, def string) (string, error) {
	ask := wire.NewElicitRequest(wire.ElicitInput, wire.NewID())
	ask.ToolUseID = toolkit.ToolUseIDFromContext(ctx)
	ask.Question = question
	ask.Default = def

	reply, err := p.ask(ctx, ask, unansweredDefers)
	if err != nil {
		return "", err
	}

	switch reply.Answer {
	case wire.AnswerNoOperator:
		return "", errNoOperatorThere
	case wire.AnswerValue:
		return reply.Value, nil
	default:
		return "", fmt.Errorf("the caller answered an input with %q", reply.Answer)
	}
}

// unanswered says what a question that got no answer reports, which differs by who
// asked it. The two are named here rather than passed as errors so each call site reads
// as the decision it is making.
type unanswered int

const (
	// unansweredAborts leaves the call unanswered and ends the run, so the resume asks
	// again. It is what the confirm gate needs.
	unansweredAborts unanswered = iota
	// unansweredDefers marks the call deferred, so the answer can be supplied to it
	// later. It is what a tool whose result is the answer needs.
	unansweredDefers
)

// errNoOperatorThere is what a caller with nobody to ask produces. It is an error
// rather than a decline so the gate treats it as the default-deny it already applies to
// a prompt it could not put, and the three human-in-the-loop tools answer the model
// with their own no-operator result.
var errNoOperatorThere = fmt.Errorf("the caller has no operator to answer on its behalf")

// ask publishes one question and waits for the answer that matches it, holding it open
// for as long as the caller keeps saying somebody is looking at it.
//
// The pending entry is registered before the question is published, so an answer
// arriving while the publish is still returning has somewhere to land rather than being
// dropped as belonging to no question.
//
// A drain stops the restarts and lets the window in force run down. Server.Drain closes
// the endpoints and waits for the runs already under way without canceling them, so a
// question that could be held indefinitely would hold the shutdown with it, and the
// operator's only way out would be the second interrupt that cancels every run
// mid-flight.
func (p *elicitPrompter) ask(ctx context.Context, question *wire.ElicitRequest, onSilence unanswered) (*wire.ElicitReply, error) {
	question.WaitMS = p.window.Milliseconds()

	waiting, err := p.register(question.QuestionID)
	if err != nil {
		return nil, err
	}
	defer p.forget(question.QuestionID)

	err = p.task.stream.Elicit(question)
	if err != nil {
		return nil, fmt.Errorf("putting the question to the caller: %w", err)
	}

	p.task.log.Info("Asked the caller a question", "question", question.QuestionID, "kind", question.Kind, "wait_ms", question.WaitMS)

	asked := time.Now()
	acks := 0

	window := time.NewTimer(p.window)
	defer window.Stop()

	for {
		var cause error

		select {
		case reply := <-waiting.answer:
			p.task.log.Info("A question was answered", "question", question.QuestionID, "held", time.Since(asked).Round(time.Second), "acks", acks)

			return reply, nil

		case <-waiting.alive:
			acks++

			if p.task.ch.draining() {
				p.task.log.Debug("A question's window is not restarted during a drain", "question", question.QuestionID)

				continue
			}

			window.Stop()
			window.Reset(p.window)

			p.task.log.Debug("The caller is still waiting on a question", "question", question.QuestionID, "acks", acks)

			continue

		case <-window.C:
			cause = context.DeadlineExceeded

		case <-p.task.stop:
			// The caller asked the run to stop. A question is not a boundary, so
			// waiting out the window first would hold the worker for minutes after the
			// answer stopped mattering.
			cause = errors.New("the caller asked the run to stop")

		case <-ctx.Done():
			// The run ended under the question: the worker was stopped rather than
			// drained.
			cause = ctx.Err()
		}

		p.task.log.Info("A question went unanswered", "question", question.QuestionID, "cause", cause, "held", time.Since(asked).Round(time.Second), "acks", acks)

		if onSilence == unansweredDefers {
			return nil, toolkit.DeferResult(fmt.Sprintf("waiting on the caller to answer question %s", question.QuestionID), question.QuestionID)
		}

		return nil, fmt.Errorf("%w: the caller did not answer: %w", toolkit.ErrPromptAborted, cause)
	}
}

// register claims the question id, so the answer to it and the evidence somebody is
// still there both reach the goroutine waiting.
func (p *elicitPrompter) register(id string) (*pending, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil, fmt.Errorf("%w: the run ended", toolkit.ErrPromptAborted)
	}

	q := &pending{answer: make(chan *wire.ElicitReply, 1), alive: make(chan struct{}, 1)}
	p.pending[id] = q

	return q, nil
}

func (p *elicitPrompter) forget(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	delete(p.pending, id)
}

// close drops every pending question, so a task that has ended holds nothing and an
// answer arriving afterwards finds no question rather than a channel nobody reads.
func (p *elicitPrompter) close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.closed = true
	p.pending = map[string]*pending{}
}

// deliver hands an answer to whoever is waiting for it, and reports whether anything
// was. An answer to a question this task did not ask, or asked and gave up on, is
// reported to the sender rather than dropped silently.
func (p *elicitPrompter) deliver(reply *wire.ElicitReply) bool {
	p.mu.Lock()
	q, waiting := p.pending[reply.QuestionID]
	p.mu.Unlock()

	if !waiting {
		return false
	}

	select {
	case q.answer <- reply:
		return true
	default:
		// A second answer to one question. The first is the operator's, and this one is
		// told it arrived at nothing.
		return false
	}
}

// stillWaiting restarts the window on the question named, and reports whether this task
// is holding one by that name.
//
// A duplicate arriving while one is already queued is dropped and still reported as
// delivered, where deliver refuses a second answer. The two are opposite answers to the
// same full buffer, and each is what the sender needs: a refused ack would tell a caller
// its question is gone when it is wide open, and a refused answer says the first one
// won.
func (p *elicitPrompter) stillWaiting(questionID string) bool {
	p.mu.Lock()
	q, waiting := p.pending[questionID]
	p.mu.Unlock()

	if !waiting {
		return false
	}

	select {
	case q.alive <- struct{}{}:
	default:
	}

	return true
}

// handleElicitReply routes one inbound reply to the question it belongs to and answers
// the sender.
//
// The answer value is read before the reply is delivered, since AnswerWaiting is not an
// answer: it restarts the question's window and must never reach the channel the answer
// travels on, where it would be taken for the reply and fail the run.
//
// The reply to the sender is an ack rather than the run's own progress: it says the
// answer was delivered to a question this task is waiting on, which is what an
// answering caller needs to know before it stops holding its operator. A caller that
// keeps saying it is waiting reads the same ack as the evidence that this worker is
// still holding its question, the reply set being silent for as long as the hold lasts.
func (t *task) handleElicitReply(_ context.Context, _ a2a.Caller, body []byte, reply a2a.Replier) {
	msg, err := t.ch.inboundElicitReply(body)
	if err != nil {
		t.log.Warn("Refusing an answer", "error", err)
		_ = reply.Error("400", err.Error())

		return
	}

	var delivered bool

	switch {
	case t.prompter == nil:
	case msg.Answer == wire.AnswerWaiting:
		delivered = t.prompter.stillWaiting(msg.QuestionID)
	default:
		delivered = t.prompter.deliver(msg)
	}

	if !delivered {
		t.log.Warn("An answer reached no question", "question", msg.QuestionID)
		_ = reply.Error("404", fmt.Sprintf("this run is not waiting on question %q", msg.QuestionID))

		return
	}

	ack := wire.NewAck(true)
	wire.StampReply(&ack.Header, &msg.Header, t.ch.identity)

	data, err := json.Marshal(ack)
	if err != nil {
		t.log.Warn("Marshaling an answer ack failed", "error", err)
		_ = reply.Error("500", "marshaling the reply")

		return
	}

	err = reply.Respond(data)
	if err != nil {
		t.log.Warn("Answering an elicit reply failed", "error", err)
	}
}

// inboundElicitReply holds an answer to the same cap, schema and protocol rule as a
// request. The subject carries the request id, so anything else arriving there is
// refused rather than acted on.
//
// It names the six ids an answer arrives under rather than the elicit family, which also
// holds the four questions: those travel the other way, and admitting one here would put a
// message of another type on a path contracted for this one. These are a peer's bytes on
// a subject that authenticates nobody, so every one of them is checked.
func (c *Channel) inboundElicitReply(body []byte) (*wire.ElicitReply, error) {
	if len(body) > wire.MaxMessageSize {
		return nil, fmt.Errorf("the answer is %d bytes, over the %d byte limit", len(body), wire.MaxMessageSize)
	}

	err := c.validator.Validate(body)
	if err != nil {
		return nil, fmt.Errorf("the answer is not a valid v1 message: %w", err)
	}

	reply, err := wire.ExpectOneProtocol[*wire.ElicitReply](body, wire.ElicitAnswerProtocols())
	if err != nil {
		return nil, fmt.Errorf("the answer path carries answers to questions: %w", err)
	}

	return reply, nil
}
