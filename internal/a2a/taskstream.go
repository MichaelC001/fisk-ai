//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"
)

// TaskStream is a caller's view of one task's reply set: an ack, then the events the
// run produced, then a terminal result or error.
//
// It is iterated rather than assembled, because a terminal rendering a live run needs
// each event as it arrives and a caller handed a finished slice could not show one.
// It is not safe for concurrent use.
type TaskStream struct {
	reader    Reader
	validator *Validator
	request   string
	seq       uint64
	gaps      uint64
	done      bool

	// idle bounds the wait for each message rather than for the set, since a set has no
	// length a caller could bound: a run may think for an hour and is not stuck. What it
	// catches is an agent that has stopped saying anything at all, which is otherwise
	// indistinguishable from one still working, because the only other end is the
	// caller's own context.
	//
	// Zero waits forever, which is what a caller that set no bound asked for.
	idle time.Duration

	// holding counts the questions this caller has been asked and not yet answered,
	// and resumed is closed when the last of them is answered. While one is
	// outstanding the idle bound does not apply: the agent is waiting for an answer
	// this caller owes it, so its silence says nothing about whether it is there.
	//
	// The count is taken on the goroutine reading the set and released on the
	// goroutine that answers, which is why both are guarded. resumed is nil whenever
	// the count is zero, which is how a read tells the two states apart.
	mu      sync.Mutex
	holding int
	resumed chan struct{}

	// agent and wire are who the set is with and where to record it, for a caller that
	// asked to see what crossed. A body is recorded as it arrives, before the size cap
	// and the schema have had an opinion, since a message that fails either is the one
	// somebody turned recording on to look at.
	agent string
	wire  *wireLog
}

// suspend stops the idle bound applying while a question this caller has been asked and
// not yet answered is outstanding, and returns the release that starts it applying
// again.
//
// It is taken on the goroutine reading the set, before the read that follows the
// question, so a read never begins under a bound the question should have lifted. The
// release is safe to call more than once.
func (t *TaskStream) suspend() func() {
	t.mu.Lock()
	if t.holding == 0 {
		t.resumed = make(chan struct{})
	}
	t.holding++
	resumed := t.resumed
	t.mu.Unlock()

	var once sync.Once

	return func() {
		once.Do(func() {
			t.mu.Lock()
			defer t.mu.Unlock()

			t.holding--
			if t.holding > 0 {
				return
			}

			close(resumed)
			t.resumed = nil
		})
	}
}

// suspended returns the channel closed when the last outstanding question is answered,
// and nil when the bound applies.
func (t *TaskStream) suspended() <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.resumed
}

// read takes one message, bounded by the idle wait when there is one. The bound is
// reported as its own error rather than as a canceled context, since a caller that gave
// up and an agent that went quiet are different things to tell somebody about.
//
// The reader runs on a goroutine of its own so that the window can be started over
// without starting the read over: Next is called once per message, where canceling it
// and calling it again would drop whatever the first call had already taken.
func (t *TaskStream) read(ctx context.Context) ([]byte, error) {
	if t.idle <= 0 {
		return t.reader.Next(ctx)
	}

	type message struct {
		body []byte
		err  error
	}

	readCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	done := make(chan message, 1)

	go func() {
		body, err := t.reader.Next(readCtx)
		done <- message{body: body, err: err}
	}()

	timer := time.NewTimer(t.idle)
	defer timer.Stop()

	for {
		// Nil for a read with no question outstanding, which blocks its case forever and
		// leaves the bound applying as it always did.
		resumed := t.suspended()

		select {
		case got := <-done:
			return got.body, got.err

		// The caller's own ending, which the reader is seeing too, so what the reader
		// answers is what this read reports.
		case <-ctx.Done():
			got := <-done

			return got.body, got.err

		// The answer has gone, so the agent owes this caller a message again and it gets
		// a whole window to send one. The time somebody spent deciding is not charged
		// against it: a question answered a minute into a two minute window would
		// otherwise leave the agent a minute to speak, which is the fault this suspension
		// exists to remove.
		case <-resumed:
			timer.Reset(t.idle)

		case <-timer.C:
			if resumed == nil {
				// A message that arrived as the window closed is a message rather than
				// silence, and the select above had no way to prefer it.
				select {
				case got := <-done:
					return got.body, got.err
				default:
				}

				return nil, fmt.Errorf("%w: nothing heard from the agent for %s", ErrAgentUnavailable, t.idle)
			}

			// The window ran out under the question. The answer is waited for here rather
			// than by going round the loop, since the release may have landed as the
			// window closed and the next pass would then find nothing left to wait on.
			select {
			case got := <-done:
				return got.body, got.err

			case <-ctx.Done():
				got := <-done

				return got.body, got.err

			case <-resumed:
				timer.Reset(t.idle)
			}
		}
	}
}

