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

// The emoji a status message opens with, one for each state a turn can be in.
//
// A thread that has scrolled past is read by these before it is read by the words, so they
// are told apart by shape rather than by color: somebody who cannot tell the red cross from
// the green tick still sees a cross where a turn failed and a tick where one answered.
//
// They are Slack shortcodes rather than the characters themselves, which Slack renders in
// the mrkdwn of a section and in the text a notification shows.
const (
	// emojiQueued is a turn waiting for a worker.
	emojiQueued = ":hourglass_flowing_sand:"

	// One per hint, so the icon says what the words say: a run between tool calls, the
	// memory tools, the knowledge ones, and every other tool.
	emojiThinking  = ":thinking_face:"
	emojiMemory    = ":brain:"
	emojiKnowledge = ":books:"
	emojiTools     = ":hammer:"

	// emojiAsking is a turn waiting on somebody in the thread, whether the run is parked in
	// the prompter or the question outlived the turn that asked it.
	emojiAsking = ":question:"

	// emojiAnswered is a turn that finished, emojiStopped one that parked where the thread
	// carries on from, and emojiFailed one that ended on a fault.
	emojiAnswered = ":white_check_mark:"
	emojiStopped  = ":octagonal_sign:"
	emojiFailed   = ":x:"
)

// statusText is the line one status message shows: the emoji for the state the turn is in,
// then the words.
//
// The emoji goes on the front of the whole line rather than into it, so an ending carrying
// Slack's link markup is still a link. The same string is what the message's text argument
// is set from, which is what a notification and a client that renders no blocks show.
func statusText(icon string, line string) string {
	if icon == "" {
		return line
	}

	return icon + " " + line
}

// hintEmoji is the emoji one hint a run reported is shown with. Anything else takes the
// thinking one, which is what a run that has reported nothing yet shows.
//
// The queued and waiting lines are not among them: neither is a hint a run reports, and the
// state that produces each of the two chooses its own emoji.
func hintEmoji(hint string) string {
	switch hint {
	case hintMemory:
		return emojiMemory
	case hintKnowledge:
		return emojiKnowledge
	case hintTools:
		return emojiTools
	default:
		return emojiThinking
	}
}

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
//
// The Stop button is part of that state rather than an edit of its own, so it goes on with
// the first write and comes off with the ending through the same publisher and the same
// allowance as every hint between them.
type status struct {
	ch  *Channel
	log *slog.Logger

	channelID string
	threadTS  string

	mu sync.Mutex

	// queued is set for a turn admitted with no worker free, which is the one state
	// that is the channel's rather than the run's.
	queued bool

	// quiet holds the first post back until the turn has something to say beyond having
	// started. It is set for a resume, where one press can produce a run that does nothing
	// but bank its answer: Outcome.Deferred is a list, so a turn that asked two questions
	// defers on the second the moment the first is answered, and a thread would collect a
	// "Thinking..." that never changes for every answer somebody gave.
	//
	// It applies to the first post and to no edit after it. Once Slack has the message,
	// what it shows is the state the turn reached.
	quiet bool

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
	// over everything above. finalEmoji is the state it ended in, which the run reports
	// alongside the words: a turn that failed and one that answered read the same at a
	// glance otherwise.
	final      string
	finalEmoji string

	// buttons are what a person may press on this message while the turn is live, which is
	// the Stop button and nothing else. They are part of the state this message publishes
	// rather than an edit of their own, so taking them off travels through the same
	// publisher and the same allowance as every hint before it.
	buttons []button

	// over is set when the turn's ending is recorded, which is what takes the buttons off
	// a turn whose run produced no text for final to point at.
	over bool

	// ts names the message once it has been posted and published is the text Slack was
	// last given, which is what decides whether another call is worth making. shown says
	// whether it was given the buttons, so taking them off is a change this message writes
	// even where the words it shows are the ones it already showed.
	ts        string
	published string
	shown     bool

	// writes serializes one delivery against another, since the ending is written by
	// whoever recorded it once the publisher has stopped and the two can overlap.
	writes sync.Mutex

	// changed wakes the publisher. It is buffered by one because it is a signal that
	// the state moved, not a queue of the states it moved through.
	changed chan struct{}

	// ending is closed when the turn ends, after which the publisher writes the state
	// one last time and stops.
	ending  chan struct{}
	endOnce sync.Once

	// gone is closed by the publisher before its last write, which hands the writing of
	// anything recorded after that to whoever records it.
	gone chan struct{}
}

