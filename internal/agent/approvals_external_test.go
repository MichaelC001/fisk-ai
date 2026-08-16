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
	"sync/atomic"
	"testing"

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
func gatedTool(t *testing.T, name string, calls *atomic.Int64) toolkit.Tool {
	t.Helper()

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
	NewWithT(t).Expect(err).NotTo(HaveOccurred())

	return tool
}

// alwaysPrompter answers every approval with "allow for the conversation" and counts
// the questions, so a spec can prove the second sitting asked nothing.
func alwaysPrompter(t *testing.T, asked *atomic.Int64) *agenttest.ScriptedPrompter {
	t.Helper()

	p := agenttest.NewScriptedPrompter(t)
	p.ApproveFn = func(toolkit.GateRequest) (toolkit.ConfirmChoice, error) {
		asked.Add(1)
		return toolkit.ConfirmAlways, nil
	}

	return p
}

// TestApprovals_HonoredOnALaterResume is the defect the item exists to fix: a
// checkpointed chat re-asked for every approval the operator had already granted.
// The resume's prompter has no ApproveFn, so a question fails the test.
func TestApprovals_HonoredOnALaterResume(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	store, err := runstatefile.NewFileStore(t.TempDir())
	g.Expect(err).NotTo(HaveOccurred())

	app := agenttest.NewFakeApp(t, exampleApp())
	var ran atomic.Int64
	tool := gatedTool(t, "stream_rm", &ran)

	opts := func(provider *agenttest.ScriptedProvider, cp agent.Checkpoint, suspend func() bool) agent.Options {
		return agent.Options{
			Config:           agenttest.Config(t, app),
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
		agenttest.NewScriptedProvider(t, agenttest.ToolUseResponse("c1", "stream_rm", json.RawMessage(`{}`))),
		agent.Checkpoint{Enabled: true},
		func() bool { polls++; return polls > 1 },
	), agenttest.NewRecordingEvents(), alwaysPrompter(t, &asked))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res1.Reason).To(Equal(runstate.ReasonSuspended))
	g.Expect(asked.Load()).To(Equal(int64(1)))
	g.Expect(ran.Load()).To(Equal(int64(1)))

	// The grant is in the journal, and it is there after the result of the call that
	// triggered it.
	records := journalRecords(t, store, res1.SessionID)
	g.Expect(recordIndex(records, runstate.DecisionProtocol)).To(BeNumerically(">", recordIndex(records, runstate.ToolResultProtocol)))

	rs, err := store.Load(res1.SessionID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rs.Approvals).To(Equal([]string{"stream_rm"}))

	// Run 2: the same command is called again in a new process. Nothing is asked, and
	// the resume tells the operator what it inherited.
	events := agenttest.NewRecordingEvents()
	res2, err := agent.Run(ctx, opts(
		agenttest.NewScriptedProvider(t,
			agenttest.ToolUseResponse("c2", "stream_rm", json.RawMessage(`{}`)),
			agenttest.TextResponse("the stream is gone"),
		),
		agent.Checkpoint{ResumeID: res1.SessionID},
		nil,
	), events, agenttest.NewScriptedPrompter(t))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res2.Reason).To(Equal(runstate.ReasonCompleted))
	g.Expect(ran.Load()).To(Equal(int64(2)))

	starts := events.Starts()
	g.Expect(starts).To(HaveLen(1))
	g.Expect(starts[0].StandingApprovals).To(Equal([]string{"stream_rm"}))
}

// TestApprovals_LostWhenTheCallIsNotAnswered is the ordering the record's position
// buys. The run ends between the approval and the tool result, so the grant is not
// in the journal, and the resume re-runs that very call and asks again. A grant
// recorded at approval time would have run the command with no prompt at all.
func TestApprovals_LostWhenTheCallIsNotAnswered(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	store, err := runstatefile.NewFileStore(t.TempDir())
	g.Expect(err).NotTo(HaveOccurred())

	app := agenttest.NewFakeApp(t, exampleApp())
	var ran atomic.Int64
	tool := gatedTool(t, "stream_rm", &ran)

	opts := func(provider *agenttest.ScriptedProvider, cp agent.Checkpoint, hooks agent.Hooks) agent.Options {
		return agent.Options{
			Config:       agenttest.Config(t, app),
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
		agenttest.NewScriptedProvider(t, agenttest.ToolUseResponse("c1", "stream_rm", json.RawMessage(`{}`))),
		agent.Checkpoint{Enabled: true},
		dying,
	), agenttest.NewRecordingEvents(), alwaysPrompter(t, &asked))
	g.Expect(err).To(HaveOccurred())
	g.Expect(asked.Load()).To(Equal(int64(1)))

	rs, err := store.Load(res1.SessionID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rs.Approvals).To(BeEmpty())

	// Run 2: the unanswered call is re-run, and the operator is asked about it again.
	var asked2 atomic.Int64
	res2, err := agent.Run(ctx, opts(
		agenttest.NewScriptedProvider(t, agenttest.TextResponse("the stream is gone")),
		agent.Checkpoint{ResumeID: res1.SessionID},
		agent.Hooks{},
	), agenttest.NewRecordingEvents(), alwaysPrompter(t, &asked2))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res2.Reason).To(Equal(runstate.ReasonCompleted))
	g.Expect(asked2.Load()).To(Equal(int64(1)))
}

