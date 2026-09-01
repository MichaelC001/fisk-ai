//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// collected reads every recorded metric back out of a manual reader.
func collected(reader *sdkmetric.ManualReader) metricdata.ResourceMetrics {
	GinkgoHelper()

	var rm metricdata.ResourceMetrics
	Expect(reader.Collect(context.Background(), &rm)).To(Succeed())

	return rm
}

// metricNamed finds one recorded instrument by name.
func metricNamed(rm metricdata.ResourceMetrics, name string) (metricdata.Metrics, bool) {
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name == name {
				return m, true
			}
		}
	}

	return metricdata.Metrics{}, false
}

// attrSets returns the attribute set of every data point on a histogram, whichever
// numeric type it carries.
func attrSets(m metricdata.Metrics) []attribute.Set {
	var out []attribute.Set

	switch h := m.Data.(type) {
	case metricdata.Histogram[float64]:
		for _, dp := range h.DataPoints {
			out = append(out, dp.Attributes)
		}
	case metricdata.Histogram[int64]:
		for _, dp := range h.DataPoints {
			out = append(out, dp.Attributes)
		}
	}

	return out
}

// boundsOf returns the bucket boundaries of a histogram's first data point.
func boundsOf(m metricdata.Metrics) []float64 {
	GinkgoHelper()

	switch h := m.Data.(type) {
	case metricdata.Histogram[float64]:
		Expect(h.DataPoints).ToNot(BeEmpty())
		return h.DataPoints[0].Bounds
	case metricdata.Histogram[int64]:
		Expect(h.DataPoints).ToNot(BeEmpty())
		return h.DataPoints[0].Bounds
	}

	Fail("not a histogram")

	return nil
}

// hasAttr reports whether a set carries key with value.
func hasAttr(set attribute.Set, key attribute.Key, value string) bool {
	v, ok := set.Value(key)

	return ok && v.AsString() == value
}

// recordingMetrics returns a Provider whose metrics land in the returned reader.
func recordingMetrics() (*Provider, *sdkmetric.ManualReader) {
	reader := sdkmetric.NewManualReader()

	return NewFromProviders(nil, sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))), reader
}

var _ = Describe("Metrics", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	// Every one of these shipped with the SDK's default boundaries, which top out at
	// 10000 and are shaped for latency in milliseconds. Three of these instruments are
	// in seconds and one is in tokens, so against a real backend every value landed in
	// the first two buckets and any run past 10000 tokens fell into +Inf: correct
	// counts, correct attributes, meaningless percentiles. Nothing about an attribute
	// set can see that, which is why the boundaries are asserted exactly rather than
	// merely asserted present.
	Describe("bucket boundaries", func() {
		It("should use the boundaries the GenAI metrics registry advises", func() {
			p, reader := recordingMetrics()

			p.recordChat(ctx, ChatInfo{Model: "claude-sonnet-5", Provider: "anthropic"},
				ChatOutcome{Usage: TokenUsage{Input: 152, Output: 8}}, time.Second)
			p.RecordTurn(ctx, TurnMetrics{
				AgentName:      "demo",
				TerminalReason: TerminalCompleted,
				Duration:       time.Second,
				InferenceCalls: 1,
				ToolCalls:      1,
			})
			p.recordTool(ctx, ToolOutcome{Outcome: ToolOutcomeExecuted, Name: "do"}, time.Second)
			p.RecordSessionAppend(ctx, "file", time.Millisecond, ErrorClass{})

			rm := collected(reader)
			for name, want := range map[string][]float64{
				"gen_ai.client.token.usage":        tokenBuckets,
				"gen_ai.client.operation.duration": durationBuckets,
				MetricInvokeAgentDuration:          agentDurationBuckets,
				MetricInvokeAgentInferenceCalls:    callCountBuckets,
				MetricInvokeAgentToolCalls:         callCountBuckets,
				MetricExecuteToolDuration:          durationBuckets,
				MetricSessionAppendDuration:        appendDurationBuckets,
			} {
				m, ok := metricNamed(rm, name)
				Expect(ok).To(BeTrue(), "expected %s to be recorded", name)
				Expect(boundsOf(m)).To(Equal(want), "wrong bucket boundaries on %s", name)
			}
		})
	})

	Describe("RecordTurn", func() {
		It("should record the three agent instruments under one attribute set", func() {
			p, reader := recordingMetrics()

			p.RecordTurn(ctx, TurnMetrics{
				AgentName:      "demo",
				TerminalReason: TerminalCompleted,
				Interactive:    true,
				Duration:       250 * time.Millisecond,
				InferenceCalls: 3,
				ToolCalls:      2,
			})

			rm := collected(reader)
			for _, name := range []string{
				MetricInvokeAgentDuration,
				MetricInvokeAgentInferenceCalls,
				MetricInvokeAgentToolCalls,
			} {
				m, ok := metricNamed(rm, name)
				Expect(ok).To(BeTrue(), "expected %s to be recorded", name)

				sets := attrSets(m)
				Expect(sets).To(HaveLen(1))
				Expect(hasAttr(sets[0], "gen_ai.agent.name", "demo")).To(BeTrue())
				Expect(hasAttr(sets[0], AttrRunTerminalReason, TerminalCompleted.String())).To(BeTrue())
			}
		})

		It("should do nothing on a Provider with no metric pipeline", func() {
			p := NewFromProviders(nil, nil)

			Expect(func() { p.RecordTurn(ctx, TurnMetrics{AgentName: "demo"}) }).ToNot(Panic())
		})
	})

	Describe("the chat instruments", func() {
		// gen_ai.token.type carries only input and output. Adding a series for the cache
		// tiers would double count, because input already includes them by spec, and the
		// histogram is meant to be summable without grouping.
		It("should split token usage into input and output only", func() {
			p, reader := recordingMetrics()

			info := ChatInfo{Model: "claude-sonnet-5", Provider: "anthropic"}
			p.recordChat(ctx, info, ChatOutcome{
				Usage: TokenUsage{Input: 152, Output: 8, CacheRead: 40, CacheCreate: 12},
			}, 100*time.Millisecond)

			m, ok := metricNamed(collected(reader), "gen_ai.client.token.usage")
			Expect(ok).To(BeTrue())

			sets := attrSets(m)
			Expect(sets).To(HaveLen(2))

			var types []string
			for _, set := range sets {
				v, ok := set.Value("gen_ai.token.type")
				Expect(ok).To(BeTrue())
				types = append(types, v.AsString())
			}
			Expect(types).To(ConsistOf("input", "output"))
		})

		It("should label a failed call with its error class", func() {
			p, reader := recordingMetrics()

			info := ChatInfo{Model: "claude-sonnet-5", Provider: "anthropic"}
			p.recordChat(ctx, info, ChatOutcome{Failed: true, Class: ClassTruncated}, time.Second)

			m, ok := metricNamed(collected(reader), "gen_ai.client.operation.duration")
			Expect(ok).To(BeTrue())

			sets := attrSets(m)
			Expect(sets).To(HaveLen(1))
			Expect(hasAttr(sets[0], "error.type", ClassTruncated.String())).To(BeTrue())
			Expect(hasAttr(sets[0], "gen_ai.request.model", "claude-sonnet-5")).To(BeTrue())
		})
	})

	Describe("the execute_tool instrument", func() {
		It("should carry the tool name, kind and outcome", func() {
			p, reader := recordingMetrics()

			p.recordTool(ctx, ToolOutcome{
				Outcome: ToolOutcomeExecuted,
				Name:    "do",
				Kind:    ToolKindApplication,
			}, 30*time.Millisecond)

			m, ok := metricNamed(collected(reader), MetricExecuteToolDuration)
			Expect(ok).To(BeTrue())

			sets := attrSets(m)
			Expect(sets).To(HaveLen(1))
			Expect(hasAttr(sets[0], "gen_ai.tool.name", "do")).To(BeTrue())
			Expect(hasAttr(sets[0], AttrToolKind, ToolKindApplication.String())).To(BeTrue())
			Expect(hasAttr(sets[0], AttrToolOutcome, ToolOutcomeExecuted.String())).To(BeTrue())
		})

		// The cardinality guard, on the surface where it costs money. A tool name the
		// model invented must never become a metric label: every hallucination would mint
		// a new series, and a backend bills for those and cannot un-see them.
		// fisk.tool.outcome already separates these calls out as unknown_tool.
		It("should omit the tool name for a tool the model invented", func() {
			p, reader := recordingMetrics()

			p.recordTool(ctx, ToolOutcome{Outcome: ToolOutcomeUnknownTool}, time.Millisecond)

			m, ok := metricNamed(collected(reader), MetricExecuteToolDuration)
			Expect(ok).To(BeTrue())

			sets := attrSets(m)
			Expect(sets).To(HaveLen(1))

			_, present := sets[0].Value("gen_ai.tool.name")
			Expect(present).To(BeFalse())
			_, present = sets[0].Value(AttrToolRequestedName)
			Expect(present).To(BeFalse())
			Expect(hasAttr(sets[0], AttrToolOutcome, ToolOutcomeUnknownTool.String())).To(BeTrue())
		})
	})
})

