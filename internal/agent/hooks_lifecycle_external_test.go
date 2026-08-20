//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// These tests exercise the run-entry lifecycle hooks (RunStart and the initial
// UserPromptSubmit) and the run-exit one (RunEnd) through the exported agent.Run API,
// since they fire from Run's setup and its panic barrier rather than the runner's loop.
// The continuation-boundary hooks (TurnEnd and the follow-up UserPromptSubmit) are
// covered at the runner level in agent_test.go.
package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// TestHooks_RunEntryFiresRunStartThenInitialPrompt proves the run-entry hooks fire
// once, in order (RunStart then the initial UserPromptSubmit), and carry the run's
// resolved parameters on a fresh checkpointed run.
func TestHooks_RunEntryFiresRunStartThenInitialPrompt(t *testing.T) {
	g := NewWithT(t)

	app := agenttest.NewFakeApp(t, exampleApp())

	var order []string
	var start agent.RunStartInfo
	var submit agent.UserPromptSubmitInfo

	res, err := agent.Run(context.Background(), agent.Options{
		Config:       agenttest.Config(t, app),
		ConfigFile:   "agent.yaml",
		Prompt:       []string{"go"},
		Provider:     agenttest.NewScriptedProvider(t, agenttest.TextResponse("done")),
		Checkpoint:   agent.Checkpoint{Enabled: true},
		SessionStore: agenttest.NewFakeSessionStore(t),
		Hooks: agent.Hooks{
			RunStart: func(_ context.Context, in agent.RunStartInfo) error {
				order = append(order, "session-start")
				start = in
				return nil
			},
			UserPromptSubmit: func(_ context.Context, in agent.UserPromptSubmitInfo) (agent.UserPromptSubmitResult, error) {
				order = append(order, "prompt-submit")
				submit = in
				return agent.UserPromptSubmitResult{}, nil
			},
		},
	}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

	// RunStart fires before the initial prompt, exactly once each.
	g.Expect(order).To(Equal([]string{"session-start", "prompt-submit"}))

	g.Expect(start.Resumed).To(BeFalse())
	g.Expect(start.Interactive).To(BeFalse())
	g.Expect(start.Model).To(Equal("test-model"))
	g.Expect(start.SessionID).To(Equal(res.SessionID))
	g.Expect(start.SessionID).NotTo(BeEmpty())
	g.Expect(start.ToolNames).To(ContainElement("do"))

	g.Expect(submit.Initial).To(BeTrue())
	g.Expect(submit.Text).To(Equal("go"))
}

// TestHooks_InitialPromptDenyAbortsBeforeSessionCreate proves a denied initial prompt
// stops the run before any session is created (no orphan) or model call is made, and
// surfaces the deny reason as an error.
func TestHooks_InitialPromptDenyAbortsBeforeSessionCreate(t *testing.T) {
	g := NewWithT(t)

	app := agenttest.NewFakeApp(t, exampleApp())
	store := agenttest.NewFakeSessionStore(t)

	_, err := agent.Run(context.Background(), agent.Options{
		Config:     agenttest.Config(t, app),
		ConfigFile: "agent.yaml",
		Prompt:     []string{"go"},
		// No scripted responses: a model call would error, proving none is made.
		Provider:     agenttest.NewScriptedProvider(t),
		Checkpoint:   agent.Checkpoint{Enabled: true},
		SessionStore: store,
		Hooks: agent.Hooks{
			UserPromptSubmit: func(context.Context, agent.UserPromptSubmitInfo) (agent.UserPromptSubmitResult, error) {
				return agent.UserPromptSubmitResult{Deny: true, DenyReason: "blocked by policy"}, nil
			},
		},
	}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))

	g.Expect(err).To(MatchError(ContainSubstring("initial prompt was rejected")))
	g.Expect(err).To(MatchError(ContainSubstring("blocked by policy")))

	// No session was created, so nothing is left to resume.
	infos, lerr := store.List()
	g.Expect(lerr).NotTo(HaveOccurred())
	g.Expect(infos).To(BeEmpty())
}

