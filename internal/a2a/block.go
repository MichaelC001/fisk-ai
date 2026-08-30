//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	"encoding/json"
	"fmt"
	"strings"
)

// BlockType is the kind of one content block, carried as the suffix of the event's
// protocol id rather than as a field of the block itself.
type BlockType string

const (
	BlockThinking   BlockType = "thinking"
	BlockText       BlockType = "text"
	BlockToolCall   BlockType = "tool_call"
	BlockToolResult BlockType = "tool_result"
	BlockAgentCall  BlockType = "agent_call"
	BlockStatus     BlockType = "status"
	BlockWarning    BlockType = "warning"
	BlockPrompt     BlockType = "prompt"

	// BlockTextDelta and BlockThinkingDelta carry a fragment of a text block and of a
	// thinking block. They take the underscore form tool_call and tool_result use,
	// since blockTypeOf refuses a suffix carrying a dot of its own.
	BlockTextDelta     BlockType = "text_delta"
	BlockThinkingDelta BlockType = "thinking_delta"
)

// BlockContent is the content of a single event block. The concrete types are
// ThinkingBlock, TextBlock, PromptBlock, WarningBlock, ToolCallBlock,
// ToolResultBlock, AgentCallBlock, StatusBlock, TextDeltaBlock, ThinkingDeltaBlock
// and UnknownBlock, which is what a kind this build does not name decodes to.
//
// What kind a block is travels as the protocol id of the event carrying it and is not
// written into the block, so the content on the wire is the variant's own fields and
// nothing else.
type BlockContent interface {
	blockType() BlockType
}

// ThinkingBlock is reasoning output. Signature is opaque and provider defined;
// it is for display and audit only and is never replayed into a model across the
// agent boundary.
type ThinkingBlock struct {
	Text      string `json:"text"`
	Signature string `json:"signature,omitempty"`
	Provider  string `json:"provider,omitempty"`
	// Index is where this block sat in the model call that produced it, and pairs with
	// Iteration on the ThinkingDeltaBlock values that carried its fragments. It means
	// what TextBlock.Index means, for reasoning rather than answer text.
	Index int `json:"index,omitempty"`
	// Trimmed reports that Text was cut to MaxBlockText, on the terms
	// TextBlock.Trimmed states.
	Trimmed bool `json:"trimmed,omitempty"`
}

func (ThinkingBlock) blockType() BlockType { return BlockThinking }

// TextBlock is answer text.
type TextBlock struct {
	Text string `json:"text"`
	// Final marks the text of the turn that ended the run, which is the answer
	// rather than narration on the way to it. The same text is in Result.Text, so a
	// caller that renders both shows the answer twice unless it can tell them apart,
	// and only the run knows which message was terminal.
	Final bool `json:"final,omitempty"`
	// Index is where this block sat in the model call that produced it, counted over
	// every block of that call including the ones that never reach the wire. It pairs
	// with Iteration on the TextDeltaBlock values that carried this block's fragments,
	// so a receiver that asked for fragments knows which buffer this block replaces.
	//
	// Position in the reply set does not answer that: a turn's tool_use and provider
	// blocks are not sent, so counting the text blocks that arrive gives a different
	// number the moment one of those sits between two of them.
	//
	// Zero is both the first block and a worker that predates the field. That ambiguity
	// is what omitting it when unset costs, and it costs nothing in practice: a worker
	// too old to set it sends no fragments, so a receiver has no buffer to key on it.
	Index int `json:"index,omitempty"`
	// Trimmed reports that Text was cut to MaxBlockText and the rest of it is only in
	// the serving worker's run journal.
	//
	// A receiver holding fragments of this block needs it: the fragments were never
	// capped in aggregate, so for a long answer they are the more complete copy and
	// discarding them for this block would lose text.
	Trimmed bool `json:"trimmed,omitempty"`
}

func (TextBlock) blockType() BlockType { return BlockText }

// TextDeltaBlock is one fragment of a TextBlock as the model writes it.
//
// A fragment is an addition to the whole block and never a replacement for it. The
// TextBlock arrives when the model call ends, carrying the same Index, so a receiver
// that ignores every fragment reads the conversation it reads today. A caller asks for
// them with Request.Deltas and gets none otherwise.
//
// The whole block carries no Iteration. A receiver reconciling one against the
// fragments it buffered takes the call from the StatusBlock count it has already seen,
// since the whole blocks of a call arrive before the status block that ends it.
type TextDeltaBlock struct {
	// Index is the position of this fragment's block in the model call that produced
	// it, counted over every block of that call. Fragments of different blocks may
	// interleave, so a receiver keys its buffer on Index rather than assuming one block
	// finishes before the next begins.
	Index int `json:"index"`
	// Iteration is the model call this fragment came from, 1-based, counted as
	// StatusBlock counts it.
	//
	// It is here because Index restarts at 0 on every call while the status block that
	// separates two calls arrives after the first has ended, so a receiver keying on
	// Index alone would append the first block of one call to the last block of the one
	// before. Zero is a worker that does not report it.
	Iteration int `json:"iteration,omitempty"`
	// Text is this fragment's text, to be appended to what has already arrived for the
	// same Iteration and Index. It is empty on a Final fragment with nothing left to
	// send.
	Text string `json:"text,omitempty"`
	// Final marks the last fragment of this block, so a receiver closes the buffer when
	// the block ends rather than holding its tail until the next fragment or the end of
	// the run. The end of a block cannot be read off the fragments themselves.
	Final bool `json:"final,omitempty"`
}

