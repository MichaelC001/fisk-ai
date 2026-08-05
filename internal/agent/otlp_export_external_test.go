//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// These drive whole runs through the real OTLP/HTTP export path and assert on what a
// collector decodes, which is the one thing the rest of this suite cannot see: the
// in-memory exporter shows what was recorded, not what was encoded, compressed, batched
// and accepted.
//
// That distinction is not theoretical here. Every defect this area has produced was
// invisible to a recorded-span assertion and was found by reading a decoded payload:
// histograms carrying the SDK's millisecond-shaped default buckets, a content document
// that arrived as a truncation marker with none of the conversation in it, and a text
// part with no text in it. Each of those has a spec below.
//
// They need no collector binary and no API key: the receiver is an httptest server and
// the model is scripted.
package agent_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/telemetry"
	"github.com/choria-io/fisk-ai/internal/telemetry/bootstrap"
	"github.com/choria-io/fisk-ai/internal/util"
)

// sdkDefaultBuckets is the SDK's default histogram layout, which tops out at 10000 and
// is shaped for latency in milliseconds. Nothing this build records is in milliseconds,
// so a histogram arriving with these is one whose boundaries were never configured: the
// counts and labels are right and every percentile is meaningless. The exact boundaries
// are pinned in the telemetry package's own specs; what these assert is that whatever
// was configured survived to the wire.
var sdkDefaultBuckets = []float64{0, 5, 10, 25, 50, 75, 100, 250, 500, 750, 1000, 2500, 5000, 7500, 10000}

// exportRun drives one run through a real export pipeline into rx and flushes it.
//
// It goes through bootstrap.Start rather than telemetry.Setup deliberately. That is the
// path a program actually writes, so these become the end-to-end coverage of it: the
// config mapping, the ordering rules and the flush discipline are all exercised by every
// spec below rather than only by the bootstrap package's own unit specs.
func exportRun(t *testing.T, g *WithT, rx *agenttest.OTLPReceiver, capture config.TelemetryCaptureConfig, opts ...exportOption) {
	t.Helper()

	o := exportOptions{prompt: "summarize the widget inventory"}
	for _, fn := range opts {
		fn(&o)
	}

	app := agenttest.NewFakeApp(t, exampleApp())
	cfg := agenttest.Config(t, app, agenttest.WithMemory(), func(c *config.Config) {
		c.SystemPrompt = o.systemPrompt
		c.Telemetry.Enabled = true
		c.Telemetry.Endpoint = rx.Endpoint()
		if capture.Enabled {
			c.Telemetry.Capture = &capture
		}
	})

	tel, err := bootstrap.Start(context.Background(), bootstrap.Options{
		Config:  cfg,
		Version: util.Version(),
	})
	g.Expect(err).ToNot(HaveOccurred())

	provider := tel.Provider

	store := agenttest.NewFakeMemoryStore(t)
	g.Expect(store.Write(context.Background(), "deploy-notes", "how the deploy works", "body", false)).To(Succeed())

	responses := o.responses
	if len(responses) == 0 {
		responses = []*llm.Response{
			exportUsage(agenttest.ToolUseResponse("call_1", "do", []byte(`{"subject":"widgets"}`))),
			exportUsage(agenttest.TextResponse("all done")),
		}
	}

	_, err = agent.Run(context.Background(), agent.Options{
		Config:      cfg,
		ConfigFile:  "agent.yaml",
		Prompt:      []string{o.prompt},
		Provider:    agenttest.NewScriptedProvider(t, responses...),
		MemoryStore: store,
		Telemetry:   provider,
	}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).ToNot(HaveOccurred())

	// Shutdown flushes synchronously, so every span has reached the receiver by the time
	// it returns. Delivery being complete is itself an assertion: it is what says the
	// collector accepted the payload rather than refusing it whole.
	delivery, err := provider.Shutdown(context.Background())
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(delivery.Complete()).To(BeTrue(),
		"the receiver did not accept everything: %d of %d spans, err=%v",
		delivery.SpansDelivered, delivery.SpansAttempted, delivery.Err)
}

