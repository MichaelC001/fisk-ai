//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/semconv/v1.41.0/genaiconv"
)

// instruments are the run's metric instruments, created once at setup.
//
// Every attribute set below is named explicitly and is deliberately low cardinality. A
// duration histogram without the tool name cannot answer "which tool is slow", so the
// useful dimensions are here; the conversation id, the session id, the tool call id,
// the model's requested tool name and anything derived from content are NOT, and must
// never be added. An unbounded metric label costs the operator real money and cannot be
// un-sent once it has left.
type instruments struct {
	// The two the semantic conventions ship as generated helpers. Their Record methods
	// take the required attributes positionally, so the attribute set cannot drift.
	tokenUsage        genaiconv.ClientTokenUsage
	operationDuration genaiconv.ClientOperationDuration

	// The four in the GenAI metrics registry that postdate the semconv package shipped
	// with the Go SDK, so they are built by hand from the names in attrs.go.
	invokeAgentDuration       metric.Float64Histogram
	invokeAgentInferenceCalls metric.Int64Histogram
	invokeAgentToolCalls      metric.Int64Histogram
	executeToolDuration       metric.Float64Histogram

	// The two fisk-named instruments. See MetricKnowledgeDegradedSearches for why that
	// state needs a metric when everything else here is content with a span attribute,
	// and MetricSessionAppendDuration for the operation that is too frequent to be one.
	knowledgeDegradedSearches metric.Int64Counter
	sessionAppendDuration     metric.Float64Histogram
}

// The bucket boundaries the GenAI metrics registry advises for each instrument.
//
// Every one of these must be passed explicitly, including to the two genaiconv helpers,
// which supply only a unit and a description. Without them the SDK applies its default
// boundaries, which top out at 10000 and are shaped for latency in milliseconds. Nothing
// here is in milliseconds: three of these are seconds and one is tokens, so the default
// layout puts every real value in the first two buckets and sends anything past 10000
// tokens to +Inf. That is a defect no attribute assertion can see, and it was found only
// by reading a real collector's decoded output: the delivery count, the attribute sets
// and the specs were all correct while every percentile built on them was meaningless.
var (
	// Seconds, for gen_ai.client.operation.duration and gen_ai.execute_tool.duration.
	durationBuckets = []float64{0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12, 10.24, 20.48, 40.96, 81.92}
	// Seconds, for gen_ai.invoke_agent.duration. Coarser than durationBuckets because
	// an agent invocation is many model calls and, when interactive, operator think
	// time between them.
	agentDurationBuckets = []float64{0.1, 0.2, 0.4, 0.8, 1.6, 3.2, 6.4, 12.8, 25.6, 51.2, 102.4, 204.8, 409.6}
	// Tokens, for gen_ai.client.token.usage.
	tokenBuckets = []float64{1, 4, 16, 64, 256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216, 67108864}
	// Counts, for gen_ai.invoke_agent.inference_calls and .tool_calls.
	callCountBuckets = []float64{1, 2, 4, 8, 16, 32, 64, 128}
	// Seconds, for fisk.session.append.duration. Two orders of magnitude finer at the
	// bottom than durationBuckets, which starts at 10ms: an append to the file backend
	// is a local write measured in microseconds, so every value would land in the first
	// bucket and the instrument would report only that journaling is fast, never how
	// fast or when it stopped being. The top end still reaches seconds, because a
	// jetstream append is a network round trip that can stall.
	appendDurationBuckets = []float64{0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5}
)