func (TextDeltaBlock) blockType() BlockType { return BlockTextDelta }

// ThinkingDeltaBlock is one fragment of a ThinkingBlock's reasoning as the model
// writes it, on the terms TextDeltaBlock states. The signature does not stream: it
// arrives with the whole block.
type ThinkingDeltaBlock struct {
	// Index is the position of this fragment's block in the model call that produced
	// it, as TextDeltaBlock.Index is.
	Index int `json:"index"`
	// Iteration is the model call this fragment came from, 1-based, as
	// TextDeltaBlock.Iteration is.
	Iteration int `json:"iteration,omitempty"`
	// Text is this fragment's reasoning text, to be appended to what has already
	// arrived for the same Iteration and Index.
	Text string `json:"text,omitempty"`
	// Final marks the last fragment of this block, as TextDeltaBlock.Final does.
	Final bool `json:"final,omitempty"`
}

func (ThinkingDeltaBlock) blockType() BlockType { return BlockThinkingDelta }

// PromptBlock is a turn somebody asked for: the prompt a conversation opened with,
// or one added to it later.
//
// A live run never sends one, the caller having just written it. It exists so that a
// conversation replayed to a caller that was not there, or read back from a journal,
// carries both halves rather than an agent talking to itself.
type PromptBlock struct {
	Text string `json:"text"`
}

func (PromptBlock) blockType() BlockType { return BlockPrompt }

// WarningBlock is an advisory the run raised: something went wrong short of the run
// failing, or something an operator should know about what the run is allowed to do.
//
// It carries the warning rather than a sentence about it, so the wording belongs to
// whatever renders it and a receiver that does not know a kind still has the fields.
// Kind is the string form rather than the harness enumeration, which is an iota whose
// values are not a wire contract.
type WarningBlock struct {
	Kind   string   `json:"kind"`
	Name   string   `json:"name,omitempty"`
	Count  int      `json:"count,omitempty"`
	Params []string `json:"params,omitempty"`
	// Error is the failure the warning reports, as text. It is display material read
	// by a person, never matched on.
	Error string `json:"error,omitempty"`
}

func (WarningBlock) blockType() BlockType { return BlockWarning }

// ToolCallBlock is the agent invoking one of its own tools.
type ToolCallBlock struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input,omitempty"`
}

func (ToolCallBlock) blockType() BlockType { return BlockToolCall }

// ToolResultBlock is the result of a ToolCallBlock, identified by CallID and
// carrying the shared ToolResult outcome.
type ToolResultBlock struct {
	CallID string `json:"call_id"`
	ToolResult
}

func (ToolResultBlock) blockType() BlockType { return BlockToolResult }

// AgentCallBlock is the agent invoking another agent, distinct from a local
// tool call. Task is the request id of the spawned sub-task; its stream is
// correlated via Header.Parent.
type AgentCallBlock struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Task string `json:"task"`
}

func (AgentCallBlock) blockType() BlockType { return BlockAgentCall }

// StatusBlock reports progress. Iteration is 1-based; a zero value is omitted.
type StatusBlock struct {
	Iteration int    `json:"iteration,omitempty"`
	Phase     string `json:"phase,omitempty"`
	Usage     *Usage `json:"usage,omitempty"`
	// Count and Truncated describe a replay: how many blocks of the stored
	// conversation were sent, and whether older ones were left behind because the
	// caller asked for fewer than the conversation holds or the worker capped it. A
	// client renders history that begins mid-conversation as such rather than as a
	// conversation that began there.
	Count     int  `json:"count,omitempty"`
	Truncated bool `json:"truncated,omitempty"`
}

// The phases a status block carries. PhaseReplayStart and PhaseReplayEnd bracket the
// stored conversation a resume was asked to replay, so a client knows which blocks
// already happened.
const (
	PhaseReplayStart = "replay_start"
	PhaseReplayEnd   = "replay_end"
)

func (StatusBlock) blockType() BlockType { return BlockStatus }

// UnknownBlock is a block of a kind this build does not name. Type is what the event's
// id called it and Raw is the block's original JSON object, so a caller can render a
// placeholder for it, log it, or forward it verbatim.
//
// Raw is peer supplied and is checked against nothing but the message size cap. It
// is the peer's own content, so an agent that forwards it never republishes it as
// its own, the same rule ToolDescriptor.Behavior carries.
//
// There is no constructor, because a producer inventing a kind it cannot describe
// is a mistake. A forwarding agent relaying a block it did not understand builds
// the value directly, and the event it sends carries the id that named it.
type UnknownBlock struct {
	Type BlockType
	Raw  json.RawMessage
}

func (u UnknownBlock) blockType() BlockType { return u.Type }

