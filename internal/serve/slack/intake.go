//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package slack

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/serve"
)

const (
	// waitingPerWorker multiplies the worker count into the backlog a channel built
	// without a MaxWaiting holds, and defaultMaxCoalesced is how many messages fold into
	// one follow-up turn for a channel built without a MaxCoalesced. NewFromConfig always
	// supplies both; an embedder assembling Options in process reaches these, where a
	// zero would refuse every mention or discard every folded line rather than reading as
	// unset.
	waitingPerWorker    = 2
	defaultMaxCoalesced = 5
)

// defaultReplyDeadline bounds one message this channel posts for itself. It is not a
// run's deadline: what these messages say is owed to a person whether or not the run they
// concern is still alive.
const defaultReplyDeadline = 30 * time.Second

// The lines this channel says for itself, as opposed to anything a run produced. Each is
// a refusal a person has to be able to act on, so each says what happened and what to do
// about it, and none of them names a session, a worker or an error.
const (
	backlogRefusal  = "I have as much waiting as I can hold, so I have not taken this one. Mention me again in a few minutes."
	drainingRefusal = "I am shutting down and have not taken this one. Mention me again once I am back."
	storeRefusal    = "I could not read my record of this thread, so I have not taken this one. Mention me again in a moment."
)

// turn is one admitted mention on its way to being a run: the thread it belongs to, the
// journal that thread runs in, and whatever arrived from the same person while it was
// waiting or running.
//
// A turn holds its thread from the moment it is admitted until its outcome is reported,
// which is what stops two mentions in one thread resuming one journal at once. runstate
// writes a claim and does not enforce one, so this is the only mutual exclusion there is.
//
// folded is guarded by the channel's own mutex rather than one of its own: every decision
// that reads or writes it is an admission decision, and those are already made under that
// lock.
type turn struct {
	ch      *Channel
	m       *mention
	session string
	id      string
	log     *slog.Logger

	folded []*mention
}

// newTurn builds a turn for one mention. It records nothing and admits nothing: admit
// decides where the turn goes.
func (c *Channel) newTurn(m *mention, session string) *turn {
	id := m.ChannelID + "/" + m.TS

	return &turn{
		ch:      c,
		m:       m,
		session: session,
		id:      id,
		log:     c.log.With("turn", id, "session", session, "thread", m.ThreadTS),
	}
}

// intake reads the socket and acts on what arrives. It is the one goroutine that reads
// envelopes, and it runs until the channel is closed or the connection ends.
//
// Nothing it calls does I/O before the acknowledgement. Slack redelivers an envelope that
// is not acknowledged within three seconds, so a read of the session store or a message
// posted on this path would turn a slow store or a slow workspace into the same mention
// arriving again and again.
func (c *Channel) intake() {
	defer close(c.intakeEnd)

	envelopes := c.socket.envelopes()

	for {
		select {
		case <-c.shutdown:
			return

		case env, ok := <-envelopes:
			if !ok {
				return
			}

			c.receive(env)
		}
	}
}

// receive acts on one envelope and acknowledges it.
//
// The order is the whole of the three-second rule: everything that decides is in memory
// and runs first, the acknowledgement follows, and anything that talks to Slack or to the
// store happens after it and somewhere else.
func (c *Channel) receive(env envelope) {
	m, wanted, err := mentionOf(env, c.workspace.UserID)
	if err != nil {
		// An envelope this channel cannot read is still an envelope Slack expects an
		// answer to, so it is acknowledged rather than left to be redelivered forever.
		c.log.Warn("Dropping an envelope that could not be read", "error", err)
	}

	if !wanted {
		c.acknowledge(env)

		return
	}

	if !c.taken.take(m.ChannelID, m.TS) {
		c.log.Debug("Dropping a message Slack delivered again", "channel", m.ChannelID, "message", m.TS, "attempt", env.RetryAttempt)
		c.acknowledge(env)

		return
	}

	refusal := c.admit(m)

	c.acknowledge(env)

	if refusal != "" {
		c.log.Warn("Refusing a mention", "channel", m.ChannelID, "thread", m.ThreadTS, "reason", refusal)
		c.reply(m, refusal)
	}
}

