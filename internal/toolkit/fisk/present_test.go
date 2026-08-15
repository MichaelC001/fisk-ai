//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package fisk

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/choria-io/fisk"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/util"
)

var _ = Describe("FiskCommandTool.Definition", func() {
	It("Should map name, description and deferred loading", func() {
		app := fisk.New("app", "an app")
		app.Command("deploy", "deploy things")

		tools, err := ApplicationTools(introspect(app))
		Expect(err).NotTo(HaveOccurred())
		Expect(tools).To(HaveLen(1))

		def := tools[0].Definition(true)
		Expect(def.Name).To(Equal("deploy"))
		Expect(def.Description).To(Equal("deploy things"))
		Expect(def.DeferLoading).To(BeTrue())
	})

	It("Should not defer loading when loading is not deferred", func() {
		app := fisk.New("app", "an app")
		app.Command("deploy", "deploy things")

		tools, err := ApplicationTools(introspect(app))
		Expect(err).NotTo(HaveOccurred())
		Expect(tools).To(HaveLen(1))

		Expect(tools[0].Definition(false).DeferLoading).To(BeFalse())
	})

	It("Should carry the restricted schema including additionalProperties and required", func() {
		app := fisk.New("app", "an app")
		cmd := app.Command("deploy", "deploy things")
		cmd.Arg("target", "where to deploy").Required().String()
		cmd.Flag("force", "force the deploy").Bool()

		tools, err := ApplicationTools(introspect(app))
		Expect(err).NotTo(HaveOccurred())
		Expect(tools).To(HaveLen(1))

		schema := tools[0].Definition(true).InputSchema
		Expect(schema["type"]).To(Equal("object"))
		// additionalProperties:false is forwarded verbatim; strict mode requires it.
		Expect(schema["additionalProperties"]).To(Equal(false))

		props, ok := schema["properties"].(map[string]any)
		Expect(ok).To(BeTrue())
		Expect(props).To(HaveKey("target"))
		Expect(props).To(HaveKey("force"))

		Expect(schema["required"]).To(ConsistOf("target"))

		// The neutral schema carries each parameter's own description verbatim; the
		// "(optional)" annotation is applied later, when the provider codec renders the
		// schema to its wire form, so it is covered in the codec tests, not here.
		target, ok := props["target"].(map[string]any)
		Expect(ok).To(BeTrue())
		Expect(target["description"]).To(Equal("where to deploy"))

		force, ok := props["force"].(map[string]any)
		Expect(ok).To(BeTrue())
		Expect(force["description"]).To(Equal("force the deploy"))
	})

	It("Should produce an object schema even for a command with no arguments", func() {
		app := fisk.New("app", "an app")
		app.Command("noop", "does nothing")

		tools, err := ApplicationTools(introspect(app))
		Expect(err).NotTo(HaveOccurred())

		Expect(tools[0].Definition(true).InputSchema["type"]).To(Equal("object"))
	})
})

// toolkitSlice adapts application tools to the toolkit.Tool interface BuildToolParams
// takes, so the deferral logic can be exercised over a real command set.
func toolkitSlice(tools []*FiskCommandTool) []toolkit.Tool {
	out := make([]toolkit.Tool, len(tools))
	for i, t := range tools {
		out[i] = t
	}
	return out
}

// appWithCommands builds an application exposing n distinct tools, named cmd0..cmdN-1.
func appWithCommands(n int) []*FiskCommandTool {
	GinkgoHelper()

	app := fisk.New("app", "an app")
	for i := 0; i < n; i++ {
		app.Command(fmt.Sprintf("cmd%d", i), "a command")
	}

	tools, err := ApplicationTools(introspect(app))
	Expect(err).NotTo(HaveOccurred())
	Expect(tools).To(HaveLen(n))

	return tools
}

