//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/tui"
	"github.com/choria-io/fisk-ai/internal/util"
)

// blockRenderer turns the blocks a run produces into the lines the full-screen view
// draws.
//
// It is the whole of what this program knows about showing a conversation, and every
// source of blocks goes through it: a run watched over the wire, and a stored one read
// back through the transcript adapter. That is the point of the shape. A call, a result
// and an answer read the same however they arrived, and there is one place to change
// how any of them looks.
//
// It is not safe for concurrent use. The client drives it from the goroutine reading
// the reply set, and the transcript viewer from the one building its lines.
type blockRenderer struct {
	// showThinking keeps the model's reasoning. The full-screen view folds thinking
	// rather than hiding it, so this is the operator's own choice about whether it is
	// there to unfold at all.
	showThinking bool

	// answer is the text of the last final block, kept so a caller can reprint the
	// answer once the alt-screen is gone.
	answer string
	// warnings collects the advisories in the order they arrived, for the same reprint.
	warnings []string
}

// Lines renders one block. An empty result means the block has nothing to show, which
// is a progress status, a suppressed thinking block, or a block this build cannot name.
func (r *blockRenderer) Lines(block a2a.Block) []tui.Line {
	switch b := block.Content().(type) {
	case a2a.TextBlock:
		return r.text(b)

	case a2a.ThinkingBlock:
		if !r.showThinking || b.Text == "" {
			return nil
		}

		return []tui.Line{{Kind: tui.LineThinking, Text: b.Text}}

	case a2a.PromptBlock:
		return []tui.Line{{Kind: tui.LinePrompt, Text: b.Text}}

	case a2a.ToolCallBlock:
		line := tui.CallLine(b.Name, b.Input)

		return []tui.Line{{Kind: tui.LineToolCall, Text: line, Short: line}}

	case a2a.AgentCallBlock:
		line := fmt.Sprintf("%s (remote %s)", util.SanitizeForTerminal(b.Name, 120), util.SanitizeForTerminal(b.Task, 120))

		return []tui.Line{{Kind: tui.LineToolCall, Text: line, Short: line}}

	case a2a.ToolResultBlock:
		return []tui.Line{toolResultLine(b.Output, b.IsError)}

	case a2a.WarningBlock:
		msg := blockWarningMessage(b)
		if msg == "" {
			return nil
		}

		r.warnings = append(r.warnings, msg)

		return []tui.Line{{Kind: tui.LineWarning, Text: msg}}

	case a2a.StatusBlock:
		return statusLines(b)
	}

	return nil
}

// text renders the model's prose. The answer is set apart, since the viewport has no
// separate channel for it, and its raw text is kept for the reprint.
func (r *blockRenderer) text(b a2a.TextBlock) []tui.Line {
	if b.Text == "" {
		return nil
	}

	if !b.Final {
		return []tui.Line{{Kind: tui.LineNarration, Text: b.Text}}
	}

	r.answer = b.Text

	return []tui.Line{
		{Kind: tui.LineMeta, Text: "--- answer ---"},
		{Kind: tui.LineNarration, Text: b.Text},
	}
}

// statusLines marks a replayed conversation, so what already happened reads as history
// rather than as a turn arriving now. The progress statuses are for a caller pacing
// itself and have nothing to show a person.
func statusLines(b a2a.StatusBlock) []tui.Line {
	switch b.Phase {
	case a2a.PhaseReplayStart:
		return []tui.Line{{Kind: tui.LineMeta, Text: "--- resuming ---"}}

	case a2a.PhaseReplayEnd:
		if b.Truncated {
			return []tui.Line{
				{Kind: tui.LineMeta, Text: fmt.Sprintf("(showing the last %d blocks; read the whole conversation with 'fisk session show --transcript')", b.Count)},
				{Kind: tui.LineMeta, Text: "--- continuing ---"},
			}
		}

		return []tui.Line{{Kind: tui.LineMeta, Text: "--- continuing ---"}}
	}

	return nil
}

