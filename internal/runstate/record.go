//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// Package runstate persists an agent run as an append-only sequence of events so
// it can be suspended and resumed later, possibly in a fresh process.
//
// A run is journaled as records (see Record): a single Meta record, then an
// Assistant record after every LLM call and a ToolResult record as each tool
// completes, a User record for each interactive follow-up in a chat run, and
// finally a Terminal record. Folding the records back into a RunState (see Fold)
// reconstructs the conversation, the counters and the resume position. The record
// stream maps one-to-one onto the durability journal today and onto the A2A event
// stream later, so the same model backs local resume and remote streaming.
package runstate

import (
	"time"

	"github.com/choria-io/fisk-ai/internal/llm"
)

// Version is the on-disk record format version, stamped into the Meta record.
// Fold accepts only this exact version: the record format is provider-neutral
// (llm.Message), and the earlier Anthropic-wire format does not round-trip
// through the neutral records, so any other version is rejected rather than
// silently mis-folded.
const Version = 3

// Protocol is the schema id of a Record, carried in the record body so a single
// stored record is self describing: given just one record you can find its
// origin schema, independent of the subject it arrived on or the store it was
// read from. The ids share the a2a product namespace under a ".session" segment
// so they never collide with the a2a wire protocols.
type Protocol string

// protocolNamespace is the a2a product namespace (a2a.ProtocolNamespace) under a
// ".session" segment. It is spelled out here rather than imported so the storage
// layer does not depend on the a2a package; it must track a2a.ProtocolNamespace
// if that changes.
const protocolNamespace = "io.choria.fisk-ai.v1.session"

const (
	// MetaProtocol frames the run: version, id, fingerprint and the initial
	// prompt. It is always the first record.
	MetaProtocol Protocol = protocolNamespace + ".meta"
	// AssistantProtocol records one assistant turn (the result of one LLM call).
	AssistantProtocol Protocol = protocolNamespace + ".assistant"
	// UserProtocol records a free-standing interactive user turn: a chat follow-up
	// the operator typed at an input boundary, which has no other origin in the loop
	// (the initial prompt lives in Meta, tool results in ToolResult). Present only in
	// an interactive (--chat) run.
	UserProtocol Protocol = protocolNamespace + ".user"
	// ToolResultProtocol records the result of a single tool invocation, written
	// as the tool completes so a crash loses at most one tool.
	ToolResultProtocol Protocol = protocolNamespace + ".tool_result"
	// DeferredProtocol records that a tool call will be answered later. It is what
	// separates a call nobody has finished from one a crash interrupted: the second
	// is re-run on resume and the first must never be, because the tool already did
	// whatever it started.
	DeferredProtocol Protocol = protocolNamespace + ".deferred"
	// DecisionProtocol records a standing operator approval for a tool.
	DecisionProtocol Protocol = protocolNamespace + ".decision"
	// CallApprovalProtocol records an operator approval for one gated call, which
	// authorizes the next dispatch of that call and nothing else.
	CallApprovalProtocol Protocol = protocolNamespace + ".call_approval"
	// TerminalProtocol records why the run ended (or that it was suspended).
	TerminalProtocol Protocol = protocolNamespace + ".terminal"
	// ClaimProtocol records that a worker took the run over on resume. It is
	// written before anything the resumed run does, so that appending it moves the
	// journal's tail ahead of whatever a previous worker still holds.
	ClaimProtocol Protocol = protocolNamespace + ".claim"
)

