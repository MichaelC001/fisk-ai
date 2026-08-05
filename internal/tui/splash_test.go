//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"unicode/utf8"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("splashInfoLines", func() {
	It("Should always show the version and model", func() {
		joined := strings.Join(splashInfoLines(Meta{Version: "1.2.3", Model: "claude-sonnet-5"}), "\n")

		Expect(joined).To(ContainSubstring("1.2.3"))
		Expect(joined).To(ContainSubstring("claude-sonnet-5"))
	})

	It("Should announce telemetry under the directory when the run exports", func() {
		lines := splashInfoLines(Meta{Version: "1.2.3", Model: "m", Dir: "/work", Telemetry: true})

		Expect(lines).To(HaveLen(4))
		Expect(lines[3]).To(ContainSubstring("telemetry"))
		Expect(lines[3]).To(ContainSubstring("OTEL Enabled"))
	})

	// The startup note that says this is printed before the UI takes the terminal, so
	// for the whole of a full-screen run it is covered and unread. This is where an
	// operator can actually see that their conversation is leaving the machine.
	It("Should distinguish exporting the conversation from exporting the structure", func() {
		lines := splashInfoLines(Meta{Version: "1.2.3", Model: "m", Telemetry: true, TelemetryContent: true})
		joined := strings.Join(lines, "\n")

		Expect(joined).To(ContainSubstring("OTEL Enabled + content"))

		// The plain export case must not claim it, or the marker means nothing.
		plain := strings.Join(splashInfoLines(Meta{Version: "1.2.3", Model: "m", Telemetry: true}), "\n")
		Expect(plain).To(ContainSubstring("OTEL Enabled"))
		Expect(plain).ToNot(ContainSubstring("content"))
	})

	// An agent that never configures telemetry is the common case, so a line reporting it
	// as off would be noise on every one of those runs.
	It("Should say nothing when the run does not export", func() {
		joined := strings.Join(splashInfoLines(Meta{Version: "1.2.3", Model: "m", Dir: "/work"}), "\n")

		Expect(joined).ToNot(ContainSubstring("telemetry"))
		Expect(joined).ToNot(ContainSubstring("OTEL"))
	})

	// Every value starts at the same offset, which is what makes the column read as one.
	// The padding is computed from the label, so this catches a label added later that is
	// too wide for the offset rather than leaving it to be noticed on screen.
	It("Should start every value at the same offset", func() {
		lines := splashInfoLines(Meta{Version: "1.2.3", Model: "m", Dir: "/work", Telemetry: true})

		for _, line := range lines {
			plain := stripSplashTags(line)
			label := strings.TrimRight(plain[:splashLabelWidth], " ")

			Expect(plain).To(HavePrefix(label))
			Expect(plain[len(label):splashLabelWidth]).To(MatchRegexp(`^ +$`), "no gap after %q", label)
			Expect(plain[splashLabelWidth:]).ToNot(HavePrefix(" "), "value is not at the shared offset: %q", plain)
		}
	})

	// The card is a fixed width that cannot be resized once drawn, so a line wider than
	// the body column is clipped. This bounds every line the card can produce, with the
	// value fields at their maximum, rather than only the short literal ones.
	It("Should keep every line inside the body column", func() {
		width := splashWidth - 2 - cardPadLeft - splashLogoWidth - cardGap

		lines := splashInfoLines(Meta{
			Version:   strings.Repeat("v", splashValueMax),
			Model:     strings.Repeat("m", splashValueMax),
			Dir:       "/" + strings.Repeat("d", splashValueMax),
			Telemetry: true,
		})

		for _, line := range lines {
			visible := utf8.RuneCountInString(stripSplashTags(line))
			Expect(visible).To(BeNumerically("<=", width), "line overflows the body column: %q", line)
		}
	})
})

// stripSplashTags removes the dynamic-color markup from a rendered info line so its
// visible width and column offsets can be measured. It handles only the fixed tags this
// card emits, which is all these specs feed it.
func stripSplashTags(s string) string {
	for _, tag := range []string{"[gray]", "[-]"} {
		s = strings.ReplaceAll(s, tag, "")
	}

	return s
}