type exportOptions struct {
	prompt       string
	systemPrompt string
	responses    []*llm.Response
}

type exportOption func(*exportOptions)

func withPrompt(s string) exportOption       { return func(o *exportOptions) { o.prompt = s } }
func withSystemPrompt(s string) exportOption { return func(o *exportOptions) { o.systemPrompt = s } }

// exportUsage gives a scripted reply realistic token counts, since the token histogram
// is only recorded for a positive count and a reply reporting none records nothing.
func exportUsage(r *llm.Response) *llm.Response {
	r.Usage = llm.Usage{In: 1200, Out: 240, CacheRead: 4096}
	r.ID = "msg_export_0001"
	r.Model = "test-model-20260101"

	return r
}

// decodeExported unmarshals a content attribute off a decoded span, failing when it is
// absent or is not the valid document the whole truncation design exists to produce.
func decodeExported(g *WithT, s agenttest.OTLPSpan, key string) []map[string]any {
	g.THelper()

	raw, ok := s.String(key)
	g.Expect(ok).To(BeTrue(), "expected %s on span %q", key, s.Name)

	var out []map[string]any
	g.Expect(json.Unmarshal([]byte(raw), &out)).To(Succeed(),
		"%s must survive the wire as valid JSON: %s", key, raw)

	return out
}

// TestExport_HistogramsKeepTheirBoundaries is the spec for the first defect this area
// produced, which shipped green: every instrument was using the SDK's defaults.
//
// No attribute assertion can see it. The values, counts and labels are all correct; only
// the buckets are wrong, and only against a decoder is that visible.
func TestExport_HistogramsKeepTheirBoundaries(t *testing.T) {
	g := NewWithT(t)

	rx := agenttest.NewOTLPReceiver(t)
	exportRun(t, g, rx, config.TelemetryCaptureConfig{})

	instruments := []string{
		"gen_ai.client.token.usage",
		"gen_ai.client.operation.duration",
		"gen_ai.invoke_agent.duration",
		"gen_ai.invoke_agent.inference_calls",
		"gen_ai.invoke_agent.tool_calls",
		"gen_ai.execute_tool.duration",
	}

	for _, name := range instruments {
		m := rx.Metric(t, name)
		g.Expect(m.Histogram).To(BeTrue(), "%s should be a histogram", name)
		g.Expect(m.Bounds).ToNot(BeEmpty(), "%s arrived with no bucket boundaries", name)
		g.Expect(m.Bounds).ToNot(Equal(sdkDefaultBuckets),
			"%s arrived with the SDK's default buckets, so its boundaries were never configured", name)
	}
}

// TestExport_NoContentWhenCaptureIsOff asserts a run that did not opt in exports no part
// of the conversation, asserted against what a collector actually received.
func TestExport_NoContentWhenCaptureIsOff(t *testing.T) {
	g := NewWithT(t)

	rx := agenttest.NewOTLPReceiver(t)
	exportRun(t, g, rx, config.TelemetryCaptureConfig{}, withSystemPrompt("you are a careful agent"))

	g.Expect(rx.Spans()).ToNot(BeEmpty())

	for _, s := range rx.Spans() {
		for _, key := range contentKeys {
			g.Expect(s.Has(string(key))).To(BeFalse(), "span %q exported %s with capture off", s.Name, key)
		}
	}
}

