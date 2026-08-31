//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/toolkit/functool"
)

// These tests pin the two axes the runner counts a tool call on. Every call, including
// the ones answered before the tool runs, is counted against the provider that supplied
// the tool, so on a fresh run the per-kind buckets partition tool_calls exactly. The
// remote and MCP totals count only the calls that were dispatched, so a denied call is
// in its bucket and in neither total. See RunStats.ToolCallsByKind.
var _ = Describe("runner tool accounting", func() {
	const objSchema = `{"type":"object"}`

	obj := func() map[string]any {
		var m map[string]any
		Expect(json.Unmarshal([]byte(objSchema), &m)).To(Succeed())

		return m
	}

	okHandler := func(context.Context, json.RawMessage, *functool.CallContext) (string, error) {
		return "ok", nil
	}

	mustFunc := func(spec functool.Spec) *functool.Tool {
		GinkgoHelper()
		t, err := functool.New(spec)
		Expect(err).NotTo(HaveOccurred())

		return t
	}

	// newRunner builds a runner whose confirm gate denies by default (its prompter
	// cannot prompt), so a confirm-gated tool exercises the confirm-denied path
	// without an operator.
	newRunner := func(tools map[string]toolkit.Tool) *runner {
		return &runner{
			stats:       &RunStats{},
			events:      &captureEvents{},
			set:         toolSetOf(tools),
			prompter:    toolkit.DefaultDenyPrompter(),
			gate:        NewConfirmGate(toolkit.DefaultDenyPrompter(), nil),
			toolWorkDir: "/run/work",
		}
	}

	customTool := func(name string) *functool.Tool {
		return mustFunc(functool.Spec{Name: name, Description: "custom", Schema: obj(), Handler: okHandler})
	}

	call := func(r *runner, name string) {
		r.executeTool(context.Background(), llm.ToolUseBlock{ID: name, Name: name, Input: json.RawMessage(`{}`)})
	}

	It("Should count each dispatched call against the provider that supplied its tool", func() {
		remote := mustFunc(functool.Spec{Name: "remote_tool", Description: "remote", Schema: obj(), Handler: okHandler, Remote: &functool.RemoteSpec{Agent: "peer"}})
		builtinLike := mustFunc(functool.Spec{Name: "builtin_tool", Description: "builtin", Schema: obj(), Handler: okHandler, Kind: toolkit.KindBuiltin})

		r := newRunner(map[string]toolkit.Tool{
			"custom_tool":  customTool("custom_tool"),
			"remote_tool":  remote,
			"builtin_tool": builtinLike,
		})

		call(r, "custom_tool")
		call(r, "remote_tool")
		call(r, "builtin_tool")

		Expect(r.stats.ToolCalls).To(BeEquivalentTo(3))
		Expect(r.stats.RemoteToolCalls).To(BeEquivalentTo(1))
		Expect(r.stats.ToolCallsByKind).To(Equal(map[toolkit.Kind]int64{
			toolkit.KindCustom:  1,
			toolkit.KindRemote:  1,
			toolkit.KindBuiltin: 1,
		}))
	})

	It("Should count an MCP call under its own kind and leave the remote counter to a2a peers", func() {
		remote := mustFunc(functool.Spec{Name: "remote_tool", Description: "remote", Schema: obj(), Handler: okHandler, Remote: &functool.RemoteSpec{Agent: "peer"}})
		served := mustFunc(functool.Spec{Name: "mcp_tool", Description: "mcp", Schema: obj(), Handler: okHandler, MCP: &functool.MCPSpec{Server: "docs"}})

		r := newRunner(map[string]toolkit.Tool{"remote_tool": remote, "mcp_tool": served})

		call(r, "remote_tool")
		call(r, "mcp_tool")

		Expect(r.stats.ToolCalls).To(BeEquivalentTo(2))
		Expect(r.stats.RemoteToolCalls).To(BeEquivalentTo(1))
		Expect(r.stats.MCPToolCalls).To(BeEquivalentTo(1))
		Expect(r.stats.ToolCallsByKind).To(Equal(map[toolkit.Kind]int64{
			toolkit.KindRemote: 1,
			toolkit.KindMCP:    1,
		}))
	})

	It("Should bucket a denied call under its kind and leave it out of the dispatch counters", func() {
		remote := mustFunc(functool.Spec{Name: "remote_tool", Description: "remote", Schema: obj(), Handler: okHandler, Remote: &functool.RemoteSpec{Agent: "peer"}})
		served := mustFunc(functool.Spec{Name: "mcp_tool", Description: "mcp", Schema: obj(), Handler: okHandler, MCP: &functool.MCPSpec{Server: "docs"}})

		r := newRunner(map[string]toolkit.Tool{"remote_tool": remote, "mcp_tool": served})
		r.hooks = Hooks{
			PreToolUse: func(_ context.Context, in PreToolUseInfo) (PreToolUseResult, error) {
				if in.ToolUseID != "denied" {
					return PreToolUseResult{}, nil
				}

				return PreToolUseResult{Deny: true, DenyReason: "blocked by policy"}, nil
			},
		}

		deny := func(name string) {
			GinkgoHelper()
			_, dispatched, _, err := r.executeTool(context.Background(), llm.ToolUseBlock{ID: "denied", Name: name, Input: json.RawMessage(`{}`)})
			Expect(err).NotTo(HaveOccurred())
			Expect(dispatched).To(BeFalse())
		}

		call(r, "mcp_tool")
		deny("mcp_tool")
		call(r, "remote_tool")
		deny("remote_tool")

		Expect(r.stats.ToolCalls).To(BeEquivalentTo(4))
		Expect(r.stats.MCPToolCalls).To(BeEquivalentTo(1), "the denied MCP call never left the process")
		Expect(r.stats.RemoteToolCalls).To(BeEquivalentTo(1), "the denied remote call never left the process")
		Expect(r.stats.ToolCallsByKind).To(Equal(map[toolkit.Kind]int64{
			toolkit.KindMCP:    2,
			toolkit.KindRemote: 2,
		}))

		var summed int64
		for _, n := range r.stats.ToolCallsByKind {
			summed += n
		}
		Expect(summed).To(Equal(r.stats.ToolCalls), "per-kind buckets must partition tool_calls")
	})

	// The kind goes into the journal so a resume recomputes every bucket. It is the
	// effective one, so a call a hook redirected is journaled under the provider that
	// served it rather than the one the model named.
	It("Should report the effective kind of each call for the journal", func() {
		served := mustFunc(functool.Spec{Name: "mcp_tool", Description: "mcp", Schema: obj(), Handler: okHandler, MCP: &functool.MCPSpec{Server: "docs"}})
		builtinLike := mustFunc(functool.Spec{Name: "builtin_tool", Description: "builtin", Schema: obj(), Handler: okHandler, Kind: toolkit.KindBuiltin})

		r := newRunner(map[string]toolkit.Tool{"mcp_tool": served, "builtin_tool": builtinLike})
		r.hooks = Hooks{
			PreToolUse: func(_ context.Context, in PreToolUseInfo) (PreToolUseResult, error) {
				if in.ToolName != "builtin_tool" {
					return PreToolUseResult{}, nil
				}

				return PreToolUseResult{RewriteTool: "mcp_tool"}, nil
			},
		}

		_, _, kind, err := r.executeTool(context.Background(), llm.ToolUseBlock{ID: "t1", Name: "mcp_tool", Input: json.RawMessage(`{}`)})
		Expect(err).NotTo(HaveOccurred())
		Expect(kind).To(Equal(toolkit.KindMCP))

		_, _, kind, err = r.executeTool(context.Background(), llm.ToolUseBlock{ID: "t2", Name: "builtin_tool", Input: json.RawMessage(`{}`)})
		Expect(err).NotTo(HaveOccurred())
		Expect(kind).To(Equal(toolkit.KindMCP), "a rewritten call is accounted under the tool that ran")

		_, _, kind, err = r.executeTool(context.Background(), llm.ToolUseBlock{ID: "t3", Name: "absent_tool", Input: json.RawMessage(`{}`)})
		Expect(err).NotTo(HaveOccurred())
		Expect(kind).To(Equal(toolkit.KindUnknown))
	})

	It("Should count the rejected paths so the buckets still partition tool_calls", func() {
		needy := mustFunc(functool.Spec{
			Name: "needy_tool", Description: "needs an arg", Handler: okHandler,
			Schema:           map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string"}}, "required": []any{"x"}},
			ValidateRequired: true,
		})
		gated := mustFunc(functool.Spec{Name: "gated_tool", Description: "gated", Schema: obj(), Handler: okHandler, Confirm: &functool.ConfirmSpec{}})

		r := newRunner(map[string]toolkit.Tool{
			"custom_tool": customTool("custom_tool"),
			"needy_tool":  needy,
			"gated_tool":  gated,
		})

		call(r, "custom_tool") // runs
		call(r, "needy_tool")  // rejected: missing required argument
		call(r, "gated_tool")  // rejected: confirm gate denies
		call(r, "absent_tool") // rejected: not in the registry

		Expect(r.stats.ToolCalls).To(BeEquivalentTo(4))

		var summed int64
		for _, n := range r.stats.ToolCallsByKind {
			summed += n
		}
		Expect(summed).To(Equal(r.stats.ToolCalls), "per-kind buckets must partition tool_calls")

		Expect(r.stats.ToolCallsByKind).To(Equal(map[toolkit.Kind]int64{
			toolkit.KindCustom:  3, // the run, the missing-arg reject, the confirm-denied reject
			toolkit.KindUnknown: 1, // the tool that is not in the registry
		}))
	})

	It("Should leave no unknown bucket for a run of tools that all declare a provider", func() {
		r := newRunner(map[string]toolkit.Tool{"custom_tool": customTool("custom_tool")})

		call(r, "custom_tool")

		Expect(r.stats.ToolCallsByKind).NotTo(HaveKey(toolkit.KindUnknown))
	})
})
