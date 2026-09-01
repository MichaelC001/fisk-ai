//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package fisktool

import (
	"github.com/choria-io/fisk-ai/internal/sanitize"
)

// maxCommandLineRunes caps the length of a resolved command line shown to the
// operator for confirmation. It is longer than a one-line question because a real
// command with several flags is legitimately longer, and cutting it could hide the
// very arguments the operator is approving.
const maxCommandLineRunes = 2000

// SanitizeCommandLine makes a resolved command line safe to print to the
// operator's terminal. The command path is fixed, but its argument values come
// from the model, so the assembled line is untrusted display text: it is stripped
// of terminal escape sequences and control characters so a model-supplied value
// cannot rewrite or spoof what the operator sees, and capped at
// maxCommandLineRunes.
func SanitizeCommandLine(s string) string {
	return sanitize.ForTerminal(s, maxCommandLineRunes)
}

// ElideMiddle keeps this much of a value: the head and the tail, with the middle
// replaced by an ellipsis, so similar values (which often share a prefix, like two
// subjects or stream names) stay distinguishable while the result stays short.
const (
	elideHeadRunes = 10
	elideTailRunes = 6
	elideEllipsis  = "..."
)

// ElideMiddle shortens s to its head and tail joined by an ellipsis when it is
// longer than the head, tail and ellipsis together, counting runes so a multibyte
// character is never split. A short value is returned unchanged. TraceLineShort
// uses it on a tool call's argument values.
func ElideMiddle(s string) string {
	r := []rune(s)
	if len(r) <= elideHeadRunes+len(elideEllipsis)+elideTailRunes {
		return s
	}

	return string(r[:elideHeadRunes]) + elideEllipsis + string(r[len(r)-elideTailRunes:])
}