var _ = Describe("BuildToolParams over application tools", func() {
	// defByName finds the tool definition for a named tool.
	defByName := func(defs []llm.ToolDef, name string) llm.ToolDef {
		GinkgoHelper()
		for _, d := range defs {
			if d.Name == name {
				return d
			}
		}
		Fail(fmt.Sprintf("tool %q not found in definitions", name))
		return llm.ToolDef{}
	}

	It("Should send every tool directly without tool search below the threshold", func() {
		defs, toolSearch := util.BuildToolParams(toolkitSlice(appWithCommands(util.ToolSearchThreshold-1)), 0, true)

		Expect(defs).To(HaveLen(util.ToolSearchThreshold - 1))
		Expect(toolSearch).To(BeFalse())
		for _, d := range defs {
			Expect(d.DeferLoading).To(BeFalse())
		}
	})

	It("Should defer every tool and request tool search at the threshold", func() {
		defs, toolSearch := util.BuildToolParams(toolkitSlice(appWithCommands(util.ToolSearchThreshold)), 0, true)

		Expect(defs).To(HaveLen(util.ToolSearchThreshold))
		Expect(toolSearch).To(BeTrue())
		for _, d := range defs {
			Expect(d.DeferLoading).To(BeTrue())
		}
	})

	It("Should keep ai:no_defer tools loaded directly while deferring the rest", func() {
		app := fisk.New("app", "an app")
		// One pinned tool plus enough others to cross the defer threshold.
		app.Command("always", "always loaded").Tag(noDeferTag)
		for i := 0; i < util.ToolSearchThreshold; i++ {
			app.Command(fmt.Sprintf("cmd%d", i), "a command")
		}

		tools, err := ApplicationTools(introspect(app))
		Expect(err).NotTo(HaveOccurred())

		defs, toolSearch := util.BuildToolParams(toolkitSlice(tools), 0, true)

		// The pinned tool is sent directly; a deferred peer is not.
		Expect(defByName(defs, "always").DeferLoading).To(BeFalse())
		Expect(defByName(defs, "cmd0").DeferLoading).To(BeTrue())

		// Something is still deferred, so tool search is requested.
		Expect(toolSearch).To(BeTrue())
	})

	It("Should not request tool search when every deferred-eligible tool is pinned", func() {
		app := fisk.New("app", "an app")
		for i := 0; i < util.ToolSearchThreshold; i++ {
			app.Command(fmt.Sprintf("cmd%d", i), "a command").Tag(noDeferTag)
		}

		tools, err := ApplicationTools(introspect(app))
		Expect(err).NotTo(HaveOccurred())

		defs, toolSearch := util.BuildToolParams(toolkitSlice(tools), 0, true)

		// Nothing was deferred, so tool search is not requested.
		Expect(defs).To(HaveLen(util.ToolSearchThreshold))
		Expect(toolSearch).To(BeFalse())
		for _, d := range defs {
			Expect(d.DeferLoading).To(BeFalse())
		}
	})
})

