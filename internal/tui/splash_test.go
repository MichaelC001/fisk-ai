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

// testValueMax is a value budget wide enough that the content specs below see their values
// whole; the width behavior has its own specs.
const testValueMax = 40

var _ = Describe("splashInfoLines", func() {
	It("Should always show the version and model", func() {
		joined := strings.Join(splashInfoLines(Meta{Version: "1.2.3", Model: "claude-sonnet-5"}, testValueMax), "\n")

		Expect(joined).To(ContainSubstring("1.2.3"))
		Expect(joined).To(ContainSubstring("claude-sonnet-5"))
	})

	It("Should announce telemetry under the directory when the run exports", func() {
		lines := splashInfoLines(Meta{Version: "1.2.3", Model: "m", Dir: "/work", Telemetry: true}, testValueMax)

		Expect(lines).To(HaveLen(4))
		Expect(lines[3]).To(ContainSubstring("telemetry"))
		Expect(lines[3]).To(ContainSubstring("exported"))
	})

	// The startup note that says this is printed before the UI takes the terminal, so
	// for the whole of a full-screen run it is covered and unread. This is where an
	// operator can actually see that their conversation is leaving the machine.
	It("Should distinguish exporting the conversation from exporting the structure", func() {
		lines := splashInfoLines(Meta{Version: "1.2.3", Model: "m", Telemetry: true, TelemetryContent: ContentExported}, testValueMax)
		joined := strings.Join(lines, "\n")

		Expect(joined).To(ContainSubstring("with this conversation"))

		// The plain export case must not claim it, or the marker means nothing.
		plain := strings.Join(splashInfoLines(Meta{Version: "1.2.3", Model: "m", Telemetry: true}, testValueMax), "\n")
		Expect(plain).To(ContainSubstring("exported"))
		Expect(plain).ToNot(ContainSubstring("conversation"))
	})

	// Not knowing is a real answer and must not look like no: the process that exports
	// is the one running the agent, so a terminal talking to a worker that did not
	// answer has been told nothing rather than told there is nothing.
	It("Should say so when the agent did not answer", func() {
		unknown := strings.Join(splashInfoLines(Meta{Version: "1.2.3", Model: "m", TelemetryContent: ContentExportUnknown}, testValueMax), "\n")
		Expect(unknown).To(ContainSubstring("unknown"))

		// An agent that answered and exports nothing says nothing, which is every run of
		// the many agents that never configure it.
		off := strings.Join(splashInfoLines(Meta{Version: "1.2.3", Model: "m"}, testValueMax), "\n")
		Expect(off).ToNot(ContainSubstring("telemetry"))
	})

	// An agent that never configures telemetry is the common case, so a line reporting it
	// as off would be noise on every one of those runs.
	It("Should say nothing when the run does not export", func() {
		joined := strings.Join(splashInfoLines(Meta{Version: "1.2.3", Model: "m", Dir: "/work"}, testValueMax), "\n")

		Expect(joined).ToNot(ContainSubstring("telemetry"))
		Expect(joined).ToNot(ContainSubstring("OTEL"))
	})

	// Every value starts at the same offset, which is what makes the column read as one.
	// The padding is computed from the label, so this catches a label added later that is
	// too wide for the offset rather than leaving it to be noticed on screen.
	It("Should start every value at the same offset", func() {
		lines := splashInfoLines(Meta{Version: "1.2.3", Model: "m", Dir: "/work", Telemetry: true}, testValueMax)

		for _, line := range lines {
			plain := stripSplashTags(line)
			label := strings.TrimRight(plain[:splashLabelWidth], " ")

			Expect(plain).To(HavePrefix(label))
			Expect(plain[len(label):splashLabelWidth]).To(MatchRegexp(`^ +$`), "no gap after %q", label)
			Expect(plain[splashLabelWidth:]).ToNot(HavePrefix(" "), "value is not at the shared offset: %q", plain)
		}
	})

	// A path keeps its leaf directories and a model id keeps the variant that says which
	// model it is, so both lose their front. A version keeps the part that names the
	// release, so it loses its tail.
	It("Should elide a value from the end that carries the least", func() {
		lines := splashInfoLines(Meta{
			Version: "2.1.0-rc1+build.20260828.1730",
			Model:   "~deepseek/deepseek-v4-flash-preview",
			Dir:     "/Users/rip/work/fisk-examples/ffd-pr-triage",
		}, 20)

		Expect(stripSplashTags(lines[0])).To(HaveSuffix("2.1.0-rc1+build.2..."))
		Expect(stripSplashTags(lines[1])).To(HaveSuffix("...-v4-flash-preview"))
		Expect(stripSplashTags(lines[2])).To(HaveSuffix("...les/ffd-pr-triage"))
	})
})