// TestExport_ContentSurvivesTheWire asserts each content attribute arrives as a document
// that parses, and that the delta's index chain reconciles once decoded.
func TestExport_ContentSurvivesTheWire(t *testing.T) {
	g := NewWithT(t)

	rx := agenttest.NewOTLPReceiver(t)
	exportRun(t, g, rx, config.TelemetryCaptureConfig{Enabled: true}, withSystemPrompt("you are a careful agent"))

	chats := rx.SpansNamed("chat ")
	g.Expect(chats).To(HaveLen(2))

	for _, chat := range chats {
		exported := decodeExported(g, chat, "gen_ai.input.messages")
		decodeExported(g, chat, "gen_ai.output.messages")

		from, ok := chat.Int(string(telemetry.AttrContentFromIndex))
		g.Expect(ok).To(BeTrue())

		sent, ok := chat.Int(string(telemetry.AttrLLMMessages))
		g.Expect(ok).To(BeTrue())

		// The property that makes a delta readable: what a span carries plus where it
		// starts accounts for every message the call was given.
		g.Expect(int(from)+len(exported)).To(BeEquivalentTo(sent),
			"from_index plus the exported messages must account for every message sent")
	}

	// The second call adds the assistant turn and the results answering it, and repeats
	// nothing the first already carried.
	second := decodeExported(g, chats[1], "gen_ai.input.messages")
	g.Expect(second).To(HaveLen(2))

	tool := rx.Span(t, "execute_tool ")
	g.Expect(tool.Has("gen_ai.tool.call.arguments")).To(BeTrue())
	g.Expect(tool.Has("gen_ai.tool.call.result")).To(BeTrue())
}

// TestExport_SystemInstructionsCarryText is the spec for a defect found by reading a
// decoded payload: the attribute led with {"type":"text"} and nothing else.
//
// The prompt is the configured one followed by optional notes, so an agent configuring
// none contributes an empty leading segment. Every assertion on this attribute passed
// throughout, because each checked the shape of a part rather than whether it said
// anything.
func TestExport_SystemInstructionsCarryText(t *testing.T) {
	g := NewWithT(t)

	rx := agenttest.NewOTLPReceiver(t)
	exportRun(t, g, rx, config.TelemetryCaptureConfig{Enabled: true})

	parts := decodeExported(g, rx.Span(t, "startup "), "gen_ai.system_instructions")
	g.Expect(parts).ToNot(BeEmpty())

	for _, p := range parts {
		g.Expect(p).To(HaveKey("content"), "a text part must carry text: %v", parts)
		g.Expect(p["content"]).ToNot(BeEmpty())
	}
}

// TestExport_TruncatedContentStillCarriesContent is the spec for the defect that mattered
// most, and it is the one whose absence let the defect ship.
//
// Under a tight cap the input arrived as the truncation marker alone, with none of the
// conversation in it. The fallback is valid JSON and inside the cap, so every assertion
// that checked validity and size passed against a document containing nothing it was
// asked to carry. The assertion has to be that the budget was spent on content.
func TestExport_TruncatedContentStillCarriesContent(t *testing.T) {
	g := NewWithT(t)

	rx := agenttest.NewOTLPReceiver(t)
	// Angle brackets and ampersands encode to six bytes each, which is the expansion the
	// byte budget has to account for and the case an ASCII fixture cannot see.
	prompt := strings.Repeat("a very long prompt about <widgets> & things ", 200)

	exportRun(t, g, rx, config.TelemetryCaptureConfig{Enabled: true, MaxBytes: 512},
		withPrompt(prompt), withSystemPrompt("you are a careful agent"))

	chat := rx.SpansNamed("chat ")[0]

	raw, ok := chat.String("gen_ai.input.messages")
	g.Expect(ok).To(BeTrue())
	g.Expect(len(raw)).To(BeNumerically("<=", 512))

	decodeExported(g, chat, "gen_ai.input.messages")

	g.Expect(raw).To(ContainSubstring("a very long prompt"),
		"the whole budget went to structure and the marker: %s", raw)
	g.Expect(len(raw)).To(BeNumerically(">", 256),
		"most of the budget should be content, got %d bytes: %s", len(raw), raw)

	cut, ok := chat.Attributes[string(telemetry.AttrContentTruncated)].([]any)
	g.Expect(ok).To(BeTrue(), "a truncated span must say which attributes were cut")
	g.Expect(cut).To(ContainElement("gen_ai.input.messages"))
}

