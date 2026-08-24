//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// These tests drive the per-kind tool accounting across a suspend and a resume through
// the exported agent.Run API, which is the only place the journal, the fold and the
// seeded run counters meet. The in-package specs in tool_accounting_test.go cover a
// single run's dispatch.
package agent_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/attribute"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/runstate"
	runstatefile "github.com/choria-io/fisk-ai/internal/runstate/file"
	"github.com/choria-io/fisk-ai/internal/telemetry"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/toolkit/functool"
	"github.com/choria-io/fisk-ai/internal/util"
)

// accountingTool is a custom tool that answers immediately, so a run can make a call
// of the custom kind alongside the wrapped application's own.
func accountingTool(t *testing.T, name string) toolkit.Tool {
	t.Helper()

	tool, err := functool.New(functool.Spec{
		Name:        name,
		Description: "answers with ok",
		Schema:      map[string]any{"type": "object"},
		Handler: func(context.Context, json.RawMessage, *functool.CallContext) (string, error) {
			return "ok", nil
		},
	})
	NewWithT(t).Expect(err).NotTo(HaveOccurred())

	return tool
}

// summedByKind adds up the per-kind buckets, which must equal tool_calls.
func summedByKind(stats *util.RunStats) int64 {
	var n int64
	for _, v := range stats.ToolCallsByKind {
		n += v
	}

	return n
}

// TestToolAccounting_PerKindSurvivesASuspend is the case the partition invariant never
// held for. A resumed run seeded only the coarse totals, so its buckets summed to the
// calls made since the resume while tool_calls counted the conversation. The journal now
// records each call's kind, so the fold recomputes every bucket and the resume seeds
// them.
func TestToolAccounting_PerKindSurvivesASuspend(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	store, err := runstatefile.NewFileStore(t.TempDir())
	g.Expect(err).NotTo(HaveOccurred())

	app := agenttest.NewFakeApp(t, exampleApp())
	tools := []toolkit.Tool{accountingTool(t, "note_add")}

	opts := func(provider *agenttest.ScriptedProvider, cp agent.Checkpoint, suspend func() bool) agent.Options {
		return agent.Options{
			Config:           agenttest.Config(t, app),
			ConfigFile:       "agent.yaml",
			Prompt:           []string{"look it up"},
			Provider:         provider,
			SessionStore:     store,
			Checkpoint:       cp,
			CustomTools:      tools,
			SuspendRequested: suspend,
		}
	}

	// Run 1: one application call, then a suspend at the next boundary.
	polls := 0
	res1, err := agent.Run(ctx, opts(
		agenttest.NewScriptedProvider(t, agenttest.ToolUseResponse("c1", "do", json.RawMessage(`{"subject":"widgets"}`))),
		agent.Checkpoint{Enabled: true},
		func() bool { polls++; return polls > 1 },
	), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res1.Reason).To(Equal(runstate.ReasonSuspended))
	g.Expect(res1.Stats.ToolCalls).To(BeEquivalentTo(1))
	g.Expect(res1.Stats.ToolCallsByKind).To(Equal(map[toolkit.Kind]int64{toolkit.KindApplication: 1}))

	// The journal carries the kind, so folding it recovers the same buckets without the
	// run that wrote them.
	rs, err := store.Load(res1.SessionID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rs.Counters.ToolCalls).To(Equal(int64(1)))
	g.Expect(rs.Counters.ToolCallsByKind).To(Equal(map[toolkit.Kind]int64{toolkit.KindApplication: 1}))

	// Run 2: a resume in a fresh process makes one custom call and answers.
	res2, err := agent.Run(ctx, opts(
		agenttest.NewScriptedProvider(t,
			agenttest.ToolUseResponse("c2", "note_add", json.RawMessage(`{}`)),
			agenttest.TextResponse("found it"),
		),
		agent.Checkpoint{ResumeID: res1.SessionID},
		nil,
	), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res2.Reason).To(Equal(runstate.ReasonCompleted))

	// The counters a resume reports are cumulative from the first instruction, and the
	// buckets say the same thing the total does.
	g.Expect(res2.Stats.ToolCalls).To(BeEquivalentTo(2))
	g.Expect(res2.Stats.ToolCallsByKind).To(Equal(map[toolkit.Kind]int64{
		toolkit.KindApplication: 1,
		toolkit.KindCustom:      1,
	}))
	g.Expect(summedByKind(res2.Stats)).To(Equal(res2.Stats.ToolCalls), "per-kind buckets must partition tool_calls after a resume")
}