// renderBlocks is every line a set of blocks produces, for a caller with the whole
// conversation in hand rather than one block at a time.
func renderBlocks(blocks []a2a.Block, showThinking bool) []tui.Line {
	r := &blockRenderer{showThinking: showThinking}

	var out []tui.Line
	for _, block := range blocks {
		out = append(out, r.Lines(block)...)
	}

	return out
}

// printTranscript writes rendered lines as text, for a terminal the full-screen viewer
// cannot take over.
//
// The prefixes are the viewer's, so a conversation read on a plain terminal and the same
// one read in the viewer are the same conversation with the same shape. Tool output is
// dropped rather than folded, a text dump having nothing to unfold it with.
func printTranscript(w io.Writer, lines []tui.Line, noColor, toolOutput bool) {
	for _, line := range lines {
		switch line.Kind {
		case tui.LinePrompt:
			fmt.Fprintf(w, "\n> %s\n", util.SanitizeForDisplay(line.Text))

		case tui.LineThinking:
			fmt.Fprintf(w, "\n[thinking]\n%s\n", util.SanitizeForDisplay(line.Text))

		case tui.LineToolCall:
			fmt.Fprintf(w, "-> %s\n", line.Text)

		case tui.LineToolResult, tui.LineToolError:
			if !toolOutput {
				continue
			}

			fmt.Fprintf(w, "<-\n%s\n", util.SanitizeForDisplay(line.Text))

		case tui.LineWarning:
			fmt.Fprintf(w, "warning: %s\n", util.SanitizeForTerminal(line.Text, 400))

		case tui.LineMeta:
			fmt.Fprintf(w, "%s\n", util.SanitizeForTerminal(line.Text, 400))

		default:
			// Sized against stdout, which is where a transcript is read, whatever writer
			// the caller is collecting it into.
			fmt.Fprintf(w, "\n%s\n", util.RenderMarkdownTo(line.Text, os.Stdout, noColor))
		}
	}
}

// toolResultLine builds the viewer line for a tool's output, shared by a live run
// and a replayed transcript so the two read the same. A failed tool becomes a
// LineToolError, which stays visible even when tool output is folded and carries
// an "(error)" marker so the failure reads on a monochrome terminal where the
// color is lost. A successful tool's body is unwrapped from its CommandResult
// envelope to the plain output an operator wants to read; a silent success is
// shown as "(no output)" so an executed tool always leaves a visible result
// rather than a blank.
func toolResultLine(output string, isError bool) tui.Line {
	if isError {
		if output == "" {
			return tui.Line{Kind: tui.LineToolError, Text: "(error)"}
		}

		return tui.Line{Kind: tui.LineToolError, Text: "(error) " + output}
	}

	if unwrapped, ok := commandResultOutput(output); ok {
		output = unwrapped
	}

	if output == "" {
		return tui.Line{Kind: tui.LineToolResult, Text: "(no output)"}
	}

	return tui.Line{Kind: tui.LineToolResult, Text: output}
}

// commandResultOutput unwraps a tool's JSON result envelope to the plain output
// worth showing. A local command tool returns its result as a util.CommandResult
// JSON body (command, exit code, combined output) and a remote tool returns the
// same shape carrying just the output, so the raw viewport line would otherwise
// bury the actual output in envelope noise. When the body is a CommandResult it is
// unwrapped to its Output, keeping an "(exit N)" marker when the command exited
// non-zero so a failure that still ran is not hidden. A body that is not a
// recognizable CommandResult (a builtin's plain text, some other tool's own JSON)
// is left unchanged, reported by the false return.
func commandResultOutput(output string) (string, bool) {
	trimmed := strings.TrimSpace(output)
	if !strings.HasPrefix(trimmed, "{") {
		return "", false
	}

	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.DisallowUnknownFields()

	var res toolkit.CommandResult

	err := dec.Decode(&res)
	if err != nil {
		return "", false
	}

	if res.ExitCode == 0 {
		return res.Output, true
	}
	if res.Output == "" {
		return fmt.Sprintf("(exit %d)", res.ExitCode), true
	}

	return fmt.Sprintf("(exit %d) %s", res.ExitCode, res.Output), true
}
