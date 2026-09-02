//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/telemetry"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/toolkit/functool"
)

// toolNamePattern is the character set a final tool name must match to be usable
// as a model-facing name. It is the rule the a2a import and the MCP server apply,
// and a name outside it is skipped rather than advertised broken.
var toolNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// Caller reaches the live session of one configured server. It is the narrow
// surface an imported tool needs, kept as an interface so a tool depends on the
// session rather than on how it was connected and so a test can drive one with a
// fake. Sessions implements it.
//
// A handler is given a session for the duration of fn and keeps nothing from it: a
// session that has ended is replaced, so one held between calls is stranded by the
// replacement.
type Caller interface {
	// Use calls fn with the live session for the named server.
	Use(ctx context.Context, server string, fn func(session *mcp.ClientSession) error) error
}

// ClaimedNames are the model-facing tool names already in use when the tools of an
// MCP server are named. A final name that is claimed is skipped and reported as a
// collision: the model addresses every tool by one flat name and cannot express two
// tools answering to it.
//
// Build it with NewClaimedNames. An agent run keeps its claimed names in two
// places: the taken set the application tools, the built-ins and the custom tools
// write, and the name map the a2a import returns, which is never written into that
// set. Both are arguments to the constructor, and the fields are unexported, so a
// caller consults both or does not build the value at all. The zero value is
// refused by Import rather than read as "nothing is claimed".
type ClaimedNames struct {
	taken  map[string]bool
	remote map[string]*functool.Tool
	built  bool
}

// NewClaimedNames records the names already claimed when MCP tools are named.
// taken is the run's name set, written by the application tools, the built-ins and
// the caller's custom tools; remote is the name-keyed map the a2a import returns,
// whose names never reach taken. Either may be nil for a run with no such tools.
func NewClaimedNames(taken map[string]bool, remote map[string]*functool.Tool) ClaimedNames {
	return ClaimedNames{taken: taken, remote: remote, built: true}
}

// Claimed reports whether name is already in use by a tool from either lookup.
func (c ClaimedNames) Claimed(name string) bool {
	return c.taken[name] || c.remote[name] != nil
}

// errClaimedNamesUnbuilt refuses a ClaimedNames the caller did not build with
// NewClaimedNames. The zero value would read as nothing being claimed, so every
// entry point that names tools refuses it rather than naming a tool over whatever
// answers to that name now.
var errClaimedNamesUnbuilt = errors.New("the claimed tool names must be built with mcpclient.NewClaimedNames, which takes both of the lookups a run keeps them in")

// SkippedTool is one tool a server advertised that was not imported.
type SkippedTool struct {
	// Name is the tool's own name on the server, before the alias prefix.
	Name string
	// Reason says why it was not imported, for an operator to read.
	Reason string
}

// ServerImport is the outcome of importing one configured MCP server. Discovery
// and filtering fill Discovered and Kept; naming and building fill Tools and
// Skipped. A non-nil Err means the server's tools could not be listed, either
// because the entry's filters would not compile or because the listing itself
// failed, in which case Discovered, Kept, Tools and Skipped are empty.
//
// Whether a server that could not be listed is fatal, and how a skipped tool is
// rendered, belong to the caller, so an agent run and fisk info share one import.
type ServerImport struct {
	// Server is the configuration entry the tools came from.
	Server config.MCPServer
	// Err is the failure that stopped the import of this server, if any.
	Err error
	// RTT is how long listing the server's tools took. It is zero when the listing was
	// never attempted, as it is for an entry whose filters would not compile.
	RTT time.Duration
	// Discovered is how many tools the server advertised, before filtering.
	Discovered int
	// Kept are the descriptors that survived the entry's include and exclude
	// filters.
	Kept []*mcp.Tool
	// Tools are the built, named tools, in the order the server advertised them.
	Tools []*functool.Tool
	// Skipped are the kept descriptors that could not be built, each with a reason.
	Skipped []SkippedTool
}

