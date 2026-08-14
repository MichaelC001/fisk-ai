//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package llm

// StopReason is the neutral reason a model turn ended. The values mirror the
// vocabulary Anthropic reports and the a2a.StopReason seed; a provider codec maps
// its own strings onto these.
type StopReason string

const (
	// StopEndTurn is a natural end to the turn with a final answer.
	StopEndTurn StopReason = "end_turn"
	// StopMaxTokens means the output token cap was hit; the turn may be truncated
	// and any trailing tool_use is not safe to execute.
	StopMaxTokens StopReason = "max_tokens"
	// StopToolUse means the model asked to run one or more tools.
	StopToolUse StopReason = "tool_use"
	// StopPauseTurn means the model paused a long-running turn it intends to
	// continue on the next call.
	StopPauseTurn StopReason = "pause_turn"
	// StopRefusal means the model declined to answer.
	StopRefusal StopReason = "refusal"
	// StopStopSequence means a configured stop sequence was reached.
	StopStopSequence StopReason = "stop_sequence"
)

// Usage is the token accounting for one call, split into the tiers the agent
// meters: uncached input, output, and the two prompt-cache input tiers.
type Usage struct {
	In          int64
	Out         int64
	CacheRead   int64
	CacheCreate int64

	// Thinking is the tokens the model spent reasoning, which every provider that
	// reasons reports separately: Anthropic as thinking_tokens, an OpenAI-compatible
	// backend as reasoning_tokens.
	//
	// It is a SUBSET of Out, not an addition to it, so a caller summing the fields
	// double-counts. It is reported apart because it is the only number that separates
	// a model that is not reasoning from one that is reasoning where nobody can see it,
	// which is otherwise a question only a raw HTTP trace can answer. Zero from a
	// provider that has no reasoning to report.
	Thinking int64
}

// Response is a model's reply to a call: the assistant turn's content, why it
// stopped, and what it cost.
type Response struct {
	// ID is the provider's own identifier for this reply, empty when a provider does
	// not issue one. It is what correlates a call in this process with the same call in
	// the provider's logs or dashboard, which is the only thing that can settle a
	// question about a specific request after the fact.
	ID string
	// Model is the model that actually answered, as the provider reports it, empty when
	// a provider does not say. It is deliberately kept apart from the requested model:
	// a request naming an alias such as claude-sonnet-5 is served by a dated snapshot,
	// and the snapshot is what bills and what a reproduction has to pin.
	Model string

	Content    []ContentBlock
	StopReason StopReason
	Usage      Usage
}