// TestHooks_RunStartErrorAbortsBeforeSessionCreate proves a RunStart error aborts
// the run before any session is created.
func TestHooks_RunStartErrorAbortsBeforeSessionCreate(t *testing.T) {
	g := NewWithT(t)

	app := agenttest.NewFakeApp(t, exampleApp())
	store := agenttest.NewFakeSessionStore(t)

	_, err := agent.Run(context.Background(), agent.Options{
		Config:       agenttest.Config(t, app),
		ConfigFile:   "agent.yaml",
		Prompt:       []string{"go"},
		Provider:     agenttest.NewScriptedProvider(t),
		Checkpoint:   agent.Checkpoint{Enabled: true},
		SessionStore: store,
		Hooks: agent.Hooks{
			RunStart: func(context.Context, agent.RunStartInfo) error {
				return errors.New("boom")
			},
		},
	}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))

	g.Expect(err).To(MatchError(ContainSubstring("RunStart hook")))

	infos, lerr := store.List()
	g.Expect(lerr).NotTo(HaveOccurred())
	g.Expect(infos).To(BeEmpty())
}

// TestHooks_ResumeFiresRunStartResumedNoInitialPrompt proves a resume fires
// RunStart with Resumed true and does NOT re-fire the initial UserPromptSubmit, since
// the reconstructed history is not a fresh prompt.
func TestHooks_ResumeFiresRunStartResumedNoInitialPrompt(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	store := agenttest.NewFakeSessionStore(t)
	app := agenttest.NewFakeApp(t, exampleApp())

	// Run 1: one tool call, then a suspend at the next boundary, journaled to the store.
	suspendPolls := 0
	res1, err := agent.Run(ctx, agent.Options{
		Config:           agenttest.Config(t, app),
		ConfigFile:       "agent.yaml",
		Prompt:           []string{"start"},
		Provider:         agenttest.NewScriptedProvider(t, agenttest.ToolUseResponse("c1", "do", json.RawMessage(`{"subject":"x"}`))),
		Checkpoint:       agent.Checkpoint{Enabled: true},
		SessionStore:     store,
		SuspendRequested: func() bool { suspendPolls++; return suspendPolls > 1 },
	}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res1.Reason).To(Equal(runstate.ReasonSuspended))

	// Run 2: resume the saved session, recording the lifecycle hooks.
	var starts []agent.RunStartInfo
	var submits []agent.UserPromptSubmitInfo
	res2, err := agent.Run(ctx, agent.Options{
		Config:       agenttest.Config(t, app),
		ConfigFile:   "agent.yaml",
		Provider:     agenttest.NewScriptedProvider(t, agenttest.TextResponse("finished")),
		Checkpoint:   agent.Checkpoint{ResumeID: res1.SessionID},
		SessionStore: store,
		Hooks: agent.Hooks{
			RunStart: func(_ context.Context, in agent.RunStartInfo) error {
				starts = append(starts, in)
				return nil
			},
			UserPromptSubmit: func(_ context.Context, in agent.UserPromptSubmitInfo) (agent.UserPromptSubmitResult, error) {
				submits = append(submits, in)
				return agent.UserPromptSubmitResult{}, nil
			},
		},
	}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res2.Reason).To(Equal(runstate.ReasonCompleted))

	// RunStart re-fires with Resumed true; the initial prompt does not re-fire.
	g.Expect(starts).To(HaveLen(1))
	g.Expect(starts[0].Resumed).To(BeTrue())
	g.Expect(starts[0].SessionID).To(Equal(res1.SessionID))
	g.Expect(submits).To(BeEmpty())
}

