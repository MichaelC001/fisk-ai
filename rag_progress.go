//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/jedib0t/go-pretty/v6/progress"

	"github.com/choria-io/fisk-ai/internal/util"
)

// indexBar draws the embedding progress of a knowledge index run as a single
// tracker. Every method tolerates a nil receiver, so a caller holds one
// possibly-nil value instead of branching everywhere on whether a bar is drawn.
type indexBar struct {
	pw       progress.Writer
	tracker  *progress.Tracker
	rendered chan struct{}
	stopOnce sync.Once

	mu sync.Mutex
	// pending holds notes that arrived after the tracker completed, which the
	// renderer would drop, to be printed once the render loop has stopped.
	pending []string
}

// newIndexBar starts a bar counting up to total chunks, or returns nil when a bar
// would be wrong: nothing to embed, or stdout is not a terminal that can carry one.
// A nil bar leaves the command's output exactly as it was before.
func newIndexBar(total int) *indexBar {
	if total <= 0 || !util.StdoutIsTerminal() || os.Getenv("TERM") == "dumb" {
		return nil
	}

	style := progress.StyleDefault
	style.Visibility.ETA = true
	style.Options.DoneString = "done"
	style.Options.PercentFormat = "%3.0f%%"
	// These default to microseconds, which renders elapsed time as 1m12.345678s and
	// rewrites the line on every frame.
	style.Options.TimeInProgressPrecision = time.Second
	style.Options.TimeDonePrecision = time.Second

	pw := progress.NewWriter()
	// The writer only tracks the terminal width when it writes to os.Stdout, and
	// without a width a long line wraps while the renderer still counts it as one
	// row, leaving an orphaned line behind on every frame.
	pw.SetOutputWriter(os.Stdout)
	pw.SetStyle(style)
	pw.SetTrackerPosition(progress.PositionRight)
	pw.SetTrackerLength(20)
	pw.SetUpdateFrequency(250 * time.Millisecond)

	b := &indexBar{
		pw:       pw,
		rendered: make(chan struct{}),
		tracker: &progress.Tracker{
			Message: "embedding",
			Total:   int64(total),
			Units: progress.Units{
				Formatter:        func(v int64) string { return strconv.FormatInt(v, 10) },
				Notation:         "/" + strconv.Itoa(total),
				NotationPosition: progress.UnitsNotationPositionAfter,
			},
		},
	}
	pw.AppendTracker(b.tracker)

	fmt.Println()

	go func() {
		defer close(b.rendered)
		pw.Render()
	}()

	return b
}

// advance moves the bar on by the chunks a file actually embedded.
func (b *indexBar) advance(chunks int) {
	if b == nil || chunks <= 0 {
		return
	}

	b.tracker.Increment(int64(chunks))
}

// note prints a progress note without tearing the bar.
func (b *indexBar) note(msg string) {
	if b == nil {
		fmt.Println(msg)
		return
	}

	// A completed tracker leaves no active trackers, and the renderer returns on
	// that before it reaches its log queue, so a note handed over now would never be
	// printed. Hold it rather than writing into a live frame.
	if b.tracker.IsDone() {
		b.mu.Lock()
		b.pending = append(b.pending, msg)
		b.mu.Unlock()

		return
	}

	b.pw.Log("%s", msg)
}

// done marks the bar complete. An estimate that over-counted (a file present for
// the estimate and gone by the time the run reached it) otherwise leaves the last
// frame parked short of its total.
func (b *indexBar) done() {
	if b == nil {
		return
	}

	b.tracker.MarkAsDone()
}

// stop ends the render loop and waits for its final frame, which starts by erasing
// the bar's own lines: anything printed before that frame lands is wiped off the
// screen. Callers print only after this returns. It is safe to call more than once.
func (b *indexBar) stop() {
	if b == nil {
		return
	}

	b.stopOnce.Do(func() {
		// Stop does nothing if the render goroutine has not yet installed its cancel
		// func, and waiting on a loop that was never told to end would hang, so ask
		// repeatedly until it actually ends.
		deadline := time.After(5 * time.Second)
		for {
			b.pw.Stop()
			select {
			case <-b.rendered:
				b.flush()
				return
			case <-deadline:
				b.flush()
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	})
}

// flush prints the notes that arrived too late for the renderer to show.
func (b *indexBar) flush() {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, msg := range b.pending {
		fmt.Println(msg)
	}
	b.pending = nil
}
