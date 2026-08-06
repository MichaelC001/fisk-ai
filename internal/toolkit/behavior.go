//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package toolkit

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Hint is a three-state answer to a question about a tool's behavior: the author
// said nothing, said yes, or said no. The zero value is HintUnset, so a tool that
// declares nothing declares it safely, and a consumer applies its own conservative
// default rather than reading an accidental "no".
//
// It marshals as a JSON boolean, and HintUnset marshals as null. Behavior's fields
// carry omitzero so an unset hint is absent from the document entirely rather than
// null; a decoder reading either an absent field or an explicit null gets HintUnset
// back.
type Hint int

const (
	// HintUnset is the zero value: nothing was declared about this aspect.
	HintUnset Hint = iota
	// HintTrue asserts the aspect holds.
	HintTrue
	// HintFalse asserts the aspect does not hold, which is not the same as saying
	// nothing: it is an author stating the negative.
	HintFalse
)

// String returns the stable, lowercase token for a Hint, for human-facing listings
// and logs. An unrecognized value formats as the HintUnset token.
func (h Hint) String() string {
	switch h {
	case HintTrue:
		return "true"
	case HintFalse:
		return "false"
	default:
		return "unset"
	}
}

// Bool returns the hint as a bool and whether it was declared at all. A caller
// projecting onto a surface with only two states uses the second return to decide
// whether to emit anything.
func (h Hint) Bool() (value bool, declared bool) {
	switch h {
	case HintTrue:
		return true, true
	case HintFalse:
		return false, true
	default:
		return false, false
	}
}

// HintOf converts a plain bool to a Hint, for a caller that has a definite answer.
func HintOf(v bool) Hint {
	if v {
		return HintTrue
	}

	return HintFalse
}

// MarshalJSON writes the hint as a JSON boolean, or null when it is unset.
func (h Hint) MarshalJSON() ([]byte, error) {
	value, declared := h.Bool()
	if !declared {
		return []byte("null"), nil
	}

	return json.Marshal(value)
}

// UnmarshalJSON reads a JSON boolean into a hint; null and an absent field both
// leave it unset. Any other JSON type is an error rather than a silent unset, so a
// malformed document is visible to the decoder that can report it.
func (h *Hint) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*h = HintUnset
		return nil
	}

	var value bool
	err := json.Unmarshal(data, &value)
	if err != nil {
		return fmt.Errorf("behavior hint must be a boolean: %w", err)
	}

	*h = HintOf(value)

	return nil
}

// Behavior is a tool's own statement of what calling it does to the world. It is
// the neutral form every tool kind declares and every serving surface projects from:
// a command tool derives it from its tags, an in-process or remote tool carries it
// on its spec, and the MCP server renders it as tool annotations.
//
// It describes and does not enforce. Nothing in the harness gates a call on it, and
// nothing should: a tool declares its own behavior, so treating the declaration as a
// boundary would let the thing being constrained write the constraint. The gate is
// ConfirmTag and the operator's confirm_tags; the reliable off switch is DenyTag.
//
// Every field is tri-state and the zero value declares nothing, which is the answer
// a tool that has not thought about it should give. Consumers apply their own
// conservative default to an unset field: over MCP those defaults are the spec's,
// which read a tool as destructive and open-world until told otherwise.
type Behavior struct {
	// ReadOnly asserts the call does not modify its environment.
	ReadOnly Hint `json:"read_only,omitzero"`
	// Destructive asserts the call may destroy or overwrite existing state, rather
	// than only adding to it. It is meaningful only when the call is not read-only.
	Destructive Hint `json:"destructive,omitzero"`
	// Idempotent asserts that repeating the call with the same arguments has no
	// further effect. It is meaningful only when the call is not read-only.
	Idempotent Hint `json:"idempotent,omitzero"`
	// OpenWorld asserts the call reaches an open set of external entities, the way a
	// web search does, rather than a closed one the operator configured. No command
	// tag maps to it: a command author cannot answer it consistently, so it is
	// declared only by a Go author who knows.
	OpenWorld Hint `json:"open_world,omitzero"`
}

// IsZero reports whether nothing was declared, so a Behavior carrying no assertion
// is omitted from a JSON document rather than written as an empty object.
func (b Behavior) IsZero() bool {
	return b == Behavior{}
}

