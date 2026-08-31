//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// These tests drive follow-up turns through the exported agent.Run API: a resumed run
// that is handed a new user turn, where in the conversation that turn lands, and what
// happens when the conversation cannot take one.
package agent_test

import (
	"context"
	"encoding/json"
	"sync/atomic"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
	runstatefile "github.com/choria-io/fisk-ai/internal/runstate/file"
	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// carriesUserText reports whether any user message in a conversation carries text, so
// a spec can assert which model call a follow-up turn reached.
func carriesUserText(msgs []llm.Message, text string) bool {
	for _, m := range msgs {
		if m.Role != llm.RoleUser {
			continue
		}
		for _, b := range m.Content {
			if b.Text != nil && b.Text.Text == text {
				return true
			}
		}
	}

	return false
}

var _ = Describe("a follow-up turn", func() {
	// This is the path every turn of a served conversation takes. A one-shot turn ends by
	// answering, which seals nothing: the next turn resumes that journal, its prompt enters
	// the conversation before any model call, and both turns fold into one conversation in
	// order.
	//
	// It also holds the two resumes that are not follow-ups: without the flag a completed
	// run is still refused, and under CreateIfMissing it is still answered from the journal,
	// which is what keeps a queue redelivery idempotent.
	It("Should continue a completed conversation", func() {
		ctx := context.Background()

		store, err := runstatefile.NewFileStore(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())

		opts := func(provider *agenttest.ScriptedProvider, prompt string, cp agent.Checkpoint) agent.Options {
			return agent.Options{
				Config:       agenttest.Config(GinkgoTB(), app),
				ConfigFile:   "agent.yaml",
				Prompt:       []string{prompt},
				Provider:     provider,
				SessionStore: store,
				Checkpoint:   cp,
			}
		}

		res1, err := agent.Run(ctx, opts(
			agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("there are three streams")),
			"how many streams are there",
			agent.Checkpoint{Enabled: true},
		), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res1.Reason).To(Equal(runstate.ReasonCompleted))
		Expect(res1.FollowUpTaken).To(BeFalse())

		// The second turn. The conversation rests where a user message may be added, so the
		// prompt is in the first request rather than after a model call nobody asked for.
		second := agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("the first one is ORDERS"))
		res2, err := agent.Run(ctx, opts(second, "what is the first one called",
			agent.Checkpoint{ResumeID: res1.SessionID, FollowUp: true},
		), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res2.Reason).To(Equal(runstate.ReasonCompleted))
		Expect(res2.FollowUpTaken).To(BeTrue())
		Expect(res2.Text).To(Equal("the first one is ORDERS"))

		Expect(second.Requests()).To(HaveLen(1))
		Expect(carriesUserText(second.Requests()[0].Messages, "what is the first one called")).To(BeTrue())

		// One conversation, both turns, in the order they were asked.
		rs, err := store.Load(context.Background(), res1.SessionID)
		Expect(err).NotTo(HaveOccurred())
		Expect(rs.Messages).To(HaveLen(4))
		Expect(rs.Messages[0].Role).To(Equal(llm.RoleUser))
		Expect(rs.Messages[1].Role).To(Equal(llm.RoleAssistant))
		Expect(rs.Messages[2].Role).To(Equal(llm.RoleUser))
		Expect(rs.Messages[3].Role).To(Equal(llm.RoleAssistant))
		Expect(carriesUserText(rs.Messages, "how many streams are there")).To(BeTrue())
		Expect(carriesUserText(rs.Messages, "what is the first one called")).To(BeTrue())

		// A resume with nothing to add is still refused: the model would be called on a
		// finished conversation with no new input.
		_, err = agent.Run(ctx, opts(agenttest.NewScriptedProvider(GinkgoTB()), "",
			agent.Checkpoint{ResumeID: res1.SessionID},
		), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).To(MatchError(ContainSubstring("has already completed")))

		// An at-least-once caller redelivering finished work still gets the stored answer,
		// with no model call, which is the branch a follow-up must not reach.
		replay := agenttest.NewScriptedProvider(GinkgoTB())
		res4, err := agent.Run(ctx, opts(replay, "what is the first one called",
			agent.Checkpoint{ResumeID: res1.SessionID, CreateIfMissing: true},
		), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res4.Text).To(Equal("the first one is ORDERS"))
		Expect(replay.Requests()).To(BeEmpty())
	})

	// This is the ordering rule that keeps a follow-up out of a conversation the model is
	// part way through.
	//
	// A turn whose tool results were all journaled but never answered leaves the
	// conversation ending on a user message with nothing pending, which is what a provider
	// error at the turn boundary, the iteration cap and a crash inside a model call all
	// produce. Delivering into it would fold the new prompt into those results and the
	// model would answer the previous prompt. So the loop makes that call first and the
	// follow-up is the turn after it.
	It("Should finish an interrupted turn first", func() {
		ctx := context.Background()

		store, err := runstatefile.NewFileStore(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		var calls atomic.Int64
		tool := countingTool(GinkgoTB(), "record_note", &calls)

		opts := func(provider *agenttest.ScriptedProvider, prompt string, cp agent.Checkpoint) agent.Options {
			return agent.Options{
				Config:       agenttest.Config(GinkgoTB(), app),
				ConfigFile:   "agent.yaml",
				Prompt:       []string{prompt},
				Provider:     provider,
				SessionStore: store,
				Checkpoint:   cp,
				CustomTools:  []toolkit.Tool{tool},
			}
		}

		// The tool runs and its result is journaled; the model call that would have read it
		// never happens, because the script has no second response.
		res1, err := agent.Run(ctx, opts(
			agenttest.NewScriptedProvider(GinkgoTB(), agenttest.ToolUseResponse("c1", "record_note", json.RawMessage(`{}`))),
			"note the outage",
			agent.Checkpoint{Enabled: true},
		), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).To(HaveOccurred())
		Expect(res1.Reason).To(Equal(runstate.ReasonError))
		Expect(calls.Load()).To(Equal(int64(1)))

		rs, err := store.Load(context.Background(), res1.SessionID)
		Expect(err).NotTo(HaveOccurred())
		Expect(rs.Pending).To(BeNil())
		Expect(rs.Messages[len(rs.Messages)-1].Role).To(Equal(llm.RoleUser))

		// The follow-up arrives. The first call answers the interrupted turn and carries no
		// follow-up; the second is the follow-up's own turn.
		second := agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.TextResponse("the outage is noted"),
			agenttest.TextResponse("nothing else is affected"),
		)
		res2, err := agent.Run(ctx, opts(second, "is anything else affected",
			agent.Checkpoint{ResumeID: res1.SessionID, FollowUp: true},
		), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res2.Reason).To(Equal(runstate.ReasonCompleted))
		Expect(res2.FollowUpTaken).To(BeTrue())
		Expect(res2.Text).To(Equal("nothing else is affected"))

		Expect(second.Requests()).To(HaveLen(2))
		Expect(carriesUserText(second.Requests()[0].Messages, "is anything else affected")).To(BeFalse())
		Expect(carriesUserText(second.Requests()[1].Messages, "is anything else affected")).To(BeTrue())
		Expect(calls.Load()).To(Equal(int64(1)))
	})

	// This holds the default every existing caller depends on: a resume replaces its
	// conversation with the journaled one and the prompt it was given is not delivered. A
	// queue redelivery re-runs the interrupted work rather than appending its prompt a
	// second time.
	It("Should be discarded without the flag", func() {
		ctx := context.Background()

		store, err := runstatefile.NewFileStore(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		var calls atomic.Int64
		tool := countingTool(GinkgoTB(), "record_note", &calls)

		opts := func(provider *agenttest.ScriptedProvider, cp agent.Checkpoint) agent.Options {
			return agent.Options{
				Config:       agenttest.Config(GinkgoTB(), app),
				ConfigFile:   "agent.yaml",
				Prompt:       []string{"note the outage"},
				Provider:     provider,
				SessionStore: store,
				Checkpoint:   cp,
				CustomTools:  []toolkit.Tool{tool},
			}
		}

		res1, err := agent.Run(ctx, opts(
			agenttest.NewScriptedProvider(GinkgoTB(), agenttest.ToolUseResponse("c1", "record_note", json.RawMessage(`{}`))),
			agent.Checkpoint{ResumeID: "job-1", CreateIfMissing: true},
		), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).To(HaveOccurred())
		Expect(res1.Reason).To(Equal(runstate.ReasonError))

		second := agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("the outage is noted"))
		res2, err := agent.Run(ctx, opts(second, agent.Checkpoint{ResumeID: "job-1", CreateIfMissing: true}),
			agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res2.Reason).To(Equal(runstate.ReasonCompleted))
		Expect(res2.FollowUpTaken).To(BeFalse())

		// The prompt was handed over twice and entered the conversation once, as the first
		// delivery's own first prompt.
		Expect(second.Requests()).To(HaveLen(1))
		Expect(second.Requests()[0].Messages).To(HaveLen(3))
	})

	// This covers the one state that reaches no boundary at all. A conversation waiting on
	// a deferred tool result suspends without committing its turn, so there is nowhere to
	// put a user message: the prompt is neither journaled nor answered, and the caller is
	// told so rather than having it silently dropped.
	It("Should not be taken into a deferred conversation", func() {
		ctx := context.Background()

		store, err := runstatefile.NewFileStore(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		var calls atomic.Int64
		tool := deferringTool(GinkgoTB(), "change_request", &calls)

		opts := func(provider *agenttest.ScriptedProvider, prompt string, cp agent.Checkpoint) agent.Options {
			return agent.Options{
				Config:       agenttest.Config(GinkgoTB(), app),
				ConfigFile:   "agent.yaml",
				Prompt:       []string{prompt},
				Provider:     provider,
				SessionStore: store,
				Checkpoint:   cp,
				CustomTools:  []toolkit.Tool{tool},
			}
		}

		res1, err := agent.Run(ctx, opts(
			agenttest.NewScriptedProvider(GinkgoTB(), agenttest.ToolUseResponse("c1", "change_request", json.RawMessage(`{}`))),
			"raise a change",
			agent.Checkpoint{Enabled: true},
		), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res1.Reason).To(Equal(runstate.ReasonSuspended))
		Expect(res1.Deferred).To(HaveLen(1))

		// The provider carries no responses, so any model call fails the spec.
		second := agenttest.NewScriptedProvider(GinkgoTB())
		res2, err := agent.Run(ctx, opts(second, "make it urgent",
			agent.Checkpoint{ResumeID: res1.SessionID, FollowUp: true},
		), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res2.Reason).To(Equal(runstate.ReasonSuspended))
		Expect(res2.FollowUpTaken).To(BeFalse())
		Expect(second.Requests()).To(BeEmpty())

		rs, err := store.Load(context.Background(), res1.SessionID)
		Expect(err).NotTo(HaveOccurred())
		Expect(carriesUserText(rs.Messages, "make it urgent")).To(BeFalse())
	})

	// This holds the two combinations that cost money to get wrong, and the empty prompt
	// that has no turn in it. Each is refused before anything runs rather than being
	// documented and left to a caller.
	It("Should refuse the option combinations that have no turn in them", func() {
		ctx := context.Background()

		store, err := runstatefile.NewFileStore(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())

		opts := func(prompt string, cp agent.Checkpoint) agent.Options {
			return agent.Options{
				Config:       agenttest.Config(GinkgoTB(), app),
				ConfigFile:   "agent.yaml",
				Prompt:       []string{prompt},
				Provider:     agenttest.NewScriptedProvider(GinkgoTB()),
				SessionStore: store,
				Checkpoint:   cp,
			}
		}

		_, err = agent.Run(ctx, opts("carry on", agent.Checkpoint{FollowUp: true}),
			agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).To(MatchError(ContainSubstring("Checkpoint.ResumeID")))

		_, err = agent.Run(ctx, opts("carry on", agent.Checkpoint{ResumeID: "conv-1", FollowUp: true, CreateIfMissing: true}),
			agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).To(MatchError(ContainSubstring("Checkpoint.CreateIfMissing")))

		_, err = agent.Run(ctx, opts("  ", agent.Checkpoint{ResumeID: "conv-1", FollowUp: true}),
			agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).To(MatchError(ContainSubstring("Options.Prompt")))
	})

	// This separates the caller's mistake from a fault of this process: a follow-up naming
	// a session the store does not hold is answerable, and a channel needs a value rather
	// than prose to answer it with.
	It("Should report an unknown conversation as its own error", func() {
		ctx := context.Background()

		store, err := runstatefile.NewFileStore(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())

		_, err = agent.Run(ctx, agent.Options{
			Config:       agenttest.Config(GinkgoTB(), app),
			ConfigFile:   "agent.yaml",
			Prompt:       []string{"carry on"},
			Provider:     agenttest.NewScriptedProvider(GinkgoTB()),
			SessionStore: store,
			Checkpoint:   agent.Checkpoint{ResumeID: "gone", FollowUp: true},
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).To(MatchError(agent.ErrConversationNotFound))
		Expect(err).To(MatchError(runstate.ErrNotFound))
	})

	// This covers the limit that would otherwise run out under a conversation. The cap is
	// an absolute loop position, so without a fresh turn's worth per follow-up the third
	// turn of a conversation starts past a cap that was set for the first.
	It("Should give each turn a fresh iteration budget", func() {
		ctx := context.Background()

		store, err := runstatefile.NewFileStore(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())

		turn := func(prompt string, cp agent.Checkpoint) *agent.Result {
			res, rerr := agent.Run(ctx, agent.Options{
				Config:       agenttest.Config(GinkgoTB(), app, agenttest.WithMaxIterations(1)),
				ConfigFile:   "agent.yaml",
				Prompt:       []string{prompt},
				Provider:     agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("answered "+prompt)),
				SessionStore: store,
				Checkpoint:   cp,
			}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
			Expect(rerr).NotTo(HaveOccurred())
			Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

			return res
		}

		first := turn("one", agent.Checkpoint{Enabled: true})
		turn("two", agent.Checkpoint{ResumeID: first.SessionID, FollowUp: true})
		third := turn("three", agent.Checkpoint{ResumeID: first.SessionID, FollowUp: true})
		Expect(third.FollowUpTaken).To(BeTrue())

		rs, err := store.Load(context.Background(), first.SessionID)
		Expect(err).NotTo(HaveOccurred())
		Expect(rs.Messages).To(HaveLen(6))
		Expect(rs.NextIteration).To(Equal(int64(3)))
	})

	// This proves the hook that filters a chat's follow-ups also filters one a resume was
	// handed, and that a denial records nothing: the prompt is not journaled, so the
	// conversation is what it was before the turn arrived.
	It("Should be denied by a policy hook", func() {
		ctx := context.Background()

		store, err := runstatefile.NewFileStore(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())

		res1, err := agent.Run(ctx, agent.Options{
			Config:       agenttest.Config(GinkgoTB(), app),
			ConfigFile:   "agent.yaml",
			Prompt:       []string{"how many streams are there"},
			Provider:     agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("there are three streams")),
			SessionStore: store,
			Checkpoint:   agent.Checkpoint{Enabled: true},
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())

		var seen []agent.UserPromptSubmitInfo
		second := agenttest.NewScriptedProvider(GinkgoTB())
		res2, err := agent.Run(ctx, agent.Options{
			Config:       agenttest.Config(GinkgoTB(), app),
			ConfigFile:   "agent.yaml",
			Prompt:       []string{"delete them all"},
			Provider:     second,
			SessionStore: store,
			Checkpoint:   agent.Checkpoint{ResumeID: res1.SessionID, FollowUp: true},
			Hooks: agent.Hooks{
				UserPromptSubmit: func(_ context.Context, info agent.UserPromptSubmitInfo) (agent.UserPromptSubmitResult, error) {
					seen = append(seen, info)
					return agent.UserPromptSubmitResult{Deny: true, DenyReason: "not that"}, nil
				},
			},
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).To(MatchError(ContainSubstring("not that")))
		Expect(res2.Reason).To(Equal(runstate.ReasonError))
		Expect(res2.FollowUpTaken).To(BeFalse())
		Expect(second.Requests()).To(BeEmpty())

		// The hook saw a follow-up rather than an initial prompt.
		Expect(seen).To(HaveLen(1))
		Expect(seen[0].Initial).To(BeFalse())
		Expect(seen[0].Text).To(Equal("delete them all"))

		rs, err := store.Load(context.Background(), res1.SessionID)
		Expect(err).NotTo(HaveOccurred())
		Expect(carriesUserText(rs.Messages, "delete them all")).To(BeFalse())
	})
})
