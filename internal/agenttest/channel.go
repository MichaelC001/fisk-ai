//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agenttest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/choria-io/fisk-ai/internal/serve"
)

// A scripted list holds nothing and so is not releasable, while a queue is. Asserting
// both here is what keeps that difference deliberate: it is the pair of shapes a server
// has to handle, and a fake that quietly grew a Close would take the untested one away.
var (
	_ serve.Channel           = (*ScriptedChannel)(nil)
	_ serve.ReleasableChannel = (*Queue)(nil)
)

// ScriptedChannel is a serve.Channel that hands out a fixed list of work and records
// every outcome, so a test drives a server without standing a transport up. It reports
// serve.ErrChannelDone once the list is spent, which is what makes Serve return by
// itself.
//
// It has no Close, so a server draining or stopping one leaves it alone. That is what a
// channel holding no resource looks like, and it keeps this usable for testing what a
// server does with a channel it cannot release. Use Queue where the release matters.
type ScriptedChannel struct {
	name string
	work []*serve.Work

	mu       sync.Mutex
	next     int
	outcomes []serve.Outcome
}

// NewScriptedChannel builds a channel answering successive calls with the given work,
// in order.
//
// The name is not checked. A server rejecting an empty one is behavior worth a spec,
// and refusing to build the channel would leave nothing to write it with.
func NewScriptedChannel(tb testing.TB, name string, work ...*serve.Work) *ScriptedChannel {
	tb.Helper()

	c, err := BuildScriptedChannel(name, work...)
	if err != nil {
		tb.Fatalf("%v", err)
	}

	return c
}

// BuildScriptedChannel is NewScriptedChannel without a testing.TB, for a func Example or
// any other caller outside a test. It returns an error naming the position of the first
// nil work.
func BuildScriptedChannel(name string, work ...*serve.Work) (*ScriptedChannel, error) {
	for i, w := range work {
		if w == nil {
			return nil, fmt.Errorf("agenttest: scripted work %d is nil", i)
		}
	}

	return &ScriptedChannel{name: name, work: work}, nil
}

// Name identifies the channel.
func (c *ScriptedChannel) Name() string { return c.name }

// Next returns the next piece of work, and serve.ErrChannelDone once the list is
// spent.
//
// Every piece of work is given a Done that records its outcome here, so a caller
// scripts what to run without also wiring up where the answer goes. Work that already
// carries one keeps it: the recorder runs first and then calls it, so a spec watching
// outcomes land still has them in Outcomes. Its error is what the server sees.
func (c *ScriptedChannel) Next(_ context.Context) (*serve.Work, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.next >= len(c.work) {
		return nil, serve.ErrChannelDone
	}

	w := c.work[c.next]
	c.next++
	w.Done = recordOutcome(&c.mu, &c.outcomes, w.Done)

	return w, nil
}

// Outcomes returns what the server has reported so far, in the order it reported it.
// It is safe to call while a server is running, which is what makes it usable with
// Eventually.
func (c *ScriptedChannel) Outcomes() []serve.Outcome {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]serve.Outcome(nil), c.outcomes...)
}

// Queue is a serve.Channel that stays open until it is closed, so a test submits work
// while a server is already running rather than scripting it in advance. It stands in
// for a durable work queue without one.
//
// That is the shape a scripted list cannot have, and it is what makes a drain
// observable: a channel reporting it is finished the moment its list is spent has
// already ended Serve before a test could drain anything. Close is how this one ends,
// and Close is what Server.Drain calls.
type Queue struct {
	tb   testing.TB
	name string

	mu       sync.Mutex
	pending  []*serve.Work
	outcomes []serve.Outcome
	faults   []ScriptingFault
	closed   bool
	closes   int
	wake     chan struct{}
}

// NewQueue builds an empty queue that is open. Work arrives through Submit.
func NewQueue(tb testing.TB, name string) *Queue {
	tb.Helper()

	q := BuildQueue(name)
	q.tb = tb

	return q
}

// BuildQueue is NewQueue without a testing.TB, for a func Example or any other caller
// outside a test. Close ends the queue, and a caller that serves one calls it where a
// test would have let the server's drain do it.
func BuildQueue(name string) *Queue {
	return &Queue{name: name, wake: make(chan struct{})}
}

// Name identifies the channel.
func (q *Queue) Name() string { return q.name }