// String renders the declared assertions as a compact, comma-separated summary for
// human-facing listings, e.g. "read only, idempotent". It is "" when nothing was
// declared, which a caller renders as it sees fit.
func (b Behavior) String() string {
	var parts []string

	appendHint := func(h Hint, yes, no string) {
		switch h {
		case HintTrue:
			parts = append(parts, yes)
		case HintFalse:
			parts = append(parts, no)
		}
	}

	appendHint(b.ReadOnly, "read only", "not read only")
	appendHint(b.Destructive, "destructive", "additive")
	appendHint(b.Idempotent, "idempotent", "not idempotent")
	appendHint(b.OpenWorld, "open world", "closed world")

	return strings.Join(parts, ", ")
}

// BehaviorDescriber is implemented by a tool that declares its behavior. It is a
// narrow capability rather than part of Tool because an undeclared behavior is the
// safe answer: a tool that does not implement it is served normally, and its
// consumers fall back to their own conservative defaults. This is deliberately
// unlike Confirmable, which a serving surface refuses a tool for not implementing,
// because there the absent answer is the dangerous one.
type BehaviorDescriber interface {
	// Behavior returns what calling the tool does to the world. It is a property of
	// the tool, not of one call, so it takes no input and must not vary run to run.
	Behavior() Behavior
}

// BehaviorOf returns a tool's declared behavior, or the all-unset zero value for a
// tool that declares none. Callers use it rather than asserting the interface
// themselves so "declared nothing" and "cannot declare" stay indistinguishable.
func BehaviorOf(t Tool) Behavior {
	d, ok := t.(BehaviorDescriber)
	if !ok {
		return Behavior{}
	}

	return d.Behavior()
}

// The reserved ai: tags. A command author writes them on a command; the harness and
// its serving surfaces read them. They are defined here, in the package every tool
// kind shares, so the vocabulary has one owner and an embedder can name a tag
// without retyping its string.
//
// The first three change what the harness does with a command. The rest describe
// what the command does and change nothing the harness enforces; they are carried to
// clients as advice.
const (
	// DenyTag keeps a command out of every tool set: it is stripped before the
	// include and exclude filters run and can never be added back, on any surface.
	// It is the reliable off switch.
	DenyTag = "ai:deny"
	// NoDeferTag forces a command to always be sent to the model directly, never
	// hidden behind the tool search tool, even within an otherwise deferred set.
	NoDeferTag = "ai:no_defer"
	// ConfirmTag marks a tool as requiring the operator's explicit approval before it
	// runs. It is always on: a tool carrying it is gated regardless of the operator's
	// configured confirm tags, so the guarantee cannot be weakened by configuration.
	// Operators gate further tools by listing additional tags, which NeedsConfirm and
	// ConfirmTrigger treat the same way. It is the single definition shared by every
	// tool kind that can be gated, so the gate logic cannot drift between them.
	ConfirmTag = "ai:confirm"

	// ReadOnlyTag asserts the command does not modify its environment.
	ReadOnlyTag = "ai:read_only"
	// DestructiveTag asserts the command may destroy or overwrite existing state.
	DestructiveTag = "ai:destructive"
	// AdditiveTag asserts the command changes state but only adds to it. It is the
	// tag that relaxes the conservative default a client applies to a write command.
	AdditiveTag = "ai:additive"
	// IdempotentTag asserts that repeating the command with the same arguments has no
	// further effect.
	IdempotentTag = "ai:idempotent"
)

// reservedTags is every ai: tag the harness recognizes, the set UnknownReservedTags
// checks against.
var reservedTags = []string{
	DenyTag,
	NoDeferTag,
	ConfirmTag,
	ReadOnlyTag,
	DestructiveTag,
	AdditiveTag,
	IdempotentTag,
}

// reservedPrefix is the namespace the harness claims. A tag under it that is not a
// reserved tag is a typo often enough to be worth reporting.
const reservedPrefix = "ai:"

// ReservedTags returns the ai: tags the harness recognizes, for a caller listing the
// vocabulary to a person. The returned slice is a copy the caller may keep.
func ReservedTags() []string {
	return slices.Clone(reservedTags)
}

