//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	"context"
)

// ClientHooks are the CLIENT-SIDE hooks: they run in the process that asks an agent for
// work, which is a terminal, a bridge or any other program driving a2a, and never in the
// agent. They fire at fixed points of a task so a caller can act on a turn starting, a
// question arriving and a turn ending without reimplementing the loop that recognizes
// them.
//
// The other family is agent.Hooks, the AGENT-SIDE hooks, which run wherever the agent
// loop runs: inside fisk serve, inside a queue worker, or inside the worker a terminal
// embeds for a local run. Those observe model calls, tools and prompts entering a
// conversation. The two are shaped the same way on purpose, a struct of funcs with one
// callback per point and nil meaning not set, composed by wrapping several behaviors in
// one func of your own.
//
// Which side a thing belongs on follows from what it can see. A tool call and a model
// call happen in the agent, so no client hook can observe or stop one; the agent may be
// on another machine. A person typing, a question waiting to be put to them and a turn
// finishing are the client's, and the agent cannot see them: it takes one turn and
// returns with no idea whether another is coming.
//
// Three rules differ from agent.Hooks and each is load-bearing:
//
// They run inline and they block. Seven of the nine are called on the goroutine reading
// the reply set, in the order the messages arrive, so a hook that is slow stops the
// stream being read: no further blocks reach the handler and the turn it is describing is
// held up. QuestionAsked is the sharpest case, since it fires immediately before a person
// is asked. A hook that has slow work to do starts a goroutine for it and returns.
//
// Being inline does not make them single-goroutine, which is the second rule.
// QuestionAnswered fires from the goroutine that puts its question, and CancelRequested
// from whichever goroutine cancels, so either can run at the same time as a hook on the
// reader. An implementation must be safe for concurrent use: one that writes a map
// without a lock works on the agent side and races here.
//
// Running on the reader is what gives the ordering a state consumer needs. Spawning for
// the caller would cost that. Two guarantees follow from it and a consumer tracking what
// a run is doing may rely on both:
//
// A turn the agent acked closes exactly once. TurnEnd fires for an answer, for a failure,
// for a canceled run and for a set whose transport died mid-turn, so a consumer that
// went to working on TurnAccepted always has the report that takes it out again. Err says
// which of those it was. A task whose ack never arrived fires neither, since no turn began.
//
// TurnEnd is last. Both question hooks precede it, including for a question nobody
// answered, so nothing about a question lands after the turn that asked it has closed.
//
// Only PromptSubmit may stop anything, and it is the only one that returns an error.
// It fires before the request is sent, so a denial costs nothing and is honored. Every
// other point reports something that has already happened and returns nothing at all,
// because there is no outcome left for it to change and a client has nowhere to report a
// complaint to; a hook that wants a failure recorded records it itself. They are trusted
// in-process code, as agent.Hooks are, so a panic in one propagates.
//
// They observe rather than drive. A hook never answers a question, sends a turn, cancels
// a task or edits a block. PromptSubmit rewriting its own prompt is the single exception,
// and it exists because the client is the last place a prompt can be changed before it
// leaves the machine.
//
// Every hook must honor ctx.
type ClientHooks struct {
	// PromptSubmit fires before a request is sent, for the prompt a caller is about to
	// submit. It is the last point at which a prompt can be changed or stopped before it
	// crosses the wire, which is where redaction belongs: what is removed here never
	// reaches the agent, its journal, or anything the agent exports.
	//
	// A request that carries no prompt does not fire it. That is a resume, a read or an
	// answer, none of which submits anything.
	PromptSubmit ClientPromptSubmitHook

	// ConversationStart fires when a task opens a conversation, which is an accepting
	// ack carrying a token for a request that named none.
	ConversationStart ConversationStartHook

	// ConversationResume fires when a task continues a conversation the request named,
	// whether it adds a turn, supplies an answer or only reads.
	ConversationResume ConversationResumeHook

	// TurnAccepted fires when the agent accepts the work and the run begins. Between
	// this and TurnEnd the agent is working.
	TurnAccepted TurnAcceptedHook

	// TurnRefused fires when the agent refuses the work, so nothing ran. The code says
	// why, and whether it is worth sending again: capacity and conversation_busy pass,
	// while budget_exhausted and unknown_conversation are properties of the conversation
	// that another attempt will meet again.
	TurnRefused TurnRefusedHook

	// QuestionAsked fires when the agent asks a question, before the handler is given
	// it. The run cannot go on until it is answered, so this is the point at which a
	// caller is blocked on a person rather than on the agent.
	QuestionAsked QuestionAskedHook

	// QuestionAnswered fires once a question is done with: answered and delivered,
	// answered too late to deliver, or given up. It always follows the QuestionAsked
	// that named the same call.
	QuestionAnswered QuestionAnsweredHook

	// TurnEnd fires when a turn the agent acked is over, with what it cost and how it
	// ended. It is the point at which the agent stops working, and it fires for an
	// ending that is not an answer as well as for one that is, including an ending no
	// terminal message described.
	TurnEnd ClientTurnEndHook

	// CancelRequested fires when a cancel is sent, before it goes. The turn does not end
	// here: a cancel asks the run to stop where it can be continued, and its ending
	// arrives as usual through TurnEnd.
	CancelRequested CancelRequestedHook
}

