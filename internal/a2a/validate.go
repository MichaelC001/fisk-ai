//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

var (
	// ErrAgentUnavailable indicates no agent answered the request (no responder,
	// or the request deadline elapsed). A transport returns it from RoundTrip.
	ErrAgentUnavailable = errors.New("remote agent unavailable")
	// ErrNoResponders narrows ErrAgentUnavailable to the case where nothing is
	// listening on the subject at all, as against one where somebody is and did not
	// answer in time. It wraps ErrAgentUnavailable, so a caller that does not care
	// which it was keeps matching on that and needs no change.
	//
	// The two want different answers. Nothing listening is settled: the agent is not
	// there, and waiting longer or asking again will not find it. A silent responder
	// is an agent that exists and is slow, which a caller may reasonably carry on
	// without rather than treat as a failure.
	ErrNoResponders = fmt.Errorf("%w: no subscription interest", ErrAgentUnavailable)
	// ErrToolImport indicates a remote agent answered but its reply could not be
	// used (a reply over the size cap, an invalid or unexpected body).
	ErrToolImport = errors.New("remote tool import failed")
	// ErrProtocolMismatch indicates a decoded message did not carry the protocol id
	// the receiving path is contracted for, or a reply was not the expected type.
	ErrProtocolMismatch = errors.New("unexpected message protocol")
	// ErrMessageTooLarge indicates a message a sender assembled is over the size cap
	// and was refused rather than sent. It is separate from the caps applied to what
	// arrives, because a sender can act on it: an event carrying a large tool result
	// can be shortened, where a reply that arrived oversized can only be dropped.
	ErrMessageTooLarge = errors.New("message exceeds the size limit")
	// ErrStreamUnsupported indicates a request asked for an event stream on a
	// transport that carries a single reply.
	ErrStreamUnsupported = errors.New("transport cannot carry an event stream")
)

// MaxMessageSize bounds a single a2a body on the wire. It is enforced in the
// engine on both inbound handler bodies and round-trip replies, before any decode
// or allocation. It is kept under the NATS default 1 MiB max payload with room for
// transport framing, and caps both a discovery reply (a large command tree with
// per-tool schemas) and a tool reply.
//
// It is exported because a channel admitting messages over this protocol applies the
// same cap to what it takes in, and a second number written somewhere else is one
// that can drift from what this engine will send.
const MaxMessageSize = 768 * 1024

// ExpectProtocol decodes a raw body, confirms its protocol id is the one the
// receiving path is contracted to carry, and returns the decoded message. A
// mismatch (a tool request arriving where a discovery request is expected, a
// malformed body) is reported as an error so the caller can fail closed rather
// than guess; this is the per-path type contract, not an inference of meaning from
// the transport.
func ExpectProtocol(data []byte, want string) (any, error) {
	msg, err := Decode(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrProtocolMismatch, err)
	}

	hdr := headerOf(msg)
	if hdr == nil {
		return nil, fmt.Errorf("%w: message carries no header", ErrProtocolMismatch)
	}
	if hdr.Protocol != want {
		return nil, fmt.Errorf("%w: got %q, want %q", ErrProtocolMismatch, hdr.Protocol, want)
	}

	return msg, nil
}

// ExpectOneProtocol is ExpectProtocol for a path contracted to carry any of several
// message ids, which is what a family split across ids leaves: an answer to a question
// arrives under one of six, chosen by which question it answers.
//
// A path takes the ids it can act on rather than a prefix of the family they sit in. The
// two halves of the elicit family share a prefix and travel in opposite directions, so a
// prefix would admit a question on the subject a worker reads answers from.
func ExpectOneProtocol(data []byte, want []string) (any, error) {
	msg, err := Decode(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrProtocolMismatch, err)
	}

	hdr := headerOf(msg)
	if hdr == nil {
		return nil, fmt.Errorf("%w: message carries no header", ErrProtocolMismatch)
	}

	if !slices.Contains(want, hdr.Protocol) {
		return nil, fmt.Errorf("%w: got %q, want one of %s", ErrProtocolMismatch, hdr.Protocol, strings.Join(want, ", "))
	}

	return msg, nil
}

// headerOf returns the embedded Header of any decoded a2a message, or nil if the
// value is not one of the known message types.
func headerOf(msg any) *Header {
	switch m := msg.(type) {
	case *Request:
		return &m.Header
	case *Event:
		return &m.Header
	case *Result:
		return &m.Header
	case *ErrorMessage:
		return &m.Header
	case *Cancel:
		return &m.Header
	case *Ack:
		return &m.Header
	case *ToolRequest:
		return &m.Header
	case *ToolReply:
		return &m.Header
	case *DiscoveryRequest:
		return &m.Header
	case *DiscoveryReply:
		return &m.Header
	case *ElicitRequest:
		return &m.Header
	case *ElicitReply:
		return &m.Header
	default:
		return nil
	}
}
