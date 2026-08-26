//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// These live in the external agent_test package alongside the examples: they drive
// only agent's exported API, asserting the precedence rule for the injectable memory
// and session seams. An injected store is the store the configuration asks for or it is
// refused, and a configuration that asks for nothing takes whatever it is given.
//
// The refusing cases name a jetstream backend and run with no broker reachable, so the
// failure each catches is the conflict error rather than "connecting to NATS". Because
// the dial gating runs before the conflict checks and skips dialing for an injected
// seam, that they fail on the conflict rather than the dial is also the memory and
// session skip-dial proof.
package agent_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/memory"
	"github.com/choria-io/fisk-ai/internal/runstate"
)

var _ = Describe("injected store precedence", func() {
	// The refusal names both backends, the injected one and the configured one.
	It("Should refuse a memory store that runs on a different backend from the one the config names", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		cfg.Harness.Memory = &config.MemoryConfig{Enabled: true, Backend: "jetstream"}

		_, err := agent.Run(context.Background(), agent.Options{
			Config:      cfg,
			ConfigFile:  "agent.yaml",
			Prompt:      []string{"go"},
			Provider:    agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
			MemoryStore: agenttest.NewFakeMemoryStore(GinkgoTB()),
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))

		Expect(err).To(HaveOccurred())
		Expect(err).To(MatchError(ContainSubstring(`Options.MemoryStore runs on the "fake" backend`)))
		Expect(err).To(MatchError(ContainSubstring(`harness.memory.backend in "agent.yaml" selects "jetstream"`)))
		Expect(err.Error()).NotTo(ContainSubstring("connecting to NATS"))
	})

	// The store the configuration asked for is accepted rather than refused for having
	// been injected at all. It is the case a host sharing one store across many runs is
	// in, and the reason the rule compares backends instead of refusing on presence.
	It("Should accept a memory store the configuration asked for", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		cfg.Harness.Memory = &config.MemoryConfig{Enabled: true, Backend: "jetstream"}

		store := agenttest.NewFakeMemoryStore(GinkgoTB())
		store.SetInfo(memory.Info{Backend: "jetstream", Location: "MEMORY"})

		res, err := agent.Run(context.Background(), agent.Options{
			Config:      cfg,
			ConfigFile:  "agent.yaml",
			Prompt:      []string{"go"},
			Provider:    agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
			MemoryStore: store,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))

		Expect(err).ToNot(HaveOccurred())
		Expect(res.Text).To(Equal("done"))
	})

	It("Should refuse a session store that runs on a different backend from the one the config names", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		cfg.Harness.Sessions = &config.SessionConfig{Backend: "jetstream"}

		_, err := agent.Run(context.Background(), agent.Options{
			Config:       cfg,
			ConfigFile:   "agent.yaml",
			Prompt:       []string{"go"},
			Provider:     agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
			Checkpoint:   agent.Checkpoint{Enabled: true},
			SessionStore: agenttest.NewFakeSessionStore(GinkgoTB()),
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))

		Expect(err).To(HaveOccurred())
		Expect(err).To(MatchError(ContainSubstring(`Options.SessionStore runs on the "fake" backend`)))
		Expect(err).To(MatchError(ContainSubstring(`harness.sessions.backend in "agent.yaml" selects "jetstream"`)))
		Expect(err.Error()).NotTo(ContainSubstring("connecting to NATS"))
	})

	// The accepting case for the session seam, which is what lets a worker share one
	// store across every job it runs.
	It("Should accept a session store the configuration asked for", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		cfg.Harness.Sessions = &config.SessionConfig{Backend: "jetstream"}

		store := agenttest.NewFakeSessionStore(GinkgoTB())
		store.SetInfo(runstate.Info{Backend: "jetstream", Location: "SESSIONS"})

		res, err := agent.Run(context.Background(), agent.Options{
			Config:       cfg,
			ConfigFile:   "agent.yaml",
			Prompt:       []string{"go"},
			Provider:     agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
			Checkpoint:   agent.Checkpoint{Enabled: true},
			SessionStore: store,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))

		Expect(err).ToNot(HaveOccurred())
		Expect(res.Text).To(Equal("done"))
	})

	// The default is not a declaration: nothing was asked for, so nothing conflicts,
	// which is what keeps an embedder free to supply a store of its own without editing
	// the operator's file.
	It("Should take any store when the configuration names no backend", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		cfg.Harness.Memory = &config.MemoryConfig{Enabled: true}

		store := agenttest.NewFakeMemoryStore(GinkgoTB())
		store.SetInfo(memory.Info{Backend: "jetstream", Location: "MEMORY"})

		res, err := agent.Run(context.Background(), agent.Options{
			Config:      cfg,
			ConfigFile:  "agent.yaml",
			Prompt:      []string{"go"},
			Provider:    agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
			MemoryStore: store,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))

		Expect(err).ToNot(HaveOccurred())
		Expect(res.Text).To(Equal("done"))
	})
})
