//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// These examples live in the external agent_test package on purpose: they can reach
// only agent's exported API, so they are proof that agent.Run is drivable from
// outside the package, which is the whole premise of the concurrent-caller work.
// Each reads as documentation of one supported composition built from the
// internal/agenttest harness. Cases needing a broker or a TCP bind are
// Integration-tagged and land elsewhere.
package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/choria-io/fisk"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	wire "github.com/choria-io/fisk-ai/internal/a2a/wire/v1"
	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/memory/file"
	"github.com/choria-io/fisk-ai/internal/rag"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/toolkit/functool"
)

// exampleConfirmApp is exampleApp with its command gated behind ai:confirm, so a run
// must have the call approved before the command executes.
func exampleConfirmApp() *fisk.Application {
	app := fisk.New("app", "an app")
	do := app.Command("do", "do a thing").Tag("ai:confirm")
	do.Flag("level", "log level").Enum("debug", "info", "warn")
	do.Arg("subject", "the subject").Required().String()
	return app
}

// examplePolicyApp is exampleApp plus a destructive command, so a policy hook has both
// something to deny and something to let through.
func examplePolicyApp() *fisk.Application {
	app := fisk.New("app", "an app")
	do := app.Command("do", "do a thing")
	do.Flag("level", "log level").Enum("debug", "info", "warn")
	do.Arg("subject", "the subject").Required().String()
	wipe := app.Command("wipe", "delete everything")
	wipe.Arg("target", "what to delete").Required().String()
	return app
}

// exampleApp is a small fisk application with one runnable command carrying a flag
// and a required argument, so the tool it becomes has a genuine input schema.
func exampleApp() *fisk.Application {
	app := fisk.New("app", "an app")
	do := app.Command("do", "do a thing")
	do.Flag("level", "log level").Enum("debug", "info", "warn")
	do.Arg("subject", "the subject").Required().String()
	return app
}

// panicProvider panics on every model call, to exercise Run's panic barrier.
type panicProvider struct{}

func (panicProvider) Call(context.Context, llm.Request) (*llm.Response, error) {
	panic("boom in the model call")
}
func (panicProvider) Capabilities() llm.Caps { return llm.Caps{Provider: "anthropic"} }

// panickyEvents panics inside Panicked, to prove the barrier's inner recover keeps a
// misbehaving Events sink from escaping and crashing the process it protects.
type panickyEvents struct{ *agenttest.RecordingEvents }

func (panickyEvents) Panicked(any, []byte) { panic("boom in the events sink") }

// requestToolResults collects the tool_result blocks of a request's messages, keyed by
// the tool_use id each answers, so an example can assert what the model was actually
// told about a call rather than only what ran.
func requestToolResults(req llm.Request) map[string]llm.ToolResultBlock {
	out := map[string]llm.ToolResultBlock{}
	for _, msg := range req.Messages {
		for _, block := range msg.Content {
			if block.ToolResult == nil {
				continue
			}
			out[block.ToolResult.ToolUseID] = *block.ToolResult
		}
	}

	return out
}

