//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// These specs exercise Options.CustomTools through the exported
// agent.Run API, alongside the living-doc examples in example_external_test.go. They
// assert the registration guards, the no-tools gate, the deferral threshold and the
// resume fingerprint, which are wiring properties rather than usage documentation.
package agent_test

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/choria-io/fisk"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/toolkit/functool"
)

// noopCustomHandler is a handler that does nothing, for tools whose registration is
// rejected before they ever run or whose call is not the point of the spec.
func noopCustomHandler(context.Context, json.RawMessage, *functool.CallContext) (string, error) {
	return "", nil
}

// staticTool is a minimal hand-rolled toolkit.Tool for the registration-guard cases a
// functool.New tool cannot express: an empty Name, or a Name that disagrees with its
// Definition. name is what Name() returns; defName is what Definition() advertises.
type staticTool struct {
	name    string
	defName string
}

func (s staticTool) Name() string                { return s.name }
func (s staticTool) Description() string         { return "a static tool" }
func (s staticTool) InputSchema() map[string]any { return map[string]any{"type": "object"} }
func (s staticTool) Definition(bool) llm.ToolDef { return llm.ToolDef{Name: s.defName} }
func (s staticTool) Execute(context.Context, json.RawMessage, toolkit.ExecDeps) (*toolkit.Outcome, error) {
	return &toolkit.Outcome{}, nil
}
func (s staticTool) ModelDescription() string { return s.Description() }
func (staticTool) MCPExposable() bool         { return false }
func (staticTool) A2AExposable() bool         { return false }

// emptyFiskApp is a fisk application with no commands, so LoadTools yields no
// application tools and a run's only tools are the ones the spec injects.
func emptyFiskApp() *fisk.Application { return fisk.New("app", "an app") }

// fiskAppWithN is a fisk application with n bare commands, for reaching the tool-search
// threshold with a known number of application tools.
func fiskAppWithN(n int) *fisk.Application {
	app := fisk.New("app", "an app")
	for i := 0; i < n; i++ {
		app.Command(fmt.Sprintf("cmd%d", i), "a command")
	}
	return app
}

func anyDeferred(defs []llm.ToolDef) bool {
	for _, d := range defs {
		if d.DeferLoading {
			return true
		}
	}
	return false
}

// plainCustomTool is a custom tool under the given name that does nothing when called.
func plainCustomTool(name string) toolkit.Tool {
	GinkgoHelper()

	tool, err := functool.New(functool.Spec{Name: name, Description: "a custom tool", Schema: map[string]any{"type": "object"}, Handler: noopCustomHandler})
	Expect(err).NotTo(HaveOccurred())

	return tool
}

// remoteSpecTool is a custom tool carrying a RemoteSpec, so it presents as another
// agent's work while running in this process.
func remoteSpecTool() toolkit.Tool {
	GinkgoHelper()

	tool, err := functool.New(functool.Spec{
		Name:        "remote_thing",
		Description: "served by a peer",
		Schema:      map[string]any{"type": "object"},
		Handler:     noopCustomHandler,
		Remote:      &functool.RemoteSpec{Agent: "peer"},
	})
	Expect(err).NotTo(HaveOccurred())

	return tool
}

// remoteKindTool is an in-process tool that declares the remote kind without a
// RemoteSpec, so it presents as self-rendered and a check on the presentation would
// pass it through to be counted and journaled as another agent's call.
func remoteKindTool() toolkit.Tool {
	GinkgoHelper()

	tool, err := functool.New(functool.Spec{
		Name:        "remote_kind_thing",
		Description: "runs here, claims to be a peer's",
		Schema:      map[string]any{"type": "object"},
		Handler:     noopCustomHandler,
		Kind:        toolkit.KindRemote,
	})
	Expect(err).NotTo(HaveOccurred())

	return tool
}

// mcpKindTool is a custom tool carrying an MCPSpec, so it claims work an MCP server
// does.
func mcpKindTool() toolkit.Tool {
	GinkgoHelper()

	tool, err := functool.New(functool.Spec{
		Name:        "mcp_thing",
		Description: "served by an mcp server",
		Schema:      map[string]any{"type": "object"},
		Handler:     noopCustomHandler,
		MCP:         &functool.MCPSpec{Server: "docs"},
	})
	Expect(err).NotTo(HaveOccurred())

	return tool
}

