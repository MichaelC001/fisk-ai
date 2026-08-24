//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/semconv/v1.41.0/genaiconv"
	"go.opentelemetry.io/otel/trace"
)

// Span is a started span. Like Provider every method is safe on a nil receiver, so a
// run with telemetry off calls these unconditionally.
//
// Attributes are not settable from outside this package. Each span kind has one
// constructor here that owns its name, its kind and its attribute set, and typed
// setters for the values that only resolve later. That is what makes the two rules
// this design turns on enforceable rather than remembered: nothing model-controlled
// can reach a span name or a metric attribute, and error.type stays inside the closed
// ErrorClass vocabulary.
type Span struct {
	span trace.Span
	// started is when the span opened, so a Finish can record its duration metric from
	// the same place it ends the span. Reading the duration back off the exported span
	// is not an option: nothing hands it to the application.
	started time.Time
	// provider is held so a Finish can reach the instruments. It is the same Provider
	// that opened the span, never a global.
	provider *Provider
}

// elapsed is how long the span has been open.
func (s *Span) elapsed() time.Duration {
	return time.Since(s.started)
}

// End closes the span. An unended span is never exported, which is the failure mode
// this whole area is prone to, so every constructor's documentation says exactly
// where its End belongs.
func (s *Span) End() {
	if s == nil || s.span == nil {
		return
	}

	s.span.End()
}

// Fail marks the span failed with a class from the closed vocabulary.
//
// err decides whether there is a failure at all: a nil err is a no-op, which is what
// lets a deferred call site pass a named return without guarding it. Nothing derived
// from err reaches the span. That is not caution for its own sake: this tree's errors
// embed absolute paths, config values and the config file path, and the span status
// crosses a trust boundary to a telemetry backend where it cannot be un-sent.
//
// span.RecordError is never called, here or anywhere. The idiomatic OTel call records
// exception.stacktrace, which is precisely what the run path already works to keep
// off anything leaving the process. An implementer will reach for it later; do not.
func (s *Span) Fail(err error, class ErrorClass) {
	if s == nil || s.span == nil || err == nil {
		return
	}

	s.span.SetAttributes(errorType(class))
	s.span.SetStatus(codes.Error, "")
}

// TraceID returns the span's trace id as a hex string, or "" when nothing is being
// recorded. It is the correlator an operator needs to find this run in a backend, and
// the only one a chat run that is not checkpointed has, so the run surfaces it on its
// summary line rather than leaving it discoverable only from the backend side.
func (s *Span) TraceID() string {
	if s == nil || s.span == nil {
		return ""
	}

	sc := s.span.SpanContext()
	if !sc.HasTraceID() {
		return ""
	}

	return sc.TraceID().String()
}

// RunInfo is what is known about a run the moment it starts, which is very little: the
// provider is not built, the token budget is not resolved and the session id does not
// exist yet. Everything else arrives through the setters below.
type RunInfo struct {
	// Identity is the agent identity, operator-configured and validated, never model
	// output, which is what makes it safe in a span name.
	Identity string
	// Interactive selects the operation this run reports as. A one-shot run is a single
	// agent invocation, so its root is that invocation; an interactive chat is several
	// invocations of one agent, which is what a workflow is, so its root is the workflow
	// and each turn is an agent invocation under it. Naming both the same would put two
	// identically named bars on top of each other in a flame graph.
	Interactive bool
	// Resumed reports that this run continued a checkpointed session.
	Resumed bool
	// Model is the configured model. It is known at start because an agent has exactly
	// one, unlike the resolved snapshot a reply reports.
	Model string
}

// TokenUsage is the token tiers for a run or a call. Input includes the cached
// tiers, per the semantic conventions, which is why Uncached is carried separately:
// cache reads bill at roughly a tenth of the uncached rate, so a cost calculation
// needs the tiers apart and a reconciliation against the run summary needs Uncached.
type TokenUsage struct {
	Input       int64
	Output      int64
	CacheRead   int64
	CacheCreate int64
	Uncached    int64
	// Reasoning is the part of Output the model spent thinking, a subset rather than a
	// sixth tier. It is carried because reasoning is not rendered by default, so a
	// dashboard is the only place its cost shows, and because a model that reasons for
	// most of its output tokens is a different cost profile from one that does not.
	Reasoning int64
}

// Sub returns the tokens accumulated since other, for pulling one turn's or one call's
// usage out of a running total that spans the whole run.
func (u TokenUsage) Sub(other TokenUsage) TokenUsage {
	return TokenUsage{
		Input:       u.Input - other.Input,
		Output:      u.Output - other.Output,
		CacheRead:   u.CacheRead - other.CacheRead,
		CacheCreate: u.CacheCreate - other.CacheCreate,
		Uncached:    u.Uncached - other.Uncached,
		Reasoning:   u.Reasoning - other.Reasoning,
	}
}

// RunOutcome is everything about a run that is only known once it has ended.
type RunOutcome struct {
	// TerminalReason is how the run ended, from the closed set the run path already
	// uses, plus setup_failed for a run that never reached the loop.
	TerminalReason string
	// Crashed reports a recovered panic.
	Crashed bool
	// Class is the error class, or empty for a run that did not fail.
	Class ErrorClass
	// Failed reports whether the run ended in failure, which cannot be inferred from
	// Class alone since a successful run has no class.
	Failed bool

	// Usage is this process's tokens only. On a resumed run that is the run's own
	// consumption with the restored prefix subtracted, so summing this attribute across
	// a session's traces gives the session total exactly once.
	Usage TokenUsage
	// SessionUsage is the session-cumulative usage including any restored prefix, set
	// only for a resumed run. It is reported separately rather than in place of Usage
	// because the two answer different questions and conflating them is what makes a
	// resumed session over-report.
	SessionUsage *TokenUsage
	// SessionLLMCalls is the session-cumulative call count, set only when resumed.
	SessionLLMCalls int64

	// Turns is the interactive turn count, zero for a one-shot run.
	Turns int64
	// ToolCalls, RemoteToolCalls and MCPToolCalls are this process's counts. On a
	// resumed run all three have the restored counts subtracted, as Usage does, so the
	// remote and MCP counts stay subsets of the total.
	ToolCalls       int64
	RemoteToolCalls int64
	MCPToolCalls    int64
}

// RunSpan is the trace root: one span covering an entire agent run, which is what
// makes one run one trace.
type RunSpan struct {
	*Span
}