// TestHooks_RunEndFiresOnEveryOutcome proves RunEnd fires exactly once for every
// way a run that reached the runner can end - a completed answer, an exhausted token
// budget, a failing turn, and a graceful suspend - carrying that outcome's reason and a
// stats snapshot, and reporting none of them as a crash.
func TestHooks_RunEndFiresOnEveryOutcome(t *testing.T) {
	cases := []struct {
		name string
		// reason is the outcome both the Result and the hook must report.
		reason runstate.TerminalReason
		// wantErr is whether the run also returns an error, which the hook sees on Err.
		wantErr bool
		// wantCalls is the model calls the stats snapshot must carry. It is zero on the
		// error case, whose single call failed and so was never counted.
		wantCalls int64
		build     func(t *testing.T, app *agenttest.FakeApp) agent.Options
	}{
		{
			name:      "completed",
			reason:    runstate.ReasonCompleted,
			wantCalls: 1,
			build: func(t *testing.T, app *agenttest.FakeApp) agent.Options {
				return agent.Options{
					Config:     agenttest.Config(t, app),
					ConfigFile: "agent.yaml",
					Prompt:     []string{"go"},
					Provider:   agenttest.NewScriptedProvider(t, agenttest.TextResponse("done")),
				}
			},
		},
		{
			name:      "budget",
			reason:    runstate.ReasonBudget,
			wantErr:   true,
			wantCalls: 1,
			build: func(t *testing.T, app *agenttest.FakeApp) agent.Options {
				resp := agenttest.ToolUseResponse("call-1", "do", json.RawMessage(`{"subject":"x"}`))
				resp.Usage = llm.Usage{In: 100, Out: 100}

				return agent.Options{
					Config:     agenttest.Config(t, app, agenttest.WithMaxTokens(50)),
					ConfigFile: "agent.yaml",
					Prompt:     []string{"go"},
					Provider:   agenttest.NewScriptedProvider(t, resp),
				}
			},
		},
		{
			name:    "error",
			reason:  runstate.ReasonError,
			wantErr: true,
			build: func(t *testing.T, app *agenttest.FakeApp) agent.Options {
				return agent.Options{
					Config:     agenttest.Config(t, app),
					ConfigFile: "agent.yaml",
					Prompt:     []string{"go"},
					// An exhausted provider fails the first model call.
					Provider: agenttest.NewScriptedProvider(t),
				}
			},
		},
		{
			name:      "suspended",
			reason:    runstate.ReasonSuspended,
			wantCalls: 1,
			build: func(t *testing.T, app *agenttest.FakeApp) agent.Options {
				polls := 0

				return agent.Options{
					Config:           agenttest.Config(t, app),
					ConfigFile:       "agent.yaml",
					Prompt:           []string{"go"},
					Provider:         agenttest.NewScriptedProvider(t, agenttest.ToolUseResponse("c1", "do", json.RawMessage(`{"subject":"x"}`))),
					Checkpoint:       agent.Checkpoint{Enabled: true},
					SessionStore:     agenttest.NewFakeSessionStore(t),
					SuspendRequested: func() bool { polls++; return polls > 1 },
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)

			app := agenttest.NewFakeApp(t, exampleApp())
			opts := tc.build(t, app)

			var ends []agent.RunEndInfo
			opts.Hooks = agent.Hooks{
				RunEnd: func(_ context.Context, in agent.RunEndInfo) error {
					ends = append(ends, in)
					return nil
				},
			}

			res, err := agent.Run(context.Background(), opts, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
			g.Expect(res.Reason).To(Equal(tc.reason))

			g.Expect(ends).To(HaveLen(1))
			g.Expect(ends[0].Crashed).To(BeFalse())
			g.Expect(ends[0].Reason).To(Equal(tc.reason))
			g.Expect(ends[0].SessionID).To(Equal(res.SessionID))
			g.Expect(ends[0].Stats.Model).To(Equal("test-model"))
			g.Expect(ends[0].Stats.LlmCalls).To(Equal(tc.wantCalls))

			// The hook sees the run's own error, whatever the outcome carries.
			if tc.wantErr {
				g.Expect(err).To(HaveOccurred())
				g.Expect(ends[0].Err).To(MatchError(err))
			} else {
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(ends[0].Err).To(BeNil())
			}
		})
	}
}

// TestHooks_RunEndFiresOnCrash proves the hook fires on the one exit that is not an
// outcome: a panic on the run goroutine. Reason is empty and Err is nil there, so a hook
// keys off Crashed, and the run still returns the PanicError.
func TestHooks_RunEndFiresOnCrash(t *testing.T) {
	g := NewWithT(t)

	app := agenttest.NewFakeApp(t, exampleApp())

	var ends []agent.RunEndInfo
	res, err := agent.Run(context.Background(), agent.Options{
		Config:     agenttest.Config(t, app),
		ConfigFile: "agent.yaml",
		Prompt:     []string{"go"},
		Provider:   panicProvider{},
		Hooks: agent.Hooks{
			RunEnd: func(_ context.Context, in agent.RunEndInfo) error {
				ends = append(ends, in)
				return nil
			},
		},
	}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))

	var panicErr *agent.PanicError
	g.Expect(errors.As(err, &panicErr)).To(BeTrue())
	g.Expect(res.Reason).To(BeEmpty())

	g.Expect(ends).To(HaveLen(1))
	g.Expect(ends[0].Crashed).To(BeTrue())
	g.Expect(ends[0].Reason).To(BeEmpty())
	g.Expect(ends[0].Err).To(BeNil())
	g.Expect(ends[0].Stats.Model).To(Equal("test-model"))
}

