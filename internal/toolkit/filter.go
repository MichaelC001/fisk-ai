//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package toolkit

import (
	"fmt"
	"regexp"
	"slices"

	"github.com/choria-io/fisk-ai/config"
)

// FilterMode selects whether a Filter keeps or removes the tools it matches.
type FilterMode int

const (
	// IncludeFilter keeps only the tools that match the filter.
	IncludeFilter FilterMode = iota
	// ExcludeFilter removes the tools that match the filter.
	ExcludeFilter
)

// Filter is a compiled config.ToolFilter applied as an include or an exclude list to
// tools of every kind. A tool matches when any of the filter's Tools patterns matches
// its Name(), or any of the filter's Tags is among the tags it reports through
// Tagged; the empty tag matches a tool with no tags, and a tool that does not
// implement Tagged has none. Name() is the tool's final name, so an import prefixed
// with its alias is matched as "alias_tool".
//
// A nil Filter keeps every tool, so a caller holds nil for a filter the
// configuration does not set.
type Filter struct {
	mode     FilterMode
	patterns []*regexp.Regexp
	tags     []string
}

// NewFilter compiles filter for mode. It returns nil for a nil filter and for one
// with neither patterns nor tags, since under IncludeFilter such a filter would
// otherwise remove every tool. A pattern that does not compile is an error.
func NewFilter(filter *config.ToolFilter, mode FilterMode) (*Filter, error) {
	if filter == nil || (len(filter.Tools) == 0 && len(filter.Tags) == 0) {
		return nil, nil
	}

	f := &Filter{mode: mode, tags: filter.Tags}
	for _, pattern := range filter.Tools {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid tool filter pattern %q: %w", pattern, err)
		}
		f.patterns = append(f.patterns, re)
	}

	return f, nil
}

// Keeps reports whether the filter leaves t in: under IncludeFilter a tool that
// matches, under ExcludeFilter one that does not. A nil Filter keeps every tool.
func (f *Filter) Keeps(t Tool) bool {
	if f == nil {
		return true
	}

	return f.matches(t) == (f.mode == IncludeFilter)
}

// matches reports whether t matches the filter's patterns or tags.
func (f *Filter) matches(t Tool) bool {
	name := t.Name()
	for _, re := range f.patterns {
		if re.MatchString(name) {
			return true
		}
	}

	tags := TagsOf(t)
	for _, tag := range f.tags {
		if tag == "" {
			if len(tags) == 0 {
				return true
			}
			continue
		}
		if slices.Contains(tags, tag) {
			return true
		}
	}

	return false
}

// FilterTools applies filter to tools as mode and returns the tools it keeps, in the
// order given. See Filter for the matching rule. A nil filter, or one with neither
// patterns nor tags, returns tools unchanged.
func FilterTools(tools []Tool, filter *config.ToolFilter, mode FilterMode) ([]Tool, error) {
	f, err := NewFilter(filter, mode)
	if err != nil {
		return nil, err
	}
	if f == nil {
		return tools, nil
	}

	var out []Tool
	for _, t := range tools {
		if f.Keeps(t) {
			out = append(out, t)
		}
	}

	return out, nil
}