// newStatus builds the status message for one turn, or nil where this channel narrates
// nothing. queued says the turn has no worker to start on.
//
// It runs under the channel's lock, where it also records the turn this message's Stop
// button reaches. The two belong together: a channel that posts no status message puts no
// button anywhere, and a press for a turn nothing recorded is answered rather than routed.
func (c *Channel) newStatus(t *turn, queued bool) *status {
	if !c.progress {
		return nil
	}

	// A turn admitted into a drain has nothing to narrate: nothing will hand it over, so
	// the message would be posted and never edited again.
	if c.draining() {
		return nil
	}

	s := &status{
		ch:        c,
		log:       t.log,
		channelID: t.m.ChannelID,
		threadTS:  t.m.ThreadTS,
		queued:    queued,
		quiet:     t.resume,
		buttons:   stopButton(t),
		changed:   make(chan struct{}, 1),
		ending:    make(chan struct{}),
		gone:      make(chan struct{}),
	}

	if len(s.buttons) > 0 {
		c.stoppable[t.id] = t
	}

	return s
}

// The Stop button one status message carries. It is plain rather than emphasized: the
// message it sits on is running commentary, and a red button on every turn shouts.
const (
	// stopActionID names it within its message, which is what Slack requires of an action
	// id and what tells it from the buttons a question is answered on.
	stopActionID = "stop_run"
	labelStop    = "Stop"
)

// stopButton is what a person presses to ask one turn's run to park at its next boundary.
//
// It carries the turn and nothing else. Who may press it is who can see the thread, which
// is who may answer a question there, and a press is placed by the team, channel and thread
// the interaction envelope authenticated rather than by anything the button said.
//
// A value that could not be built leaves the message with no button, since a status message
// that says where the run is, is worth posting either way.
func stopButton(t *turn) []button {
	value, err := encodeValue(buttonValue{Stop: t.id})
	if err != nil {
		t.log.Warn("Building the value the Stop button carries failed", "error", err)

		return nil
	}

	return []button{{ActionID: stopActionID, Label: labelStop, Value: value}}
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
//
// It stops on a drain as well as on the turn's own ending, so a worker shutting down does
// not wait for a goroutine per turn before it closes the socket. The run behind such a turn
// is still going to report, and settle is what writes the ending it reports.
func (s *status) publish() {
	for {
		s.deliver()

		select {
		case <-s.changed:

		case <-s.ending:
			s.leave()

			return

		case <-s.ch.shutdown:
			s.leave()

			return
		}
	}
}

// leave writes the state one last time and hands the next write to whoever records it.
//
// gone is closed before that write rather than after it, so the two orders both land: a
// state recorded before the close is picked up by this write, and one recorded after it is
// written by settle on the goroutine that recorded it.
func (s *status) leave() {
	close(s.gone)
	s.deliver()
}

// settle wakes the publisher, and writes the state itself where a drain has already stopped
// it.
//
// Only the ending takes this path. An intermediate hint recorded after the publisher has
// gone is dropped, as every hint the allowance skipped was, but the ending is what turns
// this message into a pointer at the answer or says why there is none.
func (s *status) settle() {
	select {
	case <-s.gone:
		s.deliver()

	default:
		s.moved()
	}
}

// deliver writes the current state to Slack, having waited for the process's allowance to
// make the call.
//
// The state is read twice, once to decide the call is worth making at all and again once
// the allowance let it through, since a run that moved on while a call was owed is better
// described by where it is than by where it was.
//
// It goes as blocks from the first call, the button being what the message has to be able
// to carry. chat.update leaves the blocks of a message alone unless it is given blocks, so
// a message posted with a button and then edited as text would keep the button for the rest
// of its life.
//
// One delivery at a time: the publisher and the goroutine that recorded an ending can both
// reach this as a drain hands the writing over, and two of them at once would post the
// message twice.
func (s *status) deliver() {
	s.writes.Lock()
	defer s.writes.Unlock()

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

	msg, ts, ok := s.current()
	if !ok {
		return
	}

	if ts == "" {
		posted, err := s.ch.api.postBlocks(ctx, s.channelID, s.threadTS, msg)
		if err != nil {
			s.log.Warn("Posting a status message failed", "error", err)

			return
		}

		s.wrote(posted, msg)

		return
	}

	err = s.ch.api.updateBlocks(ctx, s.channelID, ts, msg)
	if err != nil {
		s.log.Warn("Updating a status message failed", "error", err)

		return
	}

	s.wrote(ts, msg)
}

// pending reports whether Slack has yet to be told the state the turn has reached.
func (s *status) pending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.movedLocked(s.stateLocked())
}

