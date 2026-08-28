//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package tui

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/choria-io/fisk-ai/internal/util"
)

// splashName is the Pages name the startup card occupies while a live run waits for
// its first response.
const splashName = "splash"

// splashLogo is the FISK wordmark shown on the popup cards. It is trusted, static text;
// the block glyphs are the same family of unicode the dividers already use.
var splashLogo = []string{
	"███████ ██ ███████ ██   ██",
	"██      ██ ██      ██  ██",
	"█████   ██ ███████ █████",
	"██      ██      ██ ██  ██",
	"██      ██ ███████ ██   ██",
}

// splashLogoWidth is the widest logo row in cells, used to pad the logo column so the
// body sits beside it rather than under it.
var splashLogoWidth = func() int {
	w := 0
	for _, l := range splashLogo {
		if n := utf8.RuneCountInString(l); n > w {
			w = n
		}
	}
	return w
}()

const (
	// splashAccent tints the border, logo and spinner; the labels stay dim and the values
	// keep the terminal default so the card is colored only subtly.
	splashAccent = "blue"

	// cardPadLeft is the space between the border and the content, so the logo is not
	// jammed against the frame. cardGap separates the logo column from the body beside it.
	cardPadLeft = 1
	cardGap     = 2

	// splashMinValue is the narrowest value column the card is built with, so a small
	// terminal still gets a row that says something rather than a label and an ellipsis.
	splashMinValue = 12

	// splashMaxScreenNum over splashMaxScreenDen is the share of the terminal the card may
	// take. The card is centered, so leaving a tenth of the screen either side of it keeps
	// it reading as a card over the transcript rather than as the whole screen.
	splashMaxScreenNum = 8
	splashMaxScreenDen = 10
)

// splashLabels is every label the info column can carry, and the source splashLabelWidth
// is derived from. A label added to splashInfoLines and not to this list gets no room for
// itself and pushes its value off the shared offset.
var splashLabels = []string{"version", "model", "dir", "telemetry"}

// splashChromeWidth is every cell of a card that is not a value: the border, the left pad,
// the logo column, the gap beside it, the label offset, and one cell of slack so no value
// ends flush against the frame. A card is this plus the width its widest value asks for.
var splashChromeWidth = 2 + cardPadLeft + splashLogoWidth + cardGap + splashLabelWidth + 1

// composeCard lays out a popup card as a single opaque TextView's markup: the FISK logo
// on the left, the body lines to its right (each block vertically centered against the
// other), and an optional footer beneath. logoCaption, when set, is placed centered on the
// line directly below the logo, within the logo column (used for the project URL under the
// help logo). Every line carries a left pad and the content is framed with a blank top and
// bottom row, so it sits off the border on all sides. A single TextView is used rather than
// nested Flexes because a Flex leaves its background unfilled (tview sets dontClear on it),
// which lets the transcript bleed through the gaps between widgets; a TextView clears its
// background and is fully opaque. It returns the markup and its line count, so the caller
// can size the overlay to fit.
func composeCard(logoCaption string, body []string, footer string) (string, int) {
	blankLogo := strings.Repeat(" ", splashLogoWidth)

	// The left column is the logo, with the optional caption centered directly beneath it.
	left := make([]string, 0, len(splashLogo)+1)
	for _, l := range splashLogo {
		l += strings.Repeat(" ", splashLogoWidth-utf8.RuneCountInString(l))
		left = append(left, "["+splashAccent+"]"+l+"[-]")
	}
	if logoCaption != "" {
		left = append(left, "["+splashAccent+"]"+centerText(logoCaption, splashLogoWidth)+"[-]")
	}

	rows := len(left)
	if len(body) > rows {
		rows = len(body)
	}
	leftTop := (rows - len(left)) / 2
	bodyTop := (rows - len(body)) / 2

	pad := strings.Repeat(" ", cardPadLeft)
	gap := strings.Repeat(" ", cardGap)

	lines := make([]string, 0, rows+4)
	lines = append(lines, "")
	for r := 0; r < rows; r++ {
		leftCell := blankLogo
		if r >= leftTop && r < leftTop+len(left) {
			leftCell = left[r-leftTop]
		}
		bodyCell := ""
		if r >= bodyTop && r < bodyTop+len(body) {
			bodyCell = body[r-bodyTop]
		}
		lines = append(lines, pad+leftCell+gap+bodyCell)
	}
	if footer != "" {
		lines = append(lines, "", pad+footer)
	}
	lines = append(lines, "")

	return strings.Join(lines, "\n"), len(lines)
}

