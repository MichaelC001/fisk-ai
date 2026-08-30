//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/llm"
)

// A fixture is one turn stated once, as the JSON body Messages.New answers with,
// and streamFrames derives the SSE frames Messages.NewStreaming would answer with
// for that same message. Hand-writing both bodies would let them drift apart and
// hide exactly the divergence these specs exist to catch.

// canonicalTurn is a fixture's message decomposed far enough to render the frames
// from it. Everything that streams incrementally is read as raw JSON, so a
// fragment carries the fixture's own bytes rather than a re-encoding of them.
type canonicalTurn struct {
	ID           string            `json:"id"`
	Role         string            `json:"role"`
	Model        string            `json:"model"`
	Content      []json.RawMessage `json:"content"`
	StopReason   json.RawMessage   `json:"stop_reason"`
	StopSequence json.RawMessage   `json:"stop_sequence"`
	Usage        canonicalUsage    `json:"usage"`
}

// canonicalUsage is the usage a fixture's message reports. message_start carries
// the counts the turn began with and message_delta the final output counts, so
// this is the total the two of them have to accumulate to.
type canonicalUsage struct {
	InputTokens              int64           `json:"input_tokens"`
	OutputTokens             int64           `json:"output_tokens"`
	CacheReadInputTokens     int64           `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64           `json:"cache_creation_input_tokens"`
	OutputTokensDetails      json.RawMessage `json:"output_tokens_details"`
}

// canonicalBlock is one content block of a fixture, read for the fields the
// streamed form builds up incrementally.
type canonicalBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Signature string          `json:"signature"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
}

// firstOutputTokens is the output_tokens message_start reports, before the model
// has written the turn. message_delta overwrites it with the final count, so the
// value here is a starting point rather than part of the total.
const firstOutputTokens = 1

// streamFrames renders the SSE frames the Messages API would answer with for the
// canonical message, splitting each incrementally streamed payload into at most
// fragments pieces. The order is the one the API sends: message_start with the
// message minus its content, then each block's start, deltas and stop, then
// message_delta with the stop reason and final usage, then message_stop.
func streamFrames(message string, fragments int) ([]string, error) {
	var turn canonicalTurn

	err := json.Unmarshal([]byte(message), &turn)
	if err != nil {
		return nil, fmt.Errorf("canonical message: %w", err)
	}

	frames := []string{sseEvent("message_start", messageStartFrame(turn))}

	for i, raw := range turn.Content {
		var block canonicalBlock

		err = json.Unmarshal(raw, &block)
		if err != nil {
			return nil, fmt.Errorf("content block %d: %w", i, err)
		}

		start, deltas := blockFrames(i, raw, block, fragments)

		frames = append(frames, sseEvent("content_block_start", start))
		frames = append(frames, deltas...)
		frames = append(frames, sseEvent("content_block_stop", blockStop(i)))
	}

	frames = append(frames, sseEvent("message_delta", messageDeltaFrame(turn)))
	frames = append(frames, sseEvent("message_stop", messageStop))

	return frames, nil
}

// messageStartFrame renders the opening frame: the message with no content yet,
// no stop reason, and the input and cache counts the turn began with.
func messageStartFrame(turn canonicalTurn) string {
	return fmt.Sprintf(`{"type":"message_start","message":{"id":%q,"type":"message","role":%q,"model":%q,"content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":%d,"output_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d}}}`,
		turn.ID, turn.Role, turn.Model, turn.Usage.InputTokens, firstOutputTokens, turn.Usage.CacheReadInputTokens, turn.Usage.CacheCreationInputTokens)
}

