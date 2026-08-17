//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2aendpoint

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/choria-io/fisk-ai/internal/a2a"
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
// It holds a question open until the context it is called on ends. How long that is
// belongs to the server: serve bounds a question by Work.PromptWait, which this channel
// fills from expose.agent.a2a.request_timeout.
type elicitPrompter struct {
	task *task

	mu      sync.Mutex
	pending map[string]chan *a2a.ElicitReply
	closed  bool
}

func newElicitPrompter(t *task) *elicitPrompter {
	return &elicitPrompter{task: t, pending: map[string]chan *a2a.ElicitReply{}}
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
	ask := a2a.NewElicitRequest(a2a.ElicitApprove, a2a.NewID())
	ask.Command = req.Command
	ask.Display = req.Display
	ask.Tag = req.Tag

	reply, err := p.ask(ctx, ask, unansweredAborts)
	if err != nil {
		return toolkit.ConfirmNo, err
	}

	switch reply.Answer {
	case a2a.AnswerNoOperator:
		return toolkit.ConfirmNo, errNoOperatorThere

	case a2a.AnswerChoice:
		switch reply.Choice {
		case a2a.ChoiceAlways:
			return toolkit.ConfirmAlways, nil
		case a2a.ChoiceOnce:
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
	ask := a2a.NewElicitRequest(a2a.ElicitConfirm, a2a.NewID())
	ask.Question = question

	reply, err := p.ask(ctx, ask, unansweredDefers)
	if err != nil {
		return false, err
	}

	switch reply.Answer {
	case a2a.AnswerNoOperator:
		return false, errNoOperatorThere
	case a2a.AnswerConfirmed:
		return reply.Confirmed, nil
	default:
		return false, fmt.Errorf("the caller answered a confirmation with %q", reply.Answer)
	}
}

// Select asks the caller to choose one of options and returns its index. An index
// outside the options is a choice nobody made, reported as such rather than clamped.
func (p *elicitPrompter) Select(ctx context.Context, question string, options []string) (int, error) {
	ask := a2a.NewElicitRequest(a2a.ElicitSelect, a2a.NewID())
	ask.Question = question
	ask.Options = options

	reply, err := p.ask(ctx, ask, unansweredDefers)
	if err != nil {
		return -1, err
	}

	switch reply.Answer {
	case a2a.AnswerNoOperator:
		return -1, errNoOperatorThere

	case a2a.AnswerIndex:
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
	ask := a2a.NewElicitRequest(a2a.ElicitInput, a2a.NewID())
	ask.Question = question
	ask.Default = def

	reply, err := p.ask(ctx, ask, unansweredDefers)
	if err != nil {
		return "", err
	}

	switch reply.Answer {
	case a2a.AnswerNoOperator:
		return "", errNoOperatorThere
	case a2a.AnswerValue:
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

// ask publishes one question and waits for the answer that matches it.
//
// The pending entry is registered before the question is published, so an answer
// arriving while the publish is still returning has somewhere to land rather than being
// dropped as belonging to no question.
func (p *elicitPrompter) ask(ctx context.Context, question *a2a.ElicitRequest, onSilence unanswered) (*a2a.ElicitReply, error) {
	answers, err := p.register(question.QuestionID)
	if err != nil {
		return nil, err
	}
	defer p.forget(question.QuestionID)

	err = p.task.stream.Elicit(question)
	if err != nil {
		return nil, fmt.Errorf("putting the question to the caller: %w", err)
	}

	p.task.log.Info("Asked the caller a question", "question", question.QuestionID, "kind", question.Kind)

	select {
	case reply := <-answers:
		return reply, nil

	case <-ctx.Done():
		p.task.log.Info("A question went unanswered", "question", question.QuestionID, "cause", ctx.Err())

		if onSilence == unansweredDefers {
			return nil, toolkit.DeferResult(fmt.Sprintf("waiting on the caller to answer question %s", question.QuestionID), question.QuestionID)
		}

		return nil, fmt.Errorf("%w: the caller did not answer: %w", toolkit.ErrPromptAborted, ctx.Err())
	}
}

// register claims the question id, so the answer to it reaches the goroutine waiting.
// The channel is buffered, so a handler never blocks on a waiter that has already given
// up.
func (p *elicitPrompter) register(id string) (chan *a2a.ElicitReply, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil, fmt.Errorf("%w: the run ended", toolkit.ErrPromptAborted)
	}

	answers := make(chan *a2a.ElicitReply, 1)
	p.pending[id] = answers

	return answers, nil
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
	p.pending = map[string]chan *a2a.ElicitReply{}
}

// deliver hands an answer to whoever is waiting for it, and reports whether anything
// was. An answer to a question this task did not ask, or asked and gave up on, is
// reported to the sender rather than dropped silently.
func (p *elicitPrompter) deliver(reply *a2a.ElicitReply) bool {
	p.mu.Lock()
	answers, waiting := p.pending[reply.QuestionID]
	p.mu.Unlock()

	if !waiting {
		return false
	}

	select {
	case answers <- reply:
		return true
	default:
		// A second answer to one question. The first is the operator's, and this one is
		// told it arrived at nothing.
		return false
	}
}

// handleElicitReply routes one inbound answer to the question it belongs to and answers
// the sender.
//
// The reply to the sender is an ack rather than the run's own progress: it says the
// answer was delivered to a question this task is waiting on, which is what an
// answering caller needs to know before it stops holding its operator.
func (t *task) handleElicitReply(_ context.Context, _ a2a.Caller, body []byte, reply a2a.Replier) {
	msg, err := t.ch.inboundElicitReply(body)
	if err != nil {
		t.log.Warn("Refusing an answer", "error", err)
		_ = reply.Error("400", err.Error())

		return
	}

	if t.prompter == nil || !t.prompter.deliver(msg) {
		t.log.Warn("An answer reached no question", "question", msg.QuestionID)
		_ = reply.Error("404", fmt.Sprintf("this run is not waiting on question %q", msg.QuestionID))

		return
	}

	ack := a2a.NewAck(true)
	a2a.StampReply(&ack.Header, &msg.Header, t.ch.identity)

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
func (c *Channel) inboundElicitReply(body []byte) (*a2a.ElicitReply, error) {
	if len(body) > a2a.MaxMessageSize {
		return nil, fmt.Errorf("the answer is %d bytes, over the %d byte limit", len(body), a2a.MaxMessageSize)
	}

	err := c.validator.Validate(body)
	if err != nil {
		return nil, fmt.Errorf("the answer is not a valid v1 message: %w", err)
	}

	msg, err := a2a.ExpectProtocol(body, a2a.ElicitReplyProtocol)
	if err != nil {
		return nil, fmt.Errorf("the answer path carries %s messages: %w", a2a.ElicitReplyProtocol, err)
	}

	return msg.(*a2a.ElicitReply), nil
}