// current is the message to write and the timestamp to write it to, reporting false where
// Slack already shows it.
func (s *status) current() (msg blockMessage, ts string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	msg = s.stateLocked()
	if !s.movedLocked(msg) {
		return blockMessage{}, "", false
	}

	return msg, s.ts, true
}

// stateLocked is the whole of what this message shows: the emoji for the state the turn is
// in, the words, and the Stop button while the turn is live.
//
// The emoji is part of the state rather than something a caller writes onto the text, so it
// changes with the words and goes out on the edit those words already cost.
//
// The button comes off with the ending rather than on its own edit. A turn that ended is a
// turn nothing can park, and the same write that says where the answer is takes away the
// button that would have stopped the run producing it.
func (s *status) stateLocked() blockMessage {
	msg := blockMessage{Text: statusText(s.emojiLocked(), s.textLocked())}
	if s.over || s.final != "" {
		return msg
	}

	msg.Buttons = s.buttons

	return msg
}

// movedLocked reports whether Slack shows something other than msg.
func (s *status) movedLocked(msg blockMessage) bool {
	if s.heldBackLocked() {
		return false
	}

	return msg.Text != s.published || (len(msg.Buttons) > 0) != s.shown
}

// heldBackLocked reports whether this message is one a quiet turn holds back.
//
// The two opening states are what it holds: a turn waiting for a worker and a turn that has
// started and reported nothing say nothing a person needs, and a resume that banks an answer
// and defers again never gets past them. A run that reached a tool, a question, or an ending
// has moved somewhere worth a message, and posts.
//
// The deferral is held back for the same reason. A thread with two questions open takes a
// resume per answer, and each one ends still waiting on the other; the question message is
// already in the thread with its buttons on it, so a message per press says nothing new.
func (s *status) heldBackLocked() bool {
	if !s.quiet || s.ts != "" {
		return false
	}

	line := s.textLocked()

	return line == hintThinking || line == hintQueued || line == deferredNote
}

// wrote records what Slack now shows. A post that failed records nothing, so the next
// write posts rather than editing a message that does not exist.
func (s *status) wrote(ts string, msg blockMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.ts = ts
	s.published = msg.Text
	s.shown = len(msg.Buttons) > 0
}

// emojiLocked is the emoji for the state the words describe. It tests the same states in
// the same order textLocked does, so the icon and the line always describe one state.
//
// Every hint has its own, memory and knowledge included, so a thread scrolled through at
// speed shows what each turn spent its time on without any of them being read.
func (s *status) emojiLocked() string {
	if s.final != "" {
		return s.finalEmoji
	}
	if s.waiting {
		return emojiAsking
	}
	if s.queued {
		return emojiQueued
	}

	return hintEmoji(s.hint)
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
// rather than where the run got to, and icon is the state it ended in.
//
// The two are recorded together because the caller knows both: an answer, a turn somebody
// can carry on from and a failure all end the message, and nothing this file can read off
// the words tells them apart.
//
// It is recorded rather than written: the publisher makes the call, so the last edit is
// spent from the same allowance as every hint that came before it instead of going around
// the meter. An empty text records nothing, chat.update refusing a message with none.
func (s *status) ends(text string, icon string) {
	if s == nil || text == "" {
		return
	}

	s.mu.Lock()
	s.final = text
	s.finalEmoji = icon
	s.queued = false
	s.mu.Unlock()

	s.settle()
}

// stop ends the status message, once the state the turn ended in has been written.
//
// It records that the turn is over, which is what takes the Stop button off a message whose
// run produced no text: a turn nothing is running cannot be parked, so the button goes
// whether or not there is an answer to point at.
//
// It is idempotent: a turn reporting an outcome twice is cheaper to tolerate here than to
// prove impossible.
func (s *status) stop() {
	if s == nil {
		return
	}

	s.mu.Lock()
	s.over = true
	s.mu.Unlock()

	s.endOnce.Do(func() { close(s.ending) })

	// A turn whose publisher a drain already stopped still has a button to take off, so
	// the write happens here rather than waiting for a goroutine that has gone.
	s.settle()
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
