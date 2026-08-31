//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// Package sanitize strips terminal escape sequences and control characters from
// model-influenced text before it is printed, and validates an operator-supplied
// base URL.
//
// It imports the standard library only, which is what lets every package that
// prints model output depend on it.
package sanitize

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// ansiSequence matches terminal escape sequences (CSI and OSC) plus any other
// two-byte escape, so a model-supplied string cannot carry control sequences that
// rewrite or spoof what the operator sees on their terminal.
var ansiSequence = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]|\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)|\x1b.")

// ForTerminal makes a model-influenced string safe to print to the operator's
// terminal: it removes terminal escape sequences and other control characters,
// collapses whitespace to single spaces on one line, and caps the length at
// maxRunes. Escapes are stripped before truncation, so a cut never leaves a
// dangling sequence behind.
func ForTerminal(s string, maxRunes int) string {
	s = ansiSequence.ReplaceAllString(s, "")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")

	if utf8.RuneCountInString(s) > maxRunes {
		s = string([]rune(s)[:maxRunes]) + "…"
	}

	return s
}

// ForDisplay makes model-influenced text safe to show in the full-screen UI. It
// strips terminal escape sequences and other control characters that could spoof
// the display, and keeps newlines and tabs so multi-line content holds its
// structure. The text passes through whole, at its own length and spacing,
// because the viewport wraps and scrolls. The UI layer neutralizes tview widget
// markup separately.
func ForDisplay(s string) string {
	s = ansiSequence.ReplaceAllString(s, "")

	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}
