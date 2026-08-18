//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2aendpoint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/serve"
	"github.com/choria-io/fisk-ai/internal/telemetry"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/toolkit/builtin"
)

// The codes a terminal error carries, so a caller decides on a value rather than on
// prose. A refusal names why it was refused; an ending names how the run ended.
const (
	codeRejected   = "rejected"
	codeCapacity   = "capacity"
	codeDuplicate  = "duplicate_request"
	codeDraining   = "draining"
	codeFailed     = "failed"
	codeCrashed    = "crashed"
	codeNotStarted = "not_started"
	codeDeferred   = "deferred"
	codeSuspended  = "suspended"
	codeCanceled   = "canceled"

	// The three endings a follow-up turn has that a first prompt does not, each with a
	// different answer for the caller: send the prompt as a first turn instead, send it
	// again once the conversation is free, and send it again once whatever the
	// conversation is waiting on has been answered.
	codeUnknownConversation = "unknown_conversation"
	codeConversationBusy    = "conversation_busy"
	codeTurnNotTaken        = "turn_not_taken"

	// The endings an answer has. Each is permanent: the call it named is not one this
	// conversation can take an answer for, so sending it again reaches the same
	// answer.
	codeUnknownCall     = "unknown_call"
	codeAlreadyAnswered = "already_answered"
	codeAnswerTooLarge  = "answer_too_large"
)

// task is one accepted request: the reply set it answers on, the cancel addressed to
// it, and the state its ending needs.
//
// The reply stream is owned by one goroutine at a time. The ack is sent on the serving
// goroutine, the events by the run, and the terminal message by whichever goroutine
// reports the outcome, each handing over to the next through the channel it travels on.
// The cancel below is the exception and is guarded, being the one thing that arrives
// while the run is in progress.
type task struct {
	ch     *Channel
	req    *a2a.Request
	stream *a2a.ReplyStream
	watch  a2a.TaskWatch
	log    *slog.Logger

	// session is the journal this task runs in, derived from the token. token is the
	// handle the caller holds, minted here on a first turn and echoed on a follow-up,
	// and followUp says which of the two this is.
	//
	// The token is a credential: holding it is the authorization to add a turn to this
	// conversation, so it stays out of the log lines and out of the terminal messages
	// that name the session.
	session  string
	token    string
	followUp bool

	// answers routes the caller's replies to the questions this run asked, and
	// prompter is what the run puts them through. Both are nil when elicitation is
	// off, which leaves the server substituting its deny prompter.
	answers  a2a.TaskWatch
	prompter *elicitPrompter

	mu       sync.Mutex
	cancel   context.CancelFunc
	canceled bool
	ended    bool
}

