//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package slack

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// The lines a status message shows. Each is a hint rather than a tool name: a thread is
// read by everybody in the channel, and which tool a turn is running against what is not
// theirs to publish.
const (
	// hintQueued is what a turn admitted with no worker free shows until its run starts.
	hintQueued = "Queued..."
	// hintThinking covers the run starting and every assistant turn that is not the
	// answer.
	hintThinking = "Thinking..."
	// hintMemory covers the memory tools and hintKnowledge the knowledge ones, both
	// named as families rather than by the call being made.
	hintMemory    = "Accessing memory..."
	hintKnowledge = "Searching knowledge..."
	// hintTools covers every other tool, which is what keeps a tool this bot gained
	// yesterday from naming itself in a channel.
	hintTools = "Calling tools..."
	// hintWaiting is what a turn shows while a question of its own is open in the thread.
	// The question is its own message with the buttons on it; this says why the turn has
	// stopped moving.
	hintWaiting = "Waiting for an answer..."
)

// hintFor is what a thread is told about one tool call.
func hintFor(tool string) string {
	switch {
	case strings.HasPrefix(tool, "memory_"):
		return hintMemory
	case tool == "knowledge_search", tool == "knowledge_enumerate":
		return hintKnowledge
	default:
		return hintTools
	}
}

// status is the one message a turn posts and keeps editing while it runs.
//
// Every method tolerates a nil receiver, which is what no_progress is: a channel that
// posts no running commentary builds none of these, and nothing that drives one has to
// ask whether it exists.
//
// What it publishes is a state rather than a stream. A caller records the hint the turn
// has reached and the message catches up when the workspace's allowance lets it, so a run
// that moved on three times while a call was owed shows where it is rather than replaying
// where it was. Intermediate hints are the traffic this design drops; the last state is
// the one it may never drop, since the turn's ending is what turns this message into a
// pointer at the answer.
type status struct {
	ch  *Channel
	log *slog.Logger

	channelID string
	threadTS  string

	mu sync.Mutex

	// queued is set for a turn admitted with no worker free, which is the one state
	// that is the channel's rather than the run's.
	queued bool

	// waiting is set while a question this turn asked is open in the thread. The run
	// blocks in the prompter for as long as it is, so no hint arrives to displace it and
	// the hint the turn was on is where it goes back to.
	waiting bool

	// hint is the last hint recorded and repeats how many records in a row reached it,
	// so a long run visibly moves without saying what it is moving through.
	hint    string
	repeats int

	// final is what the message ends as, which is the pointer at the answer rather than
	// a hint. Once it is set nothing the run passed through matters any more, so it wins
	// over everything above.
	final string

	// ts names the message once it has been posted and published is the text Slack was
	// last given, which is what decides whether another call is worth making.
	ts        string
	published string

	// changed wakes the publisher. It is buffered by one because it is a signal that
	// the state moved, not a queue of the states it moved through.
	changed chan struct{}

	// ending is closed when the turn ends, after which the publisher writes the state
	// one last time and stops.
	ending  chan struct{}
	endOnce sync.Once
}

// newStatus builds the status message for one turn, or nil where this channel narrates
// nothing. queued says the turn has no worker to start on.
func (c *Channel) newStatus(t *turn, queued bool) *status {
	if !c.progress {
		return nil
	}

	// A turn admitted into a drain has nothing to narrate: nothing will hand it over, so
	// the message would be posted and never edited again.
	if c.draining() {
		return nil
	}

	return &status{
		ch:        c,
		log:       t.log,
		channelID: t.m.ChannelID,
		threadTS:  t.m.ThreadTS,
		queued:    queued,
		changed:   make(chan struct{}, 1),
		ending:    make(chan struct{}),
	}
}

