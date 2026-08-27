//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package tui

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/choria-io/fisk-ai/internal/util"
)

// promptPage is the Pages name the prompt overlays occupy. onKey lets the focused
// prompt widget own its keys while this page is front.
const promptPage = "prompt"

// errPromptCanceled is returned when the operator dismisses a prompt without
// answering (Esc). The caller treats it, like any prompt error, as a denial.
var errPromptCanceled = errors.New("the operator dismissed the prompt")

// aborted reports a prompt the context ended under as toolkit.ErrPromptAborted. The
// operator was asked and never answered, so the caller must not record a decision
// from it; on a checkpointed run one would be replayed on every later resume.
func aborted(ctx context.Context) error {
	return fmt.Errorf("%w: %w", toolkit.ErrPromptAborted, ctx.Err())
}

// errPromptLeft ends a prompt because the operator pressed a leave key while it was on
// screen. It reports as toolkit.ErrPromptAborted rather than as a dismissal: somebody
// leaving decided nothing, and a reply built from this would decline a gated command on
// their behalf and be delivered to the run later.
var errPromptLeft = fmt.Errorf("%w: the operator left with the prompt on screen", toolkit.ErrPromptAborted)

// tcellPrompter implements util.Prompter with native tview widgets, driven from the
// run goroutine. Each method marshals its widget onto the tview loop and blocks
// until the operator answers or ctx is canceled; the caller (the confirm gate and
// the human-in-the-loop builtins) treats an answer of no, and any error other than
// toolkit.ErrPromptAborted, as an authoritative denial. A torn-down loop or a
// canceled run reports the abort instead, since nobody answered. It never owns the
// deny default itself.
// A prompt no longer has a run behind it once the reply set has ended under it, which
// is why the leave key ends it here rather than by asking the run to stop: the run may
// be gone, and nothing else would take the overlay off the screen.
type tcellPrompter struct {
	live *Live

	// left is closed by the leave key while a prompt is on screen, and replaced for the
	// next one. It is nil when nothing is being asked.
	mu   sync.Mutex
	left chan struct{}
}

func newTcellPrompter(live *Live) *tcellPrompter {
	return &tcellPrompter{live: live}
}

// asking opens the channel the leave key closes and returns it, along with the func
// that closes the prompt out again once it has been answered.
func (p *tcellPrompter) asking() (<-chan struct{}, func()) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.left = make(chan struct{})
	left := p.left

	return left, func() {
		p.mu.Lock()
		defer p.mu.Unlock()

		if p.left == left {
			p.left = nil
		}
	}
}

// leave ends the prompt on screen, as the leave key does when the operator is going
// rather than answering. It does nothing when nothing is being asked.
func (p *tcellPrompter) leave() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.left == nil {
		return
	}

	close(p.left)
	p.left = nil
}

var _ toolkit.Prompter = (*tcellPrompter)(nil)

// CanPrompt reports true: the tcell prompter only exists when the full-screen UI owns
// an interactive terminal, so an operator is always reachable through its modals.
func (p *tcellPrompter) CanPrompt() bool { return true }

// ApproveCommand shows the confirm-gate modal (No default-focused; Enter on it or
// Esc declines) and returns the three-way choice.
func (p *tcellPrompter) ApproveCommand(ctx context.Context, req toolkit.GateRequest) (toolkit.ConfirmChoice, error) {
	p.live.setBlocked()
	defer p.live.setRunning()

	result := make(chan toolkit.ConfirmChoice, 1)
	// The modal draws its text through tview's tag-aware printer, so the whole body
	// is escaped to keep a literal "[" in the model-supplied command from opening a
	// color tag; the fixed wording carries no brackets, so escaping it is harmless.
	body := tview.Escape(fmt.Sprintf("Run this command?\n\n%s\n\n%q carries tag %q",
		util.SanitizeForDisplay(req.Display), req.Command, req.Tag))

	p.modal(body, []string{"No", "Once", "Always"}, func(idx int) {
		switch idx {
		case 1:
			result <- toolkit.ConfirmOnce
		case 2:
			result <- toolkit.ConfirmAlways
		default:
			result <- toolkit.ConfirmNo
		}
	})

	left, answered := p.asking()
	defer answered()

	select {
	case <-ctx.Done():
		p.dismiss()
		return toolkit.ConfirmNo, aborted(ctx)
	case <-left:
		p.dismiss()
		return toolkit.ConfirmNo, errPromptLeft
	case r := <-result:
		return r, nil
	}
}

