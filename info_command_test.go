//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/choria-io/ui/columns"
	"github.com/choria-io/ui/table"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/mcpclient"
	"github.com/choria-io/fisk-ai/internal/toolkit/functool"
	"github.com/choria-io/fisk-ai/internal/util"
)

// mcpInfoServers stands a real mcp.Server up in this process for every server info
// dials, over an in-memory transport pair, so the specs drive genuine protocol traffic
// with no subprocess and no socket.
type mcpInfoServers struct {
	mu    sync.Mutex
	tools map[string][]*mcp.Tool
	fail  map[string]error
	dials int
}

func newMCPInfoServers() *mcpInfoServers {
	return &mcpInfoServers{tools: map[string][]*mcp.Tool{}, fail: map[string]error{}}
}

// dialer builds the mcpclient.Dialer discovery reaches these servers through.
func (f *mcpInfoServers) dialer() mcpclient.Dialer {
	return func(_ context.Context, server config.MCPServer) (mcp.Transport, error) {
		f.mu.Lock()
		f.dials++
		err := f.fail[server.Name]
		tools := f.tools[server.Name]
		f.mu.Unlock()

		if err != nil {
			return nil, err
		}

		clientSide, serverSide := mcp.NewInMemoryTransports()

		srv := mcp.NewServer(&mcp.Implementation{Name: server.Name, Version: "9.9.9"}, nil)
		for _, tool := range tools {
			srv.AddTool(tool, mcpInfoHandler)
		}

		// The server side is connected before the client, as in-memory transports
		// require, and under a context of its own: the caller's carries the connect
		// timeout, which has nothing to say about how long this server lives.
		_, err = srv.Connect(context.Background(), serverSide, nil)
		if err != nil {
			return nil, err
		}

		return clientSide, nil
	}
}

// dialed is how many servers discovery has asked for a transport for.
func (f *mcpInfoServers) dialed() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.dials
}

// mcpInfoTool is a descriptor a fake server advertises. An empty description is what a
// server that described a tool badly sends, which the import skips.
func mcpInfoTool(name string, description string) *mcp.Tool {
	return &mcp.Tool{Name: name, Description: description, InputSchema: json.RawMessage(`{"type":"object"}`)}
}

// mcpInfoHandler answers a call, which no spec here makes: info reads names and
// descriptions and calls nothing.
func mcpInfoHandler(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "handled by " + req.Params.Name}}}, nil
}

// mcpInfoRemoteTool is a function tool standing in for one the a2a import already named,
// whose names never reach the taken set.
func mcpInfoRemoteTool(name string) *functool.Tool {
	GinkgoHelper()

	tool, err := functool.New(functool.Spec{
		Name:        name,
		Description: "A tool an a2a peer already holds this name for",
		Schema:      map[string]any{"type": "object"},
		Handler: func(context.Context, json.RawMessage, *functool.CallContext) (string, error) {
			return "", nil
		},
	})
	Expect(err).ToNot(HaveOccurred())

	return tool
}