// importServers discovers, filters, names and builds the tools of every connected
// server, in the order they were configured. A server that cannot be listed is
// recorded in its own outcome rather than stopping the others, so a caller decides
// whether to fail on it or report it. Listing a server is given that entry's
// startup timeout, so one that accepts the connection and then answers nothing
// fails its own outcome instead of holding up the servers after it.
//
// A name collision comes back as an error the caller may treat as fatal or render.
// The colliding tools appear in Skipped either way, so the outcomes are complete
// when it is returned. Claimed names that were not built with NewClaimedNames are
// refused before anything is listed.
//
// Every tool is named "<alias>_<tool>", so a name depends only on the server it
// came from and nothing is renamed when another server's tool list changes.
func importServers(ctx context.Context, sessions *Sessions, claimed ClaimedNames) ([]ServerImport, error) {
	if !claimed.built {
		return nil, errClaimedNamesUnbuilt
	}

	servers := sessions.configured()
	out := make([]ServerImport, 0, len(servers))
	byName := map[string]*functool.Tool{}
	var collisions []string

	for _, server := range servers {
		imported, clashes := sessions.importOne(ctx, server, claimed, byName)
		out = append(out, imported)
		collisions = append(collisions, clashes...)
	}

	if len(collisions) > 0 {
		return out, fmt.Errorf("imported mcp tool name collision: %s; set a distinct alias on the mcp_clients entry, or exclude the tool", strings.Join(collisions, ", "))
	}

	return out, nil
}

// Imported carries what a run gets from its MCP servers: the tools to offer the model,
// the map a call is dispatched through, and one outcome per server to report.
type Imported struct {
	// Tools are the built tools, in the order the servers were configured and, within
	// a server, the order it advertised them.
	Tools []*functool.Tool
	// ByName holds the same tools keyed by the model-facing name each answers to.
	ByName map[string]*functool.Tool
	// Servers is one outcome per configured server, carrying its round trip time and
	// the tools it advertised that were left out.
	Servers []ServerImport
}

// Import imports every connected server for an agent run. It is strict: a server
// that could not be listed and a name collision both fail the call, since the prompt
// may depend on tools that are not there. Servers is filled whether or not it
// returns an error, so a caller reports the round trips and the skipped tools that
// came back with the failure.
//
// The sessions stay owned by the caller, which must keep them open for the run and
// close them afterwards.
func Import(ctx context.Context, sessions *Sessions, claimed ClaimedNames) (Imported, error) {
	imports, err := importServers(ctx, sessions, claimed)

	for i := range imports {
		if imports[i].Err != nil {
			return Imported{Servers: imports}, fmt.Errorf("importing tools from mcp server %q: %w", imports[i].Server.Name, imports[i].Err)
		}
	}

	if err != nil {
		return Imported{Servers: imports}, err
	}

	out := Imported{ByName: map[string]*functool.Tool{}, Servers: imports}
	for i := range imports {
		for _, tool := range imports[i].Tools {
			out.Tools = append(out.Tools, tool)
			out.ByName[tool.Name()] = tool
		}
	}

	return out, nil
}

