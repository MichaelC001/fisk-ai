//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"fmt"
	"strings"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// These messages arrive from the SDK's own goroutines while the run is in flight. Under
// a full-screen UI that is the worst possible time: the line corrupts the display and
// the next frame paints over it, so the one channel that says "your export is broken"
// cannot be read at all.
var _ = Describe("ErrorBuffer", func() {
	It("should count repeats rather than repeat them", func() {
		var buf ErrorBuffer

		// An unreachable collector produces one of these per failed flush, roughly one
		// every five seconds for the length of the run. Printing each would bury
		// everything else under a wall of identical lines.
		for range 14 {
			_, err := buf.Write([]byte("warning: telemetry export error: 401 Unauthorized\n"))
			Expect(err).ToNot(HaveOccurred())
		}

		out := &strings.Builder{}
		_, err := buf.WriteTo(out)
		Expect(err).ToNot(HaveOccurred())

		Expect(out.String()).To(Equal("warning: telemetry export error: 401 Unauthorized (x14)\n"))
	})

	It("should report distinct messages in the order they first appeared", func() {
		var buf ErrorBuffer

		for _, msg := range []string{"traces refused\n", "metrics refused\n", "traces refused\n"} {
			_, err := buf.Write([]byte(msg))
			Expect(err).ToNot(HaveOccurred())
		}

		out := &strings.Builder{}
		_, err := buf.WriteTo(out)
		Expect(err).ToNot(HaveOccurred())

		Expect(out.String()).To(Equal("traces refused (x2)\nmetrics refused\n"))
	})

	It("should write nothing when nothing was collected", func() {
		var buf ErrorBuffer

		out := &strings.Builder{}
		n, err := buf.WriteTo(out)
		Expect(err).ToNot(HaveOccurred())
		Expect(n).To(BeZero())
		Expect(out.String()).To(BeEmpty())
	})

	It("should report the byte count it wrote", func() {
		var buf ErrorBuffer

		_, err := buf.Write([]byte("refused\n"))
		Expect(err).ToNot(HaveOccurred())

		out := &strings.Builder{}
		n, err := buf.WriteTo(out)
		Expect(err).ToNot(HaveOccurred())
		Expect(n).To(Equal(int64(len(out.String()))))
	})

	// A stream is read once. A caller draining at two points in a run must see each
	// message at the point it had arrived by, not the whole run's messages twice.
	It("should empty itself so a second drain writes nothing", func() {
		var buf ErrorBuffer

		_, err := buf.Write([]byte("refused\n"))
		Expect(err).ToNot(HaveOccurred())

		first := &strings.Builder{}
		_, err = buf.WriteTo(first)
		Expect(err).ToNot(HaveOccurred())
		Expect(first.String()).To(Equal("refused\n"))

		second := &strings.Builder{}
		_, err = buf.WriteTo(second)
		Expect(err).ToNot(HaveOccurred())
		Expect(second.String()).To(BeEmpty())
		Expect(buf.Count()).To(BeZero())
	})

	It("should count distinct messages rather than occurrences or bytes", func() {
		var buf ErrorBuffer

		for _, msg := range []string{"a\n", "a\n", "b\n"} {
			_, err := buf.Write([]byte(msg))
			Expect(err).ToNot(HaveOccurred())
		}

		Expect(buf.Count()).To(Equal(2))
	})

	// An export error carries the collector's response body and the request URL, so a
	// collector answering with a per-request identifier makes every message distinct and
	// defeats the deduplication. Unbounded, that is one entry per failed flush for the
	// length of the run.
	It("should bound distinct messages and still account for what it dropped", func() {
		var buf ErrorBuffer

		for i := range errorBufferMaxMessages + 5 {
			_, err := buf.Write([]byte(fmt.Sprintf("refused: request %d\n", i)))
			Expect(err).ToNot(HaveOccurred())
		}

		Expect(buf.Count()).To(Equal(errorBufferMaxMessages))

		out := &strings.Builder{}
		_, err := buf.WriteTo(out)
		Expect(err).ToNot(HaveOccurred())

		Expect(out.String()).To(ContainSubstring("refused: request 0"))
		Expect(out.String()).ToNot(ContainSubstring(fmt.Sprintf("refused: request %d", errorBufferMaxMessages)))
		Expect(out.String()).To(ContainSubstring("(5 further messages not shown)"))
	})

	// A message already being counted must keep counting past the bound, or a collector
	// that emits one novel line early makes every later repeat of a real failure vanish.
	It("should keep counting a known message after the bound is reached", func() {
		var buf ErrorBuffer

		_, err := buf.Write([]byte("the real failure\n"))
		Expect(err).ToNot(HaveOccurred())

		for i := range errorBufferMaxMessages + 5 {
			_, err = buf.Write([]byte(fmt.Sprintf("noise %d\n", i)))
			Expect(err).ToNot(HaveOccurred())
			_, err = buf.Write([]byte("the real failure\n"))
			Expect(err).ToNot(HaveOccurred())
		}

		out := &strings.Builder{}
		_, err = buf.WriteTo(out)
		Expect(err).ToNot(HaveOccurred())

		Expect(out.String()).To(ContainSubstring(fmt.Sprintf("the real failure (x%d)", errorBufferMaxMessages+6)))
	})

	// SetErrorHandler and WithExportErrorHandler both write from the SDK's own
	// goroutines, so this is a requirement rather than a nicety. Run with -race.
	It("should be safe for concurrent use", func() {
		var buf ErrorBuffer
		var wg sync.WaitGroup

		for i := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 50 {
					_, err := buf.Write([]byte(fmt.Sprintf("writer %d\n", i)))
					Expect(err).ToNot(HaveOccurred())
				}
			}()
		}

		wg.Wait()

		Expect(buf.Count()).To(Equal(8))
	})
})
