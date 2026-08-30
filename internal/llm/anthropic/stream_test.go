//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package anthropic

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/llm"
)

// sseEvent renders one server-sent event frame the way the Messages API writes
// them, terminated by the blank line the SDK decoder dispatches on.
func sseEvent(typ string, data string) string {
	return fmt.Sprintf("event: %s\ndata: %s\n\n", typ, data)
}

// The frames a turn is built from. A test names the ones it needs and leaves out
// the ones it is about, such as the message_stop a truncated stream never sends.
const (
	messageStart = `{"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-test-20260101","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":11,"output_tokens":1,"cache_read_input_tokens":3,"cache_creation_input_tokens":5}}}`
	messageDelta = `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":25}}`
	messageStop  = `{"type":"message_stop"}`
)

func startText(index int) string {
	return fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, index)
}

func startThinking(index int) string {
	return fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"thinking","thinking":"","signature":""}}`, index)
}

func startRedactedThinking(index int) string {
	return fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"redacted_thinking","data":"c2VjcmV0"}}`, index)
}

func textDelta(index int, text string) string {
	return fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%q}}`, index, text)
}

func thinkingDelta(index int, text string) string {
	return fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"thinking_delta","thinking":%q}}`, index, text)
}

func signatureDelta(index int, sig string) string {
	return fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"signature_delta","signature":%q}}`, index, sig)
}

func blockStop(index int) string {
	return fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, index)
}

var _ = Describe("Provider.CallStream", func() {
	var (
		server *httptest.Server
		req    llm.Request
	)

	BeforeEach(func() {
		req = llm.Request{
			Model:           "claude-test",
			MaxOutputTokens: 1024,
			Messages:        []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Text: &llm.TextBlock{Text: "go"}}}}},
		}
	})

	AfterEach(func() {
		if server != nil {
			server.Close()
			server = nil
		}
	})

	// streaming points a provider at a test server that answers every request with
	// the given event frames, which is the whole indirection the tests need: the SDK
	// takes a base URL, so the stream is driven over real HTTP with no test-only code
	// on the provider.
	streaming := func(frames ...string) *Provider {
		body := strings.Join(frames, "")

		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, body)
		}))

		return NewProvider(Options{APIKey: "test-key", BaseURL: server.URL, Timeout: 5 * time.Second})
	}

	// collect runs a call and returns the fragments it reported alongside its result.
	collect := func(p *Provider) ([]llm.Delta, *llm.Response, error) {
		var deltas []llm.Delta

		resp, err := p.CallStream(context.Background(), req, func(d llm.Delta) {
			deltas = append(deltas, d)
		})

		return deltas, resp, err
	}

	It("assembles the same response a batched call returns", func() {
		p := streaming(
			sseEvent("message_start", messageStart),
			sseEvent("content_block_start", startText(0)),
			sseEvent("content_block_delta", textDelta(0, "Hello ")),
			sseEvent("content_block_delta", textDelta(0, "world")),
			sseEvent("content_block_stop", blockStop(0)),
			sseEvent("message_delta", messageDelta),
			sseEvent("message_stop", messageStop),
		)

		deltas, resp, err := collect(p)
		Expect(err).NotTo(HaveOccurred())

		Expect(resp.ID).To(Equal("msg_01"))
		Expect(resp.Model).To(Equal("claude-test-20260101"), "the snapshot that answered, not the alias asked for")
		Expect(resp.StopReason).To(Equal(llm.StopEndTurn))
		Expect(resp.Usage).To(Equal(llm.Usage{In: 11, Out: 25, CacheRead: 3, CacheCreate: 5}))
		Expect(resp.Content).To(HaveLen(1))
		Expect(resp.Content[0].Text.Text).To(Equal("Hello world"))

		Expect(deltas).To(Equal([]llm.Delta{
			{Kind: llm.DeltaText, Index: 0, Text: "Hello "},
			{Kind: llm.DeltaText, Index: 0, Text: "world"},
			{Kind: llm.DeltaText, Index: 0, Final: true},
		}))
	})

	// The failure the streamed path can produce and the batched path cannot: the SDK
	// reports no error for a body that simply ends, so without this the caller gets a
	// truncated turn with an empty stop reason and journals it as the final answer.
	It("errors when the stream ends without message_stop", func() {
		p := streaming(
			sseEvent("message_start", messageStart),
			sseEvent("content_block_start", startText(0)),
			sseEvent("content_block_delta", textDelta(0, "half an ans")),
		)

		deltas, resp, err := collect(p)
		Expect(err).To(MatchError(ContainSubstring("stream ended without message_stop")))
		Expect(resp).To(BeNil())

		// The fragments already reported are not withdrawn; the caller discards them
		// because the call failed.
		Expect(deltas).To(HaveLen(1))
	})

	It("reports a thinking_delta and not the signature that arrives beside it", func() {
		p := streaming(
			sseEvent("message_start", messageStart),
			sseEvent("content_block_start", startThinking(0)),
			sseEvent("content_block_delta", thinkingDelta(0, "weighing it up")),
			sseEvent("content_block_delta", signatureDelta(0, "c2lnbmF0dXJl")),
			sseEvent("content_block_stop", blockStop(0)),
			sseEvent("message_delta", messageDelta),
			sseEvent("message_stop", messageStop),
		)

		deltas, resp, err := collect(p)
		Expect(err).NotTo(HaveOccurred())

		Expect(deltas).To(Equal([]llm.Delta{
			{Kind: llm.DeltaThinking, Index: 0, Text: "weighing it up"},
			{Kind: llm.DeltaThinking, Index: 0, Final: true},
		}))

		// The signature is not lost, it is only not streamed: it reaches the caller in
		// the assembled block, where nothing renders it.
		Expect(resp.Content).To(HaveLen(1))
		Expect(resp.Content[0].Thinking.Text).To(Equal("weighing it up"))
		Expect(string(resp.Content[0].Thinking.Signature)).To(Equal("c2lnbmF0dXJl"))
	})

	It("indexes every fragment by its block's position in the returned content", func() {
		p := streaming(
			sseEvent("message_start", messageStart),
			sseEvent("content_block_start", startRedactedThinking(0)),
			sseEvent("content_block_stop", blockStop(0)),
			sseEvent("content_block_start", startThinking(1)),
			sseEvent("content_block_delta", thinkingDelta(1, "reasoning")),
			sseEvent("content_block_start", startText(2)),
			// Blocks interleave: a fragment for the open thinking block arrives after the
			// text block has started.
			sseEvent("content_block_delta", textDelta(2, "answer")),
			sseEvent("content_block_delta", thinkingDelta(1, " more")),
			sseEvent("content_block_stop", blockStop(1)),
			sseEvent("content_block_stop", blockStop(2)),
			sseEvent("message_delta", messageDelta),
			sseEvent("message_stop", messageStop),
		)

		deltas, resp, err := collect(p)
		Expect(err).NotTo(HaveOccurred())

		Expect(resp.Content).To(HaveLen(3))
		Expect(resp.Content[0].Provider.Kind).To(Equal("redacted_thinking"))
		Expect(resp.Content[1].Thinking.Text).To(Equal("reasoning more"))
		Expect(resp.Content[2].Text.Text).To(Equal("answer"))

		Expect(deltas).To(Equal([]llm.Delta{
			{Kind: llm.DeltaThinking, Index: 1, Text: "reasoning"},
			{Kind: llm.DeltaText, Index: 2, Text: "answer"},
			{Kind: llm.DeltaThinking, Index: 1, Text: " more"},
			{Kind: llm.DeltaThinking, Index: 1, Final: true},
			{Kind: llm.DeltaText, Index: 2, Final: true},
		}))

		// The redacted thinking block streamed nothing, so it has no fragments and no
		// Final, and the blocks after it are still addressed by their own position.
		for _, d := range deltas {
			Expect(d.Index).NotTo(Equal(0))
		}
	})

	It("reports one Final for every block that streamed", func() {
		p := streaming(
			sseEvent("message_start", messageStart),
			sseEvent("content_block_start", startText(0)),
			sseEvent("content_block_delta", textDelta(0, "one")),
			sseEvent("content_block_stop", blockStop(0)),
			sseEvent("content_block_start", startText(1)),
			sseEvent("content_block_delta", textDelta(1, "two")),
			sseEvent("content_block_stop", blockStop(1)),
			sseEvent("message_delta", messageDelta),
			sseEvent("message_stop", messageStop),
		)

		deltas, resp, err := collect(p)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Content).To(HaveLen(2))

		finals := map[int]int{}
		for _, d := range deltas {
			if d.Final {
				Expect(d.Text).To(BeEmpty(), "a Final delta carries what is left, and nothing is")
				finals[d.Index]++
			}
		}
		Expect(finals).To(Equal(map[int]int{0: 1, 1: 1}))
	})

	// Anthropic stops every block it starts. A proxy rendering the stream itself is
	// the case, and a consumer holding an unfinished block would keep its tail.
	It("finals a block the stream left open, in index order", func() {
		p := streaming(
			sseEvent("message_start", messageStart),
			sseEvent("content_block_start", startText(0)),
			sseEvent("content_block_delta", textDelta(0, "one")),
			sseEvent("content_block_start", startText(1)),
			sseEvent("content_block_delta", textDelta(1, "two")),
			sseEvent("message_delta", messageDelta),
			sseEvent("message_stop", messageStop),
		)

		deltas, resp, err := collect(p)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Content).To(HaveLen(2))

		Expect(deltas).To(Equal([]llm.Delta{
			{Kind: llm.DeltaText, Index: 0, Text: "one"},
			{Kind: llm.DeltaText, Index: 1, Text: "two"},
			{Kind: llm.DeltaText, Index: 0, Final: true},
			{Kind: llm.DeltaText, Index: 1, Final: true},
		}))
	})

	It("adds the reasoning hint to a 400 the same way a batched call does", func() {
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"type":"error","error":{"type":"invalid_request_error","message":"thinking is not supported"}}`)
		}))
		p := NewProvider(Options{APIKey: "test-key", BaseURL: server.URL, Timeout: 5 * time.Second})

		req.Thinking = llm.ThinkingOn
		req.ReasoningEffort = "high"

		deltas, resp, err := collect(p)
		Expect(err).To(MatchError(ContainSubstring(`may not accept a thinking parameter or the effort level "high"`)))
		Expect(resp).To(BeNil())
		Expect(deltas).To(BeEmpty())
	})

	It("refuses a call with no delta function", func() {
		p := NewProvider(Options{APIKey: "test-key", Timeout: 5 * time.Second})

		resp, err := p.CallStream(context.Background(), req, nil)
		Expect(err).To(MatchError(ContainSubstring("delta function is required")))
		Expect(resp).To(BeNil())
	})
})