// acknowledge answers one envelope. A failure is logged and nothing else: the answer to
// an acknowledgement Slack did not receive is the redelivery it is already making, which
// the dedupe drops.
func (c *Channel) acknowledge(env envelope) {
	err := c.socket.ack(env.ID)
	if err != nil {
		c.log.Warn("Acknowledging an envelope failed", "envelope", env.ID, "error", err)
	}
}

// admit decides what becomes of a mention, and returns the refusal to post back or an
// empty string when the mention was taken.
//
// There are four answers. A thread nothing is running in takes the mention as a turn and
// waits for a worker. A thread running a turn for the same person folds it into that
// turn, so three lines typed in ten seconds arrive as one thought. A thread running a
// turn for somebody else queues it behind, since Work.Caller, the Stop button and the
// next question each have exactly one owner and folding two people together would leave
// them with two. A backlog already at MaxWaiting refuses, because a person told to come
// back is better served than one watching a queued message for three minutes.
//
// A mention past the coalescing cap is queued behind rather than dropped. The cap bounds
// how much one follow-up turn carries; it does not license discarding what somebody
// wrote.
//
// It decides in memory and holds no I/O of any kind, which is what lets it run before the
// acknowledgement. That placement is the point: it is the only mutual exclusion between
// two concurrent resumes of one thread.
func (c *Channel) admit(m *mention) string {
	session := SessionFor(c.identity, m.TeamID, m.ChannelID, m.ThreadTS)

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.draining() {
		return drainingRefusal
	}

	running, inFlight := c.inFlight[session]
	if inFlight && running.m.UserID == m.UserID && len(running.folded) < c.maxCoal {
		running.folded = append(running.folded, m)
		running.log.Info("Folding a mention into the turn already running for this person", "message", m.TS, "folded", len(running.folded))

		return ""
	}

	if len(c.waiting)+c.parked >= c.maxWait {
		return backlogRefusal
	}

	t := c.newTurn(m, session)

	if inFlight {
		c.queued[session] = append(c.queued[session], t)
		c.parked++
		t.log.Info("Queueing a mention behind the turn running in its thread", "user", m.UserID)

		return ""
	}

	c.inFlight[session] = t
	c.waiting = append(c.waiting, t)
	c.wakeNext()

	t.log.Info("Admitted a mention", "user", m.UserID, "waiting", len(c.waiting))

	return ""
}

// wakeNext tells Next there may be something to take. It is a signal rather than a
// handover: the queue is what holds the work, so a wake that is already pending is
// enough and this never blocks the goroutine that reads envelopes.
//
// It is called with the mutex held.
func (c *Channel) wakeNext() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// takeWaiting pops the turn Next hands over, or nil when nothing is waiting.
func (c *Channel) takeWaiting() *turn {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.waiting) == 0 {
		return nil
	}

	t := c.waiting[0]
	c.waiting = c.waiting[1:]

	return t
}

// workFor turns an admitted turn into the work the server runs.
//
// This is where the session store is read, which is why it happens here rather than at
// admission: held tells a thread this worker is continuing from one it is opening, and a
// read of the store on the intake goroutine would risk the three-second acknowledgement.
// It is also read as late as it can be, immediately before the handover, so a turn queued
// behind another one asks about the journal the turn in front of it left rather than the
// one it found on arrival.
func (c *Channel) workFor(ctx context.Context, t *turn) (*serve.Work, error) {
	held, err := c.held(t.session)
	if err != nil {
		return nil, err
	}

	work := t.work(checkpointFor(t.session, held))

	// The two reads answer to the same allowance and reach the run by different routes. An
	// opening turn's surrounding conversation is supporting material, which is what
	// Work.Context is for. A follow-up's gap is part of what the person is saying, since
	// "ok do that then" is only a sentence alongside the discussion it answers, so it goes
	// in the prompt above their words.
	//
	// A read that fails does not fail the turn. The answer is worse for missing its
	// context and a person who asked a question would rather have it answered narrowly
	// than not at all, so the failure is logged and the run goes ahead.
	if held {
		gap, err := c.gap(ctx, t.m)
		if err != nil {
			t.log.Warn("Reading what was said since this bot last spoke failed", "error", err)
		}

		rendered := c.render(ctx, gap)
		if rendered != "" {
			work.Prompt = "Said in this thread since I last replied:\n" + rendered + "\n\n" + work.Prompt
		}

		return work, nil
	}

	pre, err := c.preload(ctx, t.m)
	if err != nil {
		t.log.Warn("Reading the conversation around a mention failed", "error", err)
	}

	work.Context = c.render(ctx, pre)

	return work, nil
}

