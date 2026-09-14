//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/mcpclient"
	"github.com/choria-io/fisk-ai/internal/memory"
	"github.com/choria-io/fisk-ai/internal/rag"
	"github.com/choria-io/fisk-ai/internal/remotetools"
	"github.com/choria-io/fisk-ai/internal/telemetry"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/toolkit/builtin"
	"github.com/choria-io/fisk-ai/internal/toolkit/fisktool"
	"github.com/choria-io/fisk-ai/internal/toolkit/functool"
)

// The Source values of Assembled and Problem. A remote or MCP tool has its host or
// server alias instead, which is the prefix on its name.
const (
	// SourceApplication is the wrapped application's commands.
	SourceApplication = "application"
	// SourceHumanInTheLoop is the human_in_the_loop family of built-ins.
	SourceHumanInTheLoop = "human_in_the_loop"
	// SourceMemory is the memory family of built-ins.
	SourceMemory = "memory"
	// SourceKnowledge is the knowledge family of built-ins.
	SourceKnowledge = "knowledge"
	// SourceCustom is the caller's own tools.
	SourceCustom = "custom"
	// SourceRemoteTools is the remote_tools block.
	SourceRemoteTools = "remote_tools"
	// SourceMCPClients is the mcp_clients block.
	SourceMCPClients = "mcp_clients"
)

// Sources holds the stores, connections and tools the caller opened before calling
// Assemble.
type Sources struct {
	// Memory is the store the memory tools read and write. With no store the tools
	// are built and listed and a call to one fails, which suits fisk info and the
	// agent card, which only list; on a served surface a memory tool that is served
	// needs a store and Assemble returns an error without one.
	Memory memory.Store
	// RAG is the store the knowledge tools search, on the same terms as Memory.
	RAG *rag.Store
	// Remote is the client the remote_tools hosts are imported through. Nil on the Run
	// surface with hosts configured is a failed source unless Unbound reports it.
	Remote *a2a.Client
	// MCP are the sessions with the configured MCP servers. Each holds one or more
	// servers, and a configured server no entry holds is a failed source unless
	// Unbound reports it. A caller that wants one server's failure to leave the others
	// importable connects one Sessions per server.
	MCP []*mcpclient.Sessions
	// Custom are the caller's own tools, validated after every other source has
	// claimed its names.
	Custom []toolkit.Tool
	// Unbound are the sources the caller tried to connect and could not, each with the
	// error it got: a NATS context that did not dial as {Source: SourceRemoteTools,
	// Err: err}, a server that did not connect as {Source: SourceMCPClients, Alias:
	// name, Err: err}. Each becomes a Problem with that error, and under Strict
	// the first is the error Assemble returns. A served surface ignores it.
	Unbound []Problem
}

// Surface selects the sources Assemble consults and the filters it applies.
type Surface int

const (
	// SurfaceRun assembles the set an agent run offers the model, from every source.
	SurfaceRun Surface = iota
	// SurfaceMCP assembles the set fisk mcp serves: the application's commands
	// narrowed by expose.agent.tools, and the built-ins that declare MCP exposure and
	// that expose.agent.mcp.builtins lists. An imported tool is never re-served.
	SurfaceMCP
	// SurfaceA2A assembles the set the a2a endpoint serves: the application's commands
	// narrowed by expose.agent.tools, and the built-ins that declare a2a exposure.
	SurfaceA2A
)

// Policy selects whether a failed source stops Assemble or is recorded in
// Assembly.Problems.
type Policy int

const (
	// Strict returns the first failed source's error unchanged.
	Strict Policy = iota
	// Lenient records each failed host or server in Problems and returns the tools of
	// the sources that answered.
	Lenient
)

// Assembled is one tool with its kind and source.
type Assembled struct {
	Tool toolkit.Tool
	Kind toolkit.Kind
	// Source is SourceApplication, a built-in family, the host or server alias the
	// tool's name is prefixed with, or SourceCustom.
	Source string
}

// Problem records a source Assemble could not import.
type Problem struct {
	// Source is SourceRemoteTools or SourceMCPClients.
	Source string
	// Alias is the host or server name, and empty when the whole block failed.
	Alias string
	Err   error
}