// messageDeltaFrame renders the closing frame, which carries the stop reason and
// the output counts. Accumulate overwrites output_tokens with this one and leaves
// the counts message_delta omits at what message_start reported, so the two frames
// together add up to the canonical message's usage.
func messageDeltaFrame(turn canonicalTurn) string {
	usage := fmt.Sprintf(`{"output_tokens":%d`, turn.Usage.OutputTokens)
	if len(turn.Usage.OutputTokensDetails) > 0 {
		usage += fmt.Sprintf(`,"output_tokens_details":%s`, turn.Usage.OutputTokensDetails)
	}
	usage += "}"

	return fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%s,"stop_sequence":%s},"usage":%s}`,
		orNull(turn.StopReason), orNull(turn.StopSequence), usage)
}

// orNull renders a raw JSON value, or null when the fixture left it out.
func orNull(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "null"
	}

	return string(raw)
}

// blockFrames renders the frames one content block streams as: the
// content_block_start carrying the block with its incremental payload emptied,
// and the deltas that build that payload back up. A block whose payload does not
// stream, redacted_thinking among them, starts with its canonical JSON verbatim
// and sends no deltas at all.
func blockFrames(index int, raw json.RawMessage, block canonicalBlock, fragments int) (string, []string) {
	var deltas []string

	switch block.Type {
	case "text":
		for _, piece := range splitFragments(block.Text, fragments) {
			deltas = append(deltas, sseEvent("content_block_delta", textDelta(index, piece)))
		}

		return startText(index), deltas

	case "thinking":
		for _, piece := range splitFragments(block.Thinking, fragments) {
			deltas = append(deltas, sseEvent("content_block_delta", thinkingDelta(index, piece)))
		}
		// The signature is not split: it arrives whole, in one delta beside the last of
		// the reasoning.
		deltas = append(deltas, sseEvent("content_block_delta", signatureDelta(index, block.Signature)))

		return startThinking(index), deltas

	case "tool_use":
		for _, piece := range splitFragments(string(block.Input), fragments) {
			deltas = append(deltas, sseEvent("content_block_delta", inputJSONDelta(index, piece)))
		}

		return startToolUse(index, block.ID, block.Name), deltas
	}

	return fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":%s}`, index, raw), nil
}

// startToolUse is the start frame of a tool_use block: the call is named up front
// and only its arguments stream, so input opens as an empty object that the
// input_json_delta fragments replace.
func startToolUse(index int, id string, name string) string {
	return fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%q,"name":%q,"input":{}}}`, index, id, name)
}

func inputJSONDelta(index int, partial string) string {
	return fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%q}}`, index, partial)
}

// splitFragments cuts s into at most n pieces on rune boundaries, so the pieces
// concatenated are s again. Every piece but the last takes the same number of
// runes, which keeps the split deterministic and lets a spec assert the
// concatenation exactly. An empty payload produces no pieces.
func splitFragments(s string, n int) []string {
	if s == "" {
		return nil
	}
	if n < 1 {
		n = 1
	}

	runes := []rune(s)
	if n > len(runes) {
		n = len(runes)
	}

	size := (len(runes) + n - 1) / n

	var out []string
	for start := 0; start < len(runes); start += size {
		out = append(out, string(runes[start:min(start+size, len(runes))]))
	}

	return out
}

// equivalenceFixture is one turn rendered two ways.
type equivalenceFixture struct {
	// name says what the fixture is there to catch.
	name string
	// message is the canonical Anthropic message, the JSON body Messages.New answers
	// with. The streamed form is derived from it.
	message string
	// fragments is how many pieces each incrementally streamed payload is split into.
	fragments int
	// minFragments is the number of neutral fragments the turn must report. Without
	// it a helper that quietly stopped splitting would still satisfy the equivalence.
	minFragments int
	// minInputPieces is how many input_json_delta frames a turn with a tool_use block
	// must carry. Those frames report no neutral fragment, so minFragments cannot see
	// them, and a single piece takes Accumulate's wholesale-replace branch rather than
	// the append branch this exercises.
	minInputPieces int
}

