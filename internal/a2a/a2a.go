//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// Package a2a defines the fisk-ai agent-to-agent (A2A) protocol message types.
//
// The protocol is transport agnostic: every message is a single, flat JSON
// object that carries all of its own framing in the body (see Header), so a
// captured message is fully self describing outside of any transport. The first
// transport binding is core NATS over a micro service (see the nats subpackage),
// which carries discovery, direct tool calls, and the task flow, where a reply set
// is correlated by the Header.Request id and ordered by Header.Sequence. A binding
// carries a reply set when it implements StreamingTransport; a binding that does not
// answers a task with a single terminal message. Later bindings wrap the same body
// inside the Choria Protocol.
//
// Message bodies are versioned by their protocol id, e.g.
// "io.choria.fisk-ai.v1.request.prompt". One id names one shape, so nothing in a body says
// what kind of message it is: a prompt, an answer, a resume and a read are four ids, and so
// are the ten kinds of streamed block and the eleven questions and answers of the elicit
// family. The matching JSON schemas are embedded from internal/a2a/schemas/v1 and
// published under https://choria.io/schemas/io.choria.fisk-ai.v1, one per id, each
// stating what its own shape requires and refusing by name the fields belonging to its
// siblings.
//
// A receiver ignores properties it does not recognize rather than rejecting the
// message carrying them, so a peer built against an older copy of the schemas
// interoperates with one that has gained a field. There is no way for a sender to
// demand the opposite, so an addition must be safe to skip.
//
// A stop reason outside the set this build names is carried too, for the same reason
// stated the other way round: refusing one costs the terminal message that held it,
// and with it the answer text and the token counts. StopReason.Valid is how a receiver
// asks whether it recognizes the value.
//
// An unrecognized protocol id is rejected, being the one thing a receiver cannot hold
// and pass along: it decides what the message means, so a receiver that ignored it would
// not know what it was ignoring.
//
// A later namespace is rejected the same way, id by id: a v1 receiver answers no
// io.choria.fisk-ai.v2 message, so a sender holding both sends v1 to reach it.
// AgentCard.Protocols is what a caller picks from, listing the namespaces the serving
// agent speaks, and a caller holding more than one sends in the newest both ends list. A
// card listing none is an older peer, and the namespace its card arrived under is the one
// it speaks.
//
// Streamed blocks are the exception, and they are why every kind of block has an id of
// its own under io.choria.fisk-ai.v1.event. An id in that family names a block whatever
// else this build knows about it, so one added since is carried opaquely as an
// UnknownBlock holding the peer's own bytes. A block is advisory and the run journal is
// the authoritative transcript, so a receiver that keeps the framing and hands the block
// on loses less than one that keeps nothing. A kind the schemas do name is validated as
// strictly as ever, against a schema of its own.
//
// Property names are reserved to this project. A peer carrying its own data should
// nest it under a property assigned to it rather than add a top-level key, since two
// peers adding the same name would both validate and mean different things.
package a2a

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/segmentio/ksuid"
)

// ProtocolNamespace is the namespace and version shared by every v1 message id.
const ProtocolNamespace = "io.choria.fisk-ai.v1"

