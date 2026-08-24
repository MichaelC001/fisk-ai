//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/toolkit/functool"
)

var _ = Describe("Tools", func() {
	var (
		ctx     context.Context
		cancel  context.CancelFunc
		servers *fakeServers
	)

	BeforeEach(func() {
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		DeferCleanup(cancel)

		servers = newFakeServers()
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
		DeferCleanup(func() { Expect(sessions.Close()).To(Succeed()) })

		return sessions
	}

	// imported names the tools an outcome built, in the order they were built.
	imported := func(imp ServerImport) []string {
		out := make([]string, 0, len(imp.Tools))
		for _, tool := range imp.Tools {
			out = append(out, tool.Name())
		}

		return out
	}

	Describe("Import", func() {
		It("should page through a server offering more tools than one page carries", func() {
			var tools []fakeTool
			for i := 0; i < 5; i++ {
				tools = append(tools, textTool(fmt.Sprintf("tool%d", i), "Does a thing", "done"))
			}
			servers.tools["docs"] = tools
			servers.pageSize["docs"] = 2

			sessions := connected(config.MCPServer{Name: "docs", Command: "unused"})

			imports, err := Import(ctx, sessions, NewClaimedNames(nil, nil))
			Expect(err).ToNot(HaveOccurred())
			Expect(imports).To(HaveLen(1))
			Expect(imports[0].Server.Name).To(Equal("docs"))
			Expect(imports[0].Discovered).To(Equal(5))
			Expect(imports[0].RTT).To(BeNumerically(">", 0))
			Expect(imported(imports[0])).To(Equal([]string{"docs_tool0", "docs_tool1", "docs_tool2", "docs_tool3", "docs_tool4"}))
		})

		It("should restrict to the tools the include filter matches", func() {
			servers.tools["docs"] = []fakeTool{
				textTool("read", "Reads a page", "read"),
				textTool("search", "Searches the pages", "found"),
				textTool("write", "Writes a page", "written"),
			}

			sessions := connected(config.MCPServer{
				Name:    "docs",
				Command: "unused",
				Include: &config.ToolFilter{Tools: []string{"^(read|search)$"}},
			})

			imports, err := Import(ctx, sessions, NewClaimedNames(nil, nil))
			Expect(err).ToNot(HaveOccurred())
			Expect(imports[0].Discovered).To(Equal(3))
			Expect(imports[0].Kept).To(HaveLen(2))
			Expect(imported(imports[0])).To(Equal([]string{"docs_read", "docs_search"}))
		})

		It("should remove the tools the exclude filter matches from what include kept", func() {
			servers.tools["docs"] = []fakeTool{
				textTool("read", "Reads a page", "read"),
				textTool("search", "Searches the pages", "found"),
				textTool("write", "Writes a page", "written"),
			}

			sessions := connected(config.MCPServer{
				Name:    "docs",
				Command: "unused",
				Include: &config.ToolFilter{Tools: []string{"^(read|search)$"}},
				Exclude: &config.ToolFilter{Tools: []string{"^read$"}},
			})

			imports, err := Import(ctx, sessions, NewClaimedNames(nil, nil))
			Expect(err).ToNot(HaveOccurred())
			Expect(imported(imports[0])).To(Equal([]string{"docs_search"}))
		})

		It("should prefix every tool whether or not the bare name would clash", func() {
			servers.tools["docs"] = []fakeTool{textTool("search", "Searches the pages", "found")}
			servers.tools["issues"] = []fakeTool{textTool("search", "Searches the issues", "found")}

			sessions := connected(
				config.MCPServer{Name: "docs", Command: "unused"},
				config.MCPServer{Name: "issues", Alias: "iss", Command: "unused"},
			)

			imports, err := Import(ctx, sessions, NewClaimedNames(nil, nil))
			Expect(err).ToNot(HaveOccurred())
			Expect(imports).To(HaveLen(2))
			Expect(imported(imports[0])).To(Equal([]string{"docs_search"}))
			Expect(imported(imports[1])).To(Equal([]string{"iss_search"}))
		})

		It("should skip a tool whose final name is not a usable tool name", func() {
			servers.tools["docs"] = []fakeTool{
				textTool("search the docs", "Searches the pages", "found"),
				textTool("read", "Reads a page", "read"),
			}

			sessions := connected(config.MCPServer{Name: "docs", Command: "unused"})

			imports, err := Import(ctx, sessions, NewClaimedNames(nil, nil))
			Expect(err).ToNot(HaveOccurred())
			Expect(imported(imports[0])).To(Equal([]string{"docs_read"}))
			Expect(imports[0].Skipped).To(HaveLen(1))
			Expect(imports[0].Skipped[0].Name).To(Equal("search the docs"))
			Expect(imports[0].Skipped[0].Reason).To(ContainSubstring(`it would be named "docs_search the docs", which is not a usable tool name`))
		})

		It("should skip a tool that advertises no description", func() {
			servers.tools["docs"] = []fakeTool{
				textTool("search", "", "found"),
				textTool("read", "Reads a page", "read"),
			}

			sessions := connected(config.MCPServer{Name: "docs", Command: "unused"})

			imports, err := Import(ctx, sessions, NewClaimedNames(nil, nil))
			Expect(err).ToNot(HaveOccurred())
			Expect(imported(imports[0])).To(Equal([]string{"docs_read"}))
			Expect(imports[0].Skipped).To(HaveLen(1))
			Expect(imports[0].Skipped[0].Name).To(Equal("search"))
			Expect(imports[0].Skipped[0].Reason).To(ContainSubstring(`tool "search" from mcp server "docs" advertises no description`))
		})

		It("should skip a tool whose input schema is not an object", func() {
			servers.tools["docs"] = []fakeTool{
				textTool("search", "Searches the pages", "found"),
				textTool("read", "Reads a page", "read"),
			}
			// AddTool refuses to register a descriptor like this, so the server is made to
			// answer with one rather than to hold one.
			servers.listing["docs"] = func(res *mcp.ListToolsResult) {
				for _, tool := range res.Tools {
					if tool.Name == "search" {
						tool.InputSchema = json.RawMessage(`"a string is not a schema"`)
					}
				}
			}

			sessions := connected(config.MCPServer{Name: "docs", Command: "unused"})

			imports, err := Import(ctx, sessions, NewClaimedNames(nil, nil))
			Expect(err).ToNot(HaveOccurred())
			Expect(imported(imports[0])).To(Equal([]string{"docs_read"}))
			Expect(imports[0].Skipped).To(HaveLen(1))
			Expect(imports[0].Skipped[0].Name).To(Equal("search"))
			Expect(imports[0].Skipped[0].Reason).To(ContainSubstring(`tool "search" from mcp server "docs" advertises no usable input schema`))
		})

		It("should skip a tool whose input schema declares a type other than an object", func() {
			servers.tools["docs"] = []fakeTool{
				textTool("search", "Searches the pages", "found"),
				textTool("read", "Reads a page", "read"),
			}
			// A JSON object passes the type assertion, so the root type is what stops a
			// descriptor the model API would refuse from reaching every call in the run.
			servers.listing["docs"] = func(res *mcp.ListToolsResult) {
				for _, tool := range res.Tools {
					if tool.Name == "search" {
						tool.InputSchema = json.RawMessage(`{"type":"string"}`)
					}
				}
			}

			sessions := connected(config.MCPServer{Name: "docs", Command: "unused"})

			imports, err := Import(ctx, sessions, NewClaimedNames(nil, nil))
			Expect(err).ToNot(HaveOccurred())
			Expect(imported(imports[0])).To(Equal([]string{"docs_read"}))
			Expect(imports[0].Skipped).To(HaveLen(1))
			Expect(imports[0].Skipped[0].Name).To(Equal("search"))
			Expect(imports[0].Skipped[0].Reason).To(ContainSubstring(`tool "search" from mcp server "docs" advertises an input schema of type string; a tool's arguments must be an object`))
		})

		It("should skip a tool that advertises no name", func() {
			servers.tools["docs"] = []fakeTool{
				textTool("search", "Searches the pages", "found"),
				textTool("read", "Reads a page", "read"),
			}
			// The name pattern accepts the bare "docs_" this would be named, so the empty
			// name is what stops a call going out naming no tool.
			servers.listing["docs"] = func(res *mcp.ListToolsResult) {
				for _, tool := range res.Tools {
					if tool.Name == "search" {
						tool.Name = ""
					}
				}
			}

			sessions := connected(config.MCPServer{Name: "docs", Command: "unused"})

			imports, err := Import(ctx, sessions, NewClaimedNames(nil, nil))
			Expect(err).ToNot(HaveOccurred())
			Expect(imported(imports[0])).To(Equal([]string{"docs_read"}))
			Expect(imports[0].Skipped).To(HaveLen(1))
			Expect(imports[0].Skipped[0].Name).To(BeEmpty())
			Expect(imports[0].Skipped[0].Reason).To(Equal(`mcp server "docs" advertises a tool with no name`))
		})

		It("should skip a name a local or built-in tool already claimed", func() {
			servers.tools["docs"] = []fakeTool{
				textTool("search", "Searches the pages", "found"),
				textTool("read", "Reads a page", "read"),
			}

			sessions := connected(config.MCPServer{Name: "docs", Command: "unused"})

			imports, err := Import(ctx, sessions, NewClaimedNames(map[string]bool{"docs_search": true}, nil))
			Expect(err).To(MatchError(ContainSubstring(`imported mcp tool name collision: "docs_search" (mcp server "docs")`)))
			Expect(imported(imports[0])).To(Equal([]string{"docs_read"}))
			Expect(imports[0].Skipped).To(HaveLen(1))
			Expect(imports[0].Skipped[0].Name).To(Equal("search"))
			Expect(imports[0].Skipped[0].Reason).To(ContainSubstring(`the name "docs_search" is already taken`))
		})

		It("should skip a name an imported a2a tool already claimed, which the taken set does not hold", func() {
			servers.tools["docs"] = []fakeTool{
				textTool("search", "Searches the pages", "found"),
				textTool("read", "Reads a page", "read"),
			}

			sessions := connected(config.MCPServer{Name: "docs", Command: "unused"})

			remote := map[string]*functool.Tool{"docs_search": localTool("docs_search")}

			imports, err := Import(ctx, sessions, NewClaimedNames(nil, remote))
			Expect(err).To(MatchError(ContainSubstring(`imported mcp tool name collision: "docs_search" (mcp server "docs")`)))
			Expect(imported(imports[0])).To(Equal([]string{"docs_read"}))
			Expect(imports[0].Skipped).To(HaveLen(1))
			Expect(imports[0].Skipped[0].Reason).To(ContainSubstring(`the name "docs_search" is already taken`))
		})

		It("should refuse claimed names that were not built with NewClaimedNames", func() {
			sessions := connected(config.MCPServer{Name: "docs", Command: "unused"})

			imports, err := Import(ctx, sessions, ClaimedNames{})
			Expect(err).To(MatchError(ContainSubstring("must be built with mcpclient.NewClaimedNames")))
			Expect(imports).To(BeNil())
		})

		It("should record the failure of a server it cannot list and leave the others alone", func() {
			sessions := connected(config.MCPServer{Name: "docs", Command: "unused"})
			Expect(sessions.Close()).To(Succeed())

			imports, err := Import(ctx, sessions, NewClaimedNames(nil, nil))
			Expect(err).ToNot(HaveOccurred())
			Expect(imports).To(HaveLen(1))
			Expect(imports[0].Err).To(MatchError(ContainSubstring("are closed")))
			Expect(imports[0].Tools).To(BeEmpty())
		})

		It("should give up on a server that never answers the list and import the ones after it", func() {
			servers.tools["slow"] = []fakeTool{textTool("search", "Searches the pages", "found")}
			servers.tools["docs"] = []fakeTool{textTool("read", "Reads a page", "read")}
			servers.stallList("slow")

			sessions := connected(
				config.MCPServer{Name: "slow", Command: "unused", TimeoutParsed: 250 * time.Millisecond},
				config.MCPServer{Name: "docs", Command: "unused"},
			)

			imports, err := Import(ctx, sessions, NewClaimedNames(nil, nil))
			Expect(err).ToNot(HaveOccurred())
			Expect(imports).To(HaveLen(2))
			Expect(imports[0].Err).To(MatchError(ContainSubstring(`listing the tools of mcp server "slow": it did not answer within 250ms`)))
			Expect(imports[0].Discovered).To(BeZero())
			Expect(imports[0].Tools).To(BeEmpty())
			Expect(imported(imports[1])).To(Equal([]string{"docs_read"}))
		})

		It("should fail a server whose filters do not compile, without listing it", func() {
			servers.tools["docs"] = []fakeTool{textTool("read", "Reads a page", "read")}

			sessions := connected(config.MCPServer{
				Name:    "docs",
				Command: "unused",
				Include: &config.ToolFilter{Tools: []string{"^(read"}},
			})

			imports, err := Import(ctx, sessions, NewClaimedNames(nil, nil))
			Expect(err).ToNot(HaveOccurred())
			Expect(imports[0].Err).To(MatchError(ContainSubstring(`invalid mcp tool filter pattern "^(read"`)))
			Expect(imports[0].RTT).To(BeZero())
			Expect(imports[0].Discovered).To(BeZero())
			Expect(imports[0].Kept).To(BeEmpty())
			Expect(imports[0].Tools).To(BeEmpty())
			Expect(imports[0].Skipped).To(BeEmpty())
		})
	})

	Describe("ImportForRun", func() {
		It("should return the tools in server order and keyed by name", func() {
			servers.tools["docs"] = []fakeTool{textTool("search", "Searches the pages", "found")}
			servers.tools["issues"] = []fakeTool{textTool("open", "Opens an issue", "opened")}

			sessions := connected(
				config.MCPServer{Name: "docs", Command: "unused"},
				config.MCPServer{Name: "issues", Command: "unused"},
			)

			tools, byName, imports, err := ImportForRun(ctx, sessions, NewClaimedNames(nil, nil))
			Expect(err).ToNot(HaveOccurred())
			Expect(imports).To(HaveLen(2))
			Expect(tools).To(HaveLen(2))
			Expect(tools[0].Name()).To(Equal("docs_search"))
			Expect(tools[1].Name()).To(Equal("issues_open"))
			Expect(byName).To(HaveKey("docs_search"))
			Expect(byName).To(HaveKey("issues_open"))
		})

		It("should fail on a server it cannot list", func() {
			sessions := connected(config.MCPServer{Name: "docs", Command: "unused"})
			Expect(sessions.Close()).To(Succeed())

			_, _, imports, err := ImportForRun(ctx, sessions, NewClaimedNames(nil, nil))
			Expect(err).To(MatchError(ContainSubstring(`importing tools from mcp server "docs"`)))
			Expect(imports).To(HaveLen(1))
		})

		It("should fail on a name collision", func() {
			servers.tools["docs"] = []fakeTool{textTool("search", "Searches the pages", "found")}

			sessions := connected(config.MCPServer{Name: "docs", Command: "unused"})

			_, _, imports, err := ImportForRun(ctx, sessions, NewClaimedNames(map[string]bool{"docs_search": true}, nil))
			Expect(err).To(MatchError(ContainSubstring("imported mcp tool name collision")))
			Expect(imports[0].Skipped).To(HaveLen(1))
		})
	})

	Describe("DiscoverForInfo", func() {
		// The sessions this opens are its own and nobody else can close them: it returns
		// outcomes rather than a Sessions, so a session it left open would hold a stdio
		// child for the life of the command. The server side of each in-memory pair is
		// what reports it, so this reads the end of the session rather than the client's
		// bookkeeping about it.
		It("should close every session it opened", func() {
			servers.tools["docs"] = []fakeTool{textTool("search", "Searches the pages", "found")}
			servers.tools["issues"] = []fakeTool{textTool("open", "Opens an issue", "opened")}

			imports := DiscoverForInfo(ctx, Options{
				Servers:  []config.MCPServer{{Name: "docs", Command: "unused"}, {Name: "issues", Command: "unused"}},
				Identity: "fisk-test",
				Version:  "0.0.1",
				Dialer:   servers.dialer(),
			}, NewClaimedNames(nil, nil))
			Expect(imports).To(HaveLen(2))
			Expect(imports[0].Err).ToNot(HaveOccurred())
			Expect(imports[1].Err).ToNot(HaveOccurred())

			served := servers.served()
			Expect(served).To(HaveLen(2))

			for _, session := range served {
				Eventually(ended(session)).Should(BeClosed())
			}
		})
	})

	Describe("NewTool", func() {
		// built imports one server and returns the single tool it built, for the specs
		// that drive a call through the real protocol.
		built := func(tool fakeTool) *functool.Tool {
			GinkgoHelper()

			servers.tools["docs"] = []fakeTool{tool}
			sessions := connected(config.MCPServer{Name: "docs", Command: "unused"})

			imports, err := Import(ctx, sessions, NewClaimedNames(nil, nil))
			Expect(err).ToNot(HaveOccurred())
			Expect(imports[0].Skipped).To(BeEmpty())
			Expect(imports[0].Tools).To(HaveLen(1))

			return imports[0].Tools[0]
		}

		call := func(tool *functool.Tool, input string) (string, error) {
			out, err := tool.Execute(ctx, json.RawMessage(input), toolkit.ExecDeps{})
			if err != nil {
				return "", err
			}

			return out.Output, nil
		}

		It("should present the tool as served by its mcp server", func() {
			tool := built(textTool("search", "Searches the pages", "found"))

			info := tool.Describe(nil)
			Expect(info.Kind).To(Equal(toolkit.KindMCP))
			Expect(info.Present).To(Equal(toolkit.PresentRemote))
			Expect(info.Agent).To(Equal("docs"))
			Expect(tool.Description()).To(Equal("Searches the pages"))
			Expect(tool.InputSchema()).To(HaveKeyWithValue("type", "object"))
		})

		It("should send the model's arguments to the server", func() {
			tool := built(resultTool("search", "Searches the pages", func(req *mcp.CallToolRequest) *mcp.CallToolResult {
				args, err := json.Marshal(req.Params.Arguments)
				Expect(err).ToNot(HaveOccurred())

				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(args)}}}
			}))

			out, err := call(tool, `{"query":"acme","limit":2}`)
			Expect(err).ToNot(HaveOccurred())
			Expect(out).To(MatchJSON(`{"query":"acme","limit":2}`))
		})

		It("should concatenate the text blocks in the order the server sent them", func() {
			tool := built(resultTool("search", "Searches the pages", func(*mcp.CallToolRequest) *mcp.CallToolResult {
				return &mcp.CallToolResult{Content: []mcp.Content{
					&mcp.TextContent{Text: "first "},
					&mcp.TextContent{Text: "second"},
				}}
			}))

			out, err := call(tool, `{}`)
			Expect(err).ToNot(HaveOccurred())
			Expect(out).To(Equal("first second"))
		})

		It("should render a block that is not text as one line naming it", func() {
			tool := built(resultTool("search", "Searches the pages", func(*mcp.CallToolRequest) *mcp.CallToolResult {
				return &mcp.CallToolResult{Content: []mcp.Content{
					&mcp.TextContent{Text: "the diagram"},
					&mcp.ImageContent{MIMEType: "image/png", Data: []byte{1, 2, 3}},
					&mcp.TextContent{Text: "follows"},
				}}
			}))

			out, err := call(tool, `{}`)
			Expect(err).ToNot(HaveOccurred())
			Expect(out).To(Equal("the diagram\n[image content of type image/png, not shown]\nfollows"))
		})

		It("should name a block that carries no mime type by its type alone", func() {
			tool := built(resultTool("search", "Searches the pages", func(*mcp.CallToolRequest) *mcp.CallToolResult {
				return &mcp.CallToolResult{Content: []mcp.Content{
					&mcp.EmbeddedResource{Resource: &mcp.ResourceContents{URI: "file:///notes"}},
				}}
			}))

			out, err := call(tool, `{}`)
			Expect(err).ToNot(HaveOccurred())
			Expect(out).To(Equal("[embedded resource content, not shown]"))
		})

		It("should return structured content verbatim", func() {
			tool := built(resultTool("search", "Searches the pages", func(*mcp.CallToolRequest) *mcp.CallToolResult {
				return &mcp.CallToolResult{
					Content:           []mcp.Content{&mcp.TextContent{Text: "ignored"}},
					StructuredContent: map[string]any{"hits": 2, "page": "intro"},
				}
			}))

			out, err := call(tool, `{}`)
			Expect(err).ToNot(HaveOccurred())
			Expect(out).To(MatchJSON(`{"hits":2,"page":"intro"}`))
		})

		It("should return an error carrying the server's own text for a result flagged as an error", func() {
			tool := built(resultTool("search", "Searches the pages", func(*mcp.CallToolRequest) *mcp.CallToolResult {
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: "the index is not built"}},
				}
			}))

			_, err := call(tool, `{}`)
			Expect(err).To(MatchError("the index is not built"))
		})

		It("should report an error result that says nothing about itself", func() {
			tool := built(resultTool("search", "Searches the pages", func(*mcp.CallToolRequest) *mcp.CallToolResult {
				return &mcp.CallToolResult{IsError: true}
			}))

			_, err := call(tool, `{}`)
			Expect(err).To(MatchError(`tool "search" on mcp server "docs" reported an error and said nothing about it`))
		})

		It("should name the tool and the server when the call cannot be made", func() {
			servers.tools["docs"] = []fakeTool{textTool("search", "Searches the pages", "found")}
			sessions := connected(config.MCPServer{Name: "docs", Command: "unused"})

			imports, err := Import(ctx, sessions, NewClaimedNames(nil, nil))
			Expect(err).ToNot(HaveOccurred())
			tool := imports[0].Tools[0]

			Expect(sessions.Close()).To(Succeed())

			_, err = tool.Execute(ctx, json.RawMessage(`{}`), toolkit.ExecDeps{})
			Expect(err).To(MatchError(ContainSubstring(`calling tool "search" on mcp server "docs"`)))
			Expect(err).To(MatchError(ContainSubstring("are closed")))
		})

		It("should carry the annotations the server declared as the tool's behavior", func() {
			tool := built(fakeTool{
				tool: &mcp.Tool{
					Name:        "search",
					Description: "Searches the pages",
					InputSchema: json.RawMessage(`{"type":"object"}`),
					Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: boolPtr(false)},
				},
				handler: func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					return &mcp.CallToolResult{}, nil
				},
			})

			Expect(tool.Behavior()).To(Equal(toolkit.Behavior{
				ReadOnly:   toolkit.HintTrue,
				Idempotent: toolkit.HintTrue,
				OpenWorld:  toolkit.HintFalse,
			}))
			// The declaration is read back here and nowhere else: what the model is told
			// stays the server's own text, which already says what the server says.
			Expect(tool.ModelDescription()).To(Equal("Searches the pages"))
		})

		It("should resolve a server that contradicts itself rather than dropping its tool", func() {
			tool, err := NewTool("docs_wipe", "docs", &mcp.Tool{
				Name:        "wipe",
				Description: "Wipes the index",
				InputSchema: map[string]any{"type": "object"},
				Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: boolPtr(true)},
			}, nil)
			Expect(err).ToNot(HaveOccurred())
			Expect(tool.Behavior()).To(Equal(toolkit.Behavior{
				ReadOnly:    toolkit.HintFalse,
				Destructive: toolkit.HintTrue,
			}))
		})

		It("should refuse a descriptor with no usable schema or no description", func() {
			_, err := NewTool("docs_search", "docs", &mcp.Tool{Name: "search", Description: "Searches"}, nil)
			Expect(err).To(MatchError(`tool "search" from mcp server "docs" advertises no usable input schema; it must be a JSON object`))

			_, err = NewTool("docs_search", "docs", &mcp.Tool{Name: "search", InputSchema: map[string]any{"type": "object"}}, nil)
			Expect(err).To(MatchError(`tool "search" from mcp server "docs" advertises no description`))
		})

		It("should refuse a descriptor with no name", func() {
			_, err := NewTool("docs_", "docs", &mcp.Tool{Description: "Searches", InputSchema: map[string]any{"type": "object"}}, nil)
			Expect(err).To(MatchError(`mcp server "docs" advertises a tool with no name`))
		})

		It("should accept a root schema that declares no type and refuse one that declares another", func() {
			tool, err := NewTool("docs_search", "docs", &mcp.Tool{
				Name:        "search",
				Description: "Searches",
				InputSchema: map[string]any{"properties": map[string]any{"query": map[string]any{"type": "string"}}},
			}, nil)
			Expect(err).ToNot(HaveOccurred())
			Expect(tool.InputSchema()).ToNot(HaveKey("type"))

			_, err = NewTool("docs_search", "docs", &mcp.Tool{
				Name:        "search",
				Description: "Searches",
				InputSchema: map[string]any{"type": []any{"object", "null"}},
			}, nil)
			Expect(err).To(MatchError(`tool "search" from mcp server "docs" advertises an input schema of type [object null]; a tool's arguments must be an object`))
		})
	})

	Describe("BehaviorFromAnnotations", func() {
		It("should declare nothing for a server that declared nothing", func() {
			Expect(BehaviorFromAnnotations(nil)).To(Equal(toolkit.Behavior{}))
		})

		It("should read an absent read-only or idempotent hint as unset rather than false", func() {
			// Both are plain bools on the wire, so a server that said nothing and one that
			// said false are the same bytes. HintFalse would be an assertion the server
			// never made.
			Expect(BehaviorFromAnnotations(&mcp.ToolAnnotations{})).To(Equal(toolkit.Behavior{}))
			Expect(BehaviorFromAnnotations(&mcp.ToolAnnotations{ReadOnlyHint: false, IdempotentHint: false})).To(Equal(toolkit.Behavior{}))
		})

		It("should carry a declared read-only or idempotent hint", func() {
			Expect(BehaviorFromAnnotations(&mcp.ToolAnnotations{ReadOnlyHint: true})).To(Equal(toolkit.Behavior{ReadOnly: toolkit.HintTrue}))
			Expect(BehaviorFromAnnotations(&mcp.ToolAnnotations{IdempotentHint: true})).To(Equal(toolkit.Behavior{Idempotent: toolkit.HintTrue}))
		})

		It("should carry all three states of the destructive and open-world hints", func() {
			Expect(BehaviorFromAnnotations(&mcp.ToolAnnotations{DestructiveHint: boolPtr(true)})).To(Equal(toolkit.Behavior{Destructive: toolkit.HintTrue}))
			Expect(BehaviorFromAnnotations(&mcp.ToolAnnotations{DestructiveHint: boolPtr(false)})).To(Equal(toolkit.Behavior{Destructive: toolkit.HintFalse}))
			Expect(BehaviorFromAnnotations(&mcp.ToolAnnotations{OpenWorldHint: boolPtr(true)})).To(Equal(toolkit.Behavior{OpenWorld: toolkit.HintTrue}))
			Expect(BehaviorFromAnnotations(&mcp.ToolAnnotations{OpenWorldHint: boolPtr(false)})).To(Equal(toolkit.Behavior{OpenWorld: toolkit.HintFalse}))
		})

		It("should return the server's claim unresolved", func() {
			Expect(BehaviorFromAnnotations(&mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: boolPtr(true)})).To(Equal(toolkit.Behavior{
				ReadOnly:    toolkit.HintTrue,
				Destructive: toolkit.HintTrue,
			}))
		})
	})
})

// textTool is a fake tool answering every call with one text block.
func textTool(name string, description string, text string) fakeTool {
	return resultTool(name, description, func(*mcp.CallToolRequest) *mcp.CallToolResult {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
	})
}

// resultTool is a fake tool answering every call with the result answer builds.
func resultTool(name string, description string, answer func(*mcp.CallToolRequest) *mcp.CallToolResult) fakeTool {
	return fakeTool{
		tool: &mcp.Tool{
			Name:        name,
			Description: description,
			InputSchema: json.RawMessage(`{"type":"object"}`),
		},
		handler: func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return answer(req), nil
		},
	}
}

// localTool is a function tool standing in for one an a2a import already named.
func localTool(name string) *functool.Tool {
	GinkgoHelper()

	tool, err := functool.New(functool.Spec{
		Name:        name,
		Description: "A tool that already holds this name",
		Schema:      map[string]any{"type": "object"},
		Handler: func(context.Context, json.RawMessage, *functool.CallContext) (string, error) {
			return "", nil
		},
	})
	Expect(err).ToNot(HaveOccurred())

	return tool
}

func boolPtr(v bool) *bool { return &v }
