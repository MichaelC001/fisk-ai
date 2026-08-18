//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package runstate

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/choria-io/fisk-ai/internal/llm"
)

var (
	// ErrEmpty is returned when folding an empty record set.
	ErrEmpty = errors.New("no records")
	// ErrNoMeta is returned when the first record is not a Meta record.
	ErrNoMeta = errors.New("first record is not meta")
	// ErrCorrupt is returned when the record sequence is internally inconsistent.
	ErrCorrupt = errors.New("corrupt record sequence")
	// ErrVersion is returned when a snapshot was written by an unsupported schema
	// version.
	ErrVersion = errors.New("unsupported snapshot version")
)

// Counters are the run's cumulative statistics, derived from the journal rather
// than stored separately so they can never drift from the recorded events.
type Counters struct {
	LlmCalls          int64
	ToolCalls         int64
	RemoteToolCalls   int64
	InTokens          int64
	OutTokens         int64
	CacheReadTokens   int64
	CacheCreateTokens int64
	ThinkingTokens    int64
}

// PendingTurn is an assistant turn whose tool calls are not yet all answered:
// the run was journaled mid-batch. On resume the runner runs only the tools in
// the turn that have no result yet, reusing Results for those already done, then
// commits the turn and continues.
type PendingTurn struct {
	Assistant  llm.Message
	Iteration  int64
	StopReason string
	// Results holds the tool results gathered so far, in journal order.
	Results []llm.ToolResultBlock
	// Answered marks which tool_use ids already have a result.
	Answered map[string]bool
	// Deferred holds the calls whose answer arrives later, keyed by tool_use id. A
	// deferred call is unanswered and must not be re-run: the tool already started
	// the work, so running it again would start it twice. An id present here and in
	// Answered has had its answer supplied, and Answered is what decides.
	Deferred map[string]DeferredRecord
}

// OpenDeferrals returns the deferred calls that have no result yet, in tool_use id
// order. It is empty for a turn that is merely mid-batch, so a caller uses it to
// tell a run waiting on somebody from one a crash interrupted.
func (p *PendingTurn) OpenDeferrals() []DeferredRecord {
	var out []DeferredRecord
	for id, d := range p.Deferred {
		if p.Answered[id] {
			continue
		}
		out = append(out, d)
	}

	slices.SortFunc(out, func(a, b DeferredRecord) int { return strings.Compare(a.ToolUseID, b.ToolUseID) })

	return out
}

// RunState is the folded, resumable state of a run. It is deliberately free of
// infrastructure and of this process's credentials (clients, API keys, config):
// those are rebuilt from configuration on resume. ConversationToken is the
// exception, being the caller's credential rather than the process's.
type RunState struct {
	Version     int
	RunID       string
	Fingerprint Fingerprint
	Prompt      string
	// Interactive marks a chat run, restored from the Meta record, so a resume
	// reopens the input bar rather than making a fresh LLM call at a completed turn.
	Interactive bool
	// ConversationToken is the caller's handle for this conversation and Caller is
	// what the channel knew about who asked, both restored from the Meta record. See
	// MetaRecord for what each is worth; neither is read by the loop.
	ConversationToken string
	Caller            string

	// Messages is the committed, coherent conversation prefix: it always ends on
	// a boundary the API would accept (an initial user prompt, or an assistant
	// turn with all tool_use answered by a following user results turn). An
	// in-flight turn lives in Pending, not here.
	Messages []llm.Message
	Counters Counters

	// NextIteration is the loop index to resume at.
	NextIteration int64
	// Pending is the in-flight assistant turn, or nil when the last turn is
	// complete.
	Pending *PendingTurn
	// LastStopReason is the stop reason of the final assistant turn, used to
	// detect a resume at a paused-turn boundary whose server-side state may have
	// expired.
	LastStopReason string
	// Approvals holds the tools the operator granted a standing approval, in journal
	// order. A resume seeds the confirm gate from it so an approval already given is
	// not asked for again. It grants nothing on its own: the gate honors one only
	// where an operator is reachable to have been asked in the first place.
	Approvals []string
	// CallApprovals holds the one-shot approvals that are still live, in journal
	// order: an operator answered "allow once" for a call the run had not dispatched.
	// A resume seeds the gate from it, and the gate spends one when it dispatches the
	// call it names.
	//
	// A terminal record clears it, so an approval the run never reached is spent
	// rather than carried into a later resume. It grants nothing on its own, for the
	// reason Approvals states.
	CallApprovals []CallApprovalRecord
	// Terminal is set when a Terminal record was journaled.
	Terminal *TerminalRecord
}