// The Reason values of Withheld.
const (
	// WithheldAgentOnly is a built-in whose exposure declaration excludes the surface.
	WithheldAgentOnly = "it is reachable only in an agent run"
	// WithheldNotListed is an MCP-exposable built-in that expose.agent.mcp.builtins
	// does not list.
	WithheldNotListed = "it is not listed in expose.agent.mcp.builtins"
)

// Withheld is a built-in the configuration enables and the surface does not serve.
type Withheld struct {
	Tool   string
	Reason string
}

// Assembly is the result of Assemble.
type Assembly struct {
	// Tools holds every tool in dispatch order: commands, the built-in families, remote
	// imports, MCP imports, then the custom tools sorted by name.
	Tools []Assembled
	// Problems are the sources that failed under Lenient, plus every Unbound entry
	// under either policy.
	Problems []Problem
	// Withheld are the built-ins a served surface left out, with the reason for each.
	Withheld []Withheld
	// Remote is the outcome of importing each remote_tools host, in configured order.
	Remote []remotetools.HostImport
	// MCP is the outcome of importing each mcp_clients server, in configured order. A
	// server the caller reported in Unbound, or passed no session for, has a row with
	// that error.
	MCP []mcpclient.ServerImport

	// claimed holds every name in use except the remote tools', and remote holds the
	// remote tools by name. mcpclient.NewClaimedNames takes both, and the live
	// re-import deletes from claimed per change, so they stay separate.
	claimed map[string]bool
	remote  map[string]*functool.Tool
}

// Assemble builds an agent's tool set from its configuration and the sources the
// caller opened, for one surface under one policy.
//
// Assemble adds the application's commands, then the human-in-the-loop, memory and
// knowledge built-ins, then the remote imports, then the MCP imports, then the custom
// tools, and claims each name as it goes. A remote or MCP tool is prefixed with its
// alias as the importers do it; a custom tool is refused on a clash with any earlier
// kind.
//
// An application that cannot be introspected and a served built-in whose name a
// command took are errors under both policies, since they are configuration
// mistakes. On a served surface a built-in's name is claimed only when the tool is
// served, so a command named after a withheld built-in is served as it always was.
//
// Under Strict the returned error is the source's own, and the Assembly beside it
// holds what was assembled up to the failure, Remote and MCP included, so a caller
// reports the per-host and per-server outcomes before failing.
func Assemble(ctx context.Context, cfg *config.Config, src Sources, surface Surface, policy Policy) (*Assembly, error) {
	a := &Assembly{claimed: map[string]bool{}, remote: map[string]*functool.Tool{}}

	if surface == SurfaceRun {
		a.Problems = append(a.Problems, src.Unbound...)
		if policy == Strict && len(src.Unbound) > 0 {
			return a, src.Unbound[0].Err
		}
	}

	commands, err := loadCommands(ctx, cfg, surface)
	if err != nil {
		return a, err
	}
	for _, t := range commands {
		a.add(t, toolkit.KindApplication, SourceApplication)
	}

	err = a.addBuiltins(cfg, src, surface)
	if err != nil {
		return a, err
	}

	if surface != SurfaceRun {
		return a, nil
	}

	err = a.importRemote(ctx, cfg, src, policy)
	if err != nil {
		return a, err
	}

	err = a.importMCP(ctx, cfg, src, policy)
	if err != nil {
		return a, err
	}

	err = a.addCustom(src.Custom)
	if err != nil {
		return a, err
	}

	return a, nil
}

// loadCommands introspects the application for the surface: the configured
// include and exclude for a run, and expose.agent.tools on top of them for a served
// surface.
func loadCommands(ctx context.Context, cfg *config.Config, surface Surface) ([]*fisktool.CommandTool, error) {
	if surface == SurfaceRun {
		return fisktool.LoadTools(ctx, cfg)
	}

	return fisktool.ServedTools(ctx, cfg)
}

