//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agenttest

import (
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/proto"
)

// OTLPReceiver is a collector for tests: it accepts OTLP/HTTP exports, decodes them
// and keeps what arrived.
//
// It exists because the in-memory exporter the other telemetry specs use shows what was
// recorded, not what was exported, and the difference is where this area's defects have
// lived. Every one found so far was invisible to a recorded-span assertion: histograms
// carrying the SDK's default bucket boundaries, a content document that arrived as a
// truncation marker with none of the conversation in it, and a text part with no text.
// Each was found by reading a decoded payload.
//
// Nothing here needs a collector binary, a credential or a network: the server is an
// httptest one on a port the kernel chooses, so these run in the ordinary suite and in
// parallel with everything else. What it does not do is enforce a real collector's own
// attribute limits, request size limits or view rendering, so it narrows the manual
// check against a real backend rather than replacing it.
type OTLPReceiver struct {
	server *httptest.Server

	mu       sync.Mutex
	spans    []OTLPSpan
	metrics  []OTLPMetric
	requests []OTLPRequest
}

// OTLPSpan is one span as it arrived.
type OTLPSpan struct {
	Name string
	// StatusMessage is the exported status description. It is here so a spec can assert
	// it stays empty: this tree's errors embed absolute paths and config values, and the
	// status crosses to a backend where it cannot be un-sent.
	StatusMessage string
	Attributes    map[string]any

	// TraceID, SpanID and ParentSpanID are the identifiers as hex, so a spec can assert
	// the shape of a trace rather than only its contents.
	//
	// Nesting is not a detail a recorded-span assertion can reach and it is not
	// cosmetic: a child that arrives parented to nothing becomes a second root, which a
	// backend renders as an unrelated trace, and the operator reading it concludes the
	// instrumentation dropped spans. Context threading is exactly the kind of thing that
	// breaks silently when a call site assigns the wrong variable.
	TraceID      string
	SpanID       string
	ParentSpanID string
}

// ChildrenOf returns every span parented to the given one, in arrival order.
func (r *OTLPReceiver) ChildrenOf(parent OTLPSpan) []OTLPSpan {
	r.mu.Lock()
	defer r.mu.Unlock()

	var out []OTLPSpan
	for _, s := range r.spans {
		if s.ParentSpanID != "" && s.ParentSpanID == parent.SpanID {
			out = append(out, s)
		}
	}

	return out
}

// OTLPMetric is one instrument as it arrived. Bounds is the histogram's explicit bucket
// boundaries, which is the field worth asserting: a histogram whose values are all
// correct still answers no question if its buckets are shaped for a different unit.
type OTLPMetric struct {
	Name       string
	Histogram  bool
	Bounds     []float64
	Count      uint64
	Sum        float64
	IntValue   int64
	Attributes map[string]any
}

// OTLPRequest is one export request's shape, for the properties that only exist on the
// wire: how large it was, and whether it was compressed.
type OTLPRequest struct {
	Signal       string
	WireBytes    int
	DecodedBytes int
	Encoding     string
	Spans        int
}

// NewOTLPReceiver starts a receiver and stops it when the test ends.
func NewOTLPReceiver(tb testing.TB) *OTLPReceiver {
	tb.Helper()

	r := BuildOTLPReceiver()
	tb.Cleanup(r.Close)

	return r
}

// BuildOTLPReceiver is NewOTLPReceiver without a testing.TB, for a func Example or any
// other caller outside a test. The caller calls Close where NewOTLPReceiver registers it
// as the test's cleanup.
func BuildOTLPReceiver() *OTLPReceiver {
	r := &OTLPReceiver{}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/traces", r.handleTraces)
	mux.HandleFunc("/v1/metrics", r.handleMetrics)

	r.server = httptest.NewServer(mux)

	return r
}

// Close stops the receiver's server and waits for the requests it is still serving.
func (r *OTLPReceiver) Close() {
	r.server.Close()
}