// StartRun starts the root span and returns a context carrying it, so every span the
// run opens nests underneath and the whole run is one trace.
//
// Its End belongs on a defer registered as early as possible and, critically, before
// any panic barrier: defers unwind last-registered-first, so a barrier registered
// afterwards runs first and the root's End runs last, seeing the final error the
// barrier substituted. Appending the End inside the barrier instead does not work,
// because a barrier that returns early on the non-crash path has no statement position
// that observes both outcomes.
func (p *Provider) StartRun(ctx context.Context, i RunInfo) (context.Context, *RunSpan) {
	if p == nil || p.tracer == nil {
		return ctx, &RunSpan{}
	}

	operation := genaiconv.OperationNameInvokeAgent
	if i.Interactive {
		operation = genaiconv.OperationNameInvokeWorkflow
	}

	attrs := []attribute.KeyValue{
		semconv.GenAIOperationNameKey.String(string(operation)),
		semconv.GenAIAgentName(i.Identity),
		AttrRunInteractive.Bool(i.Interactive),
		AttrRunResumed.Bool(i.Resumed),
	}
	if i.Interactive {
		attrs = append(attrs, semconv.GenAIWorkflowName(i.Identity))
	}
	if i.Model != "" {
		attrs = append(attrs, semconv.GenAIRequestModel(i.Model))
	}

	ctx, span := p.tracer.Start(ctx, string(operation)+" "+i.Identity,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attrs...),
	)

	return ctx, &RunSpan{Span: &Span{span: span, started: time.Now(), provider: p}}
}

// SetProvider records the model backend actually in use, which is known only once it
// has been resolved or a caller-injected one has been accepted.
func (s *RunSpan) SetProvider(name string) {
	if !s.recording() || name == "" {
		return
	}

	s.Span.span.SetAttributes(semconv.GenAIProviderNameKey.String(name))
}

// SetMaxTokens records the resolved per-response output cap, which is not known until
// the thinking setting and the provider ceiling have both been applied.
func (s *RunSpan) SetMaxTokens(n int64) {
	if !s.recording() || n <= 0 {
		return
	}

	s.Span.span.SetAttributes(semconv.GenAIRequestMaxTokens(int(n)))
}

// SetConversation records the session id a checkpointed run journals under.
//
// It takes the id the run STARTED on, set as soon as it resolves and before any turn,
// which is also the right time for an attribute a sampler might read. A context reset
// can rotate to a new session mid-run; that is reported by SetSessionRotated rather
// than by overwriting this, so every turn stays attributed to the session that
// actually journaled it.
func (s *RunSpan) SetConversation(id string) {
	if !s.recording() || id == "" {
		return
	}

	s.Span.span.SetAttributes(semconv.GenAIConversationID(id))
}

// SetSessionRotated records that a context reset moved the run to a fresh session, and
// which id it ended on: that is the one an operator resumes. Searching by either id
// finds the trace.
func (s *RunSpan) SetSessionRotated(endID string) {
	if !s.recording() || endID == "" {
		return
	}

	s.Span.span.SetAttributes(AttrSessionEndID.String(endID))
	s.Span.span.AddEvent(EventSessionRotated)
}

// SetFeatures records the run-constant model feature switches. They live on the root
// rather than being repeated on every chat span, where they would be export cost for
// no extra information.
func (s *RunSpan) SetFeatures(thinking, promptCache, toolSearch bool) {
	if !s.recording() {
		return
	}

	s.Span.span.SetAttributes(
		AttrLLMThinking.Bool(thinking),
		AttrLLMPromptCache.Bool(promptCache),
		AttrLLMToolSearch.Bool(toolSearch),
	)
}

// Finish records the outcome and ends the span. It replaces a bare End for the root,
// so the attributes that are only knowable at the end cannot be forgotten.
func (s *RunSpan) Finish(o RunOutcome) {
	if !s.recording() {
		return
	}

	span := s.Span.span
	span.SetAttributes(
		AttrRunTerminalReason.String(o.TerminalReason),
		AttrRunCrashed.Bool(o.Crashed),
		AttrRunToolCalls.Int64(o.ToolCalls),
		AttrRunRemoteToolCalls.Int64(o.RemoteToolCalls),
		AttrRunMCPToolCalls.Int64(o.MCPToolCalls),
		semconv.GenAIUsageInputTokens(int(o.Usage.Input)),
		semconv.GenAIUsageOutputTokens(int(o.Usage.Output)),
		semconv.GenAIUsageCacheReadInputTokens(int(o.Usage.CacheRead)),
		semconv.GenAIUsageCacheCreationInputTokens(int(o.Usage.CacheCreate)),
		semconv.GenAIUsageReasoningOutputTokens(int(o.Usage.Reasoning)),
		AttrLLMUncachedInputTokens.Int64(o.Usage.Uncached),
	)

	if o.Turns > 0 {
		span.SetAttributes(AttrRunTurns.Int64(o.Turns))
	}

	// Emitted only for a resumed run, because on a fresh run it would duplicate the
	// attributes above and invite someone to sum both.
	if o.SessionUsage != nil {
		span.SetAttributes(
			AttrSessionUsageInputTokens.Int64(o.SessionUsage.Input),
			AttrSessionUsageOutputTokens.Int64(o.SessionUsage.Output),
			AttrSessionUsageCacheReadTokens.Int64(o.SessionUsage.CacheRead),
			AttrSessionUsageCacheCreateTokens.Int64(o.SessionUsage.CacheCreate),
			AttrSessionLLMCalls.Int64(o.SessionLLMCalls),
		)
	}

	if o.Failed {
		span.SetAttributes(errorType(o.Class))
		// The status message stays empty even for a crash. PanicError's own text is a
		// fixed generic string and would be safe, but exporting it would establish that
		// error text can reach a span, and the next error to be passed here will not be
		// a fixed string.
		span.SetStatus(codes.Error, "")
	}

	span.End()
}

// recording reports whether this span is real, guarding every setter. The nil checks
// are all three deep on purpose: a disabled Provider returns an empty RunSpan whose
// embedded pointer is nil, and reaching a promoted method through a nil outer pointer
// would panic before any of them ran.
func (s *RunSpan) recording() bool {
	return s != nil && s.Span != nil && s.Span.span != nil
}

// TurnInfo describes one turn of an interactive run as it begins.
type TurnInfo struct {
	// Identity is the agent identity.
	Identity string
	// ConversationID is the session id current at the time, which is not necessarily
	// the one the run started on: a context reset rotates it mid-run, and attributing a
	// post-rotation turn to the earlier session would be wrong about which journal
	// holds it.
	ConversationID string
	// Index is the one-based position of this turn in the run.
	Index int64
}

// TurnOutcome is what a turn reports once it has ended.
type TurnOutcome struct {
	// TerminalReason is why this turn stopped, which is not why the run stopped: a turn
	// that hit the iteration cap ends the turn and hands back to the operator.
	TerminalReason string
	// Class is the error class, empty for a turn that did not fail.
	Class ErrorClass
	// Failed reports whether the turn ended in failure.
	Failed bool
	// Usage is this turn's tokens alone, a delta over the run's running totals.
	Usage TokenUsage
}

// TurnSpan is one turn of an interactive run.
type TurnSpan struct {
	*Span
}

