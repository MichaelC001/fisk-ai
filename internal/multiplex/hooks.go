//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package multiplex

import (
	"context"

	"github.com/choria-io/fisk-ai/internal/a2a"
	wire "github.com/choria-io/fisk-ai/internal/a2a/wire/v1"
)

// ClientHooks drives a reporter from what an a2a client sees, so a caller wires one
// option and reports nothing by hand. Pass the result to a2a.WithClientHooks.
//
// The client is where a run's state is known: it holds the conversation, it puts the
// questions and it is told how each turn ended. A caller that already has hooks of its
// own composes them, since a client takes one set.
//
// A nil reporter gives hooks that fire nothing, so the caller wires the same option
// whether or not a multiplexer claimed the process.
func ClientHooks(r StateReporter) a2a.ClientHooks {
	if r == nil {
		return a2a.ClientHooks{}
	}

	return a2a.ClientHooks{
		// The prompt has left, so the wait for a person is over. TurnAccepted says the
		// same thing a moment later, and this is that moment: an agent under load, or on
		// another machine, can take seconds to ack, and a pane that says a person is
		// needed for work already sent is one somebody walks over to for nothing.
		PromptSubmit: func(_ context.Context, _ a2a.ClientPromptSubmitInfo) (a2a.ClientPromptSubmitResult, error) {
			r.Working()

			return a2a.ClientPromptSubmitResult{}, nil
		},

		TurnAccepted: func(_ context.Context, _ a2a.TurnAcceptedInfo) {
			r.Working()
		},

		QuestionAsked: func(_ context.Context, info a2a.QuestionAskedInfo) {
			r.Blocked(questionReason(info))
		},

		// Whether they answered or the run gave up on the question, nobody is waiting on
		// a person any more. A turn that ended while this was being decided reports its
		// own end after this, so the pane is not left working by it.
		QuestionAnswered: func(_ context.Context, _ a2a.QuestionAnsweredInfo) {
			r.Working()
		},

		// The agent has stopped, however it stopped. What follows is the operator's: the
		// next prompt of a conversation that continues, or the transcript of one that is
		// over.
		TurnEnd: func(_ context.Context, _ a2a.ClientTurnEndInfo) {
			r.Idle()
		},
	}
}

// questionReason is what the pane shows beside a blocked agent, which is how somebody
// watching several picks the one to go to.
//
// An approve question carries the command instead of a question, so it is labeled: on a
// list of panes a bare command line does not say that a decision is what is wanted.
func questionReason(info a2a.QuestionAskedInfo) string {
	if info.Kind == wire.ElicitApprove {
		return "approve: " + info.Display
	}

	return info.Question
}