// ClientPromptSubmitHook observes a prompt about to be sent and may deny or rewrite it
// through the returned result. A non-nil error stops the task before anything is sent
// and is returned to the caller. Return the zero result to change nothing.
type ClientPromptSubmitHook func(context.Context, ClientPromptSubmitInfo) (ClientPromptSubmitResult, error)

// ConversationStartHook observes a conversation opening.
type ConversationStartHook func(context.Context, ConversationInfo)

// ConversationResumeHook observes a task continuing a conversation.
type ConversationResumeHook func(context.Context, ConversationInfo)

// TurnAcceptedHook observes the agent taking the work.
type TurnAcceptedHook func(context.Context, TurnAcceptedInfo)

// TurnRefusedHook observes the agent refusing the work.
type TurnRefusedHook func(context.Context, TurnRefusedInfo)

// QuestionAskedHook observes a question arriving, before it is put to anybody.
type QuestionAskedHook func(context.Context, QuestionAskedInfo)

// QuestionAnsweredHook observes a question being finished with.
type QuestionAnsweredHook func(context.Context, QuestionAnsweredInfo)

// ClientTurnEndHook observes a turn ending, whatever it ended as.
type ClientTurnEndHook func(context.Context, ClientTurnEndInfo)

// CancelRequestedHook observes a cancel being sent.
type CancelRequestedHook func(context.Context, CancelRequestedInfo)

// ClientPromptSubmitInfo is the read-only snapshot handed to PromptSubmit.
type ClientPromptSubmitInfo struct {
	// Agent is the identity the request is addressed to.
	Agent string
	// Request is the correlation id this task will carry.
	Request string
	// Conversation is the token the prompt continues, empty when it opens one.
	Conversation string
	// Prompt is the text about to be sent.
	Prompt string
}

// ClientPromptSubmitResult is what PromptSubmit may change. The zero value changes
// nothing.
type ClientPromptSubmitResult struct {
	// Deny stops the task before the request is sent. Nothing reaches the agent and no
	// conversation is opened or continued.
	Deny bool
	// DenyReason says why, for the error the caller receives. Ignored unless Deny.
	DenyReason string
	// Prompt replaces the text sent, when not empty. What the agent receives, journals
	// and answers is this rather than what the caller passed.
	Prompt string
}

// ConversationInfo is the snapshot handed to ConversationStart and ConversationResume.
type ConversationInfo struct {
	// Agent is the identity answering.
	Agent string
	// Request is the correlation id of the task that opened or continued it.
	Request string
	// Conversation is the token, which for ConversationStart is the one just issued.
	Conversation string
}

// TurnAcceptedInfo is the snapshot handed to TurnAccepted.
type TurnAcceptedInfo struct {
	Agent   string
	Request string
	// Conversation is the token this turn runs under.
	Conversation string
}

// TurnRefusedInfo is the snapshot handed to TurnRefused.
type TurnRefusedInfo struct {
	Agent   string
	Request string
	// Reason is what the ack said, in the agent's own words.
	Reason string
}

