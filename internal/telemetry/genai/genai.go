//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// Package genai renders the GenAI semantic conventions' content documents from the
// neutral llm model, for the opt-in content capture the telemetry package exports.
//
// It is a subpackage rather than part of internal/telemetry because that package is a
// hard leaf: it imports the standard library and OpenTelemetry and nothing else from
// this repository, so that no future instrumentation can create an import cycle. These
// builders need internal/llm, so they live one level down where importing both is
// allowed. Nothing here imports OpenTelemetry; a content attribute is plain JSON and
// the leaf is what turns the result into an attribute.
//
// Every builder is lazy: it returns a telemetry.ContentBuilder that does the work only
// if the span is going to record it, so a run with capture off serializes nothing.
package genai

import (
	"encoding/json"
	"strconv"
	"unicode/utf8"

	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/telemetry"
)

// The part types of the GenAI messages schemas.
const (
	partText             = "text"
	partReasoning        = "reasoning"
	partToolCall         = "tool_call"
	partToolCallResponse = "tool_call_response"

	// partProviderBlock is this package's own marker for a block the neutral model
	// preserves without understanding, kept in the document so the shape of a turn
	// survives while the payload does not. See toParts.
	//
	// The conventions define no such type, so the token names fisk-ai in the emitted
	// JSON and a reader can tell it from a part the spec defines. The fisk.* attribute
	// keys are namespaced for the same reason.
	partProviderBlock = "fisk.provider_block"
)

// roleTool is the conventions' role for a message carrying tool results. The neutral
// model batches results into a user message, which is the shape the provider wants;
// the conventions give them their own role, which is the shape a GenAI view renders.
const roleTool = "tool"

// message is one entry of an input or output messages document.
//
// The document carries no version of its own, and cannot: the conventions define
// gen_ai.input.messages and its siblings as a bare JSON array of these objects, so
// there is nowhere to put one that a GenAI-aware reader would not choke on. The keys
// are imported from the semconv package at their use site in internal/telemetry, so a
// key the conventions rename fails the build; these bodies are hand-encoded, so a shape
// the conventions change reaches a backend as a document it misparses without saying so.
//
// What dates a document is the build that wrote it: service.version on the resource
// is this binary's version, and the conventions revision it encodes to is the semconv
// package that build imported. A reader chasing a document that will not parse reads
// that pair rather than looking for a version in the JSON.
type message struct {
	Role  string `json:"role"`
	Parts []part `json:"parts"`
	// FinishReason appears on output messages only.
	FinishReason string `json:"finish_reason,omitempty"`
}