// newInstruments creates the run's instruments. A nil meter yields a zero value whose
// every method is a no-op, which is the path a traces-only run takes.
func newInstruments(meter metric.Meter) (*instruments, error) {
	if meter == nil {
		return &instruments{}, nil
	}

	i := &instruments{}

	// The generated constructors take caller options and append their own afterwards,
	// and their own are a unit and a description only, so the boundaries survive.
	var err error
	i.tokenUsage, err = genaiconv.NewClientTokenUsage(meter,
		metric.WithExplicitBucketBoundaries(tokenBuckets...),
	)
	if err != nil {
		return nil, fmt.Errorf("gen_ai.client.token.usage: %w", err)
	}

	i.operationDuration, err = genaiconv.NewClientOperationDuration(meter,
		metric.WithExplicitBucketBoundaries(durationBuckets...),
	)
	if err != nil {
		return nil, fmt.Errorf("gen_ai.client.operation.duration: %w", err)
	}

	i.invokeAgentDuration, err = meter.Float64Histogram(MetricInvokeAgentDuration,
		metric.WithUnit("s"),
		metric.WithDescription("Duration of a GenAI agent invocation."),
		metric.WithExplicitBucketBoundaries(agentDurationBuckets...),
	)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", MetricInvokeAgentDuration, err)
	}

	i.invokeAgentInferenceCalls, err = meter.Int64Histogram(MetricInvokeAgentInferenceCalls,
		metric.WithUnit("{inference_call}"),
		metric.WithDescription("Model calls made during one GenAI agent invocation."),
		metric.WithExplicitBucketBoundaries(callCountBuckets...),
	)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", MetricInvokeAgentInferenceCalls, err)
	}

	i.invokeAgentToolCalls, err = meter.Int64Histogram(MetricInvokeAgentToolCalls,
		metric.WithUnit("{tool_call}"),
		metric.WithDescription("Tool calls made during one GenAI agent invocation."),
		metric.WithExplicitBucketBoundaries(callCountBuckets...),
	)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", MetricInvokeAgentToolCalls, err)
	}

	i.executeToolDuration, err = meter.Float64Histogram(MetricExecuteToolDuration,
		metric.WithUnit("s"),
		metric.WithDescription("Duration of a GenAI tool execution."),
		metric.WithExplicitBucketBoundaries(durationBuckets...),
	)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", MetricExecuteToolDuration, err)
	}

	// A counter takes no bucket boundaries, so the defect that made every histogram above
	// declare its own does not apply here.
	i.knowledgeDegradedSearches, err = meter.Int64Counter(MetricKnowledgeDegradedSearches,
		metric.WithUnit("{search}"),
		metric.WithDescription("Knowledge searches that fell back from the vector tier to lexical."),
	)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", MetricKnowledgeDegradedSearches, err)
	}

	i.sessionAppendDuration, err = meter.Float64Histogram(MetricSessionAppendDuration,
		metric.WithUnit("s"),
		metric.WithDescription("Duration of one append to the run journal."),
		metric.WithExplicitBucketBoundaries(appendDurationBuckets...),
	)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", MetricSessionAppendDuration, err)
	}

	return i, nil
}

// TurnMetrics is one agent invocation's worth of counters.
type TurnMetrics struct {
	AgentName      string
	TerminalReason TerminalReason
	Interactive    bool
	Duration       time.Duration
	InferenceCalls int64
	ToolCalls      int64
}

// RecordTurn records the agent-invocation metrics for one turn.
//
// It is called at the turn boundary in BOTH modes, treating a one-shot run as a single
// turn, which is why it is a method here rather than something the turn span does: that
// span exists only for an interactive run. Recording at the root for one-shot runs and
// at the turn for chats would mix "one turn" with "a whole session including operator
// think time" into one series, and a p95 over that means nothing.
//
// The counts must be per-turn deltas. Reading them from the run's running totals would
// be wrong on a resume, where those totals arrive pre-seeded with the whole session's
// history and the first turn would report every call the session ever made.
func (p *Provider) RecordTurn(ctx context.Context, m TurnMetrics) {
	if p == nil || p.instruments == nil {
		return
	}

	attrs := attribute.NewSet(
		semconv.GenAIAgentName(m.AgentName),
		AttrRunTerminalReason.String(m.TerminalReason.String()),
		AttrRunInteractive.Bool(m.Interactive),
	)
	set := metric.WithAttributeSet(attrs)

	p.instruments.invokeAgentDuration.Record(ctx, m.Duration.Seconds(), set)
	p.instruments.invokeAgentInferenceCalls.Record(ctx, m.InferenceCalls, set)
	p.instruments.invokeAgentToolCalls.Record(ctx, m.ToolCalls, set)
}

