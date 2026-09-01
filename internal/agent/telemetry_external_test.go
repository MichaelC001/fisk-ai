//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// These live in the external agent_test package alongside the examples: they drive
// only agent's exported API, asserting the lifetime of the startup span. Lifetime is
// the whole risk in this area, because an unended span is never exported at all: a
// leak on one of the many early returns before the run loop does not produce a wrong
// trace, it produces no trace, which is indistinguishable from telemetry being off.
//
// The recording provider is built per test through telemetry.NewFromProviders rather
// than from a process global, so these stay parallel-safe and leak nothing between
// packages. It uses a syncer, so a span reaches the exporter the instant it ends,
// which is what lets the handoff test observe the ordering from inside the run.
package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/memory"
	"github.com/choria-io/fisk-ai/internal/rag"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/telemetry"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/toolkit/functool"
)

// recordingTelemetry returns a Provider writing every ended span straight into the
// returned exporter.
func recordingTelemetry() (*telemetry.Provider, *tracetest.InMemoryExporter) {
	exp := tracetest.NewInMemoryExporter()

	return telemetry.NewFromProviders(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp)), nil), exp
}

// capturingTelemetry is recordingTelemetry with content capture on, for the specs that
// assert what the conversation looks like on the wire.
func capturingTelemetry(c telemetry.ContentCapture) (*telemetry.Provider, *tracetest.InMemoryExporter) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))

	return telemetry.NewFromProviders(tp, nil, telemetry.WithContentCapture(c)), exp
}

// contentKeys are the semantic convention attributes content capture can produce. The
// list is here so a spec can assert that a run with capture off carries none of them,
// which is the assertion that has to hold whatever else changes.
var contentKeys = []attribute.Key{
	"gen_ai.system_instructions",
	"gen_ai.input.messages",
	"gen_ai.output.messages",
	"gen_ai.tool.call.arguments",
	"gen_ai.tool.call.result",
}

// decodeContent unmarshals a content attribute, failing when it is absent or is not
// the valid JSON document the whole truncation design exists to guarantee.
func decodeContent(stub tracetest.SpanStub, key attribute.Key) []map[string]any {
	GinkgoHelper()

	v, ok := spanAttr(stub, key)
	Expect(ok).To(BeTrue(), "expected %s on span %q", key, stub.Name)

	var out []map[string]any
	Expect(json.Unmarshal([]byte(v.AsString()), &out)).To(Succeed(), "%s must be valid JSON: %s", key, v.AsString())

	return out
}

// spanNamed returns the one recorded span whose name starts with prefix, failing if
// there is not exactly one. Selecting by name rather than by position keeps these
// tests from breaking every time a run learns to emit another span.
func spanNamed(exp *tracetest.InMemoryExporter, prefix string) tracetest.SpanStub {
	GinkgoHelper()

	var found []tracetest.SpanStub
	var names []string
	for _, span := range exp.GetSpans() {
		names = append(names, span.Name)
		if strings.HasPrefix(span.Name, prefix) {
			found = append(found, span)
		}
	}

	Expect(found).To(HaveLen(1), "expected exactly one %q span, recorded: %v", prefix, names)

	return found[0]
}

// startupSpan returns the recorded startup span.
func startupSpan(exp *tracetest.InMemoryExporter) tracetest.SpanStub {
	GinkgoHelper()

	return spanNamed(exp, "startup ")
}

// rootSpan returns the recorded root span for a one-shot run, which is the agent
// invocation. A chat run's root is a workflow and is fetched by its own name.
func rootSpan(exp *tracetest.InMemoryExporter) tracetest.SpanStub {
	GinkgoHelper()

	return spanNamed(exp, "invoke_agent ")
}

// spanAttr returns one attribute from a recorded span.
func spanAttr(stub tracetest.SpanStub, key attribute.Key) (attribute.Value, bool) {
	for _, kv := range stub.Attributes {
		if kv.Key == key {
			return kv.Value, true
		}
	}

	return attribute.Value{}, false
}

// eventsNamed returns every event on a span with the given name.
func eventsNamed(stub tracetest.SpanStub, name string) []sdktrace.Event {
	var out []sdktrace.Event
	for _, e := range stub.Events {
		if e.Name == name {
			out = append(out, e)
		}
	}

	return out
}

// eventAttr returns one attribute from a span event.
func eventAttr(e sdktrace.Event, key attribute.Key) (attribute.Value, bool) {
	for _, kv := range e.Attributes {
		if kv.Key == key {
			return kv.Value, true
		}
	}

	return attribute.Value{}, false
}

// eventKeys returns an event's attribute keys, for asserting what is NOT on it.
func eventKeys(e sdktrace.Event) []string {
	var out []string
	for _, kv := range e.Attributes {
		out = append(out, string(kv.Key))
	}

	return out
}

// messagesReply is a minimal successful reply from the model backend.
const messagesReply = `{"id":"msg_01","type":"message","role":"assistant",` +
	`"model":"claude-test-20260101","content":[{"type":"text","text":"done"}],` +
	`"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2}}`