// TestExport_CaptureCompressesAndShrinksTheBatch asserts the two pipeline changes capture
// makes, neither of which is visible anywhere but on the wire.
//
// A refused request loses the whole batch and OTLP is fire and forget, so both of these
// exist to keep a request inside what a collector will accept.
func TestExport_CaptureCompressesAndShrinksTheBatch(t *testing.T) {
	g := NewWithT(t)

	plain := agenttest.NewOTLPReceiver(t)
	exportRun(t, g, plain, config.TelemetryCaptureConfig{})

	captured := agenttest.NewOTLPReceiver(t)
	exportRun(t, g, captured, config.TelemetryCaptureConfig{Enabled: true}, withSystemPrompt("you are a careful agent"))

	traceRequest := func(rx *agenttest.OTLPReceiver) agenttest.OTLPRequest {
		idx := slices.IndexFunc(rx.Requests(), func(r agenttest.OTLPRequest) bool { return r.Signal == "traces" && r.Spans > 0 })
		g.Expect(idx).To(BeNumerically(">=", 0))

		return rx.Requests()[idx]
	}

	off := traceRequest(plain)
	on := traceRequest(captured)

	g.Expect(off.Encoding).To(BeEmpty(), "a run without content has nothing worth compressing")
	g.Expect(on.Encoding).To(Equal("gzip"))

	// Content roughly doubles the payload, and compression more than pays for it. Both
	// halves matter: without the first the fixture is not exercising capture at all.
	g.Expect(on.DecodedBytes).To(BeNumerically(">", off.DecodedBytes))
	g.Expect(on.WireBytes).To(BeNumerically("<", on.DecodedBytes/2))
}

// TestExport_SessionAppendMetricCrossesTheWire covers the journal append instrument,
// which the other export specs cannot: they run uncheckpointed, so no record is ever
// appended and the instrument records nothing.
//
// It is here rather than only in the telemetry package's own specs because this
// instrument declares its own bucket boundaries, and boundaries failing to survive
// export is the exact defect this file exists for. Its layout is two orders of magnitude
// finer at the bottom than the others, since an append to the file backend is a local
// write and would otherwise sit entirely in the first bucket.
func TestExport_SessionAppendMetricCrossesTheWire(t *testing.T) {
	g := NewWithT(t)

	rx := agenttest.NewOTLPReceiver(t)

	app := agenttest.NewFakeApp(t, exampleApp())
	cfg := agenttest.Config(t, app, func(c *config.Config) {
		c.Telemetry.Enabled = true
		c.Telemetry.Endpoint = rx.Endpoint()
	})

	tel, err := bootstrap.Start(context.Background(), bootstrap.Options{Config: cfg, Version: util.Version()})
	g.Expect(err).ToNot(HaveOccurred())

	_, err = agent.Run(context.Background(), agent.Options{
		Config:       cfg,
		ConfigFile:   "agent.yaml",
		Prompt:       []string{"summarize the widget inventory"},
		Provider:     agenttest.NewScriptedProvider(t, exportUsage(agenttest.TextResponse("all done"))),
		SessionStore: agenttest.NewFakeSessionStore(t),
		Checkpoint:   agent.Checkpoint{Enabled: true},
		Telemetry:    tel.Provider,
	}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).ToNot(HaveOccurred())

	delivery, err := tel.Close()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(delivery.Complete()).To(BeTrue())

	m := rx.Metric(t, telemetry.MetricSessionAppendDuration)
	g.Expect(m.Histogram).To(BeTrue())
	g.Expect(m.Count).To(BeNumerically(">", 0))
	g.Expect(m.Bounds).ToNot(Equal(sdkDefaultBuckets),
		"%s arrived with the SDK's default buckets, so its boundaries were never configured",
		telemetry.MetricSessionAppendDuration)

	// The finest boundary is what makes a local append measurable at all; the SDK's
	// default layout starts at 0 and jumps to 5, which is 5 seconds here.
	g.Expect(m.Bounds[0]).To(BeNumerically("<", 0.001))

	backend, ok := m.Attributes[string(telemetry.AttrSessionBackend)]
	g.Expect(ok).To(BeTrue(), "the append metric arrived with no backend: %v", m.Attributes)
	g.Expect(backend).ToNot(BeEmpty())
}