// handle admits one inbound request and returns, leaving the run to the server.
//
// A refusal before the ack answers as a transport error, since nothing was accepted and
// there is no reply set to end. Every refusal after it is an ack that says no followed
// by a terminal message, because the ack does not close the set and a caller holding
// only a refusing ack would wait for a terminal message to its own deadline.
func (c *Channel) handle(_ context.Context, caller a2a.Caller, body []byte, reply a2a.Replier) {
	req, err := c.intake(body)
	if err != nil {
		c.log.Warn("Refusing a prompt", "error", err, "caller", caller.Name, "caller_verified", caller.Verified)
		_ = reply.Error("400", err.Error())

		return
	}

	sink, ok := reply.(a2a.StreamReplier)
	if !ok {
		c.log.Error("The transport declared it streams but supplied a single-reply sink", "request", req.Request)
		_ = reply.Error("500", "this worker cannot stream a reply set for the request")

		return
	}

	log := c.log.With("request", req.Request, "caller", callerName(caller, req))
	stream := a2a.NewReplyStream(sink, &req.Header, c.identity)

	// A request carrying a token continues the conversation that token names; one
	// carrying none starts a conversation and is handed a token for it. So a caller
	// declares nothing on a first turn and decides per turn afterwards.
	token := req.ConversationToken
	if token == "" {
		token = a2a.NewID()
	}

	// A follow-up is a request that adds a turn to a conversation it names. One that
	// answers a question names a conversation too and adds no turn, so it is not one:
	// calling it one would report every answered question as a turn that was not
	// taken.
	followUp := req.ConversationToken != "" && req.Answer == nil

	t := &task{
		ch:       c,
		req:      req,
		stream:   stream,
		session:  sessionFor(c.identity, token),
		token:    token,
		followUp: followUp,
		log:      log,
	}

	code, reason := c.admit(t)
	if code != "" {
		log.Warn("Refusing a prompt", "code", code, "reason", reason)
		t.refuse(code, reason)

		return
	}

	// Subscribed before the ack, so a cancel cannot arrive at nobody: a caller has no
	// reason to cancel a task it has not been told was accepted, which closes the window
	// rather than bounding it.
	t.watch, err = c.stream.WatchCancel(req.Request, t.handleCancel)
	if err != nil {
		c.release(t)
		log.Warn("Refusing a prompt whose cancels could not be routed", "error", err)
		t.refuse(codeRejected, "the request id cannot be watched for cancels")

		return
	}

	// The answers to this run's questions are routed from here for the same reason and
	// on the same terms, since a question can be asked at any point in the run.
	if c.elicits {
		t.prompter = newElicitPrompter(t)

		t.answers, err = c.stream.WatchElicitReplies(req.Request, t.handleElicitReply)
		if err != nil {
			log.Warn("Refusing a prompt whose answers could not be routed", "error", err)
			t.refuse(codeRejected, "the request id cannot be watched for answers")

			return
		}
	}

	// The ack carries the token on every acceptance, the minted one on a first turn and
	// the one it accepted on a follow-up, so a caller reads back which conversation it is
	// on rather than assuming its token was understood.
	accept := a2a.NewAck(true)
	accept.ConversationToken = t.token

	err = stream.Ack(accept)
	if err != nil {
		// Nobody to tell: the ack is the first thing this worker says, so a caller that
		// did not receive it has no reply set to be told anything else on.
		log.Warn("Accepting a prompt failed", "error", err)
		t.end()

		return
	}

	log.Info("Accepted a prompt", "session", t.session, "conversation", req.Conversation, "follow_up", followUp, "prompt_bytes", len(req.Prompt))

	work := t.work(caller)

	c.handoffs.Go(func() {
		select {
		case c.work <- work:
		case <-c.shutdown:
			log.Info("Ending a prompt accepted while the worker was stopping")
			t.terminate(serve.Outcome{}, codeDraining, "the worker is shutting down")
		}
	})
}

// intake decides whether a body can be run at all. Its error reaches the caller, so it
// names what is wrong with the request and nothing about this worker.
func (c *Channel) intake(body []byte) (*a2a.Request, error) {
	if len(body) > a2a.MaxMessageSize {
		return nil, fmt.Errorf("the request is %d bytes, over the %d byte limit", len(body), a2a.MaxMessageSize)
	}

	err := c.validator.Validate(body)
	if err != nil {
		return nil, fmt.Errorf("the request is not a valid v1 message: %w", err)
	}

	msg, err := a2a.ExpectProtocol(body, a2a.RequestProtocol)
	if err != nil {
		return nil, fmt.Errorf("this path carries %s messages: %w", a2a.RequestProtocol, err)
	}

	req := msg.(*a2a.Request)

	// A request either asks for something or answers a question, and the two are
	// separate operations on the conversation: one adds a turn and pays for it, the
	// other finishes a turn that is already there. Carrying both would leave the
	// worker to decide which the caller meant.
	switch {
	case req.Prompt == "" && req.Answer == nil:
		return nil, fmt.Errorf("the request carries neither a prompt nor an answer")
	case req.Prompt != "" && req.Answer != nil:
		return nil, fmt.Errorf("the request carries both a prompt and an answer; send one or the other")
	}

	if req.Answer != nil {
		err = checkAnswer(req)
		if err != nil {
			return nil, err
		}
	}

	return req, nil
}

