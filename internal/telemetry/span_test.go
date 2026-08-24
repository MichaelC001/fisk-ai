//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"errors"
	"reflect"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

var _ = Describe("StartStartup", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("should name the span after the identity and record the remote host count", func() {
		p, exp := recording()

		_, span := p.StartStartup(ctx, StartupInfo{Identity: "demo", RemoteHosts: 2})
		span.End()

		spans := exp.GetSpans()
		Expect(spans).To(HaveLen(1))
		Expect(spans[0].Name).To(Equal("startup demo"))
		Expect(spans[0].SpanKind).To(Equal(trace.SpanKindInternal))

		v, ok := attrOf(spans[0], AttrRemoteHosts)
		Expect(ok).To(BeTrue())
		Expect(v.AsInt64()).To(Equal(int64(2)))
	})

	It("should carry the span in the returned context so setup work nests under it", func() {
		p, _ := recording()

		spanCtx, span := p.StartStartup(ctx, StartupInfo{Identity: "demo"})
		defer span.End()

		Expect(trace.SpanContextFromContext(spanCtx).IsValid()).To(BeTrue())
	})

	// The tool counts are not known until well into setup, which is why they are a
	// setter rather than a constructor argument: the span has to exist before then so
	// the failures on the way there are recorded at all.
	It("should record the tool inventory set after the span started", func() {
		p, exp := recording()

		_, span := p.StartStartup(ctx, StartupInfo{Identity: "demo"})
		span.SetTools(ToolCounts{Application: 5, Builtin: 3, Remote: 2, Custom: 1, Deferred: true})
		span.End()

		spans := exp.GetSpans()
		Expect(spans).To(HaveLen(1))

		for key, want := range map[attribute.Key]int64{
			AttrToolsApplication: 5,
			AttrToolsBuiltin:     3,
			AttrToolsRemote:      2,
			AttrToolsCustom:      1,
		} {
			v, ok := attrOf(spans[0], key)
			Expect(ok).To(BeTrue(), "expected %s to be set", key)
			Expect(v.AsInt64()).To(Equal(want), "%s", key)
		}

		v, ok := attrOf(spans[0], AttrToolsDeferred)
		Expect(ok).To(BeTrue())
		Expect(v.AsBool()).To(BeTrue())
	})

	// Without this the span reports a smaller tool set than the run has the moment a
	// server contributes one, and the count it reports is not visibly short: every
	// other source is there and the total simply does not match what the model was
	// offered.
	It("should count the tools imported from mcp servers", func() {
		p, exp := recording()

		_, span := p.StartStartup(ctx, StartupInfo{Identity: "demo"})
		span.SetTools(ToolCounts{Application: 5, Remote: 2, MCP: 4})
		span.End()

		spans := exp.GetSpans()
		Expect(spans).To(HaveLen(1))

		for key, want := range map[attribute.Key]int64{
			AttrToolsMCP:    4,
			AttrToolsRemote: 2,
		} {
			v, ok := attrOf(spans[0], key)
			Expect(ok).To(BeTrue(), "expected %s to be set", key)
			Expect(v.AsInt64()).To(Equal(want), "%s", key)
		}
	})

	It("should export nothing until the span ends", func() {
		p, exp := recording()

		_, span := p.StartStartup(ctx, StartupInfo{Identity: "demo"})
		Expect(exp.GetSpans()).To(BeEmpty())

		span.End()
		Expect(exp.GetSpans()).To(HaveLen(1))
	})
})

