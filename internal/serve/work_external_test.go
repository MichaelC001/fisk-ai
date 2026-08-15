//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package serve_test

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/serve"
)

// The optional fields on Work are what a channel supplies to say what it can do, so
// each one is part of the contract an embedder writes against. These drive them
// through the exported API only.
var _ = Describe("Work", func() {
	var (
		ctx    context.Context
		cancel context.CancelFunc
	)

	BeforeEach(func() {
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		DeferCleanup(cancel)
	})

	// serveOne runs a server over one piece of work and returns the outcome it reported.
	serveOne := func(work *serve.Work, opts serve.Options) serve.Outcome {
		GinkgoHelper()

		ch := agenttest.NewScriptedChannel(GinkgoTB(), "jobs", work)
		opts.Channels = []serve.Channel{ch}
		if opts.Config == nil {
			opts.Config = servedConfig()
		}
		if opts.Logger == nil {
			opts.Logger = quietLogger()
		}

		srv, err := serve.New(opts)
		Expect(err).ToNot(HaveOccurred())
		Expect(srv.Serve(ctx)).To(Succeed())

		outcomes := ch.Outcomes()
		Expect(outcomes).To(HaveLen(1))

		return outcomes[0]
	}

	Describe("Context", func() {
		It("Should offer supporting material to the model alongside the prompt", func() {
			provider := agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("ok"))

			out := serveOne(&serve.Work{
				ID:      "job-1",
				Prompt:  "summarize it",
				Context: "the-supporting-material",
			}, serve.Options{Provider: provider})
			Expect(out.Err).ToNot(HaveOccurred())

			requests := provider.Requests()
			Expect(requests).ToNot(BeEmpty())

			var sent string
			for _, m := range requests[0].Messages {
				for _, b := range m.Content {
					if b.Text != nil {
						sent += b.Text.Text
					}
				}
			}

			Expect(sent).To(ContainSubstring("the-supporting-material"))
			Expect(sent).To(ContainSubstring("summarize it"))
		})
	})

	Describe("Budget", func() {
		// The in-package specs prove the clamp arithmetic. This proves a channel can
		// reach it, which is the part an embedder depends on.
		It("Should lower the limit for one piece of work", func() {
			out := serveOne(&serve.Work{
				ID:     "job-1",
				Prompt: "go",
				Budget: serve.Budget{MaxIterations: 1},
			}, serve.Options{
				Config: servedConfig(agenttest.WithMaxIterations(50)),
				Provider: agenttest.NewScriptedProvider(GinkgoTB(), &llm.Response{
					StopReason: llm.StopToolUse,
					Content: []llm.ContentBlock{
						{ToolUse: &llm.ToolUseBlock{ID: "c1", Name: "do", Input: json.RawMessage(`{"subject":"x"}`)}},
					},
				}),
			})

			Expect(out.Reason).To(Equal(runstate.ReasonMaxIterations),
				"the work asked for one iteration and got one, not the configured fifty")
		})

		It("Should never raise a limit above the configured ceiling", func() {
			out := serveOne(&serve.Work{
				ID:     "job-1",
				Prompt: "go",
				Budget: serve.Budget{MaxIterations: 99},
			}, serve.Options{
				Config: servedConfig(agenttest.WithMaxIterations(1)),
				Provider: agenttest.NewScriptedProvider(GinkgoTB(), &llm.Response{
					StopReason: llm.StopToolUse,
					Content: []llm.ContentBlock{
						{ToolUse: &llm.ToolUseBlock{ID: "c1", Name: "do", Input: json.RawMessage(`{"subject":"x"}`)}},
					},
				}),
			})

			Expect(out.Reason).To(Equal(runstate.ReasonMaxIterations))
		})
	})

	Describe("Checkpoint", func() {
		It("Should journal the run and name the session on the outcome", func() {
			store := agenttest.NewFakeSessionStore(GinkgoTB())

			out := serveOne(&serve.Work{
				ID:         "job-1",
				Prompt:     "go",
				Checkpoint: agent.Checkpoint{ResumeID: "session-1", CreateIfMissing: true},
			}, serve.Options{
				Provider:     agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
				SessionStore: store,
			})

			Expect(out.Err).ToNot(HaveOccurred())
			Expect(out.SessionID).To(Equal("session-1"))

			state, err := store.Load("session-1")
			Expect(err).ToNot(HaveOccurred())
			Expect(state).ToNot(BeNil())
		})

		It("Should leave the session empty when the work is not checkpointed", func() {
			out := serveOne(&serve.Work{ID: "job-1", Prompt: "go"}, serve.Options{
				Provider: agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
			})

			Expect(out.Err).ToNot(HaveOccurred())
			Expect(out.SessionID).To(BeEmpty())
		})
	})

	Describe("SuspendRequested and ClaimedBy", func() {
		// suspend runs one piece of work that stops at a loop boundary, which is what a
		// worker draining on shutdown leaves behind for whoever picks the session up.
		suspend := func(store runstate.Store) serve.Outcome {
			GinkgoHelper()

			polls := 0

			return serveOne(&serve.Work{
				ID:               "job-1",
				Prompt:           "go",
				Checkpoint:       agent.Checkpoint{ResumeID: "session-1", CreateIfMissing: true},
				SuspendRequested: func() bool { polls++; return polls > 1 },
			}, serve.Options{
				Provider: agenttest.NewScriptedProvider(GinkgoTB(),
					agenttest.ToolUseResponse("c1", "do", json.RawMessage(`{"subject":"x"}`))),
				SessionStore: store,
			})
		}

		It("Should stop at a loop boundary and leave a resumable session", func() {
			store := agenttest.NewFakeSessionStore(GinkgoTB())

			out := suspend(store)
			Expect(out.Reason).To(Equal(runstate.ReasonSuspended))
			Expect(out.SessionID).To(Equal("session-1"))
		})

		// A worker serving many pieces of work under one identity would stamp every
		// claim identically, so a channel's own item id is what tells them apart in a
		// journal read later. Only a resume writes one, since a run creating its journal
		// is taking nothing over.
		It("Should name the claimant when the run resumes", func() {
			store := agenttest.NewFakeSessionStore(GinkgoTB())
			Expect(suspend(store).Reason).To(Equal(runstate.ReasonSuspended))

			out := serveOne(&serve.Work{
				ID:         "job-2",
				Prompt:     "ignored on a resume",
				ClaimedBy:  "worker-a/job-2",
				Checkpoint: agent.Checkpoint{ResumeID: "session-1"},
			}, serve.Options{
				Provider:     agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("finished")),
				SessionStore: store,
			})
			Expect(out.Err).ToNot(HaveOccurred())

			journal, err := store.Open("session-1")
			Expect(err).ToNot(HaveOccurred())
			DeferCleanup(journal.Close)

			records, err := journal.Records()
			Expect(err).ToNot(HaveOccurred())

			var claims []string
			for _, r := range records {
				if r.Claim != nil {
					claims = append(claims, r.Claim.By)
				}
			}

			Expect(strings.Join(claims, ",")).To(ContainSubstring("worker-a/job-2"))
		})
	})

	Describe("Prompter", func() {
		// Nil means no operator is reachable, which is the correct answer for a queue and
		// the reason a gated tool is refused rather than left waiting.
		It("Should refuse a confirmation-gated tool when no prompter is supplied", func() {
			out := serveOne(&serve.Work{ID: "job-1", Prompt: "go"}, serve.Options{
				Config: servedConfig(agenttest.WithHITL()),
				Provider: agenttest.NewScriptedProvider(GinkgoTB(),
					agenttest.ToolUseResponse("c1", "ask_human_confirm", json.RawMessage(`{"question":"ok?"}`)),
					agenttest.TextResponse("gave up")),
			})

			Expect(out.Err).ToNot(HaveOccurred())
			Expect(out.Text).To(Equal("gave up"))
		})

		It("Should put the question to a prompter the channel supplies", func() {
			var asked string

			prompter := agenttest.NewScriptedPrompter(GinkgoTB())
			prompter.ConfirmFn = func(question string) (bool, error) {
				asked = question
				return true, nil
			}

			out := serveOne(&serve.Work{
				ID:       "job-1",
				Prompt:   "go",
				Prompter: prompter,
			}, serve.Options{
				Config: servedConfig(agenttest.WithHITL()),
				Provider: agenttest.NewScriptedProvider(GinkgoTB(),
					agenttest.ToolUseResponse("c1", "ask_human_confirm", json.RawMessage(`{"question":"ok?"}`)),
					agenttest.TextResponse("answered")),
			})

			Expect(out.Err).ToNot(HaveOccurred())
			Expect(out.Text).To(Equal("answered"))
			Expect(asked).To(Equal("ok?"), "the question reached the channel's own operator")
		})
	})

	Describe("Continue", func() {
		It("Should take another turn when the channel offers one", func() {
			turns := 0

			out := serveOne(&serve.Work{
				ID:     "job-1",
				Prompt: "first",
				Continue: func(context.Context) agent.Continuation {
					turns++
					if turns > 1 {
						return agent.Continuation{}
					}

					return agent.Continuation{Continue: true, Text: "second"}
				},
			}, serve.Options{
				Provider: agenttest.NewScriptedProvider(GinkgoTB(),
					agenttest.TextResponse("one"), agenttest.TextResponse("two")),
			})

			Expect(out.Err).ToNot(HaveOccurred())
			Expect(out.Text).To(Equal("two"), "the answer is the last turn's, not the first's")
		})

		It("Should be one shot when the channel offers none", func() {
			out := serveOne(&serve.Work{ID: "job-1", Prompt: "only"}, serve.Options{
				Provider: agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("one")),
			})

			Expect(out.Text).To(Equal("one"))
		})
	})
})
