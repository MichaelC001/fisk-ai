//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package asyncjobs

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/choria-io/asyncjobs"

	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/serve"
)

// handle is the engine's handler for one job, and the whole of the push-to-pull
// adapter: it renews the lease, admits the payload, hands the work to Next and blocks
// until the run reports, because returning is the ack and there is nothing to
// acknowledge until an outcome exists.
func (c *Channel) handle(ctx context.Context, _ asyncjobs.Logger, task *asyncjobs.Task) (any, error) {
	c.handlers.Add(1)
	defer c.handlers.Done()

	log := c.log.With("task", task.ID, "delivery", task.Tries)

	// Renewal covers the whole handler rather than starting at the handoff. The server
	// takes work and then waits for a slot, and the handoff itself waits for Next to be
	// called; both are unrenewed time otherwise.
	stopRenewing := c.renew(ctx, task, log)
	defer stopRenewing()

	req, err := c.intake(task, log)
	if err != nil {
		// Nothing intake refuses can succeed on another delivery, so it terminates. The
		// reason is one line because it is what lands in the task's LastErr; the detail
		// is already in the log.
		return nil, fmt.Errorf("%w: %s", asyncjobs.ErrTerminateTask, err)
	}

	// The delivery count is on the logger and whether this run resumed is not, because
	// the count cannot answer that: a worker that died persisted nothing. Only the run
	// knows, and it reports the session it journaled rather than which of the two it
	// did.
	log.Info("Taking a job",
		"task_type", task.Type,
		"queue", task.Queue,
		"request", req.ID,
		"caller", req.Sender.Name)

	outcome := make(chan serve.Outcome, 1)

	// Fields the request carries that Work has no home for are dropped here, and
	// deliberately: tool_hints, budget.call_timeout, stream, Header.Parent and
	// Header.Recipient. Header.Conversation is dropped with the most care, since it is
	// a caller-chosen string meaning "session" and must never reach Checkpoint, where
	// it would let one caller name another's journal.
	work := &serve.Work{
		ID:      task.ID,
		Prompt:  req.Prompt,
		Context: req.Context,
		// The task id names the session, and the store rather than the delivery count
		// decides whether this is a first run or a resume. A worker killed mid-run
		// persists no count, so the queue cannot answer it; the store knows whether the
		// journal exists, which is what at-least-once delivery actually asks.
		Checkpoint: agent.Checkpoint{
			ResumeID:        task.ID,
			CreateIfMissing: true,
		},
		// The task id again, because a claim naming this process would name the same
		// process for every job it serves. One string then greps across the worker's
		// logs, the queue, and the journal of whatever run a takeover interrupted.
		ClaimedBy:        task.ID,
		SuspendRequested: c.suspend,
		Budget:           budgetOf(req),
		Caller:           serve.Caller{Name: req.Sender.Name},
		Done: func(_ context.Context, out serve.Outcome) error {
			outcome <- out
			return nil
		},
	}

	select {
	case c.work <- work:
	case <-c.shutdown:
		log.Info("Returning a job that was claimed while the worker was stopping")
		return nil, fmt.Errorf("the worker is shutting down")
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// Once the work is handed over the server owns it and reports exactly once, and
	// Serve does not return until it has, so this waits for the answer rather than for
	// the shutdown signal. The context is the only escape: it is canceled when the lease
	// could not be renewed, and a run whose lease is gone is one this handler can no
	// longer speak for.
	select {
	case out := <-outcome:
		return c.disposition(req, out, log)
	case <-ctx.Done():
		log.Error("Abandoning a job whose lease could not be held", "error", ctx.Err())
		return nil, ctx.Err()
	}
}

// intake decides whether a payload can ever be run. The error it returns is a single
// line, because it becomes the task's LastErr; anything longer is logged here.
func (c *Channel) intake(task *asyncjobs.Task, log *slog.Logger) (*a2a.Request, error) {
	if len(task.Payload) > c.maxPayload {
		return nil, fmt.Errorf("the payload is %d bytes, over the %d byte limit", len(task.Payload), c.maxPayload)
	}

	// The task id names the journal, and the engine's own name rule is looser than the
	// session store's: it allows a leading dash, a colon, and any length. Checking it
	// here means an id no store would accept is refused before any setup rather than at
	// the create, which is after.
	err := runstate.ValidateID(task.ID)
	if err != nil {
		log.Error("A job id cannot name a session", "error", err)
		return nil, fmt.Errorf("the task id cannot name a session")
	}

	err = c.validator.Validate(task.Payload)
	if err != nil {
		log.Error("A job payload failed schema validation", "error", err)
		return nil, fmt.Errorf("the payload is not a valid v1 message")
	}

	msg, err := a2a.Decode(task.Payload)
	if err != nil {
		log.Error("A job payload could not be decoded", "error", err)
		return nil, fmt.Errorf("the payload could not be decoded")
	}

	req, ok := msg.(*a2a.Request)
	if !ok {
		log.Error("A job payload is not a request", "protocol", a2a.RequestProtocol)
		return nil, fmt.Errorf("the payload is not a %s message", a2a.RequestProtocol)
	}

	if req.Prompt == "" {
		return nil, fmt.Errorf("the request carries no prompt")
	}

	return req, nil
}

// renew keeps the job's lease and the handler's own deadline alive together for as
// long as the handler runs. The returned function stops it and waits for it, so a
// renewal is never in flight once the handler has decided its answer.
//
// A failed renewal is retried at the next tick rather than given up on, since a lease
// is lost to a transient outage as easily as to a permanent one. When it stays lost
// the engine cancels the handler's context, which is what ends the wait above.
func (c *Channel) renew(ctx context.Context, task *asyncjobs.Task, log *slog.Logger) func() {
	done := make(chan struct{})

	var wg sync.WaitGroup

	wg.Go(func() {
		ticker := time.NewTicker(c.renewEvery)
		defer ticker.Stop()

		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				err := task.MarkInProgress(ctx)
				if err != nil {
					log.Warn("Renewing the job lease failed", "error", err, "retry_in", c.renewEvery)
					continue
				}

				log.Debug("Renewed the job lease", "interval", c.renewEvery)
			}
		}
	})

	return func() {
		close(done)
		wg.Wait()
	}
}

