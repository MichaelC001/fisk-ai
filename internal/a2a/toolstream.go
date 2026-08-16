//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// DefaultIdleTimeout bounds how long a caller waits for the next message of a tool
// call's reply set when none is configured. It bounds the gap between messages rather
// than the call: a peer that keeps saying it is working is waited for, and one that
// stops is given up on.
const DefaultIdleTimeout = 2 * time.Minute

// minIdleTimeout keeps a configured wait above the interval a peer keepalives at, with
// room for one to be lost. A shorter wait would fail every call that is merely slow,
// which is the failure this whole path exists to remove.
const minIdleTimeout = 3 * KeepaliveInterval

// toolSet reads one tool call's reply set: an ack, the keepalives that follow while
// the tool runs, and the terminal tool reply.
//
// It is internal to the client because a tool call is one answer to whoever asked for
// it, unlike a task, whose caller renders every message. The keepalives are read and
// discarded, their arrival being the whole of what they say.
type toolSet struct {
	reader    Reader
	validator *Validator
	idle      time.Duration
	acked     bool
}

// reply reads the set to its end and returns the tool's answer.
//
// Each read is bounded by the idle timeout rather than the whole call being bounded
// here, so a caller gives up on a peer that went quiet and waits on one that is
// working. An expired wait is reported as ErrAgentUnavailable: to this caller a peer
// that stopped speaking is indistinguishable from one that was never there, and
// reporting the context error instead would file it as this run timing out.
func (t *toolSet) reply(ctx context.Context) (*ToolReply, error) {
	defer t.reader.Close()

	for {
		msg, err := t.next(ctx)
		if err != nil {
			return nil, err
		}

		switch m := msg.(type) {
		case *Ack:
			if t.acked {
				return nil, fmt.Errorf("%w: a reply set carries one ack", ErrProtocolMismatch)
			}
			t.acked = true
			// A refusal still sends its terminal reply, which carries the code and the
			// text, so nothing is decided here.

		case *Event:
			// A keepalive. Nothing reads its content: that the peer sent one is the
			// message.

		case *ToolReply:
			return m, nil

		default:
			return nil, fmt.Errorf("%w: %q does not belong in a tool reply set", ErrProtocolMismatch, protocolOf(msg))
		}
	}
}

// next reads and decodes one message under the idle bound.
func (t *toolSet) next(ctx context.Context) (any, error) {
	readCtx, cancel := context.WithTimeout(ctx, t.idle)
	defer cancel()

	body, err := t.reader.Next(readCtx)
	if err != nil {
		// The caller's own deadline and the idle bound both surface as the same error
		// here, so they are told apart by whose context ended: only the read context
		// expiring means the peer went quiet.
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return nil, fmt.Errorf("%w: no message for %s", ErrAgentUnavailable, t.idle)
		}

		return nil, err
	}

	if len(body) > MaxMessageSize {
		return nil, fmt.Errorf("%w: message exceeds %d bytes", ErrMessageTooLarge, MaxMessageSize)
	}

	err = t.validator.Validate(body)
	if err != nil {
		return nil, fmt.Errorf("invalid message in the tool reply set: %w", err)
	}

	return Decode(body)
}

// protocolOf names the protocol id of a decoded message, for an error that has to say
// what arrived. It answers empty for a message carrying no header.
func protocolOf(msg any) string {
	hdr := headerOf(msg)
	if hdr == nil {
		return ""
	}

	return hdr.Protocol
}
