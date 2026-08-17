//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"time"

	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// defaultPromptWait bounds one question when a channel that defers supplies no wait of
// its own. It is what expose.agent.a2a.request_timeout falls back to, since waiting for
// a caller to answer is the same measurement.
const defaultPromptWait = 120 * time.Second

// promptsThrough is the prompter one piece of work puts its questions to.
//
// A nil prompter cannot be passed through: the run and the confirm gate call CanPrompt
// without guarding it, so a configuration carrying any gated tool would dereference nil.
// Denying is also the right answer, since a channel that supplied no prompter has nobody
// to ask.
func promptsThrough(work *Work) toolkit.Prompter {
	switch {
	case work.Prompter == nil:
		return toolkit.DefaultDenyPrompter()

	case work.PromptsMayBlock:
		return work.Prompter

	default:
		wait := work.PromptWait
		if wait <= 0 {
			wait = defaultPromptWait
		}

		return &boundedPrompter{Prompter: work.Prompter, wait: wait}
	}
}

// boundedPrompter gives the run back when a question goes unanswered, for a channel
// whose caller is not attached. Human think-time is minutes to days, so a worker that
// waited it out would serve nothing while it did.
//
// No channel in this repository reaches it: the queue supplies no prompter and the a2a
// prompt channel bounds its own questions, which is what lets a caller hold one open
// while a person reads it. It is here for an embedder's channel that can reach an
// operator but cannot tell whether one is still there.
//
// It bounds rather than answers. The prompter it wraps sees the elapsed wait as its own
// context ending, and only that prompter knows what an unanswered question of each kind
// produces: a question a tool asked defers, and a gate question leaves its call
// unanswered.
type boundedPrompter struct {
	toolkit.Prompter

	wait time.Duration
}

func (p *boundedPrompter) ApproveCommand(ctx context.Context, req toolkit.GateRequest) (toolkit.ConfirmChoice, error) {
	ctx, cancel := context.WithTimeout(ctx, p.wait)
	defer cancel()

	return p.Prompter.ApproveCommand(ctx, req)
}

func (p *boundedPrompter) Confirm(ctx context.Context, question string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, p.wait)
	defer cancel()

	return p.Prompter.Confirm(ctx, question)
}

func (p *boundedPrompter) Select(ctx context.Context, question string, options []string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, p.wait)
	defer cancel()

	return p.Prompter.Select(ctx, question, options)
}

func (p *boundedPrompter) Input(ctx context.Context, question, def string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, p.wait)
	defer cancel()

	return p.Prompter.Input(ctx, question, def)
}