var _ = Describe("Agent telemetry", func() {
	It("Should export one startup span named for the identity and carrying the resolved tool counts", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		tel, exp := recordingTelemetry()

		_, err := agent.Run(context.Background(), agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			Provider:   agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
			Telemetry:  tel,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).ToNot(HaveOccurred())

		span := startupSpan(exp)
		Expect(span.Name).To(Equal("startup " + cfg.Identity))

		// The wrapped example application supplies tools, so a zero here would mean the
		// counts were recorded before the tool sources resolved rather than after.
		v, ok := spanAttr(span, telemetry.AttrToolsApplication)
		Expect(ok).To(BeTrue())
		Expect(v.AsInt64()).To(BeNumerically(">", 0))

		for _, key := range []attribute.Key{
			telemetry.AttrToolsBuiltin,
			telemetry.AttrToolsRemote,
			telemetry.AttrToolsCustom,
			telemetry.AttrToolsDeferred,
			telemetry.AttrRemoteHosts,
		} {
			_, ok := spanAttr(span, key)
			Expect(ok).To(BeTrue(), "expected %s to be set", key)
		}
	})

	// This is what makes reading the store worth doing at all.
	//
	// The config and the store agree for every configured backend, so nothing else can tell
	// apart "asked the store" from "asked the config". They disagree exactly once: an
	// injected store, where the config still says file while something else entirely serves
	// the tools. Injecting a fake that reports its own identity is the only way to prove
	// which of the two was consulted.
	It("Should report on the startup span the memory backend that ran", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app, agenttest.WithMemory())
		tel, exp := recordingTelemetry()

		store := agenttest.NewFakeMemoryStore(GinkgoTB())
		store.SetInfo(memory.Info{Backend: "jetstream", Location: "FISK_MEMORY"})

		_, err := agent.Run(context.Background(), agent.Options{
			Config:      cfg,
			ConfigFile:  "agent.yaml",
			Prompt:      []string{"go"},
			Provider:    agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
			MemoryStore: store,
			Telemetry:   tel,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).ToNot(HaveOccurred())

		startup := startupSpan(exp)

		backend, ok := spanAttr(startup, telemetry.AttrMemoryBackend)
		Expect(ok).To(BeTrue())
		Expect(backend.AsString()).To(Equal("jetstream"))
		Expect(backend.AsString()).ToNot(Equal(string(cfg.MemoryBackend())))

		location, ok := spanAttr(startup, telemetry.AttrMemoryLocation)
		Expect(ok).To(BeTrue())
		Expect(location.AsString()).To(Equal("FISK_MEMORY"))
	})

	// A backend with nothing safe to name reports the backend alone.
	//
	// The file backend's container is an absolute local directory. On a span that is high
	// cardinality and describes the operator's machine rather than the run, which is the same
	// reason the knowledge data source id may not fall back to its database path.
	It("Should omit from the startup span a memory location that is not safe to name", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app, agenttest.WithMemory())
		tel, exp := recordingTelemetry()

		_, err := agent.Run(context.Background(), agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			StoreDir:   GinkgoT().TempDir(),
			Provider:   agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
			Telemetry:  tel,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).ToNot(HaveOccurred())

		startup := startupSpan(exp)

		backend, ok := spanAttr(startup, telemetry.AttrMemoryBackend)
		Expect(ok).To(BeTrue())
		Expect(backend.AsString()).To(Equal(string(memory.BackendFile)))

		_, ok = spanAttr(startup, telemetry.AttrMemoryLocation)
		Expect(ok).To(BeFalse())

		for _, kv := range startup.Attributes {
			Expect(kv.Value.String()).ToNot(ContainSubstring("/"))
		}
	})

	// The index load is its own span under startup, and threading startup's context did
	// not drag the run loop with it.
	//
	// That second half is the whole risk. Startup's context was deliberately dropped because
	// assigning it to ctx once made every span the loop opened a child of startup, nesting
	// the entire run inside a span that ends at the handoff. Setup now has a child, so the
	// context is threaded, and this pins that the loop's spans still parent to the root.
	It("Should nest the memory index span under startup", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app, agenttest.WithMemory())
		tel, exp := recordingTelemetry()

		_, err := agent.Run(context.Background(), agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			StoreDir:   GinkgoT().TempDir(),
			Provider:   agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
			Telemetry:  tel,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).ToNot(HaveOccurred())

		root := rootSpan(exp)
		startup := startupSpan(exp)
		index := spanNamed(exp, "memory_index")

		Expect(index.Parent.SpanID()).To(Equal(startup.SpanContext.SpanID()))
		Expect(startup.Parent.SpanID()).To(Equal(root.SpanContext.SpanID()))

		// The loop still hangs off the root, not off the span that ended at the handoff.
		chat := spanNamed(exp, "chat ")
		Expect(chat.Parent.SpanID()).To(Equal(root.SpanContext.SpanID()))
	})

	// Without it, "which backend was this slow memory_write on" is a walk: find the tool
	// span, take its trace id, find that trace's startup span, read the attribute off it. No
	// backend expresses that as one query. It is deliberately not a metrics question either,
	// since the backend on the tool duration histogram would be empty on every tool that is
	// not a memory tool.
	//
	// The injected fake reporting an identity the config does not is what proves the store
	// was asked rather than the config, exactly as the startup spec above does it.
	It("Should report on a memory tool span the backend that served it", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app, agenttest.WithMemory())
		tel, exp := recordingTelemetry()

		store := agenttest.NewFakeMemoryStore(GinkgoTB())
		store.SetInfo(memory.Info{Backend: "jetstream", Location: "FISK_MEMORY"})

		_, err := agent.Run(context.Background(), agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			Provider: agenttest.NewScriptedProvider(GinkgoTB(),
				agenttest.ToolUseResponse("call_1", "memory_write", []byte(`{"key":"k","description":"d","content":"c"}`)),
				agenttest.TextResponse("done"),
			),
			MemoryStore: store,
			Telemetry:   tel,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).ToNot(HaveOccurred())

		tool := spanNamed(exp, "execute_tool ")
		Expect(tool.Name).To(Equal("execute_tool memory_write"))

		backend, ok := spanAttr(tool, telemetry.AttrMemoryBackend)
		Expect(ok).To(BeTrue())
		Expect(backend.AsString()).To(Equal("jetstream"))
		Expect(backend.AsString()).ToNot(Equal(string(cfg.MemoryBackend())))

		location, ok := spanAttr(tool, telemetry.AttrMemoryLocation)
		Expect(ok).To(BeTrue())
		Expect(location.AsString()).To(Equal("FISK_MEMORY"))
	})

	// The pair stays a filter for memory tools rather than becoming a label on every tool
	// call.
	//
	// A backend and location on a call that never touched the store would be worse than
	// absent: it would say the wrong thing, since it names a store that had no part in the
	// call, and it would make the attribute useless for selecting the calls that did.
	It("Should carry no memory attribution on a non-memory tool span", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app, agenttest.WithMemory())
		tel, exp := recordingTelemetry()

		store := agenttest.NewFakeMemoryStore(GinkgoTB())
		store.SetInfo(memory.Info{Backend: "jetstream", Location: "FISK_MEMORY"})

		_, err := agent.Run(context.Background(), agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			Provider: agenttest.NewScriptedProvider(GinkgoTB(),
				agenttest.ToolUseResponse("call_1", "do", []byte(`{"subject":"widgets"}`)),
				agenttest.TextResponse("done"),
			),
			MemoryStore: store,
			Telemetry:   tel,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).ToNot(HaveOccurred())

		tool := spanNamed(exp, "execute_tool ")
		Expect(tool.Name).To(Equal("execute_tool do"))

		_, ok := spanAttr(tool, telemetry.AttrMemoryBackend)
		Expect(ok).To(BeFalse())
		_, ok = spanAttr(tool, telemetry.AttrMemoryLocation)
		Expect(ok).To(BeFalse())

		// The run did bind a store, so the pair is on startup. This is what makes the
		// absence above a statement about the call rather than about the run.
		_, ok = spanAttr(startupSpan(exp), telemetry.AttrMemoryBackend)
		Expect(ok).To(BeTrue())
	})

	// The attribution describes the tool that ran, not the one the model asked for.
	//
	// This is the spec that fails if the pair is attached when the span opens, and nothing
	// else notices: a PreToolUse hook can redirect a memory call to a tool that never touches
	// the store, and the reverse, so both directions are driven here. Attribution taken at
	// span start reports a backend for a call that never reached one, and reports none for a
	// call that did.
	It("Should follow a rewritten call with the memory attribution", func() {
		store := agenttest.NewFakeMemoryStore(GinkgoTB())
		store.SetInfo(memory.Info{Backend: "jetstream", Location: "FISK_MEMORY"})

		run := func(called string, rewrite agent.PreToolUseResult, input []byte) tracetest.SpanStub {
			GinkgoHelper()

			app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
			cfg := agenttest.Config(GinkgoTB(), app, agenttest.WithMemory())
			tel, exp := recordingTelemetry()

			_, err := agent.Run(context.Background(), agent.Options{
				Config:     cfg,
				ConfigFile: "agent.yaml",
				Prompt:     []string{"go"},
				Provider: agenttest.NewScriptedProvider(GinkgoTB(),
					agenttest.ToolUseResponse("call_1", called, input),
					agenttest.TextResponse("done"),
				),
				MemoryStore: store,
				Telemetry:   tel,
				Hooks: agent.Hooks{
					PreToolUse: func(context.Context, agent.PreToolUseInfo) (agent.PreToolUseResult, error) {
						return rewrite, nil
					},
				},
			}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
			Expect(err).ToNot(HaveOccurred())

			return spanNamed(exp, "execute_tool ")
		}

		// The tool that ran is read off gen_ai.tool.name rather than off the span name: the
		// name is fixed when the span opens, before any hook has been consulted, while the
		// attribute is rewritten in Finish. The attribution has to agree with the attribute.
		//
		// A memory call redirected away from the store claims no backend, because none
		// served it.
		away := run("memory_list", agent.PreToolUseResult{
			RewriteTool:  "do",
			RewriteInput: json.RawMessage(`{"subject":"widgets"}`),
		}, []byte(`{}`))

		name, ok := spanAttr(away, "gen_ai.tool.name")
		Expect(ok).To(BeTrue())
		Expect(name.AsString()).To(Equal("do"))

		_, ok = spanAttr(away, telemetry.AttrMemoryBackend)
		Expect(ok).To(BeFalse())
		_, ok = spanAttr(away, telemetry.AttrMemoryLocation)
		Expect(ok).To(BeFalse())

		// And the reverse: an ordinary call redirected into the store is attributed, because
		// the store is what answered it.
		into := run("do", agent.PreToolUseResult{
			RewriteTool:  "memory_list",
			RewriteInput: json.RawMessage(`{}`),
		}, []byte(`{"subject":"widgets"}`))

		name, ok = spanAttr(into, "gen_ai.tool.name")
		Expect(ok).To(BeTrue())
		Expect(name.AsString()).To(Equal("memory_list"))

		backend, ok := spanAttr(into, telemetry.AttrMemoryBackend)
		Expect(ok).To(BeTrue())
		Expect(backend.AsString()).To(Equal("jetstream"))

		location, ok := spanAttr(into, telemetry.AttrMemoryLocation)
		Expect(ok).To(BeTrue())
		Expect(location.AsString()).To(Equal("FISK_MEMORY"))
	})

	// The pair stays on startup even once the tool spans carry it.
	//
	// This is the case the rule exists for. The memory index is the only other setup-time
	// memory work and an operator can turn it off, so a run with no_index that never calls a
	// memory tool would otherwise have nothing anywhere in its trace reporting that memory
	// existed at all.
	It("Should keep the memory attribution on startup with no index and no tool call", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app, func(c *config.Config) {
			c.Harness.Memory = &config.MemoryConfig{Enabled: true, NoIndex: true}
		})
		tel, exp := recordingTelemetry()

		store := agenttest.NewFakeMemoryStore(GinkgoTB())
		store.SetInfo(memory.Info{Backend: "jetstream", Location: "FISK_MEMORY"})

		_, err := agent.Run(context.Background(), agent.Options{
			Config:      cfg,
			ConfigFile:  "agent.yaml",
			Prompt:      []string{"go"},
			Provider:    agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
			MemoryStore: store,
			Telemetry:   tel,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).ToNot(HaveOccurred())

		for _, span := range exp.GetSpans() {
			Expect(span.Name).ToNot(Equal("memory_index"))
			Expect(span.Name).ToNot(HavePrefix("execute_tool "))
		}

		startup := startupSpan(exp)

		backend, ok := spanAttr(startup, telemetry.AttrMemoryBackend)
		Expect(ok).To(BeTrue())
		Expect(backend.AsString()).To(Equal("jetstream"))

		location, ok := spanAttr(startup, telemetry.AttrMemoryLocation)
		Expect(ok).To(BeTrue())
		Expect(location.AsString()).To(Equal("FISK_MEMORY"))
	})

	// The span closes when setup ends rather than when Run returns.
	//
	// The deferred End that catches the early returns would, left to itself, also fire at
	// function exit on the success path and charge the entire run loop to startup. A
	// duration that wrong is worse than none, because it looks plausible.
	//
	// PreModelCall is the hook to observe from. It fires inside the run loop, past the
	// handoff, so seeing the span already exported there proves the explicit End ran.
	// RunStart looks like the natural choice and is not: it fires during setup, well
	// before the runner is built, so it would see an unended span no matter what.
	It("Should end the startup span at the handoff", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		tel, exp := recordingTelemetry()

		endedBeforeLoop := false

		_, err := agent.Run(context.Background(), agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			Provider:   agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
			Telemetry:  tel,
			Hooks: agent.Hooks{
				PreModelCall: func(context.Context, agent.PreModelCallInfo) error {
					for _, span := range exp.GetSpans() {
						if strings.HasPrefix(span.Name, "startup ") {
							endedBeforeLoop = true
						}
					}
					return nil
				},
			},
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).ToNot(HaveOccurred())

		Expect(endedBeforeLoop).To(BeTrue(), "startup span had not ended by the time the run loop began")
	})

	// A setup failure still exports the span, classified.
	//
	// This is the regression the startupDone guard exists for. Run has 38 early returns
	// before the runner is constructed, and they cover the slow work the span is there to
	// show: the NATS dial, opening the knowledge index, the remote tool import. A bad
	// ToolWorkDir is the earliest of them, so it proves the guard rather than any later
	// cleanup. Without the guard this run produces no span whatsoever.
	It("Should end the startup span on an early return", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		tel, exp := recordingTelemetry()

		_, err := agent.Run(context.Background(), agent.Options{
			Config:      cfg,
			ConfigFile:  "agent.yaml",
			Prompt:      []string{"go"},
			Provider:    agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
			Telemetry:   tel,
			ToolWorkDir: filepath.Join(GinkgoT().TempDir(), "does-not-exist"),
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).To(HaveOccurred())

		span := startupSpan(exp)
		Expect(span.Name).To(Equal("startup " + cfg.Identity))

		v, ok := spanAttr(span, "error.type")
		Expect(ok).To(BeTrue())
		Expect(v.AsString()).To(Equal(telemetry.ClassConfig.String()))

		// The failure names a filesystem path. Only the class may leave the process: the
		// span status and every attribute on it are checked, since either would carry the
		// path off-box where it cannot be un-sent.
		Expect(span.Status.Description).To(BeEmpty())
		for _, kv := range span.Attributes {
			Expect(kv.Value.String()).ToNot(ContainSubstring("does-not-exist"))
		}
	})

	// A one-shot run's root is the agent invocation, the startup span nests inside it, and
	// the run's outcome is on it.
	//
	// The nesting is what makes one run one trace, and it is checked by trace id rather
	// than taken on trust: a root started after any child, or a context not threaded
	// through, produces two unrelated traces that each look fine on their own.
	It("Should make the root span the whole run", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		tel, exp := recordingTelemetry()

		res, err := agent.Run(context.Background(), agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			Provider:   agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
			Telemetry:  tel,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).ToNot(HaveOccurred())

		root := rootSpan(exp)
		Expect(root.Name).To(Equal("invoke_agent " + cfg.Identity))

		startup := startupSpan(exp)
		Expect(startup.Parent.SpanID()).To(Equal(root.SpanContext.SpanID()))
		Expect(startup.SpanContext.TraceID()).To(Equal(root.SpanContext.TraceID()))

		v, ok := spanAttr(root, telemetry.AttrRunTerminalReason)
		Expect(ok).To(BeTrue())
		Expect(v.AsString()).To(Equal("completed"))

		crashed, ok := spanAttr(root, telemetry.AttrRunCrashed)
		Expect(ok).To(BeTrue())
		Expect(crashed.AsBool()).To(BeFalse())

		// The run's own summary line points at the trace it produced, which is the only
		// correlator an un-checkpointed chat has.
		Expect(res.Stats.TraceID).To(Equal(root.SpanContext.TraceID().String()))
	})

	// A run that never reached the loop still ends its root, classified.
	//
	// A refused resume or a bad config is exactly the trace an operator goes looking for
	// when a run is rejected in CI, and an unended root would mean there is nothing to
	// find. setup_failed is reported rather than an empty reason, which would read as a
	// hole in the instrumentation.
	It("Should report a setup failure on the root span", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		tel, exp := recordingTelemetry()

		_, err := agent.Run(context.Background(), agent.Options{
			Config:      cfg,
			ConfigFile:  "agent.yaml",
			Prompt:      []string{"go"},
			Provider:    agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
			Telemetry:   tel,
			ToolWorkDir: filepath.Join(GinkgoT().TempDir(), "does-not-exist"),
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).To(HaveOccurred())

		root := rootSpan(exp)

		v, ok := spanAttr(root, telemetry.AttrRunTerminalReason)
		Expect(ok).To(BeTrue())
		Expect(v.AsString()).To(Equal("setup_failed"))

		class, ok := spanAttr(root, "error.type")
		Expect(ok).To(BeTrue())
		Expect(class.AsString()).To(Equal(telemetry.ClassConfig.String()))

		// res.Stats is nil on this path, so a root that read it without guarding would
		// crash the run it is meant to be reporting on.
		Expect(root.Status.Description).To(BeEmpty())
	})

	// A chat run's shape: the root is a workflow, each turn is an agent invocation nested
	// under it, and a one-shot run gets no turn span at all.
	//
	// The two operations are deliberately different names. A chat is several invocations
	// of one agent, which is what a workflow describes; naming the root invoke_agent too
	// would put two identically named bars directly on top of each other in a flame graph,
	// one exactly containing the other, which reads as a bug in the instrumentation.
	It("Should make an interactive run a workflow over turns", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		tel, exp := recordingTelemetry()

		calls := 0
		next := func(context.Context) agent.Continuation {
			calls++
			if calls == 1 {
				return agent.Continuation{Text: "and then?", Continue: true}
			}
			return agent.Continuation{Continue: false}
		}

		_, err := agent.Run(context.Background(), agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"start"},
			Provider: agenttest.NewScriptedProvider(GinkgoTB(),
				agenttest.TextResponse("first answer"),
				agenttest.TextResponse("second answer"),
			),
			NextPrompt: next,
			Telemetry:  tel,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).ToNot(HaveOccurred())

		root := spanNamed(exp, "invoke_workflow ")
		Expect(root.Name).To(Equal("invoke_workflow " + cfg.Identity))

		var turns []tracetest.SpanStub
		for _, span := range exp.GetSpans() {
			if strings.HasPrefix(span.Name, "invoke_agent ") {
				turns = append(turns, span)
			}
		}
		Expect(turns).To(HaveLen(2))

		for _, turn := range turns {
			Expect(turn.Parent.SpanID()).To(Equal(root.SpanContext.SpanID()))
		}

		// Turn indexes are one-based and in order, so a chat's spans can be read as a
		// sequence without sorting by start time.
		var indexes []int64
		for _, turn := range turns {
			v, ok := spanAttr(turn, telemetry.AttrTurnIndex)
			Expect(ok).To(BeTrue())
			indexes = append(indexes, v.AsInt64())
		}
		Expect(indexes).To(ConsistOf(int64(1), int64(2)))
	})

	// The other half of that rule.
	It("Should give a one-shot run no turn span", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		tel, exp := recordingTelemetry()

		_, err := agent.Run(context.Background(), agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			Provider:   agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
			Telemetry:  tel,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).ToNot(HaveOccurred())

		// Exactly one invoke_agent span, and it is the root: no workflow, no nested turn.
		root := rootSpan(exp)
		Expect(root.Parent.IsValid()).To(BeFalse())

		for _, span := range exp.GetSpans() {
			Expect(span.Name).ToNot(HavePrefix("invoke_workflow "))
		}
	})

	It("Should record a model call as a CLIENT span under the run, carrying the request shape, the reply's identifiers and its token tiers", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		tel, exp := recordingTelemetry()

		reply := agenttest.TextResponse("done")
		reply.ID = "msg_01ABC"
		reply.Model = "claude-sonnet-5-20260101"
		reply.Usage = llm.Usage{In: 100, Out: 8, CacheRead: 40, CacheCreate: 12, Thinking: 5}

		_, err := agent.Run(context.Background(), agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			Provider:   agenttest.NewScriptedProvider(GinkgoTB(), reply),
			Telemetry:  tel,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).ToNot(HaveOccurred())

		chat := spanNamed(exp, "chat ")
		Expect(chat.Name).To(Equal("chat " + cfg.LLM.Model))
		Expect(chat.SpanKind).To(Equal(trace.SpanKindClient))

		root := rootSpan(exp)
		Expect(chat.Parent.SpanID()).To(Equal(root.SpanContext.SpanID()))

		for key, want := range map[attribute.Key]string{
			"gen_ai.response.id":    "msg_01ABC",
			"gen_ai.response.model": "claude-sonnet-5-20260101",
		} {
			v, ok := spanAttr(chat, key)
			Expect(ok).To(BeTrue(), "expected %s", key)
			Expect(v.AsString()).To(Equal(want))
		}

		// The requested model and the model that answered are different values and both
		// are on the span: the request names an alias, the reply names the snapshot that
		// billed, and a reproduction has to pin the second.
		requested, ok := spanAttr(chat, "gen_ai.request.model")
		Expect(ok).To(BeTrue())
		Expect(requested.AsString()).To(Equal(cfg.LLM.Model))
		Expect(requested.AsString()).ToNot(Equal("claude-sonnet-5-20260101"))

		// gen_ai.usage.input_tokens includes the cache tiers by spec, so it is the sum
		// rather than the uncached remainder. Getting this backwards understates spend on
		// every cached run, which is most of them.
		input, ok := spanAttr(chat, "gen_ai.usage.input_tokens")
		Expect(ok).To(BeTrue())
		Expect(input.AsInt64()).To(Equal(int64(152)))

		cacheRead, ok := spanAttr(chat, "gen_ai.usage.cache_read.input_tokens")
		Expect(ok).To(BeTrue())
		Expect(cacheRead.AsInt64()).To(Equal(int64(40)))

		// Reasoning is a share of the output tokens rather than a tier beside them, so it
		// is reported alongside and never added in. It is on the span because reasoning is
		// not rendered by default, which leaves a dashboard the only place its cost shows.
		reasoning, ok := spanAttr(chat, "gen_ai.usage.reasoning.output_tokens")
		Expect(ok).To(BeTrue())
		Expect(reasoning.AsInt64()).To(Equal(int64(5)))

		output, ok := spanAttr(chat, "gen_ai.usage.output_tokens")
		Expect(ok).To(BeTrue())
		Expect(output.AsInt64()).To(Equal(int64(8)))
	})

	// A reply cut off at the output cap is an error on the span.
	//
	// The HTTP call succeeded, so nothing at the transport layer knows anything went
	// wrong, but the loop ends the run on it: a truncated turn is an incomplete answer and
	// a trailing tool_use in it is not safe to execute. A span reporting that call as
	// successful would disagree with the run's own outcome.
	It("Should mark a truncated reply failed on the chat span", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		tel, exp := recordingTelemetry()

		truncated := agenttest.TextResponse("partial")
		truncated.StopReason = llm.StopMaxTokens

		_, err := agent.Run(context.Background(), agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			Provider:   agenttest.NewScriptedProvider(GinkgoTB(), truncated),
			Telemetry:  tel,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).To(HaveOccurred())

		chat := spanNamed(exp, "chat ")
		Expect(chat.Status.Code).To(Equal(codes.Error))

		class, ok := spanAttr(chat, "error.type")
		Expect(ok).To(BeTrue())
		Expect(class.AsString()).To(Equal(telemetry.ClassTruncated.String()))

		// The provider's own stop reason is passed through rather than mapped: the
		// attribute has no enum precisely so a provider's vocabulary survives.
		finish, ok := spanAttr(chat, "gen_ai.response.finish_reasons")
		Expect(ok).To(BeTrue())
		Expect(finish.AsStringSlice()).To(ConsistOf("max_tokens"))
	})

	It("Should record a tool call as a span carrying the tool, its kind, its outcome and its argument key names", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		tel, exp := recordingTelemetry()

		_, err := agent.Run(context.Background(), agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			Provider: agenttest.NewScriptedProvider(GinkgoTB(),
				agenttest.ToolUseResponse("call_1", "do", []byte(`{"subject":"widgets"}`)),
				agenttest.TextResponse("done"),
			),
			Telemetry: tel,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).ToNot(HaveOccurred())

		tool := spanNamed(exp, "execute_tool ")
		Expect(tool.Name).To(Equal("execute_tool do"))

		name, ok := spanAttr(tool, "gen_ai.tool.name")
		Expect(ok).To(BeTrue())
		Expect(name.AsString()).To(Equal("do"))

		callID, ok := spanAttr(tool, "gen_ai.tool.call.id")
		Expect(ok).To(BeTrue())
		Expect(callID.AsString()).To(Equal("call_1"))

		out, ok := spanAttr(tool, telemetry.AttrToolOutcome)
		Expect(ok).To(BeTrue())
		Expect(out.AsString()).To(Equal(telemetry.ToolOutcomeExecuted.String()))

		// Key names, never values: this is the no-content middle tier that answers "what
		// did it ask for" without exporting what was said.
		keys, ok := spanAttr(tool, telemetry.AttrToolArgKeys)
		Expect(ok).To(BeTrue())
		Expect(keys.AsStringSlice()).To(ConsistOf("subject"))
		for _, kv := range tool.Attributes {
			Expect(kv.Value.String()).ToNot(ContainSubstring("widgets"))
		}
	})

	// This is the load-bearing assertion for fisk.tool.exit_code, and the one an
	// implementer is most likely not to write.
	//
	// "Only report failures" is the natural reading and it is wrong twice over: it makes a
	// command that succeeded indistinguishable from a built-in that ran no command at all,
	// and it reduces an int to a bool wearing an int's clothes. A spec asserting only that a
	// non-zero exit is recorded passes cleanly against that implementation.
	//
	// It also pins that a zero exit is not an error: the outcome stays executed and no
	// error.type appears, because a non-zero exit is an ordinary result and flagging exits at
	// all would put a failure on every routine "grep matched nothing".
	It("Should record a successful exit code on the tool span", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		tel, exp := recordingTelemetry()

		_, err := agent.Run(context.Background(), agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			Provider: agenttest.NewScriptedProvider(GinkgoTB(),
				agenttest.ToolUseResponse("call_1", "do", []byte(`{"subject":"widgets"}`)),
				agenttest.TextResponse("done"),
			),
			Telemetry: tel,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).ToNot(HaveOccurred())

		tool := spanNamed(exp, "execute_tool ")

		code, ok := spanAttr(tool, telemetry.AttrToolExitCode)
		Expect(ok).To(BeTrue(), "a command that exited zero must still report its status")
		Expect(code.AsInt64()).To(BeZero())

		out, ok := spanAttr(tool, telemetry.AttrToolOutcome)
		Expect(ok).To(BeTrue())
		Expect(out.AsString()).To(Equal(telemetry.ToolOutcomeExecuted.String()))

		_, ok = spanAttr(tool, "error.type")
		Expect(ok).To(BeFalse())
	})

	// The attribute is absent, not zero, for a tool that ran no command.
	//
	// Every tool kind but the local command tool leaves the exec metadata nil: a built-in, a
	// Go caller's own tool, a tool invoked on a remote agent. Publishing zero for those would
	// report "the command succeeded" for a command that never existed, which is the same
	// fabrication the remote tool path already refuses to make when it declines to wrap an
	// exec-less reply in a command envelope.
	It("Should omit the exit code for an in-process tool", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app, agenttest.WithMemory())
		tel, exp := recordingTelemetry()

		_, err := agent.Run(context.Background(), agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			Provider: agenttest.NewScriptedProvider(GinkgoTB(),
				agenttest.ToolUseResponse("call_1", "memory_list", []byte(`{}`)),
				agenttest.TextResponse("done"),
			),
			Telemetry: tel,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).ToNot(HaveOccurred())

		tool := spanNamed(exp, "execute_tool ")
		Expect(tool.Name).To(Equal("execute_tool memory_list"))

		// The kind is what tells an operator the absence is expected rather than a gap.
		kind, ok := spanAttr(tool, telemetry.AttrToolKind)
		Expect(ok).To(BeTrue())
		Expect(kind.AsString()).To(Equal("builtin"))

		_, ok = spanAttr(tool, telemetry.AttrToolExitCode)
		Expect(ok).To(BeFalse())
	})

	// A tool name the model invented cannot reach a span name.
	//
	// This is a cost control, not tidiness. The name comes straight from the model,
	// unvalidated, so a hallucinating model or a prompt injection written for exactly this
	// would otherwise mint an unbounded set of span names and, through them, metric series
	// that an operator pays for and cannot un-send. The raw string is still recorded, once,
	// as a span attribute, because otherwise nobody could tell what the model asked for.
	It("Should keep an unknown tool out of the span name", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		tel, exp := recordingTelemetry()

		invented := "definitely_not_a_real_tool_9f3a"

		_, err := agent.Run(context.Background(), agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			Provider: agenttest.NewScriptedProvider(GinkgoTB(),
				agenttest.ToolUseResponse("call_1", invented, []byte(`{}`)),
				agenttest.TextResponse("done"),
			),
			Telemetry: tel,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).ToNot(HaveOccurred())

		tool := spanNamed(exp, "execute_tool ")
		Expect(tool.Name).To(Equal("execute_tool unknown_tool"))
		Expect(tool.Name).ToNot(ContainSubstring(invented))

		// No gen_ai.tool.name at all: that attribute is for registry-validated names, and
		// it is a metric label elsewhere.
		_, ok := spanAttr(tool, "gen_ai.tool.name")
		Expect(ok).To(BeFalse())

		requested, ok := spanAttr(tool, telemetry.AttrToolRequestedName)
		Expect(ok).To(BeTrue())
		Expect(requested.AsString()).To(Equal(invented))

		out, ok := spanAttr(tool, telemetry.AttrToolOutcome)
		Expect(ok).To(BeTrue())
		Expect(out.AsString()).To(Equal(telemetry.ToolOutcomeUnknownTool.String()))
	})

	// A call a hook refused is still a span, distinguishable from one that ran and failed.
	//
	// The outcome axis is the whole point: a denial, an unknown tool, a missing argument
	// and an operator refusal all produce an error result to the model, so an error rate
	// that could not tell them apart would report a healthy agent's policy working as a
	// tool that keeps breaking.
	It("Should record a policy denial on the tool span", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		tel, exp := recordingTelemetry()

		_, err := agent.Run(context.Background(), agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			Provider: agenttest.NewScriptedProvider(GinkgoTB(),
				agenttest.ToolUseResponse("call_1", "do", []byte(`{"subject":"widgets"}`)),
				agenttest.TextResponse("done"),
			),
			Telemetry: tel,
			Hooks: agent.Hooks{
				PreToolUse: func(context.Context, agent.PreToolUseInfo) (agent.PreToolUseResult, error) {
					return agent.PreToolUseResult{Deny: true, DenyReason: "not allowed"}, nil
				},
			},
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).ToNot(HaveOccurred())

		tool := spanNamed(exp, "execute_tool ")

		out, ok := spanAttr(tool, telemetry.AttrToolOutcome)
		Expect(ok).To(BeTrue())
		Expect(out.AsString()).To(Equal(telemetry.ToolOutcomePolicyDenied.String()))

		// Denied, not failed: nothing ran, so this is not a tool error.
		Expect(tool.Status.Code).ToNot(Equal(codes.Error))
	})

	// A prompt the operator never answered is classified apart from one they refused. The
	// run suspends, the resume asks again, and an outcome of confirm_denied would count a
	// refusal that never happened against an agent whose gate is working.
	It("Should record an unanswered question on the tool span", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		tel, exp := recordingTelemetry()

		gated, err := functool.New(functool.Spec{
			Name:        "stream_rm",
			Description: "removes a stream",
			Schema:      map[string]any{"type": "object"},
			Confirm:     &functool.ConfirmSpec{},
			Handler: func(context.Context, json.RawMessage, *functool.CallContext) (string, error) {
				return "", nil
			},
		})
		Expect(err).ToNot(HaveOccurred())

		prompter := agenttest.NewScriptedPrompter(GinkgoTB())
		prompter.ApproveFn = func(toolkit.GateRequest) (toolkit.ConfirmChoice, error) {
			return toolkit.ConfirmNo, fmt.Errorf("%w: interrupt", toolkit.ErrPromptAborted)
		}

		_, err = agent.Run(context.Background(), agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"remove the stream"},
			Provider: agenttest.NewScriptedProvider(GinkgoTB(),
				agenttest.ToolUseResponse("call_1", "stream_rm", []byte(`{}`)),
			),
			Telemetry:   tel,
			CustomTools: []toolkit.Tool{gated},
		}, agenttest.NewRecordingEvents(), prompter)
		Expect(err).To(MatchError(toolkit.ErrPromptAborted))

		tool := spanNamed(exp, "execute_tool ")

		out, ok := spanAttr(tool, telemetry.AttrToolOutcome)
		Expect(ok).To(BeTrue())
		Expect(out.AsString()).To(Equal(telemetry.ToolOutcomeUnanswered.String()))
	})

	// A gate question whose answer arrives later is classified as deferred rather than as
	// an operator's refusal.
	//
	// A prompter reaches this by returning toolkit.DeferResult, which an embedder whose
	// operator is not on the end of the connection does: the question is put, nobody has
	// answered yet, and the run suspends with the call recorded. Reported as a refusal it
	// would tell an operator grouping by outcome that somebody declined the command.
	It("Should record a deferred question on the tool span", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		tel, exp := recordingTelemetry()

		var ran atomic.Bool
		gated, err := functool.New(functool.Spec{
			Name:        "stream_rm",
			Description: "removes a stream",
			Schema:      map[string]any{"type": "object"},
			Confirm:     &functool.ConfirmSpec{},
			Handler: func(context.Context, json.RawMessage, *functool.CallContext) (string, error) {
				ran.Store(true)
				return "", nil
			},
		})
		Expect(err).ToNot(HaveOccurred())

		prompter := agenttest.NewScriptedPrompter(GinkgoTB())
		prompter.ApproveFn = func(toolkit.GateRequest) (toolkit.ConfirmChoice, error) {
			return toolkit.ConfirmNo, toolkit.DeferResult("waiting on the caller", "q-1")
		}

		res, err := agent.Run(context.Background(), agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"remove the stream"},
			Provider: agenttest.NewScriptedProvider(GinkgoTB(),
				agenttest.ToolUseResponse("call_1", "stream_rm", []byte(`{}`)),
			),
			Telemetry:   tel,
			CustomTools: []toolkit.Tool{gated},
		}, agenttest.NewRecordingEvents(), prompter)
		Expect(err).ToNot(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonSuspended))
		Expect(res.Deferred).To(HaveLen(1))
		Expect(ran.Load()).To(BeFalse(), "a command whose approval is still outstanding must not run")

		tool := spanNamed(exp, "execute_tool ")

		out, ok := spanAttr(tool, telemetry.AttrToolOutcome)
		Expect(ok).To(BeTrue())
		Expect(out.AsString()).To(Equal(telemetry.ToolOutcomeDeferred.String()))
	})

	// A recovered panic still produces a root span that ends, classified, and carrying no
	// stack.
	//
	// Two failures are guarded here. The span has to END: the panic barrier substitutes
	// the error and returns, so a root whose End sat anywhere inside that barrier would
	// never run for one of the two paths through it, and an unended span is never
	// exported, meaning the crash an operator most wants to see would be the one run that
	// produced no trace at all.
	//
	// And it must carry no stack. The idiomatic OTel call for an error records
	// exception.stacktrace, which is exactly what the run path works to keep off anything
	// leaving the process: it leaks absolute paths and frame arguments. That the crash
	// still reaches the Events sink locally is the point; only the span is restricted.
	It("Should end the root span on a crash", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		tel, exp := recordingTelemetry()

		_, err := agent.Run(context.Background(), agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			Provider:   panicProvider{},
			Telemetry:  tel,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))

		var panicErr *agent.PanicError
		Expect(errors.As(err, &panicErr)).To(BeTrue())

		root := rootSpan(exp)

		crashed, ok := spanAttr(root, telemetry.AttrRunCrashed)
		Expect(ok).To(BeTrue())
		Expect(crashed.AsBool()).To(BeTrue())

		class, ok := spanAttr(root, "error.type")
		Expect(ok).To(BeTrue())
		Expect(class.AsString()).To(Equal(telemetry.ClassPanic.String()))

		// A crash inside the loop is not a setup failure, and reporting it as one would
		// send an operator to the wrong half of the run.
		reason, ok := spanAttr(root, telemetry.AttrRunTerminalReason)
		Expect(ok).To(BeTrue())
		Expect(reason.AsString()).To(Equal("error"))

		Expect(root.Events).To(BeEmpty())
		_, ok = spanAttr(root, "exception.stacktrace")
		Expect(ok).To(BeFalse())
		Expect(root.Status.Description).To(BeEmpty())
	})

	// The default path, telemetry unwired, runs to completion. Every call site calls the
	// facade unconditionally, so a nil Provider that was not safe would panic in the run
	// every operator has.
	It("Should run clean with a nil telemetry provider", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)

		_, err := agent.Run(context.Background(), agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			Provider:   agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).ToNot(HaveOccurred())
	})

	// A rate-limited model call reports each HTTP attempt as its own event and the retry
	// as a resend count.
	//
	// This drives the whole integration on purpose, and a unit test over the middleware
	// would be worthless here. Three separate things have to hold for an operator to see a
	// retry, and only one of them is inside the middleware: it has to be installed in the
	// chain agent.Run assembles, the chat span has to survive the SDK's own
	// context.WithTimeout into the request context, and the retry has to actually go back
	// through the middleware rather than around it. A hand-built request would assert the
	// middleware parses a response, which was never the part at risk. So this uses no
	// injected provider: Options.Provider is nil precisely so agent.Run builds the real
	// backend and installs the real chain.
	It("Should record every HTTP attempt of a retried call", func() {
		var requests atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// The first attempt is rate limited and the second succeeds. retry-after is zero
			// so the SDK's backoff does not make this a slow test.
			if requests.Add(1) == 1 {
				w.Header().Set("retry-after", "0")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))

				return
			}

			w.Header().Set("content-type", "application/json")
			_, _ = w.Write([]byte(messagesReply))
		}))
		defer srv.Close()

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		// agenttest.Config does not run the config's prepare step, so the per-call timeout
		// is its zero value, and the provider builds context.WithTimeout(ctx, 0), which is a
		// context that has already expired. Set it here: every other test in this file
		// injects a provider and so never reaches that code.
		cfg.LLM.Budget.CallTimeoutParsed = 30 * time.Second
		tel, exp := recordingTelemetry()

		_, err := agent.Run(context.Background(), agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			APIKey:     "test-key",
			BaseURL:    srv.URL,
			Telemetry:  tel,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).ToNot(HaveOccurred())
		Expect(requests.Load()).To(Equal(int64(2)), "expected the SDK to retry the 429")

		chat := spanNamed(exp, "chat ")

		// One event per attempt, in order, each carrying its own status.
		attempts := eventsNamed(chat, telemetry.EventLLMHTTPResponse)
		Expect(attempts).To(HaveLen(2))

		for i, wantStatus := range []int64{429, 200} {
			ordinal, ok := eventAttr(attempts[i], telemetry.AttrLLMHTTPAttempt)
			Expect(ok).To(BeTrue())
			Expect(ordinal.AsInt64()).To(Equal(int64(i + 1)))

			status, ok := eventAttr(attempts[i], "http.response.status_code")
			Expect(ok).To(BeTrue())
			Expect(status.AsInt64()).To(Equal(wantStatus))
		}

		// The status code is deliberately NOT on the span. This span is two requests, so a
		// span attribute would be last-attempt-wins and would report 200 for a call that
		// spent most of its wall clock being rate limited, which is the opposite of what an
		// operator is looking for.
		_, onSpan := spanAttr(chat, "http.response.status_code")
		Expect(onSpan).To(BeFalse())

		// The resend count is monotonic, so it can live on the span and is the only
		// per-attempt fact that aggregates: it is what makes "which runs retried" a query
		// rather than an exercise in opening spans one at a time.
		resends, ok := spanAttr(chat, "http.request.resend_count")
		Expect(ok).To(BeTrue())
		Expect(resends.AsInt64()).To(Equal(int64(1)))

		// An attempt is a status, a duration and an ordinal. The request URL can carry
		// userinfo, the headers carry the credential and the body is the prompt, so this
		// asserts the whole key set rather than the absence of any one of them: a later
		// addition has to come here and be argued for.
		Expect(eventKeys(attempts[0])).To(ConsistOf(
			string(telemetry.AttrLLMHTTPAttempt),
			string(telemetry.AttrLLMHTTPDurationMS),
			"http.response.status_code",
		))
	})

	// An attempt that never got a response is reported as an error event and does not
	// crash the run.
	//
	// The middleware wraps the SDK's own HTTP client call, which returns a nil response
	// alongside its error for every transport failure: DNS, a reset connection, a TLS
	// failure, a per-attempt deadline. Reading a status code off that response is a nil
	// dereference inside the retry loop of a live run, so the failure path is a separate
	// event shape rather than a status of zero.
	It("Should record an HTTP transport failure without a status", func() {
		// A server that is listening at the moment the URL is taken and refuses connections
		// by the time the call is made.
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := srv.URL
		srv.Close()

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		// Without this the per-call timeout is zero, the provider builds an
		// already-expired context, and this test passes on a deadline rather than on the
		// refused connection it exists to cover.
		cfg.LLM.Budget.CallTimeoutParsed = 30 * time.Second
		tel, exp := recordingTelemetry()

		_, err := agent.Run(context.Background(), agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			APIKey:     "test-key",
			BaseURL:    url,
			Telemetry:  tel,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).To(HaveOccurred())

		chat := spanNamed(exp, "chat ")

		failures := eventsNamed(chat, telemetry.EventLLMHTTPError)
		Expect(failures).ToNot(BeEmpty())
		Expect(eventsNamed(chat, telemetry.EventLLMHTTPResponse)).To(BeEmpty())

		// No status, because there was no response to read one from.
		_, ok := eventAttr(failures[0], "http.response.status_code")
		Expect(ok).To(BeFalse())

		// The class comes from the closed vocabulary, and the error text does not travel:
		// a connection error names the host and port it failed to reach.
		// A refused connection is not a context error, so ClassifyContext declines it and
		// the fallback names it. Asserting the exact class rather than a set of plausible
		// ones is what keeps this test honest: with the per-call timeout left at zero it
		// would report a timeout, and a looser assertion would call that a pass.
		class, ok := eventAttr(failures[0], "error.type")
		Expect(ok).To(BeTrue())
		Expect(class.AsString()).To(Equal(telemetry.ClassProvider.String()))

		for _, e := range failures {
			Expect(eventKeys(e)).To(ConsistOf(
				string(telemetry.AttrLLMHTTPAttempt),
				string(telemetry.AttrLLMHTTPDurationMS),
				"error.type",
			))
		}
	})

	// A knowledge search opened by the model produces a retrieval span underneath the
	// execute_tool span that dispatched it.
	//
	// The parent id is the assertion, and it is the only one that catches this. The Provider
	// reaches internal/rag through the context alone, so a path that fails to thread it
	// emits nothing at all, and a path that threads the wrong context emits a span with
	// every name and every attribute correct sitting beside its parent rather than under it.
	// Neither shows up in a test that checks the span exists.
	It("Should nest retrieval under the tool call", func() {
		ctx := context.Background()

		corpus := GinkgoT().TempDir()
		Expect(os.WriteFile(filepath.Join(corpus, "note.md"), []byte("the widget inventory is managed here"), 0o600)).To(Succeed())

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		cfg.Harness.RAG = &config.RAGConfig{Enabled: true, Directory: filepath.Join(GinkgoT().TempDir(), "kb"), Paths: []string{corpus}}

		writer, err := rag.OpenWriter(cfg, "", rag.Options{})
		Expect(err).ToNot(HaveOccurred())
		_, err = writer.Index(ctx, []string{corpus}, rag.IndexOptions{})
		Expect(err).ToNot(HaveOccurred())
		Expect(writer.Close()).To(Succeed())

		tel, exp := recordingTelemetry()

		_, err = agent.Run(ctx, agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			Provider: agenttest.NewScriptedProvider(GinkgoTB(),
				agenttest.ToolUseResponse("call_1", "knowledge_search", []byte(`{"query":"widget inventory"}`)),
				agenttest.TextResponse("done"),
			),
			Telemetry: tel,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).ToNot(HaveOccurred())

		tool := spanNamed(exp, "execute_tool ")
		Expect(tool.Name).To(Equal("execute_tool knowledge_search"))

		retrieval := spanNamed(exp, "retrieval")
		Expect(retrieval.Parent.SpanID()).To(Equal(tool.SpanContext.SpanID()))
		Expect(retrieval.SpanContext.TraceID()).To(Equal(tool.SpanContext.TraceID()))

		// The lexical tier ran, so the corpus was actually searched rather than the span
		// being recorded over an index that was never opened.
		tier, ok := spanAttr(retrieval, telemetry.AttrKnowledgeTierEffective)
		Expect(ok).To(BeTrue())
		Expect(tier.AsString()).To(Equal(telemetry.TierLexical))

		// The knowledge tools are the one datastore kind, which is what separates them from
		// every other tool on the metric the tool span records.
		toolType, ok := spanAttr(tool, "gen_ai.tool.type")
		Expect(ok).To(BeTrue())
		Expect(toolType.AsString()).To(Equal(telemetry.ToolTypeDatastore))
	})

	// This is the spec that has to hold whatever else changes: a run with telemetry on and
	// capture unset exports structure and timing and no conversation at all.
	//
	// It asserts across every span rather than one, because the failure it guards is a
	// content attribute reaching a span whose author did not think of it as carrying
	// content, and it asserts the absence of the keys rather than the absence of a
	// particular string, because a run whose fixture happens not to contain the string
	// would pass while exporting everything.
	It("Should carry no content when capture is off", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		tel, exp := recordingTelemetry()

		_, err := agent.Run(context.Background(), agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			Provider: agenttest.NewScriptedProvider(GinkgoTB(),
				agenttest.ToolUseResponse("call_1", "do", []byte(`{"subject":"widgets"}`)),
				agenttest.TextResponse("done"),
			),
			Telemetry: tel,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).ToNot(HaveOccurred())

		spans := exp.GetSpans()
		Expect(spans).ToNot(BeEmpty())

		for _, span := range spans {
			for _, key := range contentKeys {
				_, ok := spanAttr(span, key)
				Expect(ok).To(BeFalse(), "span %q carries %s with capture off", span.Name, key)
			}
			_, ok := spanAttr(span, telemetry.AttrContentFromIndex)
			Expect(ok).To(BeFalse(), "span %q carries a content marker with capture off", span.Name)
		}
	})

	It("Should carry what was sent and what came back on the chat span, each as a document that parses", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		tel, exp := capturingTelemetry(telemetry.ContentCapture{})

		_, err := agent.Run(context.Background(), agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"summarize the widget inventory"},
			Provider:   agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("all done")),
			Telemetry:  tel,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).ToNot(HaveOccurred())

		chat := spanNamed(exp, "chat ")

		in := decodeContent(chat, "gen_ai.input.messages")
		Expect(in).To(HaveLen(1))
		Expect(in[0]["role"]).To(Equal("user"))

		out := decodeContent(chat, "gen_ai.output.messages")
		Expect(out).To(HaveLen(1))
		Expect(out[0]["role"]).To(Equal("assistant"))
		Expect(out[0]["finish_reason"]).ToNot(BeEmpty())

		// The first call of a process starts at the beginning, so the marker is zero and
		// says so rather than being left off.
		idx, ok := spanAttr(chat, telemetry.AttrContentFromIndex)
		Expect(ok).To(BeTrue())
		Expect(idx.AsInt64()).To(BeZero())
	})

	// This is the spec the whole delta design rests on.
	//
	// Two things are asserted together because either alone passes against a broken
	// implementation: that the second model call carries only what was added since the
	// first, and that from_index plus the messages exported equals fisk.llm.messages on the
	// same span. The second is what makes a delta readable at all, since without it a span
	// saying "17 messages sent" beside two exported ones is just a contradiction.
	It("Should send only the delta and chain the indexes", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		tel, exp := capturingTelemetry(telemetry.ContentCapture{})

		_, err := agent.Run(context.Background(), agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			Provider: agenttest.NewScriptedProvider(GinkgoTB(),
				agenttest.ToolUseResponse("call_1", "do", []byte(`{"subject":"widgets"}`)),
				agenttest.TextResponse("done"),
			),
			Telemetry: tel,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).ToNot(HaveOccurred())

		var chats []tracetest.SpanStub
		for _, span := range exp.GetSpans() {
			if strings.HasPrefix(span.Name, "chat ") {
				chats = append(chats, span)
			}
		}
		Expect(chats).To(HaveLen(2))

		// The second call adds the assistant turn that asked for the tool and the results
		// answering it, and carries neither the prompt nor anything the first call already
		// exported.
		second := decodeContent(chats[1], "gen_ai.input.messages")
		Expect(second).To(HaveLen(2))
		Expect(second[0]["role"]).To(Equal("assistant"))
		Expect(second[1]["role"]).To(Equal("tool"))

		for _, chat := range chats {
			idx, ok := spanAttr(chat, telemetry.AttrContentFromIndex)
			Expect(ok).To(BeTrue())

			sent, ok := spanAttr(chat, telemetry.AttrLLMMessages)
			Expect(ok).To(BeTrue())

			exported := decodeContent(chat, "gen_ai.input.messages")
			Expect(int(idx.AsInt64())+len(exported)).To(Equal(int(sent.AsInt64())),
				"from_index plus the exported messages must account for every message sent")
		}
	})

	// The opt-out does what its name says, which the delta spec above cannot see.
	It("Should carry the whole conversation with full messages", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		tel, exp := capturingTelemetry(telemetry.ContentCapture{Full: true})

		_, err := agent.Run(context.Background(), agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			Provider: agenttest.NewScriptedProvider(GinkgoTB(),
				agenttest.ToolUseResponse("call_1", "do", []byte(`{"subject":"widgets"}`)),
				agenttest.TextResponse("done"),
			),
			Telemetry: tel,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).ToNot(HaveOccurred())

		var last tracetest.SpanStub
		for _, span := range exp.GetSpans() {
			if strings.HasPrefix(span.Name, "chat ") {
				last = span
			}
		}

		Expect(decodeContent(last, "gen_ai.input.messages")).To(HaveLen(3))

		idx, ok := spanAttr(last, telemetry.AttrContentFromIndex)
		Expect(ok).To(BeTrue())
		Expect(idx.AsInt64()).To(BeZero(), "the full conversation always starts at the beginning")
	})

	// The run constant is exported once, on the span that ends early, and not repeated on
	// every model call.
	//
	// The placement is the thing being pinned. On the root it would reach a collector only
	// when the run ends, which for an interactive chat is when the operator quits and for a
	// killed process is never; on the chat span it would be an identical copy per
	// iteration.
	It("Should put the system prompt on startup once", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		// A system prompt has to exist for there to be one to place. An agent that
		// configures none exports no attribute at all rather than an empty document.
		cfg := agenttest.Config(GinkgoTB(), app, func(c *config.Config) {
			c.SystemPrompt = "you are a careful agent"
		})
		tel, exp := capturingTelemetry(telemetry.ContentCapture{})

		_, err := agent.Run(context.Background(), agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			Provider: agenttest.NewScriptedProvider(GinkgoTB(),
				agenttest.ToolUseResponse("call_1", "do", []byte(`{"subject":"widgets"}`)),
				agenttest.TextResponse("done"),
			),
			Telemetry: tel,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).ToNot(HaveOccurred())

		carriers := 0
		for _, span := range exp.GetSpans() {
			_, ok := spanAttr(span, "gen_ai.system_instructions")
			if !ok {
				continue
			}
			carriers++
			Expect(span.Name).To(HavePrefix("startup "))
		}
		Expect(carriers).To(Equal(1), "the system prompt is a run constant and belongs on exactly one span")

		instructions := decodeContent(startupSpan(exp), "gen_ai.system_instructions")
		Expect(instructions).ToNot(BeEmpty())
		Expect(instructions[0]["type"]).To(Equal("text"))
	})

	// This is the spec that catches the call-point trap, and nothing else does.
	//
	// The system prompt is built where the tool inventory resolves and then appended to
	// twice more: a resumed run adds its reminder, and a memory-enabled run adds the memory
	// index. Recording it beside the other startup setters, which is the natural place,
	// exports a prompt missing exactly those pieces: shorter, entirely plausible, and
	// invisible to any assertion on presence or shape.
	It("Should capture the system prompt after every append", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app, agenttest.WithMemory())
		tel, exp := capturingTelemetry(telemetry.ContentCapture{})

		store := agenttest.NewFakeMemoryStore(GinkgoTB())
		Expect(store.Write(context.Background(), "deploy-notes", "how the deploy works", "body", false)).To(Succeed())

		_, err := agent.Run(context.Background(), agent.Options{
			Config:      cfg,
			ConfigFile:  "agent.yaml",
			Prompt:      []string{"go"},
			Provider:    agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
			MemoryStore: store,
			Telemetry:   tel,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).ToNot(HaveOccurred())

		v, ok := spanAttr(startupSpan(exp), "gen_ai.system_instructions")
		Expect(ok).To(BeTrue())

		// The memory index is appended last of all, so its presence is what proves the
		// capture happened after every append rather than where it looks like it belongs.
		Expect(v.AsString()).To(ContainSubstring("deploy-notes"))
	})

	It("Should carry the arguments and the result on the tool span", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		tel, exp := capturingTelemetry(telemetry.ContentCapture{})

		_, err := agent.Run(context.Background(), agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			Provider: agenttest.NewScriptedProvider(GinkgoTB(),
				agenttest.ToolUseResponse("call_1", "do", []byte(`{"subject":"widgets"}`)),
				agenttest.TextResponse("done"),
			),
			Telemetry: tel,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).ToNot(HaveOccurred())

		tool := spanNamed(exp, "execute_tool ")

		args, ok := spanAttr(tool, "gen_ai.tool.call.arguments")
		Expect(ok).To(BeTrue())

		var obj map[string]any
		Expect(json.Unmarshal([]byte(args.AsString()), &obj)).To(Succeed())
		Expect(obj).To(HaveKeyWithValue("subject", "widgets"))

		res, ok := spanAttr(tool, "gen_ai.tool.call.result")
		Expect(ok).To(BeTrue())
		Expect(json.Valid([]byte(res.AsString()))).To(BeTrue())
	})

	// A call that never ran still records the answer the model acted on.
	//
	// This is a decision rather than a fallout, and it is worth a spec because the opposite
	// reading is just as natural: a policy denial executed nothing, so there is no "result"
	// in the conventions' sense. But the model was told something and continued from it, so
	// a trace showing the call and not the answer describes half of what happened. It also
	// means an operator should know a denied call still exports its arguments.
	It("Should carry what the model was told on a denied tool call", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		tel, exp := capturingTelemetry(telemetry.ContentCapture{})

		_, err := agent.Run(context.Background(), agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			Provider: agenttest.NewScriptedProvider(GinkgoTB(),
				agenttest.ToolUseResponse("call_1", "do", []byte(`{"subject":"widgets"}`)),
				agenttest.TextResponse("done"),
			),
			Telemetry: tel,
			Hooks: agent.Hooks{
				PreToolUse: func(context.Context, agent.PreToolUseInfo) (agent.PreToolUseResult, error) {
					return agent.PreToolUseResult{Deny: true, DenyReason: "not in this environment"}, nil
				},
			},
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).ToNot(HaveOccurred())

		tool := spanNamed(exp, "execute_tool ")

		out, ok := spanAttr(tool, telemetry.AttrToolOutcome)
		Expect(ok).To(BeTrue())
		Expect(out.AsString()).To(Equal(telemetry.ToolOutcomePolicyDenied.String()))

		res, ok := spanAttr(tool, "gen_ai.tool.call.result")
		Expect(ok).To(BeTrue(), "the model was told why, and that is what it acted on")
		Expect(res.AsString()).To(ContainSubstring("not in this environment"))

		_, ok = spanAttr(tool, "gen_ai.tool.call.arguments")
		Expect(ok).To(BeTrue(), "a denied call still exports its arguments")
	})

	// The truncation markers reach the span, since the genai specs prove the documents and
	// this proves they are reported.
	It("Should keep oversized content valid and say so", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		tel, exp := capturingTelemetry(telemetry.ContentCapture{MaxBytes: 256})

		_, err := agent.Run(context.Background(), agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{strings.Repeat("a very long prompt ", 200)},
			Provider:   agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
			Telemetry:  tel,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).ToNot(HaveOccurred())

		chat := spanNamed(exp, "chat ")

		// Valid JSON is the property the whole truncation design exists for.
		decodeContent(chat, "gen_ai.input.messages")

		cut, ok := spanAttr(chat, telemetry.AttrContentTruncated)
		Expect(ok).To(BeTrue())
		Expect(cut.AsStringSlice()).To(ContainElement("gen_ai.input.messages"))
	})

	// The index load reports how much work it was, not only how long it took.
	//
	// Listing reads every value to recover its description, so on a network backend the
	// span's duration is a round trip per entry. Without the count a slow load cannot be
	// told from a large store, and those have different fixes.
	It("Should count the entries on the memory index span", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app, agenttest.WithMemory())
		tel, exp := recordingTelemetry()

		store := agenttest.NewFakeMemoryStore(GinkgoTB())
		for _, key := range []string{"deploy-notes", "api-conventions", "oncall"} {
			Expect(store.Write(context.Background(), key, "a description", "body", false)).To(Succeed())
		}

		_, err := agent.Run(context.Background(), agent.Options{
			Config:      cfg,
			ConfigFile:  "agent.yaml",
			Prompt:      []string{"go"},
			Provider:    agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
			MemoryStore: store,
			Telemetry:   tel,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).ToNot(HaveOccurred())

		index := spanNamed(exp, "memory_index")

		entries, ok := spanAttr(index, telemetry.AttrMemoryEntries)
		Expect(ok).To(BeTrue())
		Expect(entries.AsInt64()).To(BeEquivalentTo(3))
	})

	// This is the spec an implementer skips, and the only one that fails against the
	// obvious implementation.
	//
	// Recording len(entries) whatever happened reads as correct and passes the spec above.
	// A failed list returns no entries, so it would publish zero, and zero already means
	// something else here: the store is reachable and empty. Those send an operator to
	// different places.
	It("Should omit the count on the memory index span when the load failed", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app, agenttest.WithMemory())
		tel, exp := recordingTelemetry()

		store := agenttest.NewFakeMemoryStore(GinkgoTB())
		store.SetListError(errors.New("bucket FISK_MEMORY is not available"))

		_, err := agent.Run(context.Background(), agent.Options{
			Config:      cfg,
			ConfigFile:  "agent.yaml",
			Prompt:      []string{"go"},
			Provider:    agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
			MemoryStore: store,
			Telemetry:   tel,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))

		// A listing failure is advisory: the tools still reach the store, so the run goes on.
		Expect(err).ToNot(HaveOccurred())

		index := spanNamed(exp, "memory_index")

		_, ok := spanAttr(index, telemetry.AttrMemoryEntries)
		Expect(ok).To(BeFalse(), "a failed load has no count, and zero would mean an empty store")

		class, ok := spanAttr(index, "error.type")
		Expect(ok).To(BeTrue())
		Expect(class.AsString()).To(Equal(telemetry.ClassStore.String()))

		// The backend that could not be listed is still named, which is what makes the
		// failure attributable.
		backend, ok := spanAttr(index, telemetry.AttrMemoryBackend)
		Expect(ok).To(BeTrue())
		Expect(backend.AsString()).ToNot(BeEmpty())

		// The store's own message names a bucket, and only the class may leave the process.
		Expect(index.Status.Description).To(BeEmpty())
		for _, kv := range index.Attributes {
			Expect(kv.Value.String()).ToNot(ContainSubstring("FISK_MEMORY"))
		}
	})

	// An interrupted setup is not reported as a broken memory store.
	//
	// The listing is a round trip per entry on a network backend, so it is among the
	// likeliest places for a Ctrl-C to land, and classifying by domain first would file
	// every one of them under a store failure.
	It("Should classify a canceled memory index load as cancellation", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app, agenttest.WithMemory())
		tel, exp := recordingTelemetry()

		store := agenttest.NewFakeMemoryStore(GinkgoTB())
		store.SetListError(context.Canceled)

		_, err := agent.Run(context.Background(), agent.Options{
			Config:      cfg,
			ConfigFile:  "agent.yaml",
			Prompt:      []string{"go"},
			Provider:    agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
			MemoryStore: store,
			Telemetry:   tel,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).ToNot(HaveOccurred())

		class, ok := spanAttr(spanNamed(exp, "memory_index"), "error.type")
		Expect(ok).To(BeTrue())
		Expect(class.AsString()).To(Equal(telemetry.ClassCanceled.String()))
	})

	// This drives the whole path a configured server takes through a trace: the import of
	// the server is a span of its own, the startup span counts what it contributed, and the
	// call the model then makes names the server that served it.
	//
	// The sessions are the caller's, which is the shape fisk serve runs in, so the connect
	// happened before this run existed and there is no connect span to find. That absence
	// is asserted rather than left implicit: it is what says the connect span belongs to
	// whoever opened the sessions.
	It("Should report the MCP server on startup and on the call", func() {
		fake := &mcpFakeServers{tools: []*mcp.Tool{mcpDescriptor("search", "Searches the documentation")}}
		sessions := connectMCP(GinkgoTB(), fake, config.MCPServer{Name: "docs"})

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		cfg.MCPClients = []config.MCPServer{{Name: "docs"}}
		tel, exp := recordingTelemetry()

		_, err := agent.Run(context.Background(), agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"search the docs"},
			Provider: agenttest.NewScriptedProvider(GinkgoTB(),
				agenttest.ToolUseResponse("call-1", "docs_search", json.RawMessage(`{}`)),
				agenttest.TextResponse("done"),
			),
			MCPSessions: sessions,
			Telemetry:   tel,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).ToNot(HaveOccurred())

		count, ok := spanAttr(startupSpan(exp), telemetry.AttrToolsMCP)
		Expect(ok).To(BeTrue())
		Expect(count.AsInt64()).To(Equal(int64(1)))

		imported := spanNamed(exp, "mcp_import ")
		Expect(imported.Name).To(Equal("mcp_import docs"))

		kept, ok := spanAttr(imported, telemetry.AttrMCPToolsKept)
		Expect(ok).To(BeTrue())
		Expect(kept.AsInt64()).To(Equal(int64(1)))

		for _, span := range exp.GetSpans() {
			Expect(span.Name).ToNot(HavePrefix("mcp_connect"))
		}

		tool := spanNamed(exp, "execute_tool ")
		Expect(tool.Name).To(Equal("execute_tool docs_search"))

		server, ok := spanAttr(tool, telemetry.AttrToolMCPServer)
		Expect(ok).To(BeTrue())
		Expect(server.AsString()).To(Equal("docs"))

		// The a2a peer's key stays empty for an MCP call, which is what keeps a backend
		// filtering on it answering with a2a calls alone.
		_, ok = spanAttr(tool, telemetry.AttrToolRemoteAgent)
		Expect(ok).To(BeFalse())
	})
})
