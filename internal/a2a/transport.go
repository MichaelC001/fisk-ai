//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import "context"

// RouteHint tells a transport which of an agent's request-reply paths a message
// belongs to, so it can route it (e.g. a NATS transport picks the subject). It is
// ROUTING ONLY: the meaning of a message is always dispatched from its
// Header.Protocol id by the engine, never inferred from this hint. A transport may
// use it to keep the paths on separate channels as a permission seam, but must not
// treat it as the message's type.
type RouteHint int

const (
	// OpDiscovery is the path an agent answers discovery requests on.
	OpDiscovery RouteHint = iota
	// OpTool is the path an agent answers direct tool invocation requests on.
	OpTool
	// OpTask is the path an agent answers task requests on. One request there
	// produces a reply set rather than a reply, so a binding carries it only when it
	// is also a StreamingTransport.
	OpTask
	// OpCancel and OpElicit are the paths belonging to one running task: the cancel
	// addressed to it and the answers to its questions. Nothing Serves them, since a
	// StreamingTransport subscribes to them per task rather than registering a handler
	// for the identity, so they exist here only to be named.
	OpCancel
	OpElicit
)

// Transport is the pluggable binding the a2a engine rides on. One implementation
// exists per wire binding (NATS today, Choria services later); it is selected from
// the registry by name and constructed from a shared conns.Provider. The engine
// owns all message validation and the concurrency bound; a Transport only moves bytes
// and keeps the routing paths separate.
type Transport interface {
	// RoundTrip sends body to agent on the op path and returns the raw reply. It
	// returns ErrAgentUnavailable when no agent answers or the deadline elapses. The
	// engine validates and size-caps the reply; the transport must not decode it.
	RoundTrip(ctx context.Context, agent string, op RouteHint, body []byte) ([]byte, error)
	// Serve registers h as the handler for inbound messages on the op path for this
	// transport's own identity. The transport must invoke h synchronously on its
	// per-path serving goroutine: h answers on the Replier it is given, which is
	// single-shot and valid only for that call, and h never blocks on work of its own,
	// so a busy engine refuses rather than holding the path. It may be called once per
	// op.
	Serve(op RouteHint, h Handler) error
	// Describe returns transport-neutral {label, value} lines describing how the
	// named identity is reached, for display by the CLI (e.g. the NATS subjects).
	Describe(identity string) []DescLine
	// Close releases the transport's own resources (e.g. its service registration).
	// It does not close the shared conns.Provider, which the caller owns.
	Close() error
}

// SubjectNamer is the optional interface a Transport implements when its addresses are
// names worth showing somebody.
//
// It exists for the wire log: an operator reading what a terminal and an agent said to
// each other wants the address each message traveled on, because that is what they
// subscribe to when they go looking with their own tools. A binding whose addressing is
// not nameable does not implement it and the log simply carries no address.
//
// request is the correlating request id for the paths that are per task, and is ignored
// by the paths that are not.
type SubjectNamer interface {
	// Subject renders the address a message on the op path for agent travels on.
	Subject(op RouteHint, agent, request string) string
}

// ReplySetTransport is a Transport that can answer one request with several
// messages. A binding implements it when its substrate can do so; the server and the
// client each assert for it once, when the transport is built, so whether a tool call
// can say it is still working is known at startup rather than per request.
//
// It is separate from StreamingTransport because a tool call needs a reply set and
// nothing else: it has no cancel, and a binding that serves tools should not have to
// implement one.
type ReplySetTransport interface {
	Transport

	// Stream sends body and returns a Reader over the reply set it produces.
	Stream(ctx context.Context, agent string, op RouteHint, body []byte) (Reader, error)
}

