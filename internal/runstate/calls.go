//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package runstate

import (
	"encoding/json"
	"regexp"
	"slices"
	"time"
)

// Call is one tool call a run made, paired with the result that answered it.
//
// The call side comes from the tool_use block of an AssistantRecord and the answer side
// from the ToolResultRecord carrying the same tool_use id. The answer is the record as
// journaled, so its content is whole rather than trimmed to what a transcript block
// carries.
type Call struct {
	// Iteration is the loop index of the assistant turn that made the call.
	Iteration int64 `json:"iteration"`
	// ToolUseID is the id of the tool_use block, which the answer carries too.
	ToolUseID string `json:"tool_use_id"`
	// Tool is the tool the model named.
	Tool string `json:"tool"`
	// Input is the input the model supplied, as journaled.
	Input json.RawMessage `json:"input,omitempty"`
	// Time is when the assistant record holding the call was journaled. It is zero for a
	// record written before Record.Time existed.
	Time time.Time `json:"time,omitzero"`
	// Deferred is the record a tool wrote when it said the answer arrives later, nil for
	// a call that did not defer.
	Deferred *DeferredRecord `json:"deferred,omitempty"`
	// Answer is the result that answered the call, nil for a call nothing has answered
	// yet: one waiting on a deferred answer, or one a suspended or interrupted run left
	// behind.
	Answer *ToolResultRecord `json:"answer,omitempty"`
	// AnswerTime is when Answer was journaled. It is zero when Answer is nil and for a
	// record written before Record.Time existed.
	AnswerTime time.Time `json:"answer_time,omitzero"`
}

// CallFilter selects calls from what Calls returns. Every field that is set must match,
// and a zero CallFilter selects every call.
type CallFilter struct {
	// Tools selects the calls to any of the named tools, compared exactly. Empty selects
	// every tool.
	Tools []string
	// Iteration selects the calls made at this loop iteration. Nil selects every
	// iteration.
	Iteration *int64
	// Answered selects only the calls that have an answer.
	Answered bool
	// Errors selects only the calls whose answer has IsError set, so it selects only
	// answered calls.
	Errors bool
	// Input selects the calls whose raw input JSON matches it. Nil selects every input.
	Input *regexp.Regexp
	// Output selects the calls whose answer content matches it, so it selects only
	// answered calls. Nil selects every call.
	Output *regexp.Regexp
}

// Matches reports whether c passes every filter that is set.
func (f CallFilter) Matches(c Call) bool {
	if len(f.Tools) > 0 && !slices.Contains(f.Tools, c.Tool) {
		return false
	}
	if f.Iteration != nil && c.Iteration != *f.Iteration {
		return false
	}
	if (f.Answered || f.Errors || f.Output != nil) && c.Answer == nil {
		return false
	}
	if f.Errors && !c.Answer.Result.IsError {
		return false
	}
	if f.Input != nil && !f.Input.Match(c.Input) {
		return false
	}
	if f.Output != nil && !f.Output.MatchString(c.Answer.Result.Content) {
		return false
	}

	return true
}

// Select returns the calls f matches, in the order given. It returns nil when none
// match.
func (f CallFilter) Select(calls []Call) []Call {
	var out []Call
	for _, c := range calls {
		if f.Matches(c) {
			out = append(out, c)
		}
	}

	return out
}

// callTiming is how long one call took, measured as the gap between the record that
// ended its dispatch and the record journaled just before that one. ended is false for a
// call with neither a result nor a deferral, and timed is false where it did not end or
// where the gap could not be measured.
type callTiming struct {
	took  time.Duration
	timed bool
	ended bool
}

// Calls returns every tool call in records, in the order the calls were made, each
// paired with the result that answered it.
//
// It is pure in the way Fold is: no IO, and everything is derived from the records it
// is handed. It returns no error. A call nothing answered is returned with a nil Answer,
// a result that answers no call is passed over, and so is every record that is not an
// assistant turn, a tool result or a deferral.
//
// A result answers the most recent call carrying its tool_use id that has no answer yet.
// A call answered more than once keeps the first answer.
func Calls(records []Record) []Call {
	calls, _ := pairCalls(records)

	return calls
}

// pairCalls does the pairing Calls returns and measures each call beside it, index
// aligned with the calls.
//
// A call's time is the gap ending at the record that finished its dispatch: its result,
// or its deferral for a call that deferred. The answer to a deferred call arrives
// whenever somebody supplies it and is not a measure of the tool, so it leaves the
// measurement taken at the deferral alone.
func pairCalls(records []Record) ([]Call, []callTiming) {
	var (
		calls   []Call
		timings []callTiming
		prev    time.Time
		// open holds the indexes of the calls with no answer yet, per tool_use id, oldest
		// first.
		open = map[string][]int{}
	)

	latestOpen := func(id string) (int, bool) {
		idx := open[id]
		if len(idx) == 0 {
			return 0, false
		}

		return idx[len(idx)-1], true
	}

	gap := func(at time.Time) callTiming {
		if prev.IsZero() || at.IsZero() || at.Before(prev) {
			return callTiming{ended: true}
		}

		return callTiming{took: at.Sub(prev), timed: true, ended: true}
	}

	for _, r := range records {
		switch {
		case r.Protocol == AssistantProtocol && r.Assistant != nil:
			for _, block := range r.Assistant.Message.Content {
				if block.ToolUse == nil {
					continue
				}

				open[block.ToolUse.ID] = append(open[block.ToolUse.ID], len(calls))
				calls = append(calls, Call{
					Iteration: r.Assistant.Iteration,
					ToolUseID: block.ToolUse.ID,
					Tool:      block.ToolUse.Name,
					Input:     block.ToolUse.Input,
					Time:      r.Time,
				})
				timings = append(timings, callTiming{})
			}

		case r.Protocol == DeferredProtocol && r.Deferred != nil:
			i, ok := latestOpen(r.Deferred.ToolUseID)
			if ok && calls[i].Deferred == nil {
				d := *r.Deferred
				calls[i].Deferred = &d
				timings[i] = gap(r.Time)
			}

		case r.Protocol == ToolResultProtocol && r.ToolResult != nil:
			id := r.ToolResult.ToolUseID
			i, ok := latestOpen(id)
			if ok {
				res := *r.ToolResult
				calls[i].Answer = &res
				calls[i].AnswerTime = r.Time
				if calls[i].Deferred == nil {
					timings[i] = gap(r.Time)
				}
				open[id] = open[id][:len(open[id])-1]
			}
		}

		prev = r.Time
	}

	return calls, timings
}
