//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/choria-io/fisk-ai/internal/toolkit"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/memory"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/telemetry"
	"github.com/choria-io/fisk-ai/internal/telemetry/genai"
	"github.com/choria-io/fisk-ai/internal/util"
)

// runner drives the agentic loop. Its fields split cleanly into two groups: the
// infrastructure rebuilt from configuration on every start or resume (client,
// tool registries, prompt, budget) and the mutable run state that a snapshot must
// carry (messages). This separation is what makes a run resumable: the state is a
// plain value, the infrastructure is reconstructed.
type runner struct {
	cfg      *config.Config
	provider llm.Provider
	stats    *util.RunStats

	system          []string
	toolDefs        []llm.ToolDef
	toolSearch      bool
	thinking        llm.ThinkingMode
	maxOutputTokens int64
	maxIter         int64
	maxTokens       int64

	// budgetBase is the token total this conversation started from, subtracted from the
	// run's cumulative stats to give what the current journal has spent.
	//
	// It is zero for the whole of an ordinary run, including a resume, since a resume
	// seeds the stats from the journal it is continuing and that history is the
	// conversation's own. A session rotation moves it to the total so far, because the
	// journal it opens is a new conversation with an allowance of its own, while the
	// stats keep climbing to report the whole sitting.
	budgetBase int64

	// toolTimeout bounds one tool call, zero leaving tool execution unbounded, which a
	// configuration asks for with an explicit 0s. It is resolved once here rather than
	// read through cfg per call, as the budgets above are.
	toolTimeout time.Duration

	// tools is the single dispatch registry: every model-facing tool, whatever its
	// kind (local command, in-process built-in, remote), keyed by the unique name the
	// model addresses it by. executeTool looks a call up here once and runs it through
	// the uniform Tool contract, consulting narrow capability interfaces for the
	// kind-specific policy (argument validation, confirmation) and the Describer
	// interface for the kind-specific call trace and execution dependencies.
	tools       map[string]toolkit.Tool
	confirmTags []string
	gate        *util.ConfirmGate

	verbose bool

	// promptCache turns on Anthropic prompt caching: two cache_control breakpoints per
	// request (one after tools+system, one on the conversation tail). Resolved once at
	// setup from cfg.PromptCacheEnabled(); kept out of the fingerprint so toggling it
	// never refuses a resume.
	promptCache bool

	// interactive says this run holds its own continuation loop, so it reports turns to
	// telemetry and rests at an input boundary rather than ending.
	interactive bool

	// humanPaced says the next call on this conversation comes after somebody's think
	// time, which is what chooses the longer cache lifetime. It is separate from
	// interactive because a caller taking one turn per run holds no continuation loop and
	// is still paced by a person between turns.
	humanPaced bool

	// events receives the run's narration, tool traces and warnings so the caller
	// owns all wording and rendering.
	events Events

	// finalText is the text of the most recent assistant turn that carried any,
	// which Run reports as Result.Text. It is kept here because a caller that does
	// not render the stream has no other way to reach the answer, and because the
	// last turn of a run stopped by the budget or the iteration cap is not marked
	// terminal and so cannot be recognized from the events alone.
	finalText string

	// hooks are the caller's optional callbacks invoked at fixed points in the loop. A
	// nil field does not fire. They run on this single run goroutine, in loop order.
	hooks Hooks

	// prompter puts the run's interactive decisions (confirm-gate approval and the
	// human-in-the-loop questions) to the operator. It is used only from this single
	// run goroutine, never from the concurrent MCP path.
	prompter toolkit.Prompter

	// toolWorkDir is the directory local command tools run in, handed to each tool
	// execution so concurrent runs sharing one process do not collide on relative-path
	// writes. Empty inherits the process working directory.
	toolWorkDir string

	// messages is the conversation, grown as the loop runs. It is the core of the
	// resumable state.
	messages []llm.Message

	// journal records every event for durable suspend/resume. It is nil when
	// snapshotting is disabled, in which case the run behaves exactly as before.
	journal runstate.Journal
	// seq is the last journal seq written; the next append is seq+1. On a fresh
	// snapshotted run it starts at 1 (the meta record). On resume it is seeded
	// from the journal's last seq.
	seq uint64
	// startIter is the loop index to begin at: 0 for a fresh run, the resumed
	// NextIteration otherwise, so the iteration cap stays cumulative.
	startIter int64
	// iter is the current loop index, seeded from startIter. It lives on the runner so
	// it stays monotonic across interactive turns (each re-entry of loop continues the
	// numbering rather than restarting it), keeping AssistantRecord.Iteration and the
	// trace iteration unique per turn.
	iter int64
	// turn is the one-based index of the current interactive turn. It is separate from
	// iter, which counts model calls and continues across turns; a turn is the unit an
	// operator recognizes and one turn spans many iterations.
	turn int64
	// telemetry records the run's spans and metrics. Nil records nothing and every
	// method on it is nil-safe, so the loop calls it without asking whether it is on.
	telemetry *telemetry.Provider
	// providerName is the gen_ai.provider.name value for the backend in use, read from
	// its capabilities rather than from the config so an injected provider reports what
	// actually ran rather than what was configured.
	providerName string
	// sessionBackend names the session store backend, for the journal append metric. It
	// is resolved once at construction like providerName and identity below, rather than
	// read through cfg on every append.
	sessionBackend string
	// identity is the agent identity, held here rather than reached for through cfg so
	// the telemetry paths do not depend on the whole config object being present.
	identity string
	// contentFrom is the conversation index the next model call's captured input starts
	// at: everything from here on is what this call adds to what the previous one
	// already carried. It is maintained whether or not content capture is on, which is
	// the one place this design pays for keeping enable/disable branches out of the
	// loop, and it is cheap: one int, assigned where the conversation changes.
	//
	// It exists because the alternative is quadratic. gen_ai.input.messages is the
	// whole conversation, so capturing it per call re-exports a growing transcript on
	// every iteration, and a thirty-iteration run ships thirty copies.
	//
	// Every site that REPLACES the conversation must reset it, and the reset belongs
	// with the assignment rather than near the decision that led to it: resetContext is
	// only reached when the run is not checkpointed, a bare /clear on a checkpointed run
	// clears nothing until the next real prompt arrives, and a rotation that fails
	// leaves the original conversation in place. The builder clamps as well, so a
	// mutation site added later degrades to an empty delta rather than slicing out of
	// range and taking the run down through the panic barrier.
	contentFrom int
	// memoryTools names the built-in memory tools, and memory describes the store they
	// are bound to, so a tool span can say which backend served it. Both are handed in
	// by the setup that built them: the alternative, a name prefix or a list in here,
	// would be a second copy of the memory tool list, and a memory tool added later
	// would leave this file and builtin.MemoryTools each internally consistent while
	// the new tool silently lost its attribution. Empty when memory is off, which
	// leaves the attributes absent rather than blank.
	memoryTools map[string]bool
	memory      telemetry.MemoryInfo
	// memScope is the memory scope this run's tools read and write through, held so the
	// revisions it accumulated can be journaled as the run ends. A nil scope is a
	// working scope holding nothing, so a runner assembled without one records nothing.
	memScope *memory.Scope
	// approvals holds the confirm gate's standing grants. The runner journals the
	// ones the operator gives once the call that triggered them is answered, and
	// clears them at a context reset.
	approvals *journalApprovals
	// pending is an in-flight tool batch restored on resume: its unanswered tools
	// are run before the loop proceeds.
	pending *runstate.PendingTurn
	// completingPending is true while that restored batch is running, so those tool
	// spans can be marked as belonging to a resume.
	completingPending bool
	// deferred collects the calls this run left waiting on an answer, so Result can
	// report them. A resume seeds it from the journal before running anything, so a
	// run that inherits a deferral reports it as readily as one that made it.
	deferred []DeferredCall
	// suspendRequested reports that a graceful suspend was asked for; it is polled
	// at the loop boundary, never mid-tool. Nil when suspension is not wired.
	suspendRequested func() bool

	// nextPrompt continues the run interactively: after a boundary the operator can act
	// on it gathers the next decision (a follow-up, a context reset, or an end). Nil for
	// a one-shot run. Called only from this single run goroutine.
	nextPrompt func(context.Context) Continuation

	// sessionID is the current checkpoint session; empty for a non-checkpointed run. It
	// changes when a context reset rotates to a fresh session, so the caller reads it back
	// after the run to report the final session on-screen.
	sessionID string
	// newSession starts a fresh checkpoint session with the given first prompt, returning
	// its journal and id. It carries the store, fingerprint and meta the runner does not
	// otherwise hold, and is nil for a non-checkpointed run (which resets in memory only).
	newSession func(prompt string) (runstate.Journal, string, error)
	// resetPending marks a deferred context reset for a checkpointed run: a bare /clear
	// clears nothing until the operator supplies a prompt, so the fresh session is created
	// with a real first prompt rather than an empty one (which would fail to resume).
	resetPending bool

	// resumeAtInputBoundary is set when resuming a chat session whose last turn is
	// complete: the conversation already rests at an input boundary, so the initial
	// loop() is skipped (a fresh LLM call on a completed conversation would be wrong)
	// and control drops straight to the input bar. A resume with an in-flight turn
	// leaves this false so loop() finishes that turn first.
	resumeAtInputBoundary bool

	// followUp is the user turn a resume was asked to deliver, empty for every other
	// run. followUpAtStart says the restored conversation already rests where a user
	// message may be added, so the turn enters before the first model call; false sends
	// the loop to finish the turn the last run left and delivers at the boundary it
	// reaches. followUpTaken records that it entered the conversation, which is what
	// tells a caller its prompt ran from a conversation that never reached a boundary.
	followUp        string
	followUpAtStart bool
	followUpTaken   bool
}

