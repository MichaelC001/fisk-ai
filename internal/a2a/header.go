//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	"regexp"
	"time"
)

// schemaNamePattern is what the v1 schema allows an Identity's Name and Instance to
// be, and what it allows the ID, Request and Conversation tags to be. It is the
// schema's rule expressed in Go, so the two move together.
var schemaNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

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
	return schemaNamePattern.MatchString(name)
}

// MaxRequestIDBytes is the longest a request tag may be. The character set alone left
// it limited only by the message cap, so a caller could name a turn with several
// hundred kilobytes and have those bytes become part of a subject. Every id this
// package mints is 27 characters, and 64 leaves room for a caller that names its turns
// after something of its own.
const MaxRequestIDBytes = 64

// ValidRequestID reports whether id is one the v1 schema accepts for a message's ID,
// Request or Conversation tag: letters, digits, '-' and '_', not empty, and at most
// MaxRequestIDBytes long.
//
// It is the same character set as ValidIdentityName and is stated separately because
// the two answer different questions. A request id correlates a reply set, and on the
// task path it also becomes part of the address the process running that task
// listens on, so a caller choosing those bytes freely would shape a subscription.
func ValidRequestID(id string) bool {
	if len(id) > MaxRequestIDBytes {
		return false
	}

	return schemaNamePattern.MatchString(id)
}

// Header carries the framing fields shared by every message. It is embedded into
// each message type so the fields marshal flat into the body, keeping a captured
// message self describing without the transport.
type Header struct {
	// Protocol is the message protocol id, e.g. RequestPromptProtocol. It names one
	// shape, so reading it is enough to know what the message is. A constructor stamps
	// it and MarshalJSON sets it from the message's own kind, so a value set by hand
	// that disagrees with the body does not reach the wire.
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
	// TraceParent is the W3C trace context of the span that sent this message, empty
	// when the sender was not tracing. It is what lets a receiver's spans join the
	// sender's trace rather than starting one of their own.
	//
	// It is in the body rather than in transport metadata because a message reaches
	// bindings that have none: a queued job is a request stored on a work item with no
	// headers anywhere. It is unauthenticated, exactly like Sender, so the trace it
	// names is the sender's choice and nothing reads it as evidence of who called.
	TraceParent string `json:"traceparent,omitempty"`
}