// TestApprovals_NotHonoredWithNoOperator is what makes a durable grant safe to
// restore. A grant outlives the process that recorded it, so a resume with nobody
// reachable declines the command rather than running it unwatched.
func TestApprovals_NotHonoredWithNoOperator(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	store, err := runstatefile.NewFileStore(t.TempDir())
	g.Expect(err).NotTo(HaveOccurred())

	app := agenttest.NewFakeApp(t, exampleApp())
	var ran atomic.Int64
	tool := gatedTool(t, "stream_rm", &ran)

	opts := func(provider *agenttest.ScriptedProvider, cp agent.Checkpoint, suspend func() bool) agent.Options {
		return agent.Options{
			Config:           agenttest.Config(t, app),
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
		agenttest.NewScriptedProvider(t, agenttest.ToolUseResponse("c1", "stream_rm", json.RawMessage(`{}`))),
		agent.Checkpoint{Enabled: true},
		func() bool { polls++; return polls > 1 },
	), agenttest.NewRecordingEvents(), alwaysPrompter(t, &asked))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ran.Load()).To(Equal(int64(1)))

	// Resumed on a host with no operator: the restored grant is not consulted and the
	// command does not run.
	res2, err := agent.Run(ctx, opts(
		agenttest.NewScriptedProvider(t,
			agenttest.ToolUseResponse("c2", "stream_rm", json.RawMessage(`{}`)),
			agenttest.TextResponse("I could not remove it"),
		),
		agent.Checkpoint{ResumeID: res1.SessionID},
		nil,
	), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t).NoOperator())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res2.Reason).To(Equal(runstate.ReasonCompleted))
	g.Expect(ran.Load()).To(Equal(int64(1)))
}

// TestApprovals_DroppedByAForcedResume covers a grant keyed on a tool name whose
// tool set moved under it: --force resumes across the mismatch, the grants do not
// come with it, and the operator is told rather than discovering it at the prompt.
func TestApprovals_DroppedByAForcedResume(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	store, err := runstatefile.NewFileStore(t.TempDir())
	g.Expect(err).NotTo(HaveOccurred())

	app := agenttest.NewFakeApp(t, exampleApp())
	var ran atomic.Int64
	tool := gatedTool(t, "stream_rm", &ran)
	other, err := functool.New(functool.Spec{
		Name: "unrelated", Description: "an unrelated tool", Schema: map[string]any{"type": "object"},
		Handler: func(context.Context, json.RawMessage, *functool.CallContext) (string, error) { return "{}", nil },
	})
	g.Expect(err).NotTo(HaveOccurred())

	opts := func(provider *agenttest.ScriptedProvider, cp agent.Checkpoint, tools []toolkit.Tool, suspend func() bool) agent.Options {
		return agent.Options{
			Config:           agenttest.Config(t, app),
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
		agenttest.NewScriptedProvider(t, agenttest.ToolUseResponse("c1", "stream_rm", json.RawMessage(`{}`))),
		agent.Checkpoint{Enabled: true},
		[]toolkit.Tool{tool},
		func() bool { polls++; return polls > 1 },
	), agenttest.NewRecordingEvents(), alwaysPrompter(t, &asked))
	g.Expect(err).NotTo(HaveOccurred())

	// The tool set changed, so the fingerprint no longer matches and only --force gets
	// in. The same command is asked about again.
	var asked2 atomic.Int64
	events := agenttest.NewRecordingEvents()
	res2, err := agent.Run(ctx, opts(
		agenttest.NewScriptedProvider(t,
			agenttest.ToolUseResponse("c2", "stream_rm", json.RawMessage(`{}`)),
			agenttest.TextResponse("the stream is gone"),
		),
		agent.Checkpoint{ResumeID: res1.SessionID, Force: true},
		[]toolkit.Tool{tool, other},
		nil,
	), events, alwaysPrompter(t, &asked2))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res2.Reason).To(Equal(runstate.ReasonCompleted))
	g.Expect(asked2.Load()).To(Equal(int64(1)))
	g.Expect(events.HasWarning(agent.WarnApprovalsDropped)).To(BeTrue())
	g.Expect(events.Starts()[0].StandingApprovals).To(BeEmpty())
}