// TestHooks_RunEndStatsAreIsolated proves the stats snapshot is a copy all the way
// down: it is passed by value, but its by-kind counts are a map the caller still reads
// after Run returns, so a hook scribbling on them cannot corrupt the run summary.
func TestHooks_RunEndStatsAreIsolated(t *testing.T) {
	g := NewWithT(t)

	app := agenttest.NewFakeApp(t, exampleApp())

	res, err := agent.Run(context.Background(), agent.Options{
		Config:     agenttest.Config(t, app),
		ConfigFile: "agent.yaml",
		Prompt:     []string{"go"},
		Provider: agenttest.NewScriptedProvider(t,
			agenttest.ToolUseResponse("c1", "do", json.RawMessage(`{"subject":"x"}`)),
			agenttest.TextResponse("done"),
		),
		Hooks: agent.Hooks{
			RunEnd: func(_ context.Context, in agent.RunEndInfo) error {
				g.Expect(in.Stats.ToolCalls).To(Equal(int64(1)))
				g.Expect(in.Stats.ToolCallsByKind).To(HaveLen(1))

				in.Stats.ToolCalls = 99
				for kind := range in.Stats.ToolCallsByKind {
					in.Stats.ToolCallsByKind[kind] = 99
				}
				in.Stats.ToolCallsByKind[toolkit.KindCustom] = 99

				return nil
			},
		},
	}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.Stats.ToolCalls).To(Equal(int64(1)))
	g.Expect(res.Stats.ToolCallsByKind).To(HaveLen(1))
	g.Expect(res.Stats.ToolCallsByKind).NotTo(ContainElement(int64(99)))
}

// TestHooks_RunEndDoesNotFireBeforeRunner proves a setup failure never fires the hook:
// no session ran, so none ended. The denied-prompt case also fixes the guard on the runner
// rather than on res.Reason, which that path does set.
func TestHooks_RunEndDoesNotFireBeforeRunner(t *testing.T) {
	fired := func(t *testing.T, opts agent.Options) bool {
		t.Helper()

		count := 0
		opts.Hooks.RunEnd = func(context.Context, agent.RunEndInfo) error {
			count++
			return nil
		}

		_, err := agent.Run(context.Background(), opts, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(t))
		if err == nil {
			t.Fatal("expected the run to fail during setup")
		}

		return count > 0
	}

	t.Run("invalid options", func(t *testing.T) {
		g := NewWithT(t)

		app := agenttest.NewFakeApp(t, exampleApp())
		g.Expect(fired(t, agent.Options{
			Config:      agenttest.Config(t, app),
			ConfigFile:  "agent.yaml",
			Prompt:      []string{"go"},
			Provider:    agenttest.NewScriptedProvider(t),
			ToolWorkDir: filepath.Join(t.TempDir(), "absent"),
		})).To(BeFalse())
	})

	t.Run("denied initial prompt", func(t *testing.T) {
		g := NewWithT(t)

		app := agenttest.NewFakeApp(t, exampleApp())
		g.Expect(fired(t, agent.Options{
			Config:     agenttest.Config(t, app),
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			Provider:   agenttest.NewScriptedProvider(t),
			Hooks: agent.Hooks{
				UserPromptSubmit: func(context.Context, agent.UserPromptSubmitInfo) (agent.UserPromptSubmitResult, error) {
					return agent.UserPromptSubmitResult{Deny: true, DenyReason: "blocked by policy"}, nil
				},
			},
		})).To(BeFalse())
	})
}

