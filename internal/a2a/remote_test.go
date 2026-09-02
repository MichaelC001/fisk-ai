//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	"context"
	"encoding/json"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// fakeInvoker is a hand-written RemoteInvoker for driving RemoteTool without a
// transport: it records the last call and returns a canned reply or error.
type fakeInvoker struct {
	reply *ToolReply
	err   error

	gotAgent string
	gotTool  string
	gotInput json.RawMessage
}

func (f *fakeInvoker) InvokeTool(_ context.Context, agent, tool string, input json.RawMessage) (*ToolReply, error) {
	f.gotAgent = agent
	f.gotTool = tool
	f.gotInput = input

	return f.reply, f.err
}

var _ = Describe("RemoteTool", func() {
	descriptor := ToolDescriptor{
		Name:        "stream_info",
		Description: "Reports on a stream",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"stream":{"type":"string"}},"required":["stream"]}`),
	}

	Describe("NewRemoteTool", func() {
		It("Should prefix the local name and present its agent, keeping the remote name on the wire", func() {
			rt, err := NewRemoteTool("nats_stream_info", "nats", descriptor, &fakeInvoker{})
			Expect(err).NotTo(HaveOccurred())
			Expect(rt.Name()).To(Equal("nats_stream_info"))
			Expect(rt.Describe(nil).Agent).To(Equal("nats"))
			Expect(rt.Description()).To(Equal("Reports on a stream"))
			Expect(rt.InputSchema()).To(HaveKey("properties"))
		})

		It("Should default to an object schema when none is advertised", func() {
			rt, err := NewRemoteTool("nats_x", "nats", ToolDescriptor{Name: "x", Description: "does x"}, &fakeInvoker{})
			Expect(err).NotTo(HaveOccurred())
			Expect(rt.InputSchema()).To(Equal(map[string]any{"type": "object"}))
		})

		It("Should reject an unparsable input schema", func() {
			_, err := NewRemoteTool("nats_x", "nats", ToolDescriptor{Name: "x", InputSchema: json.RawMessage(`not json`)}, &fakeInvoker{})
			Expect(err).To(MatchError(ContainSubstring("unparsable input schema")))
		})

		It("Should reject a descriptor that advertises no description", func() {
			_, err := NewRemoteTool("nats_x", "nats", ToolDescriptor{Name: "x"}, &fakeInvoker{})
			Expect(err).To(MatchError(ContainSubstring("advertises no description")))
		})

		It("Should not validate required arguments locally, leaving that to the serving agent", func() {
			rt, err := NewRemoteTool("nats_stream_info", "nats", descriptor, &fakeInvoker{})
			Expect(err).NotTo(HaveOccurred())
			Expect(rt.MissingRequired(json.RawMessage(`{}`))).To(BeNil())
		})

		It("Should carry the behavior the serving agent declared", func() {
			desc := descriptor
			desc.Behavior = toolBehavior(toolkit.Behavior{ReadOnly: toolkit.HintTrue, Idempotent: toolkit.HintTrue})

			rt, err := NewRemoteTool("nats_stream_info", "nats", desc, &fakeInvoker{})
			Expect(err).NotTo(HaveOccurred())
			Expect(rt.Behavior()).To(Equal(desc.Behavior.Toolkit()))
		})

		It("Should not repeat the behavior the serving agent already wrote into its description", func() {
			desc := descriptor
			desc.Behavior = toolBehavior(toolkit.Behavior{ReadOnly: toolkit.HintTrue})

			rt, err := NewRemoteTool("nats_stream_info", "nats", desc, &fakeInvoker{})
			Expect(err).NotTo(HaveOccurred())
			Expect(rt.ModelDescription()).To(Equal("Reports on a stream"))
		})

		It("Should import a peer that contradicts itself, resolved, rather than dropping its tool", func() {
			desc := descriptor
			desc.Behavior = toolBehavior(toolkit.Behavior{ReadOnly: toolkit.HintTrue, Destructive: toolkit.HintTrue})

			rt, err := NewRemoteTool("nats_stream_info", "nats", desc, &fakeInvoker{})
			Expect(err).NotTo(HaveOccurred())
			Expect(rt.Behavior().ReadOnly).To(Equal(toolkit.HintFalse))
			Expect(rt.Behavior().Destructive).To(Equal(toolkit.HintTrue))
		})

		It("Should never let a peer's claim be re-served as this agent's own", func() {
			desc := descriptor
			desc.Behavior = toolBehavior(toolkit.Behavior{ReadOnly: toolkit.HintTrue})

			rt, err := NewRemoteTool("nats_stream_info", "nats", desc, &fakeInvoker{})
			Expect(err).NotTo(HaveOccurred())
			Expect(rt.MCPExposable()).To(BeFalse())
			Expect(rt.A2AExposable()).To(BeFalse())
		})
	})

	Describe("Definition", func() {
		It("Should render a custom tool with the local name and advertised schema", func() {
			rt, _ := NewRemoteTool("nats_stream_info", "nats", descriptor, &fakeInvoker{})
			def := rt.Definition(true)
			Expect(def.Name).To(Equal("nats_stream_info"))
			Expect(def.DeferLoading).To(BeTrue())
			Expect(toolkit.SchemaRequired(def.InputSchema["required"])).To(ConsistOf("stream"))
		})
	})

	Describe("ExecuteRemoteUse", func() {
		use := llm.ToolUseBlock{ID: "call-1", Name: "nats_stream_info", Input: json.RawMessage(`{"stream":"ORDERS"}`)}

		It("Should map a successful reply to a CommandResult JSON result", func() {
			inv := &fakeInvoker{reply: &ToolReply{ToolResult: ToolResult{
				Output: "all good",
				Exec:   &ExecResult{Command: "stream info ORDERS", ExitCode: 0, Truncated: false},
			}}}
			rt, _ := NewRemoteTool("nats_stream_info", "nats", descriptor, inv)

			block, _, _ := toolkit.ExecuteUse(context.Background(), rt, use, toolkit.ExecDeps{})
			Expect(block.IsError).To(BeFalse())

			var result toolkit.CommandResult
			Expect(json.Unmarshal([]byte(block.Content), &result)).To(Succeed())
			Expect(result.Command).To(Equal("stream info ORDERS"))
			Expect(result.Output).To(Equal("all good"))

			// The remote name and input travel on the wire, not the local alias name.
			Expect(inv.gotTool).To(Equal("stream_info"))
			Expect(inv.gotAgent).To(Equal("nats"))
			Expect(string(inv.gotInput)).To(Equal(`{"stream":"ORDERS"}`))
		})

		It("Should preserve a non-zero exit as a successful result", func() {
			inv := &fakeInvoker{reply: &ToolReply{ToolResult: ToolResult{
				Output: "boom",
				Exec:   &ExecResult{Command: "stream info ORDERS", ExitCode: 3},
			}}}
			rt, _ := NewRemoteTool("nats_stream_info", "nats", descriptor, inv)

			block, _, _ := toolkit.ExecuteUse(context.Background(), rt, use, toolkit.ExecDeps{})
			Expect(block.IsError).To(BeFalse())

			var result toolkit.CommandResult
			Expect(json.Unmarshal([]byte(block.Content), &result)).To(Succeed())
			Expect(result.ExitCode).To(Equal(3))
		})

		// A reply with no exec metadata came from an in-process tool on the serving
		// agent, whose output is already the JSON the model asked for. Rebuilding a
		// CommandResult around it would hand the model a command envelope carrying a
		// fabricated exit_code 0 for a command that never ran, with the tool's own
		// JSON string-escaped inside it.
		It("Should pass an exec-less reply through verbatim rather than wrapping it", func() {
			inv := &fakeInvoker{reply: &ToolReply{ToolResult: ToolResult{
				Output: `{"status":"ok","results":[]}`,
			}}}
			rt, _ := NewRemoteTool("nats_stream_info", "nats", descriptor, inv)

			block, _, _ := toolkit.ExecuteUse(context.Background(), rt, use, toolkit.ExecDeps{})
			Expect(block.IsError).To(BeFalse())
			Expect(block.Content).To(Equal(`{"status":"ok","results":[]}`))
			Expect(block.Content).ToNot(ContainSubstring("exit_code"))
		})

		It("Should map a remote harness failure to an error result", func() {
			inv := &fakeInvoker{reply: &ToolReply{ToolResult: ToolResult{IsError: true, Output: "tool not available"}}}
			rt, _ := NewRemoteTool("nats_stream_info", "nats", descriptor, inv)

			block, _, _ := toolkit.ExecuteUse(context.Background(), rt, use, toolkit.ExecDeps{})
			Expect(block.IsError).To(BeTrue())
			Expect(block.Content).To(Equal("tool not available"))
		})

		It("Should map a transport error to an error result", func() {
			inv := &fakeInvoker{err: errors.New("no responders")}
			rt, _ := NewRemoteTool("nats_stream_info", "nats", descriptor, inv)

			block, _, _ := toolkit.ExecuteUse(context.Background(), rt, use, toolkit.ExecDeps{})
			Expect(block.IsError).To(BeTrue())
			Expect(block.Content).To(ContainSubstring("no responders"))
		})
	})
})
