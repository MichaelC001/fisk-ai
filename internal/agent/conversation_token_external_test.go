//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// These tests drive the two values a channel records about a conversation through the
// exported agent.Run API: the token a caller holds for it and the name the channel was
// given for who asked. Both are written where the journal is created and read by
// nothing, so what is asserted is where they land and where they are refused.
package agent_test

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/runstate"
	runstatefile "github.com/choria-io/fisk-ai/internal/runstate/file"
)

// TestConversationToken_RecordedWhereTheJournalIsCreated is the path the a2a prompts
// channel takes. The first turn names a journal that does not exist yet and hands over
// the token it minted, and every later turn resumes a journal that already holds it, so
// a second turn arriving from somewhere else cannot rewrite either value.
func TestConversationToken_RecordedWhereTheJournalIsCreated(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	store, err := runstatefile.NewFileStore(t.TempDir())
	g.Expect(err).NotTo(HaveOccurred())

	app := agenttest.NewFakeApp(t, exampleApp())

	opts := func(prompt string, cp agent.Checkpoint) agent.Options {
		return agent.Options{
			Config:       agenttest.Config(t, app),
			ConfigFile:   "agent.yaml",
			Prompt:       []string{prompt},
			Provider:     agenttest.NewScriptedProvider(t, agenttest.TextResponse("there are three streams")),
			SessionStore: store,
			Checkpoint:   cp,
		}
	}

	const session = "t-e3b0c44298fc1c149afbf4c8996fb924"
	const token = "3Hzmp8VqrKL42NmXcPd7bTgWfR1"

	_, err = agent.Run(ctx, opts("how many streams are there", agent.Checkpoint{
		ResumeID:          session,
		CreateIfMissing:   true,
		ConversationToken: token,
		Caller:            "peer1",
	}), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).NotTo(HaveOccurred())

	rs, err := store.Load(session)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rs.ConversationToken).To(Equal(token))
	g.Expect(rs.Caller).To(Equal("peer1"))

	// A second turn of the same conversation, served by a worker that was told a
	// different caller. The journal keeps what it was created with.
	_, err = agent.Run(ctx, opts("what is the first one called", agent.Checkpoint{
		ResumeID: session,
		FollowUp: true,
		Caller:   "peer2",
	}), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).NotTo(HaveOccurred())

	rs, err = store.Load(session)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rs.ConversationToken).To(Equal(token))
	g.Expect(rs.Caller).To(Equal("peer1"))
}

// TestConversationToken_RefusedWhereNothingWouldRecordIt holds the three checkpoints
// that create no journal. A dropped token is the failure the field exists to prevent,
// since nothing else writes it down and the conversation it names is then unreachable,
// so each is refused before anything runs.
func TestConversationToken_RefusedWhereNothingWouldRecordIt(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	store, err := runstatefile.NewFileStore(t.TempDir())
	g.Expect(err).NotTo(HaveOccurred())

	app := agenttest.NewFakeApp(t, exampleApp())

	opts := func(cp agent.Checkpoint) agent.Options {
		return agent.Options{
			Config:       agenttest.Config(t, app),
			ConfigFile:   "agent.yaml",
			Prompt:       []string{"carry on"},
			Provider:     agenttest.NewScriptedProvider(t),
			SessionStore: store,
			Checkpoint:   cp,
		}
	}

	const token = "3Hzmp8VqrKL42NmXcPd7bTgWfR1"

	_, err = agent.Run(ctx, opts(agent.Checkpoint{ResumeID: "conv-1", FollowUp: true, ConversationToken: token}),
		agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).To(MatchError(ContainSubstring("Checkpoint.FollowUp")))

	_, err = agent.Run(ctx, opts(agent.Checkpoint{
		ResumeID:          "conv-1",
		Answer:            &agent.DeferredAnswer{ToolUseID: "c1", Content: "done"},
		ConversationToken: token,
	}), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).To(MatchError(ContainSubstring("Checkpoint.Answer")))

	_, err = agent.Run(ctx, opts(agent.Checkpoint{ResumeID: "conv-1", ConversationToken: token}),
		agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).To(MatchError(ContainSubstring("creates a journal")))
}

// TestConversationToken_NotCarriedByAContextReset holds what a rotation must not do. A
// channel names a journal by hashing the token, so the journal a reset rotates to is
// not one any caller reaches with it. Copying the token would put two conversations in
// a session listing claiming the same one, only one of which can be continued.
func TestConversationToken_NotCarriedByAContextReset(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	store, err := runstatefile.NewFileStore(t.TempDir())
	g.Expect(err).NotTo(HaveOccurred())

	app := agenttest.NewFakeApp(t, exampleApp())

	provider := agenttest.NewScriptedProvider(t,
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
		Config:       agenttest.Config(t, app),
		ConfigFile:   "agent.yaml",
		Prompt:       []string{"how many streams are there"},
		Provider:     provider,
		SessionStore: store,
		Checkpoint: agent.Checkpoint{
			Enabled:           true,
			ConversationToken: "3Hzmp8VqrKL42NmXcPd7bTgWfR1",
			Caller:            "peer1",
		},
		NextPrompt: next,
	}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.Reason).To(Equal(runstate.ReasonSuspended))

	// The journal the reset rotated to. Who asked did not change, so the caller is
	// carried and the token is not.
	rs, err := store.Load(res.SessionID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rs.ConversationToken).To(BeEmpty())
	g.Expect(rs.Caller).To(Equal("peer1"))
}