// checkAnswer decides whether an answer can be acted on at all, before a task is built
// around it. Everything here is about the message; whether the call it names is one
// this conversation is waiting on is the run's to answer, since only the journal knows.
func checkAnswer(req *a2a.Request) error {
	if req.ConversationToken == "" {
		return fmt.Errorf("an answer needs the conversation_token of the conversation that asked")
	}

	a := req.Answer
	if a.ToolUseID == "" {
		return fmt.Errorf("the answer names no tool call")
	}

	switch a.Kind {
	case a2a.ElicitApprove, a2a.ElicitConfirm, a2a.ElicitSelect, a2a.ElicitInput:
	default:
		return fmt.Errorf("%q is not a question this agent asks", a.Kind)
	}

	// The answer value has to fit the question, since each kind reaches a different
	// tool and an answer of the wrong shape would reach the model as one the operator
	// never gave.
	switch {
	case a.Answer == a2a.AnswerNoOperator:
	case a.Kind == a2a.ElicitApprove && a.Answer != a2a.AnswerChoice:
		return fmt.Errorf("an approval is answered with a choice, not with %q", a.Answer)
	case a.Kind == a2a.ElicitConfirm && a.Answer != a2a.AnswerConfirmed:
		return fmt.Errorf("a confirmation is answered with confirmed, not with %q", a.Answer)
	case (a.Kind == a2a.ElicitSelect || a.Kind == a2a.ElicitInput) && a.Answer != a2a.AnswerValue:
		return fmt.Errorf("a %s question is answered with a value, not with %q", a.Kind, a.Answer)
	}

	return nil
}

// admit decides whether this worker takes the task, reserving its slot when it does. It
// returns the code and the reason of a refusal, and empty strings when the task was
// admitted.
//
// Capacity is a refusal rather than a queue. A caller is waiting on the other end, so
// telling it now that this worker is full lets it retry, ask another instance or give
// up, where an acceptance it cannot see the depth behind would leave it watching a
// stream that has not started.
//
// A request id already in flight is refused because the id addresses the cancel
// subscription: two tasks sharing one would both hear a cancel meant for one of them,
// and the ack a caller reads back would come from whichever answered first.
//
// A second turn of a conversation this worker is already running is refused for a
// different reason: both turns would resume one journal, and the second to claim it
// takes it while the first fails at its next append. The caller is told to wait for the
// terminal message of the turn it already sent rather than to try elsewhere, since a
// sibling worker would take the conversation the same way.
func (c *Channel) admit(t *task) (string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	select {
	case <-c.shutdown:
		return codeDraining, "the worker is shutting down"
	default:
	}

	if len(c.inFlight) >= c.workers {
		return codeCapacity, fmt.Sprintf("this worker is running its maximum of %d prompts", c.workers)
	}

	_, running := c.inFlight[t.req.Request]
	if running {
		return codeDuplicate, "a run with this request id is already in flight here"
	}

	for _, other := range c.inFlight {
		if other.session == t.session {
			return codeConversationBusy, "a turn of this conversation is running here; wait for its terminal message before sending another"
		}
	}

	c.inFlight[t.req.Request] = t

	return "", ""
}

// release gives the slot back and forgets the request id, so the next caller is
// admitted and the id can be used again.
//
// Only the task holding the slot releases it. Every ending runs through here, including
// the endings of a task that was never admitted, and a request refused for reusing an id
// that is already running carries that id: deleting on the id alone would free the
// running task's slot and forget it, so the worker would over-admit and stop refusing
// that id. It also makes a second release a no-op, which the cancel-watch failure path
// performs.
func (c *Channel) release(t *task) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.inFlight[t.req.Request] == t {
		delete(c.inFlight, t.req.Request)
	}
}