// recordChat records the per-call metrics when a chat span ends.
//
// gen_ai.token.type carries only the spec's input and output, never the cache tiers and
// never reasoning. That is what lets the histogram be summed without grouping and still
// be right: a cache_read series would double count against input, and a reasoning series
// would double count against output, since each is already part of the total it sits
// under. Both stay span attributes, where they answer a different question.
func (p *Provider) recordChat(ctx context.Context, i ChatInfo, o ChatOutcome, d time.Duration) {
	if p == nil || p.instruments == nil {
		return
	}

	operation := genaiconv.OperationNameChat
	provider := genaiconv.ProviderNameAttr(i.Provider)

	// The optional-attribute helpers are methods on the instrument that accepts them,
	// so the model attribute cannot be attached to an instrument the conventions do not
	// define it for.
	durationAttrs := []attribute.KeyValue{p.instruments.operationDuration.AttrRequestModel(i.Model)}
	if o.Failed {
		durationAttrs = append(durationAttrs, errorType(o.Class))
	}
	p.instruments.operationDuration.Record(ctx, d.Seconds(), operation, provider, durationAttrs...)

	model := p.instruments.tokenUsage.AttrRequestModel(i.Model)
	if o.Usage.Input > 0 {
		p.instruments.tokenUsage.Record(ctx, o.Usage.Input, operation, provider, genaiconv.TokenTypeInput, model)
	}
	if o.Usage.Output > 0 {
		p.instruments.tokenUsage.Record(ctx, o.Usage.Output, operation, provider, genaiconv.TokenTypeOutput, model)
	}
}

// recordDegradedSearch counts one search that fell back to lexical, when its span ends.
//
// The degrade reason is the only attribute, and it is a five-value closed vocabulary, so
// this instrument's cardinality is fixed at compile time. Nothing about which agent,
// which corpus or which query is on it: those are span attributes, and this exists
// precisely for the case where the span was not sampled.
func (p *Provider) recordDegradedSearch(ctx context.Context, reason DegradeReason) {
	if p == nil || p.instruments == nil {
		return
	}

	p.instruments.knowledgeDegradedSearches.Add(ctx, 1,
		metric.WithAttributes(AttrKnowledgeDegradedReason.String(reason.String())))
}

// RecordSessionAppend records how long one journal append took, on the backend that
// served it. A failed append is recorded too, with its error class, since the time an
// append spent before failing is exactly what an operator chasing a stalled journal
// wants and discarding it would make an outage look like an absence of traffic.
//
// It is exported and takes a duration the caller measured, unlike the other instruments
// here, which are recorded by the span kind that owns them. There is no span to own this
// one by design: see MetricSessionAppendDuration. Measuring from outside also means
// runstate needs no context threaded through Store and Journal to be observable at all.
//
// An unset class means the append succeeded, which is why there is no separate bool: a
// caller declares an ErrorClass, sets it only on the failure path, and passes it either
// way. The alternative signature would have every success write telemetry.ErrorClass{}
// at the call site, which is the one value this closed type can be constructed with from
// outside and so the one habit worth not teaching.
func (p *Provider) RecordSessionAppend(ctx context.Context, backend string, d time.Duration, class ErrorClass) {
	if p == nil || p.instruments == nil {
		return
	}

	attrs := []attribute.KeyValue{AttrSessionBackend.String(backend)}
	if class.Set() {
		attrs = append(attrs, errorType(class))
	}

	p.instruments.sessionAppendDuration.Record(ctx, d.Seconds(), metric.WithAttributes(attrs...))
}

// recordTool records the tool duration when an execute_tool span ends.
//
// gen_ai.tool.name is the registry-validated name and is absent for an unknown tool,
// whose name came from the model. That absence is deliberate: a model-invented name on
// a metric label is unbounded cardinality, and fisk.tool.outcome already separates
// those calls out as unknown_tool.
func (p *Provider) recordTool(ctx context.Context, o ToolOutcome, d time.Duration) {
	if p == nil || p.instruments == nil {
		return
	}

	attrs := []attribute.KeyValue{
		AttrToolKind.String(o.Kind.String()),
		AttrToolOutcome.String(o.Outcome.String()),
	}
	if o.Name != "" {
		attrs = append(attrs, semconv.GenAIToolName(o.Name))
	}
	if o.Failed {
		attrs = append(attrs, errorType(ClassToolError))
	}

	p.instruments.executeToolDuration.Record(ctx, d.Seconds(), metric.WithAttributes(attrs...))
}