// Inspect connects every configured server and imports its tools for a command that
// reads a configuration rather than running it, such as fisk info. It is the lenient
// counterpart of Import and names every tool through the same pass, so the names it
// reports are the names a run would give the model. claimed carries both of the
// lookups a run keeps its claimed names in, for that reason.
//
// Nothing here is fatal. A server that cannot be started, reached or listed comes
// back as its own outcome carrying the error, and the servers configured after it
// are still connected: a configuration is inspected precisely when it may not be
// runnable, and an operator reaches for this to find out that a server is down. A
// name collision is recorded in the colliding tool's Skipped entry, as every other
// reason a tool was left out is, rather than returned: there is no run to refuse.
//
// Each server is connected on its own and its session is closed as soon as its
// tools are named, so this leaves no session open and no stdio child running. The
// tools it returns are therefore not callable: read their names, descriptions and
// schemas and call nothing.
//
// An empty server list connects to nothing, and claimed names that were not built
// with NewClaimedNames fail every server before anything is connected.
func Inspect(ctx context.Context, opts Options, claimed ClaimedNames) []ServerImport {
	if len(opts.Servers) == 0 {
		return nil
	}

	out := make([]ServerImport, 0, len(opts.Servers))

	if !claimed.built {
		for _, server := range opts.Servers {
			out = append(out, ServerImport{Server: server, Err: errClaimedNamesUnbuilt})
		}

		return out
	}

	byName := map[string]*functool.Tool{}

	for _, server := range opts.Servers {
		// One server per Connect, so the one that will not answer fails its own outcome
		// and the rest are still reached. Connect closes what it opened when it fails,
		// so nothing is left running by a server that did not come up.
		one := opts
		one.Servers = []config.MCPServer{server}

		sessions, err := Connect(ctx, one)
		if err != nil {
			out = append(out, ServerImport{Server: server, Err: err})
			continue
		}

		// byName is carried across the servers so the second server to offer a name is
		// told it is taken, which is what a run would tell it.
		imported, _ := sessions.importOne(ctx, server, claimed, byName)
		out = append(out, imported)

		// The close error describes tearing down a session whose tools have already been
		// read, which has nothing to say to someone reading a configuration. It waits
		// however long the child takes rather than under ctx, so a canceled inspection
		// still leaves no stdio child behind.
		_ = sessions.Close(context.WithoutCancel(ctx))
	}

	return out
}

// ToolListChange is one server's tool list as it stands after that server reported
// it changed, with the entry's include and exclude filters already applied. It is
// what Sessions.OnToolListChanged hands a caller, and ImportChanged names and builds
// tools from it.
//
// A non-nil Err means the server could not be re-listed, in which case Kept is empty
// and nothing about it is known; the caller keeps the tools it already had.
type ToolListChange struct {
	// Server is the configuration entry whose tools changed.
	Server config.MCPServer
	// Err is the failure that stopped the re-listing, if any.
	Err error
	// RTT is how long re-listing the server took.
	RTT time.Duration
	// Discovered is how many tools the server advertised, before filtering.
	Discovered int
	// Kept are the descriptors that survived the entry's include and exclude filters.
	Kept []*mcp.Tool
}

// ImportChanged names and builds the tools of a server that reported its tool list
// changed, for a caller replacing that one server's tools in a set it already holds.
// Every other server's tools and the caller's own are its own business and are not
// touched here.
//
// It differs from Import in what a name collision costs. Import returns an error,
// because at the start of a run nothing has happened yet and a run that cannot offer
// the tools its prompt names is better refused. Here the conversation is under way, and
// the collision is a third party's edit to its own tool list, so the colliding tool
// is left out and recorded in Skipped rather than ending the run. Every other reason
// a tool is skipped is recorded as it is at run start.
//
// claimed must hold every name in use except the ones this server's own tools hold
// now, which are about to be replaced; a caller passing those too would report every
// tool of this server as colliding with itself. Naming is always "<alias>_<tool>", so
// a tool arriving mid-conversation cannot take the name another tool answers to and
// nothing already offered to the model is renamed.
func ImportChanged(change ToolListChange, claimed ClaimedNames, caller Caller) ServerImport {
	if !claimed.built {
		return ServerImport{Server: change.Server, Err: errClaimedNamesUnbuilt}
	}
	if change.Err != nil {
		return ServerImport{Server: change.Server, Err: change.Err, RTT: change.RTT}
	}

	result := ServerImport{
		Server:     change.Server,
		RTT:        change.RTT,
		Discovered: change.Discovered,
		Kept:       change.Kept,
	}
	result.Tools, result.Skipped, _ = buildServerTools(change.Server, change.Kept, claimed, map[string]*functool.Tool{}, caller)

	return result
}

