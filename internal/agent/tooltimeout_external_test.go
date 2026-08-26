//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// These tests exercise the per-call tool bound through the exported agent.Run API,
// using an injected custom tool as the thing being bounded. The bound is cooperative,
// so every tool here observes its context; a handler that ignored one would hang the
// test rather than fail it, which is the property under test stated as a test.
package agent_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/toolkit/functool"
)

// blockingTool returns a custom tool that waits for its context to end, or for fallback
// to elapse, and reports which happened. A tool the bound stopped never returns its
// answer to the model, so the answer is only observable when the bound did not apply.
func blockingTool(tb testing.TB, name string, fallback time.Duration, operatorPaced bool) toolkit.Tool {
	tb.Helper()

	tool, err := functool.New(functool.Spec{
		Name:          name,
		Description:   "waits",
		Schema:        map[string]any{"type": "object"},
		OperatorPaced: operatorPaced,
		Handler: func(ctx context.Context, _ json.RawMessage, _ *functool.CallContext) (string, error) {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(fallback):
				return `{"waited":true}`, nil
			}
		},
	})
	Expect(err).NotTo(HaveOccurred())

	return tool
}

// runWithTool runs one tool call followed by a terminal answer, so the test can see
// both what the tool call produced and whether the loop carried on past it.
func runWithTool(tb testing.TB, tool toolkit.Tool, opts ...agenttest.ConfigOption) (*agent.Result, *agenttest.RecordingEvents, *agenttest.ScriptedProvider, error) {
	tb.Helper()

	app := agenttest.NewFakeApp(tb, emptyFiskApp())
	provider := agenttest.NewScriptedProvider(tb,
		agenttest.ToolUseResponse("c1", tool.Name(), json.RawMessage(`{}`)),
		agenttest.TextResponse("done"),
	)
	events := agenttest.NewRecordingEvents()

	res, err := agent.Run(context.Background(), agent.Options{
		Config:      agenttest.Config(tb, app, opts...),
		ConfigFile:  "agent.yaml",
		Prompt:      []string{"go"},
		Provider:    provider,
		CustomTools: []toolkit.Tool{tool},
	}, events, agenttest.NewScriptedPrompter(tb))

	return res, events, provider, err
}

// toolResultContent returns the content of the tool result for id, as the model saw it
// on the request after the call.
func toolResultContent(provider *agenttest.ScriptedProvider, id string) string {
	GinkgoHelper()

	requests := provider.Requests()
	Expect(len(requests)).To(BeNumerically(">=", 2))

	for _, msg := range requests[len(requests)-1].Messages {
		for _, block := range msg.Content {
			if block.ToolResult != nil && block.ToolResult.ToolUseID == id {
				return block.ToolResult.Content
			}
		}
	}

	Expect(false).To(BeTrue(), "no tool result for %q reached the model", id)

	return ""
}

var _ = Describe("the per-call tool timeout", func() {
	// This is the whole point of the bound: a tool that will not answer is stopped, the
	// model is told so in terms it can act on, the operator gets an advisory because on a
	// host with no operator attached the result reaches nobody else, and the run carries on
	// to its next turn rather than ending.
	It("Should stop a runaway call", func() {
		tool := blockingTool(GinkgoTB(), "waits", time.Minute, false)
		res, events, provider, err := runWithTool(GinkgoTB(), tool, agenttest.WithToolTimeout(50*time.Millisecond))

		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

		Expect(events.HasWarning(agent.WarnToolTimeout)).To(BeTrue())

		warning := events.Warnings()[len(events.Warnings())-1]
		Expect(warning.Name).To(Equal("waits"))
		Expect(warning.Err).To(MatchError(ContainSubstring("did not finish within 50ms")))

		// The loop went on to a second turn, and the result the model saw on it names the
		// bound rather than reporting a bare context error.
		Expect(provider.Requests()).To(HaveLen(2))

		answer := toolResultContent(provider, "c1")
		Expect(answer).To(ContainSubstring(`tool "waits" did not finish within 50ms`))
		Expect(answer).To(ContainSubstring("may be incomplete"))
	})

	// This covers the exemption. A tool that waits on a person outlives the bound and
	// answers normally, because bounding it would cancel the operator's question rather
	// than a runaway.
	It("Should leave an operator-paced call alone", func() {
		tool := blockingTool(GinkgoTB(), "asks", 100*time.Millisecond, true)
		res, events, provider, err := runWithTool(GinkgoTB(), tool, agenttest.WithToolTimeout(10*time.Millisecond))

		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))
		Expect(events.HasWarning(agent.WarnToolTimeout)).To(BeFalse())
		Expect(toolResultContent(provider, "c1")).To(Equal(`{"waited":true}`))
	})

	// This pins the terminal's behavior: with nothing configured a tool call carries no
	// deadline of its own, exactly as before the bound existed.
	It("Should leave tools unbounded when nothing is configured", func() {
		var hadDeadline bool
		tool, err := functool.New(functool.Spec{
			Name:        "reports",
			Description: "reports its deadline",
			Schema:      map[string]any{"type": "object"},
			Handler: func(ctx context.Context, _ json.RawMessage, _ *functool.CallContext) (string, error) {
				_, hadDeadline = ctx.Deadline()
				return `{"ok":true}`, nil
			},
		})
		Expect(err).NotTo(HaveOccurred())

		_, events, _, err := runWithTool(GinkgoTB(), tool)
		Expect(err).NotTo(HaveOccurred())
		Expect(hadDeadline).To(BeFalse())
		Expect(events.HasWarning(agent.WarnToolTimeout)).To(BeFalse())
	})

	// This separates the two ways a tool call's context can end. Canceling the run ends the
	// run; it is not reported as a bound that fired, which would tell the model to try
	// something else when there is no next turn.
	It("Should not report run cancellation as a timeout", func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		tool, err := functool.New(functool.Spec{
			Name:        "cancels",
			Description: "cancels the run from inside a call",
			Schema:      map[string]any{"type": "object"},
			Handler: func(hctx context.Context, _ json.RawMessage, _ *functool.CallContext) (string, error) {
				cancel()
				<-hctx.Done()
				return "", hctx.Err()
			},
		})
		Expect(err).NotTo(HaveOccurred())

		app := agenttest.NewFakeApp(GinkgoTB(), emptyFiskApp())
		events := agenttest.NewRecordingEvents()

		_, _ = agent.Run(ctx, agent.Options{
			Config:      agenttest.Config(GinkgoTB(), app, agenttest.WithToolTimeout(time.Minute)),
			ConfigFile:  "agent.yaml",
			Prompt:      []string{"go"},
			Provider:    agenttest.NewScriptedProvider(GinkgoTB(), agenttest.ToolUseResponse("c1", "cancels", json.RawMessage(`{}`)), agenttest.TextResponse("done")),
			CustomTools: []toolkit.Tool{tool},
		}, events, agenttest.NewScriptedPrompter(GinkgoTB()))

		Expect(events.HasWarning(agent.WarnToolTimeout)).To(BeFalse())
	})
})