// TestHooks_RunEndFailureIsWarnedNotFatal proves the hook cannot change an outcome
// that is already decided: both a returned error and a panic are downgraded to a
// WarnRunEndHook advisory, and the completed run still completes.
func TestHooks_RunEndFailureIsWarnedNotFatal(t *testing.T) {
	run := func(t *testing.T, hook agent.RunEndHook) (*agent.Result, *agenttest.RecordingEvents, error) {
		t.Helper()

		app := agenttest.NewFakeApp(t, exampleApp())
		events := agenttest.NewRecordingEvents()

		res, err := agent.Run(context.Background(), agent.Options{
			Config:     agenttest.Config(t, app),
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			Provider:   agenttest.NewScriptedProvider(t, agenttest.TextResponse("done")),
			Hooks:      agent.Hooks{RunEnd: hook},
		}, events, agenttest.NewScriptedPrompter(t))

		return res, events, err
	}

	t.Run("error", func(t *testing.T) {
		g := NewWithT(t)

		res, events, err := run(t, func(context.Context, agent.RunEndInfo) error {
			return errors.New("boom")
		})

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.Reason).To(Equal(runstate.ReasonCompleted))
		g.Expect(events.HasWarning(agent.WarnRunEndHook)).To(BeTrue())
	})

	t.Run("panic", func(t *testing.T) {
		g := NewWithT(t)

		res, events, err := run(t, func(context.Context, agent.RunEndInfo) error {
			panic("boom in the hook")
		})

		// The panic is contained where it happens: the run is not converted to a crash.
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(res.Reason).To(Equal(runstate.ReasonCompleted))
		g.Expect(events.HasWarning(agent.WarnRunEndHook)).To(BeTrue())
		g.Expect(events.Panics()).To(BeEmpty())
	})
}

// TestHooks_PanickingRunEndDuringCrashDoesNotEscape drives the worst case: the run
// crashes, and the RunEnd hook the barrier then runs panics too. The second panic must
// not escape the barrier that already recovered the first, which still becomes the
// returned PanicError.
func TestHooks_PanickingRunEndDuringCrashDoesNotEscape(t *testing.T) {
	g := NewWithT(t)

	app := agenttest.NewFakeApp(t, exampleApp())
	events := agenttest.NewRecordingEvents()

	res, err := agent.Run(context.Background(), agent.Options{
		Config:     agenttest.Config(t, app),
		ConfigFile: "agent.yaml",
		Prompt:     []string{"go"},
		Provider:   panicProvider{},
		Hooks: agent.Hooks{
			RunEnd: func(context.Context, agent.RunEndInfo) error {
				panic("boom in the hook")
			},
		},
	}, events, agenttest.NewScriptedPrompter(t))

	var panicErr *agent.PanicError
	g.Expect(errors.As(err, &panicErr)).To(BeTrue())
	g.Expect(res.Reason).To(BeEmpty())

	// The run's own crash still reaches the events sink with its stack.
	panics := events.Panics()
	g.Expect(panics).To(HaveLen(1))
	g.Expect(panics[0].Stack).NotTo(BeEmpty())
	g.Expect(events.HasWarning(agent.WarnRunEndHook)).To(BeTrue())
}
