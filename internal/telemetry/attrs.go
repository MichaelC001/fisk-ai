//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
)

// The fisk.* attribute keys. Anything fisk-specific with no home in the
// OpenTelemetry semantic conventions goes under this prefix rather than squatting on
// gen_ai.*. Every key that the conventions do define is imported from
// semconv/v1.41.0 at its use site and never transcribed here, so a version bump
// breaks the build rather than drifting silently.
const (
	// AttrToolsApplication counts the tools introspected from the wrapped
	// application.
	AttrToolsApplication = attribute.Key("fisk.tools.application")
	// AttrToolsBuiltin counts the in-process built-in tools the run exposed.
	AttrToolsBuiltin = attribute.Key("fisk.tools.builtin")
	// AttrToolsRemote counts the tools imported from remote a2a hosts.
	AttrToolsRemote = attribute.Key("fisk.tools.remote")
	// AttrToolsCustom counts the tools a Go caller injected through Options.
	AttrToolsCustom = attribute.Key("fisk.tools.custom")
	// AttrMemoryBackend and AttrMemoryLocation describe the memory store a run bound,
	// read off the store rather than the config: an injected store makes those two
	// disagree, and the one that ran is the true answer.
	//
	// They sit on startup and on memory_index, and on each memory tool call. The
	// repetition is deliberate: without it, "which backend was this slow memory_write
	// on" is a join to the startup span in the same trace, which no backend expresses
	// as one query. They stay on startup as well, because a run can bind a store, have
	// the index turned off and never call a memory tool, and nothing in its trace would
	// then report that memory existed at all.
	//
	// The location is whatever container the backend names, a KV bucket today. It is
	// absent for a backend with nothing safe to name, which is why the file backend
	// reports none: its container is an absolute local directory. Span attributes only,
	// never metric labels, since adding a backend to the tool duration histogram would
	// leave it empty on every tool that is not a memory tool.
	AttrMemoryBackend  = attribute.Key("fisk.memory.backend")
	AttrMemoryLocation = attribute.Key("fisk.memory.location")
	// AttrMemoryEntries is how many memories the start-of-run index load returned.
	//
	// It is the divisor for the span's own duration. Listing reads every value to
	// recover its description, so on a network backend it is a round trip per entry,
	// and without the count a slow load cannot be told from a large store: duration
	// alone reports the same number for a struggling backend and a healthy one holding
	// a thousand memories, and those have different fixes. A store also grows quietly
	// over a fleet's life, which nothing else here would surface until it was already a
	// problem.
	//
	// Absent rather than zero when the load failed, since zero says the store is empty,
	// which is a different answer again. A span attribute only: the count is unbounded
	// by nature and belongs nowhere near a metric label.
	AttrMemoryEntries = attribute.Key("fisk.memory.entries")

	// The content-capture markers, present only on a run that opted into exporting
	// the conversation.
	//
	// They are fisk.content.* rather than fisk.llm.content.*, which is the only
	// prefix here that names something other than a subject, because truncation
	// applies to a tool call's arguments as much as to a model call's messages and
	// "show me every span where content was cut" is a question across span kinds. The
	// precedent is fisk.memory.*, which already spans startup, memory_index and
	// execute_tool.
	//
	// AttrContentFromIndex is where in the conversation this call's captured input
	// begins. It is an integer rather than a delta/full enum because a span already
	// carries fisk.llm.messages, so the two would otherwise disagree in plain sight:
	// seventeen messages sent, two exported. It reconciles them, it says what the enum
	// said (zero means the whole conversation, or the first call of the process), and
	// because the indexes chain across a trace, a gap between consecutive model calls
	// is a span that never arrived.
	AttrContentFromIndex = attribute.Key("fisk.content.from_index")
	// AttrContentTruncated lists the content attributes on this span that were cut, by
	// name, rather than reporting a single boolean for all of them. A span carries two,
	// they are cut independently, and which one it was decides where to look next. The
	// values are a closed set of semantic convention keys.
	AttrContentTruncated = attribute.Key("fisk.content.truncated")
	// AttrContentDroppedMessages counts whole messages removed to fit the budget.
	//
	// It exists because that is the one form of truncation with no in-band sign: a cut
	// string still looks cut, while a document three messages shorter is valid,
	// complete-looking and silently wrong.
	AttrContentDroppedMessages = attribute.Key("fisk.content.dropped_messages")

	// AttrToolsDeferred reports whether the tool set was deferred behind server-side
	// tool search rather than sent to the model directly.
	AttrToolsDeferred = attribute.Key("fisk.tools.deferred")
	// AttrRemoteHosts counts the configured remote tool hosts, which is the number
	// of agents startup may have had to reach over NATS.
	AttrRemoteHosts = attribute.Key("fisk.remote_hosts")

	// AttrRunTerminalReason is how the run ended: completed, max_iterations, error,
	// budget, suspended, or setup_failed for one that never reached the loop.
	AttrRunTerminalReason = attribute.Key("fisk.run.terminal_reason")
	// AttrRunResumed reports that the run continued a checkpointed session.
	AttrRunResumed = attribute.Key("fisk.run.resumed")
	// AttrRunCrashed reports that the run ended on a recovered panic rather than on
	// any outcome.
	AttrRunCrashed = attribute.Key("fisk.run.crashed")
	// AttrRunInteractive distinguishes a chat run from a one-shot one.
	AttrRunInteractive = attribute.Key("fisk.run.interactive")
	// AttrRunTurns counts the interactive turns, absent on a one-shot run.
	AttrRunTurns = attribute.Key("fisk.run.turns")
	// AttrRunToolCalls and AttrRunRemoteToolCalls count this process's tool calls;
	// the remote ones are a subset of the total.
	AttrRunToolCalls       = attribute.Key("fisk.run.tool_calls")
	AttrRunRemoteToolCalls = attribute.Key("fisk.run.remote_tool_calls")

	// AttrTurnIndex is the one-based position of a turn within an interactive run.
	AttrTurnIndex = attribute.Key("fisk.turn.index")

	// AttrToolRequestedName is the tool name the model asked for, recorded only when it
	// resolved to nothing. It is a span attribute and never a metric label or a span
	// name: it comes straight from the model, unvalidated, so a hallucination or a
	// prompt injection engineered for it would otherwise mint unbounded metric series
	// that cost the operator real money and cannot be un-sent.
	AttrToolRequestedName = attribute.Key("fisk.tool.requested_name")
	// AttrToolKind is the provider that supplied the tool, which is the true accounting
	// axis: application command, built-in, remote, or custom.
	AttrToolKind = attribute.Key("fisk.tool.kind")
	// AttrToolOutcome is the one axis to group tool calls by. It separates a call that
	// ran and failed from one that never ran, and why it never ran.
	AttrToolOutcome = attribute.Key("fisk.tool.outcome")
	// AttrToolArgKeys is the argument KEY NAMES, never their values. It is schema
	// rather than data: it turns "it called stream_edit" into "with stream, max_age,
	// retention" without opting into content capture.
	AttrToolArgKeys = attribute.Key("fisk.tool.arg_keys")
	// AttrToolConfirmGated reports that the call required operator approval.
	AttrToolConfirmGated = attribute.Key("fisk.tool.confirm_gated")
	// AttrToolConfirmWaitMS is how long the call waited on the operator. It exists so a
	// four-minute span has a visible reason; the wait is human time and giving it a
	// child span would let it dominate every duration chart.
	AttrToolConfirmWaitMS = attribute.Key("fisk.tool.confirm_wait_ms")
	// AttrToolRewritten reports that a hook redirected the call or its arguments.
	AttrToolRewritten = attribute.Key("fisk.tool.rewritten")
	// AttrToolRemote reports that the call was dispatched to a remote agent, and
	// AttrToolRemoteAgent names it.
	AttrToolRemote      = attribute.Key("fisk.tool.remote")
	AttrToolRemoteAgent = attribute.Key("fisk.tool.remote_agent")
	// AttrToolExitCode is the exit status of the command a tool ran, and is ABSENT for
	// a tool that ran none: a built-in, a Go caller's own tool, a tool invoked on a
	// remote agent. Reporting zero for those would publish "the command succeeded" for a
	// command that never existed, which is the same fabrication the remote tool path
	// already refuses to make when it declines to wrap an exec-less reply in a command
	// envelope. fisk.tool.kind on the same span says which case applies.
	//
	// A non-zero exit is not an error and does not set error.type: it round-trips as an
	// ordinary result, so flagging it would count every routine "grep matched nothing"
	// as a failure. Span attribute only, never a metric label.
	AttrToolExitCode = attribute.Key("fisk.tool.exit_code")
	// AttrToolResumed marks a tool from a batch a resume completed. Those run before
	// the iteration loop, so they produce tool spans with no preceding model call in
	// the same trace; without the marker that shape reads as tools running unprompted.
	AttrToolResumed = attribute.Key("fisk.tool.resumed")

	// AttrLLMIteration is the loop index of a model call. It is session-scoped rather
	// than trace-scoped: a resumed run continues the numbering, so a resumed trace's
	// first call may be iteration 17 and that is correct, not a sign of dropped spans.
	AttrLLMIteration = attribute.Key("fisk.llm.iteration")
	// AttrLLMMessages and AttrLLMTools are the conversation and tool-set sizes sent on
	// one call, which is how a context window filling up becomes visible without
	// capturing any content.
	AttrLLMMessages = attribute.Key("fisk.llm.messages")
	AttrLLMTools    = attribute.Key("fisk.llm.tools")

	// AttrLLMUncachedInputTokens is the input tokens billed at the uncached rate.
	// gen_ai.usage.input_tokens includes the cached tiers by spec, so this is the term
	// a cost calculation needs and the number the run summary line already prints,
	// which is what lets a trace be reconciled against it.
	AttrLLMUncachedInputTokens = attribute.Key("fisk.llm.uncached_input_tokens")
	// AttrLLMThinking, AttrLLMPromptCache and AttrLLMToolSearch are the run-constant
	// model feature switches, on the root rather than repeated on every chat span.
	AttrLLMThinking    = attribute.Key("fisk.llm.thinking")
	AttrLLMPromptCache = attribute.Key("fisk.llm.prompt_cache")
	AttrLLMToolSearch  = attribute.Key("fisk.llm.tool_search")

	// AttrLLMHTTPAttempt and AttrLLMHTTPDurationMS describe one HTTP attempt within a
	// model call, and appear on the per-attempt events only. One chat span can be
	// several requests, so these cannot be span attributes: last-attempt-wins would
	// report a 200 for a call that spent most of its time being rate limited.
	AttrLLMHTTPAttempt    = attribute.Key("fisk.llm.http_attempt")
	AttrLLMHTTPDurationMS = attribute.Key("fisk.llm.http_duration_ms")

	// AttrSessionEndID is the session id the run ended on, set only when a context
	// reset rotated it away from the one it started with. It is the id an operator
	// resumes next, and having both means searching by either id finds the trace.
	AttrSessionEndID = attribute.Key("fisk.session.end_id")
	// The session-cumulative counters, including any restored prefix, emitted only for
	// a resumed run. They exist because gen_ai.usage.* on the root deliberately carries
	// this process's tokens alone: summing that across a session's traces has to give
	// the session total exactly once, so the cumulative view needs its own keys.
	AttrSessionUsageInputTokens       = attribute.Key("fisk.session.usage.input_tokens")
	AttrSessionUsageOutputTokens      = attribute.Key("fisk.session.usage.output_tokens")
	AttrSessionUsageCacheReadTokens   = attribute.Key("fisk.session.usage.cache_read_tokens")
	AttrSessionUsageCacheCreateTokens = attribute.Key("fisk.session.usage.cache_creation_tokens")
	AttrSessionLLMCalls               = attribute.Key("fisk.session.llm_calls")

	// AttrKnowledgeTierConfigured is the retrieval tier the config asked for and
	// AttrKnowledgeTierEffective is the one that ran, so "did this query use vectors" is
	// one attribute rather than a compound condition. The effective tier is ABSENT when
	// neither retriever ran, which a search reports for an index that is missing, empty,
	// or queried with terms that all reduced away: naming a tier there would report a
	// retrieval that never happened.
	//
	// They are two leaves under one object rather than a scalar with a child, because a
	// key that is both a keyword and an object is a mapping conflict in the document
	// stores several backends are built on.
	//
	// Neither appears on an enumerate span. Enumeration never fuses vectors whatever the
	// config says, so carrying the pair there would report a degradation on every call
	// and make the one query the pair exists for useless.
	AttrKnowledgeTierConfigured = attribute.Key("fisk.knowledge.tier.configured")
	AttrKnowledgeTierEffective  = attribute.Key("fisk.knowledge.tier.effective")
	// AttrKnowledgeTopK is the effective result ceiling for one search, after the
	// requested value has been defaulted and clamped, so it is bounded whatever the model
	// asked for. It is deliberately NOT gen_ai.retrieval.top_k: no such key exists in the
	// conventions, and the one that does exist, gen_ai.request.top_k, is the model's
	// sampling parameter and means something else entirely.
	AttrKnowledgeTopK = attribute.Key("fisk.knowledge.top_k")
	// AttrKnowledgeSearchStatus and AttrKnowledgeEnumerateStatus are separate keys on
	// purpose. A merged one would imply a search can report query_empty, which it cannot:
	// it folds "the index holds nothing" and "the query reduced to nothing" together,
	// and telling those apart is the whole reason enumeration reports its own set.
	AttrKnowledgeSearchStatus    = attribute.Key("fisk.knowledge.search.status")
	AttrKnowledgeEnumerateStatus = attribute.Key("fisk.knowledge.enumerate.status")
	// AttrKnowledgeSections counts what a search returned and AttrKnowledgeDocuments
	// counts what an enumerate returned. They are different keys because they are
	// different units: a search ranks chunks of documents and several results routinely
	// come from one file, so one key over both would be summed into a meaningless number.
	AttrKnowledgeSections  = attribute.Key("fisk.knowledge.sections")
	AttrKnowledgeDocuments = attribute.Key("fisk.knowledge.documents")
	// AttrKnowledgeIndexedChunks and AttrKnowledgeIndexedDocuments are the corpus size
	// each operation counted on its way past, which is what makes a zero result readable:
	// three hits out of twelve chunks is a different problem from three out of forty
	// thousand. Both are ABSENT rather than zero when no index exists, since zero there
	// means "unknown" and would read as an empty corpus.
	AttrKnowledgeIndexedChunks    = attribute.Key("fisk.knowledge.indexed_chunks")
	AttrKnowledgeIndexedDocuments = attribute.Key("fisk.knowledge.indexed_documents")
	// AttrKnowledgeMatched is the complete matched set before any budget, and
	// AttrKnowledgeTruncated reports that the returned set is smaller.
	AttrKnowledgeMatched   = attribute.Key("fisk.knowledge.matched")
	AttrKnowledgeTruncated = attribute.Key("fisk.knowledge.truncated")
	// AttrKnowledgeLimit and AttrKnowledgeMinBodyMatches are the enumerate options that
	// shape the counts above. Without them the matched count cannot be interpreted from
	// the span alone, since the aboutness filter is applied before it is taken.
	AttrKnowledgeLimit          = attribute.Key("fisk.knowledge.limit")
	AttrKnowledgeMinBodyMatches = attribute.Key("fisk.knowledge.min_body_matches")
	// AttrKnowledgeDegraded reports that a hybrid search fell back to lexical, and
	// AttrKnowledgeDegradedReason names the class of failure that caused it. The reason
	// is a closed vocabulary and never the underlying error text, which carries the
	// embeddings endpoint and fragments of the server's response body.
	AttrKnowledgeDegraded       = attribute.Key("fisk.knowledge.degraded")
	AttrKnowledgeDegradedReason = attribute.Key("fisk.knowledge.degraded_reason")

	// AttrEmbeddingsInputs is how many texts one embeddings request carried.
	AttrEmbeddingsInputs = attribute.Key("fisk.embeddings.inputs")
	// AttrEmbeddingsPurpose separates embedding the query from probing the model's
	// dimension. The probe is lazy and cached per process, so the first search of a run
	// makes two requests that are otherwise identical in shape; worse, a server that is
	// down never lets the probe cache, so every later search makes a probe request and no
	// query request at all. Without this key that trace reads as an embeddings call that
	// embedded nothing.
	AttrEmbeddingsPurpose = attribute.Key("fisk.embeddings.purpose")
)