var _ = Describe("Span.Fail", func() {
	It("should record the class as error.type and mark the span failed", func() {
		p, exp := recording()

		_, span := p.StartStartup(context.Background(), StartupInfo{Identity: "demo"})
		span.Fail(errors.New("dial tcp: connection refused"), ClassStore)
		span.End()

		spans := exp.GetSpans()
		Expect(spans).To(HaveLen(1))
		Expect(spans[0].Status.Code).To(Equal(codes.Error))

		v, ok := attrOf(spans[0], "error.type")
		Expect(ok).To(BeTrue())
		Expect(v.AsString()).To(Equal(ClassStore.String()))
	})

	// This tree's errors embed absolute paths, config values and the config file path,
	// and a span status crosses a trust boundary to a backend where it cannot be
	// un-sent. Only the closed class vocabulary leaves the process.
	It("should never export the error text", func() {
		p, exp := recording()

		_, span := p.StartStartup(context.Background(), StartupInfo{Identity: "demo"})
		span.Fail(errors.New("/home/operator/secret/agent.yaml is unreadable"), ClassConfig)
		span.End()

		spans := exp.GetSpans()
		Expect(spans[0].Status.Description).To(BeEmpty())
		for _, kv := range spans[0].Attributes {
			Expect(kv.Value.String()).ToNot(ContainSubstring("/home/operator"))
		}
	})

	// span.RecordError would attach exception.stacktrace, which is exactly what the run
	// path works to keep off anything leaving the process. Nothing here may call it.
	It("should never record an exception event or a stack trace", func() {
		p, exp := recording()

		_, span := p.StartStartup(context.Background(), StartupInfo{Identity: "demo"})
		span.Fail(errors.New("boom"), ClassPanic)
		span.End()

		spans := exp.GetSpans()
		Expect(spans[0].Events).To(BeEmpty())

		_, ok := attrOf(spans[0], "exception.stacktrace")
		Expect(ok).To(BeFalse())
	})

	// A nil error being a no-op is what lets a deferred call site pass a named return
	// without guarding it, which is how the startup span covers its early returns.
	It("should do nothing for a nil error", func() {
		p, exp := recording()

		_, span := p.StartStartup(context.Background(), StartupInfo{Identity: "demo"})
		span.Fail(nil, ClassConfig)
		span.End()

		spans := exp.GetSpans()
		Expect(spans[0].Status.Code).To(Equal(codes.Unset))

		_, ok := attrOf(spans[0], "error.type")
		Expect(ok).To(BeFalse())
	})

	// A failure that named no class still has to be findable. An empty error.type is not
	// a value a backend can group by, so it is the one shape worse than an imprecise
	// class, and this is what makes the fallback deliberate rather than incidental.
	It("should export the catch-all rather than an empty class", func() {
		p, exp := recording()

		_, span := p.StartStartup(context.Background(), StartupInfo{Identity: "demo"})
		span.Fail(errors.New("boom"), ErrorClass{})
		span.End()

		spans := exp.GetSpans()
		Expect(spans[0].Status.Code).To(Equal(codes.Error))

		v, ok := attrOf(spans[0], "error.type")
		Expect(ok).To(BeTrue())
		Expect(v.AsString()).To(Equal(ClassOther.String()))
		Expect(v.AsString()).ToNot(BeEmpty())
	})
})

// ErrorClass is a struct wrapping a string rather than a defined string type, and the
// closure that buys is a compile-time property no spec can assert: there is no exported
// way to build a value, so telemetry.ErrorClass(err.Error()) does not compile from
// anywhere. What is worth asserting is that the vocabulary renders, since String() is what
// every span and metric attribute goes through and an empty one would be exported silently.
var _ = Describe("ErrorClass", func() {
	It("should render every class as a non-empty distinct value", func() {
		classes := []ErrorClass{
			ClassConfig, ClassProvider, ClassTimeout, ClassCanceled, ClassToolError,
			ClassTruncated, ClassRefusal, ClassPanic, ClassStore, ClassInvalidQuery,
			ClassRemoteUnavailable, ClassOther,
		}

		seen := map[string]bool{}
		for _, c := range classes {
			Expect(c.Set()).To(BeTrue())
			Expect(c.String()).ToNot(BeEmpty())
			Expect(seen[c.String()]).To(BeFalse(), "%q is declared twice", c.String())
			seen[c.String()] = true
		}
	})

	It("should report the zero value as unnamed", func() {
		Expect(ErrorClass{}.Set()).To(BeFalse())
		Expect(ErrorClass{}.String()).To(BeEmpty())
	})

	// ClassifyContext returns the zero value for anything it declines, which is what lets
	// a caller write `class, ok := ClassifyContext(err)` and fall through on ok alone.
	It("should return an unnamed class for an error it does not recognize", func() {
		class, ok := ClassifyContext(errors.New("connection refused"))
		Expect(ok).To(BeFalse())
		Expect(class.Set()).To(BeFalse())
	})
})

