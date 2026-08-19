//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	"context"
	"fmt"
	"io"
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

	// agent and wire are who the set is with and where to record it, for a caller that
	// asked to see what crossed. A body is recorded as it arrives, before the size cap
	// and the schema have had an opinion, since a message that fails either is the one
	// somebody turned recording on to look at.
	agent string
	wire  *wireLog
}

// read takes one message, bounded by the idle wait when there is one. The bound is
// reported as its own error rather than as a canceled context, since a caller that gave
// up and an agent that went quiet are different things to tell somebody about.
func (t *TaskStream) read(ctx context.Context) ([]byte, error) {
	if t.idle <= 0 {
		return t.reader.Next(ctx)
	}

	waited, cancel := context.WithTimeout(ctx, t.idle)
	defer cancel()

	body, err := t.reader.Next(waited)
	if err != nil && waited.Err() != nil && ctx.Err() == nil {
		return nil, fmt.Errorf("%w: nothing heard from the agent for %s", ErrAgentUnavailable, t.idle)
	}

	return body, err
}

// Next returns the next message of the set, one of *Ack, *Event, *Result or
// *ErrorMessage, and io.EOF once the terminal message has been returned. A terminal
// failure arrives as an *ErrorMessage value rather than as the error, so the error
// return means the set could not be read at all.
func (t *TaskStream) Next(ctx context.Context) (any, error) {
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

	hdr := headerOf(msg)
	if hdr == nil {
		return nil, fmt.Errorf("%w: message carries no header", ErrProtocolMismatch)
	}

	t.countGap(hdr.Sequence)

	switch m := msg.(type) {
	case *Ack, *Event, *ElicitRequest:
		return m, nil
	case *Result, *ErrorMessage:
		t.done = true
		return m, nil
	default:
		return nil, fmt.Errorf("%w: %q does not belong in a reply set", ErrProtocolMismatch, hdr.Protocol)
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

	stampRequest(ctx, &req.Header, c.sender, agent)

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
	stampRequest(ctx, &msg.Header, c.sender, agent)

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

	decoded, err := ExpectProtocol(reply, AckProtocol)
	if err != nil {
		return nil, err
	}

	return decoded.(*Ack), nil
}