// TestApprovals_DroppedByAContextReset holds the scoping rule that has no
// exceptions: a grant belongs to the conversation it was given in, and a reset
// starts another one. A checkpointed reset rotates to a separately resumable
// journal, and carrying grants across would record one decision durably into both.
func TestApprovals_DroppedByAContextReset(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	store, err := runstatefile.NewFileStore(t.TempDir())
	g.Expect(err).NotTo(HaveOccurred())

	app := agenttest.NewFakeApp(t, exampleApp())
	var ran atomic.Int64
	tool := gatedTool(t, "stream_rm", &ran)

	// The turn before the reset calls the gated tool and is approved; the turn after it
	// calls the same tool, and the operator is asked a second time.
	provider := agenttest.NewScriptedProvider(t,
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
		Config:       agenttest.Config(t, app),
		ConfigFile:   "agent.yaml",
		Prompt:       []string{"remove the stream"},
		Provider:     provider,
		SessionStore: store,
		Checkpoint:   agent.Checkpoint{Enabled: true},
		CustomTools:  []toolkit.Tool{tool},
		NextPrompt:   next,
	}, agenttest.NewRecordingEvents(), alwaysPrompter(t, &asked))
	g.Expect(err).NotTo(HaveOccurred())
	// A checkpointed chat the operator ends suspends, so the session stays resumable.
	g.Expect(res.Reason).To(Equal(runstate.ReasonSuspended))
	g.Expect(ran.Load()).To(Equal(int64(2)))
	g.Expect(asked.Load()).To(Equal(int64(2)))

	// The rotated-to session carries only the grant its own turn produced.
	rs, err := store.Load(res.SessionID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rs.Approvals).To(Equal([]string{"stream_rm"}))
}

// TestApprovals_RecordedForACallThatDefers is the deferral counterpart of the
// ordering: the call produces no result and is never dispatched again, so a grant
// held for a result that is not coming would be lost with nobody left to re-ask.
func TestApprovals_RecordedForACallThatDefers(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	store, err := runstatefile.NewFileStore(t.TempDir())
	g.Expect(err).NotTo(HaveOccurred())

	app := agenttest.NewFakeApp(t, exampleApp())
	tool, err := functool.New(functool.Spec{
		Name:        "change_request",
		Description: "raises a change request and answers when it is approved",
		Schema:      map[string]any{"type": "object"},
		Confirm:     &functool.ConfirmSpec{},
		Handler: func(context.Context, json.RawMessage, *functool.CallContext) (string, error) {
			return "", toolkit.DeferResult("waiting on change approval", "CHG-1234")
		},
	})
	g.Expect(err).NotTo(HaveOccurred())

	var asked atomic.Int64
	res, err := agent.Run(ctx, agent.Options{
		Config:       agenttest.Config(t, app),
		ConfigFile:   "agent.yaml",
		Prompt:       []string{"raise a change"},
		Provider:     agenttest.NewScriptedProvider(t, agenttest.ToolUseResponse("c1", "change_request", json.RawMessage(`{}`))),
		SessionStore: store,
		Checkpoint:   agent.Checkpoint{Enabled: true},
		CustomTools:  []toolkit.Tool{tool},
	}, agenttest.NewRecordingEvents(), alwaysPrompter(t, &asked))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.Reason).To(Equal(runstate.ReasonSuspended))
	g.Expect(asked.Load()).To(Equal(int64(1)))

	rs, err := store.Load(res.SessionID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rs.Approvals).To(Equal([]string{"change_request"}))

	records := journalRecords(t, store, res.SessionID)
	g.Expect(recordIndex(records, runstate.DecisionProtocol)).To(BeNumerically(">", recordIndex(records, runstate.DeferredProtocol)))
}

// journalRecords reads a stored run's records in order.
func journalRecords(t *testing.T, store runstate.Store, id string) []runstate.Record {
	t.Helper()
	g := NewWithT(t)

	j, err := store.Open(id)
	g.Expect(err).NotTo(HaveOccurred())

	records, err := j.Records()
	g.Expect(err).NotTo(HaveOccurred())

	// Closed here rather than at cleanup: a journal left open holds the run's lock and
	// the next resume in the spec cannot claim it.
	g.Expect(j.Close()).To(Succeed())

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