// mcpSecret is what every value an MCP span is given, other than the server name and
// the transport token, is filled with before it is exported. It is shaped like the two
// things that must never reach a collector: a url carrying a credential in its
// userinfo and its query, and the bridge command that takes the same credential as an
// argument.
const mcpSecret = "npx -y mcp-remote https://user:pw@host/sse?key=hunter2"

// poison fills every string, string slice and string map field of the struct v points
// at with mcpSecret, except the fields keep names, which take the value it gives them.
// It does the same to the struct and pointer-to-struct fields it finds, all the way
// down, so a value carried a level below the type a span is handed is filled too.
//
// It is written over reflection rather than as a literal so the specs below hold a
// rule instead of a list. A field added to one of these types later is poisoned
// without anyone remembering to poison it, so a url, a command or an argument that
// starts being carried here fails those specs on the day it is added rather than
// waiting for a reviewer to notice.
func poison(v any, keep map[string]string) {
	poisonStruct(reflect.ValueOf(v).Elem(), keep, map[reflect.Type]bool{})
}

// poisonStruct fills the struct rv and everything under it. seen holds the struct
// types on the path from the value poison was handed, so a type that reaches itself
// is filled once and the descent stops there instead of allocating pointers until the
// test runs out of memory.
func poisonStruct(rv reflect.Value, keep map[string]string, seen map[reflect.Type]bool) {
	if seen[rv.Type()] {
		return
	}
	seen[rv.Type()] = true
	defer delete(seen, rv.Type())

	for i := range rv.NumField() {
		f := rv.Field(i)
		if !f.CanSet() {
			continue
		}

		kept, wanted := keep[rv.Type().Field(i).Name]

		switch {
		case f.Kind() == reflect.String && wanted:
			f.SetString(kept)

		case f.Kind() == reflect.String:
			f.SetString(mcpSecret)

		case f.Kind() == reflect.Slice && f.Type().Elem().Kind() == reflect.String:
			slice := reflect.MakeSlice(f.Type(), 1, 1)
			slice.Index(0).SetString(mcpSecret)
			f.Set(slice)

		case f.Kind() == reflect.Map && f.Type().Key().Kind() == reflect.String && f.Type().Elem().Kind() == reflect.String:
			m := reflect.MakeMap(f.Type())
			key := reflect.New(f.Type().Key()).Elem()
			key.SetString(mcpSecret)
			value := reflect.New(f.Type().Elem()).Elem()
			value.SetString(mcpSecret)
			m.SetMapIndex(key, value)
			f.Set(m)

		case f.Kind() == reflect.Struct:
			poisonStruct(f, keep, seen)

		case f.Kind() == reflect.Pointer && f.Type().Elem().Kind() == reflect.Struct:
			// A nil pointer is allocated first: a field the caller left unset is where
			// a string added below it would otherwise sit unfilled, which is the case
			// the flat filler missed.
			if f.IsNil() {
				f.Set(reflect.New(f.Type().Elem()))
			}
			poisonStruct(f.Elem(), keep, seen)
		}
	}
}

// selfReferential reaches itself, which is the shape that would keep poison
// allocating pointers forever.
type selfReferential struct {
	URL  string
	Next *selfReferential
}

