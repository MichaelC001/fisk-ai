//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/choria-io/fisk"
	"github.com/choria-io/fisk-ai/internal/telemetry"
	tools2 "github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/toolkit/fisktool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
)

func TestMCPServer(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Internal/MCPServer")
}

// introspect drives an application's real --fisk-introspect handler in-process
// and returns the parsed model, whose per-command schemas are populated.
func introspect(app *fisk.Application) *fisk.ApplicationModel {
	GinkgoHelper()

	app.Terminate(func(int) {})

	r, w, err := os.Pipe()
	Expect(err).NotTo(HaveOccurred())

	stdout := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = stdout }()

	captured := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(r)
		captured <- data
	}()

	_, err = app.Parse([]string{"--fisk-introspect"})
	Expect(err).NotTo(HaveOccurred())
	Expect(w.Close()).To(Succeed())

	var model fisk.ApplicationModel
	Expect(json.Unmarshal(<-captured, &model)).To(Succeed())

	return &model
}

// toolsFor introspects app and returns its tools.
func toolsFor(app *fisk.Application) []tools2.Tool {
	GinkgoHelper()

	return tools2.Tools(cmdToolsFor(app))
}

// cmdToolsFor builds the concrete command tools, for specs that filter or inspect
// them rather than run them.
func cmdToolsFor(app *fisk.Application) []*fisktool.CommandTool {
	GinkgoHelper()

	tools, err := fisktool.ApplicationTools(introspect(app))
	Expect(err).NotTo(HaveOccurred())

	return tools
}

// runnableTools returns the command tools of app backed by a runnable binary.
// ToolsForApp is the only route to a tool that carries a binary path, so the script
// it introspects answers --fisk-introspect with the application's real model and
// dispatches every other invocation on the command name. bodies maps a command name
// to the shell fragment that runs for it, without a shebang.
func runnableTools(app *fisk.Application, bodies map[string]string) []*fisktool.CommandTool {
	GinkgoHelper()

	model, err := json.Marshal(introspect(app))
	Expect(err).NotTo(HaveOccurred())

	dir := GinkgoT().TempDir()
	modelPath := filepath.Join(dir, "introspect.json")
	Expect(os.WriteFile(modelPath, model, 0o600)).To(Succeed())

	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = \"--fisk-introspect\" ]; then\n  cat %q\n  exit 0\nfi\ncase \"$1\" in\n", modelPath)
	for name, body := range bodies {
		script += name + ")\n" + body + ";;\n"
	}
	script += "esac\n"

	path := filepath.Join(dir, "app")
	Expect(os.WriteFile(path, []byte(script), 0o700)).To(Succeed())

	tools, err := fisktool.ToolsForApp(context.Background(), path, nil)
	Expect(err).NotTo(HaveOccurred())

	return tools
}

// connect wires an in-memory MCP client to the server and returns the session.
func connect(ctx context.Context, srv *mcp.Server) *mcp.ClientSession {
	GinkgoHelper()

	serverT, clientT := mcp.NewInMemoryTransports()
	_, err := srv.Connect(ctx, serverT, nil)
	Expect(err).NotTo(HaveOccurred())

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	Expect(err).NotTo(HaveOccurred())

	return cs
}

// callText runs a tool and returns its first text content plus the error flag.
func callText(ctx context.Context, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	GinkgoHelper()

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	Expect(err).NotTo(HaveOccurred())
	Expect(res.Content).NotTo(BeEmpty())

	text, ok := res.Content[0].(*mcp.TextContent)
	Expect(ok).To(BeTrue())

	return text.Text, res.IsError
}

