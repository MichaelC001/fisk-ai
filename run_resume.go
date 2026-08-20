//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/serve/a2aendpoint"
)

// resumeToken is what --resume travels as: the conversation token, which is the only
// handle a worker takes.
//
// A person types either of two things. A session id is what `fisk session ls` shows
// and what a suspended run printed, and it is looked up in the store, which holds the
// token beside the conversation. A token is what somebody moving between machines has,
// and it is sent as it stands, since the worker resolves it to a journal itself.
//
// A session id the store does not hold is treated as a token rather than refused: only
// the worker can say whether a token names a conversation, and it answers
// unknown_conversation when it does not.
func resumeToken(cfg *config.Config, handle string) (string, error) {
	if handle == "" {
		return "", nil
	}

	rs, err := agent.LoadSession(cfg, handle)
	if err != nil {
		if errors.Is(err, runstate.ErrNotFound) || errors.Is(err, runstate.ErrInvalidID) {
			return handle, nil
		}

		return "", err
	}

	if rs.ConversationToken == "" {
		// A journal from before a conversation had a token, or one a queued job wrote.
		// The worker names journals by hashing a token, so there is no way to ask it for
		// this one, and inventing a token would name a different journal.
		return "", fmt.Errorf("session %q cannot be continued: it was written before conversations had tokens, and is readable with 'fisk session show %s'", handle, handle)
	}

	return rs.ConversationToken, nil
}

// resumeHint is the command that continues this conversation, printed after a run that
// can be continued.
//
// The two modes print different handles because only one of them works in each. A local
// resume takes the session id and resolves it through the store, which holds the token
// beside the conversation, and the id is what fisk session names. A remote resume has
// no store to resolve it against, so the hint carries the token itself along with the
// context the agent is on.
//
// The token is not withheld from it. Reading one takes the access that already reads the
// journal it belongs to, and fisk session show prints it on request, so a hint naming
// the session id remotely handed a person a string their next command refuses.
func resumeHint(identity, natsContext, token string) string {
	if token == "" {
		return ""
	}

	// The identity goes on the remote command because it is what addresses the agent the
	// conversation happened with. Left off, the command works only where a configuration
	// file names that same agent, so it is wrong in any other directory and wrong in a
	// terminal that named the agent on the command line.
	if natsContext != "" {
		return fmt.Sprintf("resume with: fisk run --nats-context %s --identity %s --resume %s", natsContext, identity, token)
	}

	return fmt.Sprintf("resume with: fisk run --resume %s", a2aendpoint.SessionFor(identity, token))
}
