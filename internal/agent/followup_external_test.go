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
	"testing"

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

// TestFollowUp_ContinuesACompletedConversation is the path every turn of a served
// conversation takes. A one-shot turn ends by answering, which seals nothing: the next
// turn resumes that journal, its prompt enters the conversation before any model call,
// and both turns fold into one conversation in order.
//
// It also holds the two resumes that are not follow-ups: without the flag a completed
// run is still refused, and under CreateIfMissing it is still answered from the journal,
// which is what keeps a queue redelivery idempotent.
func TestFollowUp_ContinuesACompletedConversation(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	store, err := runstatefile.NewFileStore(t.TempDir())
	g.Expect(err).NotTo(HaveOccurred())

	app := agenttest.NewFakeApp(t, exampleApp())

	opts := func(provider *agenttest.ScriptedProvider, prompt string, cp agent.Checkpoint) agent.Options {
		return agent.Options{
			Config:       agenttest.Config(t, app),
			ConfigFile:   "agent.yaml",
			Prompt:       []string{prompt},
			Provider:     provider,
			SessionStore: store,
			Checkpoint:   cp,
		}
	}

	res1, err := agent.Run(ctx, opts(
		agenttest.NewScriptedProvider(t, agenttest.TextResponse("there are three streams")),
		"how many streams are there",
		agent.Checkpoint{Enabled: true},
	), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res1.Reason).To(Equal(runstate.ReasonCompleted))
	g.Expect(res1.FollowUpTaken).To(BeFalse())

	// The second turn. The conversation rests where a user message may be added, so the
	// prompt is in the first request rather than after a model call nobody asked for.
	second := agenttest.NewScriptedProvider(t, agenttest.TextResponse("the first one is ORDERS"))
	res2, err := agent.Run(ctx, opts(second, "what is the first one called",
		agent.Checkpoint{ResumeID: res1.SessionID, FollowUp: true},
	), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res2.Reason).To(Equal(runstate.ReasonCompleted))
	g.Expect(res2.FollowUpTaken).To(BeTrue())
	g.Expect(res2.Text).To(Equal("the first one is ORDERS"))

	g.Expect(second.Requests()).To(HaveLen(1))
	g.Expect(carriesUserText(second.Requests()[0].Messages, "what is the first one called")).To(BeTrue())

	// One conversation, both turns, in the order they were asked.
	rs, err := store.Load(res1.SessionID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rs.Messages).To(HaveLen(4))
	g.Expect(rs.Messages[0].Role).To(Equal(llm.RoleUser))
	g.Expect(rs.Messages[1].Role).To(Equal(llm.RoleAssistant))
	g.Expect(rs.Messages[2].Role).To(Equal(llm.RoleUser))
	g.Expect(rs.Messages[3].Role).To(Equal(llm.RoleAssistant))
	g.Expect(carriesUserText(rs.Messages, "how many streams are there")).To(BeTrue())
	g.Expect(carriesUserText(rs.Messages, "what is the first one called")).To(BeTrue())

	// A resume with nothing to add is still refused: the model would be called on a
	// finished conversation with no new input.
	_, err = agent.Run(ctx, opts(agenttest.NewScriptedProvider(t), "",
		agent.Checkpoint{ResumeID: res1.SessionID},
	), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).To(MatchError(ContainSubstring("has already completed")))

	// An at-least-once caller redelivering finished work still gets the stored answer,
	// with no model call, which is the branch a follow-up must not reach.
	replay := agenttest.NewScriptedProvider(t)
	res4, err := agent.Run(ctx, opts(replay, "what is the first one called",
		agent.Checkpoint{ResumeID: res1.SessionID, CreateIfMissing: true},
	), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res4.Text).To(Equal("the first one is ORDERS"))
	g.Expect(replay.Requests()).To(BeEmpty())
}