var _ = Describe("the agent example compositions", func() {
	// Setup 1: application tools, a scripted provider that answers once with a final
	// text turn, asserting the terminal result and stats.
	It("Should complete a minimal one-shot run", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		provider := agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("all done"))
		events := agenttest.NewRecordingEvents()

		cfg := agenttest.Config(GinkgoTB(), app)
		opts := agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"summarize the widget inventory"},
			Provider:   provider,
		}

		res, err := agent.Run(context.Background(), opts, events, agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))
		Expect(res.Stats.LlmCalls).To(BeNumerically("==", 1))

		final, ok := events.FinalMessage()
		Expect(ok).To(BeTrue())
		Expect(final.Content[0].Text.Text).To(Equal("all done"))

		// The operator's prompt is the first user turn the model is asked to act on, and the
		// application tool is offered alongside it.
		reqs := provider.Requests()
		Expect(reqs).To(HaveLen(1))
		Expect(reqs[0].Messages[0].Role).To(Equal(llm.RoleUser))
		Expect(reqs[0].Messages[0].Content[0].Text.Text).To(Equal("summarize the widget inventory"))
		Expect(reqs[0].Tools).NotTo(BeEmpty())
	})

	// Setup 2: the provider returns a tool_use, the tool executes, its result feeds
	// back, and a second turn completes the run.
	It("Should complete a tool round trip", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		input := json.RawMessage(`{"level":"info","subject":"hello"}`)
		provider := agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("call-1", "do", input),
			agenttest.TextResponse("finished"),
		)
		events := agenttest.NewRecordingEvents()

		cfg := agenttest.Config(GinkgoTB(), app)
		opts := agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"run do at info level against hello"},
			Provider:   provider,
		}

		res, err := agent.Run(context.Background(), opts, events, agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))
		Expect(res.Stats.LlmCalls).To(BeNumerically("==", 2))

		// The fake application echoes its arguments, so the result carries the subject.
		results := events.ToolResults()
		Expect(results).To(HaveLen(1))
		Expect(results[0].IsError).To(BeFalse())
		Expect(results[0].Output).To(ContainSubstring("hello"))

		// The first request opened with the operator's prompt; the second carried the tool
		// result back as a user turn, keyed to the tool_use id the model chose.
		reqs := provider.Requests()
		Expect(reqs).To(HaveLen(2))
		Expect(reqs[0].Messages[0].Content[0].Text.Text).To(Equal("run do at info level against hello"))
		last := reqs[1].Messages[len(reqs[1].Messages)-1]
		Expect(last.Role).To(Equal(llm.RoleUser))
		Expect(last.Content[0].ToolResult).NotTo(BeNil())
		Expect(last.Content[0].ToolResult.ToolUseID).To(Equal("call-1"))
	})

	// Drives a run against a caller-owned read-only knowledge store (Options.RAGStore)
	// and proves Run borrows it: the run completes and the store is still open
	// afterward, since Run did not close a store it did not open.
	It("Should borrow a shared RAG store", func() {
		ctx := context.Background()

		// A one-document corpus, indexed once into a read-only store the caller owns.
		corpus := GinkgoT().TempDir()
		Expect(os.WriteFile(filepath.Join(corpus, "note.md"), []byte("the widget inventory is managed here"), 0o600)).To(Succeed())

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		cfg.Harness.RAG = &config.RAGConfig{Enabled: true, Directory: filepath.Join(GinkgoT().TempDir(), "kb"), Paths: []string{corpus}}

		writer, err := rag.OpenWriter(cfg, "", rag.Options{})
		Expect(err).NotTo(HaveOccurred())
		_, err = writer.Index(ctx, []string{corpus}, rag.IndexOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(writer.Close()).To(Succeed())

		store, err := rag.Open(cfg, "", rag.Options{})
		Expect(err).NotTo(HaveOccurred())
		defer store.Close()
		Expect(store.Built()).To(BeTrue())

		provider := agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done"))
		events := agenttest.NewRecordingEvents()
		opts := agent.Options{Config: cfg, ConfigFile: "agent.yaml", Prompt: []string{"go"}, Provider: provider, RAGStore: store}

		res, err := agent.Run(ctx, opts, events, agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

		// The borrowed store survives the run: a close would have dropped the db handle and
		// flipped Built to false.
		Expect(store.Built()).To(BeTrue())
	})

	// Setup 11, the acceptance gate: N runs execute concurrently in one process, each
	// with its own ToolWorkDir and StoreDir, and none sees another's tool working
	// directory or store. This is the composition the whole concurrent-caller effort
	// exists to make safe (items 1, 2 and 5), driven entirely through the exported Run
	// API.
	It("Should run concurrently with no crosstalk", func() {
		const n = 6
		// One shared fake application, as a server holds one byName tool set across runs.
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())

		// Everything that touches the test handle is built on the spec goroutine up front;
		// the goroutines below only call agent.Run.
		toolDirs := make([]string, n)
		storeDirs := make([]string, n)
		providers := make([]*agenttest.ScriptedProvider, n)
		prompters := make([]*agenttest.ScriptedPrompter, n)
		events := make([]*agenttest.RecordingEvents, n)
		cfgs := make([]*config.Config, n)
		for i := 0; i < n; i++ {
			toolDirs[i] = GinkgoT().TempDir()
			storeDirs[i] = GinkgoT().TempDir()
			providers[i] = agenttest.NewScriptedProvider(GinkgoTB(),
				agenttest.ToolUseResponse("c1", "do", json.RawMessage(`{"subject":"x"}`)),
				agenttest.TextResponse("done"),
			)
			prompters[i] = agenttest.NewScriptedPrompter(GinkgoTB())
			events[i] = agenttest.NewRecordingEvents()
			cfgs[i] = agenttest.Config(GinkgoTB(), app, agenttest.WithMemory())
		}

		results := make([]*agent.Result, n)
		errs := make([]error, n)
		outputs := make([]string, n)

		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			i := i
			wg.Add(1)
			go func() {
				defer wg.Done()
				opts := agent.Options{
					Config:      cfgs[i],
					ConfigFile:  "agent.yaml",
					Prompt:      []string{"go"},
					Provider:    providers[i],
					ToolWorkDir: toolDirs[i],
					StoreDir:    storeDirs[i],
				}
				results[i], errs[i] = agent.Run(context.Background(), opts, events[i], prompters[i])
				if r := events[i].ToolResults(); len(r) == 1 {
					outputs[i] = r[0].Output
				}
			}()
		}
		wg.Wait()

		for i := 0; i < n; i++ {
			Expect(errs[i]).NotTo(HaveOccurred(), "run %d", i)
			Expect(results[i].Reason).To(Equal(runstate.ReasonCompleted), "run %d", i)

			// Nothing in these runs should reach the prompter: the tool is not gated and no
			// human-in-the-loop builtin is enabled. A fake reached with nothing to answer
			// records a fault and lets the run continue, so the spec reads them back here
			// rather than letting one pass unnoticed.
			Expect(prompters[i].ScriptingFaults()).To(BeEmpty(), "run %d", i)

			// The tool ran in this run's own working directory (commandEnv sets PWD to it),
			// and in none of the siblings'.
			Expect(outputs[i]).To(ContainSubstring("PWD=" + toolDirs[i]))
			for j := 0; j < n; j++ {
				if j != i {
					Expect(outputs[i]).NotTo(ContainSubstring("PWD=" + toolDirs[j]))
				}
			}

			// This run's memory store landed under its own StoreDir, isolated from the others.
			Expect(filepath.Join(storeDirs[i], "memory", "agent")).To(BeADirectory())
		}
	})

	// Setup 3: with memory enabled, the model calls the memory_write built-in, and the
	// memory it writes lands in this run's store under its StoreDir.
	It("Should write a memory", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		write := json.RawMessage(`{"key":"build.notes","description":"how the build works","content":"run abt t u"}`)
		provider := agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("c1", "memory_write", write),
			agenttest.TextResponse("saved"),
		)
		events := agenttest.NewRecordingEvents()

		storeDir := GinkgoT().TempDir()
		cfg := agenttest.Config(GinkgoTB(), app, agenttest.WithMemory())
		opts := agent.Options{Config: cfg, ConfigFile: "agent.yaml", Prompt: []string{"remember the build"}, Provider: provider, StoreDir: storeDir}

		res, err := agent.Run(context.Background(), opts, events, agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

		data, err := os.ReadFile(filepath.Join(storeDir, "memory", "agent", "build.notes.md"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(data)).To(ContainSubstring("run abt t u"))
	})

	// Setup 7: with human_in_the_loop enabled, the model calls ask_human_confirm, which
	// routes to the scripted prompter, and the run completes with the operator's answer.
	It("Should confirm with a human in the loop", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		provider := agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("c1", "ask_human_confirm", json.RawMessage(`{"question":"Proceed?"}`)),
			agenttest.TextResponse("done"),
		)
		events := agenttest.NewRecordingEvents()

		prompter := agenttest.NewScriptedPrompter(GinkgoTB())
		var asked string
		prompter.ConfirmFn = func(q string) (bool, error) {
			asked = q
			return true, nil
		}

		cfg := agenttest.Config(GinkgoTB(), app, agenttest.WithHITL())
		opts := agent.Options{Config: cfg, ConfigFile: "agent.yaml", Prompt: []string{"ask me first"}, Provider: provider}

		res, err := agent.Run(context.Background(), opts, events, prompter)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))
		Expect(asked).To(Equal("Proceed?"))

		results := events.ToolResults()
		Expect(results).To(HaveLen(1))
		Expect(results[0].Output).To(ContainSubstring(`"confirmed":true`))
	})

	// Setup 9: a run driven past its first completed turn by Options.NextPrompt, which
	// supplies one follow-up and then ends the session.
	It("Should continue interactively", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		provider := agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.TextResponse("first answer"),
			agenttest.TextResponse("second answer"),
		)
		events := agenttest.NewRecordingEvents()

		calls := 0
		next := func(context.Context) agent.Continuation {
			calls++
			if calls == 1 {
				return agent.Continuation{Text: "and then?", Continue: true}
			}
			return agent.Continuation{Continue: false}
		}

		cfg := agenttest.Config(GinkgoTB(), app)
		opts := agent.Options{Config: cfg, ConfigFile: "agent.yaml", Prompt: []string{"start"}, Provider: provider, NextPrompt: next}

		res, err := agent.Run(context.Background(), opts, events, agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))
		// Two turns ran: the initial prompt and the one follow-up before the session ended.
		Expect(res.Stats.LlmCalls).To(BeNumerically("==", 2))
		Expect(calls).To(Equal(2))
	})

	// Setup 8: a checkpointed run does one tool call, suspends at the next loop
	// boundary, and a second Run resumes the saved session to completion. It exercises
	// suspend/resume through the exported Run entry point (the runner-level path is
	// tested elsewhere).
	It("Should suspend and resume a checkpointed run", func() {
		ctx := context.Background()

		storeDir := GinkgoT().TempDir()
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())

		// Run 1: one tool call, then a suspend is requested at the next boundary.
		suspendPolls := 0
		opts1 := agent.Options{
			Config:           agenttest.Config(GinkgoTB(), app),
			ConfigFile:       "agent.yaml",
			Prompt:           []string{"start work"},
			Provider:         agenttest.NewScriptedProvider(GinkgoTB(), agenttest.ToolUseResponse("c1", "do", json.RawMessage(`{"subject":"x"}`))),
			StoreDir:         storeDir,
			Checkpoint:       agent.Checkpoint{Enabled: true},
			SuspendRequested: func() bool { suspendPolls++; return suspendPolls > 1 },
		}
		res1, err := agent.Run(ctx, opts1, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res1.Reason).To(Equal(runstate.ReasonSuspended))
		Expect(res1.SessionID).NotTo(BeEmpty())

		// Run 2: resume the saved session (same StoreDir) to a final answer.
		opts2 := agent.Options{
			Config:     agenttest.Config(GinkgoTB(), app),
			ConfigFile: "agent.yaml",
			Provider:   agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("finished")),
			StoreDir:   storeDir,
			Checkpoint: agent.Checkpoint{ResumeID: res1.SessionID},
		}
		res2, err := agent.Run(ctx, opts2, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res2.Reason).To(Equal(runstate.ReasonCompleted))
		Expect(res2.SessionID).To(Equal(res1.SessionID))
	})

	// Proves the panic barrier: a panic on the run goroutine is recovered and returned
	// as a distinguishable *PanicError (not a terminal outcome), the stack reaches the
	// Events sink, and the returned error carries no stack.
	It("Should recover a panic", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		events := agenttest.NewRecordingEvents()

		cfg := agenttest.Config(GinkgoTB(), app)
		opts := agent.Options{Config: cfg, ConfigFile: "agent.yaml", Prompt: []string{"go"}, Provider: panicProvider{}}

		res, err := agent.Run(context.Background(), opts, events, agenttest.NewScriptedPrompter(GinkgoTB()))

		// The crash is distinguishable from an outcome, and res.Reason stays unset.
		var panicErr *agent.PanicError
		Expect(errors.As(err, &panicErr)).To(BeTrue())
		Expect(res).NotTo(BeNil())
		Expect(res.Reason).To(BeEmpty())

		// The stack was delivered to the Events sink, and not leaked onto the returned error.
		panics := events.Panics()
		Expect(panics).To(HaveLen(1))
		Expect(panics[0].Stack).NotTo(BeEmpty())
		Expect(panicErr.Error()).NotTo(ContainSubstring("boom"))
	})

	// Drives a crash whose Events.Panicked itself panics; Run must still return a
	// PanicError rather than letting the second panic take down the process.
	It("Should not let a panicking events sink escape", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		opts := agent.Options{Config: cfg, ConfigFile: "agent.yaml", Prompt: []string{"go"}, Provider: panicProvider{}}

		res, err := agent.Run(context.Background(), opts, panickyEvents{agenttest.NewRecordingEvents()}, agenttest.NewScriptedPrompter(GinkgoTB()))

		var panicErr *agent.PanicError
		Expect(errors.As(err, &panicErr)).To(BeTrue())
		Expect(res).NotTo(BeNil())
	})

	// Setup 5 plus item 5's core property: a scripted prompter that reports it can
	// prompt (a stand-in for a non-terminal operator channel like Slack) approves a
	// confirm-gated tool, so the tool runs even though the spec has no TTY.
	// Interactivity follows the prompter's CanPrompt, not the terminal, and the
	// no-operator advisory does not fire.
	It("Should approve a confirm gate over a non-TTY channel", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleConfirmApp())
		input := json.RawMessage(`{"level":"info","subject":"hello"}`)
		provider := agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("call-1", "do", input),
			agenttest.TextResponse("done"),
		)
		events := agenttest.NewRecordingEvents()

		prompter := agenttest.NewScriptedPrompter(GinkgoTB())
		approved := false
		prompter.ApproveFn = func(toolkit.GateRequest) (toolkit.ConfirmChoice, error) {
			approved = true
			return toolkit.ConfirmOnce, nil
		}

		cfg := agenttest.Config(GinkgoTB(), app)
		opts := agent.Options{Config: cfg, ConfigFile: "agent.yaml", Prompt: []string{"do it"}, Provider: provider}

		res, err := agent.Run(context.Background(), opts, events, prompter)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))
		Expect(approved).To(BeTrue())
		// The gated tool ran (its result came back) and no "no operator" advisory fired.
		Expect(events.ToolResults()).To(HaveLen(1))
		Expect(events.HasWarning(agent.WarnConfirmNoTerminal)).To(BeFalse())
	})

	// Setup 6: with no operator reachable, a confirm-gated tool is declined without
	// running and the no-operator advisory fires. This is the composition the job
	// system uses.
	It("Should deny a confirm gate when there is no operator", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleConfirmApp())
		input := json.RawMessage(`{"level":"info","subject":"hello"}`)
		provider := agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("call-1", "do", input),
			agenttest.TextResponse("done"),
		)
		events := agenttest.NewRecordingEvents()

		// NoOperator makes CanPrompt report false; the ApproveFn is left unset because the
		// gate must deny before ever reaching it.
		prompter := agenttest.NewScriptedPrompter(GinkgoTB()).NoOperator()

		cfg := agenttest.Config(GinkgoTB(), app)
		opts := agent.Options{Config: cfg, ConfigFile: "agent.yaml", Prompt: []string{"do it"}, Provider: provider}

		res, err := agent.Run(context.Background(), opts, events, prompter)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))
		// The advisory fired at start, and the gated tool never ran, so no tool result was
		// traced (the model receives an authoritative denial in the conversation instead).
		Expect(events.HasWarning(agent.WarnConfirmNoTerminal)).To(BeTrue())
		Expect(events.ToolResults()).To(BeEmpty())
	})

	// Setup 4 (the empty-index case): with knowledge enabled and a StoreDir in effect
	// but nothing indexed under it, the run completes but warns loudly rather than
	// silently starting with an empty knowledge base (the silent-mismatch trap StoreDir
	// could otherwise introduce).
	It("Should warn about a knowledge store base with nothing indexed", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		provider := agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done"))
		events := agenttest.NewRecordingEvents()

		cfg := agenttest.Config(GinkgoTB(), app, agenttest.WithRAG())
		opts := agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"search the knowledge base"},
			Provider:   provider,
			StoreDir:   GinkgoT().TempDir(),
		}

		res, err := agent.Run(context.Background(), opts, events, agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))
		Expect(events.HasWarning(agent.WarnKnowledgeIndexAbsent)).To(BeTrue())
	})

	// Setup 10 (part one): the model never returns a final answer, so the single
	// permitted iteration is spent and the run ends on the max-iterations outcome.
	It("Should end on the max-iterations outcome", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		input := json.RawMessage(`{"subject":"x"}`)
		provider := agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("call-1", "do", input),
		)
		events := agenttest.NewRecordingEvents()

		cfg := agenttest.Config(GinkgoTB(), app, agenttest.WithMaxIterations(1))
		opts := agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"keep working on the task"},
			Provider:   provider,
		}

		res, err := agent.Run(context.Background(), opts, events, agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).To(MatchError(ContainSubstring("max iterations")))
		Expect(res.Reason).To(Equal(runstate.ReasonMaxIterations))
	})

	// Setup 10 (part two): a single turn reports enough token usage to cross the run
	// budget, so the run ends on the budget outcome before the tool even executes.
	It("Should end on the token budget outcome", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		input := json.RawMessage(`{"subject":"x"}`)
		resp := agenttest.ToolUseResponse("call-1", "do", input)
		resp.Usage = llm.Usage{In: 100, Out: 100}
		provider := agenttest.NewScriptedProvider(GinkgoTB(), resp)
		events := agenttest.NewRecordingEvents()

		cfg := agenttest.Config(GinkgoTB(), app, agenttest.WithMaxTokens(50))
		opts := agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"keep working on the task"},
			Provider:   provider,
		}

		res, err := agent.Run(context.Background(), opts, events, agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).To(MatchError(ContainSubstring("token budget")))
		Expect(res.Reason).To(Equal(runstate.ReasonBudget))
	})

	// Injects a caller-built tool through Options.CustomTools: the model calls it, the
	// handler runs in-process and returns its result with functool.Result, and the
	// result feeds back keyed to the tool_use id. It covers functool.New,
	// functool.Result, a hand-written schema, and a Trace renderer that makes the call
	// visible in the run.
	It("Should complete a custom tool round trip", func() {
		// A function tool needs only a name, description, schema and handler. The handler
		// ignores its CallContext here (it neither prompts an operator nor touches the
		// working directory) and returns its JSON result through functool.Result. The Trace
		// renderer gives the call a one-line trace, like a command's.
		tool, err := functool.New(functool.Spec{
			Name:        "lookup_ticket",
			Description: "look up a support ticket by id",
			Schema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"id": map[string]any{"type": "string", "description": "the ticket id"}},
				"required":   []any{"id"},
			},
			ValidateRequired: true,
			Handler: func(_ context.Context, input json.RawMessage, _ *functool.CallContext) (string, error) {
				var args struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(input, &args); err != nil {
					return "", err
				}
				return functool.Result(map[string]any{"ticket": args.ID, "status": "open"})
			},
			Trace: func(input json.RawMessage) string {
				var args struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(input, &args); err != nil {
					return "lookup_ticket"
				}
				return "lookup_ticket " + args.ID
			},
		})
		Expect(err).NotTo(HaveOccurred())

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		provider := agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("call-1", "lookup_ticket", json.RawMessage(`{"id":"T-42"}`)),
			agenttest.TextResponse("finished"),
		)
		events := agenttest.NewRecordingEvents()

		cfg := agenttest.Config(GinkgoTB(), app)
		opts := agent.Options{
			Config:      cfg,
			ConfigFile:  "agent.yaml",
			Prompt:      []string{"look up ticket T-42"},
			Provider:    provider,
			CustomTools: []toolkit.Tool{tool},
		}

		res, err := agent.Run(context.Background(), opts, events, agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

		// The handler's result came back, keyed to the tool_use id the model chose.
		results := events.ToolResults()
		Expect(results).To(HaveLen(1))
		Expect(results[0].IsError).To(BeFalse())
		Expect(results[0].Output).To(ContainSubstring(`"ticket":"T-42"`))
		Expect(results[0].Output).To(ContainSubstring(`"status":"open"`))

		last := provider.Requests()[1].Messages[len(provider.Requests()[1].Messages)-1]
		Expect(last.Content[0].ToolResult.ToolUseID).To(Equal("call-1"))

		// The custom tool was advertised to the model alongside the application tool.
		advertised := false
		for _, td := range provider.Requests()[0].Tools {
			if td.Name == "lookup_ticket" {
				advertised = true
			}
		}
		Expect(advertised).To(BeTrue())

		// Because a Trace renderer was set, the call was traced with its one-line form; a
		// tool with no Trace would render no call line.
		calls := events.ToolCalls()
		Expect(calls).To(HaveLen(1))
		Expect(calls[0].Name).To(Equal("lookup_ticket"))
		Expect(calls[0].Display).To(ContainSubstring("lookup_ticket T-42"))
	})

	// The worked composition of the two tool hooks: PreToolUse as a policy gate that
	// refuses one call outright and rewrites another's arguments, and PostToolUse as an
	// output filter that keeps a credential out of the conversation. This is the shape a
	// caller reaches for to wrap a run in its own rules without forking the loop, so it
	// asserts what the model is told as much as what ran.
	It("Should apply the tool policy hooks", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), examplePolicyApp())

		// The model reaches for the destructive tool first, is refused, and adapts: it does
		// real work instead, whose output echoes a credential back.
		provider := agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("call-1", "wipe", json.RawMessage(`{"target":"/"}`)),
			agenttest.ToolUseResponse("call-2", "do", json.RawMessage(`{"subject":"password=hunter2"}`)),
			agenttest.TextResponse("finished"),
		)
		events := agenttest.NewRecordingEvents()

		opts := agent.Options{
			Config:     agenttest.Config(GinkgoTB(), app),
			ConfigFile: "agent.yaml",
			Prompt:     []string{"tidy up the estate"},
			Provider:   provider,
			Hooks: agent.Hooks{
				PreToolUse: func(_ context.Context, in agent.PreToolUseInfo) (agent.PreToolUseResult, error) {
					if in.ToolName == "wipe" {
						// A deny is never a silent skip: the model is handed an error result
						// carrying this reason, so it can adapt and try another approach.
						return agent.PreToolUseResult{Deny: true, DenyReason: "wipe is not permitted by policy"}, nil
					}

					// Read-modify-write. RewriteInput replaces the whole argument object, so
					// an edit of one field starts from the model's own arguments.
					var args map[string]any
					uerr := json.Unmarshal(in.Input, &args)
					if uerr != nil {
						return agent.PreToolUseResult{}, nil // leave it to the argument validator
					}
					args["level"] = "debug"

					edited, merr := json.Marshal(args)
					if merr != nil {
						return agent.PreToolUseResult{}, nil
					}

					return agent.PreToolUseResult{RewriteInput: edited}, nil
				},
				PostToolUse: func(_ context.Context, in agent.PostToolUseInfo) (agent.PostToolUseResult, error) {
					if !strings.Contains(in.Output, "password=") {
						return agent.PostToolUseResult{}, nil
					}

					// Replace is an explicit bool rather than an empty-Output sentinel,
					// because replacing a result with nothing is a legitimate filter.
					return agent.PostToolUseResult{Replace: true, Output: "[redacted]", IsError: in.IsError}, nil
				},
			},
		}

		res, err := agent.Run(context.Background(), opts, events, agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

		// The denied tool never ran, so it was never traced; the rewrite reached the tool that
		// did, and the operator sees the arguments it actually ran with.
		calls := events.ToolCalls()
		Expect(calls).To(HaveLen(1))
		Expect(calls[0].Name).To(Equal("do"))
		Expect(calls[0].Display).To(ContainSubstring("debug"))

		// The filter runs before the result is traced, so the credential never reaches the
		// operator's screen either.
		results := events.ToolResults()
		Expect(results).To(HaveLen(1))
		Expect(results[0].Output).To(Equal("[redacted]"))

		// Both calls are still exactly one tool call each: a deny is accounted like any other.
		Expect(res.Stats.ToolCalls).To(Equal(int64(2)))

		// The model was told the refusal, as an error it can work around rather than a final
		// answer, and it was told the filtered output rather than the credential.
		reqs := provider.Requests()
		Expect(reqs).To(HaveLen(3))

		denied := requestToolResults(reqs[1])["call-1"]
		Expect(denied.IsError).To(BeTrue())
		Expect(denied.Content).To(ContainSubstring("wipe is not permitted by policy"))

		filtered := requestToolResults(reqs[2])["call-2"]
		Expect(filtered.IsError).To(BeFalse())
		Expect(filtered.Content).To(Equal("[redacted]"))
	})

	// Shows the handler error path: a handler that returns an error surfaces to the
	// model as an error result, and the run continues rather than aborting, so the model
	// can react to the failure like any other tool's.
	It("Should surface a custom tool's handler error", func() {
		tool, err := functool.New(functool.Spec{
			Name:        "flaky",
			Description: "a tool that fails",
			Schema:      map[string]any{"type": "object"},
			Handler: func(_ context.Context, _ json.RawMessage, _ *functool.CallContext) (string, error) {
				return "", errors.New("backend unavailable")
			},
		})
		Expect(err).NotTo(HaveOccurred())

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		provider := agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("call-1", "flaky", json.RawMessage(`{}`)),
			agenttest.TextResponse("recovered"),
		)
		events := agenttest.NewRecordingEvents()

		cfg := agenttest.Config(GinkgoTB(), app)
		opts := agent.Options{
			Config:      cfg,
			ConfigFile:  "agent.yaml",
			Prompt:      []string{"try the flaky tool"},
			Provider:    provider,
			CustomTools: []toolkit.Tool{tool},
		}

		res, err := agent.Run(context.Background(), opts, events, agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

		results := events.ToolResults()
		Expect(results).To(HaveLen(1))
		Expect(results[0].IsError).To(BeTrue())
		Expect(results[0].Output).To(ContainSubstring("backend unavailable"))
	})

	// Proves an injected tool's confirmation gate is real at runtime, not merely
	// counted: a custom tool built with a functool.ConfirmSpec is put to the operator
	// before it runs, and only the approved call executes. The prompter reports it can
	// prompt (a stand-in for a non-terminal operator channel), so the tool runs even
	// without a TTY and the no-operator advisory does not fire.
	It("Should gate a custom tool behind a confirmation", func() {
		tool, err := functool.New(functool.Spec{
			Name:        "delete_ticket",
			Description: "delete a support ticket",
			Schema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"id": map[string]any{"type": "string"}},
				"required":   []any{"id"},
			},
			Handler: func(_ context.Context, _ json.RawMessage, _ *functool.CallContext) (string, error) {
				return functool.Result(map[string]any{"deleted": true})
			},
			Confirm: &functool.ConfirmSpec{
				Summary: func(json.RawMessage) string { return "delete_ticket" },
			},
		})
		Expect(err).NotTo(HaveOccurred())

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		provider := agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("call-1", "delete_ticket", json.RawMessage(`{"id":"T-9"}`)),
			agenttest.TextResponse("done"),
		)
		events := agenttest.NewRecordingEvents()

		prompter := agenttest.NewScriptedPrompter(GinkgoTB())
		approved := false
		prompter.ApproveFn = func(toolkit.GateRequest) (toolkit.ConfirmChoice, error) {
			approved = true
			return toolkit.ConfirmOnce, nil
		}

		cfg := agenttest.Config(GinkgoTB(), app)
		opts := agent.Options{
			Config:      cfg,
			ConfigFile:  "agent.yaml",
			Prompt:      []string{"delete ticket T-9"},
			Provider:    provider,
			CustomTools: []toolkit.Tool{tool},
		}

		res, err := agent.Run(context.Background(), opts, events, prompter)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

		// The gate was consulted for the injected tool and the approved call ran; no
		// no-operator advisory fired because the prompter could prompt.
		Expect(approved).To(BeTrue())
		Expect(events.ToolResults()).To(HaveLen(1))
		Expect(events.HasWarning(agent.WarnConfirmNoTerminal)).To(BeFalse())
	})

	// Injects one caller-built memory store (memory/file.NewFileStore) into two
	// sequential runs and proves Run borrows it: a memory written through the model in
	// run 1 is still readable through the same handle after run 2, since Run used the
	// injected store verbatim and never closed it. Mirrors the shared RAG store spec.
	It("Should borrow a shared memory store", func() {
		ctx := context.Background()

		// One store the caller builds and owns, shared across the runs.
		store, err := file.NewFileStore(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())

		// Run 1: the model writes a memory through the built-in memory_write tool.
		write := json.RawMessage(`{"key":"build.notes","description":"how the build works","content":"run abt t u"}`)
		provider1 := agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("c1", "memory_write", write),
			agenttest.TextResponse("saved"),
		)
		res1, err := agent.Run(ctx, agent.Options{
			Config:      agenttest.Config(GinkgoTB(), app, agenttest.WithMemory()),
			ConfigFile:  "agent.yaml",
			Prompt:      []string{"remember the build"},
			Provider:    provider1,
			MemoryStore: store,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res1.Reason).To(Equal(runstate.ReasonCompleted))

		// Run 2: a second run against the same injected store completes, existing only to
		// prove the store survives another run (Run did not close it between the two).
		provider2 := agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done"))
		res2, err := agent.Run(ctx, agent.Options{
			Config:      agenttest.Config(GinkgoTB(), app, agenttest.WithMemory()),
			ConfigFile:  "agent.yaml",
			Prompt:      []string{"go"},
			Provider:    provider2,
			MemoryStore: store,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res2.Reason).To(Equal(runstate.ReasonCompleted))

		// The memory written by run 1 is still readable through the caller's handle after
		// run 2, proving Run borrowed the store rather than rebuilding or closing it.
		description, content, err := store.Read(ctx, "build.notes")
		Expect(err).NotTo(HaveOccurred())
		Expect(description).To(Equal("how the build works"))
		Expect(content).To(ContainSubstring("run abt t u"))
	})

	// Injects one in-memory session store into a checkpoint+resume pair and proves Run
	// borrows it: run 1 suspends into the injected store and run 2 resumes from it.
	// Because the store lives only in that instance, the resume succeeds only because
	// Run used the injected store for both runs rather than building its own from cfg.
	// Adapts the checkpoint suspend/resume spec, and unlike it needs no StoreDir since
	// the store is supplied directly.
	It("Should borrow a shared session store", func() {
		ctx := context.Background()

		store := agenttest.NewFakeSessionStore(GinkgoTB())
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())

		// Run 1: one tool call, then a suspend at the next boundary, journaled to the store.
		suspendPolls := 0
		res1, err := agent.Run(ctx, agent.Options{
			Config:           agenttest.Config(GinkgoTB(), app),
			ConfigFile:       "agent.yaml",
			Prompt:           []string{"start work"},
			Provider:         agenttest.NewScriptedProvider(GinkgoTB(), agenttest.ToolUseResponse("c1", "do", json.RawMessage(`{"subject":"x"}`))),
			Checkpoint:       agent.Checkpoint{Enabled: true},
			SessionStore:     store,
			SuspendRequested: func() bool { suspendPolls++; return suspendPolls > 1 },
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res1.Reason).To(Equal(runstate.ReasonSuspended))
		Expect(res1.SessionID).NotTo(BeEmpty())

		// Run 2: resume the saved session from the same injected store to a final answer.
		res2, err := agent.Run(ctx, agent.Options{
			Config:       agenttest.Config(GinkgoTB(), app),
			ConfigFile:   "agent.yaml",
			Provider:     agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("finished")),
			Checkpoint:   agent.Checkpoint{ResumeID: res1.SessionID},
			SessionStore: store,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res2.Reason).To(Equal(runstate.ReasonCompleted))
		Expect(res2.SessionID).To(Equal(res1.SessionID))
	})

	// Configures a remote_tools host and injects a fake a2a.Transport, with no broker
	// reachable, proving Run borrows the transport: the remote tool is discovered and
	// invoked through the fake, Run never dials, and it never closes the borrowed
	// transport. Injecting a transport reports no conflict error, so this is the one
	// place the skip-dial gating is directly observable end to end.
	It("Should borrow an injected A2A transport", func() {
		ctx := context.Background()

		// The fake answers discovery with a card exposing one tool and answers that tool's
		// invocation with a fixed result.
		transport := agenttest.NewFakeTransport(GinkgoTB(), wire.AgentCard{
			Name:    "weather-svc",
			Version: "1.0.0",
			Tools: []wire.ToolDescriptor{{
				Name:        "forecast",
				Description: "get the forecast",
				InputSchema: json.RawMessage(`{"type":"object"}`),
			}},
		})
		transport.SetToolReply(`{"forecast":"sunny"}`, false)

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		cfg.RemoteTools = []config.RemoteToolHost{{Name: "weather-svc"}}

		provider := agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("call-1", "forecast", json.RawMessage(`{}`)),
			agenttest.TextResponse("done"),
		)
		events := agenttest.NewRecordingEvents()

		res, err := agent.Run(ctx, agent.Options{
			Config:       cfg,
			ConfigFile:   "agent.yaml",
			Prompt:       []string{"what is the forecast"},
			Provider:     provider,
			A2ATransport: transport,
		}, events, agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

		// The imported tool round-tripped through the fake: discovery at import, then the
		// one tool call. The borrowed transport was never closed by Run.
		Expect(transport.RoundTrips()).To(BeNumerically(">=", 2))
		Expect(transport.Closed()).To(BeFalse())

		// The result the fake returned came back to the model as a tool result.
		results := events.ToolResults()
		Expect(results).To(HaveLen(1))
		Expect(results[0].IsError).To(BeFalse())
		Expect(results[0].Output).To(ContainSubstring("sunny"))
	})

	// Shows a custom tool consulting the operator: its handler calls
	// tc.Prompter().Confirm, which routes to the run's prompter, and the operator's
	// answer flows back to the model. Every other custom-tool example here discards its
	// CallContext, so this is where CallContext.Prompter() is shown at all. A run with
	// no operator fails this same call closed, which the functool package's fail-closed
	// spec asserts.
	It("Should let a custom tool elicit an answer from the operator", func() {
		tool, err := functool.New(functool.Spec{
			Name:        "escalate_ticket",
			Description: "escalate a support ticket to a human",
			Schema:      map[string]any{"type": "object"},
			Handler: func(ctx context.Context, input json.RawMessage, tc *functool.CallContext) (string, error) {
				// A custom tool may consult the operator through the run's prompter; a run with
				// no operator fails closed.
				ok, err := tc.Prompter().Confirm(ctx, "escalate this ticket to a human?")
				if err != nil {
					return "", err
				}
				return functool.Result(map[string]any{"escalated": ok})
			},
		})
		Expect(err).NotTo(HaveOccurred())

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		provider := agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("call-1", "escalate_ticket", json.RawMessage(`{}`)),
			agenttest.TextResponse("done"),
		)
		events := agenttest.NewRecordingEvents()

		// The scripted prompter records the question and approves it, standing in for a
		// reachable operator.
		prompter := agenttest.NewScriptedPrompter(GinkgoTB())
		var asked string
		prompter.ConfirmFn = func(q string) (bool, error) {
			asked = q
			return true, nil
		}

		res, err := agent.Run(context.Background(), agent.Options{
			Config:      agenttest.Config(GinkgoTB(), app),
			ConfigFile:  "agent.yaml",
			Prompt:      []string{"escalate my ticket"},
			Provider:    provider,
			CustomTools: []toolkit.Tool{tool},
		}, events, prompter)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

		// The handler reached the operator through the run's prompter with its own question,
		// and the approved answer came back as the tool result.
		Expect(asked).To(Equal("escalate this ticket to a human?"))
		results := events.ToolResults()
		Expect(results).To(HaveLen(1))
		Expect(results[0].Output).To(ContainSubstring(`"escalated":true`))
	})
})
