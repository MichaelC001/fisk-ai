//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agent_test

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/runstate"
)

// TestReasoningEffort_ReachesEveryRequest drives the whole path the key exists for:
// the configuration names a level, and every model call the run makes carries it.
// Which levels a model takes is the model's own list, so a level this build does not
// name travels unchanged and is refused by the provider rather than here.
func TestReasoningEffort_ReachesEveryRequest(t *testing.T) {
	g := NewWithT(t)

	app := agenttest.NewFakeApp(t, exampleApp())
	provider := agenttest.NewScriptedProvider(t,
		agenttest.ToolUseResponse("c1", "do", []byte(`{"subject":"hello"}`)),
		agenttest.TextResponse("done"),
	)

	cfg := agenttest.Config(t, app)
	cfg.LLM.ReasoningEffort = "ludicrous"

	res, err := agent.Run(context.Background(), agent.Options{
		Config:     cfg,
		ConfigFile: "agent.yaml",
		Prompt:     []string{"do the thing"},
		Provider:   provider,
	}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

	reqs := provider.Requests()
	g.Expect(reqs).To(HaveLen(2))
	for i, req := range reqs {
		g.Expect(req.ReasoningEffort).To(Equal("ludicrous"), "request %d", i)
	}
}

// TestReasoningEffort_AbsentAsksForNothing is the state that must stay silent: a
// configuration naming no level sends none, so a model that rejects the parameter is
// unaffected by the key existing.
func TestReasoningEffort_AbsentAsksForNothing(t *testing.T) {
	g := NewWithT(t)

	app := agenttest.NewFakeApp(t, exampleApp())
	provider := agenttest.NewScriptedProvider(t, agenttest.TextResponse("done"))

	_, err := agent.Run(context.Background(), agent.Options{
		Config:     agenttest.Config(t, app),
		ConfigFile: "agent.yaml",
		Prompt:     []string{"do the thing"},
		Provider:   provider,
	}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(provider.Requests()).To(HaveLen(1))
	g.Expect(provider.Requests()[0].ReasoningEffort).To(BeEmpty())
}