// The retrieval tiers. Hybrid means the lexical and vector retrievers were fused;
// lexical means the vector tier was off or could not run.
const (
	TierLexical = "lexical"
	TierHybrid  = "hybrid"
)

// The embeddings request purposes. See AttrEmbeddingsPurpose for why the dimension
// probe has to be distinguishable from a real query embedding.
const (
	EmbeddingsPurposeQuery          = "query"
	EmbeddingsPurposeDimensionProbe = "dimension_probe"
	// EmbeddingsPurposeDocument is corpus ingest. No run emits it today, since indexing
	// happens in a command that starts no telemetry, but it is the value that would be
	// correct if one ever did and it keeps the purpose honest at every call site.
	EmbeddingsPurposeDocument = "document"
)

// DegradeReason is why a hybrid search fell back to the lexical tier alone.
//
// It is a struct wrapping a string rather than a defined string type, and that is the
// point: a defined string type can be converted from any string by any caller, so
// telemetry.DegradeReason(err.Error()) would compile and put an embeddings endpoint and
// a server's response body on an attribute headed off-box. This is the first value in
// this package derived from an error rather than picked from a literal branch, so it is
// the first place that conversion becomes tempting. There is no exported way to build
// one; a caller picks from the values below.
type DegradeReason struct{ s string }