// StartTurn starts a turn span under the run's root.
//
// It exists only for an interactive run. A one-shot run is a single agent invocation
// and its root already is that invocation, so adding a turn span there would put two
// identically named spans on top of each other, one exactly nested in the other,
// which reads in a flame graph as a bug rather than as structure.
func (p *Provider) StartTurn(ctx context.Context, i TurnInfo) (context.Context, *TurnSpan) {
	if p == nil || p.tracer == nil {
		return ctx, &TurnSpan{}
	}

	attrs := []attribute.KeyValue{
		semconv.GenAIOperationNameKey.String(string(genaiconv.OperationNameInvokeAgent)),
		semconv.GenAIAgentName(i.Identity),
		AttrTurnIndex.Int64(i.Index),
	}
	if i.ConversationID != "" {
		attrs = append(attrs, semconv.GenAIConversationID(i.ConversationID))
	}

	ctx, span := p.tracer.Start(ctx, string(genaiconv.OperationNameInvokeAgent)+" "+i.Identity,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attrs...),
	)

	return ctx, &TurnSpan{Span: &Span{span: span, started: time.Now(), provider: p}}
}

// Finish records the turn's outcome and ends the span.
func (s *TurnSpan) Finish(o TurnOutcome) {
	if s == nil || s.Span == nil || s.Span.span == nil {
		return
	}

	span := s.Span.span
	span.SetAttributes(
		AttrRunTerminalReason.String(o.TerminalReason),
		semconv.GenAIUsageInputTokens(int(o.Usage.Input)),
		semconv.GenAIUsageOutputTokens(int(o.Usage.Output)),
		semconv.GenAIUsageCacheReadInputTokens(int(o.Usage.CacheRead)),
		semconv.GenAIUsageCacheCreationInputTokens(int(o.Usage.CacheCreate)),
		semconv.GenAIUsageReasoningOutputTokens(int(o.Usage.Reasoning)),
		AttrLLMUncachedInputTokens.Int64(o.Usage.Uncached),
	)

	if o.Failed {
		span.SetAttributes(errorType(o.Class))
		span.SetStatus(codes.Error, "")
	}

	span.End()
}

// ChatInfo describes one model call as it is made.
type ChatInfo struct {
	// Model is the model the request names, which may be an alias.
	Model string
	// Provider is the gen_ai.provider.name value for the backend in use.
	Provider string
	// ConversationID is the session id current at the time, empty when the run is not
	// checkpointed.
	ConversationID string
	// MaxTokens is the per-response output cap this request carries.
	MaxTokens int64
	// Iteration is the loop index, which continues across a resume rather than
	// restarting.
	Iteration int64
	// Messages and Tools are the sizes of what was sent, not its content.
	Messages int
	Tools    int
	// Input renders the conversation this call carried, and is nil unless the caller
	// built one. It is invoked only when content capture is on.
	Input ContentBuilder
}

// ChatOutcome is what a model call reports once it returns.
type ChatOutcome struct {
	// ResponseID and ResponseModel come from the reply. ResponseModel is the resolved
	// snapshot that billed, which is not the alias the request named.
	ResponseID    string
	ResponseModel string
	// FinishReason is the provider's own stop reason, passed through rather than
	// mapped: the attribute has no enum precisely so a provider's vocabulary survives.
	FinishReason string
	// Usage is this call's tokens.
	Usage TokenUsage
	// Class is the error class, empty when the call did not fail.
	Class ErrorClass
	// Failed reports a failure, which includes a reply the loop treats as one: a turn
	// truncated at the output cap and a refusal both end the run, so both are errors on
	// the span even though the HTTP call succeeded.
	Failed bool
	// Output renders the model's reply, and is nil unless the caller built one.
	Output ContentBuilder
}

// ChatSpan is one model call.
type ChatSpan struct {
	*Span

	// attempts counts the HTTP requests made for this call, incremented by
	// HTTPMiddleware. It is derived here rather than read from a provider's retry-count
	// header: one middleware invocation is one attempt for any backend whose retries go
	// through its HTTP client, so nothing about this is vendor specific and no header
	// can silently stop being sent. Atomic because the counter and Finish are the only
	// two readers and neither should depend on how a backend sequences its retries.
	attempts atomic.Int64
}

// StartChat starts a span over one model call.
//
// It lives in the run loop rather than inside a provider on purpose. Here it is
// provider-neutral, it sees the neutral request and reply, and it covers a
// caller-injected provider, which HTTP middleware inside the default provider would
// never see. The cost is that one span covers all of an SDK's internal retries, so
// per-attempt detail belongs in span events rather than on the span itself.
func (p *Provider) StartChat(ctx context.Context, i ChatInfo) (context.Context, *ChatSpan) {
	if p == nil || p.tracer == nil {
		return ctx, &ChatSpan{}
	}

	attrs := []attribute.KeyValue{
		semconv.GenAIOperationNameKey.String(string(genaiconv.OperationNameChat)),
		semconv.GenAIRequestModel(i.Model),
		AttrLLMIteration.Int64(i.Iteration),
		AttrLLMMessages.Int(i.Messages),
		AttrLLMTools.Int(i.Tools),
	}
	if i.Provider != "" {
		attrs = append(attrs, semconv.GenAIProviderNameKey.String(i.Provider))
	}
	if i.ConversationID != "" {
		attrs = append(attrs, semconv.GenAIConversationID(i.ConversationID))
	}
	if i.MaxTokens > 0 {
		attrs = append(attrs, semconv.GenAIRequestMaxTokens(int(i.MaxTokens)))
	}

	ctx, span := p.tracer.Start(ctx, string(genaiconv.OperationNameChat)+" "+i.Model,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrs...),
	)

	cs := &ChatSpan{Span: &Span{span: span, started: time.Now(), provider: p}}

	// The returned context carries the span itself so HTTPMiddleware can annotate this
	// call rather than whatever span happens to be innermost by the time a request is
	// made. The caller must pass this context to the model call for that to work.
	return contextWithChatSpan(ctx, cs), cs
}

// Finish records the reply, records this call's metrics, and ends the span.
//
// The metrics are recorded here rather than at the call site so there is no second
// place to drift from the first: a span kind that has a metric owns recording it.
func (s *ChatSpan) Finish(ctx context.Context, i ChatInfo, o ChatOutcome) {
	if s == nil || s.Span == nil || s.Span.span == nil {
		return
	}

	s.Span.provider.recordChat(ctx, i, o, s.Span.elapsed())

	span := s.Span.span
	span.SetAttributes(
		semconv.GenAIUsageInputTokens(int(o.Usage.Input)),
		semconv.GenAIUsageOutputTokens(int(o.Usage.Output)),
		semconv.GenAIUsageCacheReadInputTokens(int(o.Usage.CacheRead)),
		semconv.GenAIUsageCacheCreationInputTokens(int(o.Usage.CacheCreate)),
		semconv.GenAIUsageReasoningOutputTokens(int(o.Usage.Reasoning)),
	)

	if o.ResponseID != "" {
		span.SetAttributes(semconv.GenAIResponseID(o.ResponseID))
	}
	if o.ResponseModel != "" {
		span.SetAttributes(semconv.GenAIResponseModel(o.ResponseModel))
	}
	if o.FinishReason != "" {
		span.SetAttributes(semconv.GenAIResponseFinishReasons(o.FinishReason))
	}

	// The resends, not the attempts: one attempt is a call that did not retry, and by
	// the conventions' definition that is a resend count of zero. It is set only when a
	// retry actually happened, since a zero on every span is export cost carrying no
	// information, and it is the one piece of per-attempt data that can live on the span
	// because it is monotonic: whichever attempt writes it last writes the right value.
	//
	// Its absence therefore means no retry was observed, which includes a run whose
	// provider was injected by a caller: the middleware is installed only on the
	// provider this package builds, so an injected one reports no attempts at all.
	if resends := s.attempts.Load() - 1; resends > 0 {
		span.SetAttributes(semconv.HTTPRequestResendCount(int(resends)))
	}

	// The input carries the index marker: it is the one attribute here whose position
	// in the conversation is a fact about the run rather than about this document.
	s.Span.recordContent(
		contentAttr{key: semconv.GenAIInputMessagesKey, build: i.Input, withIndex: true},
		contentAttr{key: semconv.GenAIOutputMessagesKey, build: o.Output},
	)

	if o.Failed {
		span.SetAttributes(errorType(o.Class))
		span.SetStatus(codes.Error, "")
	}

	span.End()
}