var _ = Describe("the startup card's width", func() {
	// The values a real run carries: long enough that a 78 column card had to cut the model
	// and short enough that a wide terminal can show every one of them whole.
	const (
		model = "~deepseek/deepseek-v4-flash-preview"
		dir   = "/Users/rip/work/fisk-examples/ffd-pr-triage"
	)

	// The card sizes itself to its content on the first draw and follows a resize after
	// that, so a line wider than the card no longer wraps and drags the border with it.
	// Every row the card can carry is measured, at every terminal width from the floor up,
	// against the card the same width computed. Driving enableSplash and resizeSplash rather
	// than composeCard directly puts the clamp and the value budget in the measurement.
	It("Should keep every line inside the card at any terminal width", func() {
		metas := []Meta{
			{Version: "devel", Model: model, Dir: dir, Telemetry: true},
			{Version: "2.1.0-rc1+build.20260828.1730", Model: model, Dir: dir, Telemetry: true, TelemetryContent: ContentExported},
			{Version: "devel", Model: model, Dir: dir, TelemetryContent: ContentExportUnknown},
			{Version: "devel", Model: strings.Repeat("m", 400), Dir: "/" + strings.Repeat("d", 400), Telemetry: true},
		}

		for _, meta := range metas {
			for _, screen := range []int{55, 60, 80, 100, 120, 200, 400} {
				v := newViewer(meta, nil, false, true)
				v.enableSplash(meta)
				v.resizeSplash(screen)

				w := splashCardWidth(meta, screen)
				Expect(w).To(BeNumerically("<=", screen), "card wider than a %d wide terminal", screen)

				for _, line := range strings.Split(v.splashCard.GetText(true), "\n") {
					visible := utf8.RuneCountInString(line)
					Expect(visible).To(BeNumerically("<=", w-2), "line overflows a %d wide card on a %d wide terminal: %q", w, screen, line)
				}
			}
		}
	})

	// The card is centered, so taking the whole terminal would stop it reading as a card
	// over the transcript.
	It("Should take no more than its share of the screen", func() {
		meta := Meta{Version: "devel", Model: strings.Repeat("m", 400), Dir: "/" + strings.Repeat("d", 400)}

		for _, screen := range []int{80, 120, 200, 400} {
			Expect(splashCardWidth(meta, screen)).To(Equal(screen * splashMaxScreenNum / splashMaxScreenDen))
		}
	})

	// A terminal with room to spare shows the values whole; the ellipsis on the old fixed
	// width card was a shortage of card, not of terminal.
	It("Should show a real run's values whole where the terminal allows", func() {
		meta := Meta{Version: "devel", Model: model, Dir: dir, Telemetry: true, TelemetryContent: ContentExported}

		v := newViewer(meta, nil, false, true)
		v.enableSplash(meta)
		v.resizeSplash(120)

		text := v.splashCard.GetText(true)
		Expect(text).To(ContainSubstring(model))
		Expect(text).To(ContainSubstring(dir))
		Expect(text).ToNot(ContainSubstring("..."))
	})

	// The floor keeps a value column that still says something on a small terminal, and
	// nothing below it may push the card off the screen.
	It("Should never grow past the terminal on a small one", func() {
		meta := Meta{Version: "devel", Model: model, Dir: dir, Telemetry: true}

		for _, screen := range []int{10, 20, 40, 54} {
			Expect(splashCardWidth(meta, screen)).To(Equal(screen))
		}
	})
})

// stripSplashTags removes the dynamic-color markup from a rendered card so its visible
// width and column offsets can be measured. It handles only the fixed tags this card
// emits, which is all these specs feed it.
func stripSplashTags(s string) string {
	for _, tag := range []string{"[gray]", "[" + splashAccent + "]", "[-]"} {
		s = strings.ReplaceAll(s, tag, "")
	}

	return s
}