// Record is one journal entry. Exactly one of the payload pointers is set,
// selected by Protocol. Seq is the monotonic event id, the ordering authority
// (mirroring a2a.Header.Sequence); it starts at 1 on the Meta record.
type Record struct {
	Seq      uint64   `json:"seq"`
	Protocol Protocol `json:"protocol"`

	// Optional marks a record a reader may skip when it does not recognize the
	// protocol. It may be set only on a record whose absence is fail-safe, meaning a
	// reader that skips it behaves more conservatively rather than differently. A
	// record whose absence changes a decision in the restrictive direction (a
	// revocation, an expiry, a narrowed scope) must never set it and requires a
	// Version bump instead.
	//
	// DeferredProtocol is the record in this package that must not set it: a reader
	// that skipped one would dispatch a call whose answer somebody is already
	// working on.
	Optional bool `json:"optional,omitempty"`

	Meta         *MetaRecord         `json:"meta,omitempty"`
	Assistant    *AssistantRecord    `json:"assistant,omitempty"`
	User         *UserRecord         `json:"user,omitempty"`
	ToolResult   *ToolResultRecord   `json:"tool_result,omitempty"`
	Deferred     *DeferredRecord     `json:"deferred,omitempty"`
	Decision     *DecisionRecord     `json:"decision,omitempty"`
	CallApproval *CallApprovalRecord `json:"call_approval,omitempty"`
	Terminal     *TerminalRecord     `json:"terminal,omitempty"`
	Claim        *ClaimRecord        `json:"claim,omitempty"`
}

// ClaimRecord records a worker taking over a run on resume. Writing it is what
// makes acquisition a write: the append moves the journal's tail, so a worker that
// still believes it holds this run is refused at its own next append.
//
// The payload is diagnostic and nothing in the harness reads it. An empty record
// would fence identically; this exists so a person reading a journal can tell which
// worker took the run and when. It is deliberately not folded into RunState:
// nothing releases a claim on a crash, a suspend or a completion, so a claim is not
// evidence that a run is held now.
type ClaimRecord struct {
	// By names the worker that took the run, in whatever terms its operator chose. It
	// is not verified and is never used to make a decision.
	By string `json:"by"`
	// Claimed is when the worker took it.
	Claimed time.Time `json:"claimed"`
}

// MetaRecord frames a run. It carries no credentials of the process that ran it:
// the fingerprint holds only a hash of the system prompt, never the prompt or an
// API key. ConversationToken is the one credential it does carry, and it is the
// caller's rather than this process's, recorded so that it can be recovered.
type MetaRecord struct {
	Version     int         `json:"version"`
	RunID       string      `json:"run_id"`
	Created     time.Time   `json:"created"`
	Fingerprint Fingerprint `json:"fingerprint"`
	// Prompt is the initial user prompt, from which Messages[0] is rebuilt.
	Prompt string `json:"prompt"`
	// Interactive marks a run started in chat (--chat) mode, so a resume knows to
	// reopen the input bar rather than making a fresh LLM call at a completed
	// boundary. Absent (false) on a one-shot or batch checkpoint run.
	Interactive bool `json:"interactive,omitempty"`
	// ConversationToken is the handle the caller holds for the conversation this
	// journal records, empty for a run nobody was handed one for. A channel that
	// derives the run id from a token records it here, so that a caller that lost
	// one can be given it back and an operator can say which conversation is which.
	//
	// It is a credential: holding it authorizes adding a turn. Reading it needs the
	// store access that already grants reading and writing this journal directly, so
	// it discloses nothing that access does not, and it is still never logged.
	//
	// It is worth nothing on its own. A channel hashes it together with the serving
	// identity to name the journal, so a token recovered here reaches this
	// conversation only through the identity that minted it.
	ConversationToken string `json:"conversation_token,omitempty"`
	// Caller is what the channel that produced this run knew about who asked for it,
	// empty where it knew nothing or where no channel was involved. It exists so that
	// an operator can tell two conversations apart when their first prompts do not.
	//
	// It is the caller's own claim. Nothing verifies it and nothing decides on it, so
	// it is a label for a person reading a journal and never evidence of who owns one.
	Caller string `json:"caller,omitempty"`
}

// AssistantRecord is one assistant turn in the neutral model, so thinking blocks
// with signatures and provider server-side blocks are preserved verbatim for
// resume regardless of which provider produced them.
type AssistantRecord struct {
	Iteration  int64       `json:"iteration"`
	Message    llm.Message `json:"message"`
	StopReason string      `json:"stop_reason,omitempty"`
	InTokens   int64       `json:"in_tokens"`
	OutTokens  int64       `json:"out_tokens"`
	// CacheReadTokens and CacheCreateTokens are the response's prompt-cache input
	// tiers, split out from InTokens (the uncached remainder). Additive and omitempty
	// so no schema version bump is needed: a pre-caching journal omits them and folds
	// as zero, which is correct (caching was off, so there were none).
	CacheReadTokens   int64 `json:"cache_read_tokens,omitempty"`
	CacheCreateTokens int64 `json:"cache_create_tokens,omitempty"`
	// ThinkingTokens is the part of OutTokens the model spent reasoning, not a
	// separate cost on top of it. Additive and omitempty for the same reason as the
	// cache tiers: a journal written before it existed omits it and folds as zero,
	// which is what a run whose reasoning nobody counted should report.
	ThinkingTokens int64 `json:"thinking_tokens,omitempty"`
}

