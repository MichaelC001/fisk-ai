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

// The codes a terminal error carries, under local names so the call sites read as they
// did when the vocabulary lived here. It belongs to the protocol rather than to this
// endpoint, since a client decides what to do about an ending it did not produce.
const (
	codeRejected   = a2a.CodeRejected
	codeCapacity   = a2a.CodeCapacity
	codeDuplicate  = a2a.CodeDuplicate
	codeDraining   = a2a.CodeDraining
	codeFailed     = a2a.CodeFailed
	codeCrashed    = a2a.CodeCrashed
	codeNotStarted = a2a.CodeNotStarted
	codeDeferred   = a2a.CodeDeferred
	codeSuspended  = a2a.CodeSuspended
	codeCanceled   = a2a.CodeCanceled

	// The three endings a follow-up turn has that a first prompt does not, each with a
	// different answer for the caller: send the prompt as a first turn instead, send it
	// again once the conversation is free, and send it again once whatever the
	// conversation is waiting on has been answered.
	codeUnknownConversation = a2a.CodeUnknownConversation
	codeConversationBusy    = a2a.CodeConversationBusy
	codeTurnNotTaken        = a2a.CodeTurnNotTaken

	// The ending a conversation has once, permanently: its token allowance is spent, so
	// no caller gets a further turn out of it.
	codeBudgetExhausted = a2a.CodeBudgetExhausted

	// The endings an answer has. Each is permanent: the call it named is not one this
	// conversation can take an answer for, so sending it again reaches the same
	// answer.
	codeUnknownCall     = a2a.CodeUnknownCall
	codeAlreadyAnswered = a2a.CodeAlreadyAnswered
	codeAnswerTooLarge  = a2a.CodeAnswerTooLarge
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

	// stop is closed when the caller asks this run to stop. Closing it rather than
	// canceling the run's context is what makes a cancel a request for a boundary: the
	// loop polls it at the next one and parks somewhere the conversation can be
	// continued, instead of dying wherever it stood.
	//
	// It is a channel as well as a flag because a run blocked on a question is not at
	// a boundary and would otherwise sit there until the question's window ran out.
	stop     chan struct{}
	stopOnce sync.Once

	mu       sync.Mutex
	cancel   context.CancelFunc
	canceled bool
	ended    bool
}

// stopped reports whether a caller has asked this run to stop.
func (t *task) stopped() bool {
	select {
	case <-t.stop:
		return true
	default:
		return false
	}
}

