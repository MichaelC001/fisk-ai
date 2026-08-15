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
// "io.choria.fisk-ai.v1.request". The matching JSON schemas live under
// internal/a2a/schemas/io.choria.fisk-ai.v1.
//
// A receiver ignores properties it does not recognize rather than rejecting the
// message carrying them, so a peer built against an older copy of the schemas
// interoperates with one that has gained a field. There is no way for a sender to
// demand the opposite, so an addition must be safe to skip.
//
// An event block whose type this build does not name is carried opaquely for the same
// reason, as an UnknownBlock holding the peer's own bytes. A block is advisory and the
// run journal is the authoritative transcript, so a receiver that keeps the framing and
// hands the block on loses less than one that keeps nothing. A block whose type the
// schema does name is validated as strictly as ever.
//
// An unrecognized protocol id or stop reason is still rejected. Neither is content a
// receiver can hold and pass along: a protocol id decides what the message means, and a
// stop reason is why a task ended.
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
	RequestProtocol = ProtocolNamespace + ".request"
	EventProtocol   = ProtocolNamespace + ".event"
	ResultProtocol  = ProtocolNamespace + ".result"
	ErrorProtocol   = ProtocolNamespace + ".error"
	CancelProtocol  = ProtocolNamespace + ".cancel"
	AckProtocol     = ProtocolNamespace + ".ack"

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
)

// NewID returns a new KSUID string, used to mint message, request and
// conversation ids. KSUIDs are k-sorted and unique; ordering within a reply set
// still relies on Header.Sequence, not on the id.
func NewID() string {
	return ksuid.New().String()
}

// Decode parses a raw message body and returns the concrete message type as a
// pointer (*Request, *Event, *Result, *ErrorMessage, *Cancel, *Ack, *ToolRequest,
// *ToolReply, *DiscoveryRequest or *DiscoveryReply), chosen by its protocol id.
// It returns ErrUnknownProtocol for an unrecognized id.
func Decode(data []byte) (any, error) {
	var probe struct {
		Protocol string `json:"protocol"`
	}

	err := json.Unmarshal(data, &probe)
	if err != nil {
		return nil, err
	}

	switch probe.Protocol {
	case RequestProtocol:
		return decodeInto(data, &Request{})
	case EventProtocol:
		return decodeInto(data, &Event{})
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
		return nil, fmt.Errorf("%w: %q is not a terminal message", ErrProtocolMismatch, headerProtocol(msg))
	}
}

// headerProtocol reports the protocol id of a decoded message, for naming the one that
// arrived where a terminal message was expected.
func headerProtocol(msg any) string {
	hdr := headerOf(msg)
	if hdr == nil {
		return ""
	}

	return hdr.Protocol
}

func decodeInto[T any](data []byte, msg *T) (*T, error) {
	err := json.Unmarshal(data, msg)
	if err != nil {
		return nil, err
	}

	return msg, nil
}
