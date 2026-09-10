//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"strings"

	"golang.org/x/term"
)

// stdinIsTerminal reports whether this process's stdin is an interactive terminal,
// the condition the confirm gate and the ask_human_* builtins need to reach a
// human. The packages behind them take the answer as a parameter, so a test drives
// those paths by passing false rather than by replacing this.
func stdinIsTerminal() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// stdoutIsTerminal reports whether stdout is an interactive terminal. The
// full-screen UI takes over the screen only when both this and stdinIsTerminal
// hold, so a piped or redirected stdout falls back to the line UI and stays clean.
func stdoutIsTerminal() bool { return term.IsTerminal(int(os.Stdout.Fd())) }

// stdoutWidth reports the terminal width of stdout, and 0 for a pipe, a redirect
// or a terminal that will not report its size. A caller fitting text to the screen
// has no screen to fit it to in those cases and leaves the text as it is.
func stdoutWidth() int {
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 {
		return 0
	}

	return w
}

// wrapText breaks the lines of s that are wider than width at a space and leaves
// every other line where the author put it, so blank lines, list markers and the
// author's own line breaks all survive. Rendering the text as markdown instead
// would join the lines of a paragraph and reflow them, which shows an operator a
// rendering of their prompt rather than their prompt.
//
// A word wider than width goes on a line of its own and is not split, leaving the
// terminal to break it. A width of zero or less returns s unchanged, since there
// is nothing to fit it to. A line that is broken is respaced: its leading
// indentation goes, and every run of spaces or tabs inside it becomes one space.
// Only a line already too wide for the terminal reaches that.
func wrapText(s string, width int) string {
	if width <= 0 {
		return s
	}

	var out []string
	for line := range strings.SplitSeq(s, "\n") {
		if len([]rune(line)) <= width {
			out = append(out, line)
			continue
		}

		var cur string
		for _, word := range strings.Fields(line) {
			switch {
			case cur == "":
				cur = word
			case len([]rune(cur))+1+len([]rune(word)) <= width:
				cur += " " + word
			default:
				out = append(out, cur)
				cur = word
			}
		}
		out = append(out, cur)
	}

	return strings.Join(out, "\n")
}

// truncateString shortens s to at most max characters, appending an ellipsis when
// anything was cut. It counts runes so multi-byte text is not split
// mid-character.
func truncateString(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}

	return string(r[:max]) + "..."
}

// truncateLine collapses s to a single line, folding every run of whitespace to
// one space, then truncates it with truncateString. It is used for one-line
// listings where a chatty multi-line value would otherwise wrap.
func truncateLine(s string, max int) string {
	return truncateString(strings.Join(strings.Fields(s), " "), max)
}