// emit appends a record to the journal, advancing the seq. It is a no-op when
// snapshotting is disabled.
//
// It times the append and records the duration metric. That is measured from out here
// rather than inside the store because it needs no interface change to be correct:
// runstate.Journal takes no context, and threading one through Store, Journal and both
// backends to open a span per append would buy a hundred near-identical spans per run
// for something that is a local write on the default backend. The metric answers the
// question those spans were wanted for. See telemetry.MetricSessionAppendDuration.
func (r *runner) emit(rec runstate.Record) error {
	if r.journal == nil {
		return nil
	}

	r.seq++

	start := time.Now()
	err := r.journal.Append(r.seq, rec)
	r.recordAppend(start, err)

	if err != nil {
		return fmt.Errorf("journaling run: %w", err)
	}

	return nil
}

// journalGrants writes the standing approvals the operator gave during a tool call,
// and is called once that call has been answered or has deferred. Recording them
// then rather than at the moment of approval is what stops a grant outliving the
// call it was given for: a crash in between loses it, and the resume asks again for
// the command it is about to re-run.
//
// A runner assembled without an approval source journals nothing, which is the shape
// the package's own tests build.
func (r *runner) journalGrants() error {
	if r.approvals == nil {
		return nil
	}

	return r.approvals.flush()
}

// journalMemoryRevisions records the memory revisions this run read, so the next turn
// of the conversation can overwrite a value it read on an earlier one. It is written
// as the run ends, after the terminal record, since a memory read on the last tool
// call of the run counts as much as the first.
//
// A run that read no memory writes nothing, and a failed append warns rather than
// ending anything: the run is over, and the next turn reads the value again as every
// turn does today.
func (r *runner) journalMemoryRevisions() {
	revs := r.memScope.Snapshot()
	if len(revs) == 0 {
		return
	}

	jerr := r.emit(runstate.Record{
		Protocol:        runstate.MemoryRevisionsProtocol,
		Optional:        true,
		MemoryRevisions: &runstate.MemoryRevisionsRecord{Revisions: revs},
	})
	if jerr != nil {
		r.events.Warn(Warning{Kind: WarnJournalMemoryRevisions, Err: jerr})
	}
}

// recordAppend reports one append's duration, classifying a failure as a store error
// unless the context cases claim it first.
//
// ClassifyContext runs before ClassStore for the same reason the memory index span does
// it (section 6.2): an interrupt landing during a network-backed append would otherwise
// be reported as a broken session store, sending an operator to look at JetStream when
// what happened is that someone pressed Ctrl-C.
//
// The context is a background one because no context reaches emit: one of its call sites
// is appendUserPrompt, which has none, and threading one there for this would cascade
// through callers that have no other use for it. Nothing on this instrument is derived
// from a context; the cost is that a metric exemplar cannot link back to the active span,
// which this build does not enable exemplars for anyway.
func (r *runner) recordAppend(start time.Time, err error) {
	var class telemetry.ErrorClass

	if err != nil {
		var ok bool
		class, ok = telemetry.ClassifyContext(err)
		if !ok {
			class = telemetry.ClassStore
		}
	}

	r.telemetry.RecordSessionAppend(context.Background(), r.sessionBackend, time.Since(start), class)
}

// runTurn drives one turn and records it.
//
// It wraps every call into the loop, including a one-shot run's only one, because the
// turn is the unit the agent metrics are keyed on and a one-shot run is a single turn.
// The SPAN is interactive-only: a one-shot run's root already is the agent invocation,
// so a turn span there would nest an identically named span exactly inside it, which
// reads in a flame graph as a defect rather than as structure.
//
// The usage delta is taken around the call rather than read from the loop, because
// stats accumulates across the whole run (and, on a resume, across the whole session)
// and only the difference belongs to this turn.
func (r *runner) runTurn(ctx context.Context) (runstate.TerminalReason, error) {
	r.turn++
	started := time.Now()
	beforeUsage := runStatsUsage(r.stats)
	beforeCalls, beforeTools := r.stats.LlmCalls, r.stats.ToolCalls

	turnCtx := ctx
	var span *telemetry.TurnSpan
	if r.interactive {
		turnCtx, span = r.telemetry.StartTurn(ctx, telemetry.TurnInfo{
			Identity: r.identity,
			// The session current now, which a context reset may have rotated away from
			// the one the run started on. Attributing this turn to the earlier session
			// would name a journal that does not hold it.
			ConversationID: r.sessionID,
			Index:          r.turn,
		})
	}

	reason, err := r.loop(turnCtx)

	if span != nil {
		outcome := telemetry.TurnOutcome{
			TerminalReason: string(reason),
			Usage:          runStatsUsage(r.stats).Sub(beforeUsage),
		}
		if err != nil {
			outcome.Failed = true
			outcome.Class = runErrorClass(err, true)
		}
		span.Finish(outcome)
	}

	// Recorded in both modes, so the series means one thing: a one-shot run is a single
	// turn. Recording it at the root for one-shot runs and here for chats would mix "one
	// turn" with "a whole session including operator think time", and a p95 over that
	// mixture answers no question anyone has.
	//
	// The counts are deltas taken around this turn, never the running totals: a resume
	// seeds those with the whole session's history, so the first turn after a resume
	// would otherwise report every call the session has ever made.
	r.telemetry.RecordTurn(ctx, telemetry.TurnMetrics{
		AgentName:      r.identity,
		TerminalReason: string(reason),
		Interactive:    r.interactive,
		Duration:       time.Since(started),
		InferenceCalls: r.stats.LlmCalls - beforeCalls,
		ToolCalls:      r.stats.ToolCalls - beforeTools,
	})

	return reason, err
}

// chatOutcome describes a finished model call for its span.
//
// A reply the loop treats as a failure is reported as one even though the call itself
// succeeded: a turn truncated at the output cap is an incomplete answer and a refusal
// is a non-answer, and both end the run. A span that showed those as successful calls
// would disagree with the run's own outcome.
func chatOutcome(resp *llm.Response, err error) telemetry.ChatOutcome {
	if err != nil {
		out := telemetry.ChatOutcome{Failed: true, Class: telemetry.ClassProvider}
		class, ok := telemetry.ClassifyContext(err)
		if ok {
			out.Class = class
		}
		return out
	}

	if resp == nil {
		return telemetry.ChatOutcome{}
	}

	out := telemetry.ChatOutcome{
		ResponseID:    resp.ID,
		ResponseModel: resp.Model,
		FinishReason:  string(resp.StopReason),
		Usage: telemetry.TokenUsage{
			Input:       resp.Usage.In + resp.Usage.CacheRead + resp.Usage.CacheCreate,
			Output:      resp.Usage.Out,
			CacheRead:   resp.Usage.CacheRead,
			CacheCreate: resp.Usage.CacheCreate,
			Uncached:    resp.Usage.In,
			Reasoning:   resp.Usage.Thinking,
		},
		Output: genai.OutputMessages(resp.Content, string(resp.StopReason)),
	}

	switch resp.StopReason {
	case llm.StopMaxTokens:
		out.Failed, out.Class = true, telemetry.ClassTruncated
	case llm.StopRefusal:
		out.Failed, out.Class = true, telemetry.ClassRefusal
	}

	return out
}

// runStatsUsage reads the run's running token totals in the shape telemetry reports.
// Input carries the cached tiers as the semantic conventions require, while Uncached
// keeps the raw remainder that the run summary line prints.
func runStatsUsage(stats *util.RunStats) telemetry.TokenUsage {
	return telemetry.TokenUsage{
		Input:       stats.InTokens + stats.CacheReadTokens + stats.CacheCreateTokens,
		Output:      stats.OutTokens,
		CacheRead:   stats.CacheReadTokens,
		CacheCreate: stats.CacheCreateTokens,
		Uncached:    stats.InTokens,
		Reasoning:   stats.ThinkingTokens,
	}
}