// unknownToolSpanName is the span name for a call naming a tool that does not exist.
// It is a constant, and that is the point: the name the model asked for is unvalidated
// input, so putting it in a span name would let a hallucinating model, or a prompt
// injection written for exactly this, mint unbounded span names.
const unknownToolSpanName = "execute_tool unknown_tool"

// ToolInfo describes a tool call as it begins, before any hook has seen it.
type ToolInfo struct {
	// Name is the registry-validated tool name, empty when the model named a tool that
	// does not exist. Only this reaches the span name.
	Name string
	// RequestedName is the model's raw string, recorded only when Name is empty.
	RequestedName string
	// CallID is the model's id for this call, which correlates it with the reply.
	CallID string
	// Identity is the agent identity.
	Identity string
	// Kind is the provider that supplied the tool.
	Kind string
	// Datastore marks the knowledge tools, which the conventions type as a datastore
	// rather than a function. A remote agent's tool is a function: it is client-side
	// from the model's view, and extension means provider-side.
	Datastore bool
	// ConfirmGated reports that the call needs operator approval.
	ConfirmGated bool
	// Resumed marks a tool from a batch a resume is completing.
	Resumed bool
}

// ToolOutcome is what a tool call reports once it has finished, whether or not it ran.
type ToolOutcome struct {
	// Outcome is one of the closed ToolOutcome* set.
	Outcome string
	// Name is the effective tool name after any rewrite, empty on the unknown path.
	Name string
	// Kind is the effective tool's kind, which a rewrite can change.
	Kind string
	// ArgKeys is the argument key names, never their values.
	ArgKeys []string
	// Rewritten reports that a hook redirected the call or its arguments.
	Rewritten bool
	// Remote and RemoteAgent describe a call dispatched to another agent.
	Remote      bool
	RemoteAgent string
	// ConfirmWait is how long the call waited on the operator, zero when it did not.
	ConfirmWait time.Duration
	// Failed reports an error result, which is not the same as a failing command: a
	// non-zero exit round-trips as an ordinary result.
	Failed bool
	// ExitCode is the status of the command the call ran, and is nil when it ran none.
	// A pointer because zero is a real exit status: a plain int would report a
	// successful command for every built-in and every remote tool, which never run one.
	ExitCode *int
	// Memory names the store a memory tool call was served by, and is the zero value
	// for every tool that is not one. It is here rather than on ToolInfo because a hook
	// can rewrite a call: the store that answers is the one the effective tool is bound
	// to, which is not known when the span opens.
	Memory MemoryInfo
	// Arguments and Result render the call's content, and are nil unless the caller
	// built them. Arguments describe the effective call after any rewrite, and Result
	// is what the model was told, which includes the answer a call that never ran was
	// given: an unknown tool, a policy denial, a missing argument and an operator's
	// refusal all return something the model then acts on, and a trace that showed the
	// call but not the answer would be describing half of what happened.
	Arguments ContentBuilder
	Result    ContentBuilder
}

// ToolSpan is one tool call.
type ToolSpan struct {
	*Span
}

// StartTool starts a span over one tool call.
//
// It covers the whole of the call's handling, from before the tool is looked up to
// every way it can end, so a call that never ran is still visible: an unknown tool, a
// policy denial, a missing argument and an operator refusal are all outcomes an
// operator needs to see, and none of them execute anything. With eight ways to leave
// that handling, the call site ends this from a defer rather than at each return.
func (p *Provider) StartTool(ctx context.Context, i ToolInfo) (context.Context, *ToolSpan) {
	if p == nil || p.tracer == nil {
		return ctx, &ToolSpan{}
	}

	toolType := ToolTypeFunction
	if i.Datastore {
		toolType = ToolTypeDatastore
	}

	attrs := []attribute.KeyValue{
		semconv.GenAIOperationNameKey.String(string(genaiconv.OperationNameExecuteTool)),
		semconv.GenAIToolCallID(i.CallID),
		semconv.GenAIToolTypeKey.String(string(toolType)),
		semconv.GenAIAgentName(i.Identity),
		AttrToolKind.String(i.Kind),
		AttrToolConfirmGated.Bool(i.ConfirmGated),
		AttrToolResumed.Bool(i.Resumed),
	}

	name := unknownToolSpanName
	if i.Name != "" {
		name = string(genaiconv.OperationNameExecuteTool) + " " + i.Name
		attrs = append(attrs, semconv.GenAIToolName(i.Name))
	} else if i.RequestedName != "" {
		// Span attribute only. See AttrToolRequestedName for why it goes no further.
		attrs = append(attrs, AttrToolRequestedName.String(i.RequestedName))
	}

	ctx, span := p.tracer.Start(ctx, name,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attrs...),
	)

	return ctx, &ToolSpan{Span: &Span{span: span, started: time.Now(), provider: p}}
}

// ConfirmRequested marks the moment the operator was asked to approve the call.
func (s *ToolSpan) ConfirmRequested() {
	if s == nil || s.Span == nil || s.Span.span == nil {
		return
	}

	s.Span.span.AddEvent(EventToolConfirmRequested)
}

// ConfirmAnswered marks the moment the operator answered.
func (s *ToolSpan) ConfirmAnswered() {
	if s == nil || s.Span == nil || s.Span.span == nil {
		return
	}

	s.Span.span.AddEvent(EventToolConfirmAnswered)
}

