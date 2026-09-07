//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// These tests drive the agent identity onto a stored run through the exported agent.Run
// API. A store holds no identity of its own, so the run puts the configured one on the
// meta record where the journal is created, and a listing filtered by it is what a rail
// showing one agent's conversations reads.
package agent_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/runstate"
	runstatefile "github.com/choria-io/fisk-ai/internal/runstate/file"
)

var _ = Describe("the agent a session belongs to", func() {
	It("Should stamp the configured identity where the journal is created", func() {
		ctx := context.Background()

		store, err := runstatefile.NewFileStore(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())

		cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), exampleApp()))
		cfg.Identity = "worker-3"

		res, err := agent.Run(ctx, agent.Options{
			Config:       cfg,
			ConfigFile:   "agent.yaml",
			Prompt:       []string{"how many streams are there"},
			Provider:     agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("there are three streams")),
			SessionStore: store,
			Checkpoint:   agent.Checkpoint{Enabled: true},
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())

		rs, err := store.Load(ctx, res.SessionID)
		Expect(err).NotTo(HaveOccurred())
		Expect(rs.Agent).To(Equal("worker-3"))

		infos, err := store.List(ctx, runstate.ListFilter{Agent: "worker-3"})
		Expect(err).NotTo(HaveOccurred())
		Expect(infos).To(HaveLen(1))
		Expect(infos[0].RunID).To(Equal(res.SessionID))
		Expect(infos[0].Agent).To(Equal("worker-3"))

		// Another agent sharing this store lists none of it.
		infos, err = store.List(ctx, runstate.ListFilter{Agent: "worker-4"})
		Expect(err).NotTo(HaveOccurred())
		Expect(infos).To(BeEmpty())
	})

	// A context reset rotates to a fresh journal that the same process writes, so the
	// rotated conversation belongs to the same agent and lists beside the one it came
	// from.
	It("Should stamp the journal a context reset rotates to", func() {
		ctx := context.Background()

		store, err := runstatefile.NewFileStore(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())

		cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), exampleApp()))
		cfg.Identity = "worker-3"

		provider := agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.TextResponse("there are three streams"),
			agenttest.TextResponse("the first is orders"),
		)

		turns := 0
		next := func(context.Context) agent.Continuation {
			turns++
			if turns == 1 {
				return agent.Continuation{Text: "start again", Reset: true, Continue: true}
			}

			return agent.Continuation{Continue: false}
		}

		res, err := agent.Run(ctx, agent.Options{
			Config:       cfg,
			ConfigFile:   "agent.yaml",
			Prompt:       []string{"how many streams are there"},
			Provider:     provider,
			SessionStore: store,
			Checkpoint:   agent.Checkpoint{Enabled: true},
			NextPrompt:   next,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())

		rs, err := store.Load(ctx, res.SessionID)
		Expect(err).NotTo(HaveOccurred())
		Expect(rs.Agent).To(Equal("worker-3"))

		infos, err := store.List(ctx, runstate.ListFilter{Agent: "worker-3"})
		Expect(err).NotTo(HaveOccurred())
		Expect(infos).To(HaveLen(2), "the run it started on and the one the reset rotated to")
	})
})