// UnknownReservedTags returns the tags that claim the reserved ai: namespace but are
// not tags the harness recognizes, in the order given, deduplicated. A tag it returns
// does nothing at all, which is indistinguishable from a correctly tagged command
// until someone says so, so callers with somewhere to write report them. It is a
// warning and never an error: a command may legitimately carry a private ai: tag,
// and refusing to run over a metadata tag would be out of proportion.
func UnknownReservedTags(tags []string) []string {
	var unknown []string

	for _, tag := range tags {
		if !strings.HasPrefix(tag, reservedPrefix) {
			continue
		}
		if slices.Contains(reservedTags, tag) || slices.Contains(unknown, tag) {
			continue
		}
		unknown = append(unknown, tag)
	}

	return unknown
}

// Tagged is implemented by a tool that carries author-supplied tags. Only a command
// tool does: tags are how a wrapped application's author annotates a command, and a
// tool kind whose author writes Go declares the same things structurally instead.
type Tagged interface {
	// Tags are the tool's tags, in the order its author wrote them.
	Tags() []string
}

// TagsOf returns a tool's tags, or nil for a kind that carries none.
func TagsOf(t Tool) []string {
	tagged, ok := t.(Tagged)
	if !ok {
		return nil
	}

	return tagged.Tags()
}

// TagIssues reports what is wrong with a tool's reserved tags: tags claiming the ai:
// namespace that the harness does not know, and behavior tags that contradict each
// other. Neither stops the tool from being used, and both are invisible without
// someone saying so, which is why every surface that has an operator to tell calls
// this. A tool carrying no tags reports nothing.
func TagIssues(t Tool) (unknown, conflicting []string) {
	tags := TagsOf(t)
	if len(tags) == 0 {
		return nil, nil
	}

	_, conflicting = BehaviorFromTags(tags)

	return UnknownReservedTags(tags), conflicting
}

// BehaviorFromTags maps a tool's tags to the behavior they declare, and returns any
// contradictory tags it had to resolve so a caller with somewhere to write can
// report them.
//
// Each tag sets exactly the aspect it names. Read-only is deliberately not taken to
// imply anything about the other aspects: MCP reads the destructive and idempotent
// hints only when a tool is not read-only, so deriving them would send a client
// fields it is told to ignore.
//
// Contradictions resolve conservatively rather than failing. Command tags come from
// a wrapped binary the operator often cannot edit, so one mistagged command must not
// be able to stop a run or a server. A command that says it writes, either way it can
// say it, loses its claim to be read-only, since read-only is the permissive claim a
// client acts on; and DestructiveTag beats AdditiveTag. Resolution happens after every
// tag is collected, so the result does not depend on the order the author wrote them
// in.
func BehaviorFromTags(tags []string) (Behavior, []string) {
	var (
		behavior  Behavior
		conflicts []string
	)

	has := func(tag string) bool { return slices.Contains(tags, tag) }

	readOnly := has(ReadOnlyTag)
	destructive := has(DestructiveTag)
	additive := has(AdditiveTag)

	switch {
	case readOnly && destructive:
		conflicts = append(conflicts, ReadOnlyTag, DestructiveTag)
		readOnly = false
	case readOnly && additive:
		conflicts = append(conflicts, ReadOnlyTag, AdditiveTag)
		readOnly = false
	}

	if destructive && additive {
		conflicts = append(conflicts, DestructiveTag, AdditiveTag)
		additive = false
	}

	switch {
	case readOnly:
		behavior.ReadOnly = HintTrue
	case destructive || additive:
		// A command that states how it writes has stated that it writes.
		behavior.ReadOnly = HintFalse
	}

	switch {
	case destructive:
		behavior.Destructive = HintTrue
	case additive:
		behavior.Destructive = HintFalse
	}

	if has(IdempotentTag) {
		behavior.Idempotent = HintTrue
	}

	return behavior, conflicts
}

// Resolve returns the behavior with any internal contradiction settled the same way
// BehaviorFromTags settles contradictory tags: the more dangerous reading wins. It
// is how a Behavior that arrived from somewhere untrusted, such as a peer agent's
// description of its own tools, is made safe to hold without rejecting the tool that
// carried it.
func (b Behavior) Resolve() Behavior {
	if b.ReadOnly == HintTrue && b.Destructive == HintTrue {
		b.ReadOnly = HintFalse
	}

	return b
}