// TestFollowUp_FinishesAnInterruptedTurnFirst is the ordering rule that keeps a
// follow-up out of a conversation the model is part way through.
//
// A turn whose tool results were all journaled but never answered leaves the
// conversation ending on a user message with nothing pending, which is what a provider
// error at the turn boundary, the iteration cap and a crash inside a model call all
// produce. Delivering into it would fold the new prompt into those results and the
// model would answer the previous prompt. So the loop makes that call first and the
// follow-up is the turn after it.
func TestFollowUp_FinishesAnInterruptedTurnFirst(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	store, err := runstatefile.NewFileStore(t.TempDir())
	g.Expect(err).NotTo(HaveOccurred())

	app := agenttest.NewFakeApp(t, exampleApp())
	var calls atomic.Int64
	tool := countingTool(t, "record_note", &calls)

	opts := func(provider *agenttest.ScriptedProvider, prompt string, cp agent.Checkpoint) agent.Options {
		return agent.Options{
			Config:       agenttest.Config(t, app),
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
		agenttest.NewScriptedProvider(t, agenttest.ToolUseResponse("c1", "record_note", json.RawMessage(`{}`))),
		"note the outage",
		agent.Checkpoint{Enabled: true},
	), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).To(HaveOccurred())
	g.Expect(res1.Reason).To(Equal(runstate.ReasonError))
	g.Expect(calls.Load()).To(Equal(int64(1)))

	rs, err := store.Load(res1.SessionID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rs.Pending).To(BeNil())
	g.Expect(rs.Messages[len(rs.Messages)-1].Role).To(Equal(llm.RoleUser))

	// The follow-up arrives. The first call answers the interrupted turn and carries no
	// follow-up; the second is the follow-up's own turn.
	second := agenttest.NewScriptedProvider(t,
		agenttest.TextResponse("the outage is noted"),
		agenttest.TextResponse("nothing else is affected"),
	)
	res2, err := agent.Run(ctx, opts(second, "is anything else affected",
		agent.Checkpoint{ResumeID: res1.SessionID, FollowUp: true},
	), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res2.Reason).To(Equal(runstate.ReasonCompleted))
	g.Expect(res2.FollowUpTaken).To(BeTrue())
	g.Expect(res2.Text).To(Equal("nothing else is affected"))

	g.Expect(second.Requests()).To(HaveLen(2))
	g.Expect(carriesUserText(second.Requests()[0].Messages, "is anything else affected")).To(BeFalse())
	g.Expect(carriesUserText(second.Requests()[1].Messages, "is anything else affected")).To(BeTrue())
	g.Expect(calls.Load()).To(Equal(int64(1)))
}

// TestFollowUp_DiscardedWithoutTheFlag holds the default every existing caller depends
// on: a resume replaces its conversation with the journaled one and the prompt it was
// given is not delivered. A queue redelivery re-runs the interrupted work rather than
// appending its prompt a second time.
func TestFollowUp_DiscardedWithoutTheFlag(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	store, err := runstatefile.NewFileStore(t.TempDir())
	g.Expect(err).NotTo(HaveOccurred())

	app := agenttest.NewFakeApp(t, exampleApp())
	var calls atomic.Int64
	tool := countingTool(t, "record_note", &calls)

	opts := func(provider *agenttest.ScriptedProvider, cp agent.Checkpoint) agent.Options {
		return agent.Options{
			Config:       agenttest.Config(t, app),
			ConfigFile:   "agent.yaml",
			Prompt:       []string{"note the outage"},
			Provider:     provider,
			SessionStore: store,
			Checkpoint:   cp,
			CustomTools:  []toolkit.Tool{tool},
		}
	}

	res1, err := agent.Run(ctx, opts(
		agenttest.NewScriptedProvider(t, agenttest.ToolUseResponse("c1", "record_note", json.RawMessage(`{}`))),
		agent.Checkpoint{ResumeID: "job-1", CreateIfMissing: true},
	), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).To(HaveOccurred())
	g.Expect(res1.Reason).To(Equal(runstate.ReasonError))

	second := agenttest.NewScriptedProvider(t, agenttest.TextResponse("the outage is noted"))
	res2, err := agent.Run(ctx, opts(second, agent.Checkpoint{ResumeID: "job-1", CreateIfMissing: true}),
		agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res2.Reason).To(Equal(runstate.ReasonCompleted))
	g.Expect(res2.FollowUpTaken).To(BeFalse())

	// The prompt was handed over twice and entered the conversation once, as the first
	// delivery's own first prompt.
	g.Expect(second.Requests()).To(HaveLen(1))
	g.Expect(second.Requests()[0].Messages).To(HaveLen(3))
}

