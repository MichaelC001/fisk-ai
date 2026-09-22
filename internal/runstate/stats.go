//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package runstate

import (
	"slices"
	"strings"
	"time"
)

// Statistics is what a run cost and where its time went, as Stats derives it from the
// run's records.
type Statistics struct {
	// RunID, Agent, Model and Caller frame the run, read from its Meta record.
	RunID  string `json:"run_id"`
	Agent  string `json:"agent,omitempty"`
	Model  string `json:"model,omitempty"`
	Caller string `json:"caller,omitempty"`
	// Created is MetaRecord.Created, the time RunInfo.Created reports, or the Meta
	// record's own time where that field is zero.
	Created time.Time `json:"created,omitzero"`
	// Updated is the time of the last record that carries one. It is zero for a journal
	// written before Record.Time existed.
	Updated time.Time `json:"updated,omitzero"`
	// Terminal is the reason the run ended and Ended the time of the record that says
	// so, both as RunState.Ending answers: empty and zero for a run with no terminal
	// record and for one that journaled anything after its last terminal record. Ended
	// is also zero for a terminal record written before Record.Time existed.
	Terminal TerminalReason `json:"terminal,omitempty"`
	Ended    time.Time      `json:"ended,omitzero"`
	// Iterations is the number of model responses across the whole run. MaxIterations
	// and MaxTokens are the bounds the run was started with, zero where it had none.
	// MaxIterations caps each turn on its own, so a conversation of several turns can
	// hold more responses than it.
	Iterations    int64 `json:"iterations"`
	MaxIterations int64 `json:"max_iterations,omitempty"`
	MaxTokens     int64 `json:"max_tokens,omitempty"`
	// Tokens is the sum of Responses.
	Tokens Tokens `json:"tokens"`
	// Responses holds one entry per model response, which is one AssistantRecord, in
	// journal order.
	Responses []ResponseTokens `json:"responses"`
	// Tools holds a row per tool the run called, ordered by tool name.
	Tools []ToolStats `json:"tools"`
}

// WallClock returns the time from the run's creation to its last dated record. It
// reports false when either end carries no time.
func (s Statistics) WallClock() (time.Duration, bool) {
	if s.Created.IsZero() || s.Updated.IsZero() || s.Updated.Before(s.Created) {
		return 0, false
	}

	return s.Updated.Sub(s.Created), true
}

// Tokens is a token count split into the tiers a provider reports.
type Tokens struct {
	// In is the uncached input. CacheRead and CacheCreate are the two prompt-cache input
	// tiers, counted apart from it.
	In          int64 `json:"in_tokens"`
	Out         int64 `json:"out_tokens"`
	CacheRead   int64 `json:"cache_read_tokens"`
	CacheCreate int64 `json:"cache_create_tokens"`
	// Thinking is the part of Out the model spent reasoning, not a count on top of it.
	Thinking int64 `json:"thinking_tokens"`
}

// Total returns the tokens a token budget counts: the input, the output and both cache
// tiers, weighed the same. Thinking is part of Out and is not added again.
func (t Tokens) Total() int64 {
	return t.In + t.Out + t.CacheRead + t.CacheCreate
}

func (t *Tokens) add(o Tokens) {
	t.In += o.In
	t.Out += o.Out
	t.CacheRead += o.CacheRead
	t.CacheCreate += o.CacheCreate
	t.Thinking += o.Thinking
}

// ResponseTokens is what one model response cost. There is one per AssistantRecord.
type ResponseTokens struct {
	Iteration int64 `json:"iteration"`
	// Time is when the assistant record was journaled, zero for a record written before
	// Record.Time existed.
	Time   time.Time `json:"time,omitzero"`
	Tokens Tokens    `json:"tokens"`
}