// run executes the agentic loop to a terminal state, returning the reason it
// stopped and, for the failure reasons, the error to surface to the caller. A
// nil error with ReasonCompleted is a successful run. When journaling is enabled
// it records why the run ended (including a graceful suspend) so a resume can
// tell a clean stop from a crash.
func (r *runner) run(ctx context.Context) (runstate.TerminalReason, error) {
	// Seed the loop index once, here, so it is correct for a fresh run (0) and a resume
	// (the restored NextIteration) regardless of how the runner was constructed, and so
	// it then stays monotonic across the interactive re-entries below.
	r.iter = r.startIter

	var (
		reason runstate.TerminalReason
		err    error
	)

	switch {
	case r.followUp != "" && r.followUpAtStart:
		// The conversation rests where a user message may be added, so the turn the
		// caller asked for is the next model call.
		reason, err = r.followUpTurn(ctx)

	case r.resumeAtInputBoundary:
		// A resumed chat that is already at a completed boundary skips the initial loop:
		// the conversation ends on an assistant turn awaiting a follow-up, so a fresh LLM
		// call would be wrong. Treat it as a just-completed turn and fall straight into
		// the continuation loop, which opens the input bar.
		reason = runstate.ReasonCompleted

	default:
		reason, err = r.runTurn(ctx)

		// A follow-up arrived on a conversation the last run left part way through. That
		// turn is finished now, so the follow-up is the next one. A turn that ended
		// without reaching a boundary the conversation can take input at, which is one
		// waiting on a deferred tool result, leaves the prompt undelivered and says so
		// through Result.FollowUpTaken rather than journaling a turn nothing answers.
		if r.followUp != "" && continuable(reason) && ctx.Err() == nil {
			reason, err = r.followUpTurn(ctx)
		}
	}

	// Interactive continuation: after a turn the operator can act on, hand back for a
	// follow-up. A completed turn and a turn that hit the iteration cap are both
	// recoverable (the operator steers with another prompt); an infra error, an
	// exhausted budget and a cancellation end the session. Each accepted follow-up
	// extends the iteration cap by a fresh turn's worth, so one long turn does not
	// starve the next. The single terminal record below is written once, at true end.
	for r.nextPrompt != nil && ctx.Err() == nil && continuable(reason) {
		// TurnEnd fires at each interactive continuation boundary, before the next prompt
		// is gathered, reporting why the just-ended turn stopped. It is an observation
		// point with no power to continue; a returned error aborts the run.
		terr := r.hooks.fireTurnEnd(ctx, TurnEndInfo{Reason: reason, Iteration: int(r.iter)})
		if terr != nil {
			reason, err = runstate.ReasonError, fmt.Errorf("TurnEnd hook: %w", terr)
			break
		}

		switch reason {
		case runstate.ReasonMaxIterations:
			r.events.Warn(Warning{Kind: WarnMaxIterInteractive, Count: int(r.cfg.LLM.Budget.MaxIterations)})
		case runstate.ReasonError:
			// A turn that failed (an LLM call timeout, an API error) does not end an
			// interactive session: surface the cause and hand back to the input bar so
			// the operator can retry or steer, rather than stalling with no way back. An
			// abort (a canceled ctx) is filtered by the loop condition above, so it still
			// ends the session.
			r.events.Warn(Warning{Kind: WarnTurnErrorInteractive, Err: err})
		}

		cont := r.nextPrompt(ctx)
		if !cont.Continue {
			// A cancellation while the prompt was up (an abort) is surfaced as the error
			// it is, so the caller classifies it as aborted. A clean end (the operator
			// left) completes a plain chat; a checkpointed chat instead suspends, since
			// the journal keeps it resumable and there is no free-standing "user turn
			// completed the run" state to record - the operator returns with --resume.
			switch {
			case ctx.Err() != nil:
				reason, err = runstate.ReasonError, ctx.Err()
			case r.journal != nil:
				reason, err = runstate.ReasonSuspended, nil
			default:
				reason, err = runstate.ReasonCompleted, nil
			}
			break
		}

		// Follow-up UserPromptSubmit: a real prompt is entering the conversation (possibly
		// against a context this same submission clears). It fires before the reset/rotation
		// and before the prompt is appended or journaled, so a Deny reopens the input
		// without clearing the context, rotating a session, or journaling a rejected turn. A
		// bare reset carries no prompt (empty Text), so nothing submits. To reject a prompt
		// the hook sets Deny; a returned error instead ends the whole session.
		if cont.Text != "" {
			dec, herr := r.hooks.fireUserPromptSubmit(ctx, UserPromptSubmitInfo{Text: cont.Text, Initial: false})
			if herr != nil {
				reason, err = runstate.ReasonError, fmt.Errorf("UserPromptSubmit hook: %w", herr)
				break
			}
			if dec.Deny {
				// A denied follow-up does not end the session: surface the reason and reopen
				// the input. Drop the stale reason so the max-iteration or turn-error warning
				// above does not re-fire at the next boundary.
				r.events.Warn(Warning{Kind: WarnPromptDenied, Name: dec.DenyReason})
				reason = runstate.ReasonCompleted
				continue
			}
			// Applied to cont.Text itself so every path below reads the rewritten prompt:
			// the append and the journal record, and the rotation that seeds a fresh
			// session's Meta.Prompt with it.
			if dec.Rewrite != "" {
				cont.Text = dec.Rewrite
			}

			// A conversation at its token cap takes no further turn. Refused above the
			// reset below, so a prompt that will not run cannot first rotate the session
			// and become the Meta.Prompt of a fresh journal that then records only its own
			// refusal. A reset carrying a prompt is exempt: it opens a new conversation,
			// and rotateSession gives that one an allowance of its own.
			if !cont.Reset && r.overBudget() {
				reason, err = runstate.ReasonBudget, r.budgetError()
				break
			}
		}

		if cont.Reset {
			// The cleared context has no prior-turn outcome, so drop the stale reason
			// before looping back or running the fresh turn; otherwise the max-iteration
			// or turn-error warning above would re-fire at the next boundary.
			reason = runstate.ReasonCompleted
			if r.journal == nil {
				// A non-checkpointed run clears in memory at once: a bare reset reopens the
				// input, a reset with a prompt runs against the empty context.
				r.resetContext()
			} else {
				// A checkpointed run defers the clear until a prompt is in hand, so the fresh
				// session is created with a real first prompt (an empty Meta.Prompt would fail
				// to resume). Rotation happens below, once cont.Text is known.
				r.resetPending = true
			}
		}

		// A bare context reset (no prompt) reopens the input without running a turn; loop
		// back to gather the fresh prompt.
		if cont.Text == "" {
			continue
		}

		// A deferred checkpoint reset now has its prompt: rotate to a fresh session so the
		// clear is durable and the previous session stays resumable. On failure the reset is
		// abandoned and the turn runs on in the current session, which stays consistent
		// because its messages were never cleared.
		rotated := false
		if r.resetPending {
			r.resetPending = false
			rerr := r.rotateSession(cont.Text)
			if rerr != nil {
				r.events.Warn(Warning{Kind: WarnSessionRotate, Err: rerr})
			} else {
				rotated = true
			}
		}

		// A rotation already seeded the fresh session with this prompt (it is the new
		// session's Meta.Prompt, so it needs no separate user record). Otherwise add the
		// prompt to the conversation and journal it before the turn so a resume reconstructs
		// the same conversation. On a journal failure end the session here, before the turn:
		// the journal then stops at the last coherent boundary (resumable) rather than
		// recording assistant turns with no preceding user message. The in-memory prompt is
		// discarded with the run; only the journal is the source of truth.
		if !rotated {
			r.appendUserPrompt(cont.Text)

			jerr := r.emit(runstate.Record{Protocol: runstate.UserProtocol, User: &runstate.UserRecord{
				Message: llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Text: &llm.TextBlock{Text: cont.Text}}}},
			}})
			if jerr != nil {
				r.events.Warn(Warning{Kind: WarnJournalUser, Err: jerr})
				reason, err = runstate.ReasonError, jerr
				break
			}
		}

		r.maxIter += r.cfg.LLM.Budget.MaxIterations
		reason, err = r.runTurn(ctx)
	}

	if r.journal != nil {
		tr := &runstate.TerminalRecord{Reason: reason}
		if err != nil {
			tr.Message = err.Error()
		}
		jerr := r.emit(runstate.Record{Protocol: runstate.TerminalProtocol, Terminal: tr})
		if jerr != nil {
			r.events.Warn(Warning{Kind: WarnJournalTerminal, Err: jerr})
		}

		r.journalMemoryRevisions()
	}

	return reason, err
}

// followUpTurn adds the caller's new user turn to the conversation and runs it.
//
// The prompt is appended and journaled before the model call, in that order, so a
// resume reconstructs the same conversation and a crash between the two costs the turn
// rather than leaving a journal describing a conversation the model never saw. A
// journal failure ends the run here, before the turn, on the rule the interactive
// follow-up already follows: the journal stops at its last coherent boundary rather
// than recording assistant turns with no user message in front of them.
//
// The turn gets a full iteration budget from where the conversation actually is, so a
// turn that finished interrupted work first does not spend that work's iterations on
// the caller's prompt. The token budget is the opposite and is spent, so a conversation
// already at its cap takes no further turn.
func (r *runner) followUpTurn(ctx context.Context) (runstate.TerminalReason, error) {
	text := r.followUp
	r.followUp = ""

	dec, herr := r.hooks.fireUserPromptSubmit(ctx, UserPromptSubmitInfo{Text: text, Initial: false})
	if herr != nil {
		return runstate.ReasonError, fmt.Errorf("UserPromptSubmit hook: %w", herr)
	}
	if dec.Deny {
		return runstate.ReasonError, fmt.Errorf("the follow-up prompt was rejected by a policy hook: %s", dec.DenyReason)
	}
	// Applied before the append and the journal write below, which are the only two
	// readers of the text from here.
	if dec.Rewrite != "" {
		text = dec.Rewrite
	}

	// Refused here, above the append and the journal write, so a conversation that cannot
	// take the turn does not keep a user message the model never saw. Appending it would
	// leave the next turn to merge with a prompt nobody answered.
	//
	// This is the only place the cumulative bound is applied to a served conversation:
	// the channel gives every turn after the first as a follow-up, so this is the head of
	// every one of them.
	if r.overBudget() {
		return runstate.ReasonBudget, r.budgetError()
	}

	r.appendUserPrompt(text)

	jerr := r.emit(runstate.Record{Protocol: runstate.UserProtocol, User: &runstate.UserRecord{
		Message: llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Text: &llm.TextBlock{Text: text}}}},
	}})
	if jerr != nil {
		r.events.Warn(Warning{Kind: WarnJournalUser, Err: jerr})

		return runstate.ReasonError, jerr
	}

	r.followUpTaken = true
	r.maxIter = r.iter + r.cfg.LLM.Budget.MaxIterations

	return r.runTurn(ctx)
}