// customToolRejection is one way registering a custom tool is refused.
type customToolRejection struct {
	// withMemory turns on the memory built-ins, for the case that collides with one.
	withMemory bool
	// build constructs the row's custom tools. It is a builder rather than a value
	// because functool.New asserts, and an Entry's arguments are evaluated while the
	// spec tree is built: an assertion that fails there is reported as a panic during
	// tree construction and aborts the whole suite rather than failing one spec.
	build func() []toolkit.Tool
	// wantErr is the substring the refusal must carry.
	wantErr string
}

var _ = Describe("Options.CustomTools", func() {
	// Every way registering a custom tool is refused: a nil entry, an empty or mismatched
	// name, a duplicate within the slice, a collision with an application or built-in
	// tool, and a tool that claims a provider whose work happens outside this process.
	// Each aborts the run rather than silently shadowing or mis-accounting a tool. (A
	// collision with a remote tool takes the same path but needs a broker, so it is
	// covered by an Integration test rather than here.)
	DescribeTable("Should refuse a registration that",
		func(tc customToolRejection) {
			app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
			var cfg *config.Config
			if tc.withMemory {
				cfg = agenttest.Config(GinkgoTB(), app, agenttest.WithMemory())
			} else {
				cfg = agenttest.Config(GinkgoTB(), app)
			}

			opts := agent.Options{
				Config:      cfg,
				ConfigFile:  "agent.yaml",
				Prompt:      []string{"go"},
				Provider:    agenttest.NewScriptedProvider(GinkgoTB()),
				StoreDir:    GinkgoT().TempDir(),
				CustomTools: tc.build(),
			}

			_, err := agent.Run(context.Background(), opts, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
			Expect(err).To(MatchError(ContainSubstring(tc.wantErr)))
		},
		Entry("holds a nil tool", customToolRejection{
			build:   func() []toolkit.Tool { return []toolkit.Tool{nil} },
			wantErr: "custom tool at index 0 is nil",
		}),
		Entry("has an empty name", customToolRejection{
			build:   func() []toolkit.Tool { return []toolkit.Tool{staticTool{}} },
			wantErr: "has an empty name",
		}),
		Entry("has a name mismatch", customToolRejection{
			build:   func() []toolkit.Tool { return []toolkit.Tool{staticTool{name: "left", defName: "right"}} },
			wantErr: "Name() and Definition().Name must match",
		}),
		Entry("duplicates within the slice", customToolRejection{
			build:   func() []toolkit.Tool { return []toolkit.Tool{plainCustomTool("dup"), plainCustomTool("dup")} },
			wantErr: "duplicates an earlier custom tool",
		}),
		Entry("collides with an application tool", customToolRejection{
			build:   func() []toolkit.Tool { return []toolkit.Tool{plainCustomTool("do")} },
			wantErr: "existing application tool",
		}),
		Entry("collides with a built-in tool", customToolRejection{
			withMemory: true,
			build:      func() []toolkit.Tool { return []toolkit.Tool{plainCustomTool("memory_write")} },
			wantErr:    "existing built-in tool",
		}),
		Entry("declares the remote kind through a remote spec", customToolRejection{
			build:   func() []toolkit.Tool { return []toolkit.Tool{remoteSpecTool()} },
			wantErr: "declares the remote kind",
		}),
		Entry("declares the remote kind without a remote spec", customToolRejection{
			build:   func() []toolkit.Tool { return []toolkit.Tool{remoteKindTool()} },
			wantErr: "declares the remote kind",
		}),
		Entry("declares the mcp kind", customToolRejection{
			build:   func() []toolkit.Tool { return []toolkit.Tool{mcpKindTool()} },
			wantErr: "declares the mcp kind",
		}),
	)

	// The no-tools availability gate counts injected tools: a run wrapping an application
	// with no commands and no built-in or remote tools, but with one custom tool, starts
	// and completes rather than aborting as having no tools. The custom tool is
	// advertised to the model.
	It("Should start a run whose only tool is a custom one", func() {
		tool, err := functool.New(functool.Spec{Name: "only_tool", Description: "the sole tool", Schema: map[string]any{"type": "object"}, Handler: noopCustomHandler})
		Expect(err).NotTo(HaveOccurred())

		app := agenttest.NewFakeApp(GinkgoTB(), emptyFiskApp())
		provider := agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done"))

		opts := agent.Options{
			Config:      agenttest.Config(GinkgoTB(), app),
			ConfigFile:  "agent.yaml",
			Prompt:      []string{"go"},
			Provider:    provider,
			CustomTools: []toolkit.Tool{tool},
		}

		res, err := agent.Run(context.Background(), opts, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

		advertised := false
		for _, td := range provider.Requests()[0].Tools {
			if td.Name == "only_tool" {
				advertised = true
			}
		}
		Expect(advertised).To(BeTrue())
	})

	// This pins the shared deferral decision: nine application tools sit just under the
	// tool-search threshold, so nothing defers; adding one custom tool reaches the
	// threshold and the whole set, the custom tool included, is offered through tool
	// search. It guards against a custom tool being excluded from the count that decides
	// deferral.
	It("Should count a custom tool toward the deferral threshold", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), fiskAppWithN(9))

		provA := agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done"))
		_, err := agent.Run(context.Background(), agent.Options{
			Config:     agenttest.Config(GinkgoTB(), app),
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			Provider:   provA,
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(anyDeferred(provA.Requests()[0].Tools)).To(BeFalse())

		tool, err := functool.New(functool.Spec{Name: "tenth_tool", Description: "the tool that tips the set over", Schema: map[string]any{"type": "object"}, Handler: noopCustomHandler})
		Expect(err).NotTo(HaveOccurred())

		provB := agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done"))
		_, err = agent.Run(context.Background(), agent.Options{
			Config:      agenttest.Config(GinkgoTB(), app),
			ConfigFile:  "agent.yaml",
			Prompt:      []string{"go"},
			Provider:    provB,
			CustomTools: []toolkit.Tool{tool},
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())

		Expect(anyDeferred(provB.Requests()[0].Tools)).To(BeTrue())
		for _, td := range provB.Requests()[0].Tools {
			if td.Name == "tenth_tool" {
				Expect(td.DeferLoading).To(BeTrue())
			}
		}
	})

	// A custom tool is part of the run fingerprint: a checkpointed run started with one
	// custom tool notices when a different one is injected. It continues and warns rather
	// than refusing, since what a changed tool set endangers is a standing approval rather
	// than the stored conversation. This is the contract the Options.CustomTools doc rests
	// on (a custom tool's Definition must be deterministic across restarts).
	It("Should warn on a resume whose custom tool set changed", func() {
		ctx := context.Background()

		storeDir := GinkgoT().TempDir()
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())

		toolA, err := functool.New(functool.Spec{Name: "alpha", Description: "the first tool", Schema: map[string]any{"type": "object"}, Handler: noopCustomHandler})
		Expect(err).NotTo(HaveOccurred())
		toolB, err := functool.New(functool.Spec{Name: "beta", Description: "a different tool", Schema: map[string]any{"type": "object"}, Handler: noopCustomHandler})
		Expect(err).NotTo(HaveOccurred())

		// Run 1: one application tool call, then suspend at the next boundary, with toolA
		// injected.
		suspendPolls := 0
		opts1 := agent.Options{
			Config:           agenttest.Config(GinkgoTB(), app),
			ConfigFile:       "agent.yaml",
			Prompt:           []string{"start"},
			Provider:         agenttest.NewScriptedProvider(GinkgoTB(), agenttest.ToolUseResponse("c1", "do", json.RawMessage(`{"subject":"x"}`))),
			StoreDir:         storeDir,
			Checkpoint:       agent.Checkpoint{Enabled: true},
			SuspendRequested: func() bool { suspendPolls++; return suspendPolls > 1 },
			CustomTools:      []toolkit.Tool{toolA},
		}
		res1, err := agent.Run(ctx, opts1, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res1.Reason).To(Equal(runstate.ReasonSuspended))

		// Run 2: resume the saved session with a different custom tool set. The resume
		// continues, because a provider reads a stored conversation whether or not it still
		// holds every tool the history names, and says so rather than refusing.
		events2 := agenttest.NewRecordingEvents()
		opts2 := agent.Options{
			Config:      agenttest.Config(GinkgoTB(), app),
			ConfigFile:  "agent.yaml",
			Provider:    agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("finished")),
			StoreDir:    storeDir,
			Checkpoint:  agent.Checkpoint{ResumeID: res1.SessionID},
			CustomTools: []toolkit.Tool{toolB},
		}
		res2, err := agent.Run(ctx, opts2, events2, agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res2.Reason).To(Equal(runstate.ReasonCompleted))

		var drift []agent.Warning
		for _, w := range events2.Warnings() {
			if w.Kind == agent.WarnToolSetDrift {
				drift = append(drift, w)
			}
		}
		Expect(drift).To(HaveLen(1))
		Expect(drift[0].Params).To(ContainElement("tool set: changed"))
	})
})
