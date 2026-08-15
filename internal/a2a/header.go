//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	"regexp"
	"time"
)

// identityNamePattern is what the v1 schema allows an Identity's Name and Instance to
// be. It is the schema's rule expressed in Go, so the two move together.
var identityNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// Identity names an agent. Name is the logical identity (it maps to the agent's
// configured identity) and Instance optionally identifies one running instance
// of that named agent.
type Identity struct {
	Name     string `json:"name"`
	Instance string `json:"instance,omitempty"`
}

// ValidIdentityName reports whether a name is one the v1 schema accepts for an
// Identity: letters, digits, '-' and '_', and not empty.
//
// It is exported so a sender can check a name it was given before building a message
// with it. Without that the mistake surfaces at the receiver as a message that failed
// schema validation, which names neither the field nor the rule.
func ValidIdentityName(name string) bool {
	return identityNamePattern.MatchString(name)
}

// Header carries the framing fields shared by every message. It is embedded into
// each message type so the fields marshal flat into the body, keeping a captured
// message self describing without the transport.
type Header struct {
	// Protocol is the message protocol id, e.g. RequestProtocol.
	Protocol string `json:"protocol"`
	// ID uniquely identifies this message.
	ID string `json:"id"`
	// Request is the correlation tag of the originating request; every reply in
	// the set echoes it. On a request message it equals ID.
	Request string `json:"request"`
	// Conversation is stable across multiple requests in a session.
	Conversation string `json:"conversation"`
	// Parent is the request that spawned this one, for multi-hop A->B->C. It is
	// empty for a top level request.
	Parent string `json:"parent,omitempty"`
	// Sequence is the per-request, gap-free, monotonic ordering authority. It is
	// never reused across a restart and is authoritative over Time for ordering.
	Sequence uint64 `json:"sequence"`
	// Time is advisory and for audit only; it is not an ordering authority.
	Time time.Time `json:"time"`
	// Sender identifies the agent that produced the message.
	Sender Identity `json:"sender"`
	// Recipient optionally identifies the intended agent.
	Recipient *Identity `json:"recipient,omitempty"`
	// MustUnderstand, when true, requires a receiver that does not understand the
	// protocol id to fail closed rather than ignore the message.
	MustUnderstand bool `json:"must_understand,omitempty"`
}