// Protocol ids, used as the value of the Header.Protocol field and as the
// discriminator consumed by Decode.
const (
	// RequestProtocol is the family the four things a caller can ask of a worker belong
	// to rather than an id itself: a request travels as this, a dot, and what it asks
	// for, so a prompt is io.choria.fisk-ai.v1.request.prompt. Nothing in the body says
	// which of the four it is.
	RequestProtocol = ProtocolNamespace + ".request"

	// The id of each. The suffix is the RequestKind it carries, so the two are the same
	// string and there is no second spelling to keep in step.
	RequestPromptProtocol = RequestProtocol + "." + string(RequestPrompt)
	RequestAnswerProtocol = RequestProtocol + "." + string(RequestAnswer)
	RequestResumeProtocol = RequestProtocol + "." + string(RequestResume)
	RequestReadProtocol   = RequestProtocol + "." + string(RequestRead)

	ResultProtocol = ProtocolNamespace + ".result"
	ErrorProtocol  = ProtocolNamespace + ".error"
	CancelProtocol = ProtocolNamespace + ".cancel"
	AckProtocol    = ProtocolNamespace + ".ack"

	// EventProtocol is the family a streamed block's id belongs to rather than an id
	// itself: a block travels as this, a dot, and its type, so a text block is
	// io.choria.fisk-ai.v1.event.text. Nothing in the body says which kind it is.
	EventProtocol = ProtocolNamespace + ".event"

	// The id of each kind of block. The suffix is the BlockType it carries, so the two
	// are the same string and there is no second spelling to keep in step.
	EventThinkingProtocol   = EventProtocol + "." + string(BlockThinking)
	EventTextProtocol       = EventProtocol + "." + string(BlockText)
	EventToolCallProtocol   = EventProtocol + "." + string(BlockToolCall)
	EventToolResultProtocol = EventProtocol + "." + string(BlockToolResult)
	EventAgentCallProtocol  = EventProtocol + "." + string(BlockAgentCall)
	EventStatusProtocol     = EventProtocol + "." + string(BlockStatus)
	EventWarningProtocol    = EventProtocol + "." + string(BlockWarning)
	EventPromptProtocol     = EventProtocol + "." + string(BlockPrompt)

	// The id of each kind of fragment. A receiver that wants text and not reasoning
	// drops one id without reading the body, and a build naming neither reads both as
	// UnknownBlock against the framing schema, which is why the two are separate ids
	// rather than one carrying a kind.
	EventTextDeltaProtocol     = EventProtocol + "." + string(BlockTextDelta)
	EventThinkingDeltaProtocol = EventProtocol + "." + string(BlockThinkingDelta)

	// ElicitProtocol is the family a question a running task puts to its caller, and
	// the caller's answer, both belong to rather than an id itself. The question
	// travels on the task's reply set and the answer on the task's own inbound path.
	ElicitProtocol = ProtocolNamespace + ".elicit"

	// ElicitRequestProtocol is the family a question's id belongs to rather than an id
	// itself: a question travels as this, a dot, and its kind, so an approve question
	// is io.choria.fisk-ai.v1.elicit.request.approve. Nothing in the body says which
	// kind it is.
	ElicitRequestProtocol = ElicitProtocol + ".request"

	// The id of each kind of question. The suffix is the ElicitKind it carries, so the
	// two are the same string and there is no second spelling to keep in step.
	ElicitRequestApproveProtocol = ElicitRequestProtocol + "." + string(ElicitApprove)
	ElicitRequestConfirmProtocol = ElicitRequestProtocol + "." + string(ElicitConfirm)
	ElicitRequestSelectProtocol  = ElicitRequestProtocol + "." + string(ElicitSelect)
	ElicitRequestInputProtocol   = ElicitRequestProtocol + "." + string(ElicitInput)

	// ElicitReplyProtocol is the family an answer's id belongs to rather than an id
	// itself. Every id under it settles the question, no_operator included, so a caller
	// routing on this prefix reaches every answer there is.
	ElicitReplyProtocol = ElicitProtocol + ".reply"

	// The id of each answer, named for the question it answers rather than the field it
	// carries, so a capture pairs elicit.request.select with elicit.reply.select
	// without the reader knowing what an index is.
	ElicitReplyApproveProtocol    = ElicitReplyProtocol + "." + string(ElicitApprove)
	ElicitReplyConfirmProtocol    = ElicitReplyProtocol + "." + string(ElicitConfirm)
	ElicitReplySelectProtocol     = ElicitReplyProtocol + "." + string(ElicitSelect)
	ElicitReplyInputProtocol      = ElicitReplyProtocol + "." + string(ElicitInput)
	ElicitReplyNoOperatorProtocol = ElicitReplyProtocol + "." + string(AnswerNoOperator)

	// ElicitWaitingProtocol says the question is still in front of a person and nobody
	// has answered yet, which restarts the window the agent holds it open for. It
	// answers nothing, so it sits beside the replies rather than under them.
	ElicitWaitingProtocol = ElicitProtocol + "." + string(AnswerWaiting)

	// ToolRequestProtocol and ToolReplyProtocol carry a direct tool invocation
	// (request-reply), used to import or export tools between agents without
	// engaging the agentic loop.
	ToolRequestProtocol = ProtocolNamespace + ".tool.request"
	ToolReplyProtocol   = ProtocolNamespace + ".tool.reply"

	// DiscoveryRequestProtocol and DiscoveryReplyProtocol let an agent describe
	// itself (name, version, exposed tools) to others.
	DiscoveryRequestProtocol = ProtocolNamespace + ".discovery.request"
	DiscoveryReplyProtocol   = ProtocolNamespace + ".discovery.reply"
)

