//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package toolkit

import "encoding/json"

// CallInfo is a tool's own description of one call: the provider it is accounted
// under, the text a surface shows for it, and the per-run dependencies it needs. The
// runner obtains it through Describe instead of switching on the concrete tool type, so
// a new tool carries its own trace text and dependency needs and the runner gains no
// branch for it.
type CallInfo struct {
	// Kind is the provider of the tool, the value behind the kind= log token and the
	// key of the per-kind accounting. The zero value KindUnknown is the safe sentinel
	// for a tool that does not declare a provider.
	Kind Kind
	// Display is the full one-line call trace, already sanitized for terminal
	// display; an empty string suppresses the line.
	Display string
	// DisplayShort is an abbreviated trace with long argument values middle-elided,
	// already sanitized; an empty string means fall back to Display. Only a command
	// tool produces one.
	DisplayShort string
	// Agent identifies the peer serving the call: the remote agent for an a2a tool,
	// the configured server name for an MCP one. It may be empty when the peer is not
	// named, so it is never itself the signal that a call left this process.
	Agent string
	// NeedsPrompter asks the runner to supply the operator Prompter in ExecDeps.
	NeedsPrompter bool
	// NeedsWorkDir asks the runner to supply the per-run WorkDir in ExecDeps.
	NeedsWorkDir bool
	// OperatorPaced reports that the call's duration is set by a person answering, so
	// a caller bounding tool execution must leave this call alone: the bound would
	// cancel the question rather than a runaway. A tool that is merely slow is not
	// operator paced. It is distinct from NeedsPrompter, which every in-process tool
	// sets because every in-process tool is offered a Prompter whether it asks
	// anything or not.
	OperatorPaced bool
}

// Describer is implemented by a tool that describes its own call trace and per-run
// dependency needs, so the runner traces and accounts a call of any kind uniformly,
// without a concrete-type switch. A tool that does not implement it is run and
// traced by name alone, with no dependencies and not as a remote call.
type Describer interface {
	// Describe returns the trace text, provider and dependency needs for a call with
	// the given input. It must not run the tool or mutate state; it is called to
	// build the call trace before execution.
	Describe(input json.RawMessage) CallInfo
}
