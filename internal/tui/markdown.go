//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package tui

import (
	"os"
	"strings"

	"github.com/charmbracelet/glamour"
	"github.com/muesli/termenv"
	"golang.org/x/term"

	"github.com/choria-io/fisk-ai/internal/sanitize"
)

// RenderMarkdownTo formats markdown for display on out. When out is a terminal it
// is rendered with glamour using a style matched to the terminal background and
// word wrapped to the terminal width; off a terminal (piped or redirected) the raw
// markdown is returned unchanged so the result is free of ANSI escape codes. out's
// own terminal detection is used, so a replay written to stderr renders correctly.
// Rendering is also skipped when noColor is set (the --no-color flag or the
// standard NO_COLOR environment variable). Any rendering failure falls back to raw.
func RenderMarkdownTo(md string, out *os.File, noColor bool) string {
	// The model's terminal escapes are stripped before rendering, so an escape in its
	// prose cannot set a style that outlasts this message on the operator's terminal.
	// The full-screen UI strips them where it renders each entry; this covers the line
	// output.
	md = sanitize.ForDisplay(md)

	if noColor || os.Getenv("NO_COLOR") != "" {
		return md
	}

	fd := int(out.Fd())
	if !term.IsTerminal(fd) {
		return md
	}

	opts := []glamour.TermRendererOption{glamour.WithAutoStyle(), glamour.WithEmoji()}

	// Match the word wrap to the terminal width so the output uses the full
	// screen rather than glamour's fixed 80 column default; glamour fits its
	// margins within this budget, so the rendered lines stay inside the width.
	// Fall back to that default when the width cannot be determined.
	width, _, err := term.GetSize(fd)
	if err == nil && width > 0 {
		opts = append(opts, glamour.WithWordWrap(width))
	}

	r, err := glamour.NewTermRenderer(opts...)
	if err != nil {
		return md
	}

	rendered, err := r.Render(md)
	if err != nil {
		return md
	}

	// glamour adds its own surrounding blank lines; trim them so the caller's
	// single newline controls the trailing spacing.
	return strings.Trim(rendered, "\n")
}

// minRenderWidth is the floor for markdown rendering. Below it glamour's word
// wrap produces mangled output, so a very narrow viewport is rendered as if this
// wide and left to scroll horizontally rather than corrupt the text.
const minRenderWidth = 20

// RenderMarkdownWidth renders markdown to ANSI wrapped at width, for display in
// the full-screen UI. It inspects no terminal: it forces an explicit style and
// color profile so nothing queries the tty (which the UI owns while its screen is
// held), making it safe to call under the alt-screen. noColor (or the NO_COLOR
// environment variable) selects the plain "notty" style, which still formats and
// wraps but emits no color. Any failure falls back to the raw markdown so the
// content is never lost.
func RenderMarkdownWidth(md string, width int, noColor bool) string {
	if width < minRenderWidth {
		width = minRenderWidth
	}

	style := "dark"
	profile := termenv.TrueColor
	if noColor || os.Getenv("NO_COLOR") != "" {
		style = "notty"
		profile = termenv.Ascii
	}

	r, err := glamour.NewTermRenderer(
		glamour.WithStandardStyle(style),
		glamour.WithColorProfile(profile),
		glamour.WithWordWrap(width),
		glamour.WithEmoji(),
	)
	if err != nil {
		return md
	}

	out, err := r.Render(md)
	if err != nil {
		return md
	}

	// glamour surrounds its output with blank lines; trim them so the viewport
	// controls the spacing between entries.
	return strings.Trim(out, "\n")
}