// centerText centers s within width by padding both sides, so a short caption sits under
// the middle of the logo. It counts runes, not bytes, and returns s unchanged when it is
// already at least as wide as the column.
func centerText(s string, width int) string {
	n := utf8.RuneCountInString(s)
	if n >= width {
		return s
	}
	leftPad := (width - n) / 2

	return strings.Repeat(" ", leftPad) + s + strings.Repeat(" ", width-n-leftPad)
}

// coloredCard wraps composed card markup in a bordered, accented TextView. The single
// TextView keeps the card opaque; the border and title carry the accent to match the bars.
func coloredCard(text, title string) *tview.TextView {
	card := tview.NewTextView().SetDynamicColors(true)
	card.SetText(text)
	card.SetBorder(true).SetTitle(title).SetBorderColor(tcell.ColorBlue).SetTitleColor(tcell.ColorBlue)

	return card
}

// enableSplash builds the startup card and adds it as a visible page on top. It is
// called only for a live run (from newLive), so the static transcript viewer never
// shows a card. The card is removed the moment the first response is ready to draw, the
// run ends, or the operator presses a key. Version, model and dir are model- and
// operator-adjacent text, so they are sanitized and escaped like every other bar.
//
// The screen has not been initialized when newLive calls this, so the card is built at
// the narrowest width it is allowed and resizeSplash gives it the real one on the first
// draw, once the terminal width is known.
func (v *viewer) enableSplash(meta Meta) {
	v.splashMeta = meta
	v.splashValue = splashMinValue
	v.splashSpinner = spinnerFrames[0]
	v.splashBody = splashInfoLines(meta, v.splashValue)
	text, count := composeCard("", v.splashBody, splashCaptionText(spinnerFrames[0]))
	v.splashCard = coloredCard(text, " fisk-ai ")

	frame, column := resizableOverlay(v.splashCard, splashChromeWidth+v.splashValue, count+2)
	v.splashFrame = frame
	v.splashColumn = column

	v.pages.AddPage(splashName, frame, true, true)
}

// splashCardWidth is how wide the card is drawn on a terminal of the given width: what its
// widest value asks for, capped at a share of the screen so the card stays a card, floored
// at splashMinValue so a row still says something, and never wider than the screen itself.
// It is the whole card, border included.
func splashCardWidth(meta Meta, screen int) int {
	value := splashMinValue
	for _, s := range splashValues(meta) {
		if n := utf8.RuneCountInString(s); n > value {
			value = n
		}
	}

	w := splashChromeWidth + value
	if max := screen * splashMaxScreenNum / splashMaxScreenDen; w > max {
		w = max
	}
	if w < splashChromeWidth+splashMinValue {
		w = splashChromeWidth + splashMinValue
	}
	// A terminal too narrow for even the floor takes the whole of it. tview clips what does
	// not fit either way; a card at the screen width loses less of itself than one over it.
	if w > screen {
		w = screen
	}

	return w
}

// splashValues is every value the card can carry for this run, before any eliding, so the
// card can be sized against the longest of them.
func splashValues(meta Meta) []string {
	values := []string{meta.Version, meta.Model, meta.Dir}
	if s := splashTelemetryValue(meta); s != "" {
		values = append(values, s)
	}

	return values
}

// splashInfoLines is the right column of the startup card: the version, model, working
// directory and whether the run is being recorded, label-aligned so the values line up.
// valueMax is what the card's current width leaves a value. Values are sanitized and
// escaped, and every one of them is elided to valueMax, since a card sized to its content
// has nothing spare for a value that ignores the budget.
//
// The version is elided from the right and the model and directory from the left. A path's
// leaf directories are the informative part, and a model id that has to lose something
// loses its vendor prefix rather than the variant that says which model it is.
func splashInfoLines(meta Meta, valueMax int) []string {
	lines := []string{
		splashInfoLine("version", escapeSplash(elideRight(meta.Version, valueMax))),
	}
	// A run against an agent somebody else hosts may know no model: the model is the
	// worker's, and this terminal need not have been told one. A label with nothing after
	// it reads as a card that failed to fill in, so the row is left out instead.
	if meta.Model != "" {
		lines = append(lines, splashInfoLine("model", escapeSplash(elideLeft(meta.Model, valueMax))))
	}
	if meta.Dir != "" {
		lines = append(lines, splashInfoLine("dir", escapeSplash(elideLeft(meta.Dir, valueMax))))
	}
	if s := splashTelemetryValue(meta); s != "" {
		lines = append(lines, splashInfoLine("telemetry", elideRight(s, valueMax)))
	}

	return lines
}

// splashTelemetryValue is the telemetry row's text, or "" when there is nothing to say.
// Telemetry off with nothing unknown about it would be noise on every run of the many
// agents that never configure it, but an agent that did not answer is a different thing
// from one that answered no, and the distinction is the whole reason a person reads this
// line.
func splashTelemetryValue(meta Meta) string {
	switch {
	case meta.TelemetryContent == ContentExportUnknown:
		return "unknown, the agent did not answer"

	case meta.Telemetry && meta.TelemetryContent == ContentExported:
		return "exported, with this conversation"

	case meta.Telemetry:
		return "exported"

	default:
		return ""
	}
}

