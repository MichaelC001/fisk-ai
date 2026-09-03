// Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Root directory", func() {
	Describe("ApplyRootDir", func() {
		It("leaves a configuration that names no root alone", func() {
			cfg := &Config{}
			Expect(cfg.ApplyRootDir("")).To(Succeed())
			Expect(cfg.RootDirectory).To(BeEmpty())
			Expect(cfg.StoreBase("")).To(BeEmpty())
		})

		It("makes the flag absolute against the working directory", func() {
			wd, err := os.Getwd()
			Expect(err).ToNot(HaveOccurred())

			cfg := &Config{}
			Expect(cfg.ApplyRootDir(".")).To(Succeed())
			Expect(cfg.RootDirectory).To(Equal(wd))
		})

		It("replaces a root_directory the file set", func() {
			dir := GinkgoT().TempDir()

			cfg := &Config{RootDirectory: "/no/such/root"}
			Expect(cfg.ApplyRootDir(dir)).To(Succeed())
			Expect(cfg.RootDirectory).To(Equal(dir))
		})

		It("refuses a relative root_directory the flag does not replace", func() {
			cfg := &Config{RootDirectory: "srv/agent"}
			err := cfg.ApplyRootDir("")
			Expect(err).To(MatchError(ContainSubstring(`root_directory must be an absolute path, got "srv/agent"`)))

			// The same file with a flag beside it resolves, since the flag replaces it.
			dir := GinkgoT().TempDir()
			cfg = &Config{RootDirectory: "srv/agent"}
			Expect(cfg.ApplyRootDir(dir)).To(Succeed())
			Expect(cfg.RootDirectory).To(Equal(dir))
		})

		It("refuses a root that does not exist, naming the source that set it", func() {
			missing := filepath.Join(GinkgoT().TempDir(), "absent")

			cfg := &Config{}
			err := cfg.ApplyRootDir(missing)
			Expect(err).To(MatchError(ContainSubstring("--root-dir")))
			Expect(err).To(MatchError(ContainSubstring("does not exist")))

			cfg = &Config{RootDirectory: missing}
			err = cfg.ApplyRootDir("")
			Expect(err).To(MatchError(ContainSubstring("root_directory")))
			Expect(err).To(MatchError(ContainSubstring("does not exist")))
		})

		It("refuses a root that is a file", func() {
			file := filepath.Join(GinkgoT().TempDir(), "agent.yaml")
			Expect(os.WriteFile(file, []byte("x"), 0o600)).To(Succeed())

			cfg := &Config{}
			Expect(cfg.ApplyRootDir(file)).To(MatchError(ContainSubstring("is not a directory")))
		})
	})

	Describe("root_directory in a file", func() {
		It("parses and is read back", func() {
			dir := GinkgoT().TempDir()
			data := []byte(`
application_path: /bin/ls
identity: kb
system_prompt: hello
root_directory: ` + dir + `
llm:
  model: claude-opus-4-8
`)
			cfg, err := ParseConfig(data)
			Expect(err).ToNot(HaveOccurred())
			Expect(cfg.RootDirectory).To(Equal(dir))
		})
	})

	Describe("StoreBase", func() {
		It("prefers an explicit base and falls back to the root", func() {
			cfg := &Config{RootDirectory: "/srv/agent"}
			Expect(cfg.StoreBase("")).To(Equal("/srv/agent"))
			Expect(cfg.StoreBase("/var/lib/fisk")).To(Equal("/var/lib/fisk"))

			none := &Config{}
			Expect(none.StoreBase("")).To(BeEmpty())
			Expect(none.StoreBase("/var/lib/fisk")).To(Equal("/var/lib/fisk"))
		})
	})

	Describe("RAGPaths", func() {
		It("returns nothing when the block names no paths", func() {
			Expect((&Config{}).RAGPaths()).To(BeNil())
			Expect((&Config{Harness: HarnessConfig{RAG: &RAGConfig{Enabled: true}}}).RAGPaths()).To(BeNil())
		})

		It("returns the entries as written when no root is set", func() {
			cfg := &Config{Harness: HarnessConfig{RAG: &RAGConfig{Enabled: true, Paths: []string{"docs", "/srv/docs"}}}}
			Expect(cfg.RAGPaths()).To(Equal([]string{"docs", "/srv/docs"}))
		})

		It("joins a relative entry under the root and leaves an absolute one alone", func() {
			cfg := &Config{
				RootDirectory: "/srv/agent",
				Harness:       HarnessConfig{RAG: &RAGConfig{Enabled: true, Paths: []string{"docs", "../shared", "/srv/docs"}}},
			}
			Expect(cfg.RAGPaths()).To(Equal([]string{"/srv/agent/docs", "/srv/shared", "/srv/docs"}))
		})

		It("does not write the joined paths back onto the configuration", func() {
			cfg := &Config{
				RootDirectory: "/srv/agent",
				Harness:       HarnessConfig{RAG: &RAGConfig{Enabled: true, Paths: []string{"docs"}}},
			}
			Expect(cfg.RAGPaths()).To(Equal([]string{"/srv/agent/docs"}))
			Expect(cfg.Harness.RAG.Paths).To(Equal([]string{"docs"}))
		})
	})
})
