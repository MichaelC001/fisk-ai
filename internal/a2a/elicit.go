//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

// The elicit family gives every question and every answer an id of its own, so nothing in
// a body says what kind it is. The four maps below are the whole of the translation, and a
// protocol id that appears in none of them is one this build does not name.
//
// A question's suffix is its ElicitKind verbatim, so those two cannot drift. An answer's is
// the kind it answers rather than the field it carries, which pairs the two halves in a
// capture at the cost of a written table: choice answers approve, confirmed answers
// confirm, index answers select and value answers input.

var (
	// elicitRequestProtocols is the id a question of each kind travels under.
	elicitRequestProtocols = map[ElicitKind]string{
		ElicitApprove: ElicitRequestApproveProtocol,
		ElicitConfirm: ElicitRequestConfirmProtocol,
		ElicitSelect:  ElicitRequestSelectProtocol,
		ElicitInput:   ElicitRequestInputProtocol,
	}

	// elicitRequestKinds is the reverse, for reading a question back off its id.
	elicitRequestKinds = map[string]ElicitKind{}

	// elicitReplyProtocols is the id each answer travels under. AnswerWaiting is here
	// because it arrives on the same path and decodes into the same type, though it
	// answers nothing.
	elicitReplyProtocols = map[ElicitAnswer]string{
		AnswerChoice:     ElicitReplyApproveProtocol,
		AnswerConfirmed:  ElicitReplyConfirmProtocol,
		AnswerIndex:      ElicitReplySelectProtocol,
		AnswerValue:      ElicitReplyInputProtocol,
		AnswerNoOperator: ElicitReplyNoOperatorProtocol,
		AnswerWaiting:    ElicitWaitingProtocol,
	}

	// elicitReplyAnswers is the reverse, for reading an answer back off its id.
	elicitReplyAnswers = map[string]ElicitAnswer{}
)

func init() {
	for kind, protocol := range elicitRequestProtocols {
		elicitRequestKinds[protocol] = kind
	}

	for answer, protocol := range elicitReplyProtocols {
		elicitReplyAnswers[protocol] = answer
	}
}

// ElicitRequestProtocolFor is the protocol id a question of this kind is carried under, and
// false for a kind this build does not name. ElicitRequest stamps it rather than leaving it
// to a caller.
func ElicitRequestProtocolFor(kind ElicitKind) (string, bool) {
	protocol, ok := elicitRequestProtocols[kind]

	return protocol, ok
}

// ElicitReplyProtocolFor is the protocol id this answer is carried under, and false for an
// answer this build does not name.
func ElicitReplyProtocolFor(answer ElicitAnswer) (string, bool) {
	protocol, ok := elicitReplyProtocols[answer]

	return protocol, ok
}

// elicitKindOf is the question an id names, and false for anything else. The lookup is
// exact: the family mixes a two-segment suffix with three-segment ones, so there is no
// prefix rule to cut on the way the event family has.
func elicitKindOf(protocol string) (ElicitKind, bool) {
	kind, ok := elicitRequestKinds[protocol]

	return kind, ok
}

// elicitAnswerOf is the answer an id names, and false for anything else.
func elicitAnswerOf(protocol string) (ElicitAnswer, bool) {
	answer, ok := elicitReplyAnswers[protocol]

	return answer, ok
}

// ElicitAnswerProtocols is every id an answer to a question travels under, which is the set
// a worker's answer path admits. It holds the five under elicit.reply and the waiting
// keepalive, and it holds no question: those travel the other way, and admitting one here
// would put a message of another type on a path contracted for this one.
func ElicitAnswerProtocols() []string {
	return []string{
		ElicitReplyApproveProtocol,
		ElicitReplyConfirmProtocol,
		ElicitReplySelectProtocol,
		ElicitReplyInputProtocol,
		ElicitReplyNoOperatorProtocol,
		ElicitWaitingProtocol,
	}
}