// UserRecord is a free-standing user turn: one typed at a chat's input bar, or one a
// resumed run was handed to continue a conversation with. Message
// carries only the newly typed block(s), never a merged view of a preceding user
// turn: when a follow-up folds into a dangling user message at runtime (the prior
// turn errored before replying), the journal still records the follow-up on its own
// and Fold reconstructs the fold by merging consecutive user messages. Recording the
// merged message instead would double the tool_result blocks Fold already appended.
type UserRecord struct {
	Message llm.Message `json:"message"`
}

// ToolResultRecord is the result of a single tool call, keyed by the tool_use id
// it answers. Remote marks a call dispatched to another agent over A2A.
type ToolResultRecord struct {
	ToolUseID string              `json:"tool_use_id"`
	Result    llm.ToolResultBlock `json:"result"`
	Remote    bool                `json:"remote,omitempty"`
}

// DeferredRecord marks a tool call whose answer arrives later, keyed by the
// tool_use id it will eventually answer. A ToolResultRecord carrying the same id,
// appended whenever the answer exists, is what completes it; the call itself is
// never dispatched again.
//
// Note and Handle are the tool's own words and are display text: sanitize them
// before rendering, as with anything read back from a journal.
type DeferredRecord struct {
	ToolUseID string `json:"tool_use_id"`
	ToolName  string `json:"tool_name"`
	Note      string `json:"note,omitempty"`
	Handle    string `json:"handle,omitempty"`
}

// DecisionRecord is a standing approval the operator granted for a named tool,
// covering the rest of the conversation rather than one call. It holds a tool name
// and nothing else: the command line the operator saw is model-supplied and already
// in the transcript, and the approval does not depend on it.
//
// The record carries no denial. The gate has no standing refusal, so a declined
// command is asked about again next time, and a run that ended before the operator
// answered records nothing.
type DecisionRecord struct {
	// Tool is the effective tool name the gate keys on (stream_rm), which is not the
	// command path the approval prompt displayed (stream rm).
	Tool string `json:"tool"`
}

// CallApprovalRecord is an operator approval for one gated call, keyed by the tool_use
// id it authorizes. An "allow once" answer given while the run was suspended is written
// here, so the resume dispatches that call without putting the question again.
//
// It authorizes one dispatch of one call. A terminal record after it spends it, so a run
// that ends before dispatching the call leaves nothing behind and the next resume asks
// again. That costs a question and never a wrong execution.
//
// The record carries no denial, for the reason DecisionRecord states.
type CallApprovalRecord struct {
	// ToolUseID is the call the operator approved, and the whole of what the gate
	// decides on.
	ToolUseID string `json:"tool_use_id"`
	// ToolName is the tool that call names, for a person reading the journal.
	ToolName string `json:"tool_name"`
}

// TerminalReason explains why a run stopped.
type TerminalReason string

const (
	// ReasonCompleted means the agent returned a final answer.
	ReasonCompleted TerminalReason = "completed"
	// ReasonSuspended means the run was checkpointed and exited to be resumed.
	ReasonSuspended TerminalReason = "suspended"
	// ReasonError means the run ended on an error.
	ReasonError TerminalReason = "error"
	// ReasonBudget means the token budget was exhausted.
	ReasonBudget TerminalReason = "budget"
	// ReasonMaxIterations means the iteration cap was reached.
	ReasonMaxIterations TerminalReason = "max_iterations"
)

// TerminalRecord records the end of a run.
type TerminalRecord struct {
	Reason  TerminalReason `json:"reason"`
	Message string         `json:"message,omitempty"`
}
