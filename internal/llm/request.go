//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package llm

// ThinkingMode says what a request asks of the model's reasoning, which some
// providers call thinking and others reasoning.
//
// It has three values rather than two because saying nothing is not the same as
// asking for nothing. A model that reasons unaided keeps reasoning when it is asked
// nothing, so a caller that wants it to stop has to say so, and a caller that wants
// the provider's own default has to be able to stay silent. A provider with no
// thinking mechanism ignores all three.
type ThinkingMode int

const (
	// ThinkingUnset asks nothing, so the model and the backend use their own
	// defaults. It is the zero value, which keeps it the behavior of any Request that
	// does not mention thinking, and it is the only value safe to send to a model that
	// rejects the parameter outright, since it sends no parameter at all.
	ThinkingUnset ThinkingMode = iota

	// ThinkingOn asks the model to reason and to expose it.
	ThinkingOn

	// ThinkingOff asks the model not to reason. It is distinct from ThinkingUnset:
	// this states a preference where that declines to, so it reaches the provider as a
	// parameter and can be rejected by a backend that does not accept one.
	ThinkingOff
)

// Request is a single provider-neutral model call: the conversation plus the
// knobs a provider needs to render it to its own wire format. It carries no
// infrastructure (client, credentials, per-call timeout); those live on the
// Provider, so a Request is a plain value a test can build and assert on.
type Request struct {
	// Model is the provider model id to call.
	Model string

	// SystemBlocks is the system prompt as an ordered list of text segments. It is a
	// slice, not a single string, because a provider may treat each segment as its own
	// block: the Anthropic provider sends them as separate system blocks and places the
	// prompt-cache breakpoint on the last one, while a provider whose system prompt is
	// one string joins them.
	SystemBlocks []string

	// Messages is the conversation so far. It ends on a boundary the provider accepts
	// (an initial user prompt, or an assistant turn whose tool_use blocks are all
	// answered by a following user results turn).
	Messages []Message

	// Tools is the model-facing tool set. A tool that requests deferral is hidden
	// behind server-side tool search by a provider that supports it; see ToolSearch.
	Tools []ToolDef

	// ToolSearch asks the provider to add its server-side tool search tool so the
	// model can discover the deferred tools by name and description. It is set when at
	// least one tool actually defers, so the search tool is present only when there is
	// something for it to find.
	ToolSearch bool

	// Thinking says what this request asks of the model's reasoning. A provider maps
	// it to its own thinking configuration, or ignores it if it has none. The zero
	// value asks for nothing, so a caller that does not care never has to set it.
	Thinking ThinkingMode

	// ReasoningEffort is the effort level to ask the model for, which governs how
	// deeply it reasons and how many tokens it spends overall. Empty asks for nothing,
	// so the model uses its own default.
	//
	// It is a string rather than an enum because the levels belong to the model: each
	// provider names its own, and a model released after this build may take a level
	// this build has never heard of. A provider sends the value as written and lets the
	// model refuse one it does not take; a provider with no effort mechanism ignores it.
	ReasoningEffort string

	// MaxOutputTokens caps the tokens generated for this one response. It bounds a
	// single reply, distinct from any cumulative token budget the caller enforces.
	MaxOutputTokens int64

	// PromptCache turns on provider prompt caching for this request. It is deliberately
	// not part of the run fingerprint, so toggling it never refuses a resume.
	PromptCache bool

	// Interactive marks a chat run whose think-time between turns is long, letting a
	// provider pick a longer cache TTL than an autonomous loop's tight cadence needs.
	Interactive bool
}
