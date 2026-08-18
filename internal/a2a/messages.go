//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// Request starts a task. It is sent by a caller to an agent.
type Request struct {
	Header

	Prompt    string   `json:"prompt"`
	Context   string   `json:"context,omitempty"`
	ToolHints []string `json:"tool_hints,omitempty"`
	Budget    *Budget  `json:"budget,omitempty"`
	// Stream, when false, asks for only a terminal result with no event stream.
	// A nil value means the default, which is to stream.
	Stream *bool `json:"stream,omitempty"`
	// ConversationToken runs Prompt as the next turn of the conversation the token
	// names, which is the one an earlier Ack handed back. Empty starts a conversation
	// of its own, which is what every first request carries.
	//
	// A caller decides per request and declares nothing in advance: a client that
	// answers once and stops ignores the token it was given, and one that wants another
	// turn sends the token it already holds.
	ConversationToken string `json:"conversation_token,omitempty"`
	// Answer answers a question the conversation is still waiting on, for a caller
	// that was asked something and could not answer before the run gave up. It
	// requires ConversationToken and replaces Prompt: a request carries one or the
	// other and is refused for carrying both.
	//
	// The run is resumed either way. An answer to a question a tool asked becomes
	// that call's result, since a deferred call is never dispatched again. An answer
	// to an approval is held for the question the resumed run asks about the same
	// call, so nobody is asked twice for something they already answered.
	Answer *Answer `json:"answer,omitempty"`
}

// Answer carries what an operator said, for a question whose run has ended. It is an
// ElicitReply with the call it belongs to and the question it was, so a receiver knows
// what it is answering without consulting a journal.
type Answer struct {
	// ToolUseID is the call the question was about, taken from the question.
	ToolUseID string `json:"tool_use_id"`
	// Kind is the question that was asked, taken from the question. It says what the
	// answer means where the answer value alone cannot: AnswerNoOperator is the same
	// value for all four.
	Kind ElicitKind `json:"kind"`
	// Answer names which field below carries the answer, as it does on an ElicitReply.
	// AnswerIndex and AnswerWaiting have no meaning here and are refused.
	Answer ElicitAnswer `json:"answer"`
	// Choice answers an approve question.
	Choice ElicitChoice `json:"choice,omitempty"`
	// Confirmed answers a confirm question.
	Confirmed bool `json:"confirmed,omitempty"`
	// Value answers an input question, where an empty string is a valid answer, and a
	// select question, where it is the option that was chosen.
	//
	// A live reply answers a selection with a position in the options it was sent.
	// This one names the option itself, because the run that offered the list has
	// ended and a position into a list the receiver no longer holds says nothing.
	Value string `json:"value,omitempty"`
}

// NewAnswer builds the answer to ask out of the reply that would have answered it
// live, so a caller that already built one to answer in the moment sends the same
// thing when the moment has passed. A selection names the option it chose, taken from
// the options the question carried.
func NewAnswer(ask *ElicitRequest, reply *ElicitReply) (*Answer, error) {
	a := &Answer{
		ToolUseID: ask.ToolUseID,
		Kind:      ask.Kind,
		Answer:    reply.Answer,
		Choice:    reply.Choice,
		Confirmed: reply.Confirmed,
		Value:     reply.Value,
	}

	if reply.Answer == AnswerIndex {
		if reply.Index < 0 || reply.Index >= len(ask.Options) {
			return nil, fmt.Errorf("option %d is not one of the %d the question offered", reply.Index, len(ask.Options))
		}

		a.Answer = AnswerValue
		a.Value = ask.Options[reply.Index]
	}

	return a, nil
}

// NewAnsweringRequest builds the request that answers ask on the conversation token
// carries, for a caller whose question outlived its run. It sends no prompt, so the
// run is resumed rather than given a new turn.
func NewAnsweringRequest(token string, ask *ElicitRequest, reply *ElicitReply) (*Request, error) {
	answer, err := NewAnswer(ask, reply)
	if err != nil {
		return nil, err
	}

	r := &Request{ConversationToken: token, Answer: answer}
	r.Protocol = RequestProtocol

	return r, nil
}