// add appends one tool and claims its name. A remote tool's name is not written to
// claimed; the remote map, which importRemote fills, answers for it.
func (a *Assembly) add(t toolkit.Tool, kind toolkit.Kind, source string) {
	a.Tools = append(a.Tools, Assembled{Tool: t, Kind: kind, Source: source})
	if kind != toolkit.KindRemote {
		a.claimed[t.Name()] = true
	}
}

// addBuiltins adds the three built-in families in order. On a served surface it
// adds only the tools the surface serves and records the rest in Withheld.
func (a *Assembly) addBuiltins(cfg *config.Config, src Sources, surface Surface) error {
	families := []struct {
		source string
		tools  []*functool.Tool
		opened bool
		field  string
	}{
		{SourceHumanInTheLoop, builtin.HITLTools(cfg), true, ""},
		{SourceMemory, builtin.MemoryTools(cfg, src.Memory), src.Memory != nil, "Sources.Memory"},
		{SourceKnowledge, builtin.RAGTools(cfg, src.RAG), src.RAG != nil, "Sources.RAG"},
	}

	for _, family := range families {
		for _, t := range family.tools {
			reason := withheldReason(t, cfg, surface)
			if reason != "" {
				a.Withheld = append(a.Withheld, Withheld{Tool: t.Name(), Reason: reason})
				continue
			}

			if surface != SurfaceRun && !family.opened {
				return fmt.Errorf("the built-in tool %q is served on this surface but no store was passed in %s; open the store and pass it", t.Name(), family.field)
			}

			if a.claimed[t.Name()] {
				return fmt.Errorf("%s adds a built-in tool %q but the application already exposes a tool with that name; exclude or rename it", family.source, t.Name())
			}

			a.add(t, toolkit.KindBuiltin, family.source)
		}
	}

	return nil
}

// withheldReason returns why a served surface leaves a built-in out, and "" for a
// tool the surface serves. A run serves every built-in.
func withheldReason(t *functool.Tool, cfg *config.Config, surface Surface) string {
	switch surface {
	case SurfaceMCP:
		if !t.MCPExposable() {
			return WithheldAgentOnly
		}
		if !slices.Contains(cfg.MCPBuiltins(), t.Name()) {
			return WithheldNotListed
		}
	case SurfaceA2A:
		if !t.A2AExposable() {
			return WithheldAgentOnly
		}
	}

	return ""
}

// importRemote imports the remote_tools hosts through the client. A nil client with
// hosts configured is a failed source unless Unbound already reports it.
func (a *Assembly) importRemote(ctx context.Context, cfg *config.Config, src Sources, policy Policy) error {
	if len(cfg.RemoteTools) == 0 {
		return nil
	}

	if src.Remote == nil {
		if a.hasProblem(SourceRemoteTools, "") {
			return nil
		}

		err := errors.New("remote_tools is configured but Sources.Remote is nil; connect an a2a client and pass it")
		if policy == Strict {
			return err
		}
		a.Problems = append(a.Problems, Problem{Source: SourceRemoteTools, Err: err})

		return nil
	}

	imports, byName, err := remotetools.ImportHosts(ctx, src.Remote, cfg, a.claimed)
	a.Remote = imports
	a.remote = byName
	if err != nil && policy == Strict {
		return err
	}

	for _, imp := range imports {
		if imp.Err != nil {
			a.Problems = append(a.Problems, Problem{Source: SourceRemoteTools, Alias: imp.Host.Name, Err: imp.Err})
			continue
		}

		for _, t := range imp.Tools {
			a.add(t, toolkit.KindRemote, imp.Host.EffectiveAlias())
		}
	}

	return nil
}