// startStatus starts the goroutine that keeps one status message current.
//
// It is never called from the goroutine reading envelopes before that envelope has been
// acknowledged: what it starts talks to Slack, and Slack redelivers anything it has not
// been answered about within three seconds.
//
// The channel waits for these on the way down, the same way it waits for the messages it
// posts for itself, so a drain does not leave a turn's last state unwritten.
func (c *Channel) startStatus(s *status) {
	if s == nil {
		return
	}

	c.mu.Lock()
	closed := c.postsClosed
	if !closed {
		c.posts.Add(1)
	}
	c.mu.Unlock()

	// Past that point nothing is waiting for this message and the socket is on its way
	// out, so the turn narrates nothing rather than starting a goroutine the shutdown
	// has already passed.
	if closed {
		return
	}

	go func() {
		defer c.posts.Done()

		s.publish()
	}()
}

// publish writes the state the turn has reached, for as long as the turn runs, and writes
// it once more on the way out.
func (s *status) publish() {
	for {
		s.deliver()

		select {
		case <-s.changed:

		case <-s.ending:
			s.deliver()

			return

		case <-s.ch.shutdown:
			s.deliver()

			return
		}
	}
}

// deliver writes the current state to Slack, having waited for the process's allowance to
// make the call.
//
// The state is read twice, once to decide the call is worth making at all and again once
// the allowance let it through, since a run that moved on while a call was owed is better
// described by where it is than by where it was.
func (s *status) deliver() {
	if !s.pending() {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultReplyDeadline)
	defer cancel()

	err := s.ch.limit.take(ctx)
	if err != nil {
		s.log.Warn("Waiting for the allowance to update a status message failed", "error", err)

		return
	}

	text, ts, ok := s.current()
	if !ok {
		return
	}

	if ts == "" {
		posted, err := s.ch.api.postMessage(ctx, s.channelID, s.threadTS, text)
		if err != nil {
			s.log.Warn("Posting a status message failed", "error", err)

			return
		}

		s.wrote(posted, text)

		return
	}

	err = s.ch.api.updateMessage(ctx, s.channelID, ts, text)
	if err != nil {
		s.log.Warn("Updating a status message failed", "error", err)

		return
	}

	s.wrote(ts, text)
}

// pending reports whether Slack has yet to be told the state the turn has reached.
func (s *status) pending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.textLocked() != s.published
}

// current is the text to write and the message to write it to, reporting false where
// Slack already shows it.
func (s *status) current() (text string, ts string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	text = s.textLocked()
	if text == s.published {
		return "", "", false
	}

	return text, s.ts, true
}

// wrote records what Slack now shows. A post that failed records nothing, so the next
// write posts rather than editing a message that does not exist.
func (s *status) wrote(ts string, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.ts = ts
	s.published = text
}

// textLocked is what the message says now.
//
// A run that has started and reported nothing yet reads as thinking rather than as blank,
// which is also what its first event says, so the two agree and no call is spent moving
// between them.
func (s *status) textLocked() string {
	if s.final != "" {
		return s.final
	}
	if s.waiting {
		return hintWaiting
	}
	if s.queued {
		return hintQueued
	}
	if s.hint == "" {
		return hintThinking
	}
	if s.repeats > 1 {
		return fmt.Sprintf("%s (%d)", s.hint, s.repeats)
	}

	return s.hint
}

// note records the hint the turn has reached.
//
// A hint that repeats gains a count, so two minutes of tool calls read as a run that is
// moving rather than one that has hung. The count is of hints in a row: a different hint
// starts it again, since what it measures is how long this one has held.
func (s *status) note(hint string) {
	if s == nil {
		return
	}

	s.mu.Lock()
	if s.hint == hint {
		s.repeats++
	} else {
		s.hint = hint
		s.repeats = 1
	}

	// Anything the run reports is the run having started, whatever the channel believed
	// about free workers when it admitted the turn.
	s.queued = false
	s.mu.Unlock()

	s.moved()
}

// asking records whether a question this turn put to the thread is open.
//
// It is recorded rather than written, as every other state here is, so the message says the
// turn is waiting through the same publisher and the same allowance as the hints around it.
// A question answered inside the grace window may cost no call at all, the publisher writing
// whatever the state is by the time it gets through.
func (s *status) asking(open bool) {
	if s == nil {
		return
	}

	s.mu.Lock()
	s.waiting = open
	if open {
		s.queued = false
	}
	s.mu.Unlock()

	s.moved()
}

