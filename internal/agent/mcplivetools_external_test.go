//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// These tests exercise a configured MCP server changing its tool list while a run is
// under way: what the next model call is offered, what the tool batch already in
// flight still dispatches, which servers a notification touches, what happens to a
// name that would collide, and what the operator is told. The servers are real
// mcp.Servers driven over in-memory transports, and each spec changes a server's tools
// for real rather than posing as one.
package agent_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/choria-io/fisk"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/mcpclient"
	"github.com/choria-io/fisk-ai/internal/runstate"
)

// mcpStep is one model call: what to do when it arrives and what to answer it with.
type mcpStep struct {
	// before runs as the call arrives, so whatever it changes reaches the call after
	// this one: the loop takes the tools for a call before it makes it.
	before   func()
	response *llm.Response
}

// mcpStepProvider answers each model call from a script and lets a spec act between
// two calls, which agenttest.ScriptedProvider has no place for. A tool list has to
// change at a point in the run rather than before it, and the change has to have
// landed before the next call takes its tools.
type mcpStepProvider struct {
	t testing.TB

	mu       sync.Mutex
	steps    []mcpStep
	idx      int
	requests []llm.Request
}

func newMCPStepProvider(t testing.TB, steps ...mcpStep) *mcpStepProvider {
	return &mcpStepProvider{t: t, steps: steps}
}

func (p *mcpStepProvider) Capabilities() llm.Caps {
	return llm.Caps{Provider: "anthropic", SupportsToolSearch: true}
}

func (p *mcpStepProvider) Call(_ context.Context, req llm.Request) (*llm.Response, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	if p.idx >= len(p.steps) {
		p.mu.Unlock()
		p.t.Fatalf("the model was called %d times, past the %d scripted answers", p.idx+1, len(p.steps))
	}
	step := p.steps[p.idx]
	p.idx++
	p.mu.Unlock()

	if step.before != nil {
		step.before()
	}

	return step.response, nil
}

// Requests are the requests the run made, in order.
func (p *mcpStepProvider) Requests() []llm.Request {
	p.mu.Lock()
	defer p.mu.Unlock()

	return append([]llm.Request(nil), p.requests...)
}

// toolNames are the tools a request offered the model.
func toolNames(req llm.Request) []string {
	out := make([]string, 0, len(req.Tools))
	for _, d := range req.Tools {
		out = append(out, d.Name)
	}

	return out
}

// mcpAfterRebuild changes a server's tool list and returns once the run has rebuilt
// its tools from it.
//
// The registration made here is made after the run's, and the sessions call their
// watchers one after another in the order they registered, so the run's rebuild is
// done by the time this one is called.
func mcpAfterRebuild(t testing.TB, sessions *mcpclient.Sessions, change func()) {
	t.Helper()

	rebuilt := make(chan struct{}, 1)
	stop := sessions.OnToolListChanged(func(mcpclient.ToolListChange) {
		select {
		case rebuilt <- struct{}{}:
		default:
		}
	})
	defer stop()

	change()

	Eventually(rebuilt, 30*time.Second).Should(Receive(), "the server's tool list change never reached the run")
}

// mcpAddTool gives a server a tool it did not have, which is what makes it tell its
// client that its tool list changed.
func mcpAddTool(t testing.TB, fake *mcpFakeServers, server string, name string) {
	t.Helper()

	fake.server(t, server).AddTool(mcpDescriptor(name, "a tool the server added later"), mcpEchoHandler)
}

// toolDescription is what a request told the model one tool does, and the empty string
// for a tool it did not carry.
func toolDescription(req llm.Request, name string) string {
	for _, d := range req.Tools {
		if d.Name == name {
			return d.Description
		}
	}

	return ""
}

// mcpChangedWarnings are the tool-list advisories a run raised.
func mcpChangedWarnings(events *agenttest.RecordingEvents) []agent.Warning {
	var out []agent.Warning
	for _, w := range events.Warnings() {
		if w.Kind == agent.WarnMCPToolsChanged {
			out = append(out, w)
		}
	}

	return out
}

// mcpToolUse is one assistant turn asking for several tools at once, which is the
// batch a removal must not strand.
func mcpToolUse(calls ...llm.ToolUseBlock) *llm.Response {
	resp := &llm.Response{StopReason: llm.StopToolUse}
	for i := range calls {
		call := calls[i]
		resp.Content = append(resp.Content, llm.ContentBlock{ToolUse: &call})
	}

	return resp
}