// String renders the reason for the span attribute.
func (d DegradeReason) String() string { return d.s }

// Set reports whether a reason was recorded at all.
func (d DegradeReason) Set() bool { return d.s != "" }

var (
	// DegradeTimeout is an embeddings request that ran out of time.
	DegradeTimeout = DegradeReason{"timeout"}
	// DegradeCanceled is an embeddings request abandoned because its context was
	// canceled.
	DegradeCanceled = DegradeReason{"canceled"}
	// DegradeEmbeddings is the embeddings server failing to answer usefully.
	DegradeEmbeddings = DegradeReason{"embeddings"}
	// DegradeIndexMeta is the index's own pinned metadata failing to read, which is the
	// store failing rather than the embeddings server. It is separated because the two
	// are indistinguishable from the outside and the fixes are unrelated: without the
	// split every unreadable index would be reported as an unreachable server. It is also
	// the one degrade path that opens no child span, so this attribute is the only place
	// it appears in a trace.
	DegradeIndexMeta = DegradeReason{"index_meta"}
	// DegradeOther is the catch-all, for a failure that fits none of the above.
	DegradeOther = DegradeReason{"other"}
)

// The closed set of fisk.tool.outcome values. They are constants rather than strings
// at the call sites because this is the one axis the documentation tells an operator
// to group by, so a typo in one branch would silently split a series.
const (
	// ToolOutcomeExecuted is a call that ran and returned a non-error result.
	ToolOutcomeExecuted = "executed"
	// ToolOutcomeError is a call that ran and returned an error result. A non-zero
	// command exit is NOT this: that round-trips as an ordinary result envelope, so an
	// ordinary failing command is executed, not error.
	ToolOutcomeError = "error"
	// ToolOutcomeUnknownTool is a call naming a tool that does not exist.
	ToolOutcomeUnknownTool = "unknown_tool"
	// ToolOutcomePolicyDenied is a call a hook refused.
	ToolOutcomePolicyDenied = "policy_denied"
	// ToolOutcomeMissingArguments is a call rejected before running because the model
	// omitted a required parameter.
	ToolOutcomeMissingArguments = "missing_arguments"
	// ToolOutcomeConfirmDenied is a call the operator refused at the gate.
	ToolOutcomeConfirmDenied = "confirm_denied"
)