// The journal append instrument. It exists instead of a span because an append is
// uniform and frequent: a checkpointed run makes on the order of a hundred, and a span
// each would double the run's span count to describe a local write.
var _ = Describe("RecordSessionAppend", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("should record the backend and no error class for a successful append", func() {
		p, reader := recordingMetrics()

		p.RecordSessionAppend(ctx, "jetstream", 3*time.Millisecond, ErrorClass{})

		m, ok := metricNamed(collected(reader), MetricSessionAppendDuration)
		Expect(ok).To(BeTrue())

		sets := attrSets(m)
		Expect(sets).To(HaveLen(1))
		Expect(hasAttr(sets[0], AttrSessionBackend, "jetstream")).To(BeTrue())
		Expect(sets[0].HasValue("error.type")).To(BeFalse())
	})

	// A failed append's duration is the time it spent before failing, which is what an
	// operator chasing a stalled journal is looking for. Dropping it would make an
	// outage read as an absence of traffic.
	It("should record a failed append with its error class", func() {
		p, reader := recordingMetrics()

		p.RecordSessionAppend(ctx, "jetstream", 2*time.Second, ClassStore)

		m, ok := metricNamed(collected(reader), MetricSessionAppendDuration)
		Expect(ok).To(BeTrue())

		sets := attrSets(m)
		Expect(sets).To(HaveLen(1))
		Expect(hasAttr(sets[0], "error.type", ClassStore.String())).To(BeTrue())
	})

	// Success and failure have to stay separable, or a backend that fails slowly is
	// averaged into one that succeeds quickly and neither is visible.
	It("should keep successful and failed appends in separate series", func() {
		p, reader := recordingMetrics()

		p.RecordSessionAppend(ctx, "file", time.Millisecond, ErrorClass{})
		p.RecordSessionAppend(ctx, "file", time.Millisecond, ClassStore)

		m, ok := metricNamed(collected(reader), MetricSessionAppendDuration)
		Expect(ok).To(BeTrue())
		Expect(attrSets(m)).To(HaveLen(2))
	})

	It("should be safe on a Provider that records nothing", func() {
		var p *Provider

		Expect(func() { p.RecordSessionAppend(ctx, "file", time.Millisecond, ErrorClass{}) }).ToNot(Panic())
	})
})
