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
	"testing"

	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/memory"
	"github.com/choria-io/fisk-ai/internal/runstate"
)

// TestInjection_MemoryStoreConflict asserts injecting a memory store that runs on a
// different backend from the one the config names fails at run start, naming both.
func TestInjection_MemoryStoreConflict(t *testing.T) {
	g := NewWithT(t)

	app := agenttest.NewFakeApp(t, exampleApp())
	cfg := agenttest.Config(t, app)
	cfg.Harness.Memory = &config.MemoryConfig{Enabled: true, Backend: "jetstream"}

	_, err := agent.Run(context.Background(), agent.Options{
		Config:      cfg,
		ConfigFile:  "agent.yaml",
		Prompt:      []string{"go"},
		Provider:    agenttest.NewScriptedProvider(t, agenttest.TextResponse("done")),
		MemoryStore: agenttest.NewFakeMemoryStore(t),
	}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))

	g.Expect(err).To(HaveOccurred())
	g.Expect(err).To(MatchError(ContainSubstring(`Options.MemoryStore runs on the "fake" backend`)))
	g.Expect(err).To(MatchError(ContainSubstring(`harness.memory.backend in "agent.yaml" selects "jetstream"`)))
	g.Expect(err.Error()).NotTo(ContainSubstring("connecting to NATS"))
}

// TestInjection_MemoryStoreAgreeing asserts the store the configuration asked for is
// accepted rather than refused for having been injected at all. It is the case a host
// sharing one store across many runs is in, and the reason the rule compares backends
// instead of refusing on presence.
func TestInjection_MemoryStoreAgreeing(t *testing.T) {
	g := NewWithT(t)

	app := agenttest.NewFakeApp(t, exampleApp())
	cfg := agenttest.Config(t, app)
	cfg.Harness.Memory = &config.MemoryConfig{Enabled: true, Backend: "jetstream"}

	store := agenttest.NewFakeMemoryStore(t)
	store.SetInfo(memory.Info{Backend: "jetstream", Location: "MEMORY"})

	res, err := agent.Run(context.Background(), agent.Options{
		Config:      cfg,
		ConfigFile:  "agent.yaml",
		Prompt:      []string{"go"},
		Provider:    agenttest.NewScriptedProvider(t, agenttest.TextResponse("done")),
		MemoryStore: store,
	}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))

	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.Text).To(Equal("done"))
}

// TestInjection_SessionStoreConflict asserts the same refusal for the session seam.
func TestInjection_SessionStoreConflict(t *testing.T) {
	g := NewWithT(t)

	app := agenttest.NewFakeApp(t, exampleApp())
	cfg := agenttest.Config(t, app)
	cfg.Harness.Sessions = &config.SessionConfig{Backend: "jetstream"}

	_, err := agent.Run(context.Background(), agent.Options{
		Config:       cfg,
		ConfigFile:   "agent.yaml",
		Prompt:       []string{"go"},
		Provider:     agenttest.NewScriptedProvider(t, agenttest.TextResponse("done")),
		Checkpoint:   agent.Checkpoint{Enabled: true},
		SessionStore: agenttest.NewFakeSessionStore(t),
	}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))

	g.Expect(err).To(HaveOccurred())
	g.Expect(err).To(MatchError(ContainSubstring(`Options.SessionStore runs on the "fake" backend`)))
	g.Expect(err).To(MatchError(ContainSubstring(`harness.sessions.backend in "agent.yaml" selects "jetstream"`)))
	g.Expect(err.Error()).NotTo(ContainSubstring("connecting to NATS"))
}

// TestInjection_SessionStoreAgreeing asserts the accepting case for the session seam,
// which is what lets a worker share one store across every job it runs.
func TestInjection_SessionStoreAgreeing(t *testing.T) {
	g := NewWithT(t)

	app := agenttest.NewFakeApp(t, exampleApp())
	cfg := agenttest.Config(t, app)
	cfg.Harness.Sessions = &config.SessionConfig{Backend: "jetstream"}

	store := agenttest.NewFakeSessionStore(t)
	store.SetInfo(runstate.Info{Backend: "jetstream", Location: "SESSIONS"})

	res, err := agent.Run(context.Background(), agent.Options{
		Config:       cfg,
		ConfigFile:   "agent.yaml",
		Prompt:       []string{"go"},
		Provider:     agenttest.NewScriptedProvider(t, agenttest.TextResponse("done")),
		Checkpoint:   agent.Checkpoint{Enabled: true},
		SessionStore: store,
	}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))

	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.Text).To(Equal("done"))
}

// TestInjection_UndeclaredBackendTakesAnyStore asserts a configuration that names no
// backend accepts whatever it is given. The default is not a declaration: nothing was
// asked for, so nothing conflicts, which is what keeps an embedder free to supply a
// store of its own without editing the operator's file.
func TestInjection_UndeclaredBackendTakesAnyStore(t *testing.T) {
	g := NewWithT(t)

	app := agenttest.NewFakeApp(t, exampleApp())
	cfg := agenttest.Config(t, app)
	cfg.Harness.Memory = &config.MemoryConfig{Enabled: true}

	store := agenttest.NewFakeMemoryStore(t)
	store.SetInfo(memory.Info{Backend: "jetstream", Location: "MEMORY"})

	res, err := agent.Run(context.Background(), agent.Options{
		Config:      cfg,
		ConfigFile:  "agent.yaml",
		Prompt:      []string{"go"},
		Provider:    agenttest.NewScriptedProvider(t, agenttest.TextResponse("done")),
		MemoryStore: store,
	}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))

	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.Text).To(Equal("done"))
}