// The gen_ai.tool.type values this repository uses. The conventions define the
// attribute but ship no constants for its values, so they are named here.
//
// Only two of the three apply. A knowledge tool reads a local index, which is a
// datastore. Everything else is a function, including a tool served by a remote agent:
// that is client-side from the model's point of view, where extension means
// provider-side, which describes only a model backend's own server-side tooling.
const (
	ToolTypeFunction  = "function"
	ToolTypeDatastore = "datastore"
)

// The tool call events. A confirm prompt is an event pair plus a wait duration rather
// than a child span, because the wait is human time.
const (
	EventToolConfirmRequested = "fisk.tool.confirm_requested"
	EventToolConfirmAnswered  = "fisk.tool.confirm_answered"
)

// The per-attempt HTTP events on a chat span, one pair because an attempt either got a
// response or did not get one at all. A transport failure has no status code to report,
// and treating it as a zero status would put a fictional code on the event.
const (
	EventLLMHTTPResponse = "fisk.llm.http_response"
	EventLLMHTTPError    = "fisk.llm.http_error"
)

// EventSessionRotated marks the point on the root span where a context reset moved the
// run to a new session. The root carries the starting id and, through
// fisk.session.end_id, the one it ended on; this records that the transition happened
// rather than leaving two ids on a span with no account of how.
const EventSessionRotated = "fisk.session.rotated"

