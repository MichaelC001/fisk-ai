//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"fmt"
	"io"
	"strings"
	"sync"
)

// errorBufferMaxMessages bounds how many distinct messages an ErrorBuffer holds.
//
// The bound is not defensive tidiness. An export error carries the collector's own
// response body and the request URL, so a collector that answers with a per-request
// identifier makes every message distinct and defeats the deduplication entirely. The
// buffer would then grow by one entry per failed flush for the length of the run,
// which for a long-running agent against a misbehaving collector is unbounded memory
// holding text nobody will read. Occurrences past the bound are still counted, so the
// total stays truthful even when the individual messages stop being kept.
const errorBufferMaxMessages = 32

// ErrorBuffer collects diagnostic messages during a run so they can be reported once
// the terminal is free again, counting repeats rather than repeating them.
//
// It exists because of when OpenTelemetry writes. The SDK hands its diagnostics to a
// process-global destination from its own goroutines, at whatever moment an export
// fails, and SetErrorHandler names the hazard that creates: a program that owns the
// terminal (a full-screen UI) or speaks a protocol on its output has nowhere safe for
// those writes to land. Under a full-screen UI the line corrupts the display and the
// next frame paints straight over it, so the single channel that exists to say "your
// export is broken" cannot be read at all. Pointing SetErrorHandler here and draining
// it when the terminal is restored is the answer, and it is the reason this type is in
// the leaf next to the function it is for rather than in a caller.
//
// Repeats are counted because they arrive in volume: an unreachable collector produces
// one per failed flush, roughly one every five seconds for the length of the run, and
// printing each would bury everything else under a wall of identical lines.
//
// The zero value is ready to use. It is safe for concurrent use, which is required of
// anything given to SetErrorHandler or WithExportErrorHandler. One Write is one
// message: both of the SDK's writers emit a whole line per call, and a caller that
// splits a line across two Writes gets two entries.
type ErrorBuffer struct {
	mu       sync.Mutex
	counts   map[string]int
	order    []string
	overflow int
}

// Write records one message, trimming its trailing newline. It never fails.
func (b *ErrorBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	msg := strings.TrimRight(string(p), "\n")
	if b.counts == nil {
		b.counts = map[string]int{}
	}

	_, seen := b.counts[msg]
	if !seen && len(b.order) >= errorBufferMaxMessages {
		b.overflow++
		return len(p), nil
	}
	if !seen {
		b.order = append(b.order, msg)
	}
	b.counts[msg]++

	return len(p), nil
}

// Count returns how many distinct messages are held.
//
// It counts messages rather than bytes, which is why it is not called Len: a caller
// meeting a Len on something that is also an io.Writer would reasonably read it as
// bytes.Buffer.Len does, and a buffer that collapses repeats cannot answer that.
// Messages dropped past the bound are not counted here; they are reported by WriteTo.
func (b *ErrorBuffer) Count() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	return len(b.order)
}

// WriteTo writes what was collected, first occurrence first, and empties the buffer.
//
// Emptying is what makes it an io.WriterTo rather than a renderer: the buffer is a
// stream that is read once, so a second call writes nothing, and a caller draining it
// at two points in a run gets each message at the point it had arrived by. A message
// seen more than once is written once with its count.
func (b *ErrorBuffer) WriteTo(w io.Writer) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	var written int64

	for _, msg := range b.order {
		var (
			n   int
			err error
		)

		if c := b.counts[msg]; c > 1 {
			n, err = fmt.Fprintf(w, "%s (x%d)\n", msg, c)
		} else {
			n, err = fmt.Fprintln(w, msg)
		}

		written += int64(n)
		if err != nil {
			return written, err
		}
	}

	if b.overflow > 0 {
		n, err := fmt.Fprintf(w, "(%d further messages not shown)\n", b.overflow)
		written += int64(n)
		if err != nil {
			return written, err
		}
	}

	b.counts = nil
	b.order = nil
	b.overflow = 0

	return written, nil
}
