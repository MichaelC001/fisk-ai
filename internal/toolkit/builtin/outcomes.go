//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package builtin

import (
	"encoding/json"
	"fmt"
)

// The three functions here render the result one of the ask_human_* tools would have
// returned, for a caller supplying an answer to a call that deferred rather than
// answering the tool in the moment.
//
// They exist because the shapes are what the model was told to expect in each tool's
// description, so a supplier that invented its own would feed the model something it
// cannot read, silently. A supplier outside this repository has no way to know them
// otherwise, and copying them is how two definitions of one shape start drifting.
//
// Each takes the answer an operator gave and nothing else. Whether the answer may be
// supplied at all, and to which call, is the caller's question rather than this
// package's.

// The three question tools, named so a caller supplying an answer can say which of
// them it is answering without spelling the names itself.
const (
	AskHumanConfirmName = askHumanConfirmName
	AskHumanSelectName  = askHumanSelectName
	AskHumanInputName   = askHumanInputName
)

// ConfirmResult renders an ask_human_confirm answer. reason explains a false answer
// that was not a plain no, and is empty for an ordinary yes or no.
func ConfirmResult(confirmed bool, reason string) (string, error) {
	return renderOutcome(confirmOutcome{Confirmed: confirmed, Reason: reason})
}

// SelectResult renders an ask_human_select answer as the option that was chosen,
// which is the option text rather than its position: the model is told the tool
// answers with the option itself.
func SelectResult(selected string, reason string) (string, error) {
	return renderOutcome(selectOutcome{Selected: &selected, Reason: reason})
}

// InputResult renders an ask_human_input answer. An empty string is a value the
// operator gave, which is why NoAnswerResult is a separate call rather than this one
// with nothing in it.
func InputResult(value string, reason string) (string, error) {
	return renderOutcome(inputOutcome{Value: &value, Reason: reason})
}

// NoAnswerResult renders the answer of an operator who was reached and gave none, for
// whichever of the three tools deferred. It is the shape each of them produces for
// that case: a null answer carrying the reason, which is a normal result the model
// reasons about rather than a tool failure.
func NoAnswerResult(tool string, reason string) (string, error) {
	switch tool {
	case askHumanConfirmName:
		return renderOutcome(confirmOutcome{Reason: reason})
	case askHumanSelectName:
		return renderOutcome(selectOutcome{Reason: reason})
	case askHumanInputName:
		return renderOutcome(inputOutcome{Reason: reason})
	default:
		return "", fmt.Errorf("%q is not a question tool", tool)
	}
}

func renderOutcome(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("rendering a supplied answer: %w", err)
	}

	return string(data), nil
}
