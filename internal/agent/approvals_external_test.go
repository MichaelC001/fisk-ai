//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// These tests drive durable confirm-gate approvals through the exported agent.Run
// API: an approval the operator gives in one sitting, the resume that honors it
// without asking again, and the cases where it is dropped or refused.
package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/runstate"
	runstatefile "github.com/choria-io/fisk-ai/internal/runstate/file"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/toolkit/functool"
)

// gatedTool is a confirm-gated custom tool counting the calls that actually ran, so
// a spec can tell an approved call from one the gate refused.
func gatedTool(tb testing.TB, name string, calls *atomic.Int64) toolkit.Tool {
	tb.Helper()

	tool, err := functool.New(functool.Spec{
		Name:        name,
		Description: "removes a stream",
		Schema:      map[string]any{"type": "object"},
		Confirm:     &functool.ConfirmSpec{},
		Handler: func(context.Context, json.RawMessage, *functool.CallContext) (string, error) {
			calls.Add(1)
			return `{"removed":true}`, nil
		},
	})
	Expect(err).NotTo(HaveOccurred())

	return tool
}

// alwaysPrompter answers every approval with "allow for the conversation" and counts
// the questions, so a spec can prove the second sitting asked nothing.
func alwaysPrompter(tb testing.TB, asked *atomic.Int64) *agenttest.ScriptedPrompter {
	tb.Helper()

	p := agenttest.NewScriptedPrompter(tb)
	p.ApproveFn = func(toolkit.GateRequest) (toolkit.ConfirmChoice, error) {
		asked.Add(1)
		return toolkit.ConfirmAlways, nil
	}

	return p
}

// supplyCallApproval journals the answer an operator gave for one gated call while the
// run was suspended, which is what a waker appends before it resumes the session.
func supplyCallApproval(tb testing.TB, store runstate.Store, id, toolUseID, toolName string) {
	tb.Helper()

	ctx := context.Background()

	j, err := store.Open(ctx, id)
	Expect(err).NotTo(HaveOccurred())

	err = j.Append(ctx, j.LastSeq()+1, runstate.Record{
		Protocol:     runstate.CallApprovalProtocol,
		Optional:     true,
		CallApproval: &runstate.CallApprovalRecord{ToolUseID: toolUseID, ToolName: toolName},
	})
	Expect(err).NotTo(HaveOccurred())
	Expect(j.Close()).To(Succeed())
}

// journalRecords reads a stored run's records in order.
func journalRecords(tb testing.TB, store runstate.Store, id string) []runstate.Record {
	tb.Helper()

	ctx := context.Background()

	j, err := store.Open(ctx, id)
	Expect(err).NotTo(HaveOccurred())

	records, err := j.Records(ctx)
	Expect(err).NotTo(HaveOccurred())

	// Closed here rather than at cleanup: a journal left open holds the run's lock and
	// the next resume in the spec cannot claim it.
	Expect(j.Close()).To(Succeed())

	return records
}

// recordIndex is the position of the first record with the given protocol, or -1.
func recordIndex(records []runstate.Record, protocol runstate.Protocol) int {
	for i, rec := range records {
		if rec.Protocol == protocol {
			return i
		}
	}

	return -1
}