// splashLabelWidth is the cell offset every value in the info column starts at: the widest
// label in splashLabels plus a two cell gap. splashValueMax is what the body column has
// left after it, so widening a label narrows the values rather than overflowing the card.
var splashLabelWidth = func() int {
	w := 0
	for _, l := range splashLabels {
		if n := utf8.RuneCountInString(l); n > w {
			w = n
		}
	}

	return w + 2
}()

// splashInfoLine renders one label and value of the info column, padding the label to
// the shared value offset. The padding is computed rather than written out so adding a
// label cannot silently misalign the column, which counting spaces by hand invites.
func splashInfoLine(label string, value string) string {
	pad := splashLabelWidth - utf8.RuneCountInString(label)
	if pad < 1 {
		pad = 1
	}

	return fmt.Sprintf("[gray]%s[-]%s%s", label, strings.Repeat(" ", pad), value)
}

// splashCaptionText is the animated waiting line: an accented spinner glyph then the
// waiting words. It carries the run's liveness cue while the card covers the statusbar's
// own spinner.
func splashCaptionText(spin string) string {
	return fmt.Sprintf("[%s]%s[-]  waiting for first response", splashAccent, spin)
}

// escapeSplash neutralizes a value for display in a dynamic-colors TextView: terminal
// escapes are stripped and any literal "[" is escaped so a path or model id containing
// one cannot open a color tag.
func escapeSplash(s string) string {
	return tview.Escape(util.SanitizeForDisplay(s))
}

// elideLeft shortens s to at most max runes by dropping from the front and prefixing
// "...", so the tail survives; for a path that keeps the leaf directories, which are the
// informative part. It counts runes, not bytes, so it never splits a multibyte glyph.
func elideLeft(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 3 {
		return string(r[len(r)-max:])
	}

	return "..." + string(r[len(r)-(max-3):])
}

// elideRight shortens s to at most max runes by dropping from the end and appending "...",
// so the head survives; for a version that keeps the part that names the release. Like
// elideLeft it counts its own ellipsis inside max, so a caller with one budget can hand
// the same number to either and get a value that fits.
func elideRight(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 3 {
		return string(r[:max])
	}

	return string(r[:max-3]) + "..."
}

// resizeSplash redraws the card for a terminal of the given width, sizing it to its content
// up to the share of the screen it may take. It runs from the before-draw hook, which holds
// the application lock: ResizeItem only rewrites the frame's item list and SetText only the
// card's own content, so neither re-enters the application the way hiding a page would.
func (v *viewer) resizeSplash(screen int) {
	if v.splashCard == nil || v.splashDismissed {
		return
	}

	w := splashCardWidth(v.splashMeta, screen)
	// A terminal narrower than the chrome leaves the value column nothing; keep one cell so
	// the eliding helpers have a limit they can work with. The card is clipped at that size
	// whatever it is given.
	value := w - splashChromeWidth
	if value < 1 {
		value = 1
	}
	if value == v.splashValue {
		return
	}
	v.splashValue = value

	v.splashBody = splashInfoLines(v.splashMeta, value)
	text, count := composeCard("", v.splashBody, splashCaptionText(v.splashSpinner))
	v.splashCard.SetText(text)
	v.splashColumn.ResizeItem(v.splashCard, count+2, 0)
	v.splashFrame.ResizeItem(v.splashColumn, w, 0)
}

// hideSplash removes the startup card for good. It is idempotent and loop-only: the live
// view calls it directly from its loop closures (teardown, a prompt, a turn boundary)
// and through HideSplash's QueueUpdateDraw from the run goroutine. It leaves focus
// untouched so it does not fight a just-focused input row.
func (v *viewer) hideSplash() {
	if v.splashCard == nil || v.splashDismissed {
		return
	}
	v.splashDismissed = true
	v.pages.HidePage(splashName)
}

// splashActive reports whether the card is still up, so the spinner repaint only runs
// while it is showing.
func (v *viewer) splashActive() bool {
	return v.splashCard != nil && !v.splashDismissed
}

// setSplashSpinner recomposes the card with the current spinner glyph in its caption.
// Runs on the loop from the live view's status refresh. The glyph is kept so a resize
// recomposes on the frame the animation is actually on rather than restarting it.
func (v *viewer) setSplashSpinner(spin string) {
	if v.splashCard == nil {
		return
	}
	v.splashSpinner = spin
	text, _ := composeCard("", v.splashBody, splashCaptionText(spin))
	v.splashCard.SetText(text)
}
