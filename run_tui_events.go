//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"

	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/tui"
)

// tuiClient is a2a.TaskHandler for the full-screen view: it draws what the run
// produces and puts the run's questions to the operator through the native widgets.
//
// It is the same client the line UI is, differing only in where the lines go and who
// answers. Everything about how a conversation looks belongs to blockRenderer, which
// both share with the stored-session viewer.
type tuiClient struct {
	live     *tui.Live
	renderer *blockRenderer

	// drawn records that the startup card has been dismissed. Dismissing is idempotent
	// but marshals onto the tview loop, so a run producing thousands of blocks would
	// queue thousands of updates that do nothing.
	drawn bool
}

// Block draws one event of the run.
//
// It runs on the goroutine reading the reply set, and Live.Append marshals onto the
// tview loop, so nothing here touches view state directly.
func (c *tuiClient) Block(block a2a.Block) {
	// The first block is the view's cue that there is something to draw.
	if !c.drawn {
		c.drawn = true
		c.live.HideSplash()
	}

	if status, ok := block.Content().(a2a.StatusBlock); ok {
		c.usage(status)
	}

	c.live.Append(c.renderer.Lines(block)...)
}

// usage keeps the live token counter in step with what the run reports.
//
// A replay seeds it, since a resumed conversation has spent everything it spent before
// this client arrived, and an iteration adds what that one model call cost. The turn's
// last call sends no status of its own, so the counter is set from the terminal
// message's totals when the turn ends rather than left short.
func (c *tuiClient) usage(b a2a.StatusBlock) {
	if b.Usage == nil {
		return
	}

	in, out, cacheRead, cacheCreate, thinking := liveUsage(b.Usage)

	if b.Phase == a2a.PhaseReplayEnd {
		c.live.SeedUsage(in, out, cacheRead, cacheCreate, thinking)

		return
	}

	c.live.AddUsage(in, out, cacheRead, cacheCreate, thinking)
}

// setUsage fixes the counter at a run's totals, which is what a terminal message
// carries and what the end-of-run summary reports.
func (c *tuiClient) setUsage(u *a2a.Usage) {
	if u == nil {
		return
	}

	c.live.SeedUsage(liveUsage(u))
}

// Question puts one of the run's questions to the operator through the full-screen
// widgets and answers it.
func (c *tuiClient) Question(ctx context.Context, ask *a2a.ElicitRequest) (*a2a.ElicitReply, error) {
	return answerQuestion(ctx, c.live.Prompter(), ask, nil)
}

// liveUsage splits what the wire reports into what the live counter tracks.
//
// a2a.Usage counts cache with the rest of the input, since a caller reading a bill
// wants the number it was billed for. The status bar shows the uncached remainder
// beside a cache figure of its own, so the two are separated here rather than
// double-counting every cached token.
func liveUsage(u *a2a.Usage) (in, out, cacheRead, cacheCreate, thinking int64) {
	in = u.InputTokens - u.CacheReadTokens - u.CacheCreateTokens
	if in < 0 {
		in = 0
	}

	return in, u.OutputTokens, u.CacheReadTokens, u.CacheCreateTokens, u.ThinkingTokens
}