// The four GenAI metric instruments this repository declares locally. They are in
// the current OpenTelemetry GenAI metrics registry but postdate the semconv package
// shipped with the Go SDK, so their names live here with the registry entry each
// tracks; gen_ai.client.token.usage and gen_ai.client.operation.duration do ship in
// semconv/v1.41.0/genaiconv and are taken from there instead of being named here.
//
// The instruments themselves are created where they are recorded, with the spans
// they belong to.
const (
	// MetricInvokeAgentDuration tracks the registry's gen_ai.invoke_agent.duration:
	// the duration of one agent invocation, recorded at the turn boundary in both
	// interactive and one-shot mode.
	MetricInvokeAgentDuration = "gen_ai.invoke_agent.duration"
	// MetricInvokeAgentInferenceCalls tracks the registry's
	// gen_ai.invoke_agent.inference_calls: the model calls made in one agent
	// invocation.
	MetricInvokeAgentInferenceCalls = "gen_ai.invoke_agent.inference_calls"
	// MetricInvokeAgentToolCalls tracks the registry's
	// gen_ai.invoke_agent.tool_calls: the tool calls made in one agent invocation.
	MetricInvokeAgentToolCalls = "gen_ai.invoke_agent.tool_calls"
	// MetricExecuteToolDuration tracks the registry's gen_ai.execute_tool.duration:
	// the duration of one tool execution.
	MetricExecuteToolDuration = "gen_ai.execute_tool.duration"
)