// connectElicit connects a client that answers elicitation with handler, so the
// server sees a client that negotiated the elicitation capability (the SDK
// advertises it automatically when a handler is set).
//
// It goes over a real streamable HTTP transport rather than the in-memory pair the
// other helpers use, because elicitation is protocol-version dependent and only this
// transport negotiates the version Serve actually runs.
//
// The in-memory transport advertises every version the SDK knows, and the SDK client
// always initializes at the newest with no exported way to pin it, so an in-memory
// session lands on 2026-07-28. That version forbids a server from sending
// elicitation/create while serving a request (SEP-2322 replaces it with a multi
// round-trip InputRequests flow), so the confirm gate cannot be exercised there at all.
//
// The HTTP transport serves 2026-07-28 only when stateless, and Serve configures it
// stateful, so a client discovers the server supports 2025-11-25 or older and
// negotiates down. That is what production does and what these specs need.
func connectElicit(ctx context.Context, srv *mcp.Server, handler func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error)) *mcp.ClientSession {
	GinkgoHelper()

	// Nil options, matching Serve: stateful is what keeps the negotiated version on the
	// side of the protocol where the confirm gate works.
	httpSrv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	DeferCleanup(httpSrv.Close)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, &mcp.ClientOptions{ElicitationHandler: handler})
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: httpSrv.URL}, nil)
	Expect(err).NotTo(HaveOccurred())

	return cs
}

// taggedExecutable builds one command carrying tag, backed by an executable that
// prints marker, ready to serve.
func taggedExecutable(name, tag, marker string) []tools2.Tool {
	GinkgoHelper()

	app := fisk.New("app", "an app")
	app.Command(name, "a command").Tag(tag)

	tools := runnableTools(app, map[string]string{name: "echo " + marker + "\n"})
	Expect(tools).To(HaveLen(1))

	return tools2.Tools(tools)
}

// safeBuffer is an io.Writer safe for the concurrent writes a running server makes
// from its handler and initialized-notification goroutines.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