// work is the unit the server runs, with the attachment points this channel can supply.
//
// Fields the request carries that Work has no home for are dropped, and deliberately:
// tool_hints, budget.call_timeout, Header.Parent and Header.Recipient.
// Header.Conversation is dropped with the most care. It is a caller-chosen string that
// means correlation, echoed on every reply and interpreted by nothing, and it must never
// reach Checkpoint, where it would let one caller name another's journal. What names a
// journal here is the token this worker minted, hashed.
//
// Prompter is set only when elicitation is on. Nil leaves the server substituting its
// deny prompter, which refuses every confirmation-gated tool: with the key off this
// channel has nobody to ask. Continue stays nil: a conversation holds no run between
// turns, so a follow-up arrives as another request rather than through a parked one.
//
// PromptsMayBlock is set and PromptWait is left unset, so the server bounds none of
// this run's questions and elicitPrompter bounds them itself. A caller with a person in
// front of a question restarts its window by saying so, which the server's fixed
// deadline cannot express; a caller that says nothing gives the worker back after one
// window, as it does now.
func (t *task) work(caller a2a.Caller) *serve.Work {
	// A first turn creates the journal the token names; a follow-up resumes it and its
	// prompt is the conversation's next turn. CreateIfMissing is what separates them and
	// a follow-up must not carry it: a token naming no journal is a caller's mistake to
	// hear about, not a conversation to invent under a name it chose.
	checkpoint := agent.Checkpoint{ResumeID: t.session, CreateIfMissing: true}

	switch {
	case t.req.Answer != nil:
		// An answer resumes the conversation and adds no turn to it. A call that
		// deferred takes the answer as its result, since it is never dispatched again;
		// an approval needs nothing here, the resume dispatching the call it guards and
		// the gate asking again, which t.prompter answers from the same answer.
		checkpoint = agent.Checkpoint{ResumeID: t.session, Answer: t.answerFor()}

	case t.followUp:
		checkpoint = agent.Checkpoint{ResumeID: t.session, FollowUp: true}
	}

	return &serve.Work{
		// The session rather than the caller's request id: the id is a caller's to choose
		// and this names the work in a worker's logs beside jobs from a queue.
		ID:         t.session,
		Prompt:     t.req.Prompt,
		Context:    t.req.Context,
		Checkpoint: checkpoint,
		// The caller's request id, which greps across this worker's logs and the caller's
		// own record of what it asked for.
		ClaimedBy:       t.req.Request,
		Budget:          budgetOf(t.req),
		Caller:          callerOf(caller, t.req),
		Events:          t.events(),
		Prompter:        t.promptsThrough(),
		PromptsMayBlock: true,
		RunContext:      t.runContext,
		Done:            t.done,
	}
}

// answerFor turns the answer a request carried into the result of the call that
// deferred, or nil for an approval, whose call has no result to supply.
//
// The worker renders it rather than the caller, because the shape is the one the
// deferring tool's own results take and the model was told to expect it. A caller
// building that shape itself would be copying this agent's internals, and a wrong
// shape reaches the model as an answer rather than as an error.
//
// A run whose elicitation is off holds the answer for nothing, which cannot happen:
// the question it answers could only have been asked with elicitation on.
func (t *task) answerFor() *agent.DeferredAnswer {
	a := t.req.Answer

	if a.Kind == a2a.ElicitApprove {
		if t.prompter != nil {
			t.prompter.hold(a.ToolUseID, approvalFrom(a))
		}

		return nil
	}

	content, err := renderAnswer(a)
	if err != nil {
		// Nothing here can refuse: intake checked the answer fits its question, so a
		// failure is this worker's own marshaling. The run is given no answer and ends
		// on the call it is still waiting for, which the caller can answer again.
		t.log.Error("Rendering a supplied answer failed", "tool_use", a.ToolUseID, "kind", a.Kind, "error", err)

		return nil
	}

	return &agent.DeferredAnswer{ToolUseID: a.ToolUseID, Content: content}
}

// approvalFrom reads an approval out of an answer. Anything that is not an explicit
// yes is a refusal, which is the direction the gate defaults in.
func approvalFrom(a *a2a.Answer) toolkit.ConfirmChoice {
	if a.Answer != a2a.AnswerChoice {
		return toolkit.ConfirmNo
	}

	switch a.Choice {
	case a2a.ChoiceAlways:
		return toolkit.ConfirmAlways
	case a2a.ChoiceOnce:
		return toolkit.ConfirmOnce
	default:
		return toolkit.ConfirmNo
	}
}

// renderAnswer produces the result the tool that asked would have returned.
func renderAnswer(a *a2a.Answer) (string, error) {
	if a.Answer == a2a.AnswerNoOperator {
		return builtin.NoAnswerResult(questionTool(a.Kind), "the caller has no operator to answer on its behalf")
	}

	switch a.Kind {
	case a2a.ElicitConfirm:
		return builtin.ConfirmResult(a.Confirmed, "")
	case a2a.ElicitSelect:
		return builtin.SelectResult(a.Value, "")
	case a2a.ElicitInput:
		return builtin.InputResult(a.Value, "")
	default:
		return "", fmt.Errorf("%q has no result shape", a.Kind)
	}
}

