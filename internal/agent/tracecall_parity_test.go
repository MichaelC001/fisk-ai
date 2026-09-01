//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/toolkit/builtin"
	"github.com/choria-io/fisk-ai/internal/toolkit/fisktool"
	"github.com/choria-io/fisk-ai/internal/toolkit/functool"
)

// describelessTool is a model-facing Tool that does not implement toolkit.Describer,
// so traceCall must fall back to its safe default: run it, trace it by name alone,
// with no dependencies and under the unknown kind.
type describelessTool struct{}

func (describelessTool) Name() string                { return "mystery" }
func (describelessTool) Description() string         { return "a tool of unforeseen kind" }
func (describelessTool) InputSchema() map[string]any { return map[string]any{"type": "object"} }
func (describelessTool) Definition(bool) llm.ToolDef { return llm.ToolDef{Name: "mystery"} }
func (describelessTool) Execute(context.Context, json.RawMessage, toolkit.ExecDeps) (*toolkit.Outcome, error) {
	return &toolkit.Outcome{Output: "ok"}, nil
}
func (d describelessTool) ModelDescription() string { return d.Description() }
func (describelessTool) MCPExposable() bool         { return false }
func (describelessTool) A2AExposable() bool         { return false }

// findBuiltin returns the built-in tool with the given name, failing the spec when
// it is absent so a rename in the tool set is caught rather than silently skipped.
func findBuiltin(tools []*functool.Tool, name string) *functool.Tool {
	GinkgoHelper()
	for _, t := range tools {
		if t.Name() == name {
			return t
		}
	}
	Fail("built-in tool not found: " + name)

	return nil
}

// These tests pin the observable output of traceCall per tool kind: the ProviderKind
// carried on the emitted call trace (the slog token, and what the dispatch counters and
// the journaled Remote flag are taken from), the trace's Display/DisplayShort/Agent, and
// the per-run ExecDeps the kind receives. They assert today's behavior so a later
// refactor cannot silently change what the operator or the journal sees.
var _ = Describe("runner.traceCall parity", func() {
	const workDir = "/run/work-42"

	newRunner := func(ev *captureEvents, tools map[string]toolkit.Tool) *runner {
		return &runner{
			stats:       &RunStats{},
			events:      ev,
			set:         toolSetOf(tools),
			prompter:    toolkit.DefaultDenyPrompter(),
			toolWorkDir: workDir,
		}
	}

	It("Should trace a command tool with the full and short call lines under the application kind, given the work dir", func() {
		ev := &captureEvents{}
		tool := &fisktool.CommandTool{Path: []string{"stream", "info"}, Model: &fisk.CmdModel{}}
		r := newRunner(ev, map[string]toolkit.Tool{"stream_info": tool})
		use := llm.ToolUseBlock{ID: "t1", Name: "stream_info", Input: json.RawMessage(`{}`)}

		info := describeCall(r.set.tools[use.Name], use.Input)
		deps := r.traceCall(use, info)
		Expect(deps.WorkDir).To(Equal(workDir))
		Expect(deps.Prompter).To(BeNil())

		Expect(ev.calls).To(HaveLen(1))
		Expect(ev.calls[0].ProviderKind).To(Equal(toolkit.KindApplication))
		Expect(ev.calls[0].Display).To(Equal(tool.TraceLine(use.Input)))
		Expect(ev.calls[0].DisplayShort).To(Equal(tool.TraceLineShort(use.Input)))
		Expect(ev.calls[0].Display).NotTo(BeEmpty())
	})

	It("Should trace a remote tool naming its agent, under the remote kind, and pass no dependencies", func() {
		ev := &captureEvents{}
		desc := a2a.ToolDescriptor{Name: "info", Description: "reports info", InputSchema: json.RawMessage(`{"type":"object"}`)}
		rt, err := a2a.NewRemoteTool("nats_info", "nats", desc, stubInvoker{reply: a2a.NewToolReply("ok", false)})
		Expect(err).NotTo(HaveOccurred())
		r := newRunner(ev, map[string]toolkit.Tool{"nats_info": rt})
		use := llm.ToolUseBlock{ID: "t1", Name: "nats_info"}

		info := describeCall(r.set.tools[use.Name], use.Input)
		deps := r.traceCall(use, info)
		Expect(deps.Prompter).To(BeNil())
		Expect(deps.WorkDir).To(Equal(""))

		Expect(ev.calls).To(HaveLen(1))
		Expect(ev.calls[0].ProviderKind).To(Equal(toolkit.KindRemote))
		Expect(ev.calls[0].Agent).To(Equal("nats"))
	})

	It("Should trace a memory built-in with its call line and pass the operator prompter", func() {
		ev := &captureEvents{}
		memCfg := &config.Config{Harness: config.HarnessConfig{Memory: &config.MemoryConfig{Enabled: true}}}
		tool := findBuiltin(builtin.MemoryTools(memCfg, nil), "memory_list")
		r := newRunner(ev, map[string]toolkit.Tool{"memory_list": tool})
		use := llm.ToolUseBlock{ID: "t1", Name: "memory_list", Input: json.RawMessage(`{}`)}

		info := describeCall(r.set.tools[use.Name], use.Input)
		deps := r.traceCall(use, info)
		Expect(deps.Prompter).NotTo(BeNil())

		Expect(ev.calls).To(HaveLen(1))
		Expect(ev.calls[0].ProviderKind).To(Equal(toolkit.KindBuiltin))
		Expect(ev.calls[0].Display).To(Equal(tool.TraceLine(use.Input)))
		Expect(ev.calls[0].Display).NotTo(BeEmpty())
	})

	It("Should trace a human-in-the-loop built-in with no call line and pass the operator prompter", func() {
		ev := &captureEvents{}
		hitlCfg := &config.Config{Harness: config.HarnessConfig{HumanInTheLoop: &config.HumanInTheLoopConfig{Enabled: true}}}
		tool := findBuiltin(builtin.HITLTools(hitlCfg), "ask_human_confirm")
		r := newRunner(ev, map[string]toolkit.Tool{"ask_human_confirm": tool})
		use := llm.ToolUseBlock{ID: "t1", Name: "ask_human_confirm", Input: json.RawMessage(`{"question":"go?"}`)}

		info := describeCall(r.set.tools[use.Name], use.Input)
		deps := r.traceCall(use, info)
		Expect(deps.Prompter).NotTo(BeNil())

		Expect(ev.calls).To(HaveLen(1))
		Expect(ev.calls[0].ProviderKind).To(Equal(toolkit.KindBuiltin))
		// A human-in-the-loop tool shows its own prompt, so it declares no call line.
		Expect(ev.calls[0].Display).To(Equal(""))
	})

	It("Should trace a tool that does not describe itself by name, with no dependencies", func() {
		ev := &captureEvents{}
		r := newRunner(ev, map[string]toolkit.Tool{"mystery": describelessTool{}})
		use := llm.ToolUseBlock{ID: "t1", Name: "mystery", Input: json.RawMessage(`{}`)}

		info := describeCall(r.set.tools[use.Name], use.Input)
		deps := r.traceCall(use, info)
		Expect(deps.Prompter).To(BeNil())
		Expect(deps.WorkDir).To(Equal(""))

		Expect(ev.calls).To(HaveLen(1))
		Expect(ev.calls[0].ProviderKind).To(Equal(toolkit.KindUnknown))
		Expect(ev.calls[0].Name).To(Equal("mystery"))
		Expect(ev.calls[0].Display).To(Equal(""))
	})
})