var _ = Describe("BuildServer", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("Should expose each tool with its raw, unannotated schema", func() {
		app := fisk.New("app", "an app")
		deploy := app.Command("deploy", "deploy things")
		deploy.Arg("target", "where to deploy").Required().String()
		deploy.Flag("force", "force the deploy").Bool()

		srv, registered := BuildServer(toolsFor(app), Options{Name: "app", Version: "v1", LogOutput: io.Discard})
		Expect(registered).To(ConsistOf("deploy"))

		cs := connect(ctx, srv)
		defer cs.Close()

		res, err := cs.ListTools(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Tools).To(HaveLen(1))

		tool := res.Tools[0]
		Expect(tool.Name).To(Equal("deploy"))
		Expect(tool.Description).To(Equal("deploy things"))

		schema, ok := tool.InputSchema.(map[string]any)
		Expect(ok).To(BeTrue())
		Expect(schema["required"]).To(ConsistOf("target"))

		props, ok := schema["properties"].(map[string]any)
		Expect(ok).To(BeTrue())

		// The optional flag keeps its description verbatim: unlike the Anthropic
		// path, no "(optional)" suffix is added for MCP clients.
		force, ok := props["force"].(map[string]any)
		Expect(ok).To(BeTrue())
		Expect(force["description"]).To(Equal("force the deploy"))
	})

	It("Should send configured instructions to the connecting client", func() {
		app := fisk.New("app", "an app")
		app.Command("deploy", "deploy things")

		srv, _ := BuildServer(toolsFor(app), Options{Name: "app", Version: "v1", Instructions: "use deploy carefully", LogOutput: io.Discard})

		cs := connect(ctx, srv)
		defer cs.Close()

		Expect(cs.InitializeResult().Instructions).To(Equal("use deploy carefully"))
	})

	It("Should send no instructions when none are configured", func() {
		app := fisk.New("app", "an app")
		app.Command("deploy", "deploy things")

		srv, _ := BuildServer(toolsFor(app), Options{Name: "app", Version: "v1", LogOutput: io.Discard})

		cs := connect(ctx, srv)
		defer cs.Close()

		Expect(cs.InitializeResult().Instructions).To(BeEmpty())
	})

	It("Should append the command's tags to the tool description for the connecting model", func() {
		app := fisk.New("app", "an app")
		app.Command("deploy", "deploy things").Tag("impact:rw")

		srv, _ := BuildServer(toolsFor(app), Options{Name: "app", Version: "v1", LogOutput: io.Discard})

		cs := connect(ctx, srv)
		defer cs.Close()

		res, err := cs.ListTools(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Tools).To(HaveLen(1))
		Expect(res.Tools[0].Description).To(Equal("deploy things\n\nTags: impact:rw"))
	})

	It("Should give a tool a space-joined command path as its annotation title", func() {
		app := fisk.New("app", "an app")
		stream := app.Command("stream", "stream commands")
		stream.Command("info", "show stream info")

		srv, _ := BuildServer(toolsFor(app), Options{Name: "app", Version: "v1", LogOutput: io.Discard})

		cs := connect(ctx, srv)
		defer cs.Close()

		res, err := cs.ListTools(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Tools).To(HaveLen(1))
		Expect(res.Tools[0].Name).To(Equal("stream_info"))
		Expect(res.Tools[0].Annotations).NotTo(BeNil())
		Expect(res.Tools[0].Annotations.Title).To(Equal("stream info"))
	})

	It("Should advertise the behavioral hints a command's tags declare", func() {
		app := fisk.New("app", "an app")
		app.Command("stream_ls", "list streams").Tag("ai:read_only").Tag("ai:idempotent")
		app.Command("stream_rm", "remove a stream").Tag("ai:destructive")

		srv, _ := BuildServer(toolsFor(app), Options{Name: "app", Version: "v1", LogOutput: io.Discard})

		cs := connect(ctx, srv)
		defer cs.Close()

		res, err := cs.ListTools(ctx, nil)
		Expect(err).NotTo(HaveOccurred())

		byName := map[string]*mcp.Tool{}
		for _, t := range res.Tools {
			byName[t.Name] = t
		}

		readOnly := byName["stream_ls"].Annotations
		Expect(readOnly.ReadOnlyHint).To(BeTrue())
		Expect(readOnly.IdempotentHint).To(BeTrue())
		Expect(readOnly.DestructiveHint).To(BeNil())

		destructive := byName["stream_rm"].Annotations
		Expect(destructive.ReadOnlyHint).To(BeFalse())
		Expect(destructive.DestructiveHint).NotTo(BeNil())
		Expect(*destructive.DestructiveHint).To(BeTrue())
	})

	It("Should leave an undeclared hint absent so the client applies the spec default", func() {
		app := fisk.New("app", "an app")
		app.Command("plain", "says nothing about itself")

		srv, _ := BuildServer(toolsFor(app), Options{Name: "app", Version: "v1", LogOutput: io.Discard})

		cs := connect(ctx, srv)
		defer cs.Close()

		res, err := cs.ListTools(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Tools).To(HaveLen(1))

		annotations := res.Tools[0].Annotations
		Expect(annotations.DestructiveHint).To(BeNil())
		Expect(annotations.OpenWorldHint).To(BeNil())
		Expect(annotations.ReadOnlyHint).To(BeFalse())
		Expect(annotations.IdempotentHint).To(BeFalse())
	})

	It("Should still advertise read-only for a confirm-gated command, which answers a different question", func() {
		app := fisk.New("app", "an app")
		app.Command("expensive_report", "a slow read").Tag("ai:read_only").Tag("ai:confirm")

		srv, _ := BuildServer(toolsFor(app), Options{Name: "app", Version: "v1", LogOutput: io.Discard})

		cs := connect(ctx, srv)
		defer cs.Close()

		res, err := cs.ListTools(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Tools).To(HaveLen(1))
		Expect(res.Tools[0].Annotations.ReadOnlyHint).To(BeTrue())
	})

	It("Should serve a tool with contradictory behavior tags, advertising the more dangerous reading", func() {
		app := fisk.New("app", "an app")
		app.Command("confused", "claims both").Tag("ai:read_only").Tag("ai:destructive")

		log := &bytes.Buffer{}
		srv, registered := BuildServer(toolsFor(app), Options{Name: "app", Version: "v1", LogOutput: log})
		Expect(registered).To(ConsistOf("confused"))
		Expect(log.String()).To(ContainSubstring("contradictory behavior tags"))

		cs := connect(ctx, srv)
		defer cs.Close()

		res, err := cs.ListTools(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Tools[0].Annotations.ReadOnlyHint).To(BeFalse())
		Expect(*res.Tools[0].Annotations.DestructiveHint).To(BeTrue())
	})

	It("Should warn about a tag that only looks reserved, and serve the tool anyway", func() {
		app := fisk.New("app", "an app")
		app.Command("typo", "misspelled its tag").Tag("ai:readonly")

		log := &bytes.Buffer{}
		_, registered := BuildServer(toolsFor(app), Options{Name: "app", Version: "v1", LogOutput: log})
		Expect(registered).To(ConsistOf("typo"))
		Expect(log.String()).To(ContainSubstring(`unknown reserved tag(s) ai:readonly`))
	})

	It("Should not expose tools removed by the ai:deny filter", func() {
		app := fisk.New("app", "an app")
		app.Command("keep", "kept tool")
		app.Command("secret", "denied tool").Tag("ai:deny")

		cmdTools := cmdToolsFor(app)

		// The deny strip is the same first FilterTools pass the agent uses.
		filtered, err := fisktool.FilterTools(cmdTools, nil, fisktool.IncludeFilter)
		Expect(err).NotTo(HaveOccurred())

		srv, registered := BuildServer(tools2.Tools(filtered), Options{Name: "app", Version: "v1", LogOutput: io.Discard})
		Expect(registered).To(ConsistOf("keep"))

		cs := connect(ctx, srv)
		defer cs.Close()

		res, err := cs.ListTools(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Tools).To(HaveLen(1))
		Expect(res.Tools[0].Name).To(Equal("keep"))
	})

	It("Should expose confirm-tagged tools, leaving the gate to the calling client", func() {
		app := fisk.New("app", "an app")
		app.Command("keep", "kept tool")
		app.Command("gated", "needs confirmation").Tag("ai:confirm")
		app.Command("risky", "mutates state").Tag("impact:rw")

		_, registered := BuildServer(toolsFor(app), Options{Name: "app", Version: "v1", LogOutput: io.Discard})
		Expect(registered).To(ConsistOf("keep", "gated", "risky"))
	})

	It("Should skip tools whose names are not valid MCP tool names", func() {
		valid := &fisktool.CommandTool{Path: []string{"ok"}, Model: &fisk.CmdModel{RestrictedSchema: map[string]any{"type": "object"}}}
		invalid := &fisktool.CommandTool{Path: []string{"bad.name"}, Model: &fisk.CmdModel{RestrictedSchema: map[string]any{"type": "object"}}}

		_, registered := BuildServer([]tools2.Tool{valid, invalid}, Options{Name: "app", Version: "v1", LogOutput: io.Discard})
		Expect(registered).To(ConsistOf("ok"))
	})
})