// disposition turns what a run produced into one of the engine's three endings.
//
// The load-bearing choice is the last one: a run that failed is a completed job whose
// answer is that it failed. It is also the only mapping this vocabulary can express,
// since an Outcome cannot separate a transient provider outage from a final refusal.
func (c *Channel) disposition(req *a2a.Request, out serve.Outcome, log *slog.Logger) (any, error) {
	switch {
	case out.Rejected:
		// No retry helps something admission refused. Nothing produces this yet.
		log.Warn("Terminating a job that was refused", "error", out.Err)
		return nil, fmt.Errorf("%w: the work was refused", asyncjobs.ErrTerminateTask)

	case out.Crashed:
		// A panic is deterministic and each redelivery is real model spend.
		log.Error("Terminating a job whose run crashed", "error", out.Err)
		return nil, fmt.Errorf("%w: the run crashed", asyncjobs.ErrTerminateTask)

	case out.Abandoned:
		log.Info("Returning a job that was taken but never started")
		return nil, fmt.Errorf("the work was taken but never started")

	case out.Reason == runstate.ReasonSuspended:
		log.Info("Returning a suspended job", "session", out.SessionID)
		return nil, fmt.Errorf("the run suspended and left a resumable session")

	case out.Reason == "":
		// The run reached no outcome, so nothing was answered: setup failed, or the run
		// was cut short. A resume refused because another worker holds the session
		// arrives here too, and that is not a failure of the work either.
		err := out.Err
		if err == nil {
			err = fmt.Errorf("the run reported neither an outcome nor an error")
		}

		log.Warn("Returning a job whose run reached no outcome", "error", err)

		return nil, fmt.Errorf("the run reached no outcome: %w", err)
	}

	reason := stopReason(out.Reason)

	if out.Err != nil {
		log.Info("Storing the answer of a run that failed", "reason", out.Reason, "stop_reason", reason, "session", out.SessionID)

		msg := a2a.NewError(out.Err.Error())
		msg.StopReason = reason
		c.stampReply(&msg.Header, req)

		return msg, nil
	}

	log.Info("Storing an answer", "reason", out.Reason, "stop_reason", reason, "session", out.SessionID)

	msg := a2a.NewResult(reason)
	msg.Text = out.Text
	if out.Stats != nil {
		msg.Usage = &a2a.Usage{InputTokens: out.Stats.InTokens, OutputTokens: out.Stats.OutTokens}
	}
	c.stampReply(&msg.Header, req)

	return msg, nil
}

// stampReply fills in the framing of an answer so it echoes the request it answers.
// Sequence stays zero: this binding carries no event stream, so the answer is the only
// message in the set and there is nothing to order it against.
func (c *Channel) stampReply(h *a2a.Header, req *a2a.Request) {
	h.ID = a2a.NewID()
	h.Request = req.Request
	h.Conversation = req.Conversation
	h.Time = time.Now().UTC()
	h.Sender = a2a.Identity{Name: c.identity}

	if req.Sender.Name != "" {
		h.Recipient = &a2a.Identity{Name: req.Sender.Name}
	}
}

// stopReason maps a run's terminal reason onto the protocol's neutral vocabulary. A
// reason with no counterpart is reported as an error, which is what a caller that
// cannot interpret it should treat it as.
func stopReason(r runstate.TerminalReason) a2a.StopReason {
	switch r {
	case runstate.ReasonCompleted:
		return a2a.StopEndTurn
	case runstate.ReasonBudget:
		return a2a.StopBudgetExhausted
	case runstate.ReasonMaxIterations:
		return a2a.StopMaxIterations
	case runstate.ReasonSuspended:
		return a2a.StopSuspended
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