// work is the unit the server runs.
//
// ID is the mention's channel and timestamp, which is Slack's to mint rather than
// anybody's to choose, and it names this turn in the worker's logs beside work from every
// other endpoint. ClaimedBy is the same string: it is the most specific thing this
// channel holds, and a worker answering in many threads under one identity would
// otherwise stamp every claim in every journal identically.
//
// HumanPaced is set because a Slack thread is a person's pace by definition: the next
// turn arrives when somebody types it, so the gap before this history is used again is
// think time rather than a loop's.
func (t *turn) work(checkpoint agent.Checkpoint) *serve.Work {
	return &serve.Work{
		ID:               t.id,
		Prompt:           t.prompt(),
		Checkpoint:       checkpoint,
		ClaimedBy:        t.id,
		Caller:           callerOf(t.m),
		SuspendRequested: t.ch.suspendRequested,
		HumanPaced:       true,
		Done:             t.done,
	}
}

// prompt is what the person asked, with anything that folded in before the handover
// joined onto it.
//
// The buffer is drained here rather than at the ending, so a second line typed while the
// turn was still waiting for a worker is answered in the same turn instead of paying for
// another one. It runs under the same lock admission folds under, so a line either
// reaches this prompt or waits for the follow-up, and none is lost between the two.
func (t *turn) prompt() string {
	t.ch.mu.Lock()
	defer t.ch.mu.Unlock()

	lines := linesOf(append([]*mention{t.m}, t.folded...))
	t.folded = nil

	return strings.Join(lines, "\n")
}

// suspendRequested is what every run of this channel polls at a loop boundary: the
// worker's own drain signal, and this channel being closed. Either parks the run
// somewhere a later mention in the thread continues from.
func (c *Channel) suspendRequested() bool {
	if c.suspend != nil && c.suspend() {
		return true
	}

	return c.draining()
}

// done reports what a turn produced and gives its thread back.
//
// The server calls it exactly once, on a context that is not the run's, so a turn whose
// run was canceled still releases the thread it holds.
func (t *turn) done(_ context.Context, out serve.Outcome) error {
	t.ch.finish(t, out)

	return nil
}

// finish delivers or returns what folded in while the turn ran, then releases the thread
// and starts whatever was waiting behind it.
//
// The folded lines are delivered only when the run reached a boundary a user message can
// join. A run that deferred on a question has not: the follow-up would be neither
// journaled nor answered, which Outcome.FollowUpTaken reports and no amount of retrying
// here repairs. A run that suspended has not either, since somebody pressing Stop should
// not see a fresh run start seconds later. In both cases the lines go back to the thread
// as one message so the person can send them again.
func (c *Channel) finish(t *turn, out serve.Outcome) {
	c.mu.Lock()

	folded := t.folded
	t.folded = nil

	var undelivered []*mention

	if len(folded) > 0 {
		if reachedUserBoundary(out) {
			// In front of anything queued behind, since it continues the turn that has
			// just ended rather than starting a new one, and its lines were written to
			// that run.
			follow := c.newTurn(joined(folded), t.session)
			c.queued[t.session] = append([]*turn{follow}, c.queued[t.session]...)
			c.parked++
		} else {
			undelivered = folded
		}
	}

	c.releaseLocked(t)

	c.mu.Unlock()

	t.log.Info("A turn ended", "reason", out.Reason, "deferred", len(out.Deferred), "folded", len(folded), "returned", len(undelivered))

	if len(undelivered) > 0 {
		c.reply(t.m, undeliveredNote(linesOf(undelivered)))
	}
}