// terminalFor is the reason a run ends on err. An operator who was asked something
// and did not answer suspends the run rather than failing it: nothing was decided,
// the journal is intact, and the question can be put again on the next resume. Every
// other error ends the run in error, including a context that expired anywhere but a
// prompt.
//
// The distinction is what the operator sees. A suspended run prints where to resume
// and exits zero; an errored one prints neither, so treating an interrupt at an
// approval prompt as a failure would leave a resumable session with nothing naming
// it.
func terminalFor(err error) runstate.TerminalReason {
	if errors.Is(err, toolkit.ErrPromptAborted) {
		return runstate.ReasonSuspended
	}

	return runstate.ReasonError
}

// continuable reports whether a loop outcome is a boundary the operator can steer
// from with a follow-up. A completed turn is the normal case; a turn that hit the
// iteration cap is recoverable (interactive mode extends the budget on the next prompt),
// and so is a failed turn (a transient LLM/API error must not stall the chat, and the
// operator can always Ctrl-D to end). An abort is filtered separately by the run loop's
// ctx check; an exhausted token budget and a suspend end the session.
func continuable(reason runstate.TerminalReason) bool {
	switch reason {
	case runstate.ReasonCompleted, runstate.ReasonMaxIterations, runstate.ReasonError:
		return true
	default:
		return false
	}
}

// appendUserPrompt adds an interactive follow-up to the conversation. Normally the prior
// turn ended with an assistant message, so the follow-up becomes a new user turn. When the
// prior turn failed before replying, it leaves a dangling trailing user message; the
// follow-up is folded into that turn instead, so the roles keep alternating rather than
// sending two user messages in a row, which the API rejects.
func (r *runner) appendUserPrompt(text string) {
	block := llm.ContentBlock{Text: &llm.TextBlock{Text: text}}

	n := len(r.messages)
	if n > 0 && r.messages[n-1].Role == llm.RoleUser {
		r.messages[n-1].Content = append(r.messages[n-1].Content, block)

		// The fold changed a message the previous model call already carried, so the
		// next capture has to reach back far enough to include it. It clamps rather
		// than subtracting one: this folds into any trailing user message, and a
		// tool-results batch is one, so on the max-iterations path the mutated message
		// is not the one immediately before the baseline and stepping back by one
		// re-exports a message that already went out.
		r.contentFrom = min(r.contentFrom, n-1)

		return
	}

	r.messages = append(r.messages, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{block}})
}

// resetContext clears the conversation for a fresh start within the same run, keeping the
// system prompt and tools. The iteration budget is
// re-baselined to the current position so the cleared context gets a full turn's allowance
// from the next prompt rather than inheriting the budget the prior turns already spent.
// It is only reached for a non-checkpointed run; a journaled session is cleared by
// rotateSession instead, which the run loop defers until the next real prompt so the new
// session's meta never carries an empty prompt.
//
// The confirm gate's standing approvals go with the conversation. A grant is scoped to
// the conversation the operator gave it in, and the cleared context is a new one, so
// /clear costs one re-approval and means the same thing whether or not the run journals.
func (r *runner) resetContext() {
	r.messages = nil
	r.contentFrom = 0
	r.maxIter = r.iter
	if r.approvals != nil {
		r.approvals.clear()
	}
}

// rotateSession starts a fresh checkpoint session for a context reset and swaps the runner
// onto it, seeding the conversation with prompt as the new session's first turn. The new
// journal is created first so a failure leaves the current session untouched (the caller
// then runs the turn on in the existing session). On success the outgoing session is
// finalized as suspended, never completed, so the operator can return to it with --resume;
// it rests on an assistant boundary, the shape a chat normally suspends at. The per-session
// counters (seq, iteration, budget) reset for the new journal, while the run's cumulative
// stats keep climbing so the live totals reflect the whole sitting; the new session's own
// counters are derived from its journal on any later resume.
func (r *runner) rotateSession(prompt string) error {
	newJournal, newID, err := r.newSession(prompt)
	if err != nil {
		return err
	}

	prevID := r.sessionID

	// Finalize the outgoing session on its own journal before swapping. A failed terminal
	// write is not fatal: the journal still ends on an assistant turn, so the session stays
	// resumable, only unmarked; warn and proceed with the swap.
	terr := r.emit(runstate.Record{Protocol: runstate.TerminalProtocol, Terminal: &runstate.TerminalRecord{Reason: runstate.ReasonSuspended}})
	if terr != nil {
		r.events.Warn(Warning{Kind: WarnJournalTerminal, Err: terr})
	}
	closeJournal(r.journal, r.events)

	r.journal = newJournal
	r.sessionID = newID
	r.seq = newJournal.LastSeq()
	r.iter = 0
	r.maxIter = 0
	// The new journal is a new conversation, so its token allowance is whole. The stats
	// keep climbing to report the sitting, which is what this subtracts from.
	r.budgetBase = r.rawTokens()
	r.messages = []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Text: &llm.TextBlock{Text: prompt}}}}}
	r.contentFrom = 0
	// The new journal is a new conversation, separately resumable, and a grant belongs
	// to the one it was given in. Carrying them over would record a single in-the-moment
	// decision durably into two conversations, and into a third at the next reset.
	if r.approvals != nil {
		r.approvals.clear()
	}

	r.events.SessionRotated(prevID)

	return nil
}

// tokensSpent is what this conversation has processed: the uncached input, the output,
// and both cache tiers, which are weighted the same as uncached input because the cap
// counts tokens rather than money.
//
// Thinking is not added. llm.Usage documents it as a subset of the output rather than an
// addition to it, so summing it here would count reasoning twice.
func (r *runner) tokensSpent() int64 {
	return r.rawTokens() - r.budgetBase
}

// rawTokens is the sitting's running total, before the current conversation's base is
// taken off it. Nil stats read as zero so a runner assembled without counters, which
// several of this package's own tests do, is not a panic in a bound nobody set.
func (r *runner) rawTokens() int64 {
	if r.stats == nil {
		return 0
	}

	return r.stats.InTokens + r.stats.OutTokens + r.stats.CacheReadTokens + r.stats.CacheCreateTokens
}

// overBudget reports whether this conversation has processed its whole allowance. A
// MaxTokens of zero or less is no bound at all.
func (r *runner) overBudget() bool {
	return r.maxTokens > 0 && r.tokensSpent() >= r.maxTokens
}

// budgetError is what a run stopped by the token budget reports.
//
// It names both numbers because the cap is the operator's own setting and the total is
// the only evidence that it was reached, and it names the key so somebody who wants a
// larger allowance knows what to raise. It does not count iterations: the two places
// this fires are a turn that has run several and a turn that has not started.
func (r *runner) budgetError() error {
	return fmt.Errorf("this conversation has processed %d of its %d token budget (llm.budget.max_tokens)", r.tokensSpent(), r.maxTokens)
}

// recordText keeps the concatenated text of an assistant turn as the run's answer
// so far, so Result.Text reports the last turn that said anything. A turn with no
// text block (one that only calls tools) leaves the previous answer in place rather
// than clearing it, because a run stopped after such a turn should still report
// what it had reached.
func (r *runner) recordText(resp llm.Response) {
	var sb strings.Builder

	for _, block := range resp.Content {
		if block.Text == nil {
			continue
		}
		sb.WriteString(block.Text.Text)
	}

	if sb.Len() == 0 {
		return
	}

	r.finalText = sb.String()
}