// TestToolAccounting_DeniedCallsAreBucketedButNotDispatched drives both axes through the
// journal. A call a policy hook denies is one tool call of its provider's kind that never
// reached that provider, so it belongs in the per-kind bucket and in neither the remote
// nor the MCP total, live and again after the journal is folded back.
func TestToolAccounting_DeniedCallsAreBucketedButNotDispatched(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	store, err := runstatefile.NewFileStore(t.TempDir())
	g.Expect(err).NotTo(HaveOccurred())

	fake := &mcpFakeServers{tools: []*mcp.Tool{mcpDescriptor("search", "Searches the documentation")}}
	sessions := connectMCP(t, fake, config.MCPServer{Name: "docs"})

	transport := agenttest.NewFakeTransport(t, a2a.AgentCard{
		Name:    "weather-svc",
		Version: "1.0.0",
		Tools: []a2a.ToolDescriptor{{
			Name:        "forecast",
			Description: "get the forecast",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}},
	})
	transport.SetToolReply(`{"forecast":"sunny"}`, false)

	cfg := agenttest.Config(t, agenttest.NewFakeApp(t, exampleApp()))
	cfg.MCPServers = []config.MCPServer{{Name: "docs"}}
	cfg.RemoteTools = []config.RemoteToolHost{{Name: "weather-svc"}}

	// The second call to each provider is denied, so each kind has one call that was
	// dispatched and one that never left the process.
	denied := map[string]bool{"c2": true, "c4": true}

	res, err := agent.Run(ctx, agent.Options{
		Config:       cfg,
		ConfigFile:   "agent.yaml",
		Prompt:       []string{"check the docs and the weather"},
		SessionStore: store,
		Checkpoint:   agent.Checkpoint{Enabled: true},
		MCPSessions:  sessions,
		A2ATransport: transport,
		Hooks: agent.Hooks{
			PreToolUse: func(_ context.Context, in agent.PreToolUseInfo) (agent.PreToolUseResult, error) {
				if !denied[in.ToolUseID] {
					return agent.PreToolUseResult{}, nil
				}

				return agent.PreToolUseResult{Deny: true, DenyReason: "blocked by policy"}, nil
			},
		},
		Provider: agenttest.NewScriptedProvider(t,
			agenttest.ToolUseResponse("c1", "docs_search", json.RawMessage(`{}`)),
			agenttest.ToolUseResponse("c2", "docs_search", json.RawMessage(`{}`)),
			agenttest.ToolUseResponse("c3", "forecast", json.RawMessage(`{}`)),
			agenttest.ToolUseResponse("c4", "forecast", json.RawMessage(`{}`)),
			agenttest.TextResponse("done"),
		),
	}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

	want := map[toolkit.Kind]int64{toolkit.KindMCP: 2, toolkit.KindRemote: 2}
	g.Expect(res.Stats.ToolCalls).To(BeEquivalentTo(4))
	g.Expect(res.Stats.MCPToolCalls).To(BeEquivalentTo(1), "the denied MCP call never reached the server")
	g.Expect(res.Stats.RemoteToolCalls).To(BeEquivalentTo(1), "the denied remote call never reached the peer")
	g.Expect(res.Stats.ToolCallsByKind).To(Equal(want))
	g.Expect(summedByKind(res.Stats)).To(Equal(res.Stats.ToolCalls), "per-kind buckets must partition tool_calls")

	// The journal records each call's kind and whether it was dispatched, so folding it
	// recovers both axes rather than reading the buckets as the totals.
	rs, err := store.Load(res.SessionID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rs.Counters.ToolCalls).To(Equal(int64(4)))
	g.Expect(rs.Counters.MCPToolCalls).To(Equal(int64(1)))
	g.Expect(rs.Counters.RemoteToolCalls).To(Equal(int64(1)))
	g.Expect(rs.Counters.ToolCallsByKind).To(Equal(want))
}

// TestToolAccounting_ResumedRunSpanReportsThisProcessOnly pins what the root span says
// about a resumed run's tool calls.
//
// The span subtracted the resume seed from tool_calls alone, while the remote and MCP
// counts came straight from the stats a resume seeds cumulatively. A resumed run then
// reported subsets larger than their total, and summing either subset across a session's
// traces counted every restored call once per resume where the total counted it once.
//
// Both runs call the same two providers, so the session's counts and this process's are
// different numbers, and the cumulative stats are asserted alongside the span to prove
// the seed was subtracted rather than never restored.
func TestToolAccounting_ResumedRunSpanReportsThisProcessOnly(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	store, err := runstatefile.NewFileStore(t.TempDir())
	g.Expect(err).NotTo(HaveOccurred())

	fake := &mcpFakeServers{tools: []*mcp.Tool{mcpDescriptor("search", "Searches the documentation")}}
	sessions := connectMCP(t, fake, config.MCPServer{Name: "docs"})

	transport := agenttest.NewFakeTransport(t, a2a.AgentCard{
		Name:    "weather-svc",
		Version: "1.0.0",
		Tools: []a2a.ToolDescriptor{{
			Name:        "forecast",
			Description: "get the forecast",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}},
	})
	transport.SetToolReply(`{"forecast":"sunny"}`, false)

	cfg := agenttest.Config(t, agenttest.NewFakeApp(t, exampleApp()))
	cfg.MCPServers = []config.MCPServer{{Name: "docs"}}
	cfg.RemoteTools = []config.RemoteToolHost{{Name: "weather-svc"}}

	opts := func(provider *agenttest.ScriptedProvider, cp agent.Checkpoint, suspend func() bool, tel *telemetry.Provider) agent.Options {
		return agent.Options{
			Config:           cfg,
			ConfigFile:       "agent.yaml",
			Prompt:           []string{"check the docs and the weather"},
			Provider:         provider,
			SessionStore:     store,
			Checkpoint:       cp,
			MCPSessions:      sessions,
			A2ATransport:     transport,
			SuspendRequested: suspend,
			Telemetry:        tel,
		}
	}

	// Run 1: one MCP call and one call to the peer, then a suspend at the next boundary.
	polls := 0
	res1, err := agent.Run(ctx, opts(
		agenttest.NewScriptedProvider(t,
			agenttest.ToolUseResponse("c1", "docs_search", json.RawMessage(`{}`)),
			agenttest.ToolUseResponse("c2", "forecast", json.RawMessage(`{}`)),
		),
		agent.Checkpoint{Enabled: true},
		func() bool { polls++; return polls > 2 },
		nil,
	), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res1.Reason).To(Equal(runstate.ReasonSuspended))
	g.Expect(res1.Stats.ToolCalls).To(BeEquivalentTo(2))
	g.Expect(res1.Stats.RemoteToolCalls).To(BeEquivalentTo(1))
	g.Expect(res1.Stats.MCPToolCalls).To(BeEquivalentTo(1))

	// Run 2: a resume that makes one call to each provider again and answers.
	tel, exp := recordingTelemetry()
	res2, err := agent.Run(ctx, opts(
		agenttest.NewScriptedProvider(t,
			agenttest.ToolUseResponse("c3", "docs_search", json.RawMessage(`{}`)),
			agenttest.ToolUseResponse("c4", "forecast", json.RawMessage(`{}`)),
			agenttest.TextResponse("done"),
		),
		agent.Checkpoint{ResumeID: res1.SessionID},
		nil,
		tel,
	), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res2.Reason).To(Equal(runstate.ReasonCompleted))

	// The stats stay cumulative from the first instruction, which is where the span's
	// numbers come from and what makes the subtraction observable.
	g.Expect(res2.Stats.ToolCalls).To(BeEquivalentTo(4))
	g.Expect(res2.Stats.RemoteToolCalls).To(BeEquivalentTo(2))
	g.Expect(res2.Stats.MCPToolCalls).To(BeEquivalentTo(2))

	// Named rather than taken by prefix: a call to the peer opens an invoke_agent span
	// of its own, named for the peer.
	root := spanNamed(g, exp, "invoke_agent "+cfg.Identity)
	for key, want := range map[attribute.Key]int64{
		telemetry.AttrRunToolCalls:       2,
		telemetry.AttrRunRemoteToolCalls: 1,
		telemetry.AttrRunMCPToolCalls:    1,
	} {
		v, ok := spanAttr(root, key)
		g.Expect(ok).To(BeTrue(), "expected %s on the run span", key)
		g.Expect(v.AsInt64()).To(Equal(want), "%s must count this process's calls, not the session's", key)
	}
}