// MarshalJSON returns the block as it arrived, which is the peer's own value rather
// than one re-made here.
func (u UnknownBlock) MarshalJSON() ([]byte, error) {
	if len(u.Raw) == 0 {
		return nil, fmt.Errorf("%w: unknown block %q carries no content", ErrInvalidMessage, u.Type)
	}

	return u.Raw, nil
}

// Block wraps a single BlockContent for transport. It marshals to a flat JSON object
// of the variant's own fields, and decodes back to the matching concrete type from the
// kind its event's protocol id named. It is not a JSON value on its own: a block lifted
// out of its message says nothing about what it is.
type Block struct {
	content BlockContent
}

// NewBlock wraps any BlockContent.
func NewBlock(content BlockContent) Block { return Block{content: content} }

// NewThinkingBlock builds a thinking Block.
func NewThinkingBlock(text string) Block { return NewBlock(ThinkingBlock{Text: text}) }

// NewTextBlock builds a text Block.
func NewTextBlock(text string) Block { return NewBlock(TextBlock{Text: text}) }

// NewFinalTextBlock builds the text Block of the turn that ended the run.
func NewFinalTextBlock(text string) Block { return NewBlock(TextBlock{Text: text, Final: true}) }

// NewToolCallBlock builds a tool_call Block.
func NewToolCallBlock(id, name string, input json.RawMessage) Block {
	return NewBlock(ToolCallBlock{ID: id, Name: name, Input: input})
}

// NewToolResultBlock builds a tool_result Block.
func NewToolResultBlock(callID, output string, isError bool) Block {
	return NewBlock(ToolResultBlock{CallID: callID, ToolResult: ToolResult{Output: output, IsError: isError}})
}

// AsAny returns the concrete BlockContent for use in a type switch, or nil if
// the Block is empty.
func (b Block) AsAny() any {
	if b.content == nil {
		return nil
	}

	return b.content
}

// Content returns the wrapped BlockContent, or nil if the Block is empty.
func (b Block) Content() BlockContent { return b.content }

// Type returns the block type, or the empty string if the Block is empty.
func (b Block) Type() BlockType {
	if b.content == nil {
		return ""
	}

	return b.content.blockType()
}

// EventProtocolFor is the protocol id a block of this type is carried under. It is
// how a sender stamps an event, and Event does it rather than leaving it to a caller.
func EventProtocolFor(t BlockType) string {
	return EventProtocol + "." + string(t)
}

// blockTypeOf is the block an event id carries, and false for an id outside the event
// family. A type this build does not name still reports true: the id says a block is
// what arrived, which is what decides how to read the message, and UnknownBlock is
// what carries one nobody here can render.
//
// A suffix carrying a dot of its own is refused. Nothing mints one, so it is a peer
// naming something else entirely, and reading it as the type before the dot would be
// this build deciding what somebody else's id meant.
func blockTypeOf(protocol string) (BlockType, bool) {
	suffix, found := strings.CutPrefix(protocol, EventProtocol+".")
	if !found || suffix == "" || strings.Contains(suffix, ".") {
		return "", false
	}

	return BlockType(suffix), true
}

// MarshalJSON renders the block as its content fields alone. What kind of block it is
// travels as the event's protocol id, so a block lifted out of its message says
// nothing about itself and only decodes as part of one.
func (b Block) MarshalJSON() ([]byte, error) {
	if b.content == nil {
		return nil, fmt.Errorf("%w: block has no content", ErrInvalidMessage)
	}

	return json.Marshal(b.content)
}

// unmarshalAs decodes the block as the type its event's id named, or into an
// UnknownBlock when this build does not name it, so an event from a newer peer still
// delivers its header, its sequence number and everything else it held.
func (b *Block) unmarshalAs(t BlockType, data []byte) error {
	var (
		content BlockContent
		err     error
	)

	switch t {
	case BlockThinking:
		var v ThinkingBlock
		err = json.Unmarshal(data, &v)
		content = v
	case BlockText:
		var v TextBlock
		err = json.Unmarshal(data, &v)
		content = v
	case BlockToolCall:
		var v ToolCallBlock
		err = json.Unmarshal(data, &v)
		content = v
	case BlockToolResult:
		var v ToolResultBlock
		err = json.Unmarshal(data, &v)
		content = v
	case BlockAgentCall:
		var v AgentCallBlock
		err = json.Unmarshal(data, &v)
		content = v
	case BlockStatus:
		var v StatusBlock
		err = json.Unmarshal(data, &v)
		content = v
	case BlockWarning:
		var v WarningBlock
		err = json.Unmarshal(data, &v)
		content = v
	case BlockPrompt:
		var v PromptBlock
		err = json.Unmarshal(data, &v)
		content = v
	case BlockTextDelta:
		var v TextDeltaBlock
		err = json.Unmarshal(data, &v)
		content = v
	case BlockThinkingDelta:
		var v ThinkingDeltaBlock
		err = json.Unmarshal(data, &v)
		content = v
	default:
		content = UnknownBlock{Type: t, Raw: append(json.RawMessage(nil), data...)}
	}

	if err != nil {
		return err
	}

	b.content = content

	return nil
}