// Confirm shows a yes/no modal (No default-focused).
func (p *tcellPrompter) Confirm(ctx context.Context, question string) (bool, error) {
	p.live.setBlocked()
	defer p.live.setRunning()

	result := make(chan bool, 1)

	p.modal(tview.Escape(util.SanitizeForDisplay(question)), []string{"No", "Yes"}, func(idx int) {
		result <- idx == 1
	})

	left, answered := p.asking()
	defer answered()

	select {
	case <-ctx.Done():
		p.dismiss()
		return false, aborted(ctx)
	case <-left:
		p.dismiss()
		return false, errPromptLeft
	case r := <-result:
		return r, nil
	}
}

// Select shows the options in a list; Enter chooses, Esc cancels.
func (p *tcellPrompter) Select(ctx context.Context, question string, options []string) (int, error) {
	p.live.setBlocked()
	defer p.live.setRunning()

	type res struct {
		idx int
		err error
	}
	result := make(chan res, 1)

	p.present(func(finish func()) (tview.Primitive, tview.Primitive) {
		list := tview.NewList().ShowSecondaryText(false)
		list.SetBorder(true).SetTitle(promptTitle(question))
		for i, opt := range options {
			idx := i
			// List escapes item text itself, so it is only sanitized here.
			list.AddItem(util.SanitizeForDisplay(opt), "", 0, func() {
				finish()
				result <- res{idx: idx}
			})
		}
		list.SetDoneFunc(func() {
			finish()
			result <- res{idx: -1, err: errPromptCanceled}
		})
		return overlay(list, 60, clamp(len(options)+2, 3, 20)), list
	})

	left, answered := p.asking()
	defer answered()

	select {
	case <-ctx.Done():
		p.dismiss()
		return -1, aborted(ctx)
	case <-left:
		p.dismiss()
		return -1, errPromptLeft
	case r := <-result:
		return r.idx, r.err
	}
}

// Input shows a single free-text field pre-filled with def; Enter submits, Esc
// cancels.
func (p *tcellPrompter) Input(ctx context.Context, question, def string) (string, error) {
	p.live.setBlocked()
	defer p.live.setRunning()

	type res struct {
		text string
		err  error
	}
	result := make(chan res, 1)

	p.present(func(finish func()) (tview.Primitive, tview.Primitive) {
		input := tview.NewInputField().SetText(util.SanitizeForDisplay(def))
		input.SetBorder(true).SetTitle(promptTitle(question))
		input.SetDoneFunc(func(key tcell.Key) {
			finish()
			if key == tcell.KeyEsc {
				result <- res{err: errPromptCanceled}
				return
			}
			result <- res{text: input.GetText()}
		})
		return overlay(input, 60, 3), input
	})

	left, answered := p.asking()
	defer answered()

	select {
	case <-ctx.Done():
		p.dismiss()
		return "", aborted(ctx)
	case <-left:
		p.dismiss()
		return "", errPromptLeft
	case r := <-result:
		return r.text, r.err
	}
}

// modal shows a button modal. decide receives the chosen button index, or -1 when
// Esc dismisses it (which maps to the safe default, since the safe option is button
// zero and every decide treats a non-affirmative index as the decline).
func (p *tcellPrompter) modal(body string, buttons []string, decide func(idx int)) {
	p.present(func(finish func()) (tview.Primitive, tview.Primitive) {
		m := tview.NewModal().SetText(body).AddButtons(buttons)
		m.SetDoneFunc(func(idx int, _ string) {
			finish()
			decide(idx)
		})
		m.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
			if ev.Key() == tcell.KeyEsc {
				finish()
				decide(-1)
				return nil
			}
			return ev
		})
		return m, m
	})
}

// present adds a prompt overlay on the tview loop and focuses its widget. build
// returns the page primitive and the widget to focus, and receives a finish
// function (run on the loop) that removes the overlay and restores focus.
func (p *tcellPrompter) present(build func(finish func()) (page, focus tview.Primitive)) {
	p.live.v.app.QueueUpdateDraw(func() {
		finish := func() {
			p.live.v.pages.RemovePage(promptPage)
			p.live.v.app.SetFocus(p.live.v.view)
		}
		page, focus := build(finish)
		p.live.v.pages.AddPage(promptPage, page, true, true)
		p.live.v.app.SetFocus(focus)
	})
}

// dismiss removes the current prompt overlay from the run goroutine, used when ctx
// is canceled while a prompt is up.
func (p *tcellPrompter) dismiss() {
	p.live.v.app.QueueUpdateDraw(func() {
		p.live.v.pages.RemovePage(promptPage)
		p.live.v.app.SetFocus(p.live.v.view)
	})
}

// promptTitle builds a border title from a model-supplied question, escaped since a
// Box title is drawn through tview's tag-aware printer.
func promptTitle(q string) string {
	return " " + tview.Escape(util.SanitizeForDisplay(q)) + " "
}

// clamp bounds v to [lo, hi].
func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
