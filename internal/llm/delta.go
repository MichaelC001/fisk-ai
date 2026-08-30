//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package llm

import "context"

// DeltaKind is the kind of content block a Delta is a fragment of. The values
// name the two block kinds a provider produces incrementally; a codec maps its
// own event types onto these.
type DeltaKind string

const (
	// DeltaText is a fragment of a TextBlock, the model's prose as it is written.
	DeltaText DeltaKind = "text"
	// DeltaThinking is a fragment of a ThinkingBlock's human-readable reasoning. The
	// opaque signature does not stream; it arrives with the assembled block.
	DeltaThinking DeltaKind = "thinking"
)

// Delta is one fragment of an assistant turn as the provider produced it. A
// backend implementing StreamingProvider reports deltas while the model writes
// and returns the whole turn at the end, so the fragments are an addition to the
// assembled Response and never a replacement for it.
//
// Tool call arguments are deliberately not carried here, and a backend must not
// add them. Anthropic delivers them as input_json_delta and both target web
// protocols have an event for them, but the PreToolUse hook cannot run until the
// turn ends, because the tool_use block does not exist until then. Anything
// streamed before that point is pre-hook, so a hook using RewriteInput to strip a
// credential from the arguments, or denying the call outright, would be defeated
// by a peer that already held the original. Buffering the fragments until dispatch
// does not rescue it either, since ToolCall fires with the rewritten input. Tool
// arguments therefore reach a consumer only in the whole ToolUseBlock of the
// assembled Response, after PreToolUse has run.
type Delta struct {
	// Kind is the kind of block this fragment belongs to.
	Kind DeltaKind

	// Index is the position of this fragment's block in the Content slice of the
	// *Response that CallStream returns: every fragment carrying the same Index belongs
	// to Response.Content[Index]. A consumer reconciles the fragments it received
	// against the assembled turn with it.
	//
	// Implementers must hold to that. A backend that merged or dropped a block while
	// assembling the Response would leave every consumer misaligned with no error
	// raised. For the Anthropic backend it holds because MessageToNeutral appends one
	// neutral block per Anthropic block, in order.
	Index int

	// Text is this fragment's text, to be appended to the text already delivered for
	// the same Index. It is empty on a Final delta that has no text left to send.
	Text string

	// Final marks the last fragment of the block at Index. A backend emits one for every
	// block it streamed, with an empty Text when there is nothing left to send. The
	// fragments do not say where a block ends: the a2a sink flushes its coalescing
	// buffer on Final and would otherwise hold the tail of every block until the next
	// fragment or the end of the call, and both target web protocols need an explicit
	// end event per content block.
	Final bool
}

// StreamingProvider is a Provider whose backend reports an assistant turn as the model
// produces it. It embeds Provider, so a value satisfying it is usable everywhere a
// Provider is and a caller makes one assertion:
//
//	sp, ok := p.(llm.StreamingProvider)
//
// Caps declares nothing about streaming. It grows as a second provider makes a
// capability difference concrete, the assertion above answers the question for a
// registered backend, and a proxied BaseURL that cannot stream is not something a
// declaration could report.
type StreamingProvider interface {
	Provider

	// CallStream issues one model request, calls fn with each fragment as the
	// provider produces it, and returns the same assembled *Response that Call
	// returns for the same Request. That equivalence is a requirement on the
	// implementer: the codec, the journal, the budget accounting and telemetry all
	// read the returned Response, and none of them change because the turn arrived
	// in pieces.
	//
	// An implementer must return an error rather than a partial turn. Reaching the
	// provider's end-of-message event is a precondition for returning a Response,
	// because a stream that stops early is not always an error from the SDK: the
	// Anthropic SDK reports none when a stream ends without message_stop, so a cut
	// connection would otherwise return a truncated turn that the caller journals and
	// sends as the final answer.
	//
	// Fragments already passed to fn are not withdrawn when CallStream returns an
	// error. They were never authoritative: the assembled Response is what a consumer
	// reconciles them against, and a failed call sends none, so a consumer holding
	// fragments for a call that failed discards them.
	//
	// fn is called on the goroutine that called CallStream, in the order the provider
	// produced the fragments, and never after CallStream returns. It must not be nil;
	// a caller that wants no fragments calls Call. Fragments for different Index
	// values may interleave, so a consumer keys its state on Index rather than
	// assuming one block finishes before the next begins.
	CallStream(ctx context.Context, req Request, fn func(Delta)) (*Response, error)
}
