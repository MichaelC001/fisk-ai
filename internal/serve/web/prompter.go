//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// The prompter is the whole of what this channel does with Work.Prompter, so a change
// to that contract is a compile error here rather than a run that asks nobody.
var _ toolkit.Prompter = (*prompter)(nil)

// prompter ends a turn on a question and answers the next turn's question from the
// answer its request carried.
//
// A page is not an operator: it is reading the response, so it cannot answer until the
// response is over. Every unheld question is therefore recorded and reported as
// toolkit.ErrPromptAborted, which leaves the call unanswered rather than declined, ends
// the run as a suspend, and has the next resume dispatch the same call and ask again.
// That second question is answered from here when the request that resumed the thread
// carried an answer for the call.
//
// The answer lives on this turn and nowhere else. Nothing persists a question or its
// answer: runstate has no question record, and Checkpoint.Answer is for a deferred call,
// which an unanswered question here never produces. A prompter that recorded and aborted
// unconditionally would re-ask and re-abort on every resume forever.
type prompter struct {
	log *slog.Logger

	// mu guards the answer, written when the turn is built on the request goroutine and
	// read on the run's, and the question, written on the run's goroutine and read at
	// the ending.
	mu sync.Mutex

	// held is the answer the request carried, spent on the first question about its
	// call. A later call of the same tool carries its own arguments and is asked about
	// on its own.
	held *Answer

	// asked is the question this turn ended on. Only the first is kept: the abort ends
	// the run, so a second question on one turn is asked on the next request, which is
	// where the resume puts it once the first has its answer.
	asked *Question
}

func newPrompter(log *slog.Logger) *prompter {
	return &prompter{log: log}
}

// hold takes the answer the request carried.
func (p *prompter) hold(a Answer) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.held = &a
}

// heldFor reports the answer this turn holds for the named call, and spends it. An
// answer of another kind is left in place and reported as absent: the question is asked
// again rather than answered with a value of the wrong shape.
func (p *prompter) heldFor(toolUseID string, kind QuestionKind) (Answer, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.held == nil || toolUseID == "" || p.held.ToolUseID != toolUseID {
		return Answer{}, false
	}
	if p.held.Kind != kind {
		p.log.Warn("An answer names the call asked about but not the question's kind", "tool_use", toolUseID, "asked", kind, "answered", p.held.Kind)

		return Answer{}, false
	}

	a := *p.held
	p.held = nil

	return a, true
}

// question is what this turn ended on, nil when it asked nothing.
func (p *prompter) question() *Question {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.asked
}

// ask records the question and reports it unanswered, which ends the run with the call
// unanswered. A second question on the turn keeps the first: the abort has already
// ended the run, and the resume asks the second once the first has its answer.
func (p *prompter) ask(q Question) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.asked == nil {
		p.asked = &q
	} else {
		p.log.Info("A second question on one turn is asked on the next request", "kind", q.Kind, "tool_use", q.ToolUseID)
	}

	return fmt.Errorf("%w: the page answers on its next request", toolkit.ErrPromptAborted)
}

// CanPrompt reports true: the page can be asked, on the response that ends the turn.
func (p *prompter) CanPrompt() bool { return true }

// ApproveCommand answers a confirm-gated command from the approval the request carried
// for that call, and otherwise ends the turn on the question.
func (p *prompter) ApproveCommand(_ context.Context, req toolkit.GateRequest) (toolkit.ConfirmChoice, error) {
	a, held := p.heldFor(req.ToolUseID, KindApprove)
	if held {
		p.log.Info("Answering an approval from the request that resumed this thread", "tool_use", req.ToolUseID, "command", req.Command)

		return a.Approval, nil
	}

	return toolkit.ConfirmNo, p.ask(Question{
		Kind:      KindApprove,
		ToolUseID: req.ToolUseID,
		Command:   req.Command,
		Display:   req.Display,
		Tag:       req.Tag,
	})
}

// Confirm answers a yes/no question from the request, and otherwise ends the turn on
// it.
func (p *prompter) Confirm(ctx context.Context, question string) (bool, error) {
	id, err := callOf(ctx)
	if err != nil {
		return false, err
	}

	a, held := p.heldFor(id, KindConfirm)
	if held {
		return a.Confirmed, nil
	}

	return false, p.ask(Question{Kind: KindConfirm, ToolUseID: id, Text: question})
}

// Select answers a choice from the request, and otherwise ends the turn on it. An index
// outside the options is a choice nobody was offered, reported rather than clamped.
func (p *prompter) Select(ctx context.Context, question string, options []string) (int, error) {
	id, err := callOf(ctx)
	if err != nil {
		return -1, err
	}

	a, held := p.heldFor(id, KindSelect)
	if held {
		if a.Index < 0 || a.Index >= len(options) {
			return -1, fmt.Errorf("the page chose option %d of %d", a.Index, len(options))
		}

		return a.Index, nil
	}

	return -1, p.ask(Question{Kind: KindSelect, ToolUseID: id, Text: question, Options: options})
}

// Input answers a free-text question from the request, and otherwise ends the turn on
// it. An empty string is a valid answer.
func (p *prompter) Input(ctx context.Context, question, def string) (string, error) {
	id, err := callOf(ctx)
	if err != nil {
		return "", err
	}

	a, held := p.heldFor(id, KindInput)
	if held {
		return a.Value, nil
	}

	return "", p.ask(Question{Kind: KindInput, ToolUseID: id, Text: question, Default: def})
}

// callOf is the call a human-in-the-loop question belongs to. The three question tools
// take no call id of their own, so it comes from the context ExecuteUse marks.
//
// A question outside a tool call is refused with an error that is not an abort: the page
// answers by naming the call, so a question naming none could never be answered and
// would be asked again on every resume. The tool turns the error into its own null
// result, which the model reasons about.
func callOf(ctx context.Context) (string, error) {
	id := toolkit.ToolUseIDFromContext(ctx)
	if id == "" {
		return "", fmt.Errorf("a question cannot be put to the page outside a tool call: nothing the page answered could be delivered back to it")
	}

	return id, nil
}