// Submit offers work to whoever is serving the queue. It never blocks, and work
// submitted while nothing is serving waits until something is.
//
// Nil work is not queued: it records a ScriptingFault and is reported in an error
// wrapping ErrNotScripted, while the rest of the batch is submitted. A spec submitting
// from a goroutine of its own reads the faults back through ScriptingFaults.
//
// Submitting to a closed queue is allowed and the work is never delivered, which is
// what a queue a worker has drained away from looks like. Pending reports what is left.
func (q *Queue) Submit(work ...*serve.Work) error {
	if q.tb != nil {
		q.tb.Helper()
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	var faulted error

	for i, w := range work {
		if w == nil {
			fault := ScriptingFault{Call: "Submit", Subject: q.name, Missing: fmt.Sprintf("work %d is nil", i)}
			q.faults = append(q.faults, fault)
			faulted = errors.Join(faulted, fmt.Errorf("%w: %w", ErrNotScripted, fault))

			continue
		}

		q.pending = append(q.pending, w)
	}

	q.broadcastLocked()

	return faulted
}

// ScriptingFaults returns every submission the queue could not accept, in the order it
// received them. A spec submitting only real work gets an empty slice. The copy is safe
// to read from the spec goroutine while a server is running.
//
// This is not serve.FaultingEndpoint's Faults, which reports a channel's own error
// stream. The Queue does not implement that interface.
func (q *Queue) ScriptingFaults() []ScriptingFault {
	q.mu.Lock()
	defer q.mu.Unlock()

	return append([]ScriptingFault(nil), q.faults...)
}

// Next blocks until work is submitted, the queue is closed, or ctx ends. Every piece of
// work is given a Done that records its outcome here, wrapping any the work already
// carries, exactly as ScriptedChannel does.
func (q *Queue) Next(ctx context.Context) (*serve.Work, error) {
	for {
		q.mu.Lock()

		if q.closed {
			q.mu.Unlock()
			return nil, serve.ErrChannelDone
		}

		if len(q.pending) > 0 {
			w := q.pending[0]
			q.pending = q.pending[1:]
			w.Done = recordOutcome(&q.mu, &q.outcomes, w.Done)
			q.mu.Unlock()

			return w, nil
		}

		wake := q.wake
		q.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-wake:
		}
	}
}

// Close ends the queue: a blocked Next returns serve.ErrChannelDone and so does every
// later one. It does not wait for runs already in flight, which are the server's to
// wait for rather than the channel's.
//
// Calling it more than once is safe, since a program draining on one signal and
// stopping on the next releases every channel twice.
func (q *Queue) Close() error {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.closes++
	if q.closed {
		return nil
	}

	q.closed = true
	q.broadcastLocked()

	return nil
}

// Closes reports how many times the server called Close, so a spec can prove a
// drain-then-stop sequence reached the channel both times rather than assuming it.
func (q *Queue) Closes() int {
	q.mu.Lock()
	defer q.mu.Unlock()

	return q.closes
}

// Pending reports how much submitted work was never handed over, which is what a drain
// leaves behind on the queue.
func (q *Queue) Pending() int {
	q.mu.Lock()
	defer q.mu.Unlock()

	return len(q.pending)
}

// recordOutcome builds the Done a fake channel puts on every piece of work: it appends
// the outcome under mu and then calls whatever the work already carried, so wrapping
// Done to observe a run does not cost the record.
func recordOutcome(mu *sync.Mutex, into *[]serve.Outcome, next func(context.Context, serve.Outcome) error) func(context.Context, serve.Outcome) error {
	return func(ctx context.Context, out serve.Outcome) error {
		mu.Lock()
		*into = append(*into, out)
		mu.Unlock()

		if next == nil {
			return nil
		}

		return next(ctx, out)
	}
}

// Outcomes returns what the server has reported so far, in the order it reported it.
// It is safe to call while a server is running, which is what makes it usable with
// Eventually.
func (q *Queue) Outcomes() []serve.Outcome {
	q.mu.Lock()
	defer q.mu.Unlock()

	return append([]serve.Outcome(nil), q.outcomes...)
}

// broadcastLocked wakes everything blocked in Next. The channel is closed rather than
// sent on so every waiter sees it, and replaced so the next wait has something to
// block on.
func (q *Queue) broadcastLocked() {
	close(q.wake)
	q.wake = make(chan struct{})
}
