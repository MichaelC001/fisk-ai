//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package web

import (
	"net/http"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/serve"
	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// Format is one frontend protocol: it reads a request into the turn it asks for and
// writes the run back in that protocol's own shape. The channel never names one, so a
// third format is one package and one Mount.
//
// A Format is called concurrently, once per request, and holds no state between
// requests: several processes may serve one page behind a load balancer, so anything
// remembered in one of them is wrong in the others.
type Format interface {
	// Decode reads r into the Turn it asks for and returns the TurnWriter the response
	// is written with. The writer holds w and writes nothing until Open is called on
	// it, so the channel can answer a request that never starts a run with a status
	// code of its own.
	//
	// An error is answered by the channel as a 400 naming it, so it describes what is
	// wrong with the request and nothing about this worker. A Format returning an
	// error has written nothing to w.
	Decode(w http.ResponseWriter, r *http.Request) (Turn, TurnWriter, error)

	// Replayer returns the writer a stored conversation is written into, for a page
	// opening a conversation from the session list. The request carries no turn: the
	// channel has already read the conversation, and it calls Open, Replay and Close on
	// what this returns.
	//
	// It is separate from Decode because a stored conversation asks for none of what
	// Decode reads. r is here for a Format that answers a header or a query parameter
	// of its own.
	Replayer(w http.ResponseWriter, r *http.Request) TurnWriter
}

// Mount is one Format and the path segment it answers on under the channel's base
// path, so a Format mounted at "vercel" under "/fisk/v1" answers POST /fisk/v1/vercel.
type Mount struct {
	// Path is a single segment: no slash, and unique among the mounts of one channel.
	Path string
	// Format answers the requests that reach Path.
	Format Format
}

// Turn is what one request asks of a conversation.
//
// A request carries a prompt, an answer, or both. A prompt on a thread the store does
// not hold creates the conversation; on a thread it holds, the prompt is the
// conversation's next turn. An answer resumes the conversation on the question it
// stopped at, and a request carrying both answers first and the prompt is the turn
// after the answered one finishes.
type Turn struct {
	// ThreadID is the browser's name for the conversation. It is hashed with the
	// serving identity into the session id, so a page reaches only the conversations
	// this identity minted for it.
	ThreadID string

	// Prompt is what the person typed, empty on a request that only answers.
	Prompt string

	// Answer is the page's answer to the question the last turn ended on, nil on a
	// request that only prompts. The channel holds it for this one turn and the
	// prompter answers the question from it when the resumed run asks again.
	Answer *Answer

	// Caller is what the request claims about who sent it, empty when it claims
	// nothing. The channel records it on the run's Caller with Verified false: this
	// transport authenticates nobody.
	Caller string
}

// QuestionKind is which of the four questions a run asks.
type QuestionKind string

const (
	// KindApprove is the confirm gate's three-way approval of a command that has not
	// run.
	KindApprove QuestionKind = "approve"
	// KindConfirm is ask_human_confirm, KindSelect is ask_human_select and KindInput
	// is ask_human_input.
	KindConfirm QuestionKind = "confirm"
	KindSelect  QuestionKind = "select"
	KindInput   QuestionKind = "input"
)

// Question is one question a run asked, carrying a kind and the union of the four
// questions' fields. It is asked on the response that ends the turn, since the page
// reading the response cannot answer until the response is over.
type Question struct {
	// Kind says which of the four this is, and which fields below are set.
	Kind QuestionKind

	// ToolUseID is the call the question belongs to. The question is asked again under
	// the same call on every resume, so the call is the one thing the asking end and the
	// answering end agree an answer is about, and it keys the answer the next request
	// carries.
	ToolUseID string

	// Command, Display and Tag describe an approval: the command path, the sanitized
	// command line as the operator sees it, and the tag that gated it. They are set
	// for KindApprove only.
	Command string
	Display string
	Tag     string

	// Text is the question in words, for the three human-in-the-loop kinds.
	Text string

	// Options are what a selection chooses between, for KindSelect.
	Options []string

	// Default pre-fills a free-text answer, for KindInput.
	Default string
}

// Answer is the page's answer to a Question, carried on the request that resumes the
// conversation.
//
// It is keyed by ToolUseID rather than by a question id: the question is put again on
// every resume, so the call is what both ends agree the answer belongs to, and it is what
// stops a resent or stale answer landing on whichever question the resume asks first.
type Answer struct {
	// ToolUseID names the call answered. Required.
	ToolUseID string

	// Kind says which question this answers, and which field below carries the answer.
	// An answer whose kind does not match the question asked about the call is not
	// used, and the question is asked again.
	Kind QuestionKind

	// Approval is the answer to KindApprove.
	Approval toolkit.ConfirmChoice
	// Confirmed is the answer to KindConfirm.
	Confirmed bool
	// Index is the answer to KindSelect, the position of the option chosen.
	Index int
	// Value is the answer to KindInput. An empty string is a valid answer.
	Value string
}

// Ending is what a turn ended with, handed to the writer's Close.
type Ending struct {
	// Outcome is what the run produced, as the server reported it. serve.ErrorCode
	// places the failures a page can act on.
	Outcome serve.Outcome

	// PromptNotTaken reports that the request carried a prompt and the conversation
	// did not take it: it was waiting on an unanswered question, the resume put the
	// question again, and the run suspended without reaching a boundary that takes a
	// user message. The prompt was neither journaled nor answered, and the page has to
	// send it again once the question is answered.
	PromptNotTaken bool
}

// TurnWriter writes one turn's response in a Format's own shape.
//
// It is agent.Events, so the server's own sink drives it and a Format that streams
// text as the model writes it also implements agent.MessageStreamer. The channel calls
// Open before the first event it forwards, Ask when the turn ended on a question, and
// Close once with the outcome. Every method runs on the run's goroutine in order,
// so an implementation needs no locking of its own.
//
// A run that fails before its journal is claimed reaches no event and no Open: the
// channel answers with a status code and the writer is discarded unused. That is what
// lets a second turn on a thread already running be refused as a 409 rather than a 200
// with an empty body.
type TurnWriter interface {
	agent.Events

	// Open writes the response head. It is idempotent: the channel calls it before the
	// first event, and Close calls it when the outcome arrived before any event did.
	Open()

	// Ask puts a question to the page as the last thing before Close. The page answers
	// it on its next request, since it cannot answer while it is still reading this
	// one.
	Ask(Question)

	// Replay writes a stored conversation as the parts the page would have seen had it
	// been reading while the conversation ran. It is here because it is on the interface
	// a Format implements; the channel calls it when a page opens a stored session,
	// which nothing does yet.
	Replay(*runstate.RunState)

	// Close ends the body with what the turn ended on. It calls Open when nothing has,
	// so a turn that produced no event still ends as a complete response.
	Close(Ending)
}