func (r *runner) loop(ctx context.Context) (runstate.TerminalReason, error) {
	// On resume, finish the in-flight tool batch before proceeding so the
	// conversation reaches a coherent boundary. Its already-run tools are reused
	// from the journal; only the unanswered ones execute.
	if r.pending != nil {
		deferred, err := r.completePending(ctx)
		if err != nil {
			return terminalFor(err), err
		}
		if deferred {
			// Still waiting on somebody. Nothing was committed and no model call is
			// made, so a resume before the answer arrives costs a load and two records.
			return runstate.ReasonSuspended, nil
		}
		r.pending = nil
	}

	for r.iter < r.maxIter {
		// Poll for a graceful suspend only here, at a boundary where the
		// conversation is coherent and nothing is mid-flight. Checked before the index
		// is consumed so a suspend does not burn an iteration number.
		if r.suspendRequested != nil && r.suspendRequested() {
			return runstate.ReasonSuspended, nil
		}

		// i is this turn's index; advance the runner's counter now so it points at the
		// next index, keeping numbering monotonic across a terminal return and the
		// interactive re-entry that follows it.
		i := r.iter
		r.iter++

		req := llm.Request{
			Model:           r.cfg.LLM.Model,
			SystemBlocks:    r.system,
			Messages:        r.messages,
			Tools:           r.toolDefs,
			ToolSearch:      r.toolSearch,
			Thinking:        r.thinking,
			ReasoningEffort: r.cfg.ReasoningEffort(),
			MaxOutputTokens: r.maxOutputTokens,
			PromptCache:     r.promptCache,
			// Either way of being paced by a person: a run holding its own continuation
			// loop, or one turn of a conversation whose next turn is somebody's to send.
			Interactive: r.interactive || r.humanPaced,
		}

		if r.verbose {
			r.events.LLMRequest(util.LLMRequestSummary(r.messages))
		}

		// PreModelCall observes the request about to be sent. It carries counts, not the
		// live conversation, so a hook cannot alter what is sent; a returned error aborts
		// the run before the call is made or counted. It sits above the provider, so it
		// fires for an injected provider too, unlike an llm.Middleware.
		preErr := r.hooks.firePreModelCall(ctx, PreModelCallInfo{
			Iteration:    int(i),
			Model:        req.Model,
			MessageCount: len(r.messages),
			ToolCount:    len(r.toolDefs),
		})
		if preErr != nil {
			return runstate.ReasonError, fmt.Errorf("PreModelCall hook: %w", preErr)
		}

		chatInfo := telemetry.ChatInfo{
			Model:          req.Model,
			Provider:       r.providerName,
			ConversationID: r.sessionID,
			MaxTokens:      req.MaxOutputTokens,
			Iteration:      i,
			Messages:       len(r.messages),
			Tools:          len(r.toolDefs),
			// Built unconditionally and serialized only if capture is on, which is what
			// keeps this call site free of a branch on whether telemetry is configured.
			// The builder closes over the live conversation and is invoked before this
			// function returns, so the slice it reads is the one that was sent.
			Input: genai.InputMessages(r.messages, r.contentFrom),
		}
		callCtx, chatSpan := r.telemetry.StartChat(ctx, chatInfo)

		// Everything this call carried has now been accounted for, so the next one
		// starts from here: what it adds is the assistant turn appended below and any
		// tool results that answer it. Advanced before the call rather than after, so a
		// call that fails does not leave the baseline behind and re-export the whole
		// conversation on the retry.
		r.contentFrom = len(r.messages)

		resp, err := r.provider.Call(util.WithTraceIteration(callCtx, int(i)), req)
		// Finished on both paths, from one place, so the span cannot be left open by an
		// early return; the span covers the call alone, not the journaling below it.
		chatSpan.Finish(ctx, chatInfo, chatOutcome(resp, err))
		if err != nil {
			return runstate.ReasonError, fmt.Errorf("llm call: %w", err)
		}
		r.stats.LlmCalls++
		r.stats.InTokens += resp.Usage.In
		r.stats.OutTokens += resp.Usage.Out
		r.stats.CacheReadTokens += resp.Usage.CacheRead
		r.stats.CacheCreateTokens += resp.Usage.CacheCreate
		r.stats.ThinkingTokens += resp.Usage.Thinking

		// Append the assistant turn to the conversation. The neutral blocks preserve
		// any server-side tool_search blocks intact alongside text and tool_use.
		asst := llm.Message{Role: llm.RoleAssistant, Content: resp.Content}
		r.messages = append(r.messages, asst)

		// Journal the assistant turn before executing any tools, so a crash mid
		// batch resumes without re-paying for this LLM call.
		err = r.emit(runstate.Record{Protocol: runstate.AssistantProtocol, Assistant: &runstate.AssistantRecord{
			Iteration:         i,
			Message:           asst,
			StopReason:        string(resp.StopReason),
			InTokens:          resp.Usage.In,
			OutTokens:         resp.Usage.Out,
			CacheReadTokens:   resp.Usage.CacheRead,
			CacheCreateTokens: resp.Usage.CacheCreate,
			ThinkingTokens:    resp.Usage.Thinking,
		}})
		if err != nil {
			return runstate.ReasonError, err
		}

		var toolUses []llm.ToolUseBlock
		for _, block := range resp.Content {
			if block.ToolUse == nil {
				continue
			}
			toolUses = append(toolUses, *block.ToolUse)
		}

		// The turn is terminal when the model neither asked to run a tool nor paused a
		// long-running turn it intends to continue. A reply truncated at the output cap is
		// not terminal (it is an incomplete answer, handled as an error just below); the
		// StopMaxTokens guard is a no-op for every path past the truncation branch, where
		// the reason can no longer be StopMaxTokens, so it only makes Terminal correct for
		// the truncated reply PostModelCall observes.
		terminal := len(toolUses) == 0 &&
			resp.StopReason != llm.StopPauseTurn &&
			resp.StopReason != llm.StopMaxTokens

		// PostModelCall observes every reply, including one truncated at the output cap,
		// before the truncation branch decides the run's fate. The hook is handed a deep
		// copy of the reply and its own tool_use blocks, so a mutating hook cannot corrupt
		// the live conversation, which references resp.Content. A returned error aborts the
		// run (in a chat, the turn), but the assistant turn is already journaled above, so
		// the abort is not durable across resume: to reliably block a tool, use PreToolUse.
		postErr := r.hooks.firePostModelCall(ctx, int(i), *resp, terminal)
		if postErr != nil {
			return runstate.ReasonError, fmt.Errorf("PostModelCall hook: %w", postErr)
		}

		// A turn truncated at the output token cap may carry a partial tool_use whose
		// input is incomplete, so it must never be executed. Treat it as the run's end
		// with a clear cause rather than running malformed input or silently completing;
		// the caller surfaces the error and, in a chat, hands back to the operator.
		if resp.StopReason == llm.StopMaxTokens {
			r.recordText(*resp)
			r.events.Message(*resp, true)
			return runstate.ReasonError, fmt.Errorf("model reply truncated at the output token limit; the answer is incomplete")
		}

		// Text on a terminal turn is the answer; text on an intermediate turn is
		// narration. The caller decides where each goes.
		r.recordText(*resp)
		r.events.Message(*resp, terminal)

		// A terminal turn is the final answer; deliver it regardless of remaining
		// budget since no further spend follows.
		if terminal {
			if resp.StopReason == llm.StopRefusal {
				return runstate.ReasonError, fmt.Errorf("model refused to respond")
			}
			return runstate.ReasonCompleted, nil
		}

		// Stop before executing this turn's tools once the token budget is
		// exhausted, so an over-budget turn does not incur further tool spend or
		// side effects.
		//
		// A turn that ended on an answer has already returned above, which is deliberate:
		// those tokens are spent and the answer is in hand, so withholding it buys
		// nothing. What bounds a conversation of such turns is the check at the head of
		// the next one rather than this.
		if r.overBudget() {
			return runstate.ReasonBudget, r.budgetError()
		}

		if len(toolUses) > 0 {
			results := make([]llm.ContentBlock, 0, len(toolUses))
			deferred := false
			for _, use := range toolUses {
				result, dispatched, kind, herr := r.executeTool(ctx, use)
				if herr != nil {
					// A tool answering later is not a failure. The rest of the batch still
					// runs, because those results are journaled and then never re-run, so
					// running them now costs nothing a resume would not cost anyway.
					was, jerr := r.journalDeferral(use, herr)
					if jerr != nil {
						return runstate.ReasonError, jerr
					}
					if was {
						deferred = true
						continue
					}

					return terminalFor(herr), herr
				}
				err = r.emit(runstate.Record{Protocol: runstate.ToolResultProtocol, ToolResult: toolResultRecord(use.ID, result, kind, dispatched)})
				if err != nil {
					return runstate.ReasonError, err
				}
				err = r.journalGrants()
				if err != nil {
					return runstate.ReasonError, err
				}
				results = append(results, llm.ContentBlock{ToolResult: &result})
			}

			// The turn cannot be committed while a tool_use has no result, so nothing is
			// appended to the conversation and the run ends at a resumable boundary. The
			// journaled assistant turn and whichever results did land are what the next
			// resume folds.
			if deferred {
				return runstate.ReasonSuspended, nil
			}

			r.messages = append(r.messages, llm.Message{Role: llm.RoleUser, Content: results})
		}
	}

	return runstate.ReasonMaxIterations, fmt.Errorf("max iterations (%d) reached without a final answer", r.maxIter)
}