// exportedText is everything a collector receives about a span as text: its name, its
// status message, every attribute value whatever its type, and the same for each
// event. A value a span carries that is not in here is a value that never leaves.
func exportedText(stub tracetest.SpanStub) []string {
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

var _ = Describe("MCP server spans", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("should record the server, the transport and what the listing found", func() {
		p, exp := recording()

		_, span := p.StartMCPImport(ctx, MCPServerInfo{Server: "docs", Transport: MCPTransportStdio})
		span.Finish(MCPServerOutcome{Tools: &MCPToolCounts{Discovered: 7, Kept: 4, Skipped: 1}})

		spans := exp.GetSpans()
		Expect(spans).To(HaveLen(1))
		Expect(spans[0].Name).To(Equal("mcp_import docs"))
		Expect(spans[0].SpanKind).To(Equal(trace.SpanKindClient))
		Expect(spans[0].Status.Code).To(Equal(codes.Unset))

		v, ok := attrOf(spans[0], AttrMCPServer)
		Expect(ok).To(BeTrue())
		Expect(v.AsString()).To(Equal("docs"))

		v, ok = attrOf(spans[0], AttrMCPTransport)
		Expect(ok).To(BeTrue())
		Expect(v.AsString()).To(Equal("stdio"))

		for key, want := range map[attribute.Key]int64{
			AttrMCPToolsDiscovered: 7,
			AttrMCPToolsKept:       4,
			AttrMCPToolsSkipped:    1,
		} {
			v, ok := attrOf(spans[0], key)
			Expect(ok).To(BeTrue(), "expected %s to be set", key)
			Expect(v.AsInt64()).To(Equal(want), "%s", key)
		}
	})

	// The connect is what the entry's startup timeout limits and what happens before
	// the loop, so a server that takes twenty seconds to start has this span and
	// nothing else. A failure names a class from the closed vocabulary and never the
	// error, which quotes the endpoint it dialed.
	It("should record a failed connect through the closed error vocabulary", func() {
		p, exp := recording()

		_, span := p.StartMCPConnect(ctx, MCPServerInfo{Server: "docs", Transport: MCPTransportHTTP})
		span.Finish(MCPServerOutcome{Failed: true, Class: ClassRemoteUnavailable})

		spans := exp.GetSpans()
		Expect(spans).To(HaveLen(1))
		Expect(spans[0].Name).To(Equal("mcp_connect docs"))
		Expect(spans[0].Status.Code).To(Equal(codes.Error))
		Expect(spans[0].Status.Description).To(BeEmpty())

		v, ok := attrOf(spans[0], "error.type")
		Expect(ok).To(BeTrue())
		Expect(v.AsString()).To(Equal(ClassRemoteUnavailable.String()))

		v, ok = attrOf(spans[0], AttrMCPTransport)
		Expect(ok).To(BeTrue())
		Expect(v.AsString()).To(Equal("http"))

		_, ok = attrOf(spans[0], AttrMCPToolsDiscovered)
		Expect(ok).To(BeFalse())
	})

	// Zero discovered says the server advertises nothing, which is a different answer
	// from never having been asked, so a listing that failed reports no counts at all.
	It("should leave the counts off a listing that failed", func() {
		p, exp := recording()

		_, span := p.StartMCPImport(ctx, MCPServerInfo{Server: "docs", Transport: MCPTransportStdio})
		span.Finish(MCPServerOutcome{Failed: true, Class: ClassTimeout, Tools: &MCPToolCounts{}})

		spans := exp.GetSpans()
		Expect(spans).To(HaveLen(1))

		for _, key := range []attribute.Key{AttrMCPToolsDiscovered, AttrMCPToolsKept, AttrMCPToolsSkipped} {
			_, ok := attrOf(spans[0], key)
			Expect(ok).To(BeFalse(), "%s must be absent on a failure", key)
		}

		v, ok := attrOf(spans[0], "error.type")
		Expect(ok).To(BeTrue())
		Expect(v.AsString()).To(Equal(ClassTimeout.String()))
	})

	// The guard is worth what its filler is worth: a poison that quietly filled nothing
	// would let the spec below pass for a type that carried a url. This drives it over
	// the shapes a server's own values arrive as, the nested ones included, since
	// MCPServerOutcome already carries a pointer to a struct of its own and a string
	// added there has to be filled the same way.
	It("should fill every shape a server's own values arrive as", func() {
		var probe struct {
			Server  string
			URL     string
			Args    []string
			Headers map[string]string
			Nested  struct{ Command string }
			Counts  *struct{ Endpoint string }
		}

		poison(&probe, map[string]string{"Server": "docs"})

		Expect(probe.Server).To(Equal("docs"))
		Expect(probe.URL).To(Equal(mcpSecret))
		Expect(probe.Args).To(Equal([]string{mcpSecret}))
		Expect(probe.Headers).To(Equal(map[string]string{mcpSecret: mcpSecret}))
		Expect(probe.Nested.Command).To(Equal(mcpSecret))
		Expect(probe.Counts).ToNot(BeNil())
		Expect(probe.Counts.Endpoint).To(Equal(mcpSecret))
	})

	// A type that reaches itself is filled once. Without the check this spec does not
	// fail, it never returns.
	It("should fill a type that reaches itself once and stop", func() {
		var probe selfReferential

		poison(&probe, nil)

		Expect(probe.URL).To(Equal(mcpSecret))
		Expect(probe.Next).ToNot(BeNil())
		Expect(probe.Next.URL).To(BeEmpty())
		Expect(probe.Next.Next).To(BeNil())
	})

	// The rule this holds is that nothing about a server but its configured name and
	// its transport token reaches a collector. A url carries a credential in its query
	// or its userinfo, and so does an argument, since the bridge an operator reaches
	// for takes the token on the command line.
	//
	// It is written twice over so a future field cannot pass both halves: every value
	// these types carry apart from the two is poisoned by reflection, so a new field is
	// covered without anyone editing this spec, and the attribute keys are held to an
	// allowlist, so a new field exported under a new key fails even if its value came
	// from somewhere this spec did not fill.
	It("should export nothing about a server but its name and its transport", func() {
		p, exp := recording()

		info := MCPServerInfo{Transport: MCPTransportStdio}
		poison(&info, map[string]string{"Server": "docs"})

		_, connect := p.StartMCPConnect(ctx, info)
		failed := MCPServerOutcome{Failed: true, Class: ClassRemoteUnavailable}
		poison(&failed, nil)
		connect.Finish(failed)

		_, imported := p.StartMCPImport(ctx, info)
		ok := MCPServerOutcome{Tools: &MCPToolCounts{Discovered: 2, Kept: 1}}
		poison(&ok, nil)
		imported.Finish(ok)

		allowed := map[attribute.Key]bool{
			AttrMCPServer:          true,
			AttrMCPTransport:       true,
			AttrMCPToolsDiscovered: true,
			AttrMCPToolsKept:       true,
			AttrMCPToolsSkipped:    true,
			"error.type":           true,
		}

		spans := exp.GetSpans()
		Expect(spans).To(HaveLen(2))

		for _, stub := range spans {
			for _, text := range exportedText(stub) {
				Expect(text).ToNot(ContainSubstring(mcpSecret), "%s exports a value it was given", stub.Name)
				Expect(text).ToNot(ContainSubstring("hunter2"), "%s exports a value it was given", stub.Name)
			}

			for _, kv := range stub.Attributes {
				Expect(allowed[kv.Key]).To(BeTrue(), "%s carries the unexpected attribute %s", stub.Name, kv.Key)
			}
		}
	})
})

var _ = Describe("ToolSpan", func() {
	// The server is on a key of its own rather than on fisk.tool.remote_agent, which
	// means the a2a peer: the two kinds are accounted apart, so a backend filtering on
	// the remote agent has to keep answering with a2a calls alone.
	It("should name the mcp server that served the call, apart from the remote agent", func() {
		p, exp := recording()

		ctx, span := p.StartTool(context.Background(), ToolInfo{Name: "docs_search", Identity: "demo", Kind: "mcp"})
		span.Finish(ctx, ToolOutcome{Outcome: ToolOutcomeExecuted, Name: "docs_search", Kind: "mcp", MCPServer: "docs"})

		spans := exp.GetSpans()
		Expect(spans).To(HaveLen(1))

		v, ok := attrOf(spans[0], AttrToolMCPServer)
		Expect(ok).To(BeTrue())
		Expect(v.AsString()).To(Equal("docs"))

		_, ok = attrOf(spans[0], AttrToolRemoteAgent)
		Expect(ok).To(BeFalse())
	})
})