var _ = Describe("MCP servers", func() {
	var (
		ctx     context.Context
		cancel  context.CancelFunc
		servers *mcpInfoServers
	)

	BeforeEach(func() {
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		DeferCleanup(cancel)

		servers = newMCPInfoServers()
	})

	discover := func(claimed mcpclient.ClaimedNames, entries ...config.MCPServer) []mcpclient.ServerImport {
		GinkgoHelper()

		return mcpclient.DiscoverForInfo(ctx, mcpclient.Options{
			Servers:  entries,
			Identity: "fisk-info",
			Version:  "0.0.1",
			Dialer:   servers.dialer(),
		}, claimed)
	}

	render := func(imports []mcpclient.ServerImport) string {
		c := columns.New()
		printMCPServerStatus(c, imports)

		return c.String()
	}

	It("Should list a server's tools under the names a run would use", func() {
		servers.tools["docs"] = []*mcp.Tool{
			mcpInfoTool("search", "Searches the pages"),
			mcpInfoTool("read", "Reads a page"),
		}

		imports := discover(mcpclient.NewClaimedNames(nil, nil), config.MCPServer{
			Name:    "docs",
			Alias:   "dx",
			Command: "npx",
			Args:    []string{"-y", "docs-server"},
			Exclude: &config.ToolFilter{Tools: []string{"^read$"}},
		})
		Expect(imports).To(HaveLen(1))
		Expect(imports[0].Err).ToNot(HaveOccurred())

		out := render(imports)
		Expect(out).To(ContainSubstring("MCP servers"))
		Expect(out).To(ContainSubstring("docs (stdio npx -y docs-server): reachable in"))
		Expect(out).To(ContainSubstring(`advertised 2 tool(s), kept 1 after filtering, imported 1 as "dx"`))
		Expect(out).To(ContainSubstring("tools: dx_search"))

		tbl := table.NewTableWriter("")
		tbl.AddHeaders("Tool", "Source", "Confirm", "Description", "Tags")
		addMCPToolRows(tbl, imports)

		Expect(tbl.String()).To(MatchRegexp(`dx_search.*\bdx\b.*Searches the pages`))
	})

	It("Should skip a tool whose name an imported a2a tool already answers to", func() {
		servers.tools["docs"] = []*mcp.Tool{
			mcpInfoTool("search", "Searches the pages"),
			mcpInfoTool("read", "Reads a page"),
		}

		// The names the a2a import settled on are never written into the taken set, so
		// info passes the name map beside it. Given taken alone this tool would be named
		// over one the model already has.
		remote := map[string]*functool.Tool{"docs_search": mcpInfoRemoteTool("docs_search")}

		imports := discover(mcpclient.NewClaimedNames(map[string]bool{"docs_read": true}, remote), config.MCPServer{Name: "docs", Command: "unused"})
		Expect(imports[0].Tools).To(BeEmpty())

		out := render(imports)
		Expect(out).To(ContainSubstring(`tool "search" was not imported: the name "docs_search" is already taken`))
		Expect(out).To(ContainSubstring(`tool "read" was not imported: the name "docs_read" is already taken`))
	})

	It("Should render a server that cannot be reached with its error and still render the rest", func() {
		servers.fail["down"] = errors.New("the server did not start")
		servers.tools["docs"] = []*mcp.Tool{mcpInfoTool("search", "Searches the pages")}

		imports := discover(mcpclient.NewClaimedNames(nil, nil),
			config.MCPServer{Name: "down", Command: "unused"},
			config.MCPServer{Name: "docs", Command: "unused"},
		)
		Expect(imports).To(HaveLen(2))
		Expect(imports[0].Err).To(HaveOccurred())

		out := render(imports)
		Expect(out).To(ContainSubstring("down (stdio unused): UNAVAILABLE: the server did not start"))
		Expect(out).To(ContainSubstring("tools: docs_search"))
	})

	It("Should show a skipped tool with the reason it was left out", func() {
		servers.tools["docs"] = []*mcp.Tool{
			mcpInfoTool("search", "Searches the pages"),
			mcpInfoTool("read", ""),
		}

		imports := discover(mcpclient.NewClaimedNames(nil, nil), config.MCPServer{Name: "docs", Command: "unused"})

		out := render(imports)
		Expect(out).To(ContainSubstring("tools: docs_search"))
		Expect(out).To(ContainSubstring(`tool "read" was not imported: tool "read" from mcp server "docs" advertises no description`))
	})

	It("Should print the configured endpoint with its credentials redacted", func() {
		servers.tools["docs"] = []*mcp.Tool{mcpInfoTool("search", "Searches the pages")}

		imports := discover(mcpclient.NewClaimedNames(nil, nil), config.MCPServer{
			Name: "docs",
			URL:  "https://mcp.example.net/mcp/?apiKey=a-very-secret-token",
		})

		out := render(imports)
		Expect(out).To(ContainSubstring("docs (http https://mcp.example.net/mcp/?apiKey=REDACTED)"))
		Expect(out).ToNot(ContainSubstring("a-very-secret-token"))
	})

	It("Should redact an endpoint a stdio bridge carries in an argument", func() {
		servers.tools["docs"] = []*mcp.Tool{mcpInfoTool("search", "Searches the pages")}

		imports := discover(mcpclient.NewClaimedNames(nil, nil), config.MCPServer{
			Name:    "docs",
			Command: "npx",
			Args:    []string{"-y", "mcp-remote", "https://mcp.example.net/sse?key=a-very-secret-token"},
		})

		out := render(imports)
		Expect(out).To(ContainSubstring("docs (stdio npx -y mcp-remote https://mcp.example.net/sse?key=REDACTED)"))
		Expect(out).ToNot(ContainSubstring("a-very-secret-token"))
	})

	It("Should connect to nothing when no servers are configured", func() {
		imports := discover(mcpclient.NewClaimedNames(nil, nil))
		Expect(imports).To(BeNil())
		Expect(servers.dialed()).To(Equal(0))
		Expect(render(imports)).ToNot(ContainSubstring("MCP servers"))
	})
})