// completePending runs the not-yet-answered tools of a restored in-flight turn,
// reusing the results already journaled for the answered ones, then commits the
// assistant turn and its full result set to the conversation.
//
// A call the journal marks deferred is skipped rather than re-run, which is the
// whole difference between a call a crash interrupted and one somebody is still
// working on. One of the calls that does run may defer in its turn, which is the
// case the fold has never seen and which is journaled here exactly as the live batch
// journals it, so a third delivery does not run it a third time.
//
// The first return reports that a deferral is still outstanding. Nothing is committed
// then: a turn cannot be handed to the model while a tool_use has no result, so the
// caller ends the run at a resumable boundary instead.
func (r *runner) completePending(ctx context.Context) (bool, error) {
	p := r.pending

	// These tools run before the iteration loop, so their spans have no model call
	// above them in this trace: the call that asked for them lives in the previous
	// run's. The flag marks them so that shape reads as a resume rather than as tools
	// running unprompted.
	r.completingPending = true
	defer func() { r.completingPending = false }()

	results := make([]llm.ContentBlock, 0, len(p.Assistant.Content))
	for i := range p.Results {
		res := p.Results[i]
		results = append(results, llm.ContentBlock{ToolResult: &res})
	}

	// A deferral inherited from the journal is reported alongside anything this
	// process defers, so Result names what the run is waiting on however many resumes
	// ago the call was made.
	open := p.OpenDeferrals()
	for _, d := range open {
		r.deferred = append(r.deferred, DeferredCall{
			ToolUseID: d.ToolUseID,
			ToolName:  d.ToolName,
			Note:      d.Note,
			Handle:    d.Handle,
		})
	}
	deferred := len(open) > 0

	for _, block := range p.Assistant.Content {
		if block.ToolUse == nil {
			continue
		}
		id := block.ToolUse.ID
		if p.Answered[id] {
			continue
		}
		if _, waiting := p.Deferred[id]; waiting {
			continue
		}

		result, dispatched, kind, herr := r.executeTool(ctx, *block.ToolUse)
		if herr != nil {
			was, jerr := r.journalDeferral(*block.ToolUse, herr)
			if jerr != nil {
				return false, jerr
			}
			if was {
				deferred = true
				continue
			}

			return false, herr
		}
		err := r.emit(runstate.Record{Protocol: runstate.ToolResultProtocol, ToolResult: toolResultRecord(id, result, kind, dispatched)})
		if err != nil {
			return false, err
		}
		err = r.journalGrants()
		if err != nil {
			return false, err
		}
		results = append(results, llm.ContentBlock{ToolResult: &result})
	}

	if deferred {
		return true, nil
	}

	r.messages = append(r.messages, p.Assistant, llm.Message{Role: llm.RoleUser, Content: results})

	return false, nil
}

// journalDeferral records a tool call that will answer later and reports whether the
// error was one. Any other error is left to the caller, which ends the run on it.
//
// The record is what a resume reads to leave the call alone: without it a tool_use
// with no result is indistinguishable from one a crash interrupted, and the tool
// would be dispatched again for work it has already started.
func (r *runner) journalDeferral(use llm.ToolUseBlock, err error) (bool, error) {
	d, ok := toolkit.IsDeferred(err)
	if !ok {
		return false, nil
	}

	r.deferred = append(r.deferred, DeferredCall{
		ToolUseID: use.ID,
		ToolName:  use.Name,
		Note:      d.Note,
		Handle:    d.Handle,
	})

	jerr := r.emit(runstate.Record{Protocol: runstate.DeferredProtocol, Deferred: &runstate.DeferredRecord{
		ToolUseID: use.ID,
		ToolName:  use.Name,
		Note:      d.Note,
		Handle:    d.Handle,
	}})
	if jerr != nil {
		return true, jerr
	}

	// A deferred call is answered as far as an approval is concerned: the tool took the
	// work, and the resume never dispatches it again, so a grant held for a result that
	// is not coming would be lost with nobody left to re-ask.
	jerr = r.journalGrants()
	if jerr != nil {
		return true, jerr
	}

	r.events.Warn(Warning{Kind: WarnToolDeferred, Name: use.Name, Err: err})

	return true, nil
}

