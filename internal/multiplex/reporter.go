//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package multiplex

import (
	"context"
	"sync"
	"time"

	"github.com/choria-io/fisk-ai/internal/util"
)

// state is what a pane is shown as.
type state string

const (
	stateWorking state = "working"
	stateBlocked state = "blocked"
	stateIdle    state = "idle"
)

// reasonRunes cuts a block's description to what a pane list can show on one line.
const reasonRunes = 80

// deliveryTimeout limits one report, and the release that ends the session. A
// multiplexer that does not answer in this long is one whose pane goes stale, which
// costs a person nothing they cannot see for themselves on the pane itself.
const deliveryTimeout = 3 * time.Second

// report is one state to show, with the reason a block carries.
type report struct {
	state   state
	message string
}

// reporter delivers reports to one multiplexer without making the run wait for it.
//
// Every Reporter method returns as soon as it has recorded what to show. A worker
// goroutine does the delivering, which for most multiplexers means starting a process,
// and the run's own goroutines cannot afford to wait for one: the a2a hooks that drive
// this run inline on the goroutine reading the reply set, so a slow report there stops
// the conversation being read.
//
// Only the newest state is worth showing, so the mailbox holds one report and a newer
// one replaces an unsent older one. That bounds the work a chatty run makes: a pane
// shows a state rather than a history, and a state nobody saw is one nobody missed.
type reporter struct {
	name    string
	deliver func(context.Context, report) error
	release func(context.Context) error

	mu      sync.Mutex
	pending *report
	closed  bool

	wake chan struct{}
	stop chan struct{}
	done chan struct{}
}

// newReporter starts the worker that delivers what the returned reporter is told, and
// opens the pane at idle: a process that has just started is waiting on the person who
// started it, and it says so itself once it has work.
func newReporter(name string, deliver func(context.Context, report) error, release func(context.Context) error) *reporter {
	r := &reporter{
		name:    name,
		deliver: deliver,
		release: release,
		wake:    make(chan struct{}, 1),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}

	go r.work()
	r.Idle()

	return r
}

func (r *reporter) Name() string { return r.name }
func (r *reporter) Working()     { r.post(report{state: stateWorking}) }
func (r *reporter) Idle()        { r.post(report{state: stateIdle}) }

func (r *reporter) Blocked(reason string) {
	r.post(report{state: stateBlocked, message: util.SanitizeForTerminal(reason, reasonRunes)})
}

// Close stops reporting and gives up the pane.
//
// The worker is stopped and waited for first, so a report cannot land after the release
// and leave the multiplexer showing a state for a process that has exited. What is
// waiting in the mailbox at that point is dropped rather than sent: the pane is about to
// stop showing this process at all.
func (r *reporter) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()

		return
	}
	r.closed = true
	r.pending = nil
	r.mu.Unlock()

	close(r.stop)
	<-r.done

	ctx, cancel := context.WithTimeout(context.Background(), deliveryTimeout)
	defer cancel()

	_ = r.release(ctx)
}

// post records the state to show next and wakes the worker. The wake channel holds one
// token, so a run producing states faster than they can be delivered does not grow a
// queue of them.
func (r *reporter) post(rep report) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()

		return
	}
	r.pending = &rep
	r.mu.Unlock()

	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// work delivers what is posted until Close stops it. It always takes the newest report,
// so a state posted while an earlier one was being delivered is the one that goes next.
func (r *reporter) work() {
	defer close(r.done)

	for {
		select {
		case <-r.stop:
			return

		case <-r.wake:
			rep, ok := r.take()
			if !ok {
				continue
			}

			r.send(rep)
		}
	}
}

// take removes the report to deliver, and reports false when a newer post has already
// been taken by an earlier pass.
func (r *reporter) take() (report, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.pending == nil {
		return report{}, false
	}

	rep := *r.pending
	r.pending = nil

	return rep, true
}

// send delivers one report, and swallows what goes wrong with it.
//
// A failed delivery leaves the pane on the state before it, which the next report
// corrects, and there is nowhere to complain to: this goroutine runs under a full-screen
// view that owns the terminal. A panic is contained for a harder reason, since one here
// would take the process down with the terminal still in raw mode.
func (r *reporter) send(rep report) {
	defer func() { _ = recover() }()

	ctx, cancel := context.WithTimeout(context.Background(), deliveryTimeout)
	defer cancel()

	_ = r.deliver(ctx, rep)
}
