//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// These tests hold Run to config.Config.RootDirectory: it supplies the directory the
// command tools run in and the base the stores resolve under when the caller set
// neither option, and an explicit option still replaces it.
package agent_test

import (
	"context"
	"encoding/json"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/runstate"
)

var _ = Describe("The root directory", func() {
	// callDo runs one call of the fake application's "do" command and returns what the
	// model saw come back, which begins with the PWD the command ran in.
	callDo := func(opts agent.Options) string {
		GinkgoHelper()

		provider := agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("c1", "do", json.RawMessage(`{"subject":"thing"}`)),
			agenttest.TextResponse("done"),
		)

		opts.ConfigFile = "agent.yaml"
		opts.Prompt = []string{"go"}
		opts.Provider = provider

		_, err := agent.Run(context.Background(), opts, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())

		return toolResultContent(provider, "c1")
	}

	It("Should run the command tools in it", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		root := GinkgoT().TempDir()
		cfg.RootDirectory = root

		Expect(callDo(agent.Options{Config: cfg})).To(ContainSubstring("PWD=" + root))
	})

	It("Should let an explicit ToolWorkDir replace it", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		cfg.RootDirectory = GinkgoT().TempDir()
		work := GinkgoT().TempDir()

		out := callDo(agent.Options{Config: cfg, ToolWorkDir: work})
		Expect(out).To(ContainSubstring("PWD=" + work))
		Expect(out).ToNot(ContainSubstring("PWD=" + cfg.RootDirectory))
	})

	It("Should leave the tools in the process working directory when no root is set", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)

		wd, err := filepath.EvalSymlinks(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())
		GinkgoT().Chdir(wd)

		Expect(callDo(agent.Options{Config: cfg})).To(ContainSubstring("PWD=" + wd))
	})

	It("Should journal under <root>/runs", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		root := GinkgoT().TempDir()
		cfg.RootDirectory = root

		read := journaledRun(agent.Options{Config: cfg}, "root-1")
		Expect(read.StoreDir).To(BeEmpty(), "the caller set no store base of its own")
		Expect(filepath.Join(root, "runs")).To(BeADirectory())

		rs, err := agent.LoadSession(context.Background(), cfg, "root-1", agent.SessionOptions{StoreDir: root})
		Expect(err).NotTo(HaveOccurred())
		Expect(rs.RunID).To(Equal("root-1"))

		// The XDG default holds nothing for it, so the journal went to the root.
		_, err = agent.LoadSession(context.Background(), cfg, "root-1", agent.SessionOptions{StoreDir: GinkgoT().TempDir()})
		Expect(err).To(MatchError(runstate.ErrNotFound))
	})

	// An embedder that set root_directory and nothing else pre-flights a resume with
	// the SessionOptions its own run produced, whose StoreDir is empty. LoadSession
	// resolves that the way Run did, or the read looks in the XDG state directory for a
	// journal the run wrote under the root.
	It("Should read a journal back through the SessionOptions of a run that set only it", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		root := GinkgoT().TempDir()
		cfg.RootDirectory = root

		read := journaledRun(agent.Options{Config: cfg}, "root-3")
		Expect(read.StoreDir).To(BeEmpty())
		Expect(read.SessionStore).To(BeNil())

		rs, err := agent.LoadSession(context.Background(), cfg, "root-3", read)
		Expect(err).NotTo(HaveOccurred())
		Expect(rs.RunID).To(Equal("root-3"))
		Expect(rs.Prompt).To(Equal("go"))

		// The same read against a configuration with no root finds nothing, so it was
		// the root that located the journal rather than an XDG directory both share.
		rootless := agenttest.Config(GinkgoTB(), app)
		_, err = agent.LoadSession(context.Background(), rootless, "root-3", read)
		Expect(err).To(MatchError(runstate.ErrNotFound))
	})

	It("Should let an explicit StoreDir replace it", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		root := GinkgoT().TempDir()
		store := GinkgoT().TempDir()
		cfg.RootDirectory = root

		journaledRun(agent.Options{Config: cfg, StoreDir: store}, "root-2")
		Expect(filepath.Join(store, "runs")).To(BeADirectory())
		Expect(filepath.Join(root, "runs")).ToNot(BeADirectory())
	})

	It("Should place the memory store under it", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app, agenttest.WithMemory())
		root := GinkgoT().TempDir()
		cfg.RootDirectory = root

		_, err := agent.Run(context.Background(), agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			Provider:   agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())

		Expect(filepath.Join(root, "memory", cfg.Identity)).To(BeADirectory())
	})

	// The knowledge index is read-only at run time, so what the root decides is which
	// file the run looked for and did not find.
	It("Should look for the knowledge index under it", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app, agenttest.WithRAG())
		root := GinkgoT().TempDir()
		cfg.RootDirectory = root

		events := agenttest.NewRecordingEvents()
		_, err := agent.Run(context.Background(), agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			Provider:   agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
		}, events, agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())

		var named []string
		for _, w := range events.Warnings() {
			if w.Kind == agent.WarnKnowledgeIndexAbsent {
				named = append(named, w.Name)
			}
		}
		Expect(named).To(HaveLen(1))
		Expect(named[0]).To(HavePrefix(filepath.Join(root, "knowledge", cfg.Identity)))
	})

	It("Should refuse a root that does not exist", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		cfg.RootDirectory = filepath.Join(GinkgoT().TempDir(), "absent")

		_, err := agent.Run(context.Background(), agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			Provider:   agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).To(MatchError(ContainSubstring("root_directory")))
		Expect(err).To(MatchError(ContainSubstring("does not exist")))
	})

	It("Should refuse a relative root", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		cfg.RootDirectory = "srv/agent"

		_, err := agent.Run(context.Background(), agent.Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			Prompt:     []string{"go"},
			Provider:   agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done")),
		}, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).To(MatchError(ContainSubstring("root_directory must be an absolute path")))
	})
})