// executeTool dispatches a single tool call. It looks the tool up once in the
// unified registry, then runs it through the uniform Tool contract: kind-specific
// policy (argument validation, confirmation) is consulted through narrow capability
// interfaces, and the kind-specific call trace is built by a type switch, so a tool
// of any kind executes the same way. Around that pipeline it fires the PreToolUse and
// PostToolUse hooks, which may deny the call, rewrite the tool and its arguments, or
// replace the output. The second return reports whether the call was handed to whoever
// serves it rather than answered here, and the third which provider served it, both for
// the journal and stats; the fourth is non-nil when a hook aborted the run, which the
// caller surfaces on the ReasonError path, and when the tool will answer later, which
// the caller tells apart with toolkit.IsDeferred and suspends on. The kind is the
// effective one, so a call a hook redirected is accounted under the provider that
// actually served it, and it is reported for a call that never ran too, which is why
// the caller needs both to tell a bucket from a dispatch counter.
// The returns are named so the deferred span finish can read what the model was
// actually told, from one place rather than from each of the eight ways this ends.
func (r *runner) executeTool(ctx context.Context, use llm.ToolUseBlock) (result llm.ToolResultBlock, dispatched bool, kind toolkit.Kind, herr error) {
	tool, ok := r.tools[use.Name]

	// The span covers every way this can end, and there are eight of them, so it is
	// ended from one defer rather than at each return. outcome is filled in as the call
	// resolves; the zero Outcome would be a bug, so it starts at the one that applies
	// before anything else has run.
	toolName := ""
	if ok {
		toolName = use.Name
	}
	ctx, toolSpan := r.telemetry.StartTool(ctx, telemetry.ToolInfo{
		Name: toolName,
		// Only recorded when the name resolved to nothing, and only ever as a span
		// attribute: it is unvalidated model output.
		RequestedName: use.Name,
		CallID:        use.ID,
		Identity:      r.identity,
		Kind:          toolkit.KindUnknown.String(),
		ConfirmGated:  ok && confirmGated(tool, r.confirmTags),
		Datastore:     isKnowledgeTool(use.Name),
		Resumed:       r.completingPending,
	})
	outcome := telemetry.ToolOutcome{
		Outcome:   telemetry.ToolOutcomeUnknownTool,
		ArgKeys:   argumentKeys(use.Input),
		Arguments: genai.ToolArguments(use.Input),
	}
	defer func() {
		// What the model was told, whatever produced it. The four paths that answer a
		// call which never ran (an unknown tool, a policy denial, a missing argument,
		// an operator's refusal) all return something the model then acts on, so all
		// four are recorded: a trace showing the call and not the answer describes half
		// of what happened. fisk.tool.outcome on the same span says which it was.
		if result.Content != "" {
			outcome.Result = genai.ToolResult(result.Content)
		}

		// Which store served the call, resolved here rather than at any of the eight
		// returns because outcome.Name is the effective tool by the time this runs: a
		// hook that rewrote a memory call to something else is attributed to what
		// actually ran, and so is the reverse. An unknown tool has no name and so
		// matches nothing.
		if r.memoryTools[outcome.Name] {
			outcome.Memory = r.memory
		}

		toolSpan.Finish(ctx, outcome)
	}()

	if !ok {
		// An unknown tool never resolves to a kind or a PreToolUse snapshot, so it is
		// counted under KindUnknown and answered with an error result before any hook,
		// exactly as before.
		r.stats.ToolCalls++
		r.stats.CountToolKind(toolkit.KindUnknown)
		r.events.Warn(Warning{Kind: WarnUnknownTool, Name: use.Name})
		return llm.ToolResultBlock{ToolUseID: use.ID, Content: fmt.Sprintf("unknown tool %q", use.Name), IsError: true}, false, toolkit.KindUnknown, nil
	}

	outcome.Name = use.Name

	// The tool describes its own call and evaluates its confirm gate up front, before any
	// hook or mutation, so PreToolUse observes the call the model actually asked for and a
	// later rewrite can be gated on the union with this original gate. Describe must not
	// run the tool or mutate state, so calling it here is safe.
	origInfo := describeCall(tool, use.Input)
	origGated := confirmGated(tool, r.confirmTags)
	outcome.Kind = origInfo.Kind.String()

	// PreToolUse sees a copy of the model's raw arguments so a hook cannot mutate the
	// run's own buffer through the snapshot. A returned error aborts the run.
	pre, err := r.hooks.firePreToolUse(ctx, PreToolUseInfo{
		ToolName:     use.Name,
		ToolUseID:    use.ID,
		Input:        bytes.Clone(use.Input),
		Kind:         origInfo.Kind,
		ConfirmGated: origGated,
	})
	if err != nil {
		return llm.ToolResultBlock{}, false, origInfo.Kind, fmt.Errorf("PreToolUse hook: %w", err)
	}

	// A policy deny returns an error result the model can adapt to (unlike the
	// authoritative confirm-gate denial), answering use.ID so the batch stays well-formed
	// and a resume is consistent. It is still exactly one tool call, counted under the
	// original tool's kind since nothing ran and no rewrite was resolved. A rewrite is
	// ignored when Deny.
	if pre.Deny {
		outcome.Outcome = telemetry.ToolOutcomePolicyDenied
		r.stats.ToolCalls++
		r.stats.CountToolKind(origInfo.Kind)
		reason := pre.DenyReason
		if reason == "" {
			reason = "the tool call was denied by a policy hook"
		}
		return llm.ToolResultBlock{ToolUseID: use.ID, Content: reason, IsError: true}, false, origInfo.Kind, nil
	}

	// Resolve the effective tool and arguments once, applying any rewrite; the hook does
	// not re-fire on the rewritten call. A rewrite to an unregistered tool or to invalid
	// JSON aborts the run rather than dispatching a malformed call. RewriteTool may target
	// any registered tool, including one the model was never shown.
	effTool := tool
	effName := use.Name
	effInput := use.Input
	if pre.RewriteTool != "" {
		rt, ok := r.tools[pre.RewriteTool]
		if !ok {
			return llm.ToolResultBlock{}, false, origInfo.Kind, fmt.Errorf("PreToolUse hook redirected tool %q to unregistered tool %q", use.Name, pre.RewriteTool)
		}
		effTool = rt
		effName = pre.RewriteTool
	}
	if pre.RewriteInput != nil {
		if !json.Valid(pre.RewriteInput) {
			return llm.ToolResultBlock{}, false, origInfo.Kind, fmt.Errorf("PreToolUse hook rewrote %q arguments to invalid JSON", effName)
		}
		effInput = bytes.Clone(pre.RewriteInput)
	}
	effUse := llm.ToolUseBlock{ID: use.ID, Name: effName, Input: effInput}

	// The effective call describes itself again only when a rewrite changed the tool or
	// its arguments; otherwise the original describe still holds.
	effInfo := origInfo
	if pre.RewriteTool != "" || pre.RewriteInput != nil {
		effInfo = describeCall(effTool, effInput)
	}

	// Count once, now that the effective tool is resolved, so a rewritten call is
	// accounted under the tool that actually runs and the by-kind buckets still partition
	// tool_calls. RemoteToolCalls is incremented only on an actual remote dispatch below.
	r.stats.ToolCalls++
	r.stats.CountToolKind(effInfo.Kind)

	// From here the effective call is what runs, so the span reports it rather than what
	// the model originally asked for. The name is still registry-validated: a rewrite
	// can only target a registered tool.
	outcome.Name = effName
	outcome.Kind = effInfo.Kind.String()
	outcome.Rewritten = pre.RewriteTool != "" || pre.RewriteInput != nil
	if outcome.Rewritten {
		outcome.ArgKeys = argumentKeys(effInput)
		outcome.Arguments = genai.ToolArguments(effInput)
	}

	// fisk does not enforce a command's required flags or arguments: a missing one is
	// silently dropped, so the command runs incomplete and fails only on its own non-zero
	// exit. When the model omits a required parameter, reject the call before it runs and
	// return the missing parameters so the model can correct and retry. Only the tool
	// kinds that can check (local command tools) implement ArgumentValidator. This runs on
	// the effective call and before the confirm gate so the operator is never asked to
	// approve a structurally invalid call, and nothing executed, so it is reported as a
	// warning rather than a call-and-result pair.
	if v, ok := effTool.(toolkit.ArgumentValidator); ok {
		missing := v.MissingRequired(effInput)
		if len(missing) > 0 {
			outcome.Outcome = telemetry.ToolOutcomeMissingArguments
			r.events.Warn(Warning{Kind: WarnMissingRequired, Name: effName, Params: missing})
			return llm.ToolResultBlock{ToolUseID: use.ID, Content: v.MissingRequiredMessage(missing), IsError: true}, false, effInfo.Kind, nil
		}
	}

	// A confirm-tagged tool must be approved by the operator before it runs, gated on the
	// union of the original and effective tool: the call is confirmed if either is gated,
	// so a hook cannot strip a gate by redirecting a gated call to an ungated tool. The
	// operator is shown the effective command, which is what actually runs; a denial
	// returns an authoritative result to the model and the command is not run, so it emits
	// no trace or result.
	effGated := confirmGated(effTool, r.confirmTags)
	if origGated || effGated {
		// The wait is human time, so it is recorded as a duration and an event pair
		// rather than a child span: a span would dominate every duration chart and make
		// a four-minute tool call look like a four-minute tool.
		toolSpan.ConfirmRequested()
		askedAt := time.Now()
		allowed, reason, aerr := r.approveEffective(ctx, use.ID, tool, effTool, effName, effInput, effInfo)
		outcome.ConfirmWait = time.Since(askedAt)
		toolSpan.ConfirmAnswered()

		// The operator did not answer, so nothing is told to the model and nothing is
		// journaled: a refusal recorded here would be replayed as the operator's own on
		// every later resume. The run ends instead, leaving the call unanswered and the
		// session resumable, so the question can be put again.
		//
		// Neither ending is a refusal, so neither is counted as one. An unanswered
		// question is asked again on the next resume, and a prompter that answers later
		// reports a deferral, whose call the caller journals as deferred before suspending
		// the way a deferring tool does. An operator grouping tool calls by outcome would
		// otherwise read both as commands somebody refused.
		if aerr != nil {
			outcome.Outcome = telemetry.ToolOutcomeUnanswered
			_, deferred := toolkit.IsDeferred(aerr)
			if deferred {
				outcome.Outcome = telemetry.ToolOutcomeDeferred
			}

			return llm.ToolResultBlock{}, false, effInfo.Kind, aerr
		}

		if !allowed {
			outcome.Outcome = telemetry.ToolOutcomeConfirmDenied
			return util.ConfirmDeniedResult(use.ID, reason), false, effInfo.Kind, nil
		}
	}

	// The call trace shape and the execution dependencies are kind-specific; the result
	// trace and the ExecuteUse call are uniform. A call line is emitted for every tool
	// that runs, so its result always has a visible command above it.
	deps := r.traceCall(effUse, effInfo)

	// Every path above answers the call without it leaving this process, so this is
	// where a call becomes one that happened and every return below reports it as
	// dispatched. The two first-class dispatch counters are incremented here rather than
	// beside CountToolKind, which is what makes them count calls that were made while
	// the buckets count calls the model asked for. See util.RunStats.ToolCallsByKind.
	//
	// Both are keyed on the provider kind and never on the presentation or the agent
	// name: presentation is the visibility axis, and other providers present the same way
	// a remote call does while being accounted under their own kind.
	dispatched = true
	switch effInfo.Kind {
	case toolkit.KindRemote:
		r.stats.RemoteToolCalls++
	case toolkit.KindMCP:
		r.stats.MCPToolCalls++
	}

	remote := effInfo.Kind == toolkit.KindRemote
	outcome.Remote = remote
	if remote {
		// CallInfo.Agent names whoever serves the call, and other providers present
		// like a remote one, so the journal takes the name only for an a2a peer.
		outcome.RemoteAgent = effInfo.Agent
	}

	// Last check before an effect this process cannot take back. A journaled run on a
	// shared store can be taken over between tools, and the append that would tell us
	// so comes after the tool has already run. Asking here moves that discovery in
	// front of the effect. It does not cover the tool already in flight when a takeover
	// happens, which is the residue this cannot reach.
	heldErr := r.checkStillHeld()
	if heldErr != nil {
		return llm.ToolResultBlock{}, dispatched, effInfo.Kind, heldErr
	}

	// The bound goes in a context of its own rather than into ctx: ctx here is the tool
	// span's, read by the deferred Finish above and passed to PostToolUse below, and an
	// expired one there would end the span badly and abort the whole run on a hook that
	// objects. Nothing after ExecuteUse uses execCtx.
	execCtx, cancelExec := r.toolContext(ctx, effInfo)
	result, exec, unansweredErr := toolkit.ExecuteUse(effTool, execCtx, effUse, deps)
	timedOut := cancelExec()

	// A call that produced no result has nothing to trace, hand to a hook or journal, so
	// it ends here and the caller ends the run. PostToolUse does not fire: there is
	// nothing for it to observe and nothing for it to replace.
	//
	// The two endings differ in what happens next, which is why they are classified
	// apart. A deferred call is marked in the journal and never dispatched again, its
	// answer arriving later. A question the operator never answered leaves the call
	// unanswered, so the resume runs it again and asks again.
	if unansweredErr != nil {
		outcome.Outcome = telemetry.ToolOutcomeUnanswered
		_, deferred := toolkit.IsDeferred(unansweredErr)
		if deferred {
			outcome.Outcome = telemetry.ToolOutcomeDeferred
		}

		return llm.ToolResultBlock{}, dispatched, effInfo.Kind, unansweredErr
	}

	// A call the bound stopped is already an error result carrying whatever the tool
	// said about its context ending. Replacing the text here, before PostToolUse, keeps
	// the hook, the journal, the events sink and the model on one story and leaves a
	// hook's Replace authoritative over it.
	if timedOut && result.IsError && ctx.Err() == nil {
		result.Content = toolTimeoutMessage(effName, r.toolTimeout)
		r.events.Warn(Warning{Kind: WarnToolTimeout, Name: effName, Err: fmt.Errorf("did not finish within %s", r.toolTimeout)})
	}

	// Recorded before the hooks below, and deliberately not disturbed by them: a
	// PostToolUse Replace changes what the model sees, while this describes what
	// actually ran. exec is nil for every tool that ran no command, which leaves the
	// attribute absent rather than reporting a zero exit for a command that never was.
	if exec != nil {
		outcome.ExitCode = &exec.ExitCode
	}

	// PostToolUse observes the result before it is traced and journaled and may replace
	// what the model sees. Info.Output is the tool's own output; a Replace substitutes
	// Result.Output/IsError, keeping use.ID so the result still answers the call. A
	// returned error aborts the run.
	post, err := r.hooks.firePostToolUse(ctx, PostToolUseInfo{
		ToolName:  effName,
		ToolUseID: use.ID,
		Input:     bytes.Clone(effInput),
		Kind:      effInfo.Kind,
		Output:    result.Content,
		IsError:   result.IsError,
	})
	if err != nil {
		return llm.ToolResultBlock{}, false, effInfo.Kind, fmt.Errorf("PostToolUse hook: %w", err)
	}
	if post.Replace {
		result = llm.ToolResultBlock{ToolUseID: use.ID, Content: post.Output, IsError: post.IsError}
	}

	// The call ran, so the outcome is decided by what it returned. A non-zero command
	// exit is deliberately NOT an error here: it round-trips as an ordinary result
	// envelope, so flagging it would mark every routine "grep found nothing" as a
	// failure and make the error rate useless.
	outcome.Outcome = telemetry.ToolOutcomeExecuted
	if result.IsError {
		outcome.Outcome = telemetry.ToolOutcomeError
		outcome.Failed = true
	}

	r.events.ToolResult(toolResultTrace(effInfo.Present, effInfo.Kind, result))
	return result, dispatched, effInfo.Kind, nil
}