// importMCP imports the servers each Sessions holds, then writes a.MCP in configured
// order: the import outcome for a server a session held, the caller's error for one
// reported in Unbound, and a failed source for one neither covers.
func (a *Assembly) importMCP(ctx context.Context, cfg *config.Config, src Sources, policy Policy) error {
	if len(cfg.MCPClients) == 0 {
		return nil
	}

	imported := map[string]mcpclient.ServerImport{}
	for _, sessions := range src.MCP {
		if sessions == nil {
			continue
		}

		outcome, err := mcpclient.Import(ctx, sessions, mcpclient.NewClaimedNames(a.claimed, a.remote))
		for _, imp := range outcome.Servers {
			imported[imp.Server.Name] = imp
		}
		if err != nil && policy == Strict {
			a.MCP = a.mcpRows(cfg, src, imported)

			return err
		}

		for _, imp := range outcome.Servers {
			if imp.Err != nil {
				a.Problems = append(a.Problems, Problem{Source: SourceMCPClients, Alias: imp.Server.Name, Err: imp.Err})
				continue
			}

			for _, t := range imp.Tools {
				a.add(t, toolkit.KindMCP, imp.Server.EffectiveAlias())
			}
		}
	}

	for _, server := range cfg.MCPClients {
		_, held := imported[server.Name]
		if held || a.hasProblem(SourceMCPClients, server.Name) || a.hasProblem(SourceMCPClients, "") {
			continue
		}

		err := fmt.Errorf("no session was passed for mcp server %q; connect it and pass it in Sources.MCP, or report why it did not connect in Sources.Unbound", server.Name)
		if policy == Strict {
			a.MCP = a.mcpRows(cfg, src, imported)

			return err
		}
		a.Problems = append(a.Problems, Problem{Source: SourceMCPClients, Alias: server.Name, Err: err})
	}

	a.MCP = a.mcpRows(cfg, src, imported)

	return nil
}

// mcpRows returns one ServerImport per configured server, in configured order: the
// import outcome where a session held the server, and otherwise a row with the
// error of the Problem recorded for it, or for the whole block.
func (a *Assembly) mcpRows(cfg *config.Config, src Sources, imported map[string]mcpclient.ServerImport) []mcpclient.ServerImport {
	rows := make([]mcpclient.ServerImport, 0, len(cfg.MCPClients))
	for _, server := range cfg.MCPClients {
		imp, held := imported[server.Name]
		if held {
			rows = append(rows, imp)
			continue
		}

		row := mcpclient.ServerImport{Server: server}
		for _, p := range a.Problems {
			if p.Source == SourceMCPClients && (p.Alias == server.Name || p.Alias == "") {
				row.Err = p.Err
				break
			}
		}
		rows = append(rows, row)
	}

	return rows
}

// addCustom validates the caller's tools in the order given and adds them sorted by
// name, so the set a run fingerprints is the same whether the caller built the slice
// in a fixed order or by ranging a map.
func (a *Assembly) addCustom(custom []toolkit.Tool) error {
	byName := make(map[string]toolkit.Tool, len(custom))
	for i, t := range custom {
		if t == nil {
			return fmt.Errorf("custom tool at index %d is nil", i)
		}

		name := t.Name()
		if name == "" {
			return fmt.Errorf("custom tool at index %d has an empty name", i)
		}

		// The model addresses a tool by its Definition name but the runner dispatches on
		// Name(); a mismatch would advertise a tool the model could call but the runner
		// could not find.
		defName := t.Definition(false).Name
		if defName != name {
			return fmt.Errorf("custom tool %q reports Definition name %q; Name() and Definition().Name must match", name, defName)
		}

		// A custom tool may never shadow an existing one: shadowing a confirm-gated
		// command would strip its gate.
		switch {
		case byName[name] != nil:
			return fmt.Errorf("custom tool at index %d (%q) duplicates an earlier custom tool of the same name", i, name)
		case a.remote[name] != nil:
			return fmt.Errorf("custom tool at index %d (%q) collides with an existing remote tool of the same name; a custom tool may not shadow it", i, name)
		}
		switch a.kindOf(name) {
		case toolkit.KindApplication:
			return fmt.Errorf("custom tool at index %d (%q) collides with an existing application tool of the same name; a custom tool may not shadow it", i, name)
		case toolkit.KindBuiltin:
			return fmt.Errorf("custom tool at index %d (%q) collides with an existing built-in tool of the same name; a custom tool may not shadow it", i, name)
		case toolkit.KindMCP:
			return fmt.Errorf("custom tool at index %d (%q) collides with a tool of the same name imported from an mcp server; a custom tool may not shadow it", i, name)
		}

		// A custom tool runs in-process, so it may not claim a provider whose work
		// happens elsewhere: KindRemote is journaled and recomputed into the remote-call
		// counters on resume, and KindMCP owns its own bucket in the per-kind accounting.
		// The check is on the kind because the accounting reads the kind.
		d, ok := t.(toolkit.Describer)
		if ok {
			switch d.Describe(json.RawMessage("{}")).Kind {
			case toolkit.KindRemote:
				return fmt.Errorf("custom tool %q declares the remote kind; injected tools run in-process and may not be accounted as another agent's", name)
			case toolkit.KindMCP:
				return fmt.Errorf("custom tool %q declares the mcp kind; injected tools run in-process and may not be accounted as an MCP server's", name)
			}
		}

		byName[name] = t
	}

	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		a.add(byName[name], toolkit.KindCustom, SourceCustom)
	}

	return nil
}