// Completed reports whether the last run in this journal ended by answering.
//
// It is not a claim that the conversation is over. A resume with nothing to add is
// refused on it, since the model would be called on a finished conversation with no
// new input, and a resume carrying a new user turn continues past it.
func (s *RunState) Completed() bool {
	return s.Terminal != nil && s.Terminal.Reason == ReasonCompleted
}

// Fold reconstructs a RunState from an ordered record set. The records must begin
// with a Meta record and carry strictly increasing seqs. Fold is pure: it does
// no IO and derives counters and the resume position entirely from the records.
func Fold(records []Record) (*RunState, error) {
	if len(records) == 0 {
		return nil, ErrEmpty
	}

	first := records[0]
	if first.Protocol != MetaProtocol || first.Meta == nil {
		return nil, ErrNoMeta
	}
	meta := first.Meta
	// Only the current version is accepted, in either direction. A newer snapshot may
	// carry record shapes this build does not understand, and an older one predates the
	// provider-neutral record format, which does not round-trip through these records.
	// See Version in record.go for the policy.
	if meta.Version != Version {
		return nil, fmt.Errorf("%w: snapshot version %d, supported %d", ErrVersion, meta.Version, Version)
	}

	rs := &RunState{
		Version:           meta.Version,
		RunID:             meta.RunID,
		Fingerprint:       meta.Fingerprint,
		Prompt:            meta.Prompt,
		Interactive:       meta.Interactive,
		ConversationToken: meta.ConversationToken,
		Caller:            meta.Caller,
		Messages:          []llm.Message{userTextMessage(meta.Prompt)},
	}

	// cur* accumulate the assistant turn currently being answered. The journal
	// already stores the neutral model, so the folded RunState carries it verbatim
	// with no conversion.
	var (
		cur         *AssistantRecord
		curMsg      llm.Message
		curResults  []llm.ToolResultBlock
		curAnswer   map[string]bool
		curDeferred map[string]DeferredRecord
	)

	// lastIter/lastStop track the resume position (NextIteration) and the final stop
	// reason independent of the last record's kind: an interactive User record commits
	// and clears cur, so deriving these from cur alone would lose them whenever the
	// journal ends on a follow-up (submit follow-up, LLM call fails, operator leaves).
	var (
		lastIter     int64
		lastStop     string
		sawAssistant bool
	)

	// commit appends the current assistant turn and, if it produced any, its
	// tool results as a single following user message. Called when a new
	// assistant turn begins, since the loop only makes another LLM call once the
	// previous turn's tools are all answered.
	commit := func() {
		rs.Messages = append(rs.Messages, curMsg)
		if len(curResults) > 0 {
			rs.Messages = append(rs.Messages, userResultsMessage(curResults))
		}
	}

	lastSeq := first.Seq
	for _, r := range records[1:] {
		if r.Seq <= lastSeq {
			return nil, fmt.Errorf("%w: seq %d not increasing after %d", ErrCorrupt, r.Seq, lastSeq)
		}
		lastSeq = r.Seq

		switch r.Protocol {
		case MetaProtocol:
			return nil, fmt.Errorf("%w: duplicate meta at seq %d", ErrCorrupt, r.Seq)

		case AssistantProtocol:
			if r.Assistant == nil {
				return nil, fmt.Errorf("%w: assistant record with no payload at seq %d", ErrCorrupt, r.Seq)
			}
			if cur != nil {
				commit()
			}
			cur = r.Assistant
			curMsg = r.Assistant.Message
			curResults = nil
			curAnswer = map[string]bool{}
			curDeferred = map[string]DeferredRecord{}
			lastIter = r.Assistant.Iteration
			lastStop = r.Assistant.StopReason
			sawAssistant = true
			rs.Counters.LlmCalls++
			rs.Counters.InTokens += r.Assistant.InTokens
			rs.Counters.OutTokens += r.Assistant.OutTokens
			rs.Counters.CacheReadTokens += r.Assistant.CacheReadTokens
			rs.Counters.CacheCreateTokens += r.Assistant.CacheCreateTokens
			rs.Counters.ThinkingTokens += r.Assistant.ThinkingTokens

		case UserProtocol:
			if r.User == nil {
				return nil, fmt.Errorf("%w: user record with no payload at seq %d", ErrCorrupt, r.Seq)
			}
			// A free-standing follow-up closes any open assistant turn (its tools are
			// all answered by the time the operator was handed the input bar), then
			// appends as a user message. It merges into a trailing user message when one
			// is present, which mirrors the runtime appendUserPrompt fold (a follow-up
			// after an errored turn) and merges consecutive User records into one.
			if cur != nil {
				commit()
				cur = nil
				curResults = nil
				curAnswer = nil
				curDeferred = nil
			}
			appendOrMergeUser(rs, r.User.Message)

		case ToolResultProtocol:
			if r.ToolResult == nil {
				return nil, fmt.Errorf("%w: tool_result record with no payload at seq %d", ErrCorrupt, r.Seq)
			}
			if cur == nil {
				return nil, fmt.Errorf("%w: tool_result before any assistant at seq %d", ErrCorrupt, r.Seq)
			}
			curResults = append(curResults, r.ToolResult.Result)
			curAnswer[r.ToolResult.ToolUseID] = true
			rs.Counters.ToolCalls++
			if r.ToolResult.Remote {
				rs.Counters.RemoteToolCalls++
			}

		case DeferredProtocol:
			if r.Deferred == nil {
				return nil, fmt.Errorf("%w: deferred record with no payload at seq %d", ErrCorrupt, r.Seq)
			}
			if cur == nil {
				return nil, fmt.Errorf("%w: deferred before any assistant at seq %d", ErrCorrupt, r.Seq)
			}
			// The call stays unanswered, which is what keeps the turn pending until the
			// result arrives. Nothing is counted: a deferred call has produced no result,
			// and counting it here would double when the answer lands as a tool_result.
			curDeferred[r.Deferred.ToolUseID] = *r.Deferred

		case DecisionProtocol:
			if r.Decision == nil {
				return nil, fmt.Errorf("%w: decision record with no payload at seq %d", ErrCorrupt, r.Seq)
			}
			// Inert against the turn for the same reason a claim is: a grant is recorded
			// after the result of the call that triggered it, so it lands inside a batch
			// whenever the operator approves one call of several.
			rs.Approvals = append(rs.Approvals, r.Decision.Tool)

		case CallApprovalProtocol:
			if r.CallApproval == nil {
				return nil, fmt.Errorf("%w: call approval record with no payload at seq %d", ErrCorrupt, r.Seq)
			}
			// Inert against the turn, as a decision is: the call it names is unanswered,
			// so the batch it belongs to is still open.
			rs.CallApprovals = append(rs.CallApprovals, *r.CallApproval)

		case TerminalProtocol:
			if r.Terminal == nil {
				return nil, fmt.Errorf("%w: terminal record with no payload at seq %d", ErrCorrupt, r.Seq)
			}
			rs.Terminal = r.Terminal
			// A one-shot approval the run did not reach is spent here rather than carried
			// into the next resume, where a later question going unanswered would leave it
			// authorizing a dispatch nobody approved. A standing grant is not: it covers
			// the conversation and outliving a suspend is the whole of what it is for.
			rs.CallApprovals = nil

		case ClaimProtocol:
			if r.Claim == nil {
				return nil, fmt.Errorf("%w: claim record with no payload at seq %d", ErrCorrupt, r.Seq)
			}
			// Deliberately inert, and load bearing that it stays so. A claim is written
			// on resume, so it lands between an assistant turn and the tool results that
			// answer it whenever a run is taken over mid-batch. Touching cur, curResults
			// or curAnswer here would commit that turn early and destroy the Pending
			// batch the resume exists to finish. It is also not folded into RunState:
			// nothing releases a claim, so its presence is not evidence a run is held.

		default:
			// A record the writer marked skippable is passed over, which is how a journal
			// written by a newer build folds here: the writer declared that a reader
			// without the record behaves more conservatively. Everything else is corrupt,
			// since a damaged protocol id on a record this build does need would otherwise
			// drop a turn or make a completed run resumable.
			if r.Optional {
				continue
			}

			return nil, fmt.Errorf("%w: unknown record protocol %q at seq %d", ErrCorrupt, r.Protocol, r.Seq)
		}
	}

	// Resolve the trailing assistant turn: either it is complete (all tool_use
	// answered, or no tool_use at all) and gets committed, or it is in-flight and
	// becomes Pending. A trailing User record leaves cur nil (it was committed above),
	// so there is nothing pending and the resume position comes from the tracked
	// last-assistant values below rather than from cur.
	if cur != nil {
		unanswered := unansweredToolUses(curMsg, curAnswer)
		if len(unanswered) == 0 {
			commit()
			rs.Pending = nil
		} else {
			rs.Pending = &PendingTurn{
				Assistant:  curMsg,
				Iteration:  cur.Iteration,
				StopReason: cur.StopReason,
				Results:    curResults,
				Answered:   curAnswer,
				Deferred:   curDeferred,
			}
		}
	}

	// NextIteration and LastStopReason describe where an assistant turn would resume,
	// derived from the last assistant record regardless of whether a User record
	// followed it, so a journal ending on a follow-up still resumes at the right index.
	if sawAssistant {
		rs.NextIteration = lastIter + 1
		rs.LastStopReason = lastStop
	}

	return rs, nil
}