// NewRequest builds a Request with the protocol id set.
func NewRequest(prompt string) *Request {
	r := &Request{Prompt: prompt}
	r.Protocol = RequestProtocol

	return r
}

// NewFollowUp builds a request that continues the conversation ack accepted, running
// prompt as its next turn. It correlates from the ack rather than leaving a caller to
// copy the token across, for the reason NewElicitReplyFromRequest states.
//
// The conversation tag is carried over as well, so a caller's own correlation across
// the turns of one conversation survives without being set again. Header.Request is
// left for the send to stamp, since a follow-up opens a reply set of its own.
func NewFollowUp(ack *Ack, prompt string) *Request {
	r := NewRequest(prompt)
	r.ConversationToken = ack.ConversationToken
	r.Conversation = ack.Conversation

	return r
}

// WantsStream reports whether the caller wants the event stream. It defaults to
// true when Stream is unset.
func (r *Request) WantsStream() bool {
	return r.Stream == nil || *r.Stream
}

// Event carries one streamed content block from an agent to a caller.
type Event struct {
	Header

	Block Block `json:"block"`
}

// NewEvent builds an Event wrapping the block, with the protocol id set.
func NewEvent(block Block) *Event {
	e := &Event{Block: block}
	e.Protocol = EventProtocol

	return e
}

// Result is the terminal success message of a task.
type Result struct {
	Header

	StopReason StopReason `json:"stop_reason"`
	Text       string     `json:"text,omitempty"`
	Usage      *Usage     `json:"usage,omitempty"`
}

// NewResult builds a Result with the protocol id set.
func NewResult(reason StopReason) *Result {
	r := &Result{StopReason: reason}
	r.Protocol = ResultProtocol

	return r
}

// ErrorMessage is the terminal failure message of a task. It implements the
// error interface.
type ErrorMessage struct {
	Header

	StopReason StopReason `json:"stop_reason,omitempty"`
	Err        string     `json:"error"`
	Code       string     `json:"code,omitempty"`
}

// NewError builds an ErrorMessage with the protocol id set.
func NewError(message string) *ErrorMessage {
	e := &ErrorMessage{Err: message}
	e.Protocol = ErrorProtocol

	return e
}

// Error implements the error interface.
func (e *ErrorMessage) Error() string { return e.Err }

// Cancel asks an agent to cancel an in-flight task, identified by Header.Request.
type Cancel struct {
	Header

	Reason string `json:"reason,omitempty"`
}

// NewCancel builds a Cancel with the protocol id set.
func NewCancel() *Cancel {
	c := &Cancel{}
	c.Protocol = CancelProtocol

	return c
}

// Ack reports whether an agent accepted a request.
type Ack struct {
	Header

	Accepted bool   `json:"accepted"`
	Reason   string `json:"reason,omitempty"`
	// ConversationToken is the handle a later request carries to run its prompt as the
	// next turn of the conversation this ack accepted. An agent that serves follow-up
	// turns carries it on every ack it accepts with, the minted one on a first turn and
	// the accepted one on a follow-up, so a caller reads back which conversation it is
	// on. Empty from an agent that does not serve them.
	//
	// Holding it is the authorization to add a turn to that conversation, so it is
	// neither logged nor displayed.
	ConversationToken string `json:"conversation_token,omitempty"`
}

// NewAck builds an Ack with the protocol id set.
func NewAck(accepted bool) *Ack {
	a := &Ack{Accepted: accepted}
	a.Protocol = AckProtocol

	return a
}

// ToolRequest invokes a single tool on a remote agent directly, without engaging
// the remote agentic loop. It is a request-reply interaction used when one agent
// imports or exports tools to another; the reply is a ToolReply correlated by
// Header.Request.
type ToolRequest struct {
	Header

	Name  string          `json:"name"`
	Input json.RawMessage `json:"input,omitempty"`
}

// NewToolRequest builds a ToolRequest with the protocol id set.
func NewToolRequest(name string, input json.RawMessage) *ToolRequest {
	r := &ToolRequest{Name: name, Input: input}
	r.Protocol = ToolRequestProtocol

	return r
}