var _ = Describe("durable confirm-gate approvals", func() {
	// This is the defect the item exists to fix: a checkpointed chat re-asked for every
	// approval the operator had already granted. The resume's prompter has no ApproveFn, so
	// a question fails the test.
	It("Should honor an approval on a later resume", func() {
		ctx := context.Background()

		store, err := runstatefile.NewFileStore(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		var ran atomic.Int64
		tool := gatedTool(GinkgoTB(), "stream_rm", &ran)

		opts := func(provider *agenttest.ScriptedProvider, cp agent.Checkpoint, suspend func() bool) agent.Options {
			return agent.Options{
				Config:           agenttest.Config(GinkgoTB(), app),
				ConfigFile:       "agent.yaml",
				Prompt:           []string{"remove the stream"},
				Provider:         provider,
				SessionStore:     store,
				Checkpoint:       cp,
				CustomTools:      []toolkit.Tool{tool},
				SuspendRequested: suspend,
			}
		}

		// Run 1: the operator approves for the conversation, then the run suspends at the
		// next boundary.
		var asked atomic.Int64
		polls := 0
		res1, err := agent.Run(ctx, opts(
			agenttest.NewScriptedProvider(GinkgoTB(), agenttest.ToolUseResponse("c1", "stream_rm", json.RawMessage(`{}`))),
			agent.Checkpoint{Enabled: true},
			func() bool { polls++; return polls > 1 },
		), agenttest.NewRecordingEvents(), alwaysPrompter(GinkgoTB(), &asked))
		Expect(err).NotTo(HaveOccurred())
		Expect(res1.Reason).To(Equal(runstate.ReasonSuspended))
		Expect(asked.Load()).To(Equal(int64(1)))
		Expect(ran.Load()).To(Equal(int64(1)))

		// The grant is in the journal, and it is there after the result of the call that
		// triggered it.
		records := journalRecords(GinkgoTB(), store, res1.SessionID)
		Expect(recordIndex(records, runstate.DecisionProtocol)).To(BeNumerically(">", recordIndex(records, runstate.ToolResultProtocol)))

		rs, err := store.Load(context.Background(), res1.SessionID)
		Expect(err).NotTo(HaveOccurred())
		Expect(rs.Approvals).To(Equal([]string{"stream_rm"}))

		// Run 2: the same command is called again in a new process. Nothing is asked, and
		// the resume tells the operator what it inherited.
		events := agenttest.NewRecordingEvents()
		res2, err := agent.Run(ctx, opts(
			agenttest.NewScriptedProvider(GinkgoTB(),
				agenttest.ToolUseResponse("c2", "stream_rm", json.RawMessage(`{}`)),
				agenttest.TextResponse("the stream is gone"),
			),
			agent.Checkpoint{ResumeID: res1.SessionID},
			nil,
		), events, agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res2.Reason).To(Equal(runstate.ReasonCompleted))
		Expect(ran.Load()).To(Equal(int64(2)))

		starts := events.Starts()
		Expect(starts).To(HaveLen(1))
		Expect(starts[0].StandingApprovals).To(Equal([]string{"stream_rm"}))
	})

	// This is the ordering the record's position buys. The run ends between the approval
	// and the tool result, so the grant is not in the journal, and the resume re-runs that
	// very call and asks again. A grant recorded at approval time would have run the
	// command with no prompt at all.
	It("Should lose a grant when the call it covers is not answered", func() {
		ctx := context.Background()

		store, err := runstatefile.NewFileStore(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		var ran atomic.Int64
		tool := gatedTool(GinkgoTB(), "stream_rm", &ran)

		opts := func(provider *agenttest.ScriptedProvider, cp agent.Checkpoint, hooks agent.Hooks) agent.Options {
			return agent.Options{
				Config:       agenttest.Config(GinkgoTB(), app),
				ConfigFile:   "agent.yaml",
				Prompt:       []string{"remove the stream"},
				Provider:     provider,
				SessionStore: store,
				Checkpoint:   cp,
				CustomTools:  []toolkit.Tool{tool},
				Hooks:        hooks,
			}
		}

		// Run 1: the operator approves, the tool runs, and the run dies before its result
		// is journaled. PostToolUse fires after the gate and before the record.
		var asked atomic.Int64
		dying := agent.Hooks{
			PostToolUse: func(context.Context, agent.PostToolUseInfo) (agent.PostToolUseResult, error) {
				return agent.PostToolUseResult{}, context.DeadlineExceeded
			},
		}
		res1, err := agent.Run(ctx, opts(
			agenttest.NewScriptedProvider(GinkgoTB(), agenttest.ToolUseResponse("c1", "stream_rm", json.RawMessage(`{}`))),
			agent.Checkpoint{Enabled: true},
			dying,
		), agenttest.NewRecordingEvents(), alwaysPrompter(GinkgoTB(), &asked))
		Expect(err).To(HaveOccurred())
		Expect(asked.Load()).To(Equal(int64(1)))

		rs, err := store.Load(context.Background(), res1.SessionID)
		Expect(err).NotTo(HaveOccurred())
		Expect(rs.Approvals).To(BeEmpty())

		// Run 2: the unanswered call is re-run, and the operator is asked about it again.
		var asked2 atomic.Int64
		res2, err := agent.Run(ctx, opts(
			agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("the stream is gone")),
			agent.Checkpoint{ResumeID: res1.SessionID},
			agent.Hooks{},
		), agenttest.NewRecordingEvents(), alwaysPrompter(GinkgoTB(), &asked2))
		Expect(err).NotTo(HaveOccurred())
		Expect(res2.Reason).To(Equal(runstate.ReasonCompleted))
		Expect(asked2.Load()).To(Equal(int64(1)))
	})

	// This covers the operator who interrupts at an approval prompt, closes the input, or
	// whose run ends while the question is up. The gate records nothing, so the call stays
	// unanswered in the journal and the resume puts the same question rather than reading a
	// refusal nobody gave.
	It("Should ask an unanswered approval question again", func() {
		ctx := context.Background()

		store, err := runstatefile.NewFileStore(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		var ran atomic.Int64
		tool := gatedTool(GinkgoTB(), "stream_rm", &ran)

		opts := func(provider *agenttest.ScriptedProvider, cp agent.Checkpoint) agent.Options {
			return agent.Options{
				Config:       agenttest.Config(GinkgoTB(), app),
				ConfigFile:   "agent.yaml",
				Prompt:       []string{"remove the stream"},
				Provider:     provider,
				SessionStore: store,
				Checkpoint:   cp,
				CustomTools:  []toolkit.Tool{tool},
			}
		}

		// Run 1: the operator interrupts at the prompt. The run suspends and the command
		// does not run.
		interrupted := agenttest.NewScriptedPrompter(GinkgoTB())
		interrupted.ApproveFn = func(toolkit.GateRequest) (toolkit.ConfirmChoice, error) {
			return toolkit.ConfirmNo, fmt.Errorf("%w: interrupt", toolkit.ErrPromptAborted)
		}

		res1, err := agent.Run(ctx, opts(
			agenttest.NewScriptedProvider(GinkgoTB(), agenttest.ToolUseResponse("c1", "stream_rm", json.RawMessage(`{}`))),
			agent.Checkpoint{Enabled: true},
		), agenttest.NewRecordingEvents(), interrupted)
		Expect(err).To(MatchError(toolkit.ErrPromptAborted))
		Expect(res1.Reason).To(Equal(runstate.ReasonSuspended))
		Expect(ran.Load()).To(BeZero())

		// Nothing about the call reached the journal, so there is no answer to replay.
		rs, err := store.Load(context.Background(), res1.SessionID)
		Expect(err).NotTo(HaveOccurred())
		Expect(rs.Approvals).To(BeEmpty())

		// Run 2: the same question is put again, and this time it is answered.
		var asked atomic.Int64
		res2, err := agent.Run(ctx, opts(
			agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("the stream is gone")),
			agent.Checkpoint{ResumeID: res1.SessionID},
		), agenttest.NewRecordingEvents(), alwaysPrompter(GinkgoTB(), &asked))
		Expect(err).NotTo(HaveOccurred())
		Expect(res2.Reason).To(Equal(runstate.ReasonCompleted))
		Expect(asked.Load()).To(Equal(int64(1)))
		Expect(ran.Load()).To(Equal(int64(1)))
	})

	// This covers the answer that arrives after the run gave up on its question. The
	// operator answered "allow once" for one call, so the resume dispatches that call
	// without asking and asks about the next one.
	It("Should run the call a one-shot answer names", func() {
		ctx := context.Background()

		store, err := runstatefile.NewFileStore(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		var ran atomic.Int64
		tool := gatedTool(GinkgoTB(), "stream_rm", &ran)

		opts := func(provider *agenttest.ScriptedProvider, cp agent.Checkpoint) agent.Options {
			return agent.Options{
				Config:       agenttest.Config(GinkgoTB(), app),
				ConfigFile:   "agent.yaml",
				Prompt:       []string{"remove the stream"},
				Provider:     provider,
				SessionStore: store,
				Checkpoint:   cp,
				CustomTools:  []toolkit.Tool{tool},
			}
		}

		// Run 1: nobody answers the question, so the run suspends with the call unanswered.
		unanswered := agenttest.NewScriptedPrompter(GinkgoTB())
		unanswered.ApproveFn = func(toolkit.GateRequest) (toolkit.ConfirmChoice, error) {
			return toolkit.ConfirmNo, fmt.Errorf("%w: the caller did not answer", toolkit.ErrPromptAborted)
		}

		res1, err := agent.Run(ctx, opts(
			agenttest.NewScriptedProvider(GinkgoTB(), agenttest.ToolUseResponse("c1", "stream_rm", json.RawMessage(`{}`))),
			agent.Checkpoint{Enabled: true},
		), agenttest.NewRecordingEvents(), unanswered)
		Expect(err).To(MatchError(toolkit.ErrPromptAborted))
		Expect(res1.Reason).To(Equal(runstate.ReasonSuspended))
		Expect(ran.Load()).To(BeZero())

		// The answer arrives out of band and is journaled against the call it was asked
		// about, which is what wakes the session.
		supplyCallApproval(GinkgoTB(), store, res1.SessionID, "c1", "stream_rm")

		rs, err := store.Load(context.Background(), res1.SessionID)
		Expect(err).NotTo(HaveOccurred())
		Expect(rs.CallApprovals).To(Equal([]runstate.CallApprovalRecord{{ToolUseID: "c1", ToolName: "stream_rm"}}))

		// Run 2: c1 runs without a question. The model then calls the same tool again, and
		// that call is asked about, since the answer covered one dispatch.
		//
		// The prompter answers "once" rather than "always", so a question put about c1 shows
		// up in the count instead of granting a standing approval that covers c2 as well.
		var asked atomic.Int64
		once := agenttest.NewScriptedPrompter(GinkgoTB())
		once.ApproveFn = func(toolkit.GateRequest) (toolkit.ConfirmChoice, error) {
			asked.Add(1)
			return toolkit.ConfirmOnce, nil
		}

		res2, err := agent.Run(ctx, opts(
			agenttest.NewScriptedProvider(GinkgoTB(),
				agenttest.ToolUseResponse("c2", "stream_rm", json.RawMessage(`{}`)),
				agenttest.TextResponse("both streams are gone"),
			),
			agent.Checkpoint{ResumeID: res1.SessionID},
		), agenttest.NewRecordingEvents(), once)
		Expect(err).NotTo(HaveOccurred())
		Expect(res2.Reason).To(Equal(runstate.ReasonCompleted))
		Expect(ran.Load()).To(Equal(int64(2)))
		Expect(asked.Load()).To(Equal(int64(1)))
	})

	// This is what makes a durable grant safe to restore. A grant outlives the process that
	// recorded it, so a resume with nobody reachable declines the command rather than
	// running it unwatched.
	It("Should not honor a grant with no operator", func() {
		ctx := context.Background()

		store, err := runstatefile.NewFileStore(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		var ran atomic.Int64
		tool := gatedTool(GinkgoTB(), "stream_rm", &ran)

		opts := func(provider *agenttest.ScriptedProvider, cp agent.Checkpoint, suspend func() bool) agent.Options {
			return agent.Options{
				Config:           agenttest.Config(GinkgoTB(), app),
				ConfigFile:       "agent.yaml",
				Prompt:           []string{"remove the stream"},
				Provider:         provider,
				SessionStore:     store,
				Checkpoint:       cp,
				CustomTools:      []toolkit.Tool{tool},
				SuspendRequested: suspend,
			}
		}

		var asked atomic.Int64
		polls := 0
		res1, err := agent.Run(ctx, opts(
			agenttest.NewScriptedProvider(GinkgoTB(), agenttest.ToolUseResponse("c1", "stream_rm", json.RawMessage(`{}`))),
			agent.Checkpoint{Enabled: true},
			func() bool { polls++; return polls > 1 },
		), agenttest.NewRecordingEvents(), alwaysPrompter(GinkgoTB(), &asked))
		Expect(err).NotTo(HaveOccurred())
		Expect(ran.Load()).To(Equal(int64(1)))

		// Resumed on a host with no operator: the restored grant is not consulted and the
		// command does not run.
		res2, err := agent.Run(ctx, opts(
			agenttest.NewScriptedProvider(GinkgoTB(),
				agenttest.ToolUseResponse("c2", "stream_rm", json.RawMessage(`{}`)),
				agenttest.TextResponse("I could not remove it"),
			),
			agent.Checkpoint{ResumeID: res1.SessionID},
			nil,
		), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()).NoOperator())
		Expect(err).NotTo(HaveOccurred())
		Expect(res2.Reason).To(Equal(runstate.ReasonCompleted))
		Expect(ran.Load()).To(Equal(int64(1)))
	})

	// This covers a grant keyed on a tool name whose tool set moved under it: --force
	// resumes across the mismatch, the grants do not come with it, and the operator is told
	// rather than discovering it at the prompt.
	It("Should drop the grants on a forced resume", func() {
		ctx := context.Background()

		store, err := runstatefile.NewFileStore(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		var ran atomic.Int64
		tool := gatedTool(GinkgoTB(), "stream_rm", &ran)
		other, err := functool.New(functool.Spec{
			Name: "unrelated", Description: "an unrelated tool", Schema: map[string]any{"type": "object"},
			Handler: func(context.Context, json.RawMessage, *functool.CallContext) (string, error) { return "{}", nil },
		})
		Expect(err).NotTo(HaveOccurred())

		opts := func(provider *agenttest.ScriptedProvider, cp agent.Checkpoint, tools []toolkit.Tool, suspend func() bool) agent.Options {
			return agent.Options{
				Config:           agenttest.Config(GinkgoTB(), app),
				ConfigFile:       "agent.yaml",
				Prompt:           []string{"remove the stream"},
				Provider:         provider,
				SessionStore:     store,
				Checkpoint:       cp,
				CustomTools:      tools,
				SuspendRequested: suspend,
			}
		}

		var asked atomic.Int64
		polls := 0
		res1, err := agent.Run(ctx, opts(
			agenttest.NewScriptedProvider(GinkgoTB(), agenttest.ToolUseResponse("c1", "stream_rm", json.RawMessage(`{}`))),
			agent.Checkpoint{Enabled: true},
			[]toolkit.Tool{tool},
			func() bool { polls++; return polls > 1 },
		), agenttest.NewRecordingEvents(), alwaysPrompter(GinkgoTB(), &asked))
		Expect(err).NotTo(HaveOccurred())

		// The tool set changed, so the resume warns and drops the standing approvals: a
		// grant is keyed on a tool name, and the tool set moved under it. The resume itself
		// continues either way, the tools hash not being a blocking difference, so the same
		// command is asked about again.
		var asked2 atomic.Int64
		events := agenttest.NewRecordingEvents()
		res2, err := agent.Run(ctx, opts(
			agenttest.NewScriptedProvider(GinkgoTB(),
				agenttest.ToolUseResponse("c2", "stream_rm", json.RawMessage(`{}`)),
				agenttest.TextResponse("the stream is gone"),
			),
			agent.Checkpoint{ResumeID: res1.SessionID, Force: true},
			[]toolkit.Tool{tool, other},
			nil,
		), events, alwaysPrompter(GinkgoTB(), &asked2))
		Expect(err).NotTo(HaveOccurred())
		Expect(res2.Reason).To(Equal(runstate.ReasonCompleted))
		Expect(asked2.Load()).To(Equal(int64(1)))
		Expect(events.HasWarning(agent.WarnApprovalsDropped)).To(BeTrue())
		Expect(events.Starts()[0].StandingApprovals).To(BeEmpty())
	})

	// This separates the drift a resume refuses from the drift it reports. Neither budget
	// bound can leave a stored conversation incoherent, so a resume under a different cap
	// continues without --force and the grants the operator gave come with it.
	It("Should keep the grants across a budget change", func() {
		ctx := context.Background()

		store, err := runstatefile.NewFileStore(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		var ran atomic.Int64
		tool := gatedTool(GinkgoTB(), "stream_rm", &ran)

		opts := func(provider *agenttest.ScriptedProvider, cp agent.Checkpoint, tokens int64, suspend func() bool) agent.Options {
			return agent.Options{
				Config:           agenttest.Config(GinkgoTB(), app, agenttest.WithMaxTokens(tokens)),
				ConfigFile:       "agent.yaml",
				Prompt:           []string{"remove the stream"},
				Provider:         provider,
				SessionStore:     store,
				Checkpoint:       cp,
				CustomTools:      []toolkit.Tool{tool},
				SuspendRequested: suspend,
			}
		}

		var asked atomic.Int64
		polls := 0
		res1, err := agent.Run(ctx, opts(
			agenttest.NewScriptedProvider(GinkgoTB(), agenttest.ToolUseResponse("c1", "stream_rm", json.RawMessage(`{}`))),
			agent.Checkpoint{Enabled: true},
			500000,
			func() bool { polls++; return polls > 1 },
		), agenttest.NewRecordingEvents(), alwaysPrompter(GinkgoTB(), &asked))
		Expect(err).NotTo(HaveOccurred())
		Expect(ran.Load()).To(Equal(int64(1)))

		// Resumed under a lower token cap: the run continues without --force, the operator
		// is told, and the restored grant runs the second call without a question.
		var asked2 atomic.Int64
		events := agenttest.NewRecordingEvents()
		res2, err := agent.Run(ctx, opts(
			agenttest.NewScriptedProvider(GinkgoTB(),
				agenttest.ToolUseResponse("c2", "stream_rm", json.RawMessage(`{}`)),
				agenttest.TextResponse("both streams are gone"),
			),
			agent.Checkpoint{ResumeID: res1.SessionID},
			400000,
			nil,
		), events, alwaysPrompter(GinkgoTB(), &asked2))
		Expect(err).NotTo(HaveOccurred())
		Expect(res2.Reason).To(Equal(runstate.ReasonCompleted))
		Expect(ran.Load()).To(Equal(int64(2)))
		Expect(asked2.Load()).To(BeZero())
		Expect(events.HasWarning(agent.WarnBudgetDrift)).To(BeTrue())
		Expect(events.Starts()[0].StandingApprovals).To(ConsistOf("stream_rm"))
	})

	// This holds the scoping rule that has no exceptions: a grant belongs to the
	// conversation it was given in, and a reset starts another one. A checkpointed reset
	// rotates to a separately resumable journal, and carrying grants across would record
	// one decision durably into both.
	It("Should drop the grants on a context reset", func() {
		ctx := context.Background()

		store, err := runstatefile.NewFileStore(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		var ran atomic.Int64
		tool := gatedTool(GinkgoTB(), "stream_rm", &ran)

		// The turn before the reset calls the gated tool and is approved; the turn after it
		// calls the same tool, and the operator is asked a second time.
		provider := agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("c1", "stream_rm", json.RawMessage(`{}`)),
			agenttest.TextResponse("removed"),
			agenttest.ToolUseResponse("c2", "stream_rm", json.RawMessage(`{}`)),
			agenttest.TextResponse("removed again"),
		)

		turns := 0
		next := func(context.Context) agent.Continuation {
			turns++
			switch turns {
			case 1:
				return agent.Continuation{Text: "and the other one", Reset: true, Continue: true}
			default:
				return agent.Continuation{Continue: false}
			}
		}

		var asked atomic.Int64
		res, err := agent.Run(ctx, agent.Options{
			Config:       agenttest.Config(GinkgoTB(), app),
			ConfigFile:   "agent.yaml",
			Prompt:       []string{"remove the stream"},
			Provider:     provider,
			SessionStore: store,
			Checkpoint:   agent.Checkpoint{Enabled: true},
			CustomTools:  []toolkit.Tool{tool},
			NextPrompt:   next,
		}, agenttest.NewRecordingEvents(), alwaysPrompter(GinkgoTB(), &asked))
		Expect(err).NotTo(HaveOccurred())
		// A checkpointed chat the operator ends suspends, so the session stays resumable.
		Expect(res.Reason).To(Equal(runstate.ReasonSuspended))
		Expect(ran.Load()).To(Equal(int64(2)))
		Expect(asked.Load()).To(Equal(int64(2)))

		// The rotated-to session carries only the grant its own turn produced.
		rs, err := store.Load(context.Background(), res.SessionID)
		Expect(err).NotTo(HaveOccurred())
		Expect(rs.Approvals).To(Equal([]string{"stream_rm"}))
	})

	// This is the deferral counterpart of the ordering: the call produces no result and is
	// never dispatched again, so a grant held for a result that is not coming would be lost
	// with nobody left to re-ask.
	It("Should record a grant for a call that defers", func() {
		ctx := context.Background()

		store, err := runstatefile.NewFileStore(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		tool, err := functool.New(functool.Spec{
			Name:        "change_request",
			Description: "raises a change request and answers when it is approved",
			Schema:      map[string]any{"type": "object"},
			Confirm:     &functool.ConfirmSpec{},
			Handler: func(context.Context, json.RawMessage, *functool.CallContext) (string, error) {
				return "", toolkit.DeferResult("waiting on change approval", "CHG-1234")
			},
		})
		Expect(err).NotTo(HaveOccurred())

		var asked atomic.Int64
		res, err := agent.Run(ctx, agent.Options{
			Config:       agenttest.Config(GinkgoTB(), app),
			ConfigFile:   "agent.yaml",
			Prompt:       []string{"raise a change"},
			Provider:     agenttest.NewScriptedProvider(GinkgoTB(), agenttest.ToolUseResponse("c1", "change_request", json.RawMessage(`{}`))),
			SessionStore: store,
			Checkpoint:   agent.Checkpoint{Enabled: true},
			CustomTools:  []toolkit.Tool{tool},
		}, agenttest.NewRecordingEvents(), alwaysPrompter(GinkgoTB(), &asked))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonSuspended))
		Expect(asked.Load()).To(Equal(int64(1)))

		rs, err := store.Load(context.Background(), res.SessionID)
		Expect(err).NotTo(HaveOccurred())
		Expect(rs.Approvals).To(Equal([]string{"change_request"}))

		records := journalRecords(GinkgoTB(), store, res.SessionID)
		Expect(recordIndex(records, runstate.DecisionProtocol)).To(BeNumerically(">", recordIndex(records, runstate.DeferredProtocol)))
	})
})