// suspendRequested is what the run polls at each boundary: this caller's cancel, or
// the drain that takes the whole channel out of service. Either way the run parks
// where it can be resumed rather than ending mid-turn.
func (t *task) suspendRequested() bool { return t.stopped() || t.ch.draining() }

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

	// A follow-up is a prompt that adds a turn to a conversation it names. A prompt
	// carrying no token opens a conversation instead, and the other three kinds add no
	// turn at all: calling an answered question a turn would report every one of them as
	// a turn that was not taken.
	followUp := req.Kind == a2a.RequestPrompt && req.ConversationToken != ""

	t := &task{
		ch:       c,
		req:      req,
		stream:   stream,
		session:  SessionFor(c.identity, token),
		token:    token,
		followUp: followUp,
		stop:     make(chan struct{}),
		log:      log,
	}

	// A caller that names a conversation, asks for some of it back, and carries neither
	// a prompt nor an answer is asking to be told what the conversation holds. It takes
	// no turn and calls no model, so it is answered from the store here rather than
	// admitted against the worker count. It is what a client opens a resumed
	// conversation with, before there is anything to send.
	if t.reads() {
		c.serveRead(t)

		return
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
	accept.MaxTokens = t.effectiveMaxTokens()

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

// reads reports whether this request asks to be told what a conversation holds rather
// than to do anything to it.
//
// A caller that wants the conversation and then wants it continued sends two messages,
// since the two are different operations and one message cannot be both.
func (t *task) reads() bool {
	return t.req.Kind == a2a.RequestRead
}

// serveRead answers a read: the conversation as blocks, and a result that reports what
// it has consumed.
//
// It runs on the serving goroutine rather than through the worker, because there is no
// run to do. Nothing is admitted, so a read is answered while a turn of the same
// conversation is in flight, which is what a person opening a second terminal on a
// conversation expects and what a client needs when its own turn is already running.
func (c *Channel) serveRead(t *task) {
	if c.sessions == nil {
		t.log.Warn("Refusing to read a conversation back", "reason", "this worker holds no session store")
		t.refuse(codeRejected, "this worker cannot read a stored conversation back")

		return
	}

	accept := a2a.NewAck(true)
	accept.ConversationToken = t.token
	accept.MaxTokens = t.effectiveMaxTokens()

	err := t.stream.Ack(accept)
	if err != nil {
		t.log.Warn("Accepting a read failed", "error", err)
		t.end()

		return
	}

	rs, err := c.sessions.Load(t.session)
	if err != nil {
		if errors.Is(err, runstate.ErrNotFound) || errors.Is(err, runstate.ErrInvalidID) {
			// The same ending a follow-up gets for the same cause, and for the same
			// reason: the token named no conversation this worker holds.
			t.terminate(serve.Outcome{}, codeUnknownConversation, "no conversation here is named by that token")

			return
		}

		t.terminate(serve.Outcome{}, codeFailed, "reading the stored conversation failed")

		return
	}

	// The same sink a run replays through, so a conversation read back and a
	// conversation resumed arrive as the same blocks bracketed by the same markers.
	sink := &eventSink{stream: t.stream, log: t.log, replay: t.req.Replay}
	sink.ResumeTranscript(rs)

	t.log.Info("Read a stored conversation back", "session", t.session, "asked", t.req.Replay)

	res := a2a.NewResult(a2a.StopEndTurn)
	res.Usage = a2a.UsageFromCounters(rs.Counters)

	t.finish(func() error { return t.stream.Result(res) })
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

	msg, err := a2a.ExpectOneProtocol(body, a2a.RequestProtocols())
	if err != nil {
		return nil, fmt.Errorf("this path carries requests: %w", err)
	}

	req, ok := msg.(*a2a.Request)
	if !ok {
		return nil, fmt.Errorf("this path carries requests, not %T", msg)
	}

	if req.Kind == a2a.RequestAnswer {
		err = checkAnswer(req)
		if err != nil {
			return nil, err
		}
	}

	return req, nil
}

// checkAnswer decides whether an answer can be acted on at all, before a task is built
// around it. It is the one rule the schema does not hold, the answer being a nested object
// that carries its own kind and answer rather than a message with an id.
//
// Everything here is about the message; whether the call it names is one this conversation
// is waiting on is the run's to answer, since only the journal knows.
func checkAnswer(req *a2a.Request) error {
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
	//
	// The token rides along on the turn that creates the journal, and only there, so a
	// caller that lost it can be handed it back and an operator can say which stored
	// conversation is which. The run records it; nothing else reads it.
	// Force belongs to every shape that resumes and to none that creates: a caller
	// asking to continue across a configuration the conversation no longer matches is
	// answering for its own conversation, and the run drops the approvals it cannot
	// vouch for.
	checkpoint := agent.Checkpoint{ResumeID: t.session, CreateIfMissing: true, ConversationToken: t.token}

	switch {
	case t.req.Kind == a2a.RequestAnswer:
		// An answer resumes the conversation and adds no turn to it. A call that
		// deferred takes the answer as its result, since it is never dispatched again;
		// an approval needs nothing here, the resume dispatching the call it guards and
		// the gate asking again, which t.prompter answers from the same answer.
		checkpoint = agent.Checkpoint{ResumeID: t.session, Answer: t.answerFor(), Force: t.req.Force}

	case t.followUp:
		checkpoint = agent.Checkpoint{ResumeID: t.session, FollowUp: true, Force: t.req.Force}

	case t.req.Kind == a2a.RequestResume:
		// Continue a run that stopped part way, which is what a caller sends after a
		// suspend and what the terminal's own resume has always done.
		checkpoint = agent.Checkpoint{ResumeID: t.session, Force: t.req.Force}
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
		// The next turn is whoever holds the conversation token's to send, so the gap
		// before this history is used again is their pace rather than a loop's.
		HumanPaced: true,
		// A cancel from this caller and a drain of the whole channel both ask the run
		// for a boundary, and the loop polls this at each one.
		SuspendRequested: t.suspendRequested,
		RunContext:       t.runContext,
		Done:             t.done,
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

	// Replay is asked for per request rather than declared once, so a caller opening a
	// conversation gets its history and every turn after that gets none. It has no
	// meaning on a first turn, which has no history to send.
	replay := 0
	if t.req.ConversationToken != "" {
		replay = t.req.Replay
	}

	// Fragments are asked for per request as replay is, and a caller that said nothing
	// gets none, so a client that renders a turn as it is written pays for them and one
	// that reads the whole blocks does not.
	return &eventSink{stream: t.stream, log: t.log, replay: replay, deltas: t.req.WantsDeltas()}
}

// runContext derives the context the run executes under: the caller's trace joined so
// this run's spans sit under the span that asked for the work, and a cancel this task
// keeps so the run is released when it ends.
//
// A caller's cancel does not reach here. It asks for a boundary rather than a stop, so
// it closes t.stop and the loop parks at its next one, which for a cancel that arrived
// before the first model call is before that call.
func (t *task) runContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(telemetry.ContextWithRemoteTrace(parent, telemetry.TraceContext{TraceParent: t.req.TraceParent}))

	t.mu.Lock()
	t.cancel = cancel
	t.mu.Unlock()

	return ctx, cancel
}

// handleCancel asks the run this task is holding to stop at its next boundary, and
// answers the cancel.
//
// It does not cancel the run's context. A caller asking to stop is asking for a
// conversation it can continue rather than a turn that ended half done, so the run
// finishes the step in hand, parks where a resume can pick it up, and is answered
// suspended. Stopping a run where it stands stays with the operator of this worker.
//
// The ack is a reply to a plain subscription rather than a message of the reply set, so
// it is stamped as a single reply and never touches the ReplyStream, which belongs to
// the run. Canceling a task that has already ended changes nothing, which is what the
// caller is told: the cancel was received.
func (t *task) handleCancel(_ context.Context, _ a2a.Caller, body []byte, reply a2a.Replier) {
	msg, err := t.ch.inboundCancel(body)
	if err != nil {
		t.log.Warn("Refusing a cancel", "error", err)
		_ = reply.Error("400", err.Error())

		return
	}

	t.mu.Lock()
	t.canceled = true
	t.mu.Unlock()

	// A question in flight is not a boundary, so the run would sit on it until the
	// window ran out. Closing this both asks for the boundary and gives up the
	// question, which defers or leaves its call unanswered exactly as silence does.
	t.stopOnce.Do(func() { close(t.stop) })

	t.log.Info("A caller asked a run to stop", "reason", msg.Reason)

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
// The order settles the cases that overlap. A deferred call and a drain both suspend,
// and the deferred list separates them. A follow-up that was not taken is answered
// before the deferral that stopped it, since what the caller does about it is send its
// own prompt again rather than answer the call.
//
// A caller's cancel is not one of these. It asks for a boundary, so the run it stopped
// ends suspended and is answered as such, with a session to continue from. What still
// reports canceled is the worker stopping a run under its caller, which reaches here as
// a context error and no terminal reason.
func (t *task) disposition(out serve.Outcome) (string, string) {
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

	case out.Reason == "" && errors.Is(out.Err, context.Canceled):
		// Not this caller's doing: an operator stopped this worker rather than draining
		// it, so the run ended where it stood and left whatever the journal holds.
		return codeCanceled, "the worker stopped the run"

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

	case out.Reason == runstate.ReasonBudget:
		// Above the follow-up case below, which would otherwise claim every budget
		// refusal was a conversation waiting on a deferred tool result: a refused turn
		// leaves FollowUpTaken false for both reasons and only one of them is worth
		// retrying. This ending is permanent for the conversation, so it says so rather
		// than inviting the caller to send the prompt again.
		t.log.Info("A conversation reached its token budget", "session", t.session)

		return codeBudgetExhausted, out.Err.Error() + "; it will take no further turn, so continue in a new conversation or raise the budget where the agent runs"

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

// claimEnding reports whether this call is the one that ends the task. A second ending
// publishes nothing, since it would write into a set the caller has stopped reading.
func (t *task) claimEnding() bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.ended {
		return false
	}
	t.ended = true

	return true
}

// finish ends a task that answers for itself, releasing what it holds afterwards. It is
// terminateWith without an outcome, for the read that has no run behind it to describe.
func (t *task) finish(send func() error) {
	if !t.claimEnding() {
		return
	}

	defer t.end()

	err := send()
	if err != nil {
		t.log.Warn("Ending a read failed", "error", err)
	}
}

// terminateWith sends an optional ack, then the terminal message, then gives back the
// slot and the cancel subscription. It runs once per task: a second call after the
// stream has ended would publish into a set the caller stopped reading.
func (t *task) terminateWith(ack func() error, out serve.Outcome, code, reason string) {
	if !t.claimEnding() {
		return
	}

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
		res.TraceID = traceOf(out)
		res.ContentExported = contentExported(out)

		return t.stream.Result(res)
	}

	t.log.Info("Ending a run", "session", t.session, "code", code, "reason", reason)

	msg := a2a.NewError(trimForWire(reason))
	msg.Code = code
	msg.StopReason = terminalStopReason(out, code)
	// What the run spent before it ended. A suspended one did the work of a turn and
	// is answered here rather than with a result, so without this a caller cannot tell
	// what it owes for the turn it is about to continue.
	msg.Usage = a2a.UsageFrom(out.Stats)
	// An ending that was not an answer is where a caller most wants somewhere to go
	// and look, and the trace is the only thing this message can point at.
	msg.TraceID = traceOf(out)
	msg.ContentExported = contentExported(out)

	return t.stream.Error(msg)
}

// contentExported reports whether this turn's conversation itself left the worker. The
// card says what the worker is configured to do; this says what the turn did.
func contentExported(out serve.Outcome) bool {
	return out.Stats != nil && out.Stats.ContentExported
}

// traceOf is the trace the run recorded, or nothing when the worker exports no
// telemetry or the run never got far enough to open a span.
func traceOf(out serve.Outcome) string {
	if out.Stats == nil {
		return ""
	}

	return out.Stats.TraceID
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

// effectiveMaxTokens is the cumulative token bound this turn will actually be held to,
// which is the lower of the configured ceiling and whatever the request asked for.
//
// It is computed here rather than read back from the server because the ack goes out
// before the run starts, and it reports the effective value rather than the configured
// one so a caller that lowered its own budget is told what it will be held to instead of
// a larger number it will never reach.
func (t *task) effectiveMaxTokens() int64 {
	local := t.ch.maxTokens

	asked := budgetOf(t.req).MaxTokens
	if asked <= 0 {
		return local
	}
	if local <= 0 || asked < local {
		return asked
	}

	return local
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

// SessionFor is the journal a conversation token runs in: the hash of the token under
// the serving identity, prefixed so an operator reading a session list sees which
// surface a journal came from.
//
// The token is hashed rather than used as the key so the only journals a caller can
// reach are the ones this channel minted a token for. A session id is not a secret: it
// is logged, it is in the terminal message a deferred run sends, and the queue channel
// takes a submitter-chosen one as the journal to resume. Handing the store a caller's
// bytes directly would put every one of those journals within reach of anyone who
// learned an id.
//
// It is exported for a caller that also holds the store, which is a terminal talking
// to a worker it started itself: it names the journal its own token reaches, so it can
// print an id an operator can look up and read the conversation back without asking
// anyone. It says nothing about a journal existing.
func SessionFor(identity, token string) string {
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