// appendOrMergeUser appends a user message to the folded conversation, merging its
// content into a trailing user message when the last message is already a user turn.
// This keeps the roles alternating (the API rejects two user messages in a row) and
// reconstructs both the runtime appendUserPrompt fold and consecutive User records.
func appendOrMergeUser(rs *RunState, msg llm.Message) {
	n := len(rs.Messages)
	if n > 0 && rs.Messages[n-1].Role == llm.RoleUser {
		rs.Messages[n-1].Content = append(rs.Messages[n-1].Content, msg.Content...)
		return
	}

	rs.Messages = append(rs.Messages, msg)
}

// unansweredToolUses returns the ids of tool_use blocks in msg that have no result
// in answered.
func unansweredToolUses(msg llm.Message, answered map[string]bool) []string {
	var out []string
	for _, block := range msg.Content {
		if block.ToolUse == nil {
			continue
		}
		id := block.ToolUse.ID
		if !answered[id] {
			out = append(out, id)
		}
	}
	return out
}

// userTextMessage builds a user turn carrying a single text block, the shape of an
// operator prompt.
func userTextMessage(text string) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Text: &llm.TextBlock{Text: text}}}}
}

// userResultsMessage builds the synthetic user turn that answers an assistant turn's
// tool calls, one tool_result block per result in journal order.
func userResultsMessage(results []llm.ToolResultBlock) llm.Message {
	content := make([]llm.ContentBlock, len(results))
	for i := range results {
		r := results[i]
		content[i] = llm.ContentBlock{ToolResult: &r}
	}
	return llm.Message{Role: llm.RoleUser, Content: content}
}