// ToolStats is what a run's calls to one tool came to.
type ToolStats struct {
	Tool string `json:"tool"`
	// Kinds holds the toolkit.Kind tokens of the providers that answered the calls,
	// sorted. It holds more than one only for a journal whose records disagree, which a
	// run started by one build and resumed by a later one can produce. A result written
	// before the kind was recorded shows as unknown here unless it is marked remote.
	Kinds []string `json:"kinds,omitempty"`
	// Calls counts the tool_use blocks naming the tool.
	Calls int64 `json:"calls"`
	// Dispatched counts the answered calls whose result records that the call was handed
	// to the provider that serves the tool.
	Dispatched int64 `json:"dispatched"`
	// Undispatched counts the answered calls whose result records a kind and records that
	// the call was not handed to its provider: answered without running, such as a call
	// a policy hook denied or the operator refused.
	//
	// A result written before the kind was recorded, with no kind and no remote mark,
	// cannot say whether the call ran, so it counts in neither this nor Dispatched.
	Undispatched int64 `json:"undispatched"`
	// Unanswered counts the calls with no result yet.
	Unanswered int64 `json:"unanswered"`
	// Errors counts the answers that came back with IsError set.
	Errors int64 `json:"errors"`
	// Duration is the time the timed calls took, and Timed how many calls it covers.
	//
	// A call's time is the gap between the record that ended its dispatch, its result or
	// its deferral, and the record journaled just before that one. A run dispatches its
	// tools one at a time, so the gap is the call's own time plus anything the run did
	// in between without journaling it, such as an approval prompt answered while the run
	// waited. An approval given while the run was suspended is not in it: such a call is
	// timed to its deferral, or from the claim record written when the run resumed.
	Duration time.Duration `json:"duration_ns"`
	Timed    int64         `json:"timed"`
	// Untimed counts the calls that ended, by a result or a deferral, and still have no
	// time: one of the two records carries no time, as every record written before
	// Record.Time does, or the second is dated before the first. A call with neither a
	// result nor a deferral is in neither Timed nor Untimed.
	Untimed int64 `json:"untimed"`
}

// Stats derives what a run cost from its records: the frame, the tokens of each model
// response and in total, and a row per tool it called.
//
// It is pure in the way Fold is: no IO, and everything is derived from the records it
// is handed. It returns no error. A record it does not read, such as a memory revisions
// record or a claim, is passed over, and a run with no terminal record is reported as
// open.
func Stats(records []Record) Statistics {
	s := Statistics{Responses: []ResponseTokens{}}

	var (
		ending   *TerminalRecord
		ended    time.Time
		reopened bool
	)

	for i, r := range records {
		if !r.Time.IsZero() {
			s.Updated = r.Time
		}
		if ending != nil {
			reopened = true
		}

		switch {
		case i == 0 && r.Protocol == MetaProtocol && r.Meta != nil:
			s.RunID = r.Meta.RunID
			s.Agent = r.Meta.Agent
			s.Model = r.Meta.Fingerprint.Model
			s.Caller = r.Meta.Caller
			s.Created = r.Meta.Created
			if s.Created.IsZero() {
				s.Created = r.Time
			}
			s.MaxIterations = r.Meta.Fingerprint.MaxIterations
			s.MaxTokens = r.Meta.Fingerprint.MaxTokens

		case r.Protocol == AssistantProtocol && r.Assistant != nil:
			response := ResponseTokens{
				Iteration: r.Assistant.Iteration,
				Time:      r.Time,
				Tokens: Tokens{
					In:          r.Assistant.InTokens,
					Out:         r.Assistant.OutTokens,
					CacheRead:   r.Assistant.CacheReadTokens,
					CacheCreate: r.Assistant.CacheCreateTokens,
					Thinking:    r.Assistant.ThinkingTokens,
				},
			}
			s.Iterations++
			s.Responses = append(s.Responses, response)
			s.Tokens.add(response.Tokens)

		case r.Protocol == TerminalProtocol && r.Terminal != nil:
			ending = r.Terminal
			ended = r.Time
			reopened = false
		}
	}

	if ending != nil && !reopened {
		s.Terminal = ending.Reason
		s.Ended = ended
	}

	s.Tools = toolStats(records)

	return s
}

// toolStats builds the tool rows of Stats from the pairing Calls returns, ordered by
// tool name and never nil.
func toolStats(records []Record) []ToolStats {
	calls, timings := pairCalls(records)

	rows := map[string]*ToolStats{}
	for i, c := range calls {
		row, ok := rows[c.Tool]
		if !ok {
			row = &ToolStats{Tool: c.Tool}
			rows[c.Tool] = row
		}

		row.Calls++
		switch {
		case timings[i].timed:
			row.Duration += timings[i].took
			row.Timed++
		case timings[i].ended:
			row.Untimed++
		}

		if c.Answer == nil {
			row.Unanswered++
			continue
		}

		kind, dispatched := resultKind(*c.Answer)
		if !slices.Contains(row.Kinds, kind.String()) {
			row.Kinds = append(row.Kinds, kind.String())
			slices.Sort(row.Kinds)
		}
		if c.Answer.Result.IsError {
			row.Errors++
		}

		if c.Answer.Kind == "" && !c.Answer.Remote {
			continue
		}
		if dispatched {
			row.Dispatched++
		} else {
			row.Undispatched++
		}
	}

	out := make([]ToolStats, 0, len(rows))
	for _, row := range rows {
		out = append(out, *row)
	}
	slices.SortFunc(out, func(a, b ToolStats) int { return strings.Compare(a.Tool, b.Tool) })

	return out
}