// TestFollowUp_NotTakenIntoADeferredConversation covers the one state that reaches no
// boundary at all. A conversation waiting on a deferred tool result suspends without
// committing its turn, so there is nowhere to put a user message: the prompt is neither
// journaled nor answered, and the caller is told so rather than having it silently
// dropped.
func TestFollowUp_NotTakenIntoADeferredConversation(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	store, err := runstatefile.NewFileStore(t.TempDir())
	g.Expect(err).NotTo(HaveOccurred())

	app := agenttest.NewFakeApp(t, exampleApp())
	var calls atomic.Int64
	tool := deferringTool(t, "change_request", &calls)

	opts := func(provider *agenttest.ScriptedProvider, prompt string, cp agent.Checkpoint) agent.Options {
		return agent.Options{
			Config:       agenttest.Config(t, app),
			ConfigFile:   "agent.yaml",
			Prompt:       []string{prompt},
			Provider:     provider,
			SessionStore: store,
			Checkpoint:   cp,
			CustomTools:  []toolkit.Tool{tool},
		}
	}

	res1, err := agent.Run(ctx, opts(
		agenttest.NewScriptedProvider(t, agenttest.ToolUseResponse("c1", "change_request", json.RawMessage(`{}`))),
		"raise a change",
		agent.Checkpoint{Enabled: true},
	), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res1.Reason).To(Equal(runstate.ReasonSuspended))
	g.Expect(res1.Deferred).To(HaveLen(1))

	// The provider carries no responses, so any model call fails the test.
	second := agenttest.NewScriptedProvider(t)
	res2, err := agent.Run(ctx, opts(second, "make it urgent",
		agent.Checkpoint{ResumeID: res1.SessionID, FollowUp: true},
	), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res2.Reason).To(Equal(runstate.ReasonSuspended))
	g.Expect(res2.FollowUpTaken).To(BeFalse())
	g.Expect(second.Requests()).To(BeEmpty())

	rs, err := store.Load(res1.SessionID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(carriesUserText(rs.Messages, "make it urgent")).To(BeFalse())
}

// TestFollowUp_RefusedOptionCombinations holds the two combinations that cost money to
// get wrong, and the empty prompt that has no turn in it. Each is refused before
// anything runs rather than being documented and left to a caller.
func TestFollowUp_RefusedOptionCombinations(t *testing.T) {
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
			Provider:     agenttest.NewScriptedProvider(t),
			SessionStore: store,
			Checkpoint:   cp,
		}
	}

	_, err = agent.Run(ctx, opts("carry on", agent.Checkpoint{FollowUp: true}),
		agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).To(MatchError(ContainSubstring("Checkpoint.ResumeID")))

	_, err = agent.Run(ctx, opts("carry on", agent.Checkpoint{ResumeID: "conv-1", FollowUp: true, CreateIfMissing: true}),
		agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).To(MatchError(ContainSubstring("Checkpoint.CreateIfMissing")))

	_, err = agent.Run(ctx, opts("  ", agent.Checkpoint{ResumeID: "conv-1", FollowUp: true}),
		agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).To(MatchError(ContainSubstring("Options.Prompt")))
}