// Next returns the next message of the set, one of *Ack, *Event, *ElicitRequest,
// *Result or *ErrorMessage, and io.EOF once the terminal message has been returned. A
// terminal failure arrives as an *ErrorMessage value rather than as the error, so the
// error return means the set could not be read at all.
func (t *TaskStream) Next(ctx context.Context) (Message, error) {
	if t.done {
		return nil, io.EOF
	}

	body, err := t.read(ctx)
	if err != nil {
		return nil, err
	}

	t.wire.recv(OpTask, t.agent, t.request, body)

	if len(body) > MaxMessageSize {
		return nil, fmt.Errorf("%w: message exceeds %d bytes", ErrMessageTooLarge, MaxMessageSize)
	}

	err = t.validator.Validate(body)
	if err != nil {
		return nil, fmt.Errorf("invalid message in the reply set: %w", err)
	}

	msg, err := Decode(body)
	if err != nil {
		return nil, err
	}

	t.countGap(msg.MessageHeader().Sequence)

	switch m := msg.(type) {
	case *Ack, *Event, *ElicitRequest:
		return m, nil
	case *Result, *ErrorMessage:
		t.done = true
		return m, nil
	default:
		return nil, fmt.Errorf("%w: %q does not belong in a reply set", ErrProtocolMismatch, msg.MessageHeader().Protocol)
	}
}

// Gaps reports how many messages the sequence numbers say never arrived. A gap does
// not fail the task: the answer is in the result and events are advisory, so it is
// reported beside a successful result and the caller decides what it means to it.
func (t *TaskStream) Gaps() uint64 { return t.gaps }

// Request is the correlation tag of the request this set answers.
func (t *TaskStream) Request() string { return t.request }

// Close releases the reader. The run keeps publishing into a dead inbox until it
// ends, so a caller that has stopped caring sends a cancel as well.
func (t *TaskStream) Close() error { return t.reader.Close() }

// countGap records how many numbers the sequence skipped. A message numbered at or
// below the last one seen is counted as no gap: the stream is lossy rather than
// reordered, so a lower number describes a duplicate rather than a hole.
func (t *TaskStream) countGap(seq uint64) {
	if seq > t.seq+1 {
		t.gaps += seq - t.seq - 1
	}
	if seq > t.seq {
		t.seq = seq
	}
}

// Task sends a task request to the named agent and returns the reply set it
// produces. The request is stamped and validated here; the reply set is read and
// validated message by message through the returned stream.
//
// It refuses a transport that cannot carry a reply set, which the client asserted
// once when it was built, so the refusal names the binding rather than arriving as a
// task that answers nothing.
func (c *Client) Task(ctx context.Context, agent string, req *Request) (*TaskStream, error) {
	if c.stream == nil {
		return nil, fmt.Errorf("%w: a task needs a streaming transport", ErrStreamUnsupported)
	}

	StampRequest(ctx, &req.Header, c.sender, agent)

	data, err := c.marshalValid(req)
	if err != nil {
		return nil, err
	}

	c.wire.send(OpTask, agent, req.Request, data)

	reader, err := c.stream.Stream(ctx, agent, OpTask, data)
	if err != nil {
		return nil, err
	}

	return &TaskStream{reader: reader, validator: c.validator, request: req.Request, idle: c.idle, agent: agent, wire: c.wire}, nil
}

// Cancel asks the agent running the named task to stop, and reports what it
// answered. ErrAgentUnavailable means nothing is running that task there, which
// separates a never-accepted, not-yet-started or already-finished task from one that
// received the cancel.
func (c *Client) Cancel(ctx context.Context, agent, request, reason string) (*Ack, error) {
	if c.stream == nil {
		return nil, fmt.Errorf("%w: a cancel is addressed to a task", ErrStreamUnsupported)
	}
	if !ValidRequestID(request) {
		return nil, fmt.Errorf("%w: %q is not a valid request id", ErrInvalidMessage, request)
	}

	// Fired before the cancel goes rather than after it is acked, so a caller learns
	// that somebody asked for a stop even when the agent is not there to answer. The
	// turn does not end here: a cancel asks for a boundary, and the ending arrives
	// through the reply set as usual.
	c.hooks.fireCancelRequested(ctx, CancelRequestedInfo{Agent: agent, Request: request, Reason: reason})

	msg := NewCancel()
	msg.Reason = reason
	StampRequest(ctx, &msg.Header, c.sender, agent)

	// A cancel correlates to the task it stops rather than to itself, which is what
	// stamping a standalone request gives it, so the tag is set to the task's after.
	msg.Request = request

	data, err := c.marshalValid(msg)
	if err != nil {
		return nil, err
	}

	c.wire.send(OpCancel, agent, request, data)

	reply, err := c.stream.SendCancel(ctx, agent, request, data)
	if err != nil {
		return nil, err
	}

	c.wire.recv(OpCancel, agent, request, reply)

	if len(reply) > MaxMessageSize {
		return nil, fmt.Errorf("%w: reply exceeds %d bytes", ErrMessageTooLarge, MaxMessageSize)
	}

	err = c.validator.Validate(reply)
	if err != nil {
		return nil, fmt.Errorf("invalid cancel reply: %w", err)
	}

	return ExpectProtocol[*Ack](reply, AckProtocol)
}