var (
	// ErrUnknownProtocol is returned by Decode for an unrecognized protocol id.
	ErrUnknownProtocol = errors.New("unknown protocol")
	// ErrInvalidMessage is returned when a message cannot be represented on the
	// wire, e.g. an event block with no content or no type.
	ErrInvalidMessage = errors.New("invalid message")
	// ErrPromptDenied is returned by RunTask when a ClientHooks.PromptSubmit hook
	// refused the prompt. Nothing was sent, so no conversation was opened or
	// continued and the agent never saw it.
	ErrPromptDenied = errors.New("prompt denied")
	// ErrIncompleteStream reports a reply set that ended without a terminal message,
	// so how the turn ended is not known. It reaches a caller through
	// ClientTurnEndInfo.Err rather than as a return: what arrived before the set ended
	// is in the outcome and is worth having.
	ErrIncompleteStream = errors.New("the reply set ended without a terminal message")
)

// NewID returns a new KSUID string, used to mint message, request and
// conversation ids. KSUIDs are k-sorted and unique; ordering within a reply set
// still relies on Header.Sequence, not on the id.
func NewID() string {
	return ksuid.New().String()
}

// Decode parses a raw message body and returns the concrete message type as a
// pointer (*Request, *Event, *Result, *ErrorMessage, *Cancel, *Ack, *ToolRequest,
// *ToolReply, *DiscoveryRequest, *DiscoveryReply, *ElicitRequest or *ElicitReply),
// chosen by its protocol id. It returns ErrUnknownProtocol for an unrecognized id.
//
// The return is a Message, so a caller switches on the concrete type or reads the id
// off the header with MessageHeader().Protocol and switches on that. Either dispatch
// stays open, and the header a caller reaches for the id is the one carrying the
// correlation tag and the sequence number.
//
// An event id names the block it carries, so io.choria.fisk-ai.v1.event.text decodes
// into an Event holding a TextBlock. An id in that family this build does not name
// decodes into an Event holding an UnknownBlock, which keeps the peer's own bytes and
// the message's header: a block is one line of narration, and losing the message that
// held it costs more than the line is worth.
func Decode(data []byte) (Message, error) {
	var probe struct {
		Protocol string `json:"protocol"`
	}

	err := json.Unmarshal(data, &probe)
	if err != nil {
		return nil, err
	}

	if _, ok := blockTypeOf(probe.Protocol); ok {
		return decodeInto(data, &Event{})
	}

	if _, ok := elicitKindOf(probe.Protocol); ok {
		return decodeInto(data, &ElicitRequest{})
	}

	if _, ok := elicitAnswerOf(probe.Protocol); ok {
		return decodeInto(data, &ElicitReply{})
	}

	if _, ok := requestKindOf(probe.Protocol); ok {
		return decodeInto(data, &Request{})
	}

	switch probe.Protocol {
	case ResultProtocol:
		return decodeInto(data, &Result{})
	case ErrorProtocol:
		return decodeInto(data, &ErrorMessage{})
	case CancelProtocol:
		return decodeInto(data, &Cancel{})
	case AckProtocol:
		return decodeInto(data, &Ack{})
	case ToolRequestProtocol:
		return decodeInto(data, &ToolRequest{})
	case ToolReplyProtocol:
		return decodeInto(data, &ToolReply{})
	case DiscoveryRequestProtocol:
		return decodeInto(data, &DiscoveryRequest{})
	case DiscoveryReplyProtocol:
		return decodeInto(data, &DiscoveryReply{})
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownProtocol, probe.Protocol)
	}
}

// DecodeTerminal decodes the message that ends a task, which is one of two protocols: a
// Result when the task produced an answer, and an ErrorMessage when it failed.
//
// The failure is returned as the error, since ErrorMessage implements it. That makes the
// ordinary path a nil check and puts the two kinds of failure in one place, so a caller
// separates them with errors.As:
//
//	res, err := a2a.DecodeTerminal(data)
//	var failed *a2a.ErrorMessage
//	if errors.As(err, &failed) {
//		// the task ran and failed; failed.StopReason says how
//	}
//
// An error that is not an *ErrorMessage means the bytes could not be read as a terminal
// message at all, which is a different problem from a task that failed.
func DecodeTerminal(data []byte) (*Result, error) {
	msg, err := Decode(data)
	if err != nil {
		return nil, err
	}

	switch m := msg.(type) {
	case *Result:
		return m, nil
	case *ErrorMessage:
		return nil, m
	default:
		return nil, fmt.Errorf("%w: %q is not a terminal message", ErrProtocolMismatch, msg.MessageHeader().Protocol)
	}
}

func decodeInto[T any](data []byte, msg *T) (*T, error) {
	err := json.Unmarshal(data, msg)
	if err != nil {
		return nil, err
	}

	return msg, nil
}
