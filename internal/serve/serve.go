//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// Package serve hosts an agent behind one or more channels: it takes work from
// them, runs it, and reports what each run produced.
//
// A channel is a calling surface. A work queue, a NATS binding, an HTTP listener and
// an in-process caller are all channels, and they differ in what they can do rather
// than in kind. How work reaches a channel is the channel's own business: the
// interface here names no transport, so a channel is proxy and binding in one and
// the server never learns which.
//
// A channel supplies the attachment points agent.Run already takes, rather than a
// vocabulary defined beside them. What a channel can do is therefore what it
// supplies: a channel that can put a question to a human provides a Prompter, and one
// that cannot leaves it nil. There is no separate declaration to disagree with
// reality.
package serve

import (
	"context"
	"errors"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/util"
)

// ErrChannelDone reports that a channel has no more work and never will. A server
// stops serving that channel without treating it as a failure, which is how a
// finite channel (a fixed batch, a test double) ends cleanly.
var ErrChannelDone = errors.New("channel has no more work")

// Channel is a calling surface an agent is hosted behind.
//
// An implementation is used from one goroutine at a time for Next, but the sinks it
// hands out on Work run concurrently with each other and with Next, so any state a
// channel shares between them must be safe for concurrent use.
type Channel interface {
	// Name identifies the channel in logs and metrics. It should be stable and short.
	Name() string

	// Next blocks until work is available and returns it.
	//
	// It returns ErrChannelDone when the channel is finished, and ctx.Err() when the
	// context is canceled. Any other error is logged and retried after a delay, so a
	// transient failure to reach a queue does not end the channel. A nil error
	// guarantees a non-nil Work.
	Next(ctx context.Context) (*Work, error)
}

// ConcurrentChannel is the optional interface a channel implements when it knows how
// many of its runs may be in flight. A channel that claims work before a run starts has
// to size that claiming to something, and only it knows what, so it states the number
// rather than being told one and hoping the server agrees.
//
// A channel that does not implement it, or that answers with zero or less, gets
// Options.Concurrency.
type ConcurrentChannel interface {
	Channel

	// Concurrency is how many runs of this channel's work may execute at once.
	Concurrency() int
}

// ReleasableChannel is the optional interface a channel implements when it holds
// something that has to be given back, which is most of them: a connection, a
// subscription, a client with goroutines behind it. A channel holding nothing does not
// implement it and is skipped wherever channels are released.
//
// Close means stop producing work, not stop working. A blocked Next returns
// ErrChannelDone and no further work is handed over, but runs already in flight are the
// server's to wait for rather than the channel's, and Serve does not return until they
// have ended and reported.
//
// It must tolerate being called more than once. A program draining on one signal and
// stopping on the next releases every channel twice, and New releases them itself when
// it refuses its options, so the second call has to be harmless rather than an error.
//
// The method set is io.Closer's, so an existing io.Closer satisfies this without
// changing. It is named here because what closing means is the part an implementer
// needs and the stdlib interface cannot say.
type ReleasableChannel interface {
	Channel

	Close() error
}

