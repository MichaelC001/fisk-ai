//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agenttest_test

import (
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"net/http"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/choria-io/fisk-ai/internal/agenttest"
)

var _ = Describe("OTLPReceiver", func() {
	var receiver *agenttest.OTLPReceiver

	BeforeEach(func() {
		receiver = agenttest.NewOTLPReceiver(GinkgoTB())
	})

	id := func(text string) []byte {
		GinkgoHelper()

		raw, err := hex.DecodeString(text)
		Expect(err).ToNot(HaveOccurred())

		return raw
	}

	stringAttr := func(key, value string) *commonpb.KeyValue {
		return &commonpb.KeyValue{
			Key:   key,
			Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}},
		}
	}

	intAttr := func(key string, value int64) *commonpb.KeyValue {
		return &commonpb.KeyValue{
			Key:   key,
			Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: value}},
		}
	}

	// export posts a request the way an OTLP/HTTP exporter does, gzipping the body when
	// asked so the receiver's wire and decoded sizes differ.
	export := func(path string, msg proto.Message, compress bool) {
		GinkgoHelper()

		body, err := proto.Marshal(msg)
		Expect(err).ToNot(HaveOccurred())

		var payload bytes.Buffer
		encoding := ""
		if compress {
			zw := gzip.NewWriter(&payload)
			_, err = zw.Write(body)
			Expect(err).ToNot(HaveOccurred())
			Expect(zw.Close()).To(Succeed())
			encoding = "gzip"
		} else {
			payload.Write(body)
		}

		req, err := http.NewRequest(http.MethodPost, receiver.Endpoint()+path, &payload)
		Expect(err).ToNot(HaveOccurred())
		req.Header.Set("Content-Type", "application/x-protobuf")
		if encoding != "" {
			req.Header.Set("Content-Encoding", encoding)
		}

		resp, err := http.DefaultClient.Do(req)
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(resp.Body.Close)
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
	}

	exportSpans := func(spans []*tracepb.Span, compress bool) {
		GinkgoHelper()

		export("/v1/traces", &coltracepb.ExportTraceServiceRequest{
			ResourceSpans: []*tracepb.ResourceSpans{{
				ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}},
			}},
		}, compress)
	}

	exportMetrics := func(metrics []*metricspb.Metric) {
		GinkgoHelper()

		export("/v1/metrics", &colmetricspb.ExportMetricsServiceRequest{
			ResourceMetrics: []*metricspb.ResourceMetrics{{
				ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: metrics}},
			}},
		}, false)
	}

	It("Should decode the spans that arrived", func() {
		exportSpans([]*tracepb.Span{{
			Name:       "agent.run",
			TraceId:    id("0102030405060708090a0b0c0d0e0f10"),
			SpanId:     id("1112131415161718"),
			Status:     &tracepb.Status{Message: "it failed"},
			Attributes: []*commonpb.KeyValue{stringAttr("agent.name", "test"), intAttr("agent.tools", 3)},
		}}, false)

		spans := receiver.Spans()
		Expect(spans).To(HaveLen(1))
		Expect(spans[0].Name).To(Equal("agent.run"))
		Expect(spans[0].StatusMessage).To(Equal("it failed"))
		Expect(spans[0].TraceID).To(Equal("0102030405060708090a0b0c0d0e0f10"))
		Expect(spans[0].SpanID).To(Equal("1112131415161718"))
		Expect(spans[0].ParentSpanID).To(BeEmpty())

		name, ok := spans[0].String("agent.name")
		Expect(ok).To(BeTrue())
		Expect(name).To(Equal("test"))

		tools, ok := spans[0].Int("agent.tools")
		Expect(ok).To(BeTrue())
		Expect(tools).To(Equal(int64(3)))

		Expect(spans[0].Has("agent.name")).To(BeTrue())
		Expect(spans[0].Has("agent.missing")).To(BeFalse())

		_, ok = spans[0].String("agent.tools")
		Expect(ok).To(BeFalse(), "an attribute of another type is not a string")
	})

	It("Should report the shape of a trace", func() {
		parentID := "1112131415161718"
		exportSpans([]*tracepb.Span{
			{Name: "agent.run", TraceId: id("0102030405060708090a0b0c0d0e0f10"), SpanId: id(parentID)},
			{Name: "agent.tool", TraceId: id("0102030405060708090a0b0c0d0e0f10"),
				SpanId: id("2122232425262728"), ParentSpanId: id(parentID)},
			{Name: "unrelated", TraceId: id("aabbccddeeff00112233445566778899"), SpanId: id("3132333435363738")},
		}, false)

		parent := receiver.Span(GinkgoTB(), "agent.run")
		children := receiver.ChildrenOf(parent)
		Expect(children).To(HaveLen(1))
		Expect(children[0].Name).To(Equal("agent.tool"))

		Expect(receiver.ChildrenOf(children[0])).To(BeEmpty())
	})

	It("Should select a span by name prefix", func() {
		exportSpans([]*tracepb.Span{
			{Name: "agent.run", TraceId: id("0102030405060708090a0b0c0d0e0f10"), SpanId: id("1112131415161718")},
			{Name: "agent.tool.echo", TraceId: id("0102030405060708090a0b0c0d0e0f10"), SpanId: id("2122232425262728")},
			{Name: "agent.tool.list", TraceId: id("0102030405060708090a0b0c0d0e0f10"), SpanId: id("3132333435363738")},
		}, false)

		span, err := receiver.FindSpan("agent.run")
		Expect(err).ToNot(HaveOccurred())
		Expect(span.Name).To(Equal("agent.run"))

		_, err = receiver.FindSpan("agent.tool")
		Expect(err).To(MatchError(ContainSubstring(`expected exactly one "agent.tool" span, got 2`)))
		Expect(err).To(MatchError(ContainSubstring("agent.run")), "the error lists what did arrive")

		_, err = receiver.FindSpan("agent.memory")
		Expect(err).To(MatchError(ContainSubstring("got 0")))

		Expect(receiver.SpansNamed("agent.tool")).To(HaveLen(2))
		Expect(receiver.SpansNamed("agent.")).To(HaveLen(3))
		Expect(receiver.SpansNamed("nothing")).To(BeEmpty())
	})

	It("Should decode a histogram with the buckets it was exported with", func() {
		exportMetrics([]*metricspb.Metric{
			{
				Name: "agent.run.duration",
				Data: &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{
					DataPoints: []*metricspb.HistogramDataPoint{{
						ExplicitBounds: []float64{1, 5, 10},
						Count:          3,
						Sum:            proto.Float64(12.5),
						Attributes:     []*commonpb.KeyValue{stringAttr("agent.name", "test")},
					}},
				}},
			},
			{
				Name: "agent.run.tokens",
				Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
					DataPoints: []*metricspb.NumberDataPoint{{
						Value: &metricspb.NumberDataPoint_AsInt{AsInt: 400},
					}},
				}},
			},
		})

		histogram, err := receiver.FindMetric("agent.run.duration")
		Expect(err).ToNot(HaveOccurred())
		Expect(histogram.Histogram).To(BeTrue())
		Expect(histogram.Bounds).To(Equal([]float64{1, 5, 10}))
		Expect(histogram.Count).To(Equal(uint64(3)))
		Expect(histogram.Sum).To(Equal(12.5))
		Expect(histogram.Attributes).To(HaveKeyWithValue("agent.name", "test"))

		counter := receiver.Metric(GinkgoTB(), "agent.run.tokens")
		Expect(counter.Histogram).To(BeFalse())
		Expect(counter.IntValue).To(Equal(int64(400)))

		Expect(receiver.Metrics()).To(HaveLen(2))

		_, err = receiver.FindMetric("agent.run.missing")
		Expect(err).To(MatchError(ContainSubstring(`no "agent.run.missing" instrument arrived`)))
	})

	// How large a request was and whether it was compressed exist only on the wire, so a
	// recorded-span assertion cannot reach them.
	It("Should report each export request's size and encoding", func() {
		exportSpans([]*tracepb.Span{
			{Name: "agent.run", TraceId: id("0102030405060708090a0b0c0d0e0f10"), SpanId: id("1112131415161718")},
		}, true)
		exportMetrics(nil)

		requests := receiver.Requests()
		Expect(requests).To(HaveLen(2))

		Expect(requests[0].Signal).To(Equal("traces"))
		Expect(requests[0].Encoding).To(Equal("gzip"))
		Expect(requests[0].Spans).To(Equal(1))
		Expect(requests[0].WireBytes).ToNot(Equal(requests[0].DecodedBytes), "the body arrived compressed")

		Expect(requests[1].Signal).To(Equal("metrics"))
		Expect(requests[1].Encoding).To(BeEmpty())
	})

	It("Should refuse a body that is not an export request", func() {
		resp, err := http.Post(receiver.Endpoint()+"/v1/traces", "application/x-protobuf",
			bytes.NewReader([]byte{0xff, 0xff, 0xff, 0xff}))
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(resp.Body.Close)

		Expect(resp.StatusCode).To(Equal(http.StatusBadRequest))
		Expect(receiver.Spans()).To(BeEmpty())
	})

	It("Should stop answering once it is closed", func() {
		endpoint := agenttest.BuildOTLPReceiver()
		url := endpoint.Endpoint()
		Expect(url).To(HavePrefix("http://"))

		endpoint.Close()

		_, err := http.Post(url+"/v1/traces", "application/x-protobuf", bytes.NewReader(nil))
		Expect(err).To(HaveOccurred())
	})
})