// Finish records the outcome, records the tool duration, and ends the span.
func (s *ToolSpan) Finish(ctx context.Context, o ToolOutcome) {
	if s == nil || s.Span == nil || s.Span.span == nil {
		return
	}

	s.Span.provider.recordTool(ctx, o, s.Span.elapsed())

	span := s.Span.span
	span.SetAttributes(
		AttrToolOutcome.String(o.Outcome),
		AttrToolRewritten.Bool(o.Rewritten),
		AttrToolRemote.Bool(o.Remote),
	)

	// The effective name after a rewrite, which is the tool that actually ran. It is
	// still registry-validated: a rewrite can only name a registered tool.
	if o.Name != "" {
		span.SetAttributes(semconv.GenAIToolName(o.Name))
	}
	if o.Kind != "" {
		span.SetAttributes(AttrToolKind.String(o.Kind))
	}
	if len(o.ArgKeys) > 0 {
		span.SetAttributes(AttrToolArgKeys.StringSlice(o.ArgKeys))
	}
	if o.RemoteAgent != "" {
		span.SetAttributes(AttrToolRemoteAgent.String(o.RemoteAgent))
	}
	if o.ConfirmWait > 0 {
		span.SetAttributes(AttrToolConfirmWaitMS.Int64(o.ConfirmWait.Milliseconds()))
	}
	// Set whenever a command ran, INCLUDING when it exited zero. Reporting only failures
	// is the natural reading and it is wrong: it would make a successful command
	// indistinguishable from a built-in that never ran one, and it would make this field
	// a bool wearing an int's clothes.
	if o.ExitCode != nil {
		span.SetAttributes(AttrToolExitCode.Int(*o.ExitCode))
	}
	// Memory tool calls only. Without it "which backend was this slow memory_write on"
	// is a walk to the startup span in the same trace, which no backend can express as
	// one query, and it is not a metrics question either: putting the backend on the
	// tool duration histogram would leave it empty on every tool that is not a memory
	// tool.
	memAttrs := memoryAttrs(o.Memory)
	if len(memAttrs) > 0 {
		span.SetAttributes(memAttrs...)
	}

	s.Span.recordContent(
		contentAttr{key: semconv.GenAIToolCallArgumentsKey, build: o.Arguments},
		contentAttr{key: semconv.GenAIToolCallResultKey, build: o.Result},
	)

	if o.Failed {
		span.SetAttributes(errorType(ClassToolError))
		span.SetStatus(codes.Error, "")
	}

	span.End()
}

// ServedToolInfo describes one tool call this agent answers for a peer.
type ServedToolInfo struct {
	// Identity is this agent, which is what gen_ai.agent.name means on every other
	// span in the process. This is the opposite of a remote-agent span, where the name
	// would have been the peer's.
	Identity string
	// Name is the tool as this agent knows it, empty when the peer named one that does
	// not exist. Only this reaches the span name.
	Name string
	// RequestedName is the peer's raw string, recorded only when Name is empty. It is
	// unvalidated input from another process, so it goes no further than an attribute.
	RequestedName string
	// Request is the correlation tag of the peer's request, so a span is findable from
	// the log line that names the same call.
	Request string
	// Caller is what the transport vouches for about who is calling, and Verified says
	// whether it vouches for anything at all. They stay apart from Sender for the
	// reason a2a.Caller exists: a body can claim any sender.
	Caller         string
	CallerVerified bool
	// Sender is the identity the message claims, which is the peer's own assertion and
	// is not evidence of anything.
	Sender string
}

// ServedToolOutcome is what a served call reports once it has finished.
type ServedToolOutcome struct {
	// Outcome is one of the closed ToolOutcome* set, shared with a local call so the
	// two are comparable on one key.
	Outcome string
	// Failed reports that the peer was told the call failed. It cannot be inferred
	// from a Go error: a tool that failed is reported in-band on the reply.
	Failed bool
	// ExitCode is the status of the command the call ran, nil when it ran none.
	ExitCode *int
}

// ServedToolSpan is one tool call answered for a peer.
type ServedToolSpan struct {
	*Span
}

// StartServedTool starts a span over one tool call this agent answers.
//
// It is an execute_tool span with server kind rather than a mirror of
// StartRemoteAgent, because the two halves of one hop are different operations: the
// caller invokes an agent, and this runs one tool with no prompt and no loop. A trace
// reads execute_tool on the caller, then invoke_agent, then this.
//
// It records NO metric. The tool duration histogram means tools this agent's model
// ran, and feeding peer-invoked calls into it would make a percentile over that series
// describe two populations.
//
// The span opens before the server decides whether it has a slot, so a call refused for
// capacity is a span of its own rather than a request that left no trace. A saturated
// server therefore renders as a server answering, with its outcomes saying what it
// answered.
func (p *Provider) StartServedTool(ctx context.Context, i ServedToolInfo) (context.Context, *ServedToolSpan) {
	if p == nil || p.tracer == nil {
		return ctx, &ServedToolSpan{}
	}

	attrs := []attribute.KeyValue{
		semconv.GenAIOperationNameKey.String(string(genaiconv.OperationNameExecuteTool)),
		semconv.GenAIToolTypeKey.String(string(ToolTypeFunction)),
		semconv.GenAIAgentName(i.Identity),
		AttrServedRequest.String(i.Request),
		AttrServedCallerVerified.Bool(i.CallerVerified),
	}

	name := unknownToolSpanName
	if i.Name != "" {
		name = string(genaiconv.OperationNameExecuteTool) + " " + i.Name
		attrs = append(attrs, semconv.GenAIToolName(i.Name))
	} else if i.RequestedName != "" {
		attrs = append(attrs, AttrToolRequestedName.String(i.RequestedName))
	}

	if i.Caller != "" {
		attrs = append(attrs, AttrServedCaller.String(i.Caller))
	}
	if i.Sender != "" {
		attrs = append(attrs, AttrServedSender.String(i.Sender))
	}

	ctx, span := p.tracer.Start(ctx, name,
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(attrs...),
	)

	return ctx, &ServedToolSpan{Span: &Span{span: span, started: time.Now(), provider: p}}
}

// Finish records the outcome and ends the span. It is called after the reply has been
// sent, since a reply that failed to send is a peer that got nothing, and a green
// server span under a red caller's span describes the wrong thing.
func (s *ServedToolSpan) Finish(o ServedToolOutcome) {
	if s == nil || s.Span == nil || s.Span.span == nil {
		return
	}

	span := s.Span.span
	span.SetAttributes(AttrToolOutcome.String(o.Outcome))

	if o.ExitCode != nil {
		span.SetAttributes(AttrToolExitCode.Int(*o.ExitCode))
	}

	if o.Failed {
		span.SetAttributes(errorType(ClassToolError))
		span.SetStatus(codes.Error, "")
	}

	span.End()
}

// RemoteAgentInfo describes a tool call dispatched to another agent.
type RemoteAgentInfo struct {
	// Agent is the peer's name. It is operator configuration, the name of a configured
	// remote host, never model output, which is what makes it safe in a span name.
	Agent string
	// Tool is the tool name as the peer advertised it. It is peer-controlled rather
	// than operator-controlled, so it is a span attribute and never a metric label.
	Tool string
}

// RemoteAgentOutcome is what a remote call reports once it returns.
type RemoteAgentOutcome struct {
	// Class is the error class, empty when the call succeeded.
	Class ErrorClass
	// Failed reports a failure. It cannot be inferred from a Go error: a tool that
	// failed on the far side reports it in-band on the reply with no Go error at all,
	// so a span keyed on the error alone would render a failed call green underneath a
	// red parent.
	Failed bool
}

// RemoteAgentSpan is one tool call dispatched to another agent.
type RemoteAgentSpan struct {
	*Span
}

