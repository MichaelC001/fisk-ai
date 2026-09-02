//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// Package a2a carries fisk-ai agent-to-agent (A2A) protocol messages between agents:
// Client asks a peer for a discovery card, a direct tool call or a task; Server answers
// those on behalf of a local agent; Transport is what a binding implements and the
// registry is where a program names the bindings it has; TaskStream reads a task's reply
// set and ReplyStream writes one; and NewRemoteTool imports a peer's tool as a local one.
//
// The message types, their schemas and the Validator are in
// github.com/choria-io/fisk-ai/internal/a2a/wire/v1. A caller that only parses or emits
// the protocol imports that package and links none of this: no transport, no telemetry
// SDK, no terminal.
package a2a

import (
	"context"
	"errors"
	"fmt"

	wire "github.com/choria-io/fisk-ai/internal/a2a/wire/v1"
	"github.com/choria-io/fisk-ai/internal/telemetry"
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
	// ErrMessageTooLarge indicates a message a sender assembled is over the size cap
	// and was refused rather than sent. It is separate from the caps applied to what
	// arrives, because a sender can act on it: an event carrying a large tool result
	// can be shortened, where a reply that arrived oversized can only be dropped.
	ErrMessageTooLarge = errors.New("message exceeds the size limit")
	// ErrStreamUnsupported indicates a request asked for an event stream on a
	// transport that carries a single reply.
	ErrStreamUnsupported = errors.New("transport cannot carry an event stream")
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

// StampRequest fills in the framing fields the v1 header schema requires beyond the
// protocol id a constructor sets, and takes the traceparent from the span ctx carries so
// a receiver's spans join this one's trace. It is empty when nothing is tracing.
//
// wire.StampRequest is the same fill given a traceparent, for a caller holding no
// context.
func StampRequest(ctx context.Context, h *wire.Header, sender string, recipient string) {
	wire.StampRequest(h, sender, recipient, telemetry.TraceContextFrom(ctx).TraceParent)
}