// running records that the server found a slot for this turn, which is the only signal a
// channel gets between handing work over and the run beginning. It ends the queued line.
func (s *status) running() {
	if s == nil {
		return
	}

	s.mu.Lock()
	s.queued = false
	s.mu.Unlock()

	s.moved()
}

// ends records the last thing this message says, which is where the turn's answer is
// rather than where the run got to.
//
// It is recorded rather than written: the publisher makes the call, so the last edit is
// spent from the same allowance as every hint that came before it instead of going around
// the meter. An empty text records nothing, chat.update refusing a message with none.
func (s *status) ends(text string) {
	if s == nil || text == "" {
		return
	}

	s.mu.Lock()
	s.final = text
	s.queued = false
	s.mu.Unlock()

	s.moved()
}

// stop ends the status message, once the state the turn ended in has been written.
//
// It is idempotent: a turn reporting an outcome twice is cheaper to tolerate here than to
// prove impossible.
func (s *status) stop() {
	if s == nil {
		return
	}

	s.endOnce.Do(func() { close(s.ending) })
}

// moved wakes the publisher. It is a signal rather than a handover, since what the
// publisher reads is the current state: a wake already pending says everything a second
// one would.
func (s *status) moved() {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

// The allowance every call this channel makes to Slack answers to.
//
// chat.postMessage and chat.update are Tier 3 methods, roughly fifty calls a minute for
// the app across the whole workspace rather than per channel or per message, so this is
// one bucket for the process and every turn, question, refusal and note shares it. The
// burst is what makes a quiet workspace answer at once instead of pacing a handful of
// calls nobody is competing for.
const (
	defaultRateInterval = time.Minute / 50
	defaultRateBurst    = 5
)

// clock is the time a limiter measures with, so a spec drives a throttle rather than
// waiting for one.
type clock interface {
	now() time.Time
	after(d time.Duration) <-chan time.Time
}

// wallClock is the clock a channel runs on.
type wallClock struct{}

func (wallClock) now() time.Time                         { return time.Now() }
func (wallClock) after(d time.Duration) <-chan time.Time { return time.After(d) }

// limiter is the token bucket the channel's calls to Slack are spent from. It is held by
// the Channel and shared by every turn, which is the whole point: the allowance it stands
// for belongs to the app in the workspace, not to a message or a conversation.
type limiter struct {
	mu sync.Mutex

	clock    clock
	interval time.Duration
	burst    int

	// tokens is what is left to spend and last the time they were last accounted for.
	tokens int
	last   time.Time
}

// newLimiter builds the bucket. A non-positive interval or burst takes the default, so a
// caller assembling one in process cannot accidentally build a bucket that never fills.
func newLimiter(interval time.Duration, burst int, c clock) *limiter {
	if c == nil {
		c = wallClock{}
	}
	if interval <= 0 {
		interval = defaultRateInterval
	}
	if burst <= 0 {
		burst = defaultRateBurst
	}

	return &limiter{
		clock:    c,
		interval: interval,
		burst:    burst,
		tokens:   burst,
		last:     c.now(),
	}
}

// take blocks until a call may be made, or until ctx ends and the caller gives up on
// making it.
func (l *limiter) take(ctx context.Context) error {
	for {
		wait := l.reserve()
		if wait <= 0 {
			return nil
		}

		select {
		case <-l.clock.after(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// reserve spends a token where one is there to spend, and otherwise reports how long
// until the next one arrives.
func (l *limiter) reserve() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.clock.now()

	gained := int(now.Sub(l.last) / l.interval)
	if gained > 0 {
		l.tokens = min(l.burst, l.tokens+gained)
		l.last = l.last.Add(time.Duration(gained) * l.interval)

		// A full bucket banks nothing further, so the time an idle hour would otherwise
		// have credited is dropped rather than spent all at once later.
		if l.tokens == l.burst {
			l.last = now
		}
	}

	if l.tokens > 0 {
		l.tokens--

		return 0
	}

	return l.interval - now.Sub(l.last)
}