var _ = Describe("tool calls", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	// appWithExecutables builds an app whose named commands are each bound to a
	// stand-in executable body, returning tools ready to call.
	appWithExecutables := func(bodies map[string]string) []tools2.Tool {
		GinkgoHelper()

		app := fisk.New("app", "an app")
		for name := range bodies {
			app.Command(name, "a command")
		}

		return tools2.Tools(runnableTools(app, bodies))
	}

	It("Should return command output as a successful result", func() {
		tools := appWithExecutables(map[string]string{"ping": "echo pong\n"})
		srv, _ := BuildServer(tools, Options{Name: "app", Version: "v1", LogOutput: io.Discard})

		cs := connect(ctx, srv)
		defer cs.Close()

		text, isError := callText(ctx, cs, "ping", nil)
		Expect(isError).To(BeFalse())

		var result tools2.CommandResult
		Expect(json.Unmarshal([]byte(text), &result)).To(Succeed())
		Expect(result.ExitCode).To(Equal(0))
		Expect(result.Output).To(Equal("pong\n"))
	})

	It("Should deliver a non-zero exit as a successful result, not an error", func() {
		tools := appWithExecutables(map[string]string{"fail": "exit 4\n"})
		srv, _ := BuildServer(tools, Options{Name: "app", Version: "v1", LogOutput: io.Discard})

		cs := connect(ctx, srv)
		defer cs.Close()

		text, isError := callText(ctx, cs, "fail", nil)
		Expect(isError).To(BeFalse())

		var result tools2.CommandResult
		Expect(json.Unmarshal([]byte(text), &result)).To(Succeed())
		Expect(result.ExitCode).To(Equal(4))
	})

	It("Should report an execution failure as an error result", func() {
		app := fisk.New("app", "an app")
		app.Command("broken", "a command")
		tools := runnableTools(app, map[string]string{"broken": "echo ok\n"})

		// The binary is introspected at load and removed afterwards, so the call
		// fails to start the command rather than failing inside it.
		Expect(os.Remove(tools[0].AppPath())).To(Succeed())

		srv, _ := BuildServer(tools2.Tools(tools), Options{Name: "app", Version: "v1", LogOutput: io.Discard})

		cs := connect(ctx, srv)
		defer cs.Close()

		_, isError := callText(ctx, cs, "broken", nil)
		Expect(isError).To(BeTrue())
	})

	It("Should fail a call that exceeds the per-call timeout", func() {
		tools := appWithExecutables(map[string]string{"slow": "sleep 2\n"})
		srv, _ := BuildServer(tools, Options{Name: "app", Version: "v1", CallTimeout: 100 * time.Millisecond, LogOutput: io.Discard})

		cs := connect(ctx, srv)
		defer cs.Close()

		_, isError := callText(ctx, cs, "slow", nil)
		Expect(isError).To(BeTrue())
	})

	It("Should log the command line being run without its output", func() {
		tools := appWithExecutables(map[string]string{"ping": "echo pong\n"})

		var logbuf bytes.Buffer
		srv, _ := BuildServer(tools, Options{Name: "app", Version: "v1", LogOutput: &logbuf})

		cs := connect(ctx, srv)
		defer cs.Close()

		_, isError := callText(ctx, cs, "ping", nil)
		Expect(isError).To(BeFalse())

		// The log names the command that ran, but not its output.
		Expect(logbuf.String()).To(ContainSubstring("Running ping"))
		Expect(logbuf.String()).NotTo(ContainSubstring("pong"))
	})

	It("Should serialize calls beyond the concurrency limit", func() {
		tools := appWithExecutables(map[string]string{"work": "sleep 0.4\n"})
		srv, _ := BuildServer(tools, Options{Name: "app", Version: "v1", Concurrency: 1, CallTimeout: 5 * time.Second, LogOutput: io.Discard})

		cs := connect(ctx, srv)
		defer cs.Close()

		start := time.Now()
		done := make(chan struct{}, 2)
		for i := 0; i < 2; i++ {
			go func() {
				defer GinkgoRecover()
				_, isError := callText(ctx, cs, "work", nil)
				Expect(isError).To(BeFalse())
				done <- struct{}{}
			}()
		}
		<-done
		<-done

		// With concurrency 1, two 0.4s calls cannot overlap, so the total must
		// exceed a single call's duration by a clear margin. Only a lower bound is
		// asserted, so a slow machine cannot make this flaky.
		Expect(time.Since(start)).To(BeNumerically(">", 700*time.Millisecond))
	})
})

