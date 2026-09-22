//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package runstate

import (
	"regexp"
	"time"
)

// StatusOpen is the SearchFilter.Status value that selects the runs with no terminal
// reason: a run still open, one with a turn in flight, or one that journaled something
// after its last terminal record.
const StatusOpen = "open"

// SearchFilter selects runs from a listing by what RunInfo holds. Every field that is set
// must match, and a zero SearchFilter selects every run.
//
// It is applied by the caller to rows a store already listed, unlike ListFilter, which a
// store applies itself. Its Agent differs from ListFilter.Agent too: a run carrying no
// agent is excluded here, where ListFilter.MatchesAgent includes it.
type SearchFilter struct {
	// Agent selects the runs whose RunInfo.Agent equals it. A run carrying no agent,
	// which is every run journaled before the agent was recorded, is excluded.
	Agent string
	// Model and Caller select the runs whose RunInfo field of the same name equals them.
	Model  string
	Caller string
	// Status selects the runs with this status: StatusOpen for a run whose
	// RunInfo.Terminal is empty, or a TerminalReason value for a run that ended so.
	Status string
	// Since selects the runs created at or after it and Until the runs created before
	// it, both against RunInfo.Created. A zero time is no bound.
	Since time.Time
	Until time.Time
	// Prompt selects the runs whose RunInfo.Prompt matches it. Nil selects every prompt.
	Prompt *regexp.Regexp
}

// Matches reports whether info passes every filter that is set.
func (f SearchFilter) Matches(info RunInfo) bool {
	if f.Agent != "" && info.Agent != f.Agent {
		return false
	}

	return f.matchesOthers(info)
}

// Select returns the rows f matches, in the order given, and how many rows it excluded
// only because they carry no agent. That count covers the rows every filter other than
// Agent accepted, so a caller can say how many runs an agent filter hid for predating
// the field. It is zero when Agent is empty.
func (f SearchFilter) Select(infos []RunInfo) ([]RunInfo, int) {
	var (
		out     []RunInfo
		noAgent int
	)

	for _, info := range infos {
		if !f.matchesOthers(info) {
			continue
		}

		if f.Agent != "" && info.Agent == "" {
			noAgent++
			continue
		}
		if f.Agent != "" && info.Agent != f.Agent {
			continue
		}

		out = append(out, info)
	}

	return out, noAgent
}

// matchesOthers reports whether info passes every filter other than Agent.
func (f SearchFilter) matchesOthers(info RunInfo) bool {
	if f.Model != "" && info.Model != f.Model {
		return false
	}
	if f.Caller != "" && info.Caller != f.Caller {
		return false
	}
	if f.Status == StatusOpen && info.Terminal != "" {
		return false
	}
	if f.Status != "" && f.Status != StatusOpen && info.Terminal != TerminalReason(f.Status) {
		return false
	}
	if !f.Since.IsZero() && info.Created.Before(f.Since) {
		return false
	}
	if !f.Until.IsZero() && !info.Created.Before(f.Until) {
		return false
	}
	if f.Prompt != nil && !f.Prompt.MatchString(info.Prompt) {
		return false
	}

	return true
}
