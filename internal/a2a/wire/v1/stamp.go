//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package wire

import (
	"time"
)

// StampRequest fills in the framing fields the v1 header schema requires beyond the protocol
// id a constructor sets: the message id, a conversation tag, the timestamp, and sender, which
// is the name the caller answers to. A message that skips them is refused by the schema.
//
// A request tag or a conversation tag the caller already set is kept rather than minted over,
// so a caller names its own turn and correlates its own conversation before the message goes
// out. A direct tool or discovery RPC carries neither and gets one fresh id for both. Sequence
// is set to zero: the transport reply inbox correlates the answer.
//
// A recipient names who the message is for, and an empty one leaves whatever the header already
// carried. The traceparent is the W3C trace context a receiver joins its spans to, and an empty
// one leaves the field off.
func StampRequest(h *Header, sender string, recipient string, traceparent string) {
	id := NewID()

	// A request tag the caller set is kept, so a caller holds the tag its own task
	// answers to before the task is sent. Canceling a task and answering its questions
	// both name that tag, and a caller that only learns it when the call returns cannot
	// name the call it is inside. A malformed one is refused by the schema rather than
	// minted over, so a caller hears about its own mistake.
	if h.Request == "" {
		h.Request = id
	}
	// The message gets an id of its own. Request names the turn every reply echoes and ID
	// names this message, so a turn that sends more than one message can tell them apart.
	h.ID = NewID()
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
	h.TraceParent = traceparent
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