// importOne imports one server under a span of its own, so a listing that took ten
// seconds is attributed to the server that took them. It is where every import a run
// or fisk info makes goes through; the rebuild after a tools/list_changed
// notification does not, and stays untraced.
func (s *Sessions) importOne(ctx context.Context, server config.MCPServer, claimed ClaimedNames, byName map[string]*functool.Tool) (ServerImport, []string) {
	ctx, span := telemetry.ProviderFromContext(ctx).StartMCPImport(ctx, s.spanInfo(server))

	imported, clashes := importServer(ctx, s, server, claimed, byName)
	if imported.Err != nil {
		span.Finish(telemetry.MCPServerOutcome{Failed: true, Class: listClass(imported.Err)})
		return imported, clashes
	}

	span.Finish(telemetry.MCPServerOutcome{Tools: &telemetry.MCPToolCounts{
		Discovered: imported.Discovered,
		Kept:       len(imported.Kept),
		Skipped:    len(imported.Skipped),
	}})

	return imported, clashes
}

// importServer discovers one server, filters it, and names and builds what
// survives. byName holds the names the servers before it settled on, so a second
// server cannot claim one of them, and it gains the names this one settles on. The
// second return value holds the collisions found here, so the caller reports every
// server's together.
//
// The entry's filters are compiled before the server is listed, so an operator's
// typo fails without a round trip and leaves the outcome empty apart from its
// error.
func importServer(ctx context.Context, caller Caller, server config.MCPServer, claimed ClaimedNames, byName map[string]*functool.Tool) (ServerImport, []string) {
	result := ServerImport{Server: server}

	result.Kept, result.Discovered, result.RTT, result.Err = listServer(ctx, caller, server)
	if result.Err != nil {
		return ServerImport{Server: server, Err: result.Err, RTT: result.RTT}, nil
	}

	var collisions []string
	result.Tools, result.Skipped, collisions = buildServerTools(server, result.Kept, claimed, byName, caller)

	return result, collisions
}

// listServer lists one server's tools and applies the entry's filters, reporting
// what survived, how many the server advertised and how long it took to answer.
//
// The filters are compiled before the server is listed, so an operator's typo fails
// without a round trip and with no round trip time to report.
func listServer(ctx context.Context, caller Caller, server config.MCPServer) ([]*mcp.Tool, int, time.Duration, error) {
	filters, err := compileFilters(server)
	if err != nil {
		return nil, 0, 0, err
	}

	start := time.Now()
	discovered, err := discover(ctx, caller, server)
	rtt := time.Since(start)
	if err != nil {
		return nil, 0, rtt, err
	}

	return filterTools(discovered, filters), len(discovered), rtt, nil
}

// buildServerTools names and builds one server's kept descriptors, reporting the
// tools, the descriptors it could not build with the reason for each, and the names
// that collided. byName holds the names other servers settled on and gains the ones
// settled on here.
func buildServerTools(server config.MCPServer, kept []*mcp.Tool, claimed ClaimedNames, byName map[string]*functool.Tool, caller Caller) ([]*functool.Tool, []SkippedTool, []string) {
	var tools []*functool.Tool
	var skipped []SkippedTool
	var collisions []string

	for _, desc := range kept {
		name := fmt.Sprintf("%s_%s", server.EffectiveAlias(), desc.Name)

		if !toolNamePattern.MatchString(name) {
			skipped = append(skipped, SkippedTool{Name: desc.Name, Reason: fmt.Sprintf("it would be named %q, which is not a usable tool name", name)})
			continue
		}

		if claimed.Claimed(name) || byName[name] != nil {
			skipped = append(skipped, SkippedTool{Name: desc.Name, Reason: fmt.Sprintf("the name %q is already taken by another tool", name)})
			collisions = append(collisions, fmt.Sprintf("%q (mcp server %q)", name, server.Name))
			continue
		}

		tool, err := newTool(name, server.Name, desc, caller)
		if err != nil {
			skipped = append(skipped, SkippedTool{Name: desc.Name, Reason: err.Error()})
			continue
		}

		byName[name] = tool
		tools = append(tools, tool)
	}

	return tools, skipped, collisions
}

