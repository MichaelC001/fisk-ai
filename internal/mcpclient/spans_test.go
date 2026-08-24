//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/telemetry"
	"github.com/choria-io/fisk-ai/internal/toolkit/functool"
)

// secretArg is a credential where a bridge command puts one: on the command line. It
// is the shape that makes the rule here more than an argument about urls, since
// "npx -y mcp-remote https://host/sse?key=<token>" is what an operator writes to reach
// an HTTP server through a stdio client.
const secretArg = "https://mcp.example.net/sse?key=fisk-mcpclient-arg-token"

var _ = Describe("Spans", func() {
	var (
		ctx     context.Context
		cancel  context.CancelFunc
		exp     *tracetest.InMemoryExporter
		servers *fakeServers
	)

	BeforeEach(func() {
		exp = tracetest.NewInMemoryExporter()
		provider := telemetry.NewFromProviders(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp)), nil)

		ctx, cancel = context.WithTimeout(telemetry.ContextWithProvider(context.Background(), provider), 30*time.Second)
		DeferCleanup(cancel)

		servers = newFakeServers()
	})

	// named returns the one exported span with this name, so a spec asserts on the
	// span it means rather than on a position in the export order.
	named := func(name string) tracetest.SpanStub {
		GinkgoHelper()

		var found []tracetest.SpanStub
		for _, stub := range exp.GetSpans() {
			if stub.Name == name {
				found = append(found, stub)
			}
		}

		Expect(found).To(HaveLen(1), "expected exactly one %q span", name)

		return found[0]
	}

	attrOf := func(stub tracetest.SpanStub, key attribute.Key) (attribute.Value, bool) {
		for _, kv := range stub.Attributes {
			if kv.Key == key {
				return kv.Value, true
			}
		}

		return attribute.Value{}, false
	}

	// exported is everything a collector receives about a span as text, so a spec can
	// assert over the whole of it rather than over the attributes it thought to name.
	exported := func(stub tracetest.SpanStub) []string {
		out := []string{stub.Name, stub.Status.Description}

		for _, kv := range stub.Attributes {
			out = append(out, string(kv.Key), kv.Value.String())
		}

		for _, e := range stub.Events {
			out = append(out, e.Name)
			for _, kv := range e.Attributes {
				out = append(out, string(kv.Key), kv.Value.String())
			}
		}

		return out
	}

	It("should span the connect and the import of a server that answers", func() {
		servers.tools["docs"] = []fakeTool{
			{
				tool:    &mcp.Tool{Name: "search", Description: "Searches the documentation", InputSchema: json.RawMessage(`{"type":"object"}`)},
				handler: func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) { return nil, nil },
			},
			{
				tool:    &mcp.Tool{Name: "fetch", Description: "Fetches a document", InputSchema: json.RawMessage(`{"type":"object"}`)},
				handler: func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) { return nil, nil },
			},
			// Advertised, kept by the filters, and not built: it is skipped, which is the
			// third count the import span reports.
			{
				tool:    &mcp.Tool{Name: "undescribed", InputSchema: json.RawMessage(`{"type":"object"}`)},
				handler: func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) { return nil, nil },
			},
		}

		sessions, err := Connect(ctx, Options{
			Servers:  []config.MCPServer{{Name: "docs", Command: "unused"}},
			Identity: "fisk-test",
			Version:  "0.0.1",
			Dialer:   servers.dialer(),
		})
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(func() { Expect(sessions.Close()).To(Succeed()) })

		_, _, imports, err := ImportForRun(ctx, sessions, NewClaimedNames(nil, nil))
		Expect(err).ToNot(HaveOccurred())
		Expect(imports).To(HaveLen(1))

		connect := named("mcp_connect docs")
		v, ok := attrOf(connect, telemetry.AttrMCPServer)
		Expect(ok).To(BeTrue())
		Expect(v.AsString()).To(Equal("docs"))

		_, ok = attrOf(connect, telemetry.AttrMCPToolsDiscovered)
		Expect(ok).To(BeFalse())

		imported := named("mcp_import docs")
		for key, want := range map[attribute.Key]int64{
			telemetry.AttrMCPToolsDiscovered: 3,
			telemetry.AttrMCPToolsKept:       3,
			telemetry.AttrMCPToolsSkipped:    1,
		} {
			v, ok := attrOf(imported, key)
			Expect(ok).To(BeTrue(), "expected %s to be set", key)
			Expect(v.AsInt64()).To(Equal(want), "%s", key)
		}

		// The transport is the caller's own here, which is neither of the two this
		// package builds, and saying so is the honest answer.
		v, ok = attrOf(imported, telemetry.AttrMCPTransport)
		Expect(ok).To(BeTrue())
		Expect(v.AsString()).To(Equal(telemetry.MCPTransportOther.String()))
	})

	// A stdio child that cannot be started is the commonest failure an operator hits,
	// and its arguments are where the credential is: the span reports that the server
	// was unreachable and says nothing about what was run.
	It("should record a stdio server that will not start without exporting the command", func() {
		_, err := Connect(ctx, Options{
			Servers: []config.MCPServer{{
				Name:          "docs",
				Command:       "fisk-mcpclient-no-such-command",
				Args:          []string{"-y", "mcp-remote", secretArg},
				TimeoutParsed: 10 * time.Second,
			}},
			Identity: "fisk-test",
			Version:  "0.0.1",
		})
		Expect(err).To(HaveOccurred())

		connect := named("mcp_connect docs")

		v, ok := attrOf(connect, telemetry.AttrMCPTransport)
		Expect(ok).To(BeTrue())
		Expect(v.AsString()).To(Equal(telemetry.MCPTransportStdio.String()))

		v, ok = attrOf(connect, "error.type")
		Expect(ok).To(BeTrue())
		Expect(v.AsString()).To(Equal(telemetry.ClassRemoteUnavailable.String()))

		for _, text := range exported(connect) {
			Expect(text).ToNot(ContainSubstring("mcp-remote"))
			Expect(text).ToNot(ContainSubstring("fisk-mcpclient-no-such-command"))
			Expect(text).ToNot(ContainSubstring(secretArg))
		}
	})

	// The credential written into a url path as a literal is the one this tree cannot
	// redact out of an error, which is pinned by a spec of its own. The span carries it
	// anyway, because it carries no url at all.
	It("should keep the url off the span even where the error prints it", func() {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).ToNot(HaveOccurred())

		endpoint := fmt.Sprintf("http://%s/api/mcp/s/%s/mcp", listener.Addr().String(), literalToken)
		Expect(listener.Close()).To(Succeed())

		_, err = Connect(ctx, Options{
			Servers:  []config.MCPServer{{Name: "docs", URL: endpoint, TimeoutParsed: 10 * time.Second}},
			Identity: "fisk-test",
			Version:  "0.0.1",
		})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(literalToken))

		connect := named("mcp_connect docs")

		v, ok := attrOf(connect, telemetry.AttrMCPTransport)
		Expect(ok).To(BeTrue())
		Expect(v.AsString()).To(Equal(telemetry.MCPTransportHTTP.String()))

		for _, text := range exported(connect) {
			Expect(text).ToNot(ContainSubstring(literalToken))
			Expect(text).ToNot(ContainSubstring(listener.Addr().String()))
			Expect(text).ToNot(ContainSubstring("http://"))
		}
	})

	// A filter the operator wrote badly never reaches the server, so reporting it as an
	// unreachable server would send someone looking at the wrong end.
	It("should record a filter that will not compile as a configuration failure", func() {
		sessions, err := Connect(ctx, Options{
			Servers: []config.MCPServer{{
				Name:    "docs",
				Command: "unused",
				Include: &config.ToolFilter{Tools: []string{"["}},
			}},
			Identity: "fisk-test",
			Version:  "0.0.1",
			Dialer:   servers.dialer(),
		})
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(func() { Expect(sessions.Close()).To(Succeed()) })

		imports, err := Import(ctx, sessions, NewClaimedNames(nil, nil))
		Expect(err).ToNot(HaveOccurred())
		Expect(imports[0].Err).To(HaveOccurred())

		imported := named("mcp_import docs")

		v, ok := attrOf(imported, "error.type")
		Expect(ok).To(BeTrue())
		Expect(v.AsString()).To(Equal(telemetry.ClassConfig.String()))

		_, ok = attrOf(imported, telemetry.AttrMCPToolsDiscovered)
		Expect(ok).To(BeFalse())
	})

	// A server that says its tool list changed is re-listed on a goroutine of these
	// sessions' own, on a context belonging to no run, so there is no trace for that
	// work to join. It is left untraced deliberately rather than by omission.
	It("should open no span for a tool list a server changed", func() {
		servers.tools["docs"] = []fakeTool{{
			tool:    &mcp.Tool{Name: "search", Description: "Searches the documentation", InputSchema: json.RawMessage(`{"type":"object"}`)},
			handler: func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) { return nil, nil },
		}}

		sessions, err := Connect(ctx, Options{
			Servers:  []config.MCPServer{{Name: "docs", Command: "unused"}},
			Identity: "fisk-test",
			Version:  "0.0.1",
			Dialer:   servers.dialer(),
		})
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(func() { Expect(sessions.Close()).To(Succeed()) })

		changed := make(chan ToolListChange, 1)
		DeferCleanup(sessions.OnToolListChanged(func(c ToolListChange) { changed <- c }))

		before := len(exp.GetSpans())

		servers.server("docs").AddTool(&mcp.Tool{
			Name:        "summarize",
			Description: "a tool the server added",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) { return nil, nil })

		var change ToolListChange
		Eventually(changed).Should(Receive(&change))
		Expect(change.Err).ToNot(HaveOccurred())
		Expect(ImportChanged(change, NewClaimedNames(nil, map[string]*functool.Tool{}), sessions).Err).ToNot(HaveOccurred())

		Expect(exp.GetSpans()).To(HaveLen(before))
	})
})
