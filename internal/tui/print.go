//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package tui

import (
	"os"
)

// RenderAnswer formats markdown for display on stdout. See RenderMarkdownTo for
// the rendering rules.
func RenderAnswer(md string, noColor bool) string {
	return RenderMarkdownTo(md, os.Stdout, noColor)
}
