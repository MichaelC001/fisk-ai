//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package tui

import (
	"os"

	"github.com/choria-io/fisk-ai/internal/util"
)

// RenderAnswer formats markdown for display on stdout. See util.RenderMarkdownTo for
// the rendering rules.
func RenderAnswer(md string, noColor bool) string {
	return util.RenderMarkdownTo(md, os.Stdout, noColor)
}
