//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package serve_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/serve"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/toolkit/functool"
)

// echoProvider asks for one call to tool and then answers with the result that call
// produced, so the outcome's text is the tool's own output rather than a scripted
// string a spec could have written itself.
type echoProvider struct {
	tool string

	mu    sync.Mutex
	calls int
}

func (p *echoProvider) Call(_ context.Context, req llm.Request) (*llm.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.calls++
	if p.calls == 1 {
		return agenttest.ToolUseResponse("c1", p.tool, json.RawMessage(`{}`)), nil
	}

	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.ToolResult == nil {
				continue
			}
			if b.ToolResult.ToolUseID == "c1" {
				return agenttest.TextResponse(b.ToolResult.Content), nil
			}
		}
	}

	return nil, fmt.Errorf("the conversation carries no result answering c1")
}

func (p *echoProvider) Capabilities() llm.Caps {
	return llm.Caps{Provider: "anthropic", SupportsToolSearch: true}
}

// toolResultFor returns the block answering id, from the last request the provider
// received.
func toolResultFor(requests []llm.Request, id string) *llm.ToolResultBlock {
	for i := len(requests) - 1; i >= 0; i-- {
		for _, m := range requests[i].Messages {
			for _, b := range m.Content {
				if b.ToolResult == nil {
					continue
				}
				if b.ToolResult.ToolUseID == id {
					return b.ToolResult
				}
			}
		}
	}

	return nil
}

// The per-process values a calling program holds and hands to every run: a Go tool it
// wrote, and the callbacks it gates and observes its runs with. Both are the caller's
// own code, so a run hosted behind a channel has to reach them exactly as a run the
// caller drives itself does.
var _ = Describe("Options", func() {
	var (
		ctx    context.Context
		cancel context.CancelFunc
	)

	BeforeEach(func() {
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		DeferCleanup(cancel)
	})

	// runWork serves one piece of work and returns the outcome the channel was given.
	runWork := func(work *serve.Work, opts serve.Options) serve.Outcome {
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

	Describe("CustomTools", func() {
		It("Should let a hosted run call a tool the caller injected", func() {
			var handled int

			tool, err := functool.New(functool.Spec{
				Name:        "shift_report",
				Description: "reports the state of the shift",
				Schema:      map[string]any{"type": "object"},
				Handler: func(context.Context, json.RawMessage, *functool.CallContext) (string, error) {
					handled++

					return "two alarms cleared, one pump offline", nil
				},
			})
			Expect(err).ToNot(HaveOccurred())

			provider := &echoProvider{tool: "shift_report"}

			out := runWork(&serve.Work{ID: "job-1", Prompt: "how did the shift go"}, serve.Options{
				Provider:    provider,
				CustomTools: []toolkit.Tool{tool},
			})

			Expect(out.Err).ToNot(HaveOccurred())
			Expect(out.Reason).To(Equal(runstate.ReasonCompleted))
			Expect(handled).To(Equal(1), "the model reached the caller's handler")
			Expect(out.Stats.ToolCalls).To(BeEquivalentTo(1))
			Expect(out.Text).To(Equal("two alarms cleared, one pump offline"),
				"the model answered from the tool result, so the outcome carries what the tool returned")
		})
	})

	Describe("Hooks", func() {
		It("Should let a PreToolUse hook deny a call in a hosted run", func() {
			var seen []string

			provider := agenttest.NewScriptedProvider(GinkgoTB(),
				agenttest.ToolUseResponse("c1", "do", json.RawMessage(`{"subject":"the-argument"}`)),
				agenttest.TextResponse("took another route"))

			out := runWork(&serve.Work{ID: "job-1", Prompt: "do the thing"}, serve.Options{
				Provider: provider,
				Hooks: agent.Hooks{
					PreToolUse: func(_ context.Context, in agent.PreToolUseInfo) (agent.PreToolUseResult, error) {
						seen = append(seen, in.ToolName)

						return agent.PreToolUseResult{Deny: true, DenyReason: "blocked by policy"}, nil
					},
				},
			})

			Expect(out.Err).ToNot(HaveOccurred())
			Expect(seen).To(Equal([]string{"do"}))

			// The application echoes its arguments, so a call that ran would put
			// the-argument in front of the model. The deny reason is there instead.
			result := toolResultFor(provider.Requests(), "c1")
			Expect(result).ToNot(BeNil())
			Expect(result.IsError).To(BeTrue())
			Expect(result.Content).To(Equal("blocked by policy"))
			Expect(result.Content).ToNot(ContainSubstring("the-argument"))
		})
	})
})