// releaseLocked gives a thread back and promotes what was queued behind it, which is the
// next turn's admission: it takes the thread the way the turn that just ended held it, so
// the mutual exclusion never lapses between the two.
//
// A turn that no longer holds its thread releases nothing. Nothing produces that today,
// and the check is what keeps a second ending from handing the thread of a turn that has
// already started to somebody else.
func (c *Channel) releaseLocked(t *turn) {
	if c.inFlight[t.session] != t {
		return
	}

	behind := c.queued[t.session]
	if len(behind) == 0 {
		delete(c.inFlight, t.session)
		delete(c.queued, t.session)

		return
	}

	next := behind[0]
	c.parked--

	if len(behind) == 1 {
		delete(c.queued, t.session)
	} else {
		c.queued[t.session] = behind[1:]
	}

	c.inFlight[t.session] = next
	c.waiting = append(c.waiting, next)
	c.wakeNext()
}

// reachedUserBoundary reports whether a run ended somewhere the conversation can take
// another user message.
//
// A deferred call and a suspend are the two endings that are not one. Every other ending
// is: a completed turn, a turn that ran out of steps, a failure, and work that was taken
// and never started all leave the stored conversation able to take a prompt, or leave
// nothing at all, and in both cases sending the folded lines is what the person asked
// for.
func reachedUserBoundary(out serve.Outcome) bool {
	return len(out.Deferred) == 0 && out.Reason != runstate.ReasonSuspended
}

// joined is the follow-up turn's mention: one message carrying every folded line, under
// the identity of the first of them.
//
// The first is what names it rather than the last, so the work id and the thread agree
// with the first thing the person said after their turn started, which is the message
// they would look for.
func joined(folded []*mention) *mention {
	out := *folded[0]
	out.Text = strings.Join(linesOf(folded), "\n")

	return &out
}

// linesOf is what a set of messages said, with anything empty left out: a mention
// carrying no words is an address rather than a line of a prompt.
func linesOf(msgs []*mention) []string {
	out := make([]string, 0, len(msgs))

	for _, m := range msgs {
		if m.Text == "" {
			continue
		}

		out = append(out, m.Text)
	}

	return out
}

// undeliveredNote is what a person is told about lines a run never reached. It is one
// message rather than one per line, since the run they were written for has ended and
// three notifications say no more than one.
func undeliveredNote(lines []string) string {
	return "I did not get to:\n- " + strings.Join(lines, "\n- ") + "\nSend them again if they still matter."
}

// reply posts one message this channel says for itself, on a goroutine and on a deadline
// of its own.
//
// It is never called before an envelope has been acknowledged. The context is this
// channel's rather than a caller's because a refusal has no run to belong to and a person
// who was refused is owed the reason even while the worker is shutting down, which is why
// Close waits for these.
//
// Once Close has stopped waiting the message is posted on the caller's own goroutine
// instead. A run reporting its outcome after the drain has moved past that point still
// owes its thread the lines it never got to, and the socket is not closed until the runs
// have ended.
func (c *Channel) reply(m *mention, text string) {
	c.mu.Lock()
	closed := c.postsClosed
	if !closed {
		c.posts.Add(1)
	}
	c.mu.Unlock()

	if closed {
		c.post(m, text)

		return
	}

	go func() {
		defer c.posts.Done()

		c.post(m, text)
	}()
}

// post writes one of this channel's own messages into a thread, on a deadline that is not
// a run's: what it says is owed to a person whether or not the run it concerns is still
// alive.
func (c *Channel) post(m *mention, text string) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultReplyDeadline)
	defer cancel()

	_, err := c.api.postMessage(ctx, m.ChannelID, m.ThreadTS, text)
	if err != nil {
		c.log.Warn("Posting a reply failed", "channel", m.ChannelID, "thread", m.ThreadTS, "error", err)
	}
}

// awaitPosts waits for the messages this channel started, having first closed the door on
// new ones: a goroutine started while the wait is in progress is what makes a WaitGroup
// panic, and the flag is set under the same lock every start takes.
func (c *Channel) awaitPosts() {
	c.mu.Lock()
	c.postsClosed = true
	c.mu.Unlock()

	c.posts.Wait()
}