// Endpoint is the base URL to configure as telemetry.endpoint.
func (r *OTLPReceiver) Endpoint() string {
	return r.server.URL
}

// Spans returns every span that arrived. Call it after the provider has been shut down,
// which flushes synchronously; before that a batch may still be queued.
func (r *OTLPReceiver) Spans() []OTLPSpan {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]OTLPSpan(nil), r.spans...)
}

// Span returns the one span with the given name prefix, or fails the test. Selecting by
// name rather than position keeps a spec from breaking each time a run learns to emit
// another span.
func (r *OTLPReceiver) Span(tb testing.TB, prefix string) OTLPSpan {
	tb.Helper()

	span, err := r.FindSpan(prefix)
	if err != nil {
		tb.Fatalf("%v", err)
	}

	return span
}

// FindSpan is Span without a testing.TB, for a func Example or any other caller outside
// a test. No span matching the prefix is an error listing the spans that did arrive.
func (r *OTLPReceiver) FindSpan(prefix string) (OTLPSpan, error) {
	var found []OTLPSpan
	var names []string
	for _, s := range r.Spans() {
		names = append(names, s.Name)
		if len(s.Name) >= len(prefix) && s.Name[:len(prefix)] == prefix {
			found = append(found, s)
		}
	}

	if len(found) != 1 {
		return OTLPSpan{}, fmt.Errorf("expected exactly one %q span, got %d, received: %v", prefix, len(found), names)
	}

	return found[0], nil
}

// SpansNamed returns every span whose name starts with prefix, in arrival order.
func (r *OTLPReceiver) SpansNamed(prefix string) []OTLPSpan {
	var out []OTLPSpan
	for _, s := range r.Spans() {
		if len(s.Name) >= len(prefix) && s.Name[:len(prefix)] == prefix {
			out = append(out, s)
		}
	}

	return out
}

// Metrics returns every instrument that arrived.
func (r *OTLPReceiver) Metrics() []OTLPMetric {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]OTLPMetric(nil), r.metrics...)
}

// Metric returns the instrument with the given name, or fails the test.
func (r *OTLPReceiver) Metric(tb testing.TB, name string) OTLPMetric {
	tb.Helper()

	metric, err := r.FindMetric(name)
	if err != nil {
		tb.Fatalf("%v", err)
	}

	return metric
}

// FindMetric is Metric without a testing.TB, for a func Example or any other caller
// outside a test. No instrument of that name is an error listing the instruments that did
// arrive.
func (r *OTLPReceiver) FindMetric(name string) (OTLPMetric, error) {
	var names []string
	for _, m := range r.Metrics() {
		names = append(names, m.Name)
		if m.Name == name {
			return m, nil
		}
	}

	return OTLPMetric{}, fmt.Errorf("no %q instrument arrived, received: %v", name, names)
}

// Requests returns the shape of each export request.
func (r *OTLPReceiver) Requests() []OTLPRequest {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]OTLPRequest(nil), r.requests...)
}

// String returns a string attribute and whether it was present.
func (s OTLPSpan) String(key string) (string, bool) {
	v, ok := s.Attributes[key].(string)
	return v, ok
}

// Int returns an integer attribute and whether it was present.
func (s OTLPSpan) Int(key string) (int64, bool) {
	v, ok := s.Attributes[key].(int64)
	return v, ok
}

// Has reports whether an attribute was present at all, which is the assertion most of
// this area's rules are stated in: absent rather than zero, absent rather than empty.
func (s OTLPSpan) Has(key string) bool {
	_, ok := s.Attributes[key]
	return ok
}

// read decodes a request body, reporting the wire size and any compression.
func (r *OTLPReceiver) read(w http.ResponseWriter, req *http.Request) ([]byte, int, string, bool) {
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return nil, 0, "", false
	}

	wire := len(raw)
	encoding := req.Header.Get("Content-Encoding")

	if encoding == "gzip" {
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return nil, 0, "", false
		}
		raw, err = io.ReadAll(zr)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return nil, 0, "", false
		}
	}

	return raw, wire, encoding, true
}

