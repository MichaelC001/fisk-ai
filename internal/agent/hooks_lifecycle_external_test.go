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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// runEndOutcome is one way a run that reached the runner can end.
type runEndOutcome struct {
	// reason is the outcome both the Result and the hook must report.
	reason runstate.TerminalReason
	// wantErr is whether the run also returns an error, which the hook sees on Err.
	wantErr bool
	// wantCalls is the model calls the stats snapshot must carry. It is zero on the
	// error case, whose single call failed and so was never counted.
	wantCalls int64
	build     func(testing.TB, *agenttest.FakeApp) agent.Options
}

var _ = Describe("the run lifecycle hooks", func() {
	// The run-entry hooks fire once, in order, and carry the run's resolved parameters on
	// a fresh checkpointed run.
	It("Should fire RunStart then the initial prompt on run entry", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())

		var order []string
		var start agent.RunStartInfo
		var submit agent.UserPromptSubmitInfo

		res, err := agent.Run(context.Background(), agent.Options{
			Config:       agenttest.Config(GinkgoTB(), app),
			ConfigFile:   "agent.yaml",
			Prompt:       []string{"go"},
			Provider:     agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
			Checkpoint:   agent.Checkpoint{Enabled: true},
			SessionStore: agenttest.NewFakeSessionStore(GinkgoTB()),
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
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))

		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

		// RunStart fires before the initial prompt, exactly once each.
		Expect(order).To(Equal([]string{"session-start", "prompt-submit"}))

		Expect(start.Resumed).To(BeFalse())
		Expect(start.Interactive).To(BeFalse())
		Expect(start.Model).To(Equal("test-model"))
		Expect(start.SessionID).To(Equal(res.SessionID))
		Expect(start.SessionID).NotTo(BeEmpty())
		Expect(start.ToolNames).To(ContainElement("do"))

		Expect(submit.Initial).To(BeTrue())
		Expect(submit.Text).To(Equal("go"))
	})

	// A denied initial prompt stops the run before any session is created (no orphan) or
	// model call is made, and surfaces the deny reason as an error.
	It("Should abort a denied initial prompt before the session is created", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		store := agenttest.NewFakeSessionStore(GinkgoTB())

		_, err := agent.Run(context.Background(), agent.Options{
			Config:     agenttest.Config(GinkgoTB(), app),
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			// No scripted responses: a model call would error, proving none is made.
			Provider:     agenttest.NewScriptedProvider(GinkgoTB()),
			Checkpoint:   agent.Checkpoint{Enabled: true},
			SessionStore: store,
			Hooks: agent.Hooks{
				UserPromptSubmit: func(context.Context, agent.UserPromptSubmitInfo) (agent.UserPromptSubmitResult, error) {
					return agent.UserPromptSubmitResult{Deny: true, DenyReason: "blocked by policy"}, nil
				},
			},
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))

		Expect(err).To(MatchError(ContainSubstring("initial prompt was rejected")))
		Expect(err).To(MatchError(ContainSubstring("blocked by policy")))

		// No session was created, so nothing is left to resume.
		infos, lerr := store.List(context.Background())
		Expect(lerr).NotTo(HaveOccurred())
		Expect(infos).To(BeEmpty())
	})

	// A RunStart error aborts the run before any session is created.
	It("Should abort a RunStart error before the session is created", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		store := agenttest.NewFakeSessionStore(GinkgoTB())

		_, err := agent.Run(context.Background(), agent.Options{
			Config:       agenttest.Config(GinkgoTB(), app),
			ConfigFile:   "agent.yaml",
			Prompt:       []string{"go"},
			Provider:     agenttest.NewScriptedProvider(GinkgoTB()),
			Checkpoint:   agent.Checkpoint{Enabled: true},
			SessionStore: store,
			Hooks: agent.Hooks{
				RunStart: func(context.Context, agent.RunStartInfo) error {
					return errors.New("boom")
				},
			},
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))

		Expect(err).To(MatchError(ContainSubstring("RunStart hook")))

		infos, lerr := store.List(context.Background())
		Expect(lerr).NotTo(HaveOccurred())
		Expect(infos).To(BeEmpty())
	})

	// A resume fires RunStart with Resumed true and does NOT re-fire the initial
	// UserPromptSubmit, since the reconstructed history is not a fresh prompt.
	It("Should fire RunStart resumed with no initial prompt on a resume", func() {
		ctx := context.Background()

		store := agenttest.NewFakeSessionStore(GinkgoTB())
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())

		// Run 1: one tool call, then a suspend at the next boundary, journaled to the store.
		suspendPolls := 0
		res1, err := agent.Run(ctx, agent.Options{
			Config:           agenttest.Config(GinkgoTB(), app),
			ConfigFile:       "agent.yaml",
			Prompt:           []string{"start"},
			Provider:         agenttest.NewScriptedProvider(GinkgoTB(), agenttest.ToolUseResponse("c1", "do", json.RawMessage(`{"subject":"x"}`))),
			Checkpoint:       agent.Checkpoint{Enabled: true},
			SessionStore:     store,
			SuspendRequested: func() bool { suspendPolls++; return suspendPolls > 1 },
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res1.Reason).To(Equal(runstate.ReasonSuspended))

		// Run 2: resume the saved session, recording the lifecycle hooks.
		var starts []agent.RunStartInfo
		var submits []agent.UserPromptSubmitInfo
		res2, err := agent.Run(ctx, agent.Options{
			Config:       agenttest.Config(GinkgoTB(), app),
			ConfigFile:   "agent.yaml",
			Provider:     agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("finished")),
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
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res2.Reason).To(Equal(runstate.ReasonCompleted))

		// RunStart re-fires with Resumed true; the initial prompt does not re-fire.
		Expect(starts).To(HaveLen(1))
		Expect(starts[0].Resumed).To(BeTrue())
		Expect(starts[0].SessionID).To(Equal(res1.SessionID))
		Expect(submits).To(BeEmpty())
	})

	// RunEnd fires exactly once for every way a run that reached the runner can end - a
	// completed answer, an exhausted token budget, a failing turn, and a graceful suspend -
	// carrying that outcome's reason and a stats snapshot, and reporting none of them as a
	// crash.
	DescribeTable("Should fire RunEnd on every outcome",
		func(tc runEndOutcome) {
			app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
			opts := tc.build(GinkgoTB(), app)

			var ends []agent.RunEndInfo
			opts.Hooks = agent.Hooks{
				RunEnd: func(_ context.Context, in agent.RunEndInfo) error {
					ends = append(ends, in)
					return nil
				},
			}

			res, err := agent.Run(context.Background(), opts, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
			Expect(res.Reason).To(Equal(tc.reason))

			Expect(ends).To(HaveLen(1))
			Expect(ends[0].Crashed).To(BeFalse())
			Expect(ends[0].Reason).To(Equal(tc.reason))
			Expect(ends[0].SessionID).To(Equal(res.SessionID))
			Expect(ends[0].Stats.Model).To(Equal("test-model"))
			Expect(ends[0].Stats.LlmCalls).To(Equal(tc.wantCalls))

			// The hook sees the run's own error, whatever the outcome carries.
			if tc.wantErr {
				Expect(err).To(HaveOccurred())
				Expect(ends[0].Err).To(MatchError(err))
			} else {
				Expect(err).NotTo(HaveOccurred())
				Expect(ends[0].Err).To(BeNil())
			}
		},
		Entry("completed", runEndOutcome{
			reason:    runstate.ReasonCompleted,
			wantCalls: 1,
			build: func(tb testing.TB, app *agenttest.FakeApp) agent.Options {
				return agent.Options{
					Config:     agenttest.Config(tb, app),
					ConfigFile: "agent.yaml",
					Prompt:     []string{"go"},
					Provider:   agenttest.NewScriptedProvider(tb, agenttest.TextResponse("done")),
				}
			},
		}),
		Entry("budget", runEndOutcome{
			reason:    runstate.ReasonBudget,
			wantErr:   true,
			wantCalls: 1,
			build: func(tb testing.TB, app *agenttest.FakeApp) agent.Options {
				resp := agenttest.ToolUseResponse("call-1", "do", json.RawMessage(`{"subject":"x"}`))
				resp.Usage = llm.Usage{In: 100, Out: 100}

				return agent.Options{
					Config:     agenttest.Config(tb, app, agenttest.WithMaxTokens(50)),
					ConfigFile: "agent.yaml",
					Prompt:     []string{"go"},
					Provider:   agenttest.NewScriptedProvider(tb, resp),
				}
			},
		}),
		Entry("error", runEndOutcome{
			reason:  runstate.ReasonError,
			wantErr: true,
			build: func(tb testing.TB, app *agenttest.FakeApp) agent.Options {
				return agent.Options{
					Config:     agenttest.Config(tb, app),
					ConfigFile: "agent.yaml",
					Prompt:     []string{"go"},
					// An exhausted provider fails the first model call.
					Provider: agenttest.NewScriptedProvider(tb),
				}
			},
		}),
		Entry("suspended", runEndOutcome{
			reason:    runstate.ReasonSuspended,
			wantCalls: 1,
			build: func(tb testing.TB, app *agenttest.FakeApp) agent.Options {
				polls := 0

				return agent.Options{
					Config:           agenttest.Config(tb, app),
					ConfigFile:       "agent.yaml",
					Prompt:           []string{"go"},
					Provider:         agenttest.NewScriptedProvider(tb, agenttest.ToolUseResponse("c1", "do", json.RawMessage(`{"subject":"x"}`))),
					Checkpoint:       agent.Checkpoint{Enabled: true},
					SessionStore:     agenttest.NewFakeSessionStore(tb),
					SuspendRequested: func() bool { polls++; return polls > 1 },
				}
			},
		}),
	)

	// The hook fires on the one exit that is not an outcome: a panic on the run goroutine.
	// Reason is empty and Err is nil there, so a hook keys off Crashed, and the run still
	// returns the PanicError.
	It("Should fire RunEnd on a crash", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())

		var ends []agent.RunEndInfo
		res, err := agent.Run(context.Background(), agent.Options{
			Config:     agenttest.Config(GinkgoTB(), app),
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			Provider:   panicProvider{},
			Hooks: agent.Hooks{
				RunEnd: func(_ context.Context, in agent.RunEndInfo) error {
					ends = append(ends, in)
					return nil
				},
			},
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))

		var panicErr *agent.PanicError
		Expect(errors.As(err, &panicErr)).To(BeTrue())
		Expect(res.Reason).To(BeEmpty())

		Expect(ends).To(HaveLen(1))
		Expect(ends[0].Crashed).To(BeTrue())
		Expect(ends[0].Reason).To(BeEmpty())
		Expect(ends[0].Err).To(BeNil())
		Expect(ends[0].Stats.Model).To(Equal("test-model"))
	})

	// The stats snapshot is a copy all the way down: it is passed by value, but its
	// by-kind counts are a map the caller still reads after Run returns, so a hook
	// scribbling on them cannot corrupt the run summary.
	It("Should isolate the RunEnd stats snapshot", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())

		res, err := agent.Run(context.Background(), agent.Options{
			Config:     agenttest.Config(GinkgoTB(), app),
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			Provider: agenttest.NewScriptedProvider(GinkgoTB(),
				agenttest.ToolUseResponse("c1", "do", json.RawMessage(`{"subject":"x"}`)),
				agenttest.TextResponse("done"),
			),
			Hooks: agent.Hooks{
				RunEnd: func(_ context.Context, in agent.RunEndInfo) error {
					Expect(in.Stats.ToolCalls).To(Equal(int64(1)))
					Expect(in.Stats.ToolCallsByKind).To(HaveLen(1))

					in.Stats.ToolCalls = 99
					for kind := range in.Stats.ToolCallsByKind {
						in.Stats.ToolCallsByKind[kind] = 99
					}
					in.Stats.ToolCallsByKind[toolkit.KindCustom] = 99

					return nil
				},
			},
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))

		Expect(err).NotTo(HaveOccurred())
		Expect(res.Stats.ToolCalls).To(Equal(int64(1)))
		Expect(res.Stats.ToolCallsByKind).To(HaveLen(1))
		Expect(res.Stats.ToolCallsByKind).NotTo(ContainElement(int64(99)))
	})

	// A setup failure never fires the hook: no session ran, so none ended. The
	// denied-prompt case also fixes the guard on the runner rather than on res.Reason,
	// which that path does set.
	Describe("RunEnd before the runner", func() {
		fired := func(tb testing.TB, opts agent.Options) bool {
			tb.Helper()

			count := 0
			opts.Hooks.RunEnd = func(context.Context, agent.RunEndInfo) error {
				count++
				return nil
			}

			_, err := agent.Run(context.Background(), opts, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(tb))
			Expect(err).To(HaveOccurred(), "expected the run to fail during setup")

			return count > 0
		}

		It("Should not fire for invalid options", func() {
			app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
			Expect(fired(GinkgoTB(), agent.Options{
				Config:      agenttest.Config(GinkgoTB(), app),
				ConfigFile:  "agent.yaml",
				Prompt:      []string{"go"},
				Provider:    agenttest.NewScriptedProvider(GinkgoTB()),
				ToolWorkDir: filepath.Join(GinkgoT().TempDir(), "absent"),
			})).To(BeFalse())
		})

		It("Should not fire for a denied initial prompt", func() {
			app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
			Expect(fired(GinkgoTB(), agent.Options{
				Config:     agenttest.Config(GinkgoTB(), app),
				ConfigFile: "agent.yaml",
				Prompt:     []string{"go"},
				Provider:   agenttest.NewScriptedProvider(GinkgoTB()),
				Hooks: agent.Hooks{
					UserPromptSubmit: func(context.Context, agent.UserPromptSubmitInfo) (agent.UserPromptSubmitResult, error) {
						return agent.UserPromptSubmitResult{Deny: true, DenyReason: "blocked by policy"}, nil
					},
				},
			})).To(BeFalse())
		})
	})

	// The hook cannot change an outcome that is already decided: both a returned error and
	// a panic are downgraded to a WarnRunEndHook advisory, and the completed run still
	// completes.
	Describe("a failing RunEnd hook", func() {
		run := func(tb testing.TB, hook agent.RunEndHook) (*agent.Result, *agenttest.RecordingEvents, error) {
			tb.Helper()

			app := agenttest.NewFakeApp(tb, exampleApp())
			events := agenttest.NewRecordingEvents()

			res, err := agent.Run(context.Background(), agent.Options{
				Config:     agenttest.Config(tb, app),
				ConfigFile: "agent.yaml",
				Prompt:     []string{"go"},
				Provider:   agenttest.NewScriptedProvider(tb, agenttest.TextResponse("done")),
				Hooks:      agent.Hooks{RunEnd: hook},
			}, events, agenttest.NewScriptedPrompter(tb))

			return res, events, err
		}

		It("Should be warned rather than fatal on an error", func() {
			res, events, err := run(GinkgoTB(), func(context.Context, agent.RunEndInfo) error {
				return errors.New("boom")
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(res.Reason).To(Equal(runstate.ReasonCompleted))
			Expect(events.HasWarning(agent.WarnRunEndHook)).To(BeTrue())
		})

		It("Should be warned rather than fatal on a panic", func() {
			res, events, err := run(GinkgoTB(), func(context.Context, agent.RunEndInfo) error {
				panic("boom in the hook")
			})

			// The panic is contained where it happens: the run is not converted to a crash.
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Reason).To(Equal(runstate.ReasonCompleted))
			Expect(events.HasWarning(agent.WarnRunEndHook)).To(BeTrue())
			Expect(events.Panics()).To(BeEmpty())
		})
	})

	// This drives the worst case: the run crashes, and the RunEnd hook the barrier then
	// runs panics too. The second panic must not escape the barrier that already recovered
	// the first, which still becomes the returned PanicError.
	It("Should contain a panicking RunEnd during a crash", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		events := agenttest.NewRecordingEvents()

		res, err := agent.Run(context.Background(), agent.Options{
			Config:     agenttest.Config(GinkgoTB(), app),
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			Provider:   panicProvider{},
			Hooks: agent.Hooks{
				RunEnd: func(context.Context, agent.RunEndInfo) error {
					panic("boom in the hook")
				},
			},
		}, events, agenttest.NewScriptedPrompter(GinkgoTB()))

		var panicErr *agent.PanicError
		Expect(errors.As(err, &panicErr)).To(BeTrue())
		Expect(res.Reason).To(BeEmpty())

		// The run's own crash still reaches the events sink with its stack.
		panics := events.Panics()
		Expect(panics).To(HaveLen(1))
		Expect(panics[0].Stack).NotTo(BeEmpty())
		Expect(events.HasWarning(agent.WarnRunEndHook)).To(BeTrue())
	})
})
