//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2asurface

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/serve"
	"github.com/choria-io/fisk-ai/internal/telemetry"
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
	ch      *Channel
	req     *a2a.Request
	stream  *a2a.ReplyStream
	watch   a2a.CancelWatch
	session string
	log     *slog.Logger

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

	t := &task{
		ch:      c,
		req:     req,
		stream:  stream,
		session: a2a.NewID(),
		log:     log,
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

	err = stream.Ack(true, "")
	if err != nil {
		// Nobody to tell: the ack is the first thing this worker says, so a caller that
		// did not receive it has no reply set to be told anything else on.
		log.Warn("Accepting a prompt failed", "error", err)
		t.end()

		return
	}

	log.Info("Accepted a prompt", "session", t.session, "prompt_bytes", len(req.Prompt))

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
	if req.Prompt == "" {
		return nil, fmt.Errorf("the request carries no prompt")
	}

	return req, nil
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

	c.inFlight[t.req.Request] = t

	return "", ""
}

// release gives the slot back and forgets the request id, so the next caller is
// admitted and the id can be used again.
func (c *Channel) release(t *task) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.inFlight, t.req.Request)
}

// work is the unit the server runs, with the attachment points this channel can supply.
//
// Fields the request carries that Work has no home for are dropped, and deliberately:
// tool_hints, budget.call_timeout, Header.Parent and Header.Recipient.
// Header.Conversation is dropped with the most care, since it is a caller-chosen string
// meaning "session" and must never reach Checkpoint, where it would let one caller name
// another's journal.
//
// Prompter stays nil, so the server refuses every confirmation-gated tool: this channel
// has nobody to ask. Continue stays nil, so a task is one shot.
func (t *task) work(caller a2a.Caller) *serve.Work {
	return &serve.Work{
		// The minted session rather than the caller's request id: the id is a caller's
		// to choose and this names the work in a worker's logs beside jobs from a queue.
		ID:      t.session,
		Prompt:  t.req.Prompt,
		Context: t.req.Context,
		// A journal per task, so a crash leaves a resumable run and a tool that answers
		// later has somewhere for its answer to land. Nothing resumes it in this
		// release: a fresh id per task means there is never an existing journal to
		// answer from.
		Checkpoint: agent.Checkpoint{
			ResumeID:        t.session,
			CreateIfMissing: true,
		},
		// The caller's request id, which greps across this worker's logs and the caller's
		// own record of what it asked for.
		ClaimedBy:  t.req.Request,
		Budget:     budgetOf(t.req),
		Caller:     callerOf(caller, t.req),
		Events:     t.events(),
		RunContext: t.runContext,
		Done:       t.done,
	}
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
// deferred call and a drain both suspend, and the deferred list separates them.
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

	case len(out.Deferred) > 0:
		// Nothing wakes it in this release. The ids travel to the caller and to the log
		// because the answer is supplied against the session on the worker that holds
		// the journal, which is an operator's job rather than the caller's.
		ids := deferredIDs(out.Deferred)
		t.log.Info("A run is waiting on a deferred tool result", "session", t.session, "deferred", ids)

		return codeDeferred, fmt.Sprintf("the run is waiting on a deferred tool result; answer session %s tool_use %s on this worker", t.session, ids)

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
	t.terminateWith(func() error { return t.stream.Ack(false, reason) }, serve.Outcome{}, code, reason)
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

// end releases the cancel subscription and the slot. It runs on every ending, including
// the ones that never got as far as a terminal message.
func (t *task) end() {
	if t.watch != nil {
		err := t.watch.Close()
		if err != nil {
			t.log.Warn("Releasing a run's cancel watch failed", "error", err)
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