var equivalenceFixtures = []equivalenceFixture{
	{
		name:         "a plain text turn",
		message:      `{"id":"msg_text","type":"message","role":"assistant","model":"claude-test-20260101","content":[{"type":"text","text":"Hello world"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":11,"output_tokens":25,"cache_read_input_tokens":3,"cache_creation_input_tokens":5}}`,
		fragments:    1,
		minFragments: 1,
	},
	{
		name:         "text spanning several fragments",
		message:      `{"id":"msg_split","type":"message","role":"assistant","model":"claude-test-20260101","content":[{"type":"text","text":"The answer arrives in pieces and is assembled at the end."}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":11,"output_tokens":31,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`,
		fragments:    4,
		minFragments: 4,
	},
	{
		name:         "a thinking block with its signature",
		message:      `{"id":"msg_think","type":"message","role":"assistant","model":"claude-test-20260101","content":[{"type":"thinking","thinking":"Weighing the options before answering.","signature":"c2lnbmF0dXJl"},{"type":"text","text":"The considered answer."}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":18,"output_tokens":52,"cache_read_input_tokens":4,"cache_creation_input_tokens":0,"output_tokens_details":{"thinking_tokens":30}}}`,
		fragments:    3,
		minFragments: 6,
	},
	{
		name:           "a tool_use whose input arrives as fragments",
		message:        `{"id":"msg_tool","type":"message","role":"assistant","model":"claude-test-20260101","content":[{"type":"text","text":"Checking the tree."},{"type":"tool_use","id":"toolu_01","name":"shell","input":{"command":"ls -l","timeout":30}}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":22,"output_tokens":44,"cache_read_input_tokens":0,"cache_creation_input_tokens":9}}`,
		fragments:      3,
		minFragments:   3,
		minInputPieces: 3,
	},
	{
		name:           "several block kinds including a redacted_thinking that streams nothing",
		message:        `{"id":"msg_mixed","type":"message","role":"assistant","model":"claude-test-20260101","content":[{"type":"redacted_thinking","data":"c3VwcHJlc3NlZA=="},{"type":"thinking","thinking":"Reasoning that is shown.","signature":"c2lnLXR3bw=="},{"type":"text","text":"Here is the plan."},{"type":"tool_use","id":"toolu_02","name":"shell","input":{"command":"go test ./..."}}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":31,"output_tokens":77,"cache_read_input_tokens":6,"cache_creation_input_tokens":2,"output_tokens_details":{"thinking_tokens":19}}}`,
		fragments:      3,
		minFragments:   6,
		minInputPieces: 3,
	},
}

var _ = Describe("Provider.CallStream equivalence with Provider.Call", func() {
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

	// twoWays answers one turn both ways from a single server, telling a batched
	// request from a streamed one by the stream property the SDK sets on the request
	// body. Both calls run through the real SDK over real HTTP, so how the message
	// arrives is the only difference between them.
	twoWays := func(message string, frames []string) *Provider {
		body := strings.Join(frames, "")

		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}

			var probe struct {
				Stream bool `json:"stream"`
			}
			err = json.Unmarshal(raw, &probe)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}

			if probe.Stream {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, body)
				return
			}

			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, message)
		}))

		return NewProvider(Options{APIKey: "test-key", BaseURL: server.URL, Timeout: 5 * time.Second})
	}

	for _, fixture := range equivalenceFixtures {
		It("returns the same response either way for "+fixture.name, func() {
			frames, err := streamFrames(fixture.message, fixture.fragments)
			Expect(err).NotTo(HaveOccurred())

			inputPieces := 0
			for _, frame := range frames {
				if strings.Contains(frame, `"type":"input_json_delta"`) {
					inputPieces++
				}
			}
			Expect(inputPieces).To(BeNumerically(">=", fixture.minInputPieces))

			p := twoWays(fixture.message, frames)

			batched, err := p.Call(context.Background(), req)
			Expect(err).NotTo(HaveOccurred())

			var deltas []llm.Delta
			streamed, err := p.CallStream(context.Background(), req, func(d llm.Delta) {
				deltas = append(deltas, d)
			})
			Expect(err).NotTo(HaveOccurred())

			// The whole design rests on this: content blocks, stop reason and every usage
			// counter the codec reads, compared as one value rather than field by field
			// against numbers a spec author chose.
			Expect(streamed).To(Equal(batched))

			// The other half of the claim, and what every consumer above reconciles against:
			// the fragments for an index concatenate to that block's text, and an index that
			// streamed is finished exactly once.
			joined := map[int]string{}
			finals := map[int]int{}
			reported := 0

			for _, d := range deltas {
				Expect(d.Index).To(BeNumerically("<", len(streamed.Content)))

				if d.Final {
					Expect(d.Text).To(BeEmpty(), "a Final delta carries what is left, and nothing is")
					finals[d.Index]++
					continue
				}

				reported++
				joined[d.Index] += d.Text
			}

			// Only text and thinking stream. A tool_use block's arguments deliberately do
			// not, and a redacted_thinking block has nothing to send, so neither appears
			// here while both still hold their own index in the assembled content.
			want := map[int]string{}
			for i, block := range streamed.Content {
				switch {
				case block.Text != nil && block.Text.Text != "":
					want[i] = block.Text.Text
				case block.Thinking != nil && block.Thinking.Text != "":
					want[i] = block.Thinking.Text
				}
			}

			Expect(joined).To(Equal(want))
			Expect(reported).To(BeNumerically(">=", fixture.minFragments))

			oneEach := map[int]int{}
			for i := range want {
				oneEach[i] = 1
			}
			Expect(finals).To(Equal(oneEach))
		})
	}
})