var _ = Describe("Serve", func() {
	It("Should shut down promptly when a client holds the SSE stream open", func() {
		// A streamable HTTP client opens a standalone, long-lived SSE GET stream
		// after initialization. http.Server.Shutdown waits for in-flight requests
		// to go idle, which that stream never does, so unless the serving code
		// cancels request contexts a single interrupt blocks for the full
		// shutdownTimeout. This guards that the shutdown returns well within it.
		app := fisk.New("app", "an app")
		app.Command("ping", "a command")
		tools := runnableTools(app, map[string]string{"ping": "echo pong\n"})

		srv, registered := BuildServer(tools2.Tools(tools), Options{Name: "app", Version: "v1", LogOutput: io.Discard})
		Expect(registered).To(ConsistOf("ping"))

		ln, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())

		serveCtx, cancelServe := context.WithCancel(context.Background())
		defer cancelServe()

		errCh := make(chan error, 1)
		go func() {
			defer GinkgoRecover()
			errCh <- serveListener(serveCtx, ln, srv, registered, Options{Name: "app", Version: "v1", LogOutput: io.Discard})
		}()

		clientCtx, cancelClient := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelClient()

		transport := &mcp.StreamableClientTransport{Endpoint: "http://" + ln.Addr().String()}
		client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
		cs, err := client.Connect(clientCtx, transport, nil)
		Expect(err).NotTo(HaveOccurred())
		defer cs.Close()

		// Confirm the session is live (and thus the standalone SSE stream is open)
		// before triggering shutdown, so the prompt-return assertion is meaningful.
		_, err = cs.ListTools(clientCtx, nil)
		Expect(err).NotTo(HaveOccurred())

		cancelServe()

		// With the fix the hanging stream is unblocked and Shutdown drains at once,
		// returning nil; without it the call blocks for the whole shutdownTimeout,
		// so a window comfortably under that timeout is the regression assertion.
		Eventually(errCh, shutdownTimeout-time.Second).Should(Receive(BeNil()))
	})
})

