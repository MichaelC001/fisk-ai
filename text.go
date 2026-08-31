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