// ToolReply is the result of a ToolRequest. It carries the shared ToolResult
// outcome, the same shape as a streamed ToolResultBlock. A failed, denied or refused
// call is reported in-band with IsError true and an explanatory Output.
type ToolReply struct {
	Header
	ToolResult

	// Code says why a call did not run, and is empty for one that did. A caller
	// switches on it where Output is prose for a model, so a refusal it can act on
	// does not depend on matching text.
	//
	// It is here rather than on the embedded ToolResult, which a streamed
	// ToolResultBlock also carries: the conditions it names belong to a directly
	// answered call, and a block in a task stream has no way to produce one.
	Code string `json:"code,omitempty"`
}

// NewToolReply builds a ToolReply with the protocol id set.
func NewToolReply(output string, isError bool) *ToolReply {
	r := &ToolReply{ToolResult: ToolResult{Output: output, IsError: isError}}
	r.Protocol = ToolReplyProtocol

	return r
}

// ToolDescriptor describes a tool an agent exposes, as reported in a discovery
// reply. It carries enough to import the tool and later invoke it with a
// ToolRequest.
type ToolDescriptor struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	// Behavior is what the serving agent says calling the tool does to the world. It
	// carries the neutral declaration rather than the serving agent's own tags, so a
	// tool that declares its behavior some other way than a command tag is described
	// the same way. It is the serving agent's unverified claim about its own tools, so
	// an importer sanitizes it and never republishes it as its own; the toolkit type is
	// used directly because this wire format has no need to differ from it.
	Behavior toolkit.Behavior `json:"behavior,omitzero"`
}

// AgentCard is an agent's self description: who it is, its version, and the
// tools it exposes.
type AgentCard struct {
	Name        string           `json:"name"`
	Version     string           `json:"version"`
	Description string           `json:"description,omitempty"`
	Protocols   []string         `json:"protocols,omitempty"`
	Tools       []ToolDescriptor `json:"tools,omitempty"`
}

// ElicitKind names what a run is asking the caller for. The four are the four
// methods of toolkit.Prompter, so a caller that answers all of them can drive every
// question the harness knows how to put to a person.
type ElicitKind string

const (
	// ElicitApprove asks whether a confirmation-gated command may run. The answer is
	// an ElicitChoice, since approving for the rest of the conversation is a wider
	// claim than approving one call.
	ElicitApprove ElicitKind = "approve"
	// ElicitConfirm asks a yes/no question.
	ElicitConfirm ElicitKind = "confirm"
	// ElicitSelect asks the caller to choose one of Options, answered by index.
	ElicitSelect ElicitKind = "select"
	// ElicitInput asks for a free text value.
	ElicitInput ElicitKind = "input"
)

// ElicitChoice is the answer to an ElicitApprove question, mirroring
// toolkit.ConfirmChoice.
type ElicitChoice string

const (
	// ChoiceNo declines the command.
	ChoiceNo ElicitChoice = "no"
	// ChoiceOnce runs the command this time. It authorizes the one call that asked
	// and nothing else.
	ChoiceOnce ElicitChoice = "once"
	// ChoiceAlways runs the command and stops asking for that tool for the rest of
	// the conversation, which the answering party is claiming on behalf of an
	// operator it can reach.
	ChoiceAlways ElicitChoice = "always"
)

// ElicitAnswer names which field of an ElicitReply carries the answer, so a zero
// index and an absent one are never confused. Two values name no field: AnswerNoOperator,
// which reports that nobody is there to answer, and AnswerWaiting, which is not an answer
// at all.
type ElicitAnswer string

const (
	// AnswerChoice reads Choice, for an approve question.
	AnswerChoice ElicitAnswer = "choice"
	// AnswerConfirmed reads Confirmed, for a confirm question.
	AnswerConfirmed ElicitAnswer = "confirmed"
	// AnswerIndex reads Index, for a select question.
	AnswerIndex ElicitAnswer = "index"
	// AnswerValue reads Value, for an input question.
	AnswerValue ElicitAnswer = "value"
	// AnswerNoOperator says the caller has nobody to ask. It is a legitimate answer
	// rather than a failure, and it fails closed: a gated command does not run.
	AnswerNoOperator ElicitAnswer = "no_operator"
	// AnswerWaiting says the question is in front of a person and nobody has answered
	// yet. It restarts the window the agent holds the question open for, so a person
	// takes as long as they take. It answers nothing, and a caller whose person has
	// gone sends AnswerNoOperator rather than falling silent.
	AnswerWaiting ElicitAnswer = "waiting"
)

