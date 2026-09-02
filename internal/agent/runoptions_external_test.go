//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// These tests drive the two arguments agent.Run cannot do without through the
// exported API, so a caller that leaves one out is told which one rather than being
// handed a crash report.
package agent_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
)

var _ = Describe("Run's required arguments", func() {
	It("Should refuse a nil Options.Config", func() {
		res, err := agent.Run(context.Background(), agent.Options{}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).To(MatchError(ContainSubstring("Options.Config is required")))
		Expect(res).ToNot(BeNil())
	})

	It("Should refuse a nil events sink", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), emptyFiskApp())

		opts := agent.Options{
			Config:     agenttest.Config(GinkgoTB(), app),
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			Provider:   agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
		}

		res, err := agent.Run(context.Background(), opts, nil, agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).To(MatchError(ContainSubstring("events is required")))
		Expect(res).ToNot(BeNil())
	})
})