var _ = Describe("mcpServerTarget", func() {
	It("Should redact the endpoint a stdio argument carries", func() {
		target := mcpServerTarget(config.MCPServer{
			Name:    "docs",
			Command: "npx",
			Args:    []string{"-y", "mcp-remote", "https://mcp.example.net/sse?key=a-very-secret-token"},
		})

		Expect(target).To(Equal("stdio npx -y mcp-remote https://mcp.example.net/sse?key=REDACTED"))
	})

	It("Should redact the userinfo and the fragment of an endpoint in an argument", func() {
		target := mcpServerTarget(config.MCPServer{
			Name:    "docs",
			Command: "npx",
			Args:    []string{"mcp-remote", "https://operator:hunter2@mcp.example.net/sse#a-very-secret-token"},
		})

		Expect(target).To(Equal("stdio npx mcp-remote https://REDACTED@mcp.example.net/sse#REDACTED"))
	})

	// Most entries carry no url in their arguments at all, and an operator reads the
	// command line to check that it is the one they meant to run, so an ordinary
	// argument has to survive the redaction as it was written.
	It("Should print an ordinary command and its arguments unchanged", func() {
		target := mcpServerTarget(config.MCPServer{
			Name:    "docs",
			Command: "/usr/local/bin/server",
			Args:    []string{"-y", "mcp-remote", "--port=8080", "--config", "/etc/fisk/docs.yaml", ""},
		})

		Expect(target).To(Equal("stdio /usr/local/bin/server -y mcp-remote --port=8080 --config /etc/fisk/docs.yaml"))
	})

	It("Should print the configured endpoint of an http entry redacted", func() {
		target := mcpServerTarget(config.MCPServer{
			Name: "docs",
			URL:  "https://mcp.example.net/mcp/?apiKey=a-very-secret-token",
		})

		Expect(target).To(Equal("http https://mcp.example.net/mcp/?apiKey=REDACTED"))
	})
})

var _ = Describe("toolSearchStatus", func() {
	It("Should report tool search enabled for the default provider", func() {
		cfg := &config.Config{}
		cfg.LLM.Model = "claude-sonnet-5"
		Expect(toolSearchStatus(cfg, 3)).To(ContainSubstring("enabled"))
	})

	It("Should report the operator-disabled cause when no_tool_search is set", func() {
		cfg := &config.Config{}
		cfg.LLM.Model = "claude-sonnet-5"
		cfg.LLM.NoToolSearch = true
		Expect(toolSearchStatus(cfg, util.ToolSearchThreshold-1)).To(Equal("disabled (no_tool_search)"))
	})

	It("Should name what the operator-disabled tool search costs once the set crosses the threshold", func() {
		cfg := &config.Config{}
		cfg.LLM.Model = "claude-sonnet-5"
		cfg.LLM.NoToolSearch = true

		status := toolSearchStatus(cfg, 12)
		Expect(status).To(ContainSubstring("disabled (no_tool_search)"))
		Expect(status).To(ContainSubstring("12 tools are sent to the model directly"))
		Expect(status).To(ContainSubstring("Anthropic models only"))
	})

	It("Should report an unavailable provider that is not linked into the build", func() {
		cfg := &config.Config{}
		cfg.LLM.Model = "gpt-5"
		cfg.LLM.Provider = "openai"
		Expect(toolSearchStatus(cfg, 3)).To(ContainSubstring(`provider "openai" is not available`))
	})
})

var _ = Describe("printSessionsSection", func() {
	render := func(cfg *config.Config) string {
		c := columns.New()
		printSessionsSection(c, cfg)

		return c.String()
	}

	It("Should omit the section for an MCP-only config with no model", func() {
		cfg := &config.Config{}
		cfg.Harness.Sessions = &config.SessionConfig{Backend: "jetstream"}
		Expect(render(cfg)).ToNot(ContainSubstring("Sessions"))
	})

	It("Should show the file backend and its configured directory", func() {
		cfg := &config.Config{}
		cfg.LLM.Model = "claude-sonnet-5"
		cfg.Harness.Sessions = config.SessionConfigFromStateDir("/tmp/runs")

		out := render(cfg)
		Expect(out).To(ContainSubstring("Sessions"))
		Expect(out).To(ContainSubstring("file"))
		Expect(out).To(ContainSubstring("/tmp/runs"))
	})

	It("Should show the XDG default when the file backend has no directory", func() {
		cfg := &config.Config{}
		cfg.LLM.Model = "claude-sonnet-5"

		Expect(render(cfg)).To(ContainSubstring("(XDG default)"))
	})

	It("Should show the jetstream stream, context, and the derived-prefix note", func() {
		cfg := &config.Config{}
		cfg.LLM.Model = "claude-sonnet-5"
		cfg.NatsContext = "prod"
		cfg.Harness.Sessions = &config.SessionConfig{
			Backend: "jetstream",
			Options: json.RawMessage(`{"stream":"FISKSESSIONS"}`),
		}

		out := render(cfg)
		Expect(out).To(ContainSubstring("jetstream"))
		Expect(out).To(ContainSubstring("FISKSESSIONS"))
		Expect(out).To(ContainSubstring("prod"))
		Expect(out).To(ContainSubstring("derived from the stream"))
	})

	It("Should show (default) as the jetstream context when none is configured", func() {
		cfg := &config.Config{}
		cfg.LLM.Model = "claude-sonnet-5"
		cfg.Harness.Sessions = &config.SessionConfig{
			Backend: "jetstream",
			Options: json.RawMessage(`{"stream":"FISKSESSIONS"}`),
		}

		Expect(render(cfg)).To(ContainSubstring("(default)"))
	})
})
