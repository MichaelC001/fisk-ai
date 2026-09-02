//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package wire

// MaxBlockText bounds the text of one block on a reply set.
//
// A tool's output is the one unbounded thing on this path, and a ReplyStream refuses a
// message over the size cap without advancing the sequence, so a block dropped for size
// would leave no gap for a caller to notice. Trimming keeps the block and says what
// happened to the rest. The journal on the serving worker holds all of it.
//
// It is a constant rather than a setting: it exists so a message fits, not so an
// operator can choose how much of a run a caller sees.
const MaxBlockText = 64 * 1024

// trimMarker closes a value that was cut, so a caller renders a truncation rather than
// an answer that stops mid-sentence.
const trimMarker = "\n[trimmed for the event stream; the full text is in this worker's run journal]"

// TrimBlockText cuts a value to what one block can carry. It cuts on a rune boundary,
// since half a rune reaches a caller as a replacement character in the middle of an
// answer.
//
// Both producers use it: the sink that renders a live run, and the adapter that renders
// a stored one, so a replayed block is bounded exactly as the block it replays was.
func TrimBlockText(s string) string {
	if len(s) <= MaxBlockText {
		return s
	}

	cut := MaxBlockText
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}

	return s[:cut] + trimMarker
}

// TrimmedBlockText is TrimBlockText for a producer that also fills in TextBlock.Trimmed
// or ThinkingBlock.Trimmed. It compares what came back with what went in, so the limit
// and the cutting rule stay above and no caller restates them.
func TrimmedBlockText(s string) (string, bool) {
	out := TrimBlockText(s)

	return out, out != s
}

// utf8Start reports whether b begins a rune rather than continuing one.
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