var _ = Describe("FiskCommandTool.ExecuteUse", func() {
	// useBlock builds a tool_use block addressing tool t with the given raw input.
	useBlock := func(t *FiskCommandTool, id, input string) llm.ToolUseBlock {
		return llm.ToolUseBlock{ID: id, Name: t.Name(), Input: json.RawMessage(input)}
	}

	// doTool builds an "app do" command tool with a required argument, bound to
	// the given application path.
	doTool := func(appPath string) *FiskCommandTool {
		GinkgoHelper()

		app := fisk.New("app", "an app")
		app.Command("do", "do a thing").Arg("subject", "the subject").Required().String()

		tools, err := ApplicationTools(introspect(app))
		Expect(err).NotTo(HaveOccurred())

		tool := toolsByName(tools)["do"]
		Expect(tool).NotTo(BeNil())
		tool.AppPath = appPath
		return tool
	}

	// resultBlock extracts the tool_result fields from a neutral tool result.
	resultBlock := func(block llm.ToolResultBlock) (id string, text string, isError bool) {
		return block.ToolUseID, block.Content, block.IsError
	}

	It("Should return a successful tool_result carrying the command JSON", func() {
		tool := doTool(writeExecutable("#!/bin/sh\necho ran\n"))

		block, _, _ := toolkit.ExecuteUse(tool, context.Background(), useBlock(tool, "tu_1", `{"subject":"x"}`), toolkit.ExecDeps{})
		id, text, isError := resultBlock(block)
		Expect(id).To(Equal("tu_1"))
		Expect(isError).To(BeFalse())

		var result toolkit.CommandResult
		Expect(json.Unmarshal([]byte(text), &result)).To(Succeed())
		Expect(result.ExitCode).To(Equal(0))
		Expect(result.Output).To(Equal("ran\n"))
	})

	It("Should deliver a non-zero exit as a successful tool_result", func() {
		tool := doTool(writeExecutable("#!/bin/sh\nexit 4\n"))

		block, _, _ := toolkit.ExecuteUse(tool, context.Background(), useBlock(tool, "tu_2", `{"subject":"x"}`), toolkit.ExecDeps{})
		_, text, isError := resultBlock(block)
		Expect(isError).To(BeFalse())

		var result toolkit.CommandResult
		Expect(json.Unmarshal([]byte(text), &result)).To(Succeed())
		Expect(result.ExitCode).To(Equal(4))
	})

	It("Should report an execution failure as an error tool_result", func() {
		tool := doTool("/nonexistent/binary")

		block, _, _ := toolkit.ExecuteUse(tool, context.Background(), useBlock(tool, "tu_3", `{"subject":"x"}`), toolkit.ExecDeps{})
		id, text, isError := resultBlock(block)
		Expect(id).To(Equal("tu_3"))
		Expect(isError).To(BeTrue())
		Expect(text).To(ContainSubstring("running command"))
	})

	It("Should run with no arguments when the model sends a null input", func() {
		tool := doTool(writeExecutable("#!/bin/sh\necho ok\n"))

		block, _, _ := toolkit.ExecuteUse(tool, context.Background(), useBlock(tool, "tu_4", `null`), toolkit.ExecDeps{})
		_, _, isError := resultBlock(block)
		Expect(isError).To(BeFalse())
	})

	// The exit code survives the adaptation to the model's shape. Without this it exists
	// only inside the marshaled envelope, so a caller wanting it back would have to parse
	// the output it just produced.
	It("Should return the command metadata alongside the model-facing result", func() {
		tool := doTool(writeExecutable("#!/bin/sh\nexit 4\n"))

		_, exec, _ := toolkit.ExecuteUse(tool, context.Background(), useBlock(tool, "tu_5", `{"subject":"x"}`), toolkit.ExecDeps{})
		Expect(exec).ToNot(BeNil())
		Expect(exec.ExitCode).To(Equal(4))
		Expect(exec.Command).To(Equal("do -- x"))
	})

	// Zero is a real exit status and must come back as one, not as an absence. Treating
	// it as "nothing to report" is what makes a successful command indistinguishable from
	// a tool that never ran one.
	It("Should return metadata for a command that succeeded", func() {
		tool := doTool(writeExecutable("#!/bin/sh\necho ok\n"))

		_, exec, _ := toolkit.ExecuteUse(tool, context.Background(), useBlock(tool, "tu_6", `{"subject":"x"}`), toolkit.ExecDeps{})
		Expect(exec).ToNot(BeNil())
		Expect(exec.ExitCode).To(BeZero())
	})

	// A harness failure ran no command, so there is no status to report.
	It("Should return no metadata when the command could not run", func() {
		tool := doTool("/nonexistent/binary")

		_, exec, _ := toolkit.ExecuteUse(tool, context.Background(), useBlock(tool, "tu_7", `{"subject":"x"}`), toolkit.ExecDeps{})
		Expect(exec).To(BeNil())
	})
})
