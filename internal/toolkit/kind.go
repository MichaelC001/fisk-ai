//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package toolkit

// Kind identifies which provider supplies a tool: the wrapped application, the
// harness itself, another agent, an MCP server, or the embedding caller. It is the
// provider a call is accounted under and the value behind the machine-readable
// kind= log token.
type Kind int

const (
	// KindUnknown is the zero value and the safe sentinel: a tool that never declared
	// a provider, or a call to a name that is not in the registry, is accounted here.
	// It surfaces as kind=unknown rather than silently masquerading as a real
	// provider, so a forgotten assignment is visible in the accounting.
	KindUnknown Kind = iota
	// KindApplication is a tool of the wrapped application: a fisk command tool.
	KindApplication
	// KindBuiltin is a tool the harness provides itself, in-process: the
	// human-in-the-loop, memory, and knowledge tools.
	KindBuiltin
	// KindRemote is a tool served by another agent over a2a.
	KindRemote
	// KindCustom is a tool supplied by the embedding caller through the run's
	// custom tools.
	KindCustom
	// KindMCP is a tool served by an MCP server the operator configured. It is a
	// provider of its own rather than a flavor of KindRemote: a call to a third
	// party's server and a call to another agent over a2a are different providers,
	// and an operator reading the accounting wants to tell them apart.
	KindMCP
)

// String returns the stable, lowercase, machine-readable token for a Kind, used for
// the kind= log token and as the key of the per-kind accounting map in the JSON
// trace. The tokens are part of the log and trace contract, so they are stable
// across releases. An unrecognized value formats as the KindUnknown token so a new
// Kind added without a token here is visible rather than blank.
func (k Kind) String() string {
	switch k {
	case KindApplication:
		return "application"
	case KindBuiltin:
		return "builtin"
	case KindRemote:
		return "remote"
	case KindCustom:
		return "custom"
	case KindMCP:
		return "mcp"
	default:
		return "unknown"
	}
}

// ParseKind returns the Kind a String token names, for reading back a kind that was
// recorded as its token: a run journal, a log line, a stored trace.
//
// A token this build does not have a Kind for, which is what a record written by a
// newer build carries, returns KindUnknown. The call is still counted, under the
// sentinel that says the provider is not known here rather than under a provider it
// was not.
func ParseKind(token string) Kind {
	switch token {
	case "application":
		return KindApplication
	case "builtin":
		return KindBuiltin
	case "remote":
		return KindRemote
	case "custom":
		return KindCustom
	case "mcp":
		return KindMCP
	default:
		return KindUnknown
	}
}
