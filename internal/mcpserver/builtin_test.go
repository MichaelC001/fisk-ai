//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"

	"github.com/choria-io/fisk"
	tools2 "github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/toolkit/builtin"
	"github.com/choria-io/fisk-ai/internal/toolkit/functool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/rag"
)

// lexicalKnowledgeBuiltins builds a real lexical knowledge_search built-in backed
// by a small on-disk index, so a dispatch exercises the actual tool end to end.
func lexicalKnowledgeBuiltins(ctx context.Context) []*functool.Tool {
	GinkgoHelper()

	storeDir := GinkgoT().TempDir()
	docsDir := GinkgoT().TempDir()
	Expect(os.WriteFile(filepath.Join(docsDir, "a.md"),
		[]byte("# Backpressure\n\nThe queue applies backpressure when the buffer is full.\n"), 0o644)).To(Succeed())

	cfg := &config.Config{
		Identity: "test",
		Harness:  config.HarnessConfig{RAG: &config.RAGConfig{Enabled: true, Directory: storeDir}},
	}

	w, err := rag.OpenWriter(cfg, "", rag.Options{})
	Expect(err).NotTo(HaveOccurred())
	_, err = w.Index(ctx, []string{docsDir}, rag.IndexOptions{Reconcile: true})
	Expect(err).NotTo(HaveOccurred())
	Expect(w.Close()).To(Succeed())

	store, err := rag.Open(cfg, "", rag.Options{})
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() { store.Close() })

	return builtin.RAGTools(cfg, store)
}

var _ = Describe("BuildServer built-ins", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("serves a built-in-only server and returns its JSON result verbatim", func() {
		builtins := lexicalKnowledgeBuiltins(ctx)
		// BuildServer applies capability only, so it registers both knowledge tools
		// here; narrowing to the operator's allowlist happens before this point.
		srv, registered := BuildServer(tools2.Tools(builtins), Options{Name: "app", Version: "v1", LogOutput: io.Discard})
		Expect(registered).To(ConsistOf("knowledge_search", "knowledge_enumerate"))

		cs := connect(ctx, srv)
		defer cs.Close()

		text, isError := callText(ctx, cs, "knowledge_search", map[string]any{"query": "backpressure"})
		Expect(isError).To(BeFalse())

		// The handler already returns JSON, so the result must be that object, not a
		// double-encoded JSON string.
		var out struct {
			Tier    string `json:"tier"`
			Status  string `json:"status"`
			Results []struct {
				Citation string `json:"citation"`
				Content  string `json:"content"`
			} `json:"results"`
		}
		Expect(json.Unmarshal([]byte(text), &out)).To(Succeed())
		Expect(out.Tier).To(ContainSubstring("lexical"))
		Expect(out.Status).To(Equal("ok"))
		Expect(out.Results).NotTo(BeEmpty())
		Expect(out.Results[0].Citation).To(ContainSubstring("a.md#"))
	})

	// Both kinds now register through one loop and one handler, so the split between
	// the two result shapes is a runtime decision rather than two code paths. This
	// pins both halves of it against one server, which is the case a single-kind
	// spec cannot cover.
	It("serves a command tool and a built-in from one list, each in its own shape", func() {
		app := fisk.New("app", "an app")
		app.Command("ping", "a command")
		cmdTools := cmdToolsFor(app)
		cmdTools[0].AppPath = writeExecutable("#!/bin/sh\necho pong\n")

		served := append(tools2.Tools(cmdTools), tools2.Tools(lexicalKnowledgeBuiltins(ctx))...)
		srv, registered := BuildServer(served, Options{Name: "app", Version: "v1", LogOutput: io.Discard})
		Expect(registered).To(ConsistOf("ping", "knowledge_search", "knowledge_enumerate"))

		cs := connect(ctx, srv)
		defer cs.Close()

		// A command's output is wrapped so the client sees the exit code.
		text, isError := callText(ctx, cs, "ping", nil)
		Expect(isError).To(BeFalse())

		var res tools2.CommandResult
		Expect(json.Unmarshal([]byte(text), &res)).To(Succeed())
		Expect(res.ExitCode).To(Equal(0))
		Expect(res.Output).To(ContainSubstring("pong"))

		// The built-in's own JSON travels verbatim, with no envelope around it.
		text, isError = callText(ctx, cs, "knowledge_search", map[string]any{"query": "backpressure"})
		Expect(isError).To(BeFalse())
		Expect(text).To(ContainSubstring(`"tier"`))
		Expect(text).ToNot(ContainSubstring("exit_code"))
	})

	It("skips a built-in whose name a wrapped command already exposes", func() {
		app := fisk.New("app", "an app")
		k := app.Command("knowledge", "a group")
		k.Command("search", "a command") // introspects to the tool name knowledge_search
		cmdTools := toolsFor(app)

		var logs bytes.Buffer
		_, registered := BuildServer(append(cmdTools, tools2.Tools(lexicalKnowledgeBuiltins(ctx))...), Options{Name: "app", Version: "v1", LogOutput: &logs})

		// The command tool wins; the built-in is skipped, so the name registers once.
		count := 0
		for _, n := range registered {
			if n == "knowledge_search" {
				count++
			}
		}
		Expect(count).To(Equal(1))
		Expect(logs.String()).To(ContainSubstring("that name is already exposed"))
	})

	It("dispatches concurrent calls to one read-only store", func() {
		builtins := lexicalKnowledgeBuiltins(ctx)
		srv, _ := BuildServer(tools2.Tools(builtins), Options{Name: "app", Version: "v1", LogOutput: io.Discard})

		cs := connect(ctx, srv)
		defer cs.Close()

		done := make(chan bool, 8)
		for i := 0; i < 8; i++ {
			go func() {
				defer GinkgoRecover()
				_, isError := callText(ctx, cs, "knowledge_search", map[string]any{"query": "backpressure buffer"})
				done <- isError
			}()
		}
		for i := 0; i < 8; i++ {
			Expect(<-done).To(BeFalse())
		}
	})
})