// questionTool names the built-in that asks each kind of question, which is what says
// the shape its result takes.
func questionTool(kind a2a.ElicitKind) string {
	switch kind {
	case a2a.ElicitConfirm:
		return builtin.AskHumanConfirmName
	case a2a.ElicitSelect:
		return builtin.AskHumanSelectName
	default:
		return builtin.AskHumanInputName
	}
}

// promptsThrough is the prompter the run puts its questions to, or nil when
// elicitation is off. It is a method rather than the field so a nil *elicitPrompter
// never reaches Work as a non-nil interface holding one.
func (t *task) promptsThrough() toolkit.Prompter {
	if t.prompter == nil {
		return nil
	}

	return t.prompter
}

// events is the sink that turns the run's narration into blocks on the reply set, or
// nil for a caller that asked for no stream. A caller that asked for none still gets
// the ack and the terminal message, which is the whole of what it asked for.
func (t *task) events() agent.Events {
	streams, err := a2a.AcceptStream(t.ch.stream, t.req)
	if err != nil || !streams {
		return nil
	}

	return &eventSink{stream: t.stream, log: t.log}
}

// runContext derives the context the run executes under: the caller's trace joined so
// this run's spans sit under the span that asked for the work, and a cancel this task
// keeps so a peer can stop it.
//
// A cancel that arrived before the run started is honored here. Between the ack and
// this call there is no context to cancel, so the run is entered on a canceled one and
// ends in setup rather than being started and stopped.
func (t *task) runContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(telemetry.ContextWithRemoteTrace(parent, telemetry.TraceContext{TraceParent: t.req.TraceParent}))

	t.mu.Lock()
	t.cancel = cancel
	canceled := t.canceled
	t.mu.Unlock()

	if canceled {
		cancel()
	}

	return ctx, cancel
}

// handleCancel stops the run this task is holding and answers the cancel.
//
// The ack is a reply to a plain subscription rather than a message of the reply set, so
// it is stamped as a single reply and never touches the ReplyStream, which belongs to
// the run. Canceling a task that has already ended is a no-op on a spent cancel func,
// which is what the caller is told: the cancel was received.
func (t *task) handleCancel(_ context.Context, _ a2a.Caller, body []byte, reply a2a.Replier) {
	msg, err := t.ch.inboundCancel(body)
	if err != nil {
		t.log.Warn("Refusing a cancel", "error", err)
		_ = reply.Error("400", err.Error())

		return
	}

	t.mu.Lock()
	t.canceled = true
	cancel := t.cancel
	t.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	t.log.Info("Canceling a run", "reason", msg.Reason)

	ack := a2a.NewAck(true)
	a2a.StampReply(&ack.Header, &msg.Header, t.ch.identity)

	data, err := json.Marshal(ack)
	if err != nil {
		t.log.Warn("Marshaling a cancel ack failed", "error", err)
		_ = reply.Error("500", "marshaling the reply")

		return
	}

	err = reply.Respond(data)
	if err != nil {
		t.log.Warn("Answering a cancel failed", "error", err)
	}
}

// inboundCancel holds a cancel to the same cap, schema and protocol rule as a request.
// The subject carries the request id, so anything else arriving there is refused rather
// than acted on.
func (c *Channel) inboundCancel(body []byte) (*a2a.Cancel, error) {
	if len(body) > a2a.MaxMessageSize {
		return nil, fmt.Errorf("the cancel is %d bytes, over the %d byte limit", len(body), a2a.MaxMessageSize)
	}

	err := c.validator.Validate(body)
	if err != nil {
		return nil, fmt.Errorf("the cancel is not a valid v1 message: %w", err)
	}

	msg, err := a2a.ExpectProtocol(body, a2a.CancelProtocol)
	if err != nil {
		return nil, fmt.Errorf("the cancel path carries %s messages: %w", a2a.CancelProtocol, err)
	}

	return msg.(*a2a.Cancel), nil
}

// done reports the run's outcome to the caller and ends the task. The server calls it
// exactly once, on a context of its own, so a run that was canceled or timed out still
// says what happened.
func (t *task) done(_ context.Context, out serve.Outcome) error {
	code, reason := t.disposition(out)
	t.terminate(out, code, reason)

	return nil
}