func (r *OTLPReceiver) handleTraces(w http.ResponseWriter, req *http.Request) {
	raw, wire, encoding, ok := r.read(w, req)
	if !ok {
		return
	}

	var msg coltracepb.ExportTraceServiceRequest
	err := proto.Unmarshal(raw, &msg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var spans []OTLPSpan
	for _, rs := range msg.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			for _, s := range ss.Spans {
				spans = append(spans, OTLPSpan{
					Name:          s.Name,
					StatusMessage: s.Status.GetMessage(),
					Attributes:    attrMap(s.Attributes),
					TraceID:       hex.EncodeToString(s.TraceId),
					SpanID:        hex.EncodeToString(s.SpanId),
					ParentSpanID:  hex.EncodeToString(s.ParentSpanId),
				})
			}
		}
	}

	r.mu.Lock()
	r.spans = append(r.spans, spans...)
	r.requests = append(r.requests, OTLPRequest{
		Signal: "traces", WireBytes: wire, DecodedBytes: len(raw), Encoding: encoding, Spans: len(spans),
	})
	r.mu.Unlock()

	respond(w, &coltracepb.ExportTraceServiceResponse{})
}

func (r *OTLPReceiver) handleMetrics(w http.ResponseWriter, req *http.Request) {
	raw, wire, encoding, ok := r.read(w, req)
	if !ok {
		return
	}

	var msg colmetricspb.ExportMetricsServiceRequest
	err := proto.Unmarshal(raw, &msg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var out []OTLPMetric
	for _, rm := range msg.ResourceMetrics {
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				out = append(out, decodeMetric(m)...)
			}
		}
	}

	r.mu.Lock()
	r.metrics = append(r.metrics, out...)
	r.requests = append(r.requests, OTLPRequest{
		Signal: "metrics", WireBytes: wire, DecodedBytes: len(raw), Encoding: encoding,
	})
	r.mu.Unlock()

	respond(w, &colmetricspb.ExportMetricsServiceResponse{})
}

// decodeMetric flattens one instrument's data points.
func decodeMetric(m *metricspb.Metric) []OTLPMetric {
	var out []OTLPMetric

	switch d := m.Data.(type) {
	case *metricspb.Metric_Histogram:
		for _, dp := range d.Histogram.DataPoints {
			out = append(out, OTLPMetric{
				Name:       m.Name,
				Histogram:  true,
				Bounds:     dp.ExplicitBounds,
				Count:      dp.Count,
				Sum:        dp.GetSum(),
				Attributes: attrMap(dp.Attributes),
			})
		}
	case *metricspb.Metric_Sum:
		for _, dp := range d.Sum.DataPoints {
			out = append(out, OTLPMetric{
				Name:       m.Name,
				IntValue:   dp.GetAsInt(),
				Attributes: attrMap(dp.Attributes),
			})
		}
	}

	return out
}

// respond writes an empty success reply, which is what an exporter reads as accepted.
func respond(w http.ResponseWriter, msg proto.Message) {
	body, err := proto.Marshal(msg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/x-protobuf")
	_, _ = w.Write(body)
}

// attrMap converts exported attributes to plain Go values, so a spec asserts against
// the language rather than against protobuf wrappers.
func attrMap(attrs []*commonpb.KeyValue) map[string]any {
	out := make(map[string]any, len(attrs))
	for _, kv := range attrs {
		out[kv.Key] = attrValue(kv.Value)
	}

	return out
}

func attrValue(v *commonpb.AnyValue) any {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_IntValue:
		return x.IntValue
	case *commonpb.AnyValue_BoolValue:
		return x.BoolValue
	case *commonpb.AnyValue_DoubleValue:
		return x.DoubleValue
	case *commonpb.AnyValue_ArrayValue:
		out := make([]any, 0, len(x.ArrayValue.Values))
		for _, e := range x.ArrayValue.Values {
			out = append(out, attrValue(e))
		}
		return out
	}

	return nil
}
