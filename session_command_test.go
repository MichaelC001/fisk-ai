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

	"github.com/choria-io/ui/columns"

	"github.com/choria-io/fisk-ai/internal/runstate"
)

var _ = Describe("printSessionMeta", func() {
	// The page an operator opens to hand a conversation back. Both values are the
	// caller's own strings, so what is asserted is that they reach the page and that
	// the token says what a caller does with it.
	It("Should show the caller and the conversation token when the journal holds them", func() {
		c := columns.New()
		printSessionMeta(c, &runstate.RunState{
			RunID:             "t-e3b0c44298fc1c149afbf4c8996fb924",
			Prompt:            "how many streams are there",
			ConversationToken: "3Hzmp8VqrKL42NmXcPd7bTgWfR1",
			Caller:            "peer1",
		})

		// The label and the value are asserted apart, since columns renders Markdown
		// rather than text where the environment asks for it and only the value is the
		// same in both.
		out := c.String()
		Expect(out).To(ContainSubstring("Caller:"))
		Expect(out).To(ContainSubstring("peer1"))
		Expect(out).To(ContainSubstring("Conversation token:"))
		Expect(out).To(ContainSubstring("3Hzmp8VqrKL42NmXcPd7bTgWfR1 (a caller sends this to continue the conversation)"))
	})

	It("Should show neither for a run that has neither", func() {
		c := columns.New()
		printSessionMeta(c, &runstate.RunState{RunID: "2ZqL", Prompt: "list the streams"})

		out := c.String()
		Expect(out).ToNot(ContainSubstring("Caller"))
		Expect(out).ToNot(ContainSubstring("Conversation token"))
	})
})

var _ = Describe("openSessionStore", func() {
	// The session flags are package globals; snapshot and restore the ones these cases
	// touch so they stay independent and never leak a --config or --state-dir.
	var origConfig, origStateDir string

	BeforeEach(func() {
		origConfig = sessionConfigFile
		origStateDir = stateDirFlag
	})

	AfterEach(func() {
		sessionConfigFile = origConfig
		stateDirFlag = origStateDir
	})

	It("Should open the file backend under --state-dir with no config and no NATS", func() {
		sessionConfigFile = ""
		stateDirFlag = GinkgoT().TempDir()

		// No NATS server runs in the unit suite: that this succeeds proves the file
		// backend path never dialed one.
		store, cleanup, err := openSessionStore()
		Expect(err).ToNot(HaveOccurred())
		Expect(store).ToNot(BeNil())
		defer cleanup()

		infos, err := store.List(context.Background())
		Expect(err).ToNot(HaveOccurred())
		Expect(infos).To(BeEmpty())
	})

	It("Should surface a bad file backend options block at store construction", func() {
		dir := GinkgoT().TempDir()
		cfgPath := filepath.Join(dir, "agent.yaml")
		// directory must be a string; a number is captured raw by the strict config
		// decode (options is an opaque block) and rejected only when the file backend
		// decodes it at construction, which is what this asserts.
		err := os.WriteFile(cfgPath, []byte(`
llm:
  model: claude-sonnet-4-6
harness:
  sessions:
    backend: file
    options:
      directory: 123
`), 0o600)
		Expect(err).ToNot(HaveOccurred())

		sessionConfigFile = cfgPath
		stateDirFlag = ""

		store, cleanup, err := openSessionStore()
		defer cleanup()
		Expect(err).To(HaveOccurred())
		Expect(store).To(BeNil())
		Expect(err.Error()).To(ContainSubstring("file session options"))
	})

	It("Should hard error on --state-dir combined with a non-file configured backend", func() {
		dir := GinkgoT().TempDir()
		cfgPath := filepath.Join(dir, "agent.yaml")
		err := os.WriteFile(cfgPath, []byte(`
llm:
  model: claude-sonnet-4-6
harness:
  sessions:
    backend: jetstream
    options:
      stream: FISK_SESSIONS
`), 0o600)
		Expect(err).ToNot(HaveOccurred())

		sessionConfigFile = cfgPath
		stateDirFlag = "/tmp/runs"

		store, cleanup, err := openSessionStore()
		defer cleanup()
		Expect(err).To(HaveOccurred())
		Expect(store).To(BeNil())
		Expect(err.Error()).To(ContainSubstring("--state-dir applies only to the file session backend"))
	})
})