// ElicitRequest is a question a running task puts to the caller that submitted it,
// sent on the task's reply set. Header.Request names the task and QuestionID names
// the question within it, since one task may ask several.
//
// The text fields are model-supplied and are sanitized before they are sent, as they
// are before a terminal prompter renders them. A caller displaying one sanitizes
// again for its own display.
type ElicitRequest struct {
	Header

	// QuestionID correlates the reply. It is unique within the task.
	QuestionID string `json:"question_id"`
	// ToolUseID is the tool call this question is about. It is what an answer given
	// after the run has ended names, since a resume asks the question again under a
	// new QuestionID and the call is what both ends can agree on.
	//
	// Empty from an agent that predates it, which is an agent that takes no answer
	// once its run has ended.
	ToolUseID string `json:"tool_use_id,omitempty"`
	// Kind selects which of the four questions this is and which fields below carry
	// its detail.
	Kind ElicitKind `json:"kind"`
	// Question is the text to put to the operator, for confirm, select and input.
	Question string `json:"question,omitempty"`
	// Command is the command path an approve question is about, e.g. "stream rm".
	Command string `json:"command,omitempty"`
	// Display is the full command line an approve question shows, already sanitized.
	Display string `json:"display,omitempty"`
	// Tag is the tag that gated the command, e.g. ai:confirm, named so the operator
	// sees why they are being asked.
	Tag string `json:"tag,omitempty"`
	// Options are the choices a select question offers, in the order to show them.
	Options []string `json:"options,omitempty"`
	// Default is the value an input question pre-fills for the operator to accept or
	// edit.
	Default string `json:"default,omitempty"`
	// WaitMS is how long this question is held open before the agent gives up on it,
	// in milliseconds. A caller that keeps a person in front of the question sends
	// AnswerWaiting inside that window to restart it, at AckInterval.
	//
	// Zero is an agent that predates this and takes no such replies, which is also
	// what a 400 on one means. Answer inside the window instead.
	WaitMS int64 `json:"wait_ms,omitempty"`
}

// AckInterval is how often to say the question is still in front of a person, which is
// a third of the window so two replies may be lost or late before it closes. It is zero
// for a question whose agent takes no such replies.
//
// The window restarts when the agent receives the reply rather than when it is sent, so
// the third that is left over is also where a caller's own round trip is paid for.
func (r *ElicitRequest) AckInterval() time.Duration {
	if r.WaitMS <= 0 {
		return 0
	}

	return time.Duration(r.WaitMS) * time.Millisecond / 3
}

// NewElicitRequest builds an ElicitRequest with the protocol id and kind set.
func NewElicitRequest(kind ElicitKind, questionID string) *ElicitRequest {
	r := &ElicitRequest{QuestionID: questionID, Kind: kind}
	r.Protocol = ElicitRequestProtocol

	return r
}

// ElicitReply answers one ElicitRequest, addressed to the task that asked and
// correlated by QuestionID. Answer says which field to read.
//
// Nothing authenticates it beyond the transport's own permissions, exactly as with a
// cancel: whoever may address the running task may answer its questions, and one of
// those answers approves a confirmation-gated command.
type ElicitReply struct {
	Header

	// QuestionID is the question this answers.
	QuestionID string `json:"question_id"`
	// Answer names the field carrying the answer, or reports that the caller has
	// nobody to ask.
	Answer ElicitAnswer `json:"answer"`
	// Choice answers an approve question.
	Choice ElicitChoice `json:"choice,omitempty"`
	// Confirmed answers a confirm question.
	Confirmed bool `json:"confirmed,omitempty"`
	// Index answers a select question, as a position in the Options that were sent.
	Index int `json:"index,omitempty"`
	// Value answers an input question. An empty string is a valid answer, which is
	// why Answer rather than emptiness says what was given.
	Value string `json:"value,omitempty"`
}

