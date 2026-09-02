//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agent

import (
	"fmt"
	"reflect"
	"slices"
	"sync"

	"github.com/choria-io/fisk-ai/internal/mcpclient"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/toolkit/functool"
)

// warnQueue holds the advisories raised away from the run goroutine until the loop
// can report them. Every Events method is called from the run goroutine alone, which
// is what lets a sink hold state without locking, and an MCP server's tool list
// changes on that server's own schedule, so the two are joined here rather than by
// making every sink safe for concurrent use.
//
// A queue is per run, and the loop drains it where it takes the tools for a model
// call, so an advisory about the set reaches the operator with the call that carries
// it.
type warnQueue struct {
	mu       sync.Mutex
	warnings []Warning
}

// add queues one advisory. A nil queue drops it, so a run with nothing publishing to
// it needs no queue at all.
func (q *warnQueue) add(w Warning) {
	if q == nil {
		return
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	q.warnings = append(q.warnings, w)
}

// drain returns the queued advisories in the order they were raised and empties the
// queue.
func (q *warnQueue) drain() []Warning {
	if q == nil {
		return nil
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	out := q.warnings
	q.warnings = nil

	return out
}

// liveMCPTools rebuilds the tool set of a run that is already under way when one of
// its configured MCP servers reports that its tool list changed. It holds the parts of
// the set a server cannot change, so a rebuild replaces that server's tools and leaves
// every other server's and every local tool exactly as they were.
//
// The sessions are connected before any run exists and one set backs every run a
// fisk serve process hosts, so this is registered on them per run and dropped when
// the run ends. Each run rebuilds and publishes to its own source; the round trip to
// the server is made once however many runs are listening, since the sessions list
// the server and hand the same descriptors to all of them.
type liveMCPTools struct {
	src      *toolSource
	caller   mcpclient.Caller
	warnings *warnQueue

	// deferrable tools either side of the MCP ones, in the order newToolSet is given
	// them: the application's and the peers' before, the caller's custom tools after.
	before []toolkit.Tool
	after  []toolkit.Tool
	// builtins are never deferred and always follow the deferrable tools.
	builtins []toolkit.Tool
	// toolSearchAllowed is the run's resolved gate, which is a property of the
	// provider and the configuration and so cannot move with a server's tool list.
	toolSearchAllowed bool

	mu sync.Mutex
	// order is the configured server order, which the tools are assembled in so a
	// rebuilt set carries them where the run started with them.
	order []string
	// tools and skipped are what each server contributes now. skipped is kept so a
	// server that reports its list changed without anything moving is not reported as
	// a change.
	tools   map[string][]*functool.Tool
	skipped map[string][]mcpclient.SkippedTool
	// claimed is every model-facing name in use, this server's own included, so
	// removing its current names before naming its new ones leaves exactly the names a
	// new tool may not take.
	claimed map[string]bool
	// remote are the tools imported from a2a peers, whose names a run keeps out of the
	// claimed set and which the naming pass consults separately.
	remote map[string]*functool.Tool
}

// liveMCPSetup is the run's side of the rebuild: where the new set goes, what it is
// assembled from, and what it was at the start of the run.
type liveMCPSetup struct {
	// Source is where a rebuilt set is published.
	Source *toolSource
	// Caller reaches the servers, for the tools a rebuild builds to call through.
	Caller mcpclient.Caller
	// Warnings carries the advisory to the run goroutine.
	Warnings *warnQueue
	// Imports are the per-server outcomes the run started with, in configured order.
	Imports []mcpclient.ServerImport
	// Claimed is the run's whole name set, the names its MCP tools hold now included.
	// It is copied, so what a rebuild does to it does not reach the run.
	Claimed map[string]bool
	// Remote are the tools imported from a2a peers, whose names a run keeps out of
	// Claimed.
	Remote map[string]*functool.Tool
	// Before and After are the deferrable tools either side of the MCP ones.
	Before []toolkit.Tool
	After  []toolkit.Tool
	// Builtins are never deferred and always follow the deferrable tools.
	Builtins []toolkit.Tool
	// ToolSearchAllowed is the run's resolved tool-search gate.
	ToolSearchAllowed bool
}

// newLiveMCPTools prepares the rebuild for one run.
func newLiveMCPTools(setup liveMCPSetup) *liveMCPTools {
	l := &liveMCPTools{
		src:               setup.Source,
		caller:            setup.Caller,
		warnings:          setup.Warnings,
		before:            slices.Clone(setup.Before),
		after:             slices.Clone(setup.After),
		builtins:          slices.Clone(setup.Builtins),
		toolSearchAllowed: setup.ToolSearchAllowed,
		tools:             make(map[string][]*functool.Tool, len(setup.Imports)),
		skipped:           make(map[string][]mcpclient.SkippedTool, len(setup.Imports)),
		claimed:           make(map[string]bool, len(setup.Claimed)),
		remote:            setup.Remote,
	}

	for _, imp := range setup.Imports {
		l.order = append(l.order, imp.Server.Name)
		l.tools[imp.Server.Name] = imp.Tools
		l.skipped[imp.Server.Name] = imp.Skipped
	}

	for name := range setup.Claimed {
		l.claimed[name] = true
	}

	return l
}

// changed rebuilds one server's tools and publishes the run's new set.
//
// A server that dropped a tool the model has already been told about needs nothing
// beyond this: the definition is gone from the next call, and the tool batch answering
// the last one runs against the set it was dispatched with.
//
// The set is published before the advisory is queued, so an operator is never told
// about a tool the run does not offer yet.
func (l *liveMCPTools) changed(change mcpclient.ToolListChange) {
	l.mu.Lock()
	defer l.mu.Unlock()

	server := change.Server.Name

	if change.Err != nil {
		l.warnings.add(Warning{Kind: WarnMCPToolsChanged, Name: server, Err: change.Err})
		return
	}

	// The names this server holds now are the ones it is about to give up, so they are
	// dropped before its new tools are named: left in, every tool it still offers would
	// be reported as colliding with the tool it replaces.
	held := l.tools[server]
	for _, t := range held {
		delete(l.claimed, t.Name())
	}

	imported := mcpclient.ImportChanged(change, mcpclient.NewClaimedNames(l.claimed, l.remote), l.caller)
	for _, t := range imported.Tools {
		l.claimed[t.Name()] = true
	}

	moved := toolListMoves(held, l.skipped[server], imported)
	if len(moved) == 0 {
		return
	}

	l.tools[server] = imported.Tools
	l.skipped[server] = imported.Skipped

	l.src.publish(newToolSet(l.deferrable(), l.builtins, l.toolSearchAllowed))
	l.warnings.add(Warning{Kind: WarnMCPToolsChanged, Name: server, Params: moved})
}

// deferrable is the run's deferrable tools with every server's current tools in
// configured order, which is where the run started with them.
func (l *liveMCPTools) deferrable() []toolkit.Tool {
	count := len(l.before) + len(l.after)
	for _, server := range l.order {
		count += len(l.tools[server])
	}

	out := make([]toolkit.Tool, 0, count)
	out = append(out, l.before...)
	for _, server := range l.order {
		for _, t := range l.tools[server] {
			out = append(out, t)
		}
	}

	return append(out, l.after...)
}

// toolListMoves says what one server's re-listing changed, for an operator to read:
// the tools the model gains, the ones it loses, the ones whose definition the server
// rewrote, and the ones the server offers that this run cannot take, each with the
// reason.
//
// A tool that kept its name is compared on its Definition, which is what the model is
// told and what the run's fingerprint hashes. A server that rewrites a description or
// an input schema without adding or removing a name has changed every call from here
// on, and comparing names alone would discard the rebuild and leave the run on the
// definitions it started with.
//
// The skipped tools are compared too, so a server that adds a tool this run cannot
// take is reported rather than looking like a notification about nothing, and a
// server that keeps offering it is not reported again at every notification. Nothing
// having moved returns nothing, and the run publishes no set.
func toolListMoves(held []*functool.Tool, heldSkipped []mcpclient.SkippedTool, imported mcpclient.ServerImport) []string {
	before := make(map[string]*functool.Tool, len(held))
	for _, t := range held {
		before[t.Name()] = t
	}

	now := make(map[string]bool, len(imported.Tools))
	for _, t := range imported.Tools {
		now[t.Name()] = true
	}

	var moves []string
	for _, t := range imported.Tools {
		prior, kept := before[t.Name()]
		switch {
		case !kept:
			moves = append(moves, fmt.Sprintf("added %s", t.Name()))
		case !reflect.DeepEqual(prior.Definition(false), t.Definition(false)):
			moves = append(moves, fmt.Sprintf("redefined %s", t.Name()))
		}
	}
	for _, t := range held {
		if !now[t.Name()] {
			moves = append(moves, fmt.Sprintf("removed %s", t.Name()))
		}
	}

	was := make(map[string]string, len(heldSkipped))
	for _, s := range heldSkipped {
		was[s.Name] = s.Reason
	}
	for _, s := range imported.Skipped {
		if was[s.Name] == s.Reason {
			continue
		}
		moves = append(moves, fmt.Sprintf("skipped %s: %s", s.Name, s.Reason))
	}

	return moves
}