// StartRemoteAgent starts a span over one invocation of a tool on a remote agent.
//
// It nests under the execute_tool span the runner opened for the local alias, so a trace
// reads local name, then peer, then remote tool, and the hop has its own latency and
// error class even though the far side stays a black box.
//
// It records NO metric. gen_ai.invoke_agent.duration means one turn of this agent, and
// section 7 went out of its way to make that series mean exactly one thing; feeding peer
// invocations into it would mix in another agent's work and make a percentile over it
// meaningless.
//
// gen_ai.agent.name is deliberately absent: on every other span in the trace it is this
// agent's identity, and reusing it for the peer would make a backend filter on that key
// start returning other agents' spans. The peer is in the span name and on
// fisk.tool.remote_agent, which the parent execute_tool span already carries.
func (p *Provider) StartRemoteAgent(ctx context.Context, i RemoteAgentInfo) (context.Context, *RemoteAgentSpan) {
	// The empty value with a nil inner Span, never a nil *RemoteAgentSpan: End and Fail
	// are promoted from the embedded pointer, and a promoted method reached through a
	// nil outer pointer must dereference it to find the field, so returning nil here
	// panics at every call site the moment telemetry is off.
	if p == nil || p.tracer == nil {
		return ctx, &RemoteAgentSpan{}
	}

	attrs := []attribute.KeyValue{
		semconv.GenAIOperationNameKey.String(string(genaiconv.OperationNameInvokeAgent)),
		AttrToolRemoteAgent.String(i.Agent),
	}
	if i.Tool != "" {
		attrs = append(attrs, semconv.GenAIToolName(i.Tool))
	}

	ctx, span := p.tracer.Start(ctx, string(genaiconv.OperationNameInvokeAgent)+" "+i.Agent,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrs...),
	)

	return ctx, &RemoteAgentSpan{Span: &Span{span: span, started: time.Now(), provider: p}}
}

// Finish records the outcome and ends the span.
func (s *RemoteAgentSpan) Finish(o RemoteAgentOutcome) {
	if s == nil || s.Span == nil || s.Span.span == nil {
		return
	}

	if o.Failed {
		s.Span.span.SetAttributes(errorType(o.Class))
		s.Span.span.SetStatus(codes.Error, "")
	}

	s.Span.span.End()
}

// StartupInfo is what is known about a run when setup begins.
type StartupInfo struct {
	// Identity is the agent identity. It is operator-configured and validated to a
	// restricted character set before it ever reaches here, never model-controlled,
	// which is what makes it safe in a span name.
	Identity string
	// RemoteHosts is the number of configured remote tool hosts, so a slow startup
	// has a visible reason.
	RemoteHosts int
}

// ToolCounts is the tool inventory a run resolved during setup, partitioned by where
// each tool came from. Deferred reports whether the set was put behind server-side
// tool search rather than sent to the model directly.
type ToolCounts struct {
	Application int
	Builtin     int
	Remote      int
	Custom      int
	Deferred    bool
}

// StartupSpan covers local process startup: loading tools, dialing NATS, opening the
// stores, importing remote tools, and resolving or restoring the session.
type StartupSpan struct {
	*Span
}

// StartStartup starts the startup span and returns a context carrying it, so the
// spans opened during setup nest under it.
//
// It is deliberately not the create_agent operation and carries no
// gen_ai.operation.name. That operation means creating an agent definition or
// instance and carries gen_ai.agent.id and gen_ai.agent.description; a GenAI backend
// reading it would report one agent creation per run and charge tool-import time as
// creation cost. This measures local process startup, so it carries fisk.* only.
//
// The span must end at the handoff to the run loop, and it must also end on every
// early return before that handoff. Those early returns include the slow paths this
// span exists to make visible, so the call site pairs one explicit End at the handoff
// with a guarded deferred End that catches the rest.
func (p *Provider) StartStartup(ctx context.Context, i StartupInfo) (context.Context, *StartupSpan) {
	if p == nil || p.tracer == nil {
		// An empty StartupSpan, not a nil one. End and Fail are promoted from the
		// embedded *Span, and a promoted method reached through a nil outer pointer has
		// to dereference that pointer to find the field, so a nil *StartupSpan would
		// panic at every call site the moment telemetry is off. The inner *Span being
		// nil is fine: its own methods take a nil receiver.
		return ctx, &StartupSpan{}
	}

	ctx, span := p.tracer.Start(ctx, "startup "+i.Identity,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(AttrRemoteHosts.Int(i.RemoteHosts)),
	)

	return ctx, &StartupSpan{Span: &Span{span: span, started: time.Now(), provider: p}}
}

// SetTools records the tool inventory once setup has resolved it. It is separate
// from StartStartup because the counts are not known until well into setup, and the
// span has to exist before then so the failures on the way there are recorded.
func (s *StartupSpan) SetTools(c ToolCounts) {
	if s == nil || s.Span == nil || s.Span.span == nil {
		return
	}

	s.Span.span.SetAttributes(
		AttrToolsApplication.Int(c.Application),
		AttrToolsBuiltin.Int(c.Builtin),
		AttrToolsRemote.Int(c.Remote),
		AttrToolsCustom.Int(c.Custom),
		AttrToolsDeferred.Bool(c.Deferred),
	)
}

// SearchInfo is what is known when a knowledge search starts.
type SearchInfo struct {
	// Hybrid reports that the vector tier is configured, which is the tier the search was
	// asked for rather than the one it managed to run.
	Hybrid bool
	// TopK is the effective result ceiling, already defaulted and clamped, so a value the
	// model asked for cannot reach a span unbounded.
	TopK int
}

// SearchOutcome is everything a search knows once it has finished or failed.
//
// Its zero value is deliberately not a success. A search has nine ways to return and
// five of them abandon the result entirely, so an outcome that defaulted to "ok" would
// export a green, plausible-looking span alongside error.type on every failure path.
type SearchOutcome struct {
	// Status is the soft outcome the search reports without treating it as an error, or
	// empty when it failed outright.
	Status string
	// EffectiveTier is the tier that actually ran, empty when neither retriever did.
	EffectiveTier string
	// Sections is how many chunks were returned, and IndexedChunks the corpus size the
	// search counted. IndexedChunks is nil when no index existed to count.
	Sections      int
	IndexedChunks *int
	// Degraded reports a fallback to lexical, and Degrade names why.
	Degraded bool
	Degrade  DegradeReason
	// Class is the error class and Failed reports that the search returned an error.
	Class  ErrorClass
	Failed bool
}

// SearchSpan is one knowledge search.
type SearchSpan struct {
	*Span
}

// StartSearch starts a span over one knowledge search.
//
// The returned context MUST be used for the rest of the search: it is what makes the
// embeddings request a child of this span rather than a sibling of it under whatever
// opened the enclosing span. A trace where the two are siblings has every name and every
// attribute right and still reports the wrong thing, and only a comparison of parent ids
// tells them apart.
//
// The span is started before the index-not-built check, so the ordinary first run of an
// agent whose index has never been built produces a span saying so rather than nothing.
func (p *Provider) StartSearch(ctx context.Context, i SearchInfo) (context.Context, *SearchSpan) {
	// The empty value with a nil inner Span, never a nil *SearchSpan: End and Fail are
	// promoted from the embedded pointer, and a promoted method reached through a nil
	// outer pointer must dereference it to find the field.
	if p == nil || p.tracer == nil {
		return ctx, &SearchSpan{}
	}

	tier := TierLexical
	if i.Hybrid {
		tier = TierHybrid
	}

	ctx, span := p.tracer.Start(ctx, string(genaiconv.OperationNameRetrieval),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			semconv.GenAIOperationNameKey.String(string(genaiconv.OperationNameRetrieval)),
			AttrKnowledgeTierConfigured.String(tier),
			AttrKnowledgeTopK.Int(i.TopK),
		),
	)

	return ctx, &SearchSpan{Span: &Span{span: span, started: time.Now(), provider: p}}
}