var _ = Describe("an MCP server that changes its tools mid-run", func() {
	// This proves the whole path: a server adds a tool mid-run, the next model call is
	// offered it under the server's own alias, the model calls it and it dispatches to
	// the server it came from.
	It("Should offer an added tool on the next model call", func() {
		fake := &mcpFakeServers{tools: []*mcp.Tool{mcpDescriptor("search", "Searches the documentation")}}
		sessions := connectMCP(GinkgoTB(), fake, config.MCPServer{Name: "docs"})

		cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), exampleApp()))
		cfg.MCPClients = []config.MCPServer{{Name: "docs"}}

		provider := newMCPStepProvider(GinkgoTB(),
			mcpStep{
				before: func() {
					mcpAfterRebuild(GinkgoTB(), sessions, func() { mcpAddTool(GinkgoTB(), fake, "docs", "fetch") })
				},
				response: agenttest.ToolUseResponse("call-1", "docs_search", json.RawMessage(`{}`)),
			},
			mcpStep{response: agenttest.ToolUseResponse("call-2", "docs_fetch", json.RawMessage(`{}`))},
			mcpStep{response: agenttest.TextResponse("done")},
		)
		events := agenttest.NewRecordingEvents()

		res, err := agent.Run(context.Background(), agent.Options{
			Config:      cfg,
			ConfigFile:  "agent.yaml",
			Prompt:      []string{"search the docs"},
			Provider:    provider,
			MCPSessions: sessions,
		}, events, agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

		requests := provider.Requests()
		Expect(requests).To(HaveLen(3))
		Expect(toolNames(requests[0])).To(ContainElement("docs_search"))
		Expect(toolNames(requests[0])).NotTo(ContainElement("docs_fetch"))
		Expect(toolNames(requests[1])).To(ContainElement("docs_fetch"))

		// The added tool was dispatched to the server that added it.
		results := events.ToolResults()
		Expect(results).To(HaveLen(2))
		Expect(results[1].IsError).To(BeFalse())
		Expect(results[1].Output).To(Equal("handled by fetch"))

		// And the run said so, naming the server and what moved.
		var changed []agent.Warning
		for _, w := range events.Warnings() {
			if w.Kind == agent.WarnMCPToolsChanged {
				changed = append(changed, w)
			}
		}
		Expect(changed).To(HaveLen(1))
		Expect(changed[0].Name).To(Equal("docs"))
		Expect(changed[0].Params).To(ConsistOf("added docs_fetch"))
	})

	// This pins the other direction. A server dropping a tool the model was already told
	// about is ordinary: the batch answering the last call is dispatched against the set
	// that call carried, and the definition is gone from the call after it.
	It("Should leave the batch in flight intact when a tool is removed", func() {
		fake := &mcpFakeServers{tools: []*mcp.Tool{
			mcpDescriptor("search", "Searches the documentation"),
			mcpDescriptor("fetch", "Fetches a document"),
		}}
		sessions := connectMCP(GinkgoTB(), fake, config.MCPServer{Name: "docs"})

		cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), exampleApp()))
		cfg.MCPClients = []config.MCPServer{{Name: "docs"}}

		provider := newMCPStepProvider(GinkgoTB(),
			mcpStep{
				before: func() {
					mcpAfterRebuild(GinkgoTB(), sessions, func() { fake.server(GinkgoTB(), "docs").RemoveTools("fetch") })
				},
				response: mcpToolUse(
					llm.ToolUseBlock{ID: "call-1", Name: "docs_search", Input: json.RawMessage(`{}`)},
					llm.ToolUseBlock{ID: "call-2", Name: "docs_fetch", Input: json.RawMessage(`{}`)},
				),
			},
			mcpStep{response: agenttest.TextResponse("done")},
		)
		events := agenttest.NewRecordingEvents()

		res, err := agent.Run(context.Background(), agent.Options{
			Config:      cfg,
			ConfigFile:  "agent.yaml",
			Prompt:      []string{"search the docs"},
			Provider:    provider,
			MCPSessions: sessions,
		}, events, agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

		// Both calls of the batch were dispatched, the removed one included: the set it ran
		// against is the one its call was made with.
		var called []string
		for _, c := range events.ToolCalls() {
			called = append(called, c.Name)
		}
		Expect(called).To(Equal([]string{"docs_search", "docs_fetch"}))
		Expect(events.HasWarning(agent.WarnUnknownTool)).To(BeFalse())

		// The next call is not offered it.
		requests := provider.Requests()
		Expect(requests).To(HaveLen(2))
		Expect(toolNames(requests[0])).To(ContainElement("docs_fetch"))
		Expect(toolNames(requests[1])).NotTo(ContainElement("docs_fetch"))

		var changed []agent.Warning
		for _, w := range events.Warnings() {
			if w.Kind == agent.WarnMCPToolsChanged {
				changed = append(changed, w)
			}
		}
		Expect(changed).To(HaveLen(1))
		Expect(changed[0].Params).To(ConsistOf("removed docs_fetch"))
	})

	// This pins the scope of a rebuild: the server that spoke is re-listed and every
	// other one is left alone, tools and round trips both.
	It("Should rebuild only the server that notified", func() {
		fake := &mcpFakeServers{tools: []*mcp.Tool{mcpDescriptor("search", "Searches the documentation")}}
		sessions := connectMCP(GinkgoTB(), fake, config.MCPServer{Name: "docs"}, config.MCPServer{Name: "wiki"})

		cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), exampleApp()))
		cfg.MCPClients = []config.MCPServer{{Name: "docs"}, {Name: "wiki"}}

		provider := newMCPStepProvider(GinkgoTB(),
			mcpStep{
				before: func() {
					mcpAfterRebuild(GinkgoTB(), sessions, func() { mcpAddTool(GinkgoTB(), fake, "docs", "fetch") })
				},
				response: agenttest.ToolUseResponse("call-1", "docs_search", json.RawMessage(`{}`)),
			},
			mcpStep{response: agenttest.TextResponse("done")},
		)

		res, err := agent.Run(context.Background(), agent.Options{
			Config:      cfg,
			ConfigFile:  "agent.yaml",
			Prompt:      []string{"search the docs"},
			Provider:    provider,
			MCPSessions: sessions,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

		requests := provider.Requests()
		Expect(toolNames(requests[1])).To(ContainElements("docs_search", "docs_fetch", "wiki_search"))
		Expect(toolNames(requests[1])).NotTo(ContainElement("wiki_fetch"))

		// The run listed the wiki server when it started and never again.
		Expect(fake.lists("docs")).To(Equal(2))
		Expect(fake.lists("wiki")).To(Equal(1))
	})

	// This pins where this differs from run start. A name that would collide fails the
	// run when the run has not started yet; arriving mid-conversation it is left out and
	// recorded, since ending a conversation over a third party's edit to its own tool
	// list costs more than the tool does.
	It("Should skip a colliding tool rather than end the run", func() {
		// The application's "docs status" command loads as the tool "docs_status", which is
		// the name the server's "status" would take under the alias "docs".
		application := fisk.New("app", "an app")
		application.Command("docs", "documentation commands").Command("status", "report the documentation status")

		fake := &mcpFakeServers{tools: []*mcp.Tool{mcpDescriptor("search", "Searches the documentation")}}
		sessions := connectMCP(GinkgoTB(), fake, config.MCPServer{Name: "docs"})

		cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), application))
		cfg.MCPClients = []config.MCPServer{{Name: "docs"}}

		provider := newMCPStepProvider(GinkgoTB(),
			mcpStep{
				before: func() {
					mcpAfterRebuild(GinkgoTB(), sessions, func() { mcpAddTool(GinkgoTB(), fake, "docs", "status") })
				},
				response: agenttest.ToolUseResponse("call-1", "docs_search", json.RawMessage(`{}`)),
			},
			mcpStep{response: agenttest.TextResponse("done")},
		)
		events := agenttest.NewRecordingEvents()

		res, err := agent.Run(context.Background(), agent.Options{
			Config:      cfg,
			ConfigFile:  "agent.yaml",
			Prompt:      []string{"search the docs"},
			Provider:    provider,
			MCPSessions: sessions,
		}, events, agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

		// The run carries on with the tools it had: the application's docs_status is still
		// the tool of that name, and nothing was added under it.
		requests := provider.Requests()
		Expect(toolNames(requests[1])).To(Equal(toolNames(requests[0])))

		var changed []agent.Warning
		for _, w := range events.Warnings() {
			if w.Kind == agent.WarnMCPToolsChanged {
				changed = append(changed, w)
			}
		}
		Expect(changed).To(HaveLen(1))
		Expect(changed[0].Name).To(Equal("docs"))
		Expect(changed[0].Params).To(HaveLen(1))
		Expect(changed[0].Params[0]).To(ContainSubstring(`skipped status: the name "docs_status" is already taken`))
	})

	// This pins the ordinary run: a server that never says anything is listed once,
	// offers the model the same tools on every call, and raises nothing.
	It("Should run as before for a server that says nothing", func() {
		fake := &mcpFakeServers{tools: []*mcp.Tool{mcpDescriptor("search", "Searches the documentation")}}
		sessions := connectMCP(GinkgoTB(), fake, config.MCPServer{Name: "docs"})

		cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), exampleApp()))
		cfg.MCPClients = []config.MCPServer{{Name: "docs"}}

		provider := newMCPStepProvider(GinkgoTB(),
			mcpStep{response: agenttest.ToolUseResponse("call-1", "docs_search", json.RawMessage(`{}`))},
			mcpStep{response: agenttest.TextResponse("done")},
		)
		events := agenttest.NewRecordingEvents()

		res, err := agent.Run(context.Background(), agent.Options{
			Config:      cfg,
			ConfigFile:  "agent.yaml",
			Prompt:      []string{"search the docs"},
			Provider:    provider,
			MCPSessions: sessions,
		}, events, agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

		requests := provider.Requests()
		Expect(requests).To(HaveLen(2))
		Expect(toolNames(requests[1])).To(Equal(toolNames(requests[0])))
		Expect(events.HasWarning(agent.WarnMCPToolsChanged)).To(BeFalse())
		Expect(fake.lists("docs")).To(Equal(1))
	})

	// This pins the change that adds and removes nothing. A server that rewrites what one
	// of its tools says it does has changed what the model is told, so the next call
	// carries the new text and the operator is told which tool was redefined.
	It("Should carry a rewritten description on the next model call", func() {
		fake := &mcpFakeServers{tools: []*mcp.Tool{mcpDescriptor("search", "Searches the documentation")}}
		sessions := connectMCP(GinkgoTB(), fake, config.MCPServer{Name: "docs"})

		cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), exampleApp()))
		cfg.MCPClients = []config.MCPServer{{Name: "docs"}}

		provider := newMCPStepProvider(GinkgoTB(),
			mcpStep{
				before: func() {
					// AddTool replaces the tool of that name, so the server keeps offering
					// "search" and describes it differently.
					mcpAfterRebuild(GinkgoTB(), sessions, func() {
						fake.server(GinkgoTB(), "docs").AddTool(mcpDescriptor("search", "Searches the documentation and the changelog"), mcpEchoHandler)
					})
				},
				response: agenttest.ToolUseResponse("call-1", "docs_search", json.RawMessage(`{}`)),
			},
			mcpStep{response: agenttest.TextResponse("done")},
		)
		events := agenttest.NewRecordingEvents()

		res, err := agent.Run(context.Background(), agent.Options{
			Config:      cfg,
			ConfigFile:  "agent.yaml",
			Prompt:      []string{"search the docs"},
			Provider:    provider,
			MCPSessions: sessions,
		}, events, agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

		requests := provider.Requests()
		Expect(requests).To(HaveLen(2))
		Expect(toolNames(requests[1])).To(Equal(toolNames(requests[0])))
		Expect(toolDescription(requests[0], "docs_search")).To(Equal("Searches the documentation"))
		Expect(toolDescription(requests[1], "docs_search")).To(Equal("Searches the documentation and the changelog"))

		changed := mcpChangedWarnings(events)
		Expect(changed).To(HaveLen(1))
		Expect(changed[0].Name).To(Equal("docs"))
		Expect(changed[0].Params).To(ConsistOf("redefined docs_search"))
	})

	// This pins the advisory that has no model call left to travel with. The set is
	// published while the run's last call is in flight, the run ends on the answer that
	// call returned, and the operator still hears that the server moved.
	It("Should report a change that arrives after the last model call", func() {
		fake := &mcpFakeServers{tools: []*mcp.Tool{mcpDescriptor("search", "Searches the documentation")}}
		sessions := connectMCP(GinkgoTB(), fake, config.MCPServer{Name: "docs"})

		cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), exampleApp()))
		cfg.MCPClients = []config.MCPServer{{Name: "docs"}}

		provider := newMCPStepProvider(GinkgoTB(),
			mcpStep{response: agenttest.ToolUseResponse("call-1", "docs_search", json.RawMessage(`{}`))},
			mcpStep{
				before: func() {
					mcpAfterRebuild(GinkgoTB(), sessions, func() { mcpAddTool(GinkgoTB(), fake, "docs", "fetch") })
				},
				response: agenttest.TextResponse("done"),
			},
		)
		events := agenttest.NewRecordingEvents()

		res, err := agent.Run(context.Background(), agent.Options{
			Config:      cfg,
			ConfigFile:  "agent.yaml",
			Prompt:      []string{"search the docs"},
			Provider:    provider,
			MCPSessions: sessions,
		}, events, agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

		// The change landed after the last call had taken its tools, so no request carried
		// the added tool and none was made after it.
		requests := provider.Requests()
		Expect(requests).To(HaveLen(2))
		Expect(toolNames(requests[1])).NotTo(ContainElement("docs_fetch"))

		changed := mcpChangedWarnings(events)
		Expect(changed).To(HaveLen(1))
		Expect(changed[0].Name).To(Equal("docs"))
		Expect(changed[0].Params).To(ConsistOf("added docs_fetch"))
	})
})
