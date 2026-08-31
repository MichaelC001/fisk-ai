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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/runstate"
	runstatefile "github.com/choria-io/fisk-ai/internal/runstate/file"
)

var _ = Describe("the conversation token", func() {
	// This is the path the a2a prompts channel takes. The first turn names a journal that
	// does not exist yet and hands over the token it minted, and every later turn resumes a
	// journal that already holds it, so a second turn arriving from somewhere else cannot
	// rewrite either value.
	It("Should be recorded where the journal is created", func() {
		ctx := context.Background()

		store, err := runstatefile.NewFileStore(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())

		opts := func(prompt string, cp agent.Checkpoint) agent.Options {
			return agent.Options{
				Config:       agenttest.Config(GinkgoTB(), app),
				ConfigFile:   "agent.yaml",
				Prompt:       []string{prompt},
				Provider:     agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("there are three streams")),
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
		}), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())

		rs, err := store.Load(context.Background(), session)
		Expect(err).NotTo(HaveOccurred())
		Expect(rs.ConversationToken).To(Equal(token))
		Expect(rs.Caller).To(Equal("peer1"))

		// A second turn of the same conversation, served by a worker that was told a
		// different caller. The journal keeps what it was created with.
		_, err = agent.Run(ctx, opts("what is the first one called", agent.Checkpoint{
			ResumeID: session,
			FollowUp: true,
			Caller:   "peer2",
		}), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())

		rs, err = store.Load(context.Background(), session)
		Expect(err).NotTo(HaveOccurred())
		Expect(rs.ConversationToken).To(Equal(token))
		Expect(rs.Caller).To(Equal("peer1"))
	})

	// This holds the three checkpoints that create no journal. A dropped token is the
	// failure the field exists to prevent, since nothing else writes it down and the
	// conversation it names is then unreachable, so each is refused before anything runs.
	It("Should be refused where nothing would record it", func() {
		ctx := context.Background()

		store, err := runstatefile.NewFileStore(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())

		opts := func(cp agent.Checkpoint) agent.Options {
			return agent.Options{
				Config:       agenttest.Config(GinkgoTB(), app),
				ConfigFile:   "agent.yaml",
				Prompt:       []string{"carry on"},
				Provider:     agenttest.NewScriptedProvider(GinkgoTB()),
				SessionStore: store,
				Checkpoint:   cp,
			}
		}

		const token = "3Hzmp8VqrKL42NmXcPd7bTgWfR1"

		_, err = agent.Run(ctx, opts(agent.Checkpoint{ResumeID: "conv-1", FollowUp: true, ConversationToken: token}),
			agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).To(MatchError(ContainSubstring("Checkpoint.FollowUp")))

		_, err = agent.Run(ctx, opts(agent.Checkpoint{
			ResumeID:          "conv-1",
			Answer:            &agent.DeferredAnswer{ToolUseID: "c1", Content: "done"},
			ConversationToken: token,
		}), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).To(MatchError(ContainSubstring("Checkpoint.Answer")))

		_, err = agent.Run(ctx, opts(agent.Checkpoint{ResumeID: "conv-1", ConversationToken: token}),
			agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).To(MatchError(ContainSubstring("creates a journal")))
	})

	// This holds what a rotation must not do. A channel names a journal by hashing the
	// token, so the journal a reset rotates to is not one any caller reaches with it.
	// Copying the token would put two conversations in a session listing claiming the same
	// one, only one of which can be continued.
	It("Should not be carried by a context reset", func() {
		ctx := context.Background()

		store, err := runstatefile.NewFileStore(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())

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
			Config:       agenttest.Config(GinkgoTB(), app),
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
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonSuspended))

		// The journal the reset rotated to. Who asked did not change, so the caller is
		// carried and the token is not.
		rs, err := store.Load(context.Background(), res.SessionID)
		Expect(err).NotTo(HaveOccurred())
		Expect(rs.ConversationToken).To(BeEmpty())
		Expect(rs.Caller).To(Equal("peer1"))
	})
})