// Finish records the outcome and ends the span.
//
// The call site builds the outcome explicitly at each of the search's terminal points
// rather than reading a partially assembled result: the result value is nil on the error
// paths and pre-initialized to a success on some others, so a deferred read of it would
// either dereference nil inside a defer or export a successful search that failed.
//
// gen_ai.retrieval.query.text and gen_ai.retrieval.documents are deliberately absent,
// permanently rather than pending content capture. Both ship as constants in the
// conventions package this file already imports, so they are the obvious things to reach
// for; the first is the model's raw query and is flagged in the conventions themselves as
// possibly sensitive, and the second is document ids and relevance scores, which here
// means corpus paths.
//
// Content capture does not change that. What the search returned already reaches the wire
// by a shorter route when it is on, as gen_ai.tool.call.result on the execute_tool span
// above this one, so exporting it here too would publish the same corpus paths twice under
// two vocabularies with nothing saying which is authoritative.
func (s *SearchSpan) Finish(ctx context.Context, o SearchOutcome) {
	if s == nil || s.Span == nil || s.Span.span == nil {
		return
	}

	span := s.Span.span
	if o.Status != "" {
		span.SetAttributes(AttrKnowledgeSearchStatus.String(o.Status))
	}
	if o.EffectiveTier != "" {
		span.SetAttributes(AttrKnowledgeTierEffective.String(o.EffectiveTier))
	}
	if o.IndexedChunks != nil {
		span.SetAttributes(AttrKnowledgeIndexedChunks.Int(*o.IndexedChunks))
	}
	if o.Degraded {
		span.SetAttributes(
			AttrKnowledgeDegraded.Bool(true),
			AttrKnowledgeSections.Int(o.Sections),
		)
		if o.Degrade.Set() {
			span.SetAttributes(AttrKnowledgeDegradedReason.String(o.Degrade.String()))
		}
		s.Span.provider.recordDegradedSearch(ctx, o.Degrade)
	} else if !o.Failed {
		span.SetAttributes(AttrKnowledgeSections.Int(o.Sections))
	}

	if o.Failed {
		span.SetAttributes(errorType(o.Class))
		span.SetStatus(codes.Error, "")
	}

	span.End()
}

// EnumerateInfo is what is known when a knowledge enumeration starts.
type EnumerateInfo struct {
	// Limit caps the documents returned, zero for the complete set.
	Limit int
	// MinBodyMatches is the aboutness filter, applied before the matched count is taken,
	// which is why it has to be on the span for that count to be readable.
	MinBodyMatches int
}

// EnumerateOutcome is everything an enumeration knows once it has finished or failed.
// Its zero value is not a success, for the reason SearchOutcome's is not.
type EnumerateOutcome struct {
	// Status is the soft outcome, or empty when the enumeration failed outright.
	Status string
	// Matched is the complete matched set after the aboutness filter and before any
	// limit; Documents is what was returned; Truncated reports that they differ.
	Matched   int
	Documents int
	Truncated bool
	// IndexedDocuments is the corpus size, nil when no index existed to count.
	IndexedDocuments *int
	// Class is the error class and Failed reports that the enumeration returned an error.
	Class  ErrorClass
	Failed bool
}

// EnumerateSpan is one knowledge enumeration.
type EnumerateSpan struct {
	*Span
}

// StartEnumerate starts a span over one knowledge enumeration.
//
// It is deliberately not the retrieval operation and carries no gen_ai.operation.name,
// on the same reasoning that keeps the startup span from being create_agent. Enumeration
// never ranks, never fuses vectors and returns no document text; it answers set
// membership over a corpus, which no standard GenAI operation describes.
//
// Its own span name rather than a shared one with an attribute to tell them apart is
// what keeps the two operations' latencies separable. Span name is the primary
// aggregation key in every backend this exports to, and these two have very different
// cost profiles, so sharing a name would put two populations in one series recoverable
// only by a join no alert rule can do.
func (p *Provider) StartEnumerate(ctx context.Context, i EnumerateInfo) (context.Context, *EnumerateSpan) {
	if p == nil || p.tracer == nil {
		return ctx, &EnumerateSpan{}
	}

	attrs := []attribute.KeyValue{}
	if i.Limit > 0 {
		attrs = append(attrs, AttrKnowledgeLimit.Int(i.Limit))
	}
	if i.MinBodyMatches > 0 {
		attrs = append(attrs, AttrKnowledgeMinBodyMatches.Int(i.MinBodyMatches))
	}

	ctx, span := p.tracer.Start(ctx, "knowledge_enumerate",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attrs...),
	)

	return ctx, &EnumerateSpan{Span: &Span{span: span, started: time.Now(), provider: p}}
}

// Finish records the outcome and ends the span.
func (s *EnumerateSpan) Finish(o EnumerateOutcome) {
	if s == nil || s.Span == nil || s.Span.span == nil {
		return
	}

	span := s.Span.span
	if o.Status != "" {
		span.SetAttributes(AttrKnowledgeEnumerateStatus.String(o.Status))
	}
	if o.IndexedDocuments != nil {
		span.SetAttributes(AttrKnowledgeIndexedDocuments.Int(*o.IndexedDocuments))
	}
	if !o.Failed {
		span.SetAttributes(
			AttrKnowledgeMatched.Int(o.Matched),
			AttrKnowledgeDocuments.Int(o.Documents),
		)
		if o.Truncated {
			span.SetAttributes(AttrKnowledgeTruncated.Bool(true))
		}
	}

	if o.Failed {
		span.SetAttributes(errorType(o.Class))
		span.SetStatus(codes.Error, "")
	}

	span.End()
}

// EmbeddingsInfo describes one request to the embeddings server.
type EmbeddingsInfo struct {
	// Model is the configured embeddings model, operator configuration and never model
	// output, which is what makes it safe in a span name.
	Model string
	// Purpose separates a query embedding from the dimension probe.
	Purpose string
	// Inputs is how many texts this request carried.
	Inputs int
	// ServerAddress and ServerPort name the embeddings endpoint, parsed out by the caller
	// so the raw base URL never reaches here: a base URL can carry userinfo credentials,
	// and the host alone is what answers "which deployment is slow" across a fleet.
	ServerAddress string
	ServerPort    int
}

// EmbeddingsOutcome is what one embeddings request reports once it returns.
type EmbeddingsOutcome struct {
	// StatusCode is the HTTP status, zero when no response arrived at all.
	StatusCode int
	// Class is the error class and Failed reports a failure.
	Class  ErrorClass
	Failed bool
}