// kindOf returns the kind of the assembled tool with this name, and KindUnknown when
// no tool holds it.
func (a *Assembly) kindOf(name string) toolkit.Kind {
	for _, t := range a.Tools {
		if t.Tool.Name() == name {
			return t.Kind
		}
	}

	return toolkit.KindUnknown
}

// hasProblem reports whether a Problem for the source and alias was recorded.
func (a *Assembly) hasProblem(source string, alias string) bool {
	for _, p := range a.Problems {
		if p.Source == source && p.Alias == alias {
			return true
		}
	}

	return false
}

// Len returns the number of tools.
func (a *Assembly) Len() int {
	return len(a.Tools)
}

// Builtins returns the never-deferred tools in family order: human-in-the-loop,
// memory, knowledge.
func (a *Assembly) Builtins() []toolkit.Tool {
	return a.ofKinds(toolkit.KindBuiltin)
}

// BeforeMCP returns the deferrable tools ahead of the MCP imports: the application's
// commands, then the remote imports.
func (a *Assembly) BeforeMCP() []toolkit.Tool {
	return a.ofKinds(toolkit.KindApplication, toolkit.KindRemote)
}

// MCPTools returns the MCP imports, in the order the servers were configured and,
// within a server, the order it advertised them.
func (a *Assembly) MCPTools() []toolkit.Tool {
	return a.ofKinds(toolkit.KindMCP)
}

// AfterMCP returns the deferrable tools after the MCP imports: the custom tools,
// sorted by name.
func (a *Assembly) AfterMCP() []toolkit.Tool {
	return a.ofKinds(toolkit.KindCustom)
}

// ofKinds returns the tools of the given kinds, in assembly order.
func (a *Assembly) ofKinds(kinds ...toolkit.Kind) []toolkit.Tool {
	var out []toolkit.Tool
	for _, t := range a.Tools {
		if slices.Contains(kinds, t.Kind) {
			out = append(out, t.Tool)
		}
	}

	return out
}

// Counts returns the per-kind tool counts. The caller sets Deferred, since only it
// knows whether the set went behind tool search.
func (a *Assembly) Counts() telemetry.ToolCounts {
	var c telemetry.ToolCounts
	for _, t := range a.Tools {
		switch t.Kind {
		case toolkit.KindApplication:
			c.Application++
		case toolkit.KindBuiltin:
			c.Builtin++
		case toolkit.KindRemote:
			c.Remote++
		case toolkit.KindMCP:
			c.MCP++
		case toolkit.KindCustom:
			c.Custom++
		}
	}

	return c
}

// Names returns the names of the tools of one kind, one source, or both, in
// assembly order. KindUnknown matches every kind and an empty source every source,
// so Names(toolkit.KindBuiltin, "") is every built-in and Names(toolkit.KindUnknown,
// SourceMemory) the memory tools.
func (a *Assembly) Names(kind toolkit.Kind, source string) []string {
	var out []string
	for _, t := range a.Tools {
		if kind != toolkit.KindUnknown && t.Kind != kind {
			continue
		}
		if source != "" && t.Source != source {
			continue
		}
		out = append(out, t.Tool.Name())
	}

	return out
}