// Work is one unit of work a channel supplies: what to do, and how to talk to
// whoever asked for it.
//
// The optional fields are what the channel can do. A queue supplies none of them and
// gets a one-shot run with no operator; a channel fronting a live caller supplies the
// ones it can honor.
type Work struct {
	// ID identifies this work in the server's logs and metrics. It does not name the
	// journal: that is the session, which Checkpoint governs. A channel with an
	// identifier of its own supplies it and owns its uniqueness across channels; an
	// empty one is minted and reported back on Outcome.
	ID string

	// Prompt is what the agent is asked to do.
	Prompt string

	// Context is optional supporting material, offered to the model alongside the
	// prompt.
	Context string

	// Checkpoint asks for a journaled run, or resumes one. For a queued job it is
	// crash recovery: a redelivered item resumes its session rather than paying again
	// for the model calls a previous attempt already made. The zero value runs without
	// a journal.
	//
	// It does not carry a follow-up turn. A resumed run replaces its conversation with
	// the journaled one, so Prompt is not delivered to a run that resumes.
	Checkpoint agent.Checkpoint

	// ClaimedBy names this worker in the claim a resumed run writes to its journal,
	// for a person reading it later. Empty leaves the run to derive one from the agent
	// identity, the host and the pid.
	//
	// A channel should set it wherever it holds something more specific than the
	// process, which is most of the time: one worker serving many pieces of work under
	// one identity would otherwise stamp every claim in every journal identically,
	// which is exactly what a reader is trying to tell apart. A channel's own item
	// identifier is the better answer and only the channel has it. It is never verified
	// and nothing decides anything on it.
	ClaimedBy string

	// SuspendRequested is polled at a loop boundary, so a worker draining on shutdown
	// stops its runs at a point they can resume from. Nil never suspends.
	SuspendRequested func() bool

	// Budget lowers the process limits for this work. It may only lower them.
	Budget Budget

	// Caller is what the channel knows about who asked. It is recorded and logged; no
	// policy consults it.
	Caller Caller

	// Events receives the run's narration. Nil discards it. Whatever is supplied is
	// wrapped, so the server still records what it needs from a run whose channel
	// wants no narration at all.
	Events agent.Events

	// Prompter answers a question the run asks part way through a turn. Nil means no
	// operator is reachable and every confirmation-gated tool is refused, which is the
	// correct answer for a queue.
	Prompter toolkit.Prompter

	// Continue is called at a turn boundary for the next turn, holding the run open
	// while it blocks. Nil is one shot.
	Continue func(context.Context) agent.Continuation

	// Done reports the outcome exactly once, on a context that is not the run's so a
	// canceled or timed-out run still records what happened. A non-nil error is
	// logged; the server has nowhere else to take it. Required.
	Done func(context.Context, Outcome) error
}

// Budget bounds what one piece of work may spend. A zero field is unset and leaves
// the configured limit alone; a value above the configured limit is lowered to it,
// since local configuration is the ceiling.
type Budget struct {
	// MaxTokens caps total tokens across the run.
	MaxTokens int64
	// MaxIterations caps how many times the loop may call the model.
	MaxIterations int64
}

// Caller is what a channel knows about who asked for a piece of work. It is carried
// and logged rather than enforced: nothing here authorizes on it, and a channel that
// cannot identify its caller leaves it zero.
type Caller struct {
	// Name is the channel's own term for the caller. It is not verified.
	Name string
}

// Outcome is what a run produced, in terms a channel can record without inspecting
// Go error types.
//
// Reason alone does not separate the cases a caller has to act on differently: a
// crash and a failure during setup both leave it unset, and a model refusal, a reply
// truncated at the output cap, a provider failure and an aborting hook all report the
// error reason. The three flags below carry what Reason cannot.
type Outcome struct {
	// ID is the work's identifier, minted by the server when the channel supplied
	// none.
	ID string

	// SessionID is the journaled session, empty when the run was not checkpointed.
	SessionID string

	// Text is the last thing the agent said, empty when it said nothing. Pair it with
	// Reason before treating it as an answer.
	Text string

	// Reason is the run's terminal outcome, empty when it never reached one.
	Reason runstate.TerminalReason

	// Stats is the run's accounting, nil when the run failed before it started.
	Stats *util.RunStats

	// Err is the failure, nil on success. Note that a run stopped by its budget or its
	// iteration cap reports both a reason and an error.
	Err error

	// Rejected reports that admission refused the work before it ran, so a caller
	// knows a retry will not help. Nothing refuses work yet: Caller is recorded
	// rather than enforced, so this is always false until a policy consults it.
	Rejected bool

	// Abandoned reports that the work was taken but never started, because the server
	// shut down while it waited for a slot. Nothing ran, so a retry is safe.
	Abandoned bool

	// Crashed reports a bug in this software rather than an outcome of the work, so a
	// caller can escalate it instead of recording it as a result.
	Crashed bool
}
