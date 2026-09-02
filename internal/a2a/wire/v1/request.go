//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package wire

import "strings"

// RequestKind is one of the four things a caller can ask of a worker, carried as the
// suffix of the request's protocol id rather than as a field of the body.
//
// A caller declares it by building the request through one of the constructors, and a
// receiver reads it off the id. Nothing works it out from which fields are set: which
// fields each kind may carry is stated by that kind's schema, which every inbound path
// validates against before it decodes.
type RequestKind string

const (
	// RequestPrompt asks the agent for something. With a conversation token the prompt
	// runs as that conversation's next turn, and without one it opens a conversation.
	RequestPrompt RequestKind = "prompt"
	// RequestAnswer answers a question the conversation is still waiting on, for a
	// caller that was asked something and could not answer before the run gave up. The
	// run is resumed and the conversation gains no turn.
	RequestAnswer RequestKind = "answer"
	// RequestResume continues a run that stopped part way, which is what a caller sends
	// after a suspended ending.
	RequestResume RequestKind = "resume"
	// RequestRead asks to be told what a conversation holds. The worker sends the
	// blocks and ends the reply set, taking no turn and calling no model.
	RequestRead RequestKind = "read"
)

// requestProtocols is the id a request of each kind travels under.
var requestProtocols = map[RequestKind]string{
	RequestPrompt: RequestPromptProtocol,
	RequestAnswer: RequestAnswerProtocol,
	RequestResume: RequestResumeProtocol,
	RequestRead:   RequestReadProtocol,
}

// requestKinds is the reverse, for reading a request back off its id.
var requestKinds = map[string]RequestKind{}

func init() {
	for kind, protocol := range requestProtocols {
		requestKinds[protocol] = kind
	}
}

// RequestProtocolFor is the protocol id a request of this kind is carried under, and false
// for a kind this build does not name. Request stamps it rather than leaving it to a
// caller.
func RequestProtocolFor(kind RequestKind) (string, bool) {
	protocol, ok := requestProtocols[kind]

	return protocol, ok
}

// RequestProtocols is every id a request travels under, which is the set a worker's task
// path admits.
func RequestProtocols() []string {
	return []string{
		RequestPromptProtocol,
		RequestAnswerProtocol,
		RequestResumeProtocol,
		RequestReadProtocol,
	}
}

// IsRequestProtocol reports whether an id names a request. The dot is part of the prefix,
// since without it io.choria.fisk-ai.v1.requestfoo is a match.
//
// It answers on the id alone, for a caller holding a raw body it has not decoded. A caller
// that wants the kind reads it off the decoded Request.
func IsRequestProtocol(protocol string) bool {
	return strings.HasPrefix(protocol, RequestProtocol+".")
}

// RequestKindOf is the kind an id names, and false for anything else. It is the inverse
// of RequestProtocolFor.
func RequestKindOf(protocol string) (RequestKind, bool) {
	kind, ok := requestKinds[protocol]

	return kind, ok
}