// part is one element of a message's content. The schemas define a distinct object
// per part type; one struct with omitted empties renders all of them and keeps the
// truncation code from having to switch on a type union.
type part struct {
	Type      string          `json:"type"`
	Content   string          `json:"content,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	// Kind and Omitted describe a provider block whose payload is deliberately not
	// exported.
	Kind    string `json:"kind,omitempty"`
	Omitted bool   `json:"omitted,omitempty"`
}

// InputMessages renders the conversation sent to the model.
//
// from is the index the turn's delta starts at, and is clamped here rather than
// trusted. The run's message slice is replaced wholesale on a context reset and on a
// session rotation, so an index taken before either would slice out of range, and a
// panic in an opt-in observability feature would end the run through the panic
// barrier. Clamping is the half of that guard the compiler cannot let rot.
func InputMessages(msgs []llm.Message, from int) telemetry.ContentBuilder {
	return func(o telemetry.ContentOptions) telemetry.Content {
		start := 0
		if !o.Full {
			start = clamp(from, 0, len(msgs))
		}

		items := make([]message, 0, len(msgs)-start)
		for _, m := range msgs[start:] {
			items = append(items, toMessage(m, ""))
		}

		return renderMessages(items, start, o)
	}
}

// OutputMessages renders the model's reply as the single message it is.
func OutputMessages(blocks []llm.ContentBlock, finish string) telemetry.ContentBuilder {
	return func(o telemetry.ContentOptions) telemetry.Content {
		m := toMessage(llm.Message{Role: llm.RoleAssistant, Content: blocks}, finish)

		return renderMessages([]message{m}, 0, o)
	}
}

// SystemInstructions renders the system prompt segments.
//
// An empty segment is skipped rather than rendered. The prompt is assembled by
// appending optional notes to the configured one, so an agent that configures no system
// prompt of its own leads the list with an empty string, and a text part carrying no
// text is not a document a reader can do anything with. Found by reading a decoded
// export: every assertion on the shape of this attribute passed while its first element
// was {"type":"text"} and nothing else.
func SystemInstructions(blocks []string) telemetry.ContentBuilder {
	return func(o telemetry.ContentOptions) telemetry.Content {
		parts := make([]part, 0, len(blocks))
		for _, b := range blocks {
			if b == "" {
				continue
			}
			parts = append(parts, part{Type: partText, Content: b})
		}

		return renderParts(parts, o)
	}
}

// ToolArguments renders a tool call's arguments.
//
// raw is the model's own bytes, so it is validated before it is embedded. The runner
// already applies the same rule to a hook's rewritten arguments; embedding an invalid
// RawMessage would fail the marshal and lose the whole document, and there is a
// truthful fallback available (the bytes, as a string).
func ToolArguments(raw json.RawMessage) telemetry.ContentBuilder {
	return func(o telemetry.ContentOptions) telemetry.Content {
		return renderValue(raw, o)
	}
}

// ToolResult renders what a tool returned to the model.
//
// The conventions ask an instrumentation to deserialize a serialized result where it
// can, which is worth doing here: the command envelope and every built-in already
// return JSON, so the common case renders as an object rather than as a quoted blob.
//
// It is rendered for a failed call too, though the conventions describe this attribute
// as the result of a successful one. The error text is what the model was told and is
// usually the reason a trace is being read at all; fisk.tool.outcome and error.type on
// the same span already say the call failed.
func ToolResult(content string) telemetry.ContentBuilder {
	return func(o telemetry.ContentOptions) telemetry.Content {
		return renderValue(json.RawMessage(content), o)
	}
}

// toMessage maps one neutral message onto the schema shape.
func toMessage(m llm.Message, finish string) message {
	parts := toParts(m.Content)

	// A message whose every part answers a tool call is the conventions' tool role.
	// A mixed message keeps the neutral role, which happens when an interactive
	// follow-up is folded into a trailing tool-results turn.
	role := string(m.Role)
	if len(parts) > 0 && allToolResponses(parts) {
		role = roleTool
	}

	return message{Role: role, Parts: parts, FinishReason: finish}
}

// toParts maps the neutral content blocks onto schema parts.
//
// Two payloads never leave the process. A thinking block's Signature is opaque
// provider bytes the neutral model only preserves, large and meaningless off-box. A
// provider block's Raw is provider JSON nothing in this process has inspected, and on
// the Anthropic backend it is where a server-side tool search result lives, which is
// content this build never reviewed. The block is kept as a marker rather than
// dropped, so a reader sees that the turn had a part there and does not read the
// absence as a instrumentation gap.
func toParts(blocks []llm.ContentBlock) []part {
	parts := make([]part, 0, len(blocks))

	for _, b := range blocks {
		switch {
		case b.Text != nil:
			// An empty text block carries nothing a reader can use, and the part would
			// claim to be text with no text.
			if b.Text.Text == "" {
				continue
			}
			parts = append(parts, part{Type: partText, Content: b.Text.Text})

		case b.Thinking != nil:
			// Redacted reasoning arrives as a signature with no readable text. The part
			// is kept and marked rather than dropped, on the same reasoning as a
			// provider block below: that the model reasoned here is itself information,
			// and an absence would read as an instrumentation gap.
			if b.Thinking.Text == "" {
				parts = append(parts, part{Type: partReasoning, Omitted: true})
				continue
			}
			parts = append(parts, part{Type: partReasoning, Content: b.Thinking.Text})

		case b.ToolUse != nil:
			args := b.ToolUse.Input
			if !json.Valid(args) {
				args = quote(string(args))
			}
			parts = append(parts, part{Type: partToolCall, ID: b.ToolUse.ID, Name: b.ToolUse.Name, Arguments: args})

		case b.ToolResult != nil:
			result := json.RawMessage(b.ToolResult.Content)
			if !json.Valid(result) {
				result = quote(b.ToolResult.Content)
			}
			parts = append(parts, part{Type: partToolCallResponse, ID: b.ToolResult.ToolUseID, Result: result})

		case b.Provider != nil:
			parts = append(parts, part{Type: partProviderBlock, Kind: b.Provider.Kind, Omitted: true})
		}
	}

	return parts
}

// allToolResponses reports whether every part answers a tool call.
func allToolResponses(parts []part) bool {
	for _, p := range parts {
		if p.Type != partToolCallResponse {
			return false
		}
	}

	return true
}

// renderMessages assembles a messages document that fits the budget.
//
// Each message is marshaled once and measured, then the longest suffix that fits is
// kept: dropping from the front keeps the newest content, which is what a reader is
// looking at. Marshaling once per message rather than re-marshaling the whole document
// per attempt matters more than it looks, because the conversation only grows and a
// model can be induced to put one very large tool output in it, so a re-marshal loop
// would be quadratic work an operator does not control.
func renderMessages(items []message, fromIndex int, o telemetry.ContentOptions) telemetry.Content {
	out := telemetry.Content{FromIndex: fromIndex}

	if len(items) == 0 {
		out.JSON = "[]"
		return out
	}

	docs := make([]json.RawMessage, len(items))
	// The encoded array is the sum of its elements plus one comma between each and
	// the two brackets, which is len(items)+1 over the sum.
	total := len(items) + 1
	for i := range items {
		b, err := json.Marshal(items[i])
		if err != nil {
			return telemetry.Content{}
		}
		docs[i] = b
		total += len(b)
	}

	start := 0
	for start < len(docs)-1 && total > o.MaxBytes {
		total -= len(docs[start]) + 1
		start++
	}

	// A kept message that answers tool calls which were dropped is worse than one
	// message shorter: its ids reference nothing anywhere in the trace, so a GenAI
	// view renders tool output attributed to calls that appear never to have happened,
	// and the first reading of that is "the instrumentation lost spans".
	for start < len(docs)-1 && allToolResponses(items[start].Parts) && len(items[start].Parts) > 0 {
		total -= len(docs[start]) + 1
		start++
	}

	if start > 0 {
		out.Truncated = true
		out.DroppedMessages = start
		out.FromIndex = fromIndex + start
	}

	// The newest message can exceed the budget by itself, which is the ordinary case
	// for one large tool result rather than an edge case.
	if total > o.MaxBytes {
		trimmed, ok := trimMessage(items[len(items)-1], o.MaxBytes-2)
		if !ok {
			return telemetry.Content{}
		}
		out.Truncated = true
		out.DroppedMessages = len(items) - 1
		out.FromIndex = fromIndex + len(items) - 1
		docs = []json.RawMessage{trimmed}
	} else {
		docs = docs[start:]
	}

	b, err := json.Marshal(docs)
	if err != nil {
		return telemetry.Content{}
	}
	out.JSON = string(b)

	return out
}

// renderParts assembles a bare parts document, which is the shape system instructions
// take. The budget is shared evenly rather than spent front to back, so one long
// segment cannot silently consume every other segment's room.
func renderParts(parts []part, o telemetry.ContentOptions) telemetry.Content {
	var out telemetry.Content

	// No parts means no document. An empty array is valid JSON and would set the
	// attribute, which then claims there were instructions and that they were empty;
	// absent says the true thing, that this run configured none.
	if len(parts) == 0 {
		return out
	}

	b, err := json.Marshal(parts)
	if err != nil {
		return telemetry.Content{}
	}
	if len(b) <= o.MaxBytes {
		out.JSON = string(b)
		return out
	}

	trimmed, ok := fitParts(parts, o.MaxBytes-2)
	if !ok {
		return telemetry.Content{}
	}

	out.Truncated = true
	out.JSON = string(trimmed)

	return out
}

// renderValue assembles a single-value document, which is the shape a tool call's
// arguments and its result take. A value that is not valid JSON is rendered as a JSON
// string of its bytes rather than dropped, and one that does not fit is replaced by a
// truncated string of itself: an object cannot be shortened structurally without
// choosing which keys matter, and a string that says so is more use than nothing.
func renderValue(raw json.RawMessage, o telemetry.ContentOptions) telemetry.Content {
	var out telemetry.Content

	if len(raw) == 0 {
		return out
	}

	if json.Valid(raw) && len(raw) <= o.MaxBytes {
		out.JSON = string(raw)
		return out
	}

	text, cut := fitText(string(raw), o.MaxBytes-2)
	out.Truncated = cut || json.Valid(raw)
	out.JSON = string(quote(text))

	return out
}

// trimMessage shortens one message's payloads so it fits budget.
//
// The skeleton is measured with every variable payload emptied, so what is left is
// genuinely available, and the remainder is shared evenly between them.
func trimMessage(m message, budget int) (json.RawMessage, bool) {
	trimmed, ok := fitParts(m.Parts, budget-messageOverhead(m))
	if !ok {
		return fallbackMessage(m.Role, budget)
	}

	var parts []part
	err := json.Unmarshal(trimmed, &parts)
	if err != nil {
		return fallbackMessage(m.Role, budget)
	}

	m.Parts = parts
	b, err := json.Marshal(m)
	if err != nil || len(b) > budget {
		return fallbackMessage(m.Role, budget)
	}

	return b, true
}

// messageOverhead is how many bytes a message costs before its parts.
func messageOverhead(m message) int {
	skeleton := message{Role: m.Role, FinishReason: m.FinishReason, Parts: []part{}}

	b, err := json.Marshal(skeleton)
	if err != nil {
		return 0
	}

	return len(b)
}

// fitParts shortens a parts array to fit budget, sharing the room evenly.
func fitParts(parts []part, budget int) (json.RawMessage, bool) {
	if budget <= 0 || len(parts) == 0 {
		return nil, false
	}

	// The skeleton keeps a one-character placeholder in every payload rather than
	// emptying it, and that is not cosmetic: Content is omitempty, so an empty string
	// takes the whole `,"content":""` wrapper out of the measurement, over-allocating
	// the room below by thirteen bytes per part. The parts then marshal past the budget,
	// the final check rejects them, and every one of them is replaced by the fallback
	// marker. The symptom is that a tight cap exports no content at all where it should
	// have exported most of it, and it is invisible to an assertion that only checks the
	// document is valid and within the cap, because the fallback is both.
	const placeholder = "x"

	skeleton := make([]part, len(parts))
	copy(skeleton, parts)
	payloads := 0
	for i := range skeleton {
		if skeleton[i].Content != "" {
			skeleton[i].Content = placeholder
			payloads++
		}
		if skeleton[i].Arguments != nil {
			skeleton[i].Arguments = json.RawMessage(`"` + placeholder + `"`)
			payloads++
		}
		if skeleton[i].Result != nil {
			skeleton[i].Result = json.RawMessage(`"` + placeholder + `"`)
			payloads++
		}
	}

	base, err := json.Marshal(skeleton)
	if err != nil {
		return nil, false
	}

	room := budget - len(base) + payloads*len(placeholder)
	if room <= 0 {
		return nil, false
	}
	share := room / len(parts)
	if share < minPayloadBytes {
		return nil, false
	}

	fitted := make([]part, len(parts))
	copy(fitted, parts)
	for i := range fitted {
		if fitted[i].Content != "" {
			fitted[i].Content, _ = fitText(fitted[i].Content, share)
		}
		if len(fitted[i].Arguments) > share {
			text, _ := fitText(string(fitted[i].Arguments), share)
			fitted[i].Arguments = quote(text)
		}
		if len(fitted[i].Result) > share {
			text, _ := fitText(string(fitted[i].Result), share)
			fitted[i].Result = quote(text)
		}
	}

	b, err := json.Marshal(fitted)
	if err != nil || len(b) > budget {
		return nil, false
	}

	return b, true
}

// minPayloadBytes is the least room worth giving one payload. Below it the document
// is all marker and no content, and the fallback says the same thing more honestly.
const minPayloadBytes = 32

// fallbackMessage is the smallest truthful document for a message that cannot be made
// to fit: the marker alone, so the reader learns the budget was the problem rather
// than receiving nothing at all.
func fallbackMessage(role string, budget int) (json.RawMessage, bool) {
	m := message{Role: role, Parts: []part{{Type: partText, Content: marker(0)}}}

	b, err := json.Marshal(m)
	if err != nil || len(b) > budget {
		return nil, false
	}

	return b, true
}

// fitText shortens s so that its encoded form fits budget, appending the marker when
// there is room for it.
//
// The marker is cosmetic and the span attribute is the signal, deliberately: a model
// can write this exact string into its own output, so a reader who trusts the marker
// alone can be told content was elided when it was not.
func fitText(s string, budget int) (string, bool) {
	if encodedLen(s) <= budget {
		return s, false
	}

	m := marker(len(s))
	room := budget - encodedLen(m)
	if room < minPayloadBytes {
		head, _ := truncateEncoded(s, budget)
		return head, true
	}

	head, _ := truncateEncoded(s, room)

	return head + m, true
}

// marker names the cause, the size and the fix, in the value the reader is already
// looking at. n is the original length, omitted when it is not known.
func marker(n int) string {
	if n == 0 {
		return "[truncated; raise telemetry.capture.max_bytes]"
	}

	return "...[truncated: " + strconv.Itoa(n) + " bytes; raise telemetry.capture.max_bytes]"
}

// truncateEncoded returns the longest prefix of s whose encoded form fits budget, and
// whether anything was removed. It never splits a rune.
func truncateEncoded(s string, budget int) (string, bool) {
	if budget <= 0 {
		return "", s != ""
	}

	n := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		c := encodedRuneLen(r, size)
		if n+c > budget {
			return s[:i], true
		}
		n += c
		i += size
	}

	return s, false
}

// encodedLen is how many bytes s occupies inside a JSON string, excluding the quotes.
func encodedLen(s string) int {
	n := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		n += encodedRuneLen(r, size)
		i += size
	}

	return n
}

// encodedRuneLen is how many bytes one rune costs once encoding/json has escaped it.
//
// This exists because a budget counted on the Go string does not bound the attribute.
// The encoder escapes the HTML set to six bytes each, so a tool result of angle
// brackets encodes to six times its length, and it replaces every invalid UTF-8 byte
// with a three-byte U+FFFD, which command output routinely contains. A spec over ASCII
// fixtures cannot see either.
func encodedRuneLen(r rune, size int) int {
	switch {
	case r == utf8.RuneError && size == 1:
		// An invalid byte. The encoder does not write a raw three-byte U+FFFD for it,
		// it writes a six-character backslash-u escape, so this costs six and not
		// three.
		// A validly encoded U+FFFD already in the input decodes at size 3 and falls
		// through to the default, where it is written raw.
		return 6
	case r == '"' || r == '\\':
		return 2
	case r == '\n' || r == '\r' || r == '\t':
		return 2
	case r < 0x20:
		return 6
	case r == '<' || r == '>' || r == '&':
		// encoding/json escapes these by default so a document is safe to embed in
		// HTML. It is the largest single source of expansion in practice.
		return 6
	case r == '\u2028' || r == '\u2029':
		// The line and paragraph separators, which the encoder also escapes.
		return 6
	default:
		return size
	}
}

// quote renders s as a JSON string.
func quote(s string) json.RawMessage {
	b, err := json.Marshal(s)
	if err != nil {
		return json.RawMessage(`""`)
	}

	return b
}

// clamp bounds v to [lo, hi].
func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}

	return v
}