// disposition decides which ending the outcome earns and what the caller is told.
//
// The order settles the cases that overlap. A canceled run reports a context error and
// no terminal reason, so it is recognized before the outcome's own vocabulary; a
// deferred call and a drain both suspend, and the deferred list separates them. A
// follow-up that was not taken is answered before the deferral that stopped it, since
// what the caller does about it is send its own prompt again rather than answer the call.
func (t *task) disposition(out serve.Outcome) (string, string) {
	t.mu.Lock()
	canceled := t.canceled
	t.mu.Unlock()

	switch {
	case out.Rejected:
		return codeRejected, "the work was refused"

	case out.Crashed:
		// The panic and its stack stay in this worker's log. A peer is told the run
		// crashed and nothing about where.
		t.log.Error("A run crashed", "session", t.session, "error", out.Err)

		return codeCrashed, "the run crashed"

	case out.Abandoned:
		return codeNotStarted, "the work was taken but never started"

	case canceled:
		return codeCanceled, "the run was canceled"

	case errors.Is(out.Err, runstate.ErrNotDeferred):
		// The call is not one this conversation is waiting on: it was never deferred,
		// or the conversation has no turn in flight at all.
		return codeUnknownCall, "this conversation is not waiting on an answer for that call"

	case errors.Is(out.Err, runstate.ErrAlreadyAnswered):
		return codeAlreadyAnswered, "that call already has an answer"

	case errors.Is(out.Err, runstate.ErrResultTooLarge):
		return codeAnswerTooLarge, out.Err.Error()

	case errors.Is(out.Err, agent.ErrConversationNotFound):
		// The token named no journal here. Every worker of this identity reads one store,
		// so this is a token that was never minted, or one whose conversation the store no
		// longer holds.
		return codeUnknownConversation, "this conversation is not known here; send the prompt without a conversation token to start one"

	case t.followUp && !out.FollowUpTaken:
		// The conversation was waiting on a deferred tool result, so it reached no
		// boundary a user turn could join. The prompt was not journaled and not answered.
		t.log.Info("A follow-up turn was not taken", "session", t.session, "deferred", deferredIDs(out.Deferred))

		return codeTurnNotTaken, "the conversation is waiting on a deferred tool result and cannot take a turn; send the prompt again once it has been answered"

	case len(out.Deferred) > 0:
		// The ids travel to the caller because they are what an answer names, whether
		// it comes back on a request of its own or from an operator running
		// fisk-ai session against the worker holding the journal.
		ids := deferredIDs(out.Deferred)
		t.log.Info("A run is waiting on a deferred tool result", "session", t.session, "deferred", ids)

		return codeDeferred, fmt.Sprintf("the run is waiting on an answer for tool_use %s; send it on a request carrying this conversation's token", ids)

	case out.Reason == runstate.ReasonSuspended:
		return codeSuspended, "the run suspended and left a resumable session"

	case out.Reason == "":
		if out.Err != nil {
			return codeFailed, fmt.Sprintf("the run reached no outcome: %s", out.Err)
		}

		return codeFailed, "the run reported neither an outcome nor an error"

	case out.Err != nil:
		return codeFailed, out.Err.Error()
	}

	return "", ""
}

// refuse tells a caller its request was not taken: the ack that says no, then the
// terminal message that ends the set.
func (t *task) refuse(code, reason string) {
	t.terminateWith(func() error {
		refusal := a2a.NewAck(false)
		refusal.Reason = reason

		return t.stream.Ack(refusal)
	}, serve.Outcome{}, code, reason)
}

// terminate ends an accepted task with a result or an error and releases what it holds.
func (t *task) terminate(out serve.Outcome, code, reason string) {
	t.terminateWith(nil, out, code, reason)
}

// terminateWith sends an optional ack, then the terminal message, then gives back the
// slot and the cancel subscription. It runs once per task: a second call after the
// stream has ended would publish into a set the caller stopped reading.
func (t *task) terminateWith(ack func() error, out serve.Outcome, code, reason string) {
	t.mu.Lock()
	if t.ended {
		t.mu.Unlock()

		return
	}
	t.ended = true
	t.mu.Unlock()

	defer t.end()

	if ack != nil {
		err := ack()
		if err != nil {
			t.log.Warn("Sending a refusal failed", "error", err)

			return
		}
	}

	err := t.send(out, code, reason)
	if err != nil {
		// A caller left without a terminal message holds a stream that never ends, so
		// this is reported rather than dropped. There is nowhere to take it but the log.
		t.log.Warn("Ending a run failed", "error", err, "code", code)
	}
}

