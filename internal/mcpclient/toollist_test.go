//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/toolkit/functool"
)

// changeWatcher collects the changes one registration was handed.
type changeWatcher struct {
	mu      sync.Mutex
	changes []ToolListChange
}

func (w *changeWatcher) record(change ToolListChange) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.changes = append(w.changes, change)
}

func (w *changeWatcher) seen() []ToolListChange {
	w.mu.Lock()
	defer w.mu.Unlock()

	return append([]ToolListChange(nil), w.changes...)
}

// count is how many changes have arrived, for Eventually and Consistently.
func (w *changeWatcher) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()

	return len(w.changes)
}

var _ = Describe("Tool list changes", func() {
	var (
		ctx     context.Context
		cancel  context.CancelFunc
		servers *fakeServers
	)

	// docsTools are the two tools the "docs" server starts with, so a spec can add a
	// third or take one away and see what a watcher is handed.
	docsTools := []fakeTool{
		{
			tool:    &mcp.Tool{Name: "search", Description: "Searches the documentation", InputSchema: json.RawMessage(`{"type":"object"}`)},
			handler: func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) { return nil, nil },
		},
		{
			tool:    &mcp.Tool{Name: "fetch", Description: "Fetches a document", InputSchema: json.RawMessage(`{"type":"object"}`)},
			handler: func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) { return nil, nil },
		},
	}

	BeforeEach(func() {
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		DeferCleanup(cancel)

		servers = newFakeServers()
		servers.tools["docs"] = docsTools
		servers.tools["issues"] = docsTools
	})

	connected := func(entries ...config.MCPServer) *Sessions {
		GinkgoHelper()

		sessions, err := Connect(ctx, Options{
			Servers:  entries,
			Identity: "fisk-test",
			Version:  "0.0.1",
			Dialer:   servers.dialer(),
		})
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(func() { Expect(sessions.Close(ctx)).To(Succeed()) })

		return sessions
	}

	// addTool gives a server a new tool, which is what makes it tell its clients that
	// its tool list changed.
	addTool := func(server string, name string) {
		GinkgoHelper()

		servers.server(server).AddTool(&mcp.Tool{
			Name:        name,
			Description: "a tool the server added",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "added tool answered"}}}, nil
		})
	}

	// keptNames are the tool names a change carried.
	keptNames := func(change ToolListChange) []string {
		out := make([]string, 0, len(change.Kept))
		for _, t := range change.Kept {
			out = append(out, t.Name)
		}

		return out
	}

	Describe("OnToolListChanged", func() {
		It("should hand a watcher the server's list as it stands after the change", func() {
			sessions := connected(config.MCPServer{Name: "docs", Command: "unused"})

			watcher := &changeWatcher{}
			DeferCleanup(sessions.OnToolListChanged(watcher.record))

			addTool("docs", "summarize")

			Eventually(watcher.count).Should(Equal(1))
			change := watcher.seen()[0]
			Expect(change.Err).ToNot(HaveOccurred())
			Expect(change.Server.Name).To(Equal("docs"))
			Expect(change.Discovered).To(Equal(3))
			Expect(keptNames(change)).To(ConsistOf("search", "fetch", "summarize"))
			Expect(change.RTT).To(BeNumerically(">", time.Duration(0)))
		})

		It("should report a tool the server took away", func() {
			sessions := connected(config.MCPServer{Name: "docs", Command: "unused"})

			watcher := &changeWatcher{}
			DeferCleanup(sessions.OnToolListChanged(watcher.record))

			servers.server("docs").RemoveTools("fetch")

			Eventually(watcher.count).Should(Equal(1))
			Expect(keptNames(watcher.seen()[0])).To(ConsistOf("search"))
		})

		It("should apply the entry's filters to the new list", func() {
			sessions := connected(config.MCPServer{
				Name:    "docs",
				Command: "unused",
				Exclude: &config.ToolFilter{Tools: []string{"^fetch$"}},
			})

			watcher := &changeWatcher{}
			DeferCleanup(sessions.OnToolListChanged(watcher.record))

			addTool("docs", "summarize")

			Eventually(watcher.count).Should(Equal(1))
			change := watcher.seen()[0]
			Expect(change.Discovered).To(Equal(3))
			Expect(keptNames(change)).To(ConsistOf("search", "summarize"))
		})

		It("should re-list only the server that changed", func() {
			sessions := connected(
				config.MCPServer{Name: "docs", Command: "unused"},
				config.MCPServer{Name: "issues", Command: "unused"},
			)

			watcher := &changeWatcher{}
			DeferCleanup(sessions.OnToolListChanged(watcher.record))

			addTool("docs", "summarize")

			Eventually(watcher.count).Should(Equal(1))
			Expect(watcher.seen()[0].Server.Name).To(Equal("docs"))
			Expect(servers.lists("docs")).To(Equal(1))
			Expect(servers.lists("issues")).To(Equal(0))
		})

		It("should not list a server nobody is watching", func() {
			connected(config.MCPServer{Name: "docs", Command: "unused"})

			addTool("docs", "summarize")

			Consistently(func() int { return servers.lists("docs") }, 300*time.Millisecond).Should(Equal(0))
		})

		It("should stop calling a watcher whose registration was dropped", func() {
			sessions := connected(config.MCPServer{Name: "docs", Command: "unused"})

			dropped := &changeWatcher{}
			stop := sessions.OnToolListChanged(dropped.record)

			kept := &changeWatcher{}
			DeferCleanup(sessions.OnToolListChanged(kept.record))

			stop()
			addTool("docs", "summarize")

			Eventually(kept.count).Should(Equal(1))
			Expect(dropped.count()).To(Equal(0))
		})

		It("should report a server that cannot be listed again", func() {
			sessions := connected(config.MCPServer{Name: "docs", Command: "unused"})

			watcher := &changeWatcher{}
			DeferCleanup(sessions.OnToolListChanged(watcher.record))

			servers.failListing("docs", fmt.Errorf("the tool list is unavailable"))
			addTool("docs", "summarize")

			Eventually(watcher.count).Should(Equal(1))
			change := watcher.seen()[0]
			Expect(change.Err).To(MatchError(ContainSubstring(`listing the tools of mcp server "docs"`)))
			Expect(change.Kept).To(BeEmpty())
		})
	})

	Describe("ImportChanged", func() {
		It("should name and build the tools the change carries", func() {
			sessions := connected(config.MCPServer{Name: "docs", Command: "unused"})

			watcher := &changeWatcher{}
			DeferCleanup(sessions.OnToolListChanged(watcher.record))

			addTool("docs", "summarize")
			Eventually(watcher.count).Should(Equal(1))

			imported := ImportChanged(watcher.seen()[0], NewClaimedNames(nil, nil), sessions)
			Expect(imported.Err).ToNot(HaveOccurred())
			Expect(imported.Skipped).To(BeEmpty())

			var names []string
			for _, t := range imported.Tools {
				names = append(names, t.Name())
			}
			Expect(names).To(ConsistOf("docs_search", "docs_fetch", "docs_summarize"))
		})

		It("should skip a claimed name and record it rather than failing the caller", func() {
			sessions := connected(config.MCPServer{Name: "docs", Command: "unused"})

			watcher := &changeWatcher{}
			DeferCleanup(sessions.OnToolListChanged(watcher.record))

			addTool("docs", "summarize")
			Eventually(watcher.count).Should(Equal(1))

			// The name a local tool already answers to, which the server's own "summarize"
			// would take.
			claimed := NewClaimedNames(map[string]bool{"docs_summarize": true}, nil)

			imported := ImportChanged(watcher.seen()[0], claimed, sessions)
			Expect(imported.Err).ToNot(HaveOccurred())

			var names []string
			for _, t := range imported.Tools {
				names = append(names, t.Name())
			}
			Expect(names).To(ConsistOf("docs_search", "docs_fetch"))

			Expect(imported.Skipped).To(HaveLen(1))
			Expect(imported.Skipped[0].Name).To(Equal("summarize"))
			Expect(imported.Skipped[0].Reason).To(ContainSubstring(`the name "docs_summarize" is already taken`))
		})

		It("should skip a name a tool imported from a peer holds", func() {
			sessions := connected(config.MCPServer{Name: "docs", Command: "unused"})

			watcher := &changeWatcher{}
			DeferCleanup(sessions.OnToolListChanged(watcher.record))

			addTool("docs", "summarize")
			Eventually(watcher.count).Should(Equal(1))

			peer, err := functool.New(functool.Spec{
				Name:        "docs_summarize",
				Description: "a tool imported from a peer",
				Schema:      map[string]any{"type": "object"},
				Handler: func(context.Context, json.RawMessage, *functool.CallContext) (string, error) {
					return "", nil
				},
			})
			Expect(err).ToNot(HaveOccurred())

			imported := ImportChanged(watcher.seen()[0], NewClaimedNames(nil, map[string]*functool.Tool{"docs_summarize": peer}), sessions)
			Expect(imported.Skipped).To(HaveLen(1))
			Expect(imported.Skipped[0].Name).To(Equal("summarize"))
		})

		It("should refuse claimed names that were not built with NewClaimedNames", func() {
			imported := ImportChanged(ToolListChange{Server: config.MCPServer{Name: "docs"}}, ClaimedNames{}, nil)
			Expect(imported.Err).To(MatchError(ContainSubstring("must be built with mcpclient.NewClaimedNames")))
		})

		It("should carry a failed re-listing through as its own error", func() {
			imported := ImportChanged(ToolListChange{
				Server: config.MCPServer{Name: "docs"},
				Err:    fmt.Errorf("the tool list is unavailable"),
			}, NewClaimedNames(nil, nil), nil)
			Expect(imported.Err).To(MatchError(ContainSubstring("the tool list is unavailable")))
			Expect(imported.Tools).To(BeEmpty())
		})
	})
})
