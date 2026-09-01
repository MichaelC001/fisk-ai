//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package toolkit

import (
	"context"
	"encoding/json"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/llm"
)

// outcomeTool is a tool kind that returns whatever outcome a spec hands it, so the
// adaptation from an outcome to each surface's shape can be exercised without a
// subprocess or an in-process handler behind it.
type outcomeTool struct {
	outcome *Outcome
	err     error
}

func (outcomeTool) Name() string                { return "probe" }
func (outcomeTool) Description() string         { return "a probe" }
func (outcomeTool) ModelDescription() string    { return "a probe" }
func (outcomeTool) InputSchema() map[string]any { return map[string]any{"type": "object"} }
func (outcomeTool) Definition(bool) llm.ToolDef { return llm.ToolDef{Name: "probe"} }
func (outcomeTool) MCPExposable() bool          { return false }
func (outcomeTool) A2AExposable() bool          { return false }

func (t outcomeTool) Execute(context.Context, json.RawMessage, ExecDeps) (*Outcome, error) {
	return t.outcome, t.err
}

var _ = Describe("ExecuteUse", func() {
	ctx := context.Background()
	use := llm.ToolUseBlock{ID: "tu_1", Name: "probe", Input: json.RawMessage(`{}`)}

	// These assert exact bytes rather than a shape, because the content is what the
	// model reads. A change here changes what every model sees on every surface, so
	// it should have to be made deliberately.
	It("passes an in-process tool's output through verbatim", func() {
		tool := outcomeTool{outcome: &Outcome{Output: `{"status":"ok","results":[]}`}}

		res, _, _ := ExecuteUse(ctx, tool, use, ExecDeps{})
		Expect(res.IsError).To(BeFalse())
		Expect(res.ToolUseID).To(Equal("tu_1"))
		Expect(res.Content).To(Equal(`{"status":"ok","results":[]}`))
	})

	It("wraps a command's output in the command envelope", func() {
		tool := outcomeTool{outcome: &Outcome{
			Output: "out",
			Exec:   &CommandExec{Command: "ping", ExitCode: 2, Truncated: true},
		}}

		res, _, _ := ExecuteUse(ctx, tool, use, ExecDeps{})
		Expect(res.IsError).To(BeFalse())
		Expect(res.ToolUseID).To(Equal("tu_1"))
		Expect(res.Content).To(Equal(`{"command":"ping","exit_code":2,"output":"out","truncated":true}`))
	})

	// Truncated is omitempty, so an untruncated command omits it rather than
	// reporting false: the same shape a command tool has always produced.
	It("omits the truncation flag when a command was not truncated", func() {
		tool := outcomeTool{outcome: &Outcome{
			Output: "out",
			Exec:   &CommandExec{Command: "ping"},
		}}

		res, _, _ := ExecuteUse(ctx, tool, use, ExecDeps{})
		Expect(res.Content).To(Equal(`{"command":"ping","exit_code":0,"output":"out"}`))
	})

	It("reports a harness failure as an error result carrying the tool_use id", func() {
		tool := outcomeTool{err: errors.New("could not run")}

		res, _, _ := ExecuteUse(ctx, tool, use, ExecDeps{})
		Expect(res.IsError).To(BeTrue())
		Expect(res.ToolUseID).To(Equal("tu_1"))
		Expect(res.Content).To(Equal("could not run"))
	})
})

var _ = Describe("Outcome", func() {
	It("renders a command's outcome as the CommandResult a surface marshals", func() {
		out := &Outcome{Output: "out", Exec: &CommandExec{Command: "ping", ExitCode: 1, Truncated: true}}
		Expect(out.CommandResult()).To(Equal(CommandResult{Command: "ping", ExitCode: 1, Output: "out", Truncated: true}))
	})

	It("renders an in-process outcome as output alone", func() {
		out := &Outcome{Output: "{}"}
		Expect(out.CommandResult()).To(Equal(CommandResult{Output: "{}"}))
	})
})

var _ = Describe("Tools", func() {
	It("widens a concrete slice preserving order, which decides who keeps a name", func() {
		widened := Tools([]outcomeTool{{outcome: &Outcome{Output: "a"}}, {outcome: &Outcome{Output: "b"}}})
		Expect(widened).To(HaveLen(2))

		first, err := widened[0].Execute(context.Background(), nil, ExecDeps{})
		Expect(err).ToNot(HaveOccurred())
		Expect(first.Output).To(Equal("a"))
	})
})