// MetricKnowledgeDegradedSearches counts hybrid searches that fell back to lexical.
//
// It is the only fisk-named instrument and the only counter, both deliberate. Every
// other signal in this package is a span attribute or a registry histogram, and a
// duration instrument here would add nothing that gen_ai.execute_tool.duration does not
// already measure end to end. This one exists because spans are head-sampled: at a
// sample ratio meant for a fleet, most degraded searches are never exported, so an
// embeddings outage that silently downgrades every query's quality is unalertable from
// the trace signal alone. Metrics are not sampled.
//
// Its only attribute is the degrade reason, which is a five-value closed vocabulary.
const MetricKnowledgeDegradedSearches = "fisk.knowledge.degraded_searches"

// MetricSessionAppendDuration tracks how long one journal append takes.
//
// It is a metric rather than a span, and that is the whole design rather than a
// convenience. A checkpointed run appends per record, so on the order of a hundred times
// in a thirty-iteration run; a span each would roughly double the run's span count with
// near-identical entries, bury the dozen spans that carry meaning in every flame graph,
// and cost more to record than the work it measures on the default backend, where an
// append is a local write. A uniform, high-frequency operation is what a histogram is
// for. It is also what makes this cheap to collect at all: recording it needs no context
// threaded through runstate.Store and Journal, because the caller already holds both the
// clock and the provider.
//
// What it answers is the one operational question the spans were wanted for: whether
// journaling is costing a run real time, and on which backend. The jetstream backend
// makes every append a network round trip inside the run loop, where nothing else
// reports it; the file backend is a local append and will sit in the first bucket, which
// is itself the answer.
//
// Its attributes are the backend and, on a failure, the error class. Both are closed
// vocabularies: a backend is one compiled into this build, and the class is an
// ErrorClass. Nothing about the session id, the record kind or the agent is here.
const MetricSessionAppendDuration = "fisk.session.append.duration"

// AttrSessionBackend is the session store backend that served a journal append.
//
// It is read from the configured backend name rather than from the store that served
// the call, which is the one narrowing worth knowing: a caller injecting their own
// runstate.Store through agent.Options is reported as whatever the config named. It is
// not closed now because the value is right for every configured backend, which is
// every run this ships with.
//
// runstate.Store.Info reports the store's own backend, so closing it is a matter of
// reading that instead. Two conditions come with doing so, and they are why it is not
// the one-line change it looks like. This is a metric label where the memory equivalent
// is a span attribute, and a label value mints a time series for the life of the
// process. So the value must be clamped against runstate.Backends(), falling back to a
// fixed literal for anything unregistered, because an injected store names itself and an
// embedder chooses that name; and an empty name must drop the attribute rather than
// record it, the way memoryAttrs does, because "" matches every run rather than none.
// Until both hold, the closed-vocabulary claim above stops being true.
var AttrSessionBackend = attribute.Key("fisk.session.backend")

