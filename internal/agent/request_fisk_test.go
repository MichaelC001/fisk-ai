//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/choria-io/fisk"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	fisktool "github.com/choria-io/fisk-ai/internal/toolkit/fisk"
)

// introspect mirrors the production path: it drives the application's real
// --fisk-introspect handler in-process, capturing the JSON it writes to stdout
// and unmarshaling it, yielding a model whose schemas are populated but whose
// Values are gone (as they are over the --fisk-introspect process boundary).
func introspect(app *fisk.Application) *fisk.ApplicationModel {
	GinkgoHelper()

	// --fisk-introspect calls terminate(0); make it a no-op so the test survives.
	app.Terminate(func(int) {})

	r, w, err := os.Pipe()
	Expect(err).NotTo(HaveOccurred())

	stdout := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = stdout }()

	// read concurrently so a large model can't fill the pipe and block the write.
	captured := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(r)
		captured <- data
	}()

	_, err = app.Parse([]string{"--fisk-introspect"})
	Expect(err).NotTo(HaveOccurred())
	Expect(w.Close()).To(Succeed())

	var m fisk.ApplicationModel
	Expect(json.Unmarshal(<-captured, &m)).To(Succeed())
	return &m
}

// toolkitSlice adapts application tools to the toolkit.Tool interface
// BuildToolParams takes, so the deferral logic runs over a real command set.
func toolkitSlice(tools []*fisktool.FiskCommandTool) []toolkit.Tool {
	out := make([]toolkit.Tool, len(tools))
	for i, t := range tools {
		out[i] = t
	}
	return out
}

// appWithCommands builds an application exposing n distinct tools, named cmd0..cmdN-1.
func appWithCommands(n int) []*fisktool.FiskCommandTool {
	GinkgoHelper()

	app := fisk.New("app", "an app")
	for i := 0; i < n; i++ {
		app.Command(fmt.Sprintf("cmd%d", i), "a command")
	}

	tools, err := fisktool.ApplicationTools(introspect(app))
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
		defs, toolSearch := BuildToolParams(toolkitSlice(appWithCommands(ToolSearchThreshold-1)), 0, true)

		Expect(defs).To(HaveLen(ToolSearchThreshold - 1))
		Expect(toolSearch).To(BeFalse())
		for _, d := range defs {
			Expect(d.DeferLoading).To(BeFalse())
		}
	})

	It("Should defer every tool and request tool search at the threshold", func() {
		defs, toolSearch := BuildToolParams(toolkitSlice(appWithCommands(ToolSearchThreshold)), 0, true)

		Expect(defs).To(HaveLen(ToolSearchThreshold))
		Expect(toolSearch).To(BeTrue())
		for _, d := range defs {
			Expect(d.DeferLoading).To(BeTrue())
		}
	})

	It("Should keep ai:no_defer tools loaded directly while deferring the rest", func() {
		app := fisk.New("app", "an app")
		// One pinned tool plus enough others to cross the defer threshold.
		app.Command("always", "always loaded").Tag(toolkit.NoDeferTag)
		for i := 0; i < ToolSearchThreshold; i++ {
			app.Command(fmt.Sprintf("cmd%d", i), "a command")
		}

		tools, err := fisktool.ApplicationTools(introspect(app))
		Expect(err).NotTo(HaveOccurred())

		defs, toolSearch := BuildToolParams(toolkitSlice(tools), 0, true)

		// The pinned tool is sent directly; a deferred peer is not.
		Expect(defByName(defs, "always").DeferLoading).To(BeFalse())
		Expect(defByName(defs, "cmd0").DeferLoading).To(BeTrue())

		// Something is still deferred, so tool search is requested.
		Expect(toolSearch).To(BeTrue())
	})

	It("Should not request tool search when every deferred-eligible tool is pinned", func() {
		app := fisk.New("app", "an app")
		for i := 0; i < ToolSearchThreshold; i++ {
			app.Command(fmt.Sprintf("cmd%d", i), "a command").Tag(toolkit.NoDeferTag)
		}

		tools, err := fisktool.ApplicationTools(introspect(app))
		Expect(err).NotTo(HaveOccurred())

		defs, toolSearch := BuildToolParams(toolkitSlice(tools), 0, true)

		// Nothing was deferred, so tool search is not requested.
		Expect(defs).To(HaveLen(ToolSearchThreshold))
		Expect(toolSearch).To(BeFalse())
		for _, d := range defs {
			Expect(d.DeferLoading).To(BeFalse())
		}
	})
})