// discover lists every tool a server offers, within the time the entry allows for
// its startup. It iterates rather than calling ListTools once, so a server that
// answers in pages is fully enumerated, and the deadline covers the whole chain of
// pages: a server that never answers, or that answers with a cursor chain that
// never ends, fails this server's import instead of holding up every server after
// it.
//
// A failure is redacted on the url's structure alone, since a Caller is reached by
// name and its credentials belong to whoever opened the session: Sessions.Use has
// already replaced them in what it returns.
func discover(ctx context.Context, caller Caller, server config.MCPServer) ([]*mcp.Tool, error) {
	timeout := server.StartupTimeout()

	listCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var tools []*mcp.Tool

	err := caller.Use(listCtx, server.Name, func(session *mcp.ClientSession) error {
		for tool, err := range session.Tools(listCtx, nil) {
			if err != nil {
				return err
			}
			tools = append(tools, tool)
		}

		return nil
	})
	if err != nil {
		// The caller's own context expiring is reported as it arrived: the entry's
		// timeout had nothing to do with it.
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return nil, redacted(fmt.Errorf("listing the tools of mcp server %q: it did not answer within %v: %w", server.Name, timeout, err), nil)
		}

		return nil, redacted(fmt.Errorf("listing the tools of mcp server %q: %w", server.Name, err), nil)
	}

	return tools, nil
}

// toolFilters are one entry's compiled include and exclude patterns.
type toolFilters struct {
	include []*regexp.Regexp
	exclude []*regexp.Regexp
}

// compileFilters compiles an entry's include and exclude patterns. Configuration
// parsing accepts the patterns as written, so a typo an operator made is found
// here.
func compileFilters(server config.MCPServer) (toolFilters, error) {
	var filters toolFilters
	var err error

	if server.Include != nil {
		filters.include, err = compilePatterns(server.Include.Tools)
		if err != nil {
			return toolFilters{}, err
		}
	}

	if server.Exclude != nil {
		filters.exclude, err = compilePatterns(server.Exclude.Tools)
		if err != nil {
			return toolFilters{}, err
		}
	}

	return filters, nil
}

// filterError marks a pattern the operator wrote that would not compile. It is the
// configuration failing rather than the server, and it is the one import failure that
// never made a round trip, which is what a span needs to classify it as a
// configuration error instead of an unreachable server. It wraps rather than
// replaces, so the message is unchanged and errors.Is still reaches what regexp
// reported.
type filterError struct{ err error }

func (e filterError) Error() string { return e.err.Error() }
func (e filterError) Unwrap() error { return e.err }

// compilePatterns compiles one filter's name patterns.
func compilePatterns(patterns []string) ([]*regexp.Regexp, error) {
	out := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, filterError{fmt.Errorf("invalid mcp tool filter pattern %q: %w", p, err)}
		}
		out = append(out, re)
	}

	return out, nil
}

// filterTools applies an entry's compiled filters, matching on the tool name.
// Include restricts to matching names, exclude removes matching names, and include
// runs first. Tags are not consulted: MCP tools carry none, and an entry that
// filters on them is rejected when the configuration is parsed.
func filterTools(tools []*mcp.Tool, filters toolFilters) []*mcp.Tool {
	kept := tools

	if len(filters.include) > 0 {
		kept = matchTools(kept, filters.include)
	}

	if len(filters.exclude) > 0 {
		kept = subtractTools(kept, matchTools(kept, filters.exclude))
	}

	return kept
}

// matchTools returns the tools whose name matches any of the patterns.
func matchTools(tools []*mcp.Tool, patterns []*regexp.Regexp) []*mcp.Tool {
	var out []*mcp.Tool
	for _, t := range tools {
		for _, re := range patterns {
			if re.MatchString(t.Name) {
				out = append(out, t)
				break
			}
		}
	}

	return out
}

