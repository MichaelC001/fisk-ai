//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	"context"
	"time"

	"github.com/choria-io/fisk-ai/internal/telemetry"
)

// stampRequest fills in the framing fields of a standalone request header. The
// message constructors set only the protocol id, so a request still needs an id,
// the correlation and conversation tags, a timestamp, and the sender before it is
// schema-valid. A direct tool or discovery RPC is not part of a larger task or
// session, so id, request and conversation are all the same fresh id, and
// sequence is unused (the transport reply inbox handles correlation), matching the
// transport notes for direct tool calls.
//
// The trace context of whatever span ctx carries is stamped alongside the rest, so a
// receiver's spans join this one's trace. It is empty when nothing is tracing, which
// is what leaves the field off the wire.
func stampRequest(ctx context.Context, h *Header, sender string, recipient string) {
	id := NewID()

	h.ID = id
	h.Request = id
	// A conversation tag the caller set is kept, which is how the turns of one
	// conversation carry one tag: the field is the caller's own correlation and means
	// nothing to a receiver, so minting over it would leave it unable to do the one
	// thing it is for. Every message built here with the field empty gets a fresh id,
	// which is what a request that is part of no larger session carries.
	if h.Conversation == "" {
		h.Conversation = id
	}
	h.Sequence = 0
	h.Time = time.Now().UTC()
	h.Sender = Identity{Name: sender}
	h.TraceParent = telemetry.TraceContextFrom(ctx).TraceParent
	if recipient != "" {
		h.Recipient = &Identity{Name: recipient}
	}
}

// StampReply fills in the framing fields of a reply header so it echoes the
// request it answers. The request and conversation tags are copied from the
// inbound request, the sender becomes this agent's identity, and the recipient
// becomes the original sender.
//
// Sequence is left at zero, which is what a single reply carries. A message
// belonging to a reply set is numbered by the ReplyStream that sends it.
func StampReply(h *Header, req *Header, sender string) {
	h.ID = NewID()
	h.Request = req.Request
	h.Conversation = req.Conversation
	h.Sequence = 0
	h.Time = time.Now().UTC()
	h.Sender = Identity{Name: sender}
	if req.Sender.Name != "" {
		h.Recipient = &Identity{Name: req.Sender.Name}
	}
}
