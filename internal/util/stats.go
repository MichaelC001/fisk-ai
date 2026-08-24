//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package util

import (
	"fmt"
	"os"
	"time"

	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// RunStats accumulates per-run counters for the summary line.
type RunStats struct {
	Start time.Time
	// Model is the LLM model the run used, shown in the summary line.
	Model string
	// Session is the checkpointed session id, shown in the summary line when set.
	Session string
	// TraceID is the telemetry trace this run was exported as, empty when telemetry is
	// off. It is on the summary line because export that silently fails looks exactly
	// like export that works, and because it is the only correlator a chat run that is
	// not checkpointed has: --chat does not imply --checkpoint, so such a run has no
	// session id to group its turns by.
	TraceID string
	// ContentExported reports that this run exported the conversation itself and not
	// only its structure and timing.
	//
	// It is on the summary line because that is the durable channel: the startup note
	// that says the same thing is printed before the full-screen UI opens and is
	// covered for the whole run, while this survives in scrollback in both renderers
	// and is the line an operator pastes into a ticket. A pre-run note cannot tell
	// anyone afterwards that this run's conversation left the machine.
	ContentExported bool
	// Suspended reports that the run was checkpointed and paused rather than
	// completed, so the summary reads "Run suspended" rather than "Run summary".
	Suspended bool
	// LlmCalls is the number of LLM requests made.
	LlmCalls int64
	// ToolCalls is the total number of tool invocations, including the remote and MCP
	// ones counted separately in RemoteToolCalls and MCPToolCalls.
	ToolCalls int64
	// RemoteToolCalls is the number of tool invocations dispatched to a remote
	// agent over a2a, a subset of ToolCalls. It counts the calls that were made, so a
	// call a policy hook denied or the operator refused is outside it, and it is a
	// subset of the KindRemote bucket of ToolCallsByKind rather than that bucket. See
	// ToolCallsByKind.
	RemoteToolCalls int64
	// MCPToolCalls is the number of tool invocations dispatched to an MCP server, a
	// subset of ToolCalls. It counts the calls that were made, on the same terms as
	// RemoteToolCalls, so it is a subset of the KindMCP bucket of ToolCallsByKind rather
	// than that bucket. See ToolCallsByKind.
	MCPToolCalls int64
	// ToolCallsByKind counts calls by the provider that supplied the tool. Every call
	// counted in ToolCalls is counted here too, including the ones answered without
	// running: an unknown tool, a policy denial, a missing required argument, a confirm
	// gate the operator refused. The buckets partition ToolCalls exactly.
	//
	// That is what separates it from RemoteToolCalls and MCPToolCalls, which count the
	// calls that actually left this process. A denied or refused call is in its bucket
	// here and in neither counter, so a counter is a subset of the bucket of the same
	// kind and never that bucket. Equality between the two is a run in which nothing was
	// refused, not a contract.
	//
	// A resume seeds both from the journal, which records each call's kind and whether
	// it was dispatched, so the partition and the two counters hold across a suspend.
	// ToolCallsByKind is nil until the first call is counted.
	ToolCallsByKind map[toolkit.Kind]int64
	InTokens        int64
	OutTokens       int64
	// CacheReadTokens is the input tokens served from the prompt cache (billed at
	// roughly a tenth of the uncached input rate). CacheCreateTokens is the input
	// tokens written into the cache this run (billed at a premium). InTokens keeps
	// its meaning: the uncached input remainder. A healthy multi-iteration run shows
	// InTokens small and CacheReadTokens climbing; a silent cache miss shows
	// CacheReadTokens stuck at zero against a large InTokens.
	CacheReadTokens   int64
	CacheCreateTokens int64
	// ThinkingTokens is the part of OutTokens the model spent reasoning, reported
	// apart because it is the only thing that separates a model that is not reasoning
	// from one reasoning where nobody can see it. It is a subset of OutTokens, so a
	// summary adding the two together reports a cost that was never paid.
	ThinkingTokens int64
}

// CountToolKind records one tool call against its provider kind, allocating the map
// on first use. It is called for every call ToolCalls counts, the ones answered
// without running included, so the buckets partition ToolCalls. It leaves
// RemoteToolCalls and MCPToolCalls alone: those count dispatches, and the caller
// increments them where it dispatches a call. See ToolCallsByKind.
//
// A RunStats is driven only from its own single run goroutine, so this needs no lock.
func (s *RunStats) CountToolKind(kind toolkit.Kind) {
	if s.ToolCallsByKind == nil {
		s.ToolCallsByKind = make(map[toolkit.Kind]int64)
	}

	s.ToolCallsByKind[kind]++
}

// Print writes the run summary to stderr. It is deferred so every exit path,
// including errors and a graceful suspend, reports what was spent. Verbose adds the
// cache-write count, of interest mainly when diagnosing cache behavior.
func (s *RunStats) Print(verbose bool) {
	fmt.Fprintf(os.Stderr, "\n%s\n", s.summaryLine(verbose))
}

// summaryLine formats the run summary. The label distinguishes a suspended run
// from a completed one; the session id and model are shown only when set, and the
// remote and MCP tool call counts only when any were made, so an ephemeral run stays
// uncluttered. The cache-read count appears only when the cache was hit (following
// the remote_tool_calls pattern); the cache-write count is verbose-only.
func (s *RunStats) summaryLine(verbose bool) string {
	label := "Run summary"
	if s.Suspended {
		label = "Run suspended"
	}

	session := ""
	if s.Session != "" {
		session = fmt.Sprintf("session=%s ", s.Session)
	}

	model := ""
	if s.Model != "" {
		model = fmt.Sprintf("model=%s ", s.Model)
	}

	tools := fmt.Sprintf("tool_calls=%d", s.ToolCalls)
	if s.RemoteToolCalls > 0 {
		tools += fmt.Sprintf(" remote_tool_calls=%d", s.RemoteToolCalls)
	}
	if s.MCPToolCalls > 0 {
		tools += fmt.Sprintf(" mcp_tool_calls=%d", s.MCPToolCalls)
	}

	cache := ""
	if s.CacheReadTokens > 0 {
		cache += fmt.Sprintf(" cached=%d", s.CacheReadTokens)
	}
	if verbose && s.CacheCreateTokens > 0 {
		cache += fmt.Sprintf(" cache_write=%d", s.CacheCreateTokens)
	}

	// Unconditionally, unlike the cache tiers above. Reasoning is not rendered unless
	// it is asked for, so this is the only place a run says whether any happened, and
	// omitting it at zero would make "did not reason" and "reasoned invisibly" look the
	// same. It is a share of the output half of tokens=, not an addition to it.
	thinking := fmt.Sprintf(" thinking=%d", s.ThinkingTokens)

	trace := ""
	if s.TraceID != "" {
		trace = fmt.Sprintf(" trace=%s", s.TraceID)
	}
	// Next to the trace id, since together they say what left the machine and where to
	// find it. Present only when it happened: a marker on every run would be noise, and
	// this one has to read as a fact about this run.
	if s.ContentExported {
		trace += " content=exported"
	}

	return fmt.Sprintf("%s: %s%sllm_calls=%d %s tokens=%d/%d%s%s latency=%s%s",
		label, session, model, s.LlmCalls, tools, s.InTokens, s.OutTokens, thinking, cache, time.Since(s.Start).Round(time.Millisecond), trace)
}
