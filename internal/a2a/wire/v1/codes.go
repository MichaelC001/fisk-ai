//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package wire

// The codes a terminal error carries, so a caller decides on a value rather than on
// prose. A refusal names why the work was refused; an ending names how the run ended.
//
// They are here rather than with the endpoint that produces them because both ends need
// them: one to say how a run ended and the other to decide what to do about it. A code a
// receiver does not know is still a code, and a caller that cannot act on one shows the
// message beside it.
const (
	// CodeRejected means admission refused the caller.
	CodeRejected = "rejected"
	// CodeCapacity is work refused because the answering agent is already running as
	// much as it will run at once. Nothing was started, so the same work may succeed
	// against a peer with a free slot, or against this one later.
	CodeCapacity = "capacity"
	// CodeDuplicate means a request with this id is already running here.
	CodeDuplicate = "duplicate_request"
	// CodeDraining means the worker is going out of service and took nothing on.
	CodeDraining = "draining"
	// CodeFailed means the run ran and failed; the message says how.
	CodeFailed = "failed"
	// CodeCrashed means a bug in the software running the agent. The detail stays in
	// that worker's log.
	CodeCrashed = "crashed"
	// CodeNotStarted means the work was taken and then abandoned without running.
	CodeNotStarted = "not_started"
	// CodeDeferred means a tool will answer later, so the run is parked with the
	// conversation waiting on it.
	CodeDeferred = "deferred"
	// CodeSuspended means the run stopped at a point a resume can continue from, which
	// is what a cancel asks for and what a drain produces.
	CodeSuspended = "suspended"
	// CodeCanceled means the worker stopped the run rather than the caller asking it
	// to stop at a boundary.
	CodeCanceled = "canceled"

	// CodeUnknownConversation means the conversation token names nothing here: send
	// the prompt without one to start a conversation.
	CodeUnknownConversation = "unknown_conversation"
	// CodeConversationBusy means a turn of this conversation is already running: wait
	// for its terminal message rather than trying another worker.
	CodeConversationBusy = "conversation_busy"
	// CodeTurnNotTaken means the conversation could not take the turn, so the prompt
	// did not run and was not journaled.
	CodeTurnNotTaken = "turn_not_taken"
	// CodeConfigDrift means the agent's configuration changed in a way the stored
	// conversation does not survive, so the resume was refused and nothing ran. The
	// message lists what changed. A caller may send the same request again asking for
	// the resume to be forced, which continues the conversation under the current
	// configuration.
	CodeConfigDrift = "config_drift"
	// CodeBudgetExhausted means the conversation has processed its whole token budget,
	// so it took no turn and will take none: the prompt did not run and was not
	// journaled. It is permanent for that conversation, whoever sends the next turn,
	// since the allowance is a property of the conversation rather than of the caller.
	// Starting a conversation is what gets a fresh allowance.
	CodeBudgetExhausted = "budget_exhausted"

	// CodeProviderBusy means the agent's model provider refused the call for want of
	// capacity, or because the account is over its rate limit. Nothing about the
	// request is wrong and every worker of this identity calls the same provider, so a
	// caller waits and sends the same work again rather than trying a peer.
	//
	// The turn may have run some way first: a loop that called tools for three
	// iterations and met the refusal on the fourth journaled those three.
	CodeProviderBusy = "provider_busy"
	// CodeProviderRefused means the agent cannot use its model provider at all: the
	// provider rejected its credentials, or the configured model does not exist. Every
	// worker of this identity runs the same configuration, so a retry here and a retry
	// against a sibling both reach this. A caller with another agent to ask asks it; an
	// operator fixes this one.
	CodeProviderRefused = "provider_refused"
	// CodeContextExceeded means the conversation holds more than the model's context
	// window takes, so the model refused the call. A first prompt reaches it by carrying
	// too much context, and sending less is what gets a turn. A conversation reaches it
	// by growing, and no further turn of it will run, so a caller starts a conversation
	// instead.
	//
	// The conversation is not left as it was. The window is measured at the call, which
	// is after the prompt was journaled, so the journal holds a user turn with no answer
	// after it.
	CodeContextExceeded = "context_exceeded"

	// CodeUnknownCall means no such call is waiting for an answer.
	CodeUnknownCall = "unknown_call"
	// CodeAlreadyAnswered means the call already has an answer.
	CodeAlreadyAnswered = "already_answered"
	// CodeAnswerTooLarge means the answer is over the size an answer may carry.
	CodeAnswerTooLarge = "answer_too_large"
)