// subtractTools returns the tools that are not in remove, compared by name.
func subtractTools(tools, remove []*mcp.Tool) []*mcp.Tool {
	removed := make(map[string]bool, len(remove))
	for _, t := range remove {
		removed[t.Name] = true
	}

	var out []*mcp.Tool
	for _, t := range tools {
		if !removed[t.Name] {
			out = append(out, t)
		}
	}

	return out
}

// newTool builds a function tool from a descriptor an MCP server advertised.
// localName is the alias-prefixed name the model is given; the descriptor's own
// name is what the server is called with.
//
// The descriptor comes from a third party and is checked before it reaches the
// model API. A descriptor with no name is an error and the tool is not built,
// since a call to it would go out naming no tool. So is a schema that is absent or
// not a JSON object: from a client, the SDK holds the server's schema as the
// default unmarshaling of its JSON, so this is a type assertion rather than the
// decode an a2a descriptor's raw bytes need. So is a schema whose root declares a
// type other than "object", which the model API refuses, and every tool definition
// travels in one model request, so one descriptor it refuses fails every call in
// the run rather than only the calls to this tool. Nothing below the root is
// checked. A descriptor with no description is refused for the same reason a2a
// refuses one: it gives the model nothing to decide on.
//
// The handler calls the tool through caller for each call rather than holding a
// session, so a call after a reconnect reaches the server it is connected to now. A
// result is mapped for the model the way a2a maps a peer's reply: structured
// content is returned as the JSON the caller asked for, text blocks are
// concatenated, a block that is not text becomes one line naming what arrived, and
// a result flagged as an error becomes an error the model reasons about. Nothing is
// wrapped in a command envelope, since no command ran and there is no exit code to
// report.
//
// The annotations the server declares are carried over as the tool's behavior,
// resolved first so a server that contradicts itself cannot make its own tool
// vanish from the import. It stays the server's claim about the server's tool, and
// it is read back through functool.Tool.Behavior alone. The model is not told it:
// ModelDescription leaves an imported tool's description as the server wrote it,
// which already says whatever the server says about the tool, and no client or peer
// is told it either, since an MCP tool can never itself be served on (see
// functool.New).
func newTool(localName string, server string, desc *mcp.Tool, caller Caller) (*functool.Tool, error) {
	if desc.Name == "" {
		return nil, fmt.Errorf("mcp server %q advertises a tool with no name", server)
	}

	schema, ok := desc.InputSchema.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("tool %q from mcp server %q advertises no usable input schema; it must be a JSON object", desc.Name, server)
	}

	declaredType, declared := schema["type"]
	if declared && declaredType != "object" {
		return nil, fmt.Errorf("tool %q from mcp server %q advertises an input schema of type %v; a tool's arguments must be an object", desc.Name, server, declaredType)
	}

	if desc.Description == "" {
		return nil, fmt.Errorf("tool %q from mcp server %q advertises no description", desc.Name, server)
	}

	remoteName := desc.Name
	handler := func(ctx context.Context, input json.RawMessage, _ *functool.CallContext) (string, error) {
		// The model's arguments are forwarded as they arrived rather than decoded and
		// re-encoded, so a number the server cares about is not rewritten on the way.
		// An empty or null input is sent as the empty object the SDK substitutes for a
		// nil Arguments.
		var args any
		if len(input) > 0 && string(input) != "null" {
			args = input
		}

		var res *mcp.CallToolResult
		err := caller.Use(ctx, server, func(session *mcp.ClientSession) error {
			called, err := session.CallTool(ctx, &mcp.CallToolParams{Name: remoteName, Arguments: args})
			if err != nil {
				return err
			}
			res = called

			return nil
		})
		if err != nil {
			return "", redacted(fmt.Errorf("calling tool %q on mcp server %q: %w", remoteName, server, err), nil)
		}

		out, err := toolOutput(res, remoteName, server)
		if err != nil {
			return "", err
		}

		// The server's own sentence is what the model reads, as it does for an a2a
		// reply that ends in an error, rather than a rewrite of it.
		if res.IsError {
			if out == "" {
				return "", fmt.Errorf("tool %q on mcp server %q reported an error and said nothing about it", remoteName, server)
			}

			return "", errors.New(out)
		}

		return out, nil
	}

	return functool.New(functool.Spec{
		Name:        localName,
		Description: desc.Description,
		Schema:      schema,
		Handler:     handler,
		MCP:         &functool.MCPSpec{Server: server},
		Behavior:    behaviorFromAnnotations(desc.Annotations).Resolve(),
	})
}

