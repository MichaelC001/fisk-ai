//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package anthropic

import (
	"context"
	"fmt"
	"maps"
	"slices"

	sdk "github.com/anthropics/anthropic-sdk-go"

	"github.com/choria-io/fisk-ai/internal/llm"
)

// Provider streams, so a caller can assert llm.StreamingProvider on a registered
// anthropic backend and get the fragments as well as the turn.
var _ llm.StreamingProvider = (*Provider)(nil)

// CallStream issues one Anthropic request as a stream under the provider's
// per-call timeout, reports each text and thinking fragment to fn as the model
// writes it, and returns the same neutral Response Call returns for the same
// Request. It renders the request with buildParams and decodes the assembled
// message with ResponseToNeutral, so the two call paths differ only in how the
// message arrives.
//
// A stream that ends without message_stop is an error. The SDK reports none for a
// clean end of body and Accumulate sets the stop reason only from message_delta, so
// a cut connection or a proxy that truncates the body leaves a partly accumulated
// message with an empty stop reason and short usage. Nothing above this can tell
// that from a finished turn: a truncated turn with no tool_use blocks reads as
// terminal, and the caller journals it and answers with it.
//
// fn is called on the calling goroutine, in the order the events arrive, and never
// after this returns. It must not be nil. A block still open when the stream ends
// gets its Final then, in index order, so nothing depends on the stream having
// stopped every block it started.
func (p *Provider) CallStream(ctx context.Context, req llm.Request, fn func(llm.Delta)) (*llm.Response, error) {
	if fn == nil {
		return nil, fmt.Errorf("a delta function is required")
	}

	params, err := p.buildParams(req)
	if err != nil {
		return nil, err
	}

	callCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	stream := p.client.Messages.NewStreaming(callCtx, params)
	// The stream holds the response body, so it is closed on every path out of here,
	// including the early returns below.
	defer stream.Close()

	var (
		msg sdk.Message
		// The kind of each block that has streamed a fragment, so the Final delta for a
		// block carries its kind and a block that streamed nothing gets none. Deltas and
		// stops for open blocks interleave, so this is keyed by index rather than tracking
		// one current block.
		streaming = map[int64]llm.DeltaKind{}
		stopped   bool
	)

	for stream.Next() {
		event := stream.Current()

		// Accumulate first: it validates the event's index against the blocks started so
		// far, so a fragment is only reported for a block the assembled message has.
		err = msg.Accumulate(event)
		if err != nil {
			return nil, fmt.Errorf("accumulating %s event: %w", event.Type, err)
		}

		switch event.Type {
		case "content_block_delta":
			kind, text, ok := deltaFragment(event)
			if !ok {
				continue
			}

			streaming[event.Index] = kind
			fn(llm.Delta{Kind: kind, Index: int(event.Index), Text: text})

		case "content_block_stop":
			kind, ok := streaming[event.Index]
			if !ok {
				continue
			}

			delete(streaming, event.Index)
			fn(llm.Delta{Kind: kind, Index: int(event.Index), Final: true})

		case "message_stop":
			stopped = true
		}
	}

	err = stream.Err()
	if err != nil {
		return nil, badRequestHint(err, req)
	}

	if !stopped {
		return nil, fmt.Errorf("stream ended without message_stop after %d content blocks", len(msg.Content))
	}

	// Anthropic stops every block it starts, but a proxy that renders the stream
	// itself is the same door the message_stop guard is here for, and a block left
	// open would leave its consumer holding the tail of it. Ascending index so the
	// order does not depend on the map.
	for _, idx := range slices.Sorted(maps.Keys(streaming)) {
		fn(llm.Delta{Kind: streaming[idx], Index: int(idx), Final: true})
	}

	resp, err := ResponseToNeutral(&msg)
	if err != nil {
		return nil, err
	}

	return &resp, nil
}

// deltaFragment maps a content_block_delta onto a neutral fragment, reporting
// false for a delta that produces none.
//
// Only the two deltas the neutral model renders produce a fragment.
// signature_delta arrives beside thinking_delta for the same block and carries the
// opaque signature, which llm.ThinkingBlock keeps as bytes and nothing renders;
// forwarding it would put it on the wire as delta text, undoing on the streamed
// path the strip the assembled path performs. input_json_delta is deliberately not
// streamed at all, for the reason llm.Delta gives: PreToolUse cannot run until the
// tool_use block exists, so arguments reach a consumer only in the assembled turn.
// citations_delta carries a citation rather than text.
//
// The signature, the tool arguments and the citations all reach the caller in the
// assembled Response, which carries every block in full.
func deltaFragment(event sdk.MessageStreamEventUnion) (llm.DeltaKind, string, bool) {
	switch event.Delta.Type {
	case "text_delta":
		return llm.DeltaText, event.Delta.Text, true

	case "thinking_delta":
		return llm.DeltaThinking, event.Delta.Thinking, true
	}

	return "", "", false
}