// QuestionAskedInfo is the snapshot handed to QuestionAsked.
type QuestionAskedInfo struct {
	Agent   string
	Request string
	// QuestionID identifies the question, and ToolUseID the call it is about, so a
	// caller can pair this with the tool call it already drew.
	QuestionID string
	ToolUseID  string
	// Kind is the shape of the question: approve, confirm, select or input.
	Kind ElicitKind
	// Question is the text to put to a person, for confirm, select and input. An
	// approve question carries none and uses Display instead.
	Question string
	// Display is the command line an approve question is about, already sanitized by
	// the agent that sent it.
	Display string
}

// QuestionAnsweredInfo is the snapshot handed to QuestionAnswered.
type QuestionAnsweredInfo struct {
	Agent      string
	Request    string
	QuestionID string
	ToolUseID  string
	// Answered reports whether a reply reached the run. False covers a question given
	// up on and one answered too late to deliver, which the caller keeps to send on a
	// request of its own.
	Answered bool
	// Held reports that what the person decided is being kept for a later request
	// because delivering it failed.
	Held bool
}

// ClientTurnEndInfo is the snapshot handed to TurnEnd.
type ClientTurnEndInfo struct {
	Agent   string
	Request string
	// Conversation is the token this turn ran under, empty when the ack never arrived.
	Conversation string
	// Answered reports whether the turn ended with an answer rather than with an error.
	Answered bool
	// Code is the terminal code for an ending that was not an answer, empty for one
	// that was.
	Code string
	// StopReason is why the run stopped, in the protocol's vocabulary.
	StopReason StopReason
	// Usage is what the conversation has consumed, as the terminal message reports it.
	// Nil from an ending that carried none.
	Usage *Usage
	// Err is why the turn ended when no terminal message said: the set could not be
	// read, or it ended without one, which is ErrIncompleteStream. A canceled run
	// arrives here as context.Canceled. Nil for every ending the agent reported, which
	// is where Code and StopReason say what happened instead.
	Err error
}

// CancelRequestedInfo is the snapshot handed to CancelRequested.
type CancelRequestedInfo struct {
	Agent   string
	Request string
	// Reason is what the caller gave for stopping, which travels to the agent.
	Reason string
}

// WithClientHooks sets the callbacks a client invokes at the points ClientHooks
// documents. Hooks with nil fields are accepted and fire nothing.
func WithClientHooks(h ClientHooks) ClientOption {
	return func(c *Client) {
		c.hooks = h
	}
}

// firePromptSubmit runs the one hook that may stop a task, so its error is returned
// rather than reported.
func (h ClientHooks) firePromptSubmit(ctx context.Context, info ClientPromptSubmitInfo) (ClientPromptSubmitResult, error) {
	if h.PromptSubmit == nil {
		return ClientPromptSubmitResult{}, nil
	}

	return h.PromptSubmit(ctx, info)
}

// The rest report something that has already happened and answer nothing back.

func (h ClientHooks) fireConversationStart(ctx context.Context, info ConversationInfo) {
	if h.ConversationStart != nil {
		h.ConversationStart(ctx, info)
	}
}

func (h ClientHooks) fireConversationResume(ctx context.Context, info ConversationInfo) {
	if h.ConversationResume != nil {
		h.ConversationResume(ctx, info)
	}
}

func (h ClientHooks) fireTurnAccepted(ctx context.Context, info TurnAcceptedInfo) {
	if h.TurnAccepted != nil {
		h.TurnAccepted(ctx, info)
	}
}

func (h ClientHooks) fireTurnRefused(ctx context.Context, info TurnRefusedInfo) {
	if h.TurnRefused != nil {
		h.TurnRefused(ctx, info)
	}
}

func (h ClientHooks) fireQuestionAsked(ctx context.Context, info QuestionAskedInfo) {
	if h.QuestionAsked != nil {
		h.QuestionAsked(ctx, info)
	}
}

func (h ClientHooks) fireQuestionAnswered(ctx context.Context, info QuestionAnsweredInfo) {
	if h.QuestionAnswered != nil {
		h.QuestionAnswered(ctx, info)
	}
}

func (h ClientHooks) fireTurnEnd(ctx context.Context, info ClientTurnEndInfo) {
	if h.TurnEnd != nil {
		h.TurnEnd(ctx, info)
	}
}

func (h ClientHooks) fireCancelRequested(ctx context.Context, info CancelRequestedInfo) {
	if h.CancelRequested != nil {
		h.CancelRequested(ctx, info)
	}
}