// behaviorFromAnnotations maps the annotations an MCP server declares for a tool
// onto the neutral behavior every tool kind carries. Nil annotations declare
// nothing, which is the answer a server that said nothing should get.
//
// The mapping loses information in one direction because the SDK's types do.
// DestructiveHint and OpenWorldHint are *bool and carry all three states, so they
// become Hint values directly. ReadOnlyHint and IdempotentHint are plain bool, so a
// server that declared false and one that declared nothing arrive as the same
// bytes: true becomes HintTrue and false becomes HintUnset, since HintFalse would
// put an assertion in a server's mouth it never made. mcpserver.toolAnnotations
// records the same limitation from the serving side.
//
// What it returns is the server's claim as it arrived. Pass it through
// Behavior.Resolve before handing it to functool.New, as newTool does, so a server
// that contradicts itself cannot make its own tool vanish.
func behaviorFromAnnotations(annotations *mcp.ToolAnnotations) toolkit.Behavior {
	if annotations == nil {
		return toolkit.Behavior{}
	}

	behavior := toolkit.Behavior{}
	if annotations.ReadOnlyHint {
		behavior.ReadOnly = toolkit.HintTrue
	}
	if annotations.IdempotentHint {
		behavior.Idempotent = toolkit.HintTrue
	}
	if annotations.DestructiveHint != nil {
		behavior.Destructive = toolkit.HintOf(*annotations.DestructiveHint)
	}
	if annotations.OpenWorldHint != nil {
		behavior.OpenWorld = toolkit.HintOf(*annotations.OpenWorldHint)
	}

	return behavior
}

// toolOutput renders a call result as the string the model is shown.
//
// Structured content is returned as the JSON it marshals to, since it is already
// the output the caller asked for. Otherwise the text blocks are concatenated in
// the order the server sent them, and a block that is not text becomes one line
// naming what arrived, so the model is told something came back rather than shown
// a gap where an image or a resource was.
func toolOutput(res *mcp.CallToolResult, tool string, server string) (string, error) {
	if res.StructuredContent != nil {
		out, err := json.Marshal(res.StructuredContent)
		if err != nil {
			return "", fmt.Errorf("tool %q on mcp server %q returned a structured result that cannot be encoded: %w", tool, server, err)
		}

		return string(out), nil
	}

	var out strings.Builder
	for i, block := range res.Content {
		text, ok := block.(*mcp.TextContent)
		if ok {
			out.WriteString(text.Text)
			continue
		}

		if out.Len() > 0 && !strings.HasSuffix(out.String(), "\n") {
			out.WriteString("\n")
		}
		out.WriteString(contentPlaceholder(block))
		if i < len(res.Content)-1 {
			out.WriteString("\n")
		}
	}

	return out.String(), nil
}

// contentPlaceholder renders a content block that is not text as one line naming
// its type and, when the server gave one, its mime type.
func contentPlaceholder(block mcp.Content) string {
	kind := "unrecognized"
	mime := ""

	switch c := block.(type) {
	case *mcp.ImageContent:
		kind, mime = "image", c.MIMEType
	case *mcp.AudioContent:
		kind, mime = "audio", c.MIMEType
	case *mcp.ResourceLink:
		kind, mime = "resource link", c.MIMEType
	case *mcp.EmbeddedResource:
		kind = "embedded resource"
		if c.Resource != nil {
			mime = c.Resource.MIMEType
		}
	}

	if mime == "" {
		return fmt.Sprintf("[%s content, not shown]", kind)
	}

	return fmt.Sprintf("[%s content of type %s, not shown]", kind, mime)
}
