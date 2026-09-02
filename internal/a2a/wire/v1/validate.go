//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package wire

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ErrProtocolMismatch indicates a decoded message did not carry the protocol id
// the receiving path is contracted for, or a reply was not the expected type.
var ErrProtocolMismatch = errors.New("unexpected message protocol")

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
// receiving path is contracted to carry, and returns the decoded message as T. A
// mismatch (a tool request arriving where a discovery request is expected, a
// malformed body) is reported as an error so the caller can fail closed rather
// than guess; this is the per-path type contract, not an inference of meaning from
// the transport.
//
// T is the type the wanted id decodes into, e.g.
// ExpectProtocol[*Cancel](body, CancelProtocol). An id names one shape, so the pairing
// is fixed, and naming T here is what saves every caller a type assertion whose failure
// branch cannot run. A T that is not the wanted id's type is reported as a mismatch, on
// the same terms as a body carrying the wrong id.
func ExpectProtocol[T Message](data []byte, want string) (T, error) {
	var zero T

	msg, err := Decode(data)
	if err != nil {
		return zero, fmt.Errorf("%w: %w", ErrProtocolMismatch, err)
	}

	got := msg.MessageHeader().Protocol
	if got != want {
		return zero, fmt.Errorf("%w: got %q, want %q", ErrProtocolMismatch, got, want)
	}

	return narrow[T](msg, got)
}

// ExpectOneProtocol is ExpectProtocol for a path contracted to carry any of several
// message ids, which is what a family split across ids leaves: an answer to a question
// arrives under one of six, chosen by which question it answers.
//
// A path takes the ids it can act on rather than a prefix of the family they sit in. The
// two halves of the elicit family share a prefix and travel in opposite directions, so a
// prefix would admit a question on the subject a worker reads answers from.
//
// Every id in want decodes into T, which is what makes a family askable as a set: the
// six answers are one ElicitReply and the four requests are one Request.
func ExpectOneProtocol[T Message](data []byte, want []string) (T, error) {
	var zero T

	msg, err := Decode(data)
	if err != nil {
		return zero, fmt.Errorf("%w: %w", ErrProtocolMismatch, err)
	}

	got := msg.MessageHeader().Protocol
	if !slices.Contains(want, got) {
		return zero, fmt.Errorf("%w: got %q, want one of %s", ErrProtocolMismatch, got, strings.Join(want, ", "))
	}

	return narrow[T](msg, got)
}

// narrow is the assertion the two Expect functions share. It fails where the caller
// named a T the wanted id does not decode into, which is a call written wrong rather
// than a message that arrived wrong; the error says both so whoever reads it can tell
// which.
func narrow[T Message](msg Message, protocol string) (T, error) {
	out, ok := msg.(T)
	if !ok {
		var zero T
		return zero, fmt.Errorf("%w: %q decodes into %T, not %T", ErrProtocolMismatch, protocol, msg, zero)
	}

	return out, nil
}
