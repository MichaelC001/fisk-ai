//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
	"time"

	"github.com/choria-io/ui/columns"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/remotetools"
)

// printRemoteToolStatus prints a per-host status block after the tool table so an
// operator can tell why a remote tool is or is not present: whether the host
// answered, how long it took, how many tools it advertised and how many were
// imported, and any ignored filters or skipped tools.
func printRemoteToolStatus(c *columns.Document, cfg *config.Config, imports []remotetools.HostImport) {
	if len(imports) == 0 {
		return
	}

	c.Blank()
	c.Heading("Remote tool hosts")

	for _, imp := range imports {
		if imp.Err != nil {
			c.Printf("  %s: UNAVAILABLE via context %q after %s: %v\n",
				imp.Host.Name, cfg.NatsContext, imp.RTT.Round(time.Millisecond), imp.Err)
			continue
		}

		c.Printf("  %s (%s): reachable in %s, advertised %d tool(s), imported %d as %q\n",
			imp.Host.Name, imp.Version, imp.RTT.Round(time.Millisecond), imp.Discovered, len(imp.Tools), imp.Host.EffectiveAlias())
		for _, note := range hostNotes(imp) {
			c.Printf("warning: %s\n", note)
		}
	}
}

// hostNotes returns the per-host advisories shared by run and info: an ignored
// tag-based include filter and any tools skipped during import. It returns the
// text rather than writing it so each surface renders it its own way, a columns
// document for info, stderr for the line UI and a warning line for the full-screen
// UI, while the wording stays in one place. The notes carry no severity prefix;
// the caller adds whatever its surface uses.
func hostNotes(imp remotetools.HostImport) []string {
	var notes []string

	for _, w := range agent.HostImportWarnings(imp) {
		msg := warningMessage(w)
		if msg != "" {
			notes = append(notes, msg)
		}
	}

	return notes
}
