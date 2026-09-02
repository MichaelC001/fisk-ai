//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agent

import (
	"sync/atomic"

	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// toolSet is the tools a run offers the model at one moment: the definitions a
// request carries, the registry the runner dispatches a call through, and whether
// the request asks for the tool search tool. newToolSet derives all three from the
// same tools, so the model is never offered a tool the runner cannot dispatch and
// never denied one it can.
//
// A set does not change once it is built. A run holds one for a model call and for
// the whole tool_use batch that answers it, and a change arrives as a new set
// published to a toolSource.
type toolSet struct {
	defs   []llm.ToolDef
	tools  map[string]toolkit.Tool
	search bool
}

// newToolSet builds a set from the tools a run can offer the model.
//
// deferrable are the tools that may be hidden behind tool search: the application's
// commands, the tools imported from remote agents and MCP servers, and a caller's
// own. Their definitions are sent in the order given. builtins follow them and are
// never deferred. Each tool is registered under its Name, which is the name the
// model addresses it by and the name the runner dispatches on.
//
// toolSearchAllowed is the caller's resolved gate: the provider supports tool search
// and the operator has not turned it off. The threshold is applied to the whole set,
// deferrable and built-in together, and it is applied here rather than once at the
// start of a run, so a set that grows past the threshold starts deferring and one
// that shrinks back below it stops.
func newToolSet(deferrable []toolkit.Tool, builtins []toolkit.Tool, toolSearchAllowed bool) *toolSet {
	defs, search := BuildToolParams(deferrable, len(builtins), toolSearchAllowed)

	set := &toolSet{
		defs:   defs,
		tools:  make(map[string]toolkit.Tool, len(deferrable)+len(builtins)),
		search: search,
	}

	for _, t := range deferrable {
		set.tools[t.Name()] = t
	}
	for _, t := range builtins {
		set.defs = append(set.defs, t.Definition(false))
		set.tools[t.Name()] = t
	}

	return set
}

// tool looks up the tool a call names, reporting whether the set holds one.
func (s *toolSet) tool(name string) (toolkit.Tool, bool) {
	t, ok := s.tools[name]

	return t, ok
}

// toolSource is where the tools a run offers the model come from, and where a
// change to them lands. A run reads it before each model call and sends whatever it
// holds at that moment.
//
// One source can back many runs, which is what it is built for even though every run
// holds its own today: hosted runs under fisk serve share a configuration and an
// application and so resolve identical names, so a rebuild can be computed once and
// published once for all of them. Sharing one source across those runs is not wired
// up here. Whenever it is, the readers are run goroutines and the publisher is an MCP
// session's own goroutine, which is why a reader takes a whole set rather than a view
// that can change under it.
type toolSource struct {
	set atomic.Pointer[toolSet]
}

// newToolSource holds set until something publishes another. set must not be nil:
// unlike publish, which has an earlier set to keep when it refuses one, there is
// nothing here to fall back to, so a nil set would leave every run reading this
// source with no tools and would surface at the first model call rather than where
// the mistake was made.
func newToolSource(set *toolSet) *toolSource {
	src := &toolSource{}
	src.set.Store(set)

	return src
}

// snapshot is the set as it stands. What it returns does not change afterwards,
// whatever is published next, so a caller can hold it for as long as its work needs
// one set.
func (s *toolSource) snapshot() *toolSet {
	return s.set.Load()
}

// publish replaces the set every later snapshot returns. A run holding an earlier
// one keeps it until its next model call, so a set published while a tool batch runs
// reaches the model from the call after it rather than part way through the batch.
//
// A nil set is not published: it would leave every run reading this source with no
// tools at all.
func (s *toolSource) publish(set *toolSet) {
	if set == nil {
		return
	}

	s.set.Store(set)
}