// EmbeddingsSpan is one request to the embeddings server.
type EmbeddingsSpan struct {
	*Span
}

// StartEmbeddings starts a span over one embeddings request.
//
// It records no metric. gen_ai.client.operation.duration would be the fit, but it takes
// gen_ai.provider.name as a required attribute and this build has no honest value for an
// arbitrary OpenAI-compatible endpoint: the config carries a base URL and a model, not a
// provider identity, and naming a vendor for what is usually a local server would be a
// fabrication. The span carries its own duration and the enclosing tool span already
// measures the operation end to end.
func (p *Provider) StartEmbeddings(ctx context.Context, i EmbeddingsInfo) (context.Context, *EmbeddingsSpan) {
	if p == nil || p.tracer == nil {
		return ctx, &EmbeddingsSpan{}
	}

	attrs := []attribute.KeyValue{
		semconv.GenAIOperationNameKey.String(string(genaiconv.OperationNameEmbeddings)),
		semconv.GenAIRequestModel(i.Model),
		AttrEmbeddingsInputs.Int(i.Inputs),
	}
	if i.Purpose != "" {
		attrs = append(attrs, AttrEmbeddingsPurpose.String(i.Purpose))
	}
	if i.ServerAddress != "" {
		attrs = append(attrs, semconv.ServerAddress(i.ServerAddress))
	}
	if i.ServerPort > 0 {
		attrs = append(attrs, semconv.ServerPort(i.ServerPort))
	}

	ctx, span := p.tracer.Start(ctx, string(genaiconv.OperationNameEmbeddings)+" "+i.Model,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrs...),
	)

	return ctx, &EmbeddingsSpan{Span: &Span{span: span, started: time.Now(), provider: p}}
}

// Finish records the outcome and ends the span.
//
// http.response.status_code sits on the span here, where the chat span deliberately
// keeps it off and reports per-attempt events instead. The reason inverts cleanly: a
// chat span is N HTTP requests, so a last-attempt-wins status would report 200 for a
// call that spent its wall clock being rate limited, while this span is exactly one
// request and the code describes it exactly. It is absent when the request never got a
// response, which is what a DNS failure, a reset or a TLS error produces.
func (s *EmbeddingsSpan) Finish(o EmbeddingsOutcome) {
	if s == nil || s.Span == nil || s.Span.span == nil {
		return
	}

	span := s.Span.span
	if o.StatusCode > 0 {
		span.SetAttributes(semconv.HTTPResponseStatusCode(o.StatusCode))
	}
	if o.Failed {
		span.SetAttributes(errorType(o.Class))
		span.SetStatus(codes.Error, "")
	}

	span.End()
}

// MemoryInfo describes the memory store a run bound, as the store itself reports it.
type MemoryInfo struct {
	// Backend is the backend name the store reports.
	Backend string
	// Location is the container it is bound to, empty for a backend with nothing safe
	// to name.
	Location string
}

// memoryAttrs renders a store's identity. The location is absent rather than empty for
// a backend with nothing safe to name, so a query for it selects the backends that have
// one instead of matching every file-backed run on "".
//
// It is one function because three spans carry this pair: startup, memory_index and a
// memory tool call. Each of those has a different reason to be told which store ran, and
// none of them should be able to render it differently from the others.
func memoryAttrs(i MemoryInfo) []attribute.KeyValue {
	if i.Backend == "" {
		return nil
	}

	attrs := []attribute.KeyValue{AttrMemoryBackend.String(i.Backend)}
	if i.Location != "" {
		attrs = append(attrs, AttrMemoryLocation.String(i.Location))
	}

	return attrs
}

// SetMemory records the memory backend once setup has bound one.
//
// It lives on this span as well as on each memory tool call because a run can bind a
// store, have the index turned off and never call a memory tool, and its trace would
// then say nothing anywhere about memory existing at all.
func (s *StartupSpan) SetMemory(i MemoryInfo) {
	if s == nil || s.Span == nil || s.Span.span == nil {
		return
	}

	attrs := memoryAttrs(i)
	if len(attrs) == 0 {
		return
	}

	s.Span.span.SetAttributes(attrs...)
}

// SetSystemInstructions records the system prompt, when content capture is on.
//
// It lives on this span rather than on each model call for the reason section 6.1
// already gives for the feature switches: the system prompt is a run constant, built
// during setup and never changed by the loop, so putting it on the chat span would
// export an identical copy per iteration.
//
// startup rather than the root, which also holds run constants, because the root does
// not end until the run does: on an interactive chat the prompt would reach the
// collector only when the operator quits, and on a killed process never at all, which
// is the run someone most wants to read. This span ends at the handoff.
//
// The call site has to be the last thing before that handoff, not where the other
// startup setters sit. The prompt is not final where it looks final: a resumed run
// appends its reminder and a memory-enabled run appends the memory index well after
// the tool inventory resolves, and a value set early is short, plausible and wrong in
// a way no assertion on presence or shape can see.
func (s *StartupSpan) SetSystemInstructions(b ContentBuilder) {
	if s == nil || s.Span == nil {
		return
	}

	s.Span.recordContent(contentAttr{key: semconv.GenAISystemInstructionsKey, build: b})
}

// MemorySpan is one memory operation during setup.
type MemorySpan struct {
	*Span
}

// StartMemoryIndex starts a span over the start-of-run memory index load.
//
// That load lists every stored memory and reads each value to recover its description,
// which on a network backend is a round trip per entry, and it happens inside setup with
// nothing else naming it. It is the first child startup has, which is why StartStartup's
// context is now threaded rather than dropped; the run loop is handed the root's context
// explicitly so it does not nest under a span that ends at the handoff.
func (p *Provider) StartMemoryIndex(ctx context.Context, i MemoryInfo) (context.Context, *MemorySpan) {
	if p == nil || p.tracer == nil {
		return ctx, &MemorySpan{}
	}

	ctx, span := p.tracer.Start(ctx, "memory_index",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(memoryAttrs(i)...),
	)

	return ctx, &MemorySpan{Span: &Span{span: span, started: time.Now(), provider: p}}
}

// Finish records how many memories the load returned and ends the span.
//
// The count is recorded only when the load succeeded, and that rule lives here rather
// than at the call site because the natural call-site version is an if an implementer
// can forget. A failed list returns no entries, so reporting the length regardless
// would put "0" on the span, and zero already means something else: the store is empty.
//
// The class is not the caller's to choose either. A store failure is the only domain
// outcome this span has, but cancellation reaches it through the same call, and on a
// network backend that is one round trip per entry, so it is the likeliest way for this
// span to fail at all. Classifying by domain first would report every Ctrl-C during
// setup as a broken memory store.
func (s *MemorySpan) Finish(err error, entries int) {
	if s == nil || s.Span == nil || s.Span.span == nil {
		return
	}

	if err == nil {
		s.Span.span.SetAttributes(AttrMemoryEntries.Int(entries))
	} else {
		class, ok := ClassifyContext(err)
		if !ok {
			class = ClassStore
		}
		s.Span.Fail(err, class)
	}

	s.Span.span.End()
}