var _ = Describe("config mode", func() {
	It("Should validate an MCP config that has no prompt or model", func() {
		cfg, err := config.ParseConfigForMode([]byte("application_path: /bin/echo\n"), config.ModeMCP)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.ApplicationPath).To(Equal("/bin/echo"))
		// Identity defaults to the binary basename, used as the server name.
		Expect(cfg.Identity).To(Equal("echo"))
	})

	It("Should still require a prompt and model in agent mode", func() {
		_, err := config.ParseConfigForMode([]byte("application_path: /bin/echo\n"), config.ModeAgent)
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("claudeAddHint", func() {
	It("Should suggest an http transport command on localhost for an unspecified bind", func() {
		hint := claudeAddHint("myagent", &net.TCPAddr{Port: 8080})
		Expect(hint).To(Equal("claude mcp add --transport http myagent http://localhost:8080"))
	})

	It("Should rewrite an unspecified IPv6 bind host to localhost", func() {
		hint := claudeAddHint("myagent", &net.TCPAddr{IP: net.IPv6unspecified, Port: 8080})
		Expect(hint).To(Equal("claude mcp add --transport http myagent http://localhost:8080"))
	})

	It("Should preserve a concrete bind host", func() {
		hint := claudeAddHint("myagent", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 9000})
		Expect(hint).To(Equal("claude mcp add --transport http myagent http://127.0.0.1:9000"))
	})

	It("Should omit the hint when the identity is empty", func() {
		hint := claudeAddHint("", &net.TCPAddr{Port: 8080})
		Expect(hint).To(BeEmpty())
	})
})

var _ = Describe("Confirm gating over MCP", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	approve := func(_ context.Context, _ *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"approve": true}}, nil
	}

	// denies asserts that an ai:confirm command handled by the given elicitation
	// handler is refused and never runs: the result is an error carrying wantText
	// and not the command's output marker.
	denies := func(handler func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error), wantText string) {
		GinkgoHelper()

		tools := taggedExecutable("deploy", "ai:confirm", "deployed")
		srv, _ := BuildServer(tools, Options{Name: "app", Version: "v1", LogOutput: io.Discard})

		cs := connectElicit(ctx, srv, handler)
		defer cs.Close()

		text, isError := callText(ctx, cs, "deploy", nil)
		Expect(isError).To(BeTrue())
		Expect(text).To(ContainSubstring(wantText))
		Expect(text).NotTo(ContainSubstring("deployed"))
	}

	It("Should run an ai:confirm tool when the client approves", func() {
		tools := taggedExecutable("deploy", "ai:confirm", "deployed")
		srv, _ := BuildServer(tools, Options{Name: "app", Version: "v1", LogOutput: io.Discard})

		cs := connectElicit(ctx, srv, approve)
		defer cs.Close()

		text, isError := callText(ctx, cs, "deploy", nil)
		Expect(isError).To(BeFalse())

		var result tools2.CommandResult
		Expect(json.Unmarshal([]byte(text), &result)).To(Succeed())
		Expect(result.Output).To(Equal("deployed\n"))
	})

	It("Should deny when the user declines", func() {
		denies(func(_ context.Context, _ *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			return &mcp.ElicitResult{Action: "decline"}, nil
		}, "declined")
	})

	It("Should deny when the user dismisses the prompt", func() {
		denies(func(_ context.Context, _ *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			return &mcp.ElicitResult{Action: "cancel"}, nil
		}, "dismissed")
	})

	It("Should deny when the user accepts without approving", func() {
		denies(func(_ context.Context, _ *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"approve": false}}, nil
		}, "chose not to run")
	})

	It("Should deny when the answer violates the boolean schema", func() {
		// The SDK validates the client's answer against the requested schema, so a
		// non-boolean approve is rejected before it reaches the handler; it surfaces
		// as an elicitation error and denies. The handler's own checked type
		// assertion remains as defense in depth should a client ever bypass this.
		denies(func(_ context.Context, _ *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"approve": "yes"}}, nil
		}, "failed")
	})

	It("Should deny when the elicitation itself errors", func() {
		denies(func(_ context.Context, _ *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			return nil, io.EOF
		}, "failed")
	})

	It("Should run an ai:confirm tool ungated when the client cannot elicit", func() {
		tools := taggedExecutable("deploy", "ai:confirm", "deployed")
		logs := &safeBuffer{}
		srv, _ := BuildServer(tools, Options{Name: "app", Version: "v1", LogOutput: logs})

		cs := connect(ctx, srv)
		defer cs.Close()

		text, isError := callText(ctx, cs, "deploy", nil)
		Expect(isError).To(BeFalse())

		var result tools2.CommandResult
		Expect(json.Unmarshal([]byte(text), &result)).To(Succeed())
		Expect(result.Output).To(Equal("deployed\n"))
		Expect(logs.String()).To(ContainSubstring("ungated"))
	})

	It("Should not elicit for a tool that carries no confirm tag", func() {
		tools := taggedExecutable("list", "impact:ro", "listed")
		srv, _ := BuildServer(tools, Options{Name: "app", Version: "v1", LogOutput: io.Discard})

		// A decline handler would deny the call if it were gated; a successful run
		// with output proves the tool was never gated.
		cs := connectElicit(ctx, srv, func(_ context.Context, _ *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			return &mcp.ElicitResult{Action: "decline"}, nil
		})
		defer cs.Close()

		text, isError := callText(ctx, cs, "list", nil)
		Expect(isError).To(BeFalse())

		var result tools2.CommandResult
		Expect(json.Unmarshal([]byte(text), &result)).To(Succeed())
		Expect(result.Output).To(Equal("listed\n"))
	})

	It("Should gate a configured confirm tag the same as ai:confirm", func() {
		tools := taggedExecutable("write", "impact:rw", "wrote")
		srv, _ := BuildServer(tools, Options{Name: "app", Version: "v1", ConfirmTags: []string{"impact:rw"}, LogOutput: io.Discard})

		cs := connectElicit(ctx, srv, func(_ context.Context, _ *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			return &mcp.ElicitResult{Action: "decline"}, nil
		})
		defer cs.Close()

		text, isError := callText(ctx, cs, "write", nil)
		Expect(isError).To(BeTrue())
		Expect(text).NotTo(ContainSubstring("wrote"))
	})

	It("Should not gate a tag that is not configured as a confirm tag", func() {
		tools := taggedExecutable("write", "impact:rw", "wrote")
		srv, _ := BuildServer(tools, Options{Name: "app", Version: "v1", LogOutput: io.Discard})

		cs := connectElicit(ctx, srv, func(_ context.Context, _ *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			return &mcp.ElicitResult{Action: "decline"}, nil
		})
		defer cs.Close()

		text, isError := callText(ctx, cs, "write", nil)
		Expect(isError).To(BeFalse())

		var result tools2.CommandResult
		Expect(json.Unmarshal([]byte(text), &result)).To(Succeed())
		Expect(result.Output).To(Equal("wrote\n"))
	})

	It("Should refuse an ai:confirm tool in always mode when the client cannot elicit", func() {
		tools := taggedExecutable("deploy", "ai:confirm", "deployed")
		srv, _ := BuildServer(tools, Options{Name: "app", Version: "v1", ConfirmMode: ConfirmAlways, LogOutput: io.Discard})

		cs := connect(ctx, srv)
		defer cs.Close()

		text, isError := callText(ctx, cs, "deploy", nil)
		Expect(isError).To(BeTrue())
		Expect(text).To(ContainSubstring("requires approval"))
		Expect(text).NotTo(ContainSubstring("deployed"))
	})

	It("Should run an ai:confirm tool in always mode when the client approves", func() {
		tools := taggedExecutable("deploy", "ai:confirm", "deployed")
		srv, _ := BuildServer(tools, Options{Name: "app", Version: "v1", ConfirmMode: ConfirmAlways, LogOutput: io.Discard})

		cs := connectElicit(ctx, srv, approve)
		defer cs.Close()

		text, isError := callText(ctx, cs, "deploy", nil)
		Expect(isError).To(BeFalse())

		var result tools2.CommandResult
		Expect(json.Unmarshal([]byte(text), &result)).To(Succeed())
		Expect(result.Output).To(Equal("deployed\n"))
	})

	It("Should run an ai:confirm tool ungated in never mode without eliciting", func() {
		tools := taggedExecutable("deploy", "ai:confirm", "deployed")
		srv, _ := BuildServer(tools, Options{Name: "app", Version: "v1", ConfirmMode: ConfirmNever, LogOutput: io.Discard})

		// A decline handler would deny the call if it were gated; a successful run on
		// an elicitation-capable client proves never mode skips the gate entirely.
		cs := connectElicit(ctx, srv, func(_ context.Context, _ *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			return &mcp.ElicitResult{Action: "decline"}, nil
		})
		defer cs.Close()

		text, isError := callText(ctx, cs, "deploy", nil)
		Expect(isError).To(BeFalse())

		var result tools2.CommandResult
		Expect(json.Unmarshal([]byte(text), &result)).To(Succeed())
		Expect(result.Output).To(Equal("deployed\n"))
	})
})

// The base context every served tool call inherits. Both halves fail silently in a
// different direction: no values and a knowledge search served over MCP exports nothing
// while looking wired, inherited cancellation and an interrupt kills in-flight calls
// instead of letting them drain.
var _ = Describe("serveBaseContext", func() {
	It("should carry the caller's values so a served tool reaches the telemetry provider", func() {
		p := telemetry.NewFromProviders(nil, nil)
		ctx := telemetry.ContextWithProvider(context.Background(), p)

		base, cancel := serveBaseContext(ctx)
		defer cancel()

		Expect(telemetry.ProviderFromContext(base)).To(BeIdenticalTo(p))
	})

	It("should not inherit the caller's cancellation", func() {
		ctx, cancelCaller := context.WithCancel(context.Background())

		base, cancel := serveBaseContext(ctx)
		defer cancel()

		cancelCaller()

		Expect(ctx.Done()).To(BeClosed())
		Expect(base.Err()).ToNot(HaveOccurred())
	})

	It("should be cancelable on its own, which is what unblocks a held-open stream", func() {
		base, cancel := serveBaseContext(context.Background())

		cancel()

		Expect(base.Done()).To(BeClosed())
	})
})
