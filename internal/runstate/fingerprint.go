//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package runstate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// Fingerprint captures the configuration a run was started with. It is validated
// on resume: continuing a conversation against a changed model, prompt or tool
// set can be incoherent (a stored tool_use may reference a tool that no longer
// exists, or a thinking signature may be rejected), so a mismatch is refused
// unless the operator forces it.
//
// The system prompt is stored as a hash, not verbatim, so the fingerprint never
// leaks prompt contents.
type Fingerprint struct {
	// Provider is the neutral provider id the run was started with. It is a HARD
	// resume gate that --force cannot cross (a turn from another provider is
	// incoherent: a stored thinking signature or provider block belongs to the
	// provider that produced it), so it is deliberately excluded from Equal and
	// Diff, which govern only the forceable configuration drift. The provider check
	// lives at the resume gate and is unconditional.
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	SystemHash   string `json:"system_hash"`
	ToolsHash    string `json:"tools_hash"`
	ThinkingMode string `json:"thinking_mode"`
	// ReasoningEffort is the effort level the run was started with, empty when it
	// asked for none. A journal written before this field existed folds it empty, and a
	// run that sets no effort computes it empty, so every session journaled until now
	// still resumes.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	MaxTokens       int64  `json:"max_tokens"`
	MaxIterations   int64  `json:"max_iterations"`
}

// quotedOrNone renders a fingerprint value for a diff line, so an empty one reads as
// the absence it is rather than as a gap in the sentence.
func quotedOrNone(v string) string {
	if v == "" {
		return "none"
	}

	return v
}

// HashHex returns the hex-encoded SHA-256 of b, for building the system prompt
// and tool-set hashes.
func HashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Equal reports whether two fingerprints match exactly. It is the strict
// comparison; a resume asks BlockingDiff instead, since not every difference can
// make a stored conversation incoherent.
func (f Fingerprint) Equal(o Fingerprint) bool {
	return len(f.Diff(o)) == 0
}

// BlockingDiff returns the differences a resume must refuse: the model, the system
// prompt, the thinking mode and the reasoning effort, each of which can leave a stored
// conversation the provider will not accept. A thinking signature belongs to the mode and
// effort that produced it.
//
// The tool set is excluded, and was in this list on a claim nothing tested. A stored
// tool_use naming a tool that is gone was said to break a conversation, and it does not:
// against the Messages API a history holding a completed tool_use and tool_result for an
// undeclared tool is accepted, as it is with no tools declared at all, and so is the
// error result the resume itself produces for the call. The one shape a provider refuses
// is a message list ending on a tool_use with no result, which is refused whether or not
// the tool is declared and which a run never sends: completePending dispatches the call
// and appends its result, unknown tool included, before the next model call. Drift in the
// tool set is reported through ToolsDiff and drops the standing approvals.
//
// MaxTokens and MaxIterations are excluded. Neither can corrupt history: they bound
// how far a run gets, so a difference finishes a run under limits it did not start
// with, which is reported to the operator rather than refused. A served conversation
// makes that routine, since a caller may lower both per request and the local
// configuration is the ceiling, so refusing on them would end a conversation on a
// difference that changes nothing about what the model can be sent.
func (f Fingerprint) BlockingDiff(o Fingerprint) []string {
	var out []string

	if f.Model != o.Model {
		out = append(out, fmt.Sprintf("model: %s -> %s", f.Model, o.Model))
	}
	if f.SystemHash != o.SystemHash {
		out = append(out, "system prompt: changed")
	}
	if f.ThinkingMode != o.ThinkingMode {
		out = append(out, fmt.Sprintf("thinking: %s -> %s", f.ThinkingMode, o.ThinkingMode))
	}
	if f.ReasoningEffort != o.ReasoningEffort {
		out = append(out, fmt.Sprintf("reasoning_effort: %s -> %s", quotedOrNone(f.ReasoningEffort), quotedOrNone(o.ReasoningEffort)))
	}

	return out
}

// ToolsDiff reports whether the tool set moved, for a resume to warn about while
// continuing and to drop its standing approvals on.
//
// It does not gate the resume, the history being readable either way, and it is separate
// from BlockingDiff because what it endangers is a grant rather than a conversation: an
// approval is keyed on a tool name, so a tool set that moved under it may have changed
// the very command the operator approved.
func (f Fingerprint) ToolsDiff(o Fingerprint) []string {
	if f.ToolsHash == o.ToolsHash {
		return nil
	}

	return []string{"tool set: changed"}
}

// BudgetDiff returns the differences in the two bounds BlockingDiff excludes, for a
// resume to report while continuing.
func (f Fingerprint) BudgetDiff(o Fingerprint) []string {
	var out []string

	if f.MaxTokens != o.MaxTokens {
		out = append(out, fmt.Sprintf("max_tokens: %d -> %d", f.MaxTokens, o.MaxTokens))
	}
	if f.MaxIterations != o.MaxIterations {
		out = append(out, fmt.Sprintf("max_iterations: %d -> %d", f.MaxIterations, o.MaxIterations))
	}

	return out
}

// Diff returns a human-readable line per field that differs between f (the saved
// fingerprint) and o (the current one). Hashed fields are reported as changed rather
// than showing their opaque values.
//
// It is every field, across all three of the narrower diffs, since Equal is the strict
// comparison and a resume asks one of those instead.
func (f Fingerprint) Diff(o Fingerprint) []string {
	out := f.BlockingDiff(o)
	out = append(out, f.ToolsDiff(o)...)
	out = append(out, f.BudgetDiff(o)...)

	return out
}
