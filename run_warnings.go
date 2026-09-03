//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strings"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/sanitize"
)

// warningLead announces a warning the agent raised, naming which agent that was.
//
// Every warning a run produces reaches this terminal the same way, over a2a, whether the
// agent is in this process behind the embedded broker or on somebody else's machine. Read
// as a bare "warning:" the two are indistinguishable, and a failure on a worker then reads
// as a failure here: a model call refused for want of a key is the case that showed it,
// where the terminal holding no key is the normal arrangement and the message looked like
// it had.
//
// A hosted agent is named too rather than only a remote one, so the wording that appears
// when something is wrong is the wording that appears when nothing is. The nats context is
// what tells them apart, being empty for an agent hosted here.
func warningLead(identity string, natsContext string) string {
	if identity == "" {
		return "warning"
	}

	if natsContext == "" {
		return "warning from agent " + sanitize.ForTerminal(identity, 120)
	}

	return "warning from remote agent " + sanitize.ForTerminal(identity, 120)
}

// warningMessage is the operator-facing text for a Warning, without the "warning: "
// prefix, so both the line UI and the full-screen UI render the same wording (the
// line UI prints it after "warning: "; the full-screen UI shows it as a warning
// line). An empty string means the kind carries no message.
func warningMessage(w agent.Warning) string {
	switch w.Kind {
	case agent.WarnHITLNoTerminal:
		return "human_in_the_loop is enabled, but this run cannot ask anyone. Its tools decline instead of prompting. Set expose.agent.a2a.prompts.elicit on the agent, or run it at an interactive terminal."
	case agent.WarnConfirmNoTerminal:
		return fmt.Sprintf("%d tool(s) need approval before they run, but this run cannot ask anyone. They are always declined. Set expose.agent.a2a.prompts.elicit on the agent, or run it at an interactive terminal.", w.Count)
	case agent.WarnConfirmTagUnmatched:
		return fmt.Sprintf("confirm_tags entry %q matches no loaded tool; check the tag spelling (run 'fisk info' to list tags)", w.Name)
	case agent.WarnUnknownTool:
		return fmt.Sprintf("model called unknown tool %q", w.Name)
	case agent.WarnMissingRequired:
		return fmt.Sprintf("model called tool %q without required parameter(s): %s; not run", w.Name, strings.Join(w.Params, ", "))
	case agent.WarnJournalTerminal:
		return fmt.Sprintf("recording terminal state: %v", w.Err)
	case agent.WarnJournalUser:
		return fmt.Sprintf("recording your follow-up failed: %v; the session ended but remains resumable from the last saved turn", w.Err)
	case agent.WarnResumePausedTurn:
		return "this session was suspended at a paused turn; if its server-side tool state has expired the resume may error"
	case agent.WarnMaxIterInteractive:
		return fmt.Sprintf("the previous turn reached the iteration cap (%d) before finishing; send a follow-up to steer it, or Ctrl-D to end", w.Count)
	case agent.WarnTurnErrorInteractive:
		return fmt.Sprintf("the previous turn failed: %v; send a follow-up to retry, or Ctrl-D to end", w.Err)
	case agent.WarnMemoryIndex:
		return fmt.Sprintf("reading memory for the start-of-run index failed: %v; continuing without it, the memory tools still work", w.Err)
	case agent.WarnToolSearchUnsupported:
		return fmt.Sprintf("%d tools are available but the model backend does not support server-side tool search, so all are sent to the model directly and use more context each request; use a provider that supports tool search to defer them", w.Count)
	case agent.WarnKnowledgeIndexAbsent:
		return fmt.Sprintf("knowledge is enabled but no index exists at %q; if you built it with 'knowledge index', build it again under the same root, or set root_directory in the config so the CLI and the agent read it from one file, or give both a matching --store-dir or an absolute knowledge directory, or knowledge_search will return nothing", w.Name)
	case agent.WarnTraceClose:
		return fmt.Sprintf("closing trace file: %v", w.Err)
	case agent.WarnJournalClose:
		return fmt.Sprintf("closing session journal: %v", w.Err)
	case agent.WarnTraceWrite:
		return fmt.Sprintf("trace write failed, trace will be incomplete: %v", w.Err)
	case agent.WarnPromptDenied:
		return fmt.Sprintf("your prompt was rejected by a policy hook: %s; enter a different prompt, or Ctrl-D to end", w.Name)
	case agent.WarnRunEndHook:
		return fmt.Sprintf("the RunEnd hook failed: %v; the run's outcome is unaffected", w.Err)
	case agent.WarnUnknownReservedTag:
		return fmt.Sprintf("tool %q carries unknown reserved tag(s): %s; the ai: prefix is reserved and these do nothing, check the spelling (run 'fisk info' to list the reserved tags)", w.Name, strings.Join(w.Params, ", "))
	case agent.WarnBehaviorTagConflict:
		return fmt.Sprintf("tool %q carries contradictory behavior tags: %s; the more dangerous reading was used and the tool is still available", w.Name, strings.Join(w.Params, ", "))
	case agent.WarnToolTimeout:
		return fmt.Sprintf("tool %q was stopped: %v; raise harness.tool_timeout if the tool needs longer", w.Name, w.Err)
	case agent.WarnApprovalsDropped:
		return fmt.Sprintf("%d standing approval(s) were not restored because --force resumed this session across a changed configuration; you will be asked again for those commands", w.Count)
	case agent.WarnRemoteTagFilterIgnored:
		return fmt.Sprintf("remote agent %q include filter uses tags, which discovery does not carry; the tag filter was ignored (filter by tool name instead)", w.Name)
	case agent.WarnRemoteToolsSkipped:
		return fmt.Sprintf("remote agent %q: skipped %s", w.Name, strings.Join(w.Params, "; "))
	case agent.WarnRemoteNoTools:
		return fmt.Sprintf("remote agent %q contributed no tools after filtering; check the include/exclude for that host (run 'fisk info' to see what it advertises)", w.Name)
	case agent.WarnMCPToolsChanged:
		if w.Err != nil {
			return fmt.Sprintf("mcp server %q reported that its tool list changed and could not be listed again: %v; this run continues with the tools it already had", w.Name, w.Err)
		}
		return fmt.Sprintf("mcp server %q changed its tool list: %s; the model is offered the new set from its next request", w.Name, strings.Join(w.Params, "; "))
	case agent.WarnToolSetDrift:
		return "this session was saved with a different tool set; it continues under the current one, and the approvals you gave it were dropped since a tool may have moved under them"
	case agent.WarnBudgetDrift:
		return fmt.Sprintf("this session was saved with different budget bounds (%s); it continues under the current configuration", strings.Join(w.Params, ", "))
	case agent.WarnPIIRedacted:
		return fmt.Sprintf("personal data was redacted from %s (%d value(s): %s) before the model, the session store or telemetry saw it; further redactions in this run are logged but not repeated here, and harness.pii.mode controls this", w.Name, w.Count, strings.Join(w.Params, ", "))
	case agent.WarnPIIWithheld:
		if w.Err != nil {
			return fmt.Sprintf("text from %s was withheld because it could not be scanned for personal data: %v; set harness.pii.mode to off to run unscanned", w.Name, w.Err)
		}
		return fmt.Sprintf("text from %s was withheld because it contains personal data (%d value(s): %s) and harness.pii.mode is reject; set it to redact to have the values replaced instead", w.Name, w.Count, strings.Join(w.Params, ", "))
	default:
		return ""
	}
}