// send publishes the terminal message. A code names the failure a caller can decide on;
// an empty one is the successful answer.
func (t *task) send(out serve.Outcome, code, reason string) error {
	if code == "" {
		t.log.Info("Answering a prompt", "session", t.session, "reason", out.Reason)

		res := a2a.NewResult(a2a.StopReasonFor(out.Reason))
		res.Text = trimForWire(out.Text)
		res.Usage = a2a.UsageFrom(out.Stats)

		return t.stream.Result(res)
	}

	t.log.Info("Ending a run", "session", t.session, "code", code, "reason", reason)

	msg := a2a.NewError(trimForWire(reason))
	msg.Code = code
	msg.StopReason = terminalStopReason(out, code)

	return t.stream.Error(msg)
}

// end releases the subscriptions this task owns and the slot. It runs on every ending,
// including the ones that never got as far as a terminal message.
//
// The pending questions are dropped before the subscriptions go, so an answer that
// arrives in between is told it reached no question rather than being handed to a run
// that has finished.
func (t *task) end() {
	if t.prompter != nil {
		t.prompter.close()
	}

	if t.watch != nil {
		err := t.watch.Close()
		if err != nil {
			t.log.Warn("Releasing a run's cancel watch failed", "error", err)
		}
	}

	if t.answers != nil {
		err := t.answers.Close()
		if err != nil {
			t.log.Warn("Releasing a run's answer watch failed", "error", err)
		}
	}

	t.ch.release(t)
}

// terminalStopReason is the neutral reason a failed task carries. A run that reached
// one of its own reports it; one that was refused, canceled or never started reports
// what happened to it instead.
func terminalStopReason(out serve.Outcome, code string) a2a.StopReason {
	switch {
	case code == codeCanceled:
		return a2a.StopCanceled
	case out.Reason != "":
		return a2a.StopReasonFor(out.Reason)
	default:
		return a2a.StopError
	}
}

// budgetOf carries the limits a request may lower. The server clamps them against the
// local configuration, which stays the ceiling. A request's call_timeout is dropped:
// Work has nowhere to put it.
func budgetOf(req *a2a.Request) serve.Budget {
	if req.Budget == nil {
		return serve.Budget{}
	}

	return serve.Budget{
		MaxTokens:     req.Budget.MaxTokens,
		MaxIterations: req.Budget.MaxIterations,
	}
}

// callerOf reports who asked, keeping what the transport vouches for apart from what
// the body claims. On a binding that authenticates nobody the name is the sender's own
// claim and Verified is false, which records it as the unverified thing it is.
func callerOf(caller a2a.Caller, req *a2a.Request) serve.Caller {
	if caller.Verified {
		return serve.Caller{Name: caller.Name, Verified: true}
	}

	return serve.Caller{Name: req.Sender.Name}
}

// callerName is what a log line calls the caller: the verified principal when there is
// one, the body's claim when there is not, and "unknown" when the body claims nothing.
func callerName(caller a2a.Caller, req *a2a.Request) string {
	c := callerOf(caller, req)
	if c.Name == "" {
		return "unknown"
	}

	return c.Name
}

// sessionFor is the journal a conversation token runs in: the hash of the token under
// this identity, prefixed so an operator reading a session list sees which surface a
// journal came from.
//
// The token is hashed rather than used as the key so the only journals a caller can
// reach are the ones this channel minted a token for. A session id is not a secret: it
// is logged, it is in the terminal message a deferred run sends, and the queue channel
// takes a submitter-chosen one as the journal to resume. Handing the store a caller's
// bytes directly would put every one of those journals within reach of anyone who
// learned an id.
func sessionFor(identity, token string) string {
	sum := sha256.Sum256([]byte(identity + "\x00" + token))

	return "t-" + hex.EncodeToString(sum[:])
}

// deferredIDs renders the tool_use ids a run is waiting on. The note and the handle are
// left out: they are tool-supplied text and this string crosses the wire, so only the
// ids an answer is supplied against travel.
func deferredIDs(calls []agent.DeferredCall) string {
	ids := make([]string, len(calls))
	for i, c := range calls {
		ids[i] = c.ToolUseID
	}

	return strings.Join(ids, ",")
}