// checkStillHeld reports whether this run may go on writing to its journal, so a run
// that was taken over stops before its next irreversible step rather than at its next
// append, which is one tool too late.
//
// An un-journaled run has nothing to lose and nothing to ask, so it always may. A
// journaled one asks the journal, and an error of any kind ends the run: losing the
// run and being unable to tell are different facts, but neither is a state in which to
// keep running work whose results may have nowhere to go.
func (r *runner) checkStillHeld() error {
	if r.journal == nil {
		return nil
	}

	err := r.journal.CheckHeld()
	if err != nil {
		return fmt.Errorf("this run is no longer safe to continue: %w", err)
	}

	return nil
}

// toolContext bounds one tool call. It returns the context to execute on and a
// release function that must be called exactly once, after the call returns, and
// which reports whether the bound expired. The context must not be used after it.
//
// The bound is cooperative: it cancels a context and cannot unblock a handler that
// never observes one. A command tool is stopped for real, since exec.CommandContext
// kills the child and its process group; an in-process tool stops when its handler
// says so.
//
// Two calls run on the run's own context instead. One whose configuration set 0s, which
// is how an operator asks for no bound, and one a person paces, where the bound would
// cancel the operator's question rather than a runaway.
func (r *runner) toolContext(ctx context.Context, info toolkit.CallInfo) (context.Context, func() bool) {
	if r.toolTimeout <= 0 || info.OperatorPaced {
		return ctx, func() bool { return false }
	}

	execCtx, cancel := context.WithTimeout(ctx, r.toolTimeout)

	return execCtx, func() bool {
		// Read before canceling, which would report Canceled over the deadline that
		// actually fired.
		expired := errors.Is(execCtx.Err(), context.DeadlineExceeded)
		cancel()

		return expired
	}
}

// toolTimeoutMessage is what the model is told about a call the bound stopped. It
// names no configuration key on purpose: the duration may be the operator's or a
// default the host applied when they set none, and the two are indistinguishable
// here. The operator learns which from the advisory that accompanies it.
func toolTimeoutMessage(name string, d time.Duration) string {
	return fmt.Sprintf("tool %q did not finish within %s and was stopped by the agent's tool timeout; it returned no output and any work it started may be incomplete. Do not repeat the same call unchanged.", name, d)
}

// argumentKeys returns the top-level key names of a tool call's arguments, never their
// values. It is the no-content middle tier: it turns "it called stream_edit" into
// "with stream, max_age, retention", which is usually the question, without opting into
// content capture. Arguments that are not a JSON object yield nothing.
func argumentKeys(input json.RawMessage) []string {
	if len(input) == 0 {
		return nil
	}

	var fields map[string]json.RawMessage
	err := json.Unmarshal(input, &fields)
	if err != nil {
		return nil
	}

	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	return keys
}

// isKnowledgeTool reports whether a tool name is one of the read-only knowledge tools,
// which the conventions type as a datastore rather than a function. Everything else
// here is a function, a remote agent's tool included: that is client-side from the
// model's point of view.
func isKnowledgeTool(name string) bool {
	return name == config.KnowledgeSearchToolName || name == config.KnowledgeEnumerateToolName
}

// confirmGated reports whether a tool is confirm-gated for the run's tags: it opts into
// confirmation through toolkit.Confirmable and its tags trigger the gate. Only local
// command tools are Confirmable; every other kind yields false.
func confirmGated(tool toolkit.Tool, tags []string) bool {
	c, ok := tool.(toolkit.Confirmable)
	return ok && c.NeedsConfirm(tags)
}

// approveEffective drives the confirm gate for a call on the union of its original and
// effective tool. It presents the effective command, which is what actually runs,
// preferring the effective tool's own Confirmable rendering; when the effective tool is
// not Confirmable (a rewrite to a built-in or remote tool) it falls back to the effective
// tool's describe line and the original tool's gating tag, so an original gate is enforced
// rather than silently dropped. The line is sanitized upstream because its argument values
// come from the model and must not be able to spoof the operator's terminal.
func (r *runner) approveEffective(ctx context.Context, useID string, orig, eff toolkit.Tool, effName string, effInput json.RawMessage, effInfo toolkit.CallInfo) (bool, string, error) {
	commandPath := effName
	display := effInfo.Display
	var tag string

	if c, ok := eff.(toolkit.Confirmable); ok {
		commandPath = c.Command()
		display = c.TraceLine(effInput)
		tag = c.ConfirmTrigger(r.confirmTags)
	}

	// The gate is owed to the original tool when the effective tool supplies no trigger
	// (not Confirmable, or Confirmable but its own tags do not gate): name the original's
	// trigger so the operator sees why approval is asked.
	if tag == "" {
		if c, ok := orig.(toolkit.Confirmable); ok {
			tag = c.ConfirmTrigger(r.confirmTags)
		}
	}

	if display == "" {
		display = effName
	}

	return r.gate.Approve(ctx, useID, effName, commandPath, display, tag)
}

// describeCall asks a tool to describe one call, from the CallInfo the runner uses
// for both accounting and tracing. A tool that does not implement toolkit.Describer
// yields the zero CallInfo, so it is accounted under toolkit.KindUnknown and traced
// by name alone, with no dependencies and not as a remote call: the safe default for
// a tool of an unforeseen kind.
func describeCall(tool toolkit.Tool, input json.RawMessage) toolkit.CallInfo {
	d, ok := tool.(toolkit.Describer)
	if !ok {
		return toolkit.CallInfo{}
	}

	return d.Describe(input)
}

// traceCall emits the ToolCall trace for a dispatched call from the CallInfo the
// runner already obtained, and returns the execution dependencies the call's kind
// needs. The tool described its own call rather than the runner switching on its
// concrete type, so the presentation and dependency needs
// travel with the tool on info.Present: a built-in shows its own call line (a
// human-in-the-loop tool is distracting to name and is shown only under verbose
// downstream, a memory or knowledge tool is traced like a command); a remote tool
// names the agent it runs on; a command tool carries the full call line and a short
// form with long argument values elided, so a width-aware surface can fall back to
// the short one only when the full line would overflow.
func (r *runner) traceCall(use llm.ToolUseBlock, info toolkit.CallInfo) toolkit.ExecDeps {
	r.events.ToolCall(ToolTrace{
		ID:           use.ID,
		Name:         use.Name,
		Display:      info.Display,
		DisplayShort: info.DisplayShort,
		Input:        use.Input,
		Agent:        info.Agent,
		Present:      info.Present,
		ProviderKind: info.Kind,
	})

	// A kind receives only the dependencies it asked for: a command tool the per-run
	// working directory, a built-in the operator prompter (and a working directory it
	// ignores), a remote tool neither.
	var deps toolkit.ExecDeps
	if info.NeedsPrompter {
		deps.Prompter = r.prompter
	}
	if info.NeedsWorkDir {
		deps.WorkDir = r.toolWorkDir
	}

	return deps
}

// toolResultRecord builds the journal entry for one answered tool call. Kind is
// recorded for every call, including one that never ran, and Dispatched only for a call
// that was handed to whoever serves it, so a fold recovers both the per-kind buckets and
// the remote and MCP totals. Remote is derived from the two and still written, so a
// build that predates them reads an a2a dispatch out of a journal this one wrote. See
// runstate.Counters.
func toolResultRecord(id string, result llm.ToolResultBlock, kind toolkit.Kind, dispatched bool) *runstate.ToolResultRecord {
	return &runstate.ToolResultRecord{
		ToolUseID:  id,
		Result:     result,
		Remote:     dispatched && kind == toolkit.KindRemote,
		Kind:       kind.String(),
		Dispatched: dispatched,
	}
}

// toolResultTrace extracts the display fields from a tool result: its presentation
// (carried through so the result renderer suppresses it by the same rule as its
// call), its provider kind for the log token, its text content, and whether the tool
// reported a failure.
func toolResultTrace(present toolkit.Presentation, provider toolkit.Kind, result llm.ToolResultBlock) ToolResultTrace {
	return ToolResultTrace{CallID: result.ToolUseID, Present: present, ProviderKind: provider, IsError: result.IsError, Output: result.Content}
}