// TestFollowUp_UnknownConversationIsItsOwnError separates the caller's mistake from a
// fault of this process: a follow-up naming a session the store does not hold is
// answerable, and a channel needs a value rather than prose to answer it with.
func TestFollowUp_UnknownConversationIsItsOwnError(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	store, err := runstatefile.NewFileStore(t.TempDir())
	g.Expect(err).NotTo(HaveOccurred())

	app := agenttest.NewFakeApp(t, exampleApp())

	_, err = agent.Run(ctx, agent.Options{
		Config:       agenttest.Config(t, app),
		ConfigFile:   "agent.yaml",
		Prompt:       []string{"carry on"},
		Provider:     agenttest.NewScriptedProvider(t),
		SessionStore: store,
		Checkpoint:   agent.Checkpoint{ResumeID: "gone", FollowUp: true},
	}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).To(MatchError(agent.ErrConversationNotFound))
	g.Expect(err).To(MatchError(runstate.ErrNotFound))
}

// TestFollowUp_EachTurnGetsAFreshIterationBudget covers the bound that would otherwise
// run out under a conversation. The cap is an absolute loop position, so without a
// fresh turn's worth per follow-up the third turn of a conversation starts past a cap
// that was set for the first.
func TestFollowUp_EachTurnGetsAFreshIterationBudget(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	store, err := runstatefile.NewFileStore(t.TempDir())
	g.Expect(err).NotTo(HaveOccurred())

	app := agenttest.NewFakeApp(t, exampleApp())

	turn := func(prompt string, cp agent.Checkpoint) *agent.Result {
		res, rerr := agent.Run(ctx, agent.Options{
			Config:       agenttest.Config(t, app, agenttest.WithMaxIterations(1)),
			ConfigFile:   "agent.yaml",
			Prompt:       []string{prompt},
			Provider:     agenttest.NewScriptedProvider(t, agenttest.TextResponse("answered "+prompt)),
			SessionStore: store,
			Checkpoint:   cp,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
		g.Expect(rerr).NotTo(HaveOccurred())
		g.Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

		return res
	}

	first := turn("one", agent.Checkpoint{Enabled: true})
	turn("two", agent.Checkpoint{ResumeID: first.SessionID, FollowUp: true})
	third := turn("three", agent.Checkpoint{ResumeID: first.SessionID, FollowUp: true})
	g.Expect(third.FollowUpTaken).To(BeTrue())

	rs, err := store.Load(first.SessionID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rs.Messages).To(HaveLen(6))
	g.Expect(rs.NextIteration).To(Equal(int64(3)))
}

// TestFollowUp_DeniedByAPolicyHook proves the hook that filters a chat's follow-ups
// also filters one a resume was handed, and that a denial records nothing: the prompt
// is not journaled, so the conversation is what it was before the turn arrived.
func TestFollowUp_DeniedByAPolicyHook(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	store, err := runstatefile.NewFileStore(t.TempDir())
	g.Expect(err).NotTo(HaveOccurred())

	app := agenttest.NewFakeApp(t, exampleApp())

	res1, err := agent.Run(ctx, agent.Options{
		Config:       agenttest.Config(t, app),
		ConfigFile:   "agent.yaml",
		Prompt:       []string{"how many streams are there"},
		Provider:     agenttest.NewScriptedProvider(t, agenttest.TextResponse("there are three streams")),
		SessionStore: store,
		Checkpoint:   agent.Checkpoint{Enabled: true},
	}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).NotTo(HaveOccurred())

	var seen []agent.UserPromptSubmitInfo
	second := agenttest.NewScriptedProvider(t)
	res2, err := agent.Run(ctx, agent.Options{
		Config:       agenttest.Config(t, app),
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
	}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).To(MatchError(ContainSubstring("not that")))
	g.Expect(res2.Reason).To(Equal(runstate.ReasonError))
	g.Expect(res2.FollowUpTaken).To(BeFalse())
	g.Expect(second.Requests()).To(BeEmpty())

	// The hook saw a follow-up rather than an initial prompt.
	g.Expect(seen).To(HaveLen(1))
	g.Expect(seen[0].Initial).To(BeFalse())
	g.Expect(seen[0].Text).To(Equal("delete them all"))

	rs, err := store.Load(res1.SessionID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(carriesUserText(rs.Messages, "delete them all")).To(BeFalse())
}