// NewElicitReplyFromRequest builds the reply to ask, for a caller that then fills in the
// value for answer. It correlates the reply to the task and to the question, and stamps
// it as coming from sender, which addresses it back to the agent that asked.
//
// Answering a question means filling five header fields correctly, and a reply that gets
// any of them wrong reaches no question and is refused. Deriving them from the request
// is what stops each caller reimplementing that.
func NewElicitReplyFromRequest(ask *ElicitRequest, sender string, answer ElicitAnswer) *ElicitReply {
	r := NewElicitReply(ask.QuestionID, answer)
	StampReply(&r.Header, &ask.Header, sender)

	return r
}

// NewApproveReply answers an ElicitApprove question with the operator's three-way choice.
func NewApproveReply(ask *ElicitRequest, sender string, choice ElicitChoice) *ElicitReply {
	r := NewElicitReplyFromRequest(ask, sender, AnswerChoice)
	r.Choice = choice

	return r
}

// NewConfirmReply answers an ElicitConfirm question yes or no.
func NewConfirmReply(ask *ElicitRequest, sender string, confirmed bool) *ElicitReply {
	r := NewElicitReplyFromRequest(ask, sender, AnswerConfirmed)
	r.Confirmed = confirmed

	return r
}

// NewSelectReply answers an ElicitSelect question with a position in the Options that
// were sent. An index outside them is refused by the agent that asked, since it is a
// choice nobody offered.
func NewSelectReply(ask *ElicitRequest, sender string, index int) *ElicitReply {
	r := NewElicitReplyFromRequest(ask, sender, AnswerIndex)
	r.Index = index

	return r
}

// NewInputReply answers an ElicitInput question with a value, which may be empty.
func NewInputReply(ask *ElicitRequest, sender string, value string) *ElicitReply {
	r := NewElicitReplyFromRequest(ask, sender, AnswerValue)
	r.Value = value

	return r
}

// NewNoOperatorReply answers any question with the fact that nobody is there to answer
// it. It is an answer rather than a failure, and it fails closed: the agent that asked
// treats it as a refusal, so a gated command does not run.
func NewNoOperatorReply(ask *ElicitRequest, sender string) *ElicitReply {
	return NewElicitReplyFromRequest(ask, sender, AnswerNoOperator)
}

// NewWaitingAck says the question is still in front of a person and nobody has answered
// yet, which restarts the window the agent holds it open for. It is sent every
// ElicitRequest.AckInterval while the question is displayed.
//
// Stop sending them before the answer goes out. One arriving after it reaches a question
// the agent has finished with and is refused.
func NewWaitingAck(ask *ElicitRequest, sender string) *ElicitReply {
	return NewElicitReplyFromRequest(ask, sender, AnswerWaiting)
}

// NewElicitReply builds an ElicitReply with the protocol id set. The caller fills the
// header itself, where NewElicitReplyFromRequest derives it from the question and the
// five constructors above set an answer and its value together.
func NewElicitReply(questionID string, answer ElicitAnswer) *ElicitReply {
	r := &ElicitReply{QuestionID: questionID, Answer: answer}
	r.Protocol = ElicitReplyProtocol

	return r
}

// DiscoveryRequest asks an agent to describe itself. The reply is a
// DiscoveryReply.
type DiscoveryRequest struct {
	Header
}

// NewDiscoveryRequest builds a DiscoveryRequest with the protocol id set.
func NewDiscoveryRequest() *DiscoveryRequest {
	r := &DiscoveryRequest{}
	r.Protocol = DiscoveryRequestProtocol

	return r
}

// DiscoveryReply describes the replying agent, correlated to the request by
// Header.Request.
type DiscoveryReply struct {
	Header
	AgentCard
}

// NewDiscoveryReply builds a DiscoveryReply with the protocol id set.
func NewDiscoveryReply(name, version string) *DiscoveryReply {
	r := &DiscoveryReply{AgentCard: AgentCard{Name: name, Version: version}}
	r.Protocol = DiscoveryReplyProtocol

	return r
}