// ErrorClass is the closed vocabulary for the error.type attribute.
//
// error.type is low-cardinality by spec, and this tree's errors embed absolute paths,
// config values and the config file path, none of which may leave the process on an
// attribute headed for a telemetry backend. So a caller must not be able to put an
// arbitrary string here.
//
// It is a struct wrapping a string rather than a defined string type, and the difference
// is the whole point. `type ErrorClass string` reads as closed and is not: Go allows
// conversion from any string by any package, so telemetry.ErrorClass(err.Error()) would
// compile and put an absolute path on a span. This shape has no exported way to build a
// value at all; a caller picks one below. That is a property the compiler enforces rather
// than one a code review has to remember, which is what the rest of this design assumes.
//
// The cost is that the members are variables rather than constants, since a struct cannot
// be one. Reassigning one is still possible and is a deliberate act rather than a slip,
// which is the failure this is defending against.
//
// There is deliberately no ErrorType(err) classifier over the domain sentinels: one
// would have to recognize a2a, runstate, agent and SDK error types, which would drag
// half the tree into this leaf. Each package names the class for the sentinel it is
// already inspecting.
type ErrorClass struct{ s string }

// String renders the class for the error.type attribute.
func (c ErrorClass) String() string { return c.s }

// Set reports whether a class was named at all, for a caller choosing between one it was
// handed and one it derives.
func (c ErrorClass) Set() bool { return c.s != "" }

var (
	// ClassConfig is a configuration or setup failure, including a refused resume.
	ClassConfig = ErrorClass{"config"}
	// ClassProvider is a failure reported by an upstream inference service: the model
	// backend, or the embeddings server. It covers both because an operator filtering on
	// it wants "something we call out to failed", and gen_ai.operation.name on the same
	// span already separates chat from embeddings. It does NOT cover a request this
	// process could not even build, which is a defect here rather than there.
	ClassProvider = ErrorClass{"provider"}
	// ClassTimeout is a deadline that expired.
	ClassTimeout = ErrorClass{"timeout"}
	// ClassCanceled is work abandoned because its context was canceled.
	ClassCanceled = ErrorClass{"canceled"}
	// ClassToolError is a tool that returned an error result.
	ClassToolError = ErrorClass{"tool_error"}
	// ClassTruncated is a response cut short by the output token limit.
	ClassTruncated = ErrorClass{"truncated"}
	// ClassRefusal is a response the model refused to give.
	ClassRefusal = ErrorClass{"refusal"}
	// ClassPanic is a recovered crash. The span records this class and a status
	// message only; see Span.Fail for why no stack ever accompanies it.
	ClassPanic = ErrorClass{"panic"}
	// ClassStore is a memory, knowledge or session store failure.
	ClassStore = ErrorClass{"store"}
	// ClassInvalidQuery is a query the caller could not compile, which for the knowledge
	// tools means one the model wrote badly. It is separated from ClassOther because it
	// names a distinct operator action: a run repeatedly reporting it has a prompt
	// problem rather than an infrastructure one.
	ClassInvalidQuery = ErrorClass{"invalid_query"}
	// ClassRemoteUnavailable is a remote agent that could not be reached.
	ClassRemoteUnavailable = ErrorClass{"remote_unavailable"}
	// ClassOther is the spec's catch-all for an error that fits no other class. It
	// is the value to reach for rather than inventing a new one at a call site.
	ClassOther = ErrorClass{"_OTHER"}
)

// errorType renders class as the error.type attribute, falling back to the catch-all when
// a failure arrived without one.
//
// It is the single place this attribute is built, which is what makes that fallback worth
// having: an empty error.type is not a value a backend can group by, so an outcome that
// failed to name its class would otherwise export a failure nobody can find. Imprecise
// beats absent, and the specs pin the fallback so it is a decision rather than an accident.
func errorType(class ErrorClass) attribute.KeyValue {
	if !class.Set() {
		class = ClassOther
	}

	return semconv.ErrorTypeKey.String(class.String())
}

// ClassifyContext returns the class for the two standard library context errors and
// reports whether err was one of them. It covers only those cases on purpose: every
// other classification needs a domain sentinel that this leaf must not import, so
// the calling package names the class itself.
func ClassifyContext(err error) (ErrorClass, bool) {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return ClassTimeout, true
	case errors.Is(err, context.Canceled):
		return ClassCanceled, true
	default:
		return ErrorClass{}, false
	}
}
