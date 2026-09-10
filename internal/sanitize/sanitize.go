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

// MapText applies f to every run of ordinary text in s and hands the terminal
// escape sequences through untouched.
//
// It exists for a caller that has to rewrite text a model supplied while the
// sequences that color it are still in the string. A CSI sequence is a bracket
// followed by digits, semicolons and a letter, which is the shape of the widget
// markup a UI neutralizes, so a rewrite applied to the whole string edits inside
// the sequences and corrupts them.
//
// f is called for the text between sequences, which is the empty string where two
// sequences sit side by side.
func MapText(s string, f func(string) string) string {
	spans := ansiSequence.FindAllStringIndex(s, -1)
	if len(spans) == 0 {
		return f(s)
	}

	var b strings.Builder
	last := 0
	for _, span := range spans {
		b.WriteString(f(s[last:span[0]]))
		b.WriteString(s[span[0]:span[1]])
		last = span[1]
	}
	b.WriteString(f(s[last:]))

	return b.String()
}

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