// StreamingTransport is a ReplySetTransport that can also carry a task: the reply set
// in one direction and a cancel addressed to a running task in the other.
//
// Cancel is here rather than on an interface of its own because a task is what has a
// cancel: a binding that can carry a reply set is the binding that can run a task
// long enough for one to arrive.
type StreamingTransport interface {
	ReplySetTransport

	// WatchCancel routes cancels addressed to the named request to h, and only to
	// this process, until the returned watch is released. It is the running task's
	// own subscription rather than a path of the service, so it is opened when the
	// task is accepted and released on every ending. request must be a
	// ValidRequestID, since a binding may make it part of an address.
	WatchCancel(request string, h Handler) (TaskWatch, error)

	// SendCancel delivers body as a cancel for request on agent and returns the raw
	// reply, which the engine validates. It reports ErrAgentUnavailable when nothing
	// answers, which is how a caller tells a delivered cancel from a task that is not
	// running there.
	SendCancel(ctx context.Context, agent, request string, body []byte) ([]byte, error)

	// WatchElicitReplies routes the answers to the named task's questions to h, on
	// the same terms as WatchCancel: this process only, opened when the task is
	// accepted, released on every ending, and request must be a ValidRequestID.
	//
	// It is a path of its own rather than part of the cancel watch. The two carry
	// different messages, and a binding that makes each an address can then grant
	// them separately: answering a question decides what a run does, where a cancel
	// only stops it.
	WatchElicitReplies(request string, h Handler) (TaskWatch, error)

	// SendElicitReply delivers body as an answer to a question the named task asked
	// on agent, and returns the raw reply, which the engine validates. It reports
	// ErrAgentUnavailable when nothing answers, which is how the answering party
	// learns the run it was answering has ended.
	SendElicitReply(ctx context.Context, agent, request string, body []byte) ([]byte, error)

	// DescribeTasks returns the {label, value} lines describing how tasks reach the
	// named identity and where their cancels are addressed, for display beside
	// Describe's lines. It is here rather than on Describe because a binding that
	// cannot carry a reply set has no task path to describe, and a surface that serves
	// tasks must not have to know what a subject is to print one.
	//
	// elicits says whether the surface puts questions to its callers. With it false
	// the answer address is left out, since a caller publishing there would reach a
	// run that asks nothing and every question would be refused before it was asked.
	DescribeTasks(identity string, elicits bool) []DescLine
}

// TaskWatch is one running task's claim on a class of messages addressed to it:
// its cancels, or the answers to its questions.
type TaskWatch interface {
	// Close stops routing those messages for that request. It is called on every
	// ending of the task, so a second call is harmless.
	Close() error
}

// Reader yields the raw messages of one reply set in arrival order. The engine
// validates each body; the Reader decides only which messages belong to the set and
// where it ends.
type Reader interface {
	// Next returns the next message body, or io.EOF once the terminal message has
	// been returned.
	Next(ctx context.Context) ([]byte, error)
	// Close releases the reader. It tells the producer nothing: a run keeps
	// publishing until it ends, and Cancel is how a caller says it has stopped caring.
	Close() error
}

// Caller is what the transport knows about who sent a request.
//
// It is supplied by the transport rather than read out of the message body, for the
// same reason Replier targets only the inbox the transport supplied: a body can claim
// any sender, so a claim in one is evidence of nothing. Header.Sender remains that
// claim and is not merged into this.
//
// It is per request, not per transport. A binding that can authenticate some requests
// and not others reports each one as it is, so a transport is not one or the other for
// its lifetime.
type Caller struct {
	// Name is the transport's term for the principal, empty when it knows none.
	Name string

	// Verified reports whether the transport authenticated Name. A false value means
	// the transport is vouching for nothing, whether or not Name is set, so anything
	// deciding on identity must read this and not Name alone.
	Verified bool
}

// Handler processes one inbound message body and answers through reply. It is
// invoked synchronously on the transport's serving goroutine; the engine may
// acquire a semaphore and spawn a worker inside it. reply stays valid for use from
// that worker after Handler returns.
//
// caller is what the transport knows about who sent this request, which for a binding
// that authenticates nobody is the zero value.
type Handler func(ctx context.Context, caller Caller, body []byte, reply Replier)

// Replier is the reply side of one inbound request, the transport-neutral form of
// a NATS micro request's reply. It targets only the reply inbox the transport
// supplied for this request, never an identity taken from the message body, and is
// single-shot: exactly one of Respond or Error is called per request. It stays
// valid for use from a worker goroutine spawned by the handler after the handler
// returns.
type Replier interface {
	// Respond sends a successful reply body.
	Respond(body []byte) error
	// Error reports a transport-level handler failure with a code and description,
	// distinct from an in-band application error carried in a normal reply body.
	Error(code, description string) error
}

// StreamReplier is the reply side of a request on a StreamingTransport. Every
// Replier such a transport supplies implements it, so a handler reaches it by type
// assertion on the Replier it was given.
type StreamReplier interface {
	Replier

	// Publish sends one message of the reply set, marking whether it is the last, to
	// the same inbox Respond targets. It may be called many times. Respond stays what
	// it was and carries the ack, so Replier's single-shot contract describes the one
	// message it now sends.
	Publish(body []byte, final bool) error
}

// DescLine is one {label, value} row describing how an identity is reached, for
// CLI display. It is transport-neutral: a NATS transport fills it with subjects, a
// later transport with whatever addresses it.
type DescLine struct {
	Label string
	Value string
}
