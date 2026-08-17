//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// Package serve hosts an agent behind one or more channels: it takes work from
// them, runs it, and reports what each run produced.
//
// A channel is a calling endpoint. A work queue, a NATS binding, an HTTP listener and
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
	"time"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/util"
)

// ErrChannelDone reports that a channel has no more work and never will. A server
// stops serving that channel without treating it as a failure, which is how a
// finite channel (a fixed batch, a test double) ends cleanly.
var ErrChannelDone = errors.New("channel has no more work")

// Channel is a calling endpoint an agent is hosted behind.
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

// Service is an endpoint a server hosts that answers its callers directly instead of
// producing work.
//
// A tool call served to a peer runs one tool and returns. No prompt is involved, no
// run is journaled and nothing reaches the agent loop, so there is no Work to hand
// over and no Outcome to report. That is the whole of the difference between the two
// kinds of endpoint, and the reason a server takes both.
//
// A service answers from the moment it is built. Nothing here starts it: the
// constructor that registers its handlers is what makes it live, which is why New
// releases the services it was given when it refuses its options rather than leaving
// them answering in a process that serves nothing.
//
// An implementation is called concurrently, by whatever transport it listens on and
// while the server runs its channels, so its state must be safe for concurrent use.
type Service interface {
	// Name identifies the service in logs and on a program's startup banner. It
	// should be stable and short.
	Name() string

	// Close stops the service answering.
	//
	// It is required rather than optional, which is where this departs from
	// ReleasableChannel: a channel is pulled from and may hold nothing, while a
	// service is called and therefore always holds the registration that lets it be
	// called.
	//
	// Drain closes services as well as channels, so a service stops answering when a
	// worker begins shutting down rather than when it finishes. A endpoint that shares
	// a queue group with its siblings sheds to them that way.
	//
	// It must tolerate being called more than once. A program that drains on one
	// signal and stops on the next releases every endpoint twice.
	Close() error
}

// FaultingEndpoint is the optional interface an endpoint implements when it can stop
// working for a reason nobody asked for and nothing here can see: a registration the
// substrate dropped, a subscription that overflowed, a listener that died.
//
// Serve ends when a fault arrives, draining what is in flight first and returning the
// error, so the program exits non-zero and a supervisor restarts it. That is the only
// answer available: an endpoint that has stopped answering cannot be restarted from
// here, and a worker whose endpoints are gone keeps running while doing nothing.
//
// A endpoint that cannot fail this way does not implement it, as a channel holding
// nothing does not implement ReleasableChannel.
type FaultingEndpoint interface {
	// Faults yields at most one fault per endpoint lifetime; a nil channel never
	// yields. It is read once, when Serve starts, and never closed by the reader.
	Faults() <-chan error
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
	// A resumed run replaces its conversation with the journaled one and discards
	// Prompt, unless Checkpoint.FollowUp says to deliver it as the conversation's next
	// turn. A channel whose deliveries can repeat must leave FollowUp unset: a queue
	// cannot tell a first delivery from a redelivery, so a redelivered item carrying a
	// follow-up would append the same prompt to the conversation again and pay for
	// another turn on it. Setting it is for a channel where each delivery is a caller
	// asking for something, which is what a live caller sending a second prompt is.
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

	// PromptsMayBlock says this channel bounds its own questions, so the server holds
	// none of them to PromptWait. Two kinds of channel set it: one whose operator is on
	// the other end of a live connection, where the run context is the only bound; and
	// one that holds the question against evidence the operator is still there, as the
	// a2a prompt channel does with the replies a caller sends while a question is on
	// its screen.
	//
	// False, the zero value, bounds each question by PromptWait, which is what a
	// channel with nobody attached needs. Human think-time is minutes to days and a
	// worker held for it serves nothing, so an unanswered question gives the run back
	// instead: a question a tool asked defers, and a gate question leaves its call
	// unanswered for the next resume to ask again.
	PromptsMayBlock bool

	// PromptWait is how long one question is held open. A non-positive value takes two
	// minutes, the default expose.agent.a2a.request_timeout carries, since waiting for
	// a caller to answer is the same measurement. PromptsMayBlock set ignores it.
	//
	// No channel in this repository sets it, every one of them either supplying no
	// prompter or bounding its own questions. It stays for an embedder's channel that
	// can reach an operator but cannot tell whether one is still there.
	PromptWait time.Duration

	// Continue is called at a turn boundary for the next turn, holding the run open
	// while it blocks. Nil is one shot.
	Continue func(context.Context) agent.Continuation

	// RunContext derives the context this work's run executes under from the server's
	// own. A channel supplies it to stop one run without stopping the server, or to
	// carry a caller's trace onto the run. Nil runs the work on the server's context.
	//
	// The server calls it once, on its own goroutine, immediately before the run and
	// after the run's slot was acquired, so a channel learns from it that its work is
	// about to start. Work that is taken and never started reports Abandoned without
	// it being called. The returned cancel is called once the run has ended, and a
	// channel that kept the cancel may call it earlier, which is how a caller's cancel
	// reaches a run.
	//
	// A nil context leaves the run on the server's own and a nil cancel is not called,
	// so a channel's mistake here does not take a worker down.
	RunContext func(context.Context) (context.Context, context.CancelFunc)

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
	// Name is the channel's own term for the caller, empty when it knows none.
	Name string

	// Verified reports whether the channel authenticated Name. A false value means
	// nothing is being vouched for, whether or not Name is set, so anything reading
	// this for a decision must read it and not Name alone. A channel over a transport
	// that authenticates nobody leaves it false and still fills Name with the claim
	// the caller made, which is worth recording and worth nothing as evidence.
	Verified bool
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

	// Deferred lists the tool calls the run is waiting on an answer for. It is what
	// separates a suspend nobody asked for from a drain: a channel draining sees a
	// suspend with nothing here, while a run that called a tool answering later
	// suspends naming the call.
	//
	// The work is not finished and not failed. It resumes under the same session once
	// every one of these is answered, which is what a channel decides how to arrange.
	Deferred []agent.DeferredCall

	// FollowUpTaken reports whether a Checkpoint.FollowUp prompt entered the
	// conversation. It is false where the stored conversation reached no boundary that
	// could take a user message, which is one waiting on a deferred tool result: the
	// prompt was neither journaled nor answered and has to be sent again. A channel that
	// sets FollowUp tells its caller from it; every other channel gets false and has
	// nothing to report.
	FollowUpTaken bool
}
