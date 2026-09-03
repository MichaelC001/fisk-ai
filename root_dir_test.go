//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/rag"
	"github.com/choria-io/fisk-ai/internal/runstate"
)

var _ = Describe("--root-dir", func() {
	// The flags these cases drive are package globals; snapshot and restore them so a
	// spec never leaks a root, a config or a set of typed paths into the next one.
	var origRoot, origConfig, origSessionConfig, origStateDir string
	var origPaths []string

	BeforeEach(func() {
		origRoot = rootDirFlag
		origConfig = configFile
		origSessionConfig = sessionConfigFile
		origStateDir = stateDirFlag
		origPaths = knowledgePaths
	})

	AfterEach(func() {
		rootDirFlag = origRoot
		configFile = origConfig
		sessionConfigFile = origSessionConfig
		stateDirFlag = origStateDir
		knowledgePaths = origPaths
	})

	// writeConfig writes a minimal agent configuration carrying body and returns its
	// path.
	writeConfig := func(dir string, body string) string {
		GinkgoHelper()

		path := filepath.Join(dir, "agent.yaml")
		Expect(os.WriteFile(path, []byte("llm:\n  model: claude-sonnet-4-6\n"+body), 0o600)).To(Succeed())

		return path
	}

	Describe("the fold in versionedConfig", func() {
		It("leaves a configuration alone when nothing sets a root", func() {
			rootDirFlag = ""

			cfg, err := versionedConfig(config.NewConfig())
			Expect(err).ToNot(HaveOccurred())
			Expect(cfg.RootDirectory).To(BeEmpty())
		})

		It("makes the flag absolute against the working directory", func() {
			dir, err := filepath.EvalSymlinks(GinkgoT().TempDir())
			Expect(err).ToNot(HaveOccurred())
			GinkgoT().Chdir(dir)

			rootDirFlag = "."

			cfg, err := versionedConfig(config.NewConfig())
			Expect(err).ToNot(HaveOccurred())
			Expect(cfg.RootDirectory).To(Equal(dir))
		})

		It("refuses a root that does not exist, naming the flag", func() {
			rootDirFlag = filepath.Join(GinkgoT().TempDir(), "absent")

			_, err := versionedConfig(config.NewConfig())
			Expect(err).To(MatchError(ContainSubstring("--root-dir")))
			Expect(err).To(MatchError(ContainSubstring("does not exist")))
		})

		It("refuses a relative root_directory in a file unless the flag replaces it", func() {
			dir := GinkgoT().TempDir()
			path := writeConfig(dir, "root_directory: srv/agent\n")

			rootDirFlag = ""
			_, err := versionedConfig(config.ParseConfigFileForMode(path, config.ModeMCP))
			Expect(err).To(MatchError(ContainSubstring("root_directory must be an absolute path")))

			rootDirFlag = dir
			cfg, err := versionedConfig(config.ParseConfigFileForMode(path, config.ModeMCP))
			Expect(err).ToNot(HaveOccurred())
			Expect(cfg.RootDirectory).To(Equal(dir))
		})
	})

	Describe("the session journal", func() {
		It("lands under <root>/runs, which every session subcommand reads", func() {
			root := GinkgoT().TempDir()

			sessionConfigFile = ""
			stateDirFlag = ""
			rootDirFlag = root

			store, cleanup, err := openSessionStore(context.Background())
			Expect(err).ToNot(HaveOccurred())
			defer cleanup()
			Expect(store).ToNot(BeNil())

			Expect(filepath.Join(root, "runs")).To(BeADirectory())
		})

		// A run opens its journal through sessionStoreFor and a session subcommand
		// opens one through openSessionStore. Under one root the two reach the same
		// journals, which is what lets fisk session ls show what fisk run wrote.
		It("is read by a session subcommand after a run under the same root wrote it", func() {
			root := GinkgoT().TempDir()

			rootDirFlag = root
			stateDirFlag = ""
			sessionConfigFile = ""

			// The store the hosted run journals into, opened the way runAction opens it.
			runCfg, err := versionedConfig(config.NewConfig())
			Expect(err).ToNot(HaveOccurred())

			runStore, releaseRun, err := sessionStoreFor(context.Background(), runCfg)
			Expect(err).ToNot(HaveOccurred())
			defer releaseRun()

			journal, err := runStore.Create(context.Background(), "root-session-1", runstate.MetaRecord{
				RunID:       "root-session-1",
				Prompt:      "how many streams are there",
				Fingerprint: runstate.Fingerprint{Model: "claude-opus-4-8"},
			})
			Expect(err).ToNot(HaveOccurred())
			Expect(journal.Close()).To(Succeed())

			// What fisk session ls and fisk session show read.
			sessionStore, releaseSession, err := openSessionStore(context.Background())
			Expect(err).ToNot(HaveOccurred())
			defer releaseSession()

			infos, err := sessionStore.List(context.Background())
			Expect(err).ToNot(HaveOccurred())
			Expect(infos).To(HaveLen(1))
			Expect(infos[0].RunID).To(Equal("root-session-1"))

			rs, err := sessionStore.Load(context.Background(), "root-session-1")
			Expect(err).ToNot(HaveOccurred())
			Expect(rs.Prompt).To(Equal("how many streams are there"))

			// A session command run without the root reads elsewhere, so it was the root
			// that brought the two together.
			rootDirFlag = ""
			stateDirFlag = GinkgoT().TempDir()

			elsewhere, releaseElsewhere, err := openSessionStore(context.Background())
			Expect(err).ToNot(HaveOccurred())
			defer releaseElsewhere()

			infos, err = elsewhere.List(context.Background())
			Expect(err).ToNot(HaveOccurred())
			Expect(infos).To(BeEmpty())
		})

		It("lands under the root when --state-dir names a relative directory", func() {
			root := GinkgoT().TempDir()

			sessionConfigFile = ""
			stateDirFlag = "sessions"
			rootDirFlag = root

			store, cleanup, err := openSessionStore(context.Background())
			Expect(err).ToNot(HaveOccurred())
			defer cleanup()
			Expect(store).ToNot(BeNil())

			Expect(filepath.Join(root, "sessions")).To(BeADirectory())
			Expect(filepath.Join(root, "runs")).ToNot(BeADirectory())
		})
	})

	Describe("knowledgeRoots", func() {
		ragConfig := func(root string, paths ...string) *config.Config {
			return &config.Config{
				Identity:      "agent",
				RootDirectory: root,
				Harness:       config.HarnessConfig{RAG: &config.RAGConfig{Enabled: true, Paths: paths}},
			}
		}

		It("walks the configured paths and reconciles when none are typed", func() {
			knowledgePaths = nil

			roots, reconcile, err := knowledgeRoots(ragConfig("/srv/agent", "docs"))
			Expect(err).ToNot(HaveOccurred())
			Expect(reconcile).To(BeTrue())
			Expect(roots).To(Equal([]string{"/srv/agent/docs"}))
		})

		It("returns nothing when none are typed and none are configured", func() {
			knowledgePaths = nil

			roots, reconcile, err := knowledgeRoots(ragConfig("/srv/agent"))
			Expect(err).ToNot(HaveOccurred())
			Expect(reconcile).To(BeTrue())
			Expect(roots).To(BeEmpty())
		})

		It("leaves a typed path as written when no root is set", func() {
			knowledgePaths = []string{"docs"}

			roots, reconcile, err := knowledgeRoots(ragConfig(""))
			Expect(err).ToNot(HaveOccurred())
			Expect(reconcile).To(BeFalse())
			Expect(roots).To(Equal([]string{"docs"}))
		})

		// A typed path resolves where the operator typed it, which under a root is what
		// keeps a hand-run index off a second relative copy of every document: nothing
		// prunes a typed walk, so two spellings of one corpus would both stay.
		It("makes a typed path absolute against the working directory under a root", func() {
			root, err := filepath.EvalSymlinks(GinkgoT().TempDir())
			Expect(err).ToNot(HaveOccurred())
			GinkgoT().Chdir(root)

			knowledgePaths = []string{"docs"}
			cfg := ragConfig(root, "docs")

			typed, reconcile, err := knowledgeRoots(cfg)
			Expect(err).ToNot(HaveOccurred())
			Expect(reconcile).To(BeFalse())
			Expect(typed).To(Equal([]string{filepath.Join(root, "docs")}))

			// The configured walk resolves to the same directory, so both write the same
			// document keys and one document is held once.
			Expect(typed).To(Equal(cfg.RAGPaths()))
		})

		// Nothing prunes a typed walk, so a typed path left relative would insert a
		// second copy of every document beside the keys the configured walk wrote,
		// doubling the index and returning each hit twice.
		It("leaves one copy of each document when the root's own corpus is indexed both ways", func() {
			root, err := filepath.EvalSymlinks(GinkgoT().TempDir())
			Expect(err).ToNot(HaveOccurred())

			corpus := filepath.Join(root, "docs")
			Expect(os.MkdirAll(corpus, 0o755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(corpus, "design.md"), []byte("# Design\n\nThe queue applies backpressure when the buffer is full.\n"), 0o600)).To(Succeed())

			cfg, err := config.ParseConfig([]byte(`
application_path: /bin/ls
identity: agent
system_prompt: hello
root_directory: ` + root + `
llm:
  model: claude-opus-4-8
harness:
  knowledge:
    enabled: true
    directory: ` + filepath.Join(root, "knowledge") + `
    paths:
      - docs
`))
			Expect(err).ToNot(HaveOccurred())

			index := func() {
				GinkgoHelper()

				roots, reconcile, err := knowledgeRoots(cfg)
				Expect(err).ToNot(HaveOccurred())

				w, err := rag.OpenWriter(cfg, "", rag.Options{})
				Expect(err).ToNot(HaveOccurred())
				defer w.Close()

				_, err = w.Index(context.Background(), roots, rag.IndexOptions{Reconcile: reconcile})
				Expect(err).ToNot(HaveOccurred())
			}

			// The configured walk first, as a run under this root builds it.
			knowledgePaths = nil
			index()

			// Then an operator standing in the root typing the path themselves.
			GinkgoT().Chdir(root)
			knowledgePaths = []string{"docs"}
			index()

			store, err := rag.Open(cfg, "", rag.Options{})
			Expect(err).ToNot(HaveOccurred())
			defer store.Close()

			sources, err := store.Sources(context.Background())
			Expect(err).ToNot(HaveOccurred())
			Expect(sources).To(HaveLen(1))
			Expect(sources[0].Path).To(Equal(filepath.ToSlash(filepath.Join(corpus, "design.md"))))
		})

		It("resolves a typed path from elsewhere against that directory, not the root", func() {
			root := GinkgoT().TempDir()
			elsewhere, err := filepath.EvalSymlinks(GinkgoT().TempDir())
			Expect(err).ToNot(HaveOccurred())
			GinkgoT().Chdir(elsewhere)

			knowledgePaths = []string{"docs"}

			typed, _, err := knowledgeRoots(ragConfig(root, "docs"))
			Expect(err).ToNot(HaveOccurred())
			Expect(typed).To(Equal([]string{filepath.Join(elsewhere, "docs")}))
		})
	})
})
