//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
)

var _ = Describe("validateRunFlags", func() {
	// The run flags are package globals; reset the ones this suite touches after each
	// case so cases stay independent.
	AfterEach(func() {
		resumeID = ""
		forceResume = false
		q = nil
		runIdentity = ""
		runNatsContext = ""
	})

	// Every run is journaled now, so there is no flag to combine with and nothing to
	// refuse: what a person asks for is a conversation, and they always get one.
	It("Should accept a plain run", func() {
		q = []string{"tell me a joke"}
		Expect(validateRunFlags()).To(Succeed())
	})

	It("Should refuse a prompt beside --resume, which restores its own", func() {
		resumeID = "abc"
		q = []string{"and the second one"}
		Expect(validateRunFlags()).To(MatchError(ContainSubstring("--resume does not take a query")))
	})

	It("Should refuse --force without something to resume", func() {
		forceResume = true
		Expect(validateRunFlags()).To(MatchError(ContainSubstring("--force only applies when resuming")))
	})

	It("Should accept a resume on its own", func() {
		resumeID = "abc"
		Expect(validateRunFlags()).To(Succeed())
	})

	// An agent hosted here takes its identity from the configuration, which is also what
	// names where its conversations are stored, so a flag that replaced it would put them
	// out of reach.
	It("Should refuse --identity without an agent elsewhere to address", func() {
		runIdentity = "billing"
		Expect(validateRunFlags()).To(MatchError(ContainSubstring("--identity only applies with --nats-context")))
	})

	It("Should accept --identity beside --nats-context", func() {
		runIdentity = "billing"
		runNatsContext = "ngs_user"
		Expect(validateRunFlags()).To(Succeed())
	})
})

var _ = Describe("loadRunConfig", func() {
	var dir string

	BeforeEach(func() {
		dir = GinkgoT().TempDir()
		configFile = filepath.Join(dir, "agent.yaml")
		setConfigFile = false
		runIdentity = ""
	})

	AfterEach(func() {
		configFile = "agent.yaml"
		setConfigFile = false
		runIdentity = ""
	})

	// The worker holds the model and the prompt, so a terminal is not made to supply
	// either to talk to one.
	It("Should read a file that describes no agent of its own", func() {
		Expect(os.WriteFile(configFile, []byte("identity: billing\n"), 0600)).To(Succeed())

		cfg, err := loadRunConfig(true)
		Expect(err).ToNot(HaveOccurred())
		Expect(cfg.Identity).To(Equal("billing"))

		_, err = loadRunConfig(false)
		Expect(err).To(MatchError(ContainSubstring("prompt is required")), "hosting one still needs all of it")
	})

	It("Should need no file at all when the agent is named on the command line", func() {
		runIdentity = "billing"

		cfg, err := loadRunConfig(true)
		Expect(err).ToNot(HaveOccurred())
		Expect(cfg.Identity).To(Equal("billing"))
		Expect(cfg.IdentityIsNamed()).To(BeTrue(), "so the target validation accepts it")
	})

	// Whichever agent.yaml the working directory happened to hold would otherwise supply
	// this run's timeouts and its model, and could refuse it over a field this terminal
	// never reads.
	It("Should leave a file nobody named unread", func() {
		Expect(os.WriteFile(configFile, []byte("identity: orders\nnonsense: true\n"), 0600)).To(Succeed())
		runIdentity = "billing"

		cfg, err := loadRunConfig(true)
		Expect(err).ToNot(HaveOccurred())
		Expect(cfg.Identity).To(Equal("billing"))
	})

	It("Should read a file somebody named, and let the flag win", func() {
		Expect(os.WriteFile(configFile, []byte("identity: orders\nharness:\n  no_bell: true\n"), 0600)).To(Succeed())
		runIdentity = "billing"
		setConfigFile = true

		cfg, err := loadRunConfig(true)
		Expect(err).ToNot(HaveOccurred())
		Expect(cfg.Identity).To(Equal("billing"))
		Expect(cfg.BellEnabled()).To(BeFalse(), "the rest of the file still applies")
	})

	It("Should refuse a file somebody named that is not there", func() {
		runIdentity = "billing"
		setConfigFile = true

		_, err := loadRunConfig(true)
		Expect(err).To(MatchError(ContainSubstring("reading config")))
		Expect(err).ToNot(MatchError(ContainSubstring("--identity")), "the path they gave is what to fix")
	})

	// The likeliest first attempt at this: a context, no identity and no file in the
	// directory. All the run wanted from that file was a name.
	It("Should name the flag when the default file is missing", func() {
		_, err := loadRunConfig(true)
		Expect(err).To(MatchError(ContainSubstring("reading config")))
		Expect(err).To(MatchError(ContainSubstring("pass --identity NAME")))
	})

	It("Should refuse an identity that is not a legal name", func() {
		runIdentity = "billing.eu"

		_, err := loadRunConfig(true)
		Expect(err).To(MatchError(ContainSubstring("is invalid")))
	})
})

var _ = Describe("validateRunTarget", func() {
	var cfg *config.Config

	BeforeEach(func() {
		// An application path so the config supplies a tool. These specs are about the
		// credential and identity rules, and a config with nothing to call is refused
		// before either is reached.
		cfg = &config.Config{Identity: "worker1", ApplicationPath: "/usr/bin/abt"}
		apiKey = ""
		setAPIKey = false
		setBaseURL = false
		setTraceFile = false
		setHTTPDebug = false
		setVerbose = false
		setStateDir = false
		setNoTelemetry = false
	})

	AfterEach(func() {
		apiKey = ""
		setVerbose = false
		setStateDir = false
	})

	Describe("Hosting the agent here", func() {
		It("Should require a key, since the model is called from this process", func() {
			err := validateRunTarget(cfg, false)
			Expect(err).To(MatchError(ContainSubstring("--api-key is required to run an agent in this process")))

			apiKey = "sk-test"
			Expect(validateRunTarget(cfg, false)).To(Succeed())
		})

		// This command injects no tools, so a config naming none is refused here rather
		// than after the stores and the debug files have been opened.
		It("Should refuse a config that supplies no tools", func() {
			apiKey = "sk-test"
			cfg.ApplicationPath = ""

			err := validateRunTarget(cfg, false)
			Expect(err).To(MatchError(ContainSubstring("no tools available")))
			Expect(err).To(MatchError(ContainSubstring("harness.memory")))

			cfg.Harness.Memory = &config.MemoryConfig{Enabled: true}
			Expect(validateRunTarget(cfg, false)).To(Succeed())
		})

		// A local run opens no bus, so the identity is only part of what names its
		// journals and a derived one costs nothing.
		It("Should not ask for a named identity", func() {
			apiKey = "sk-test"
			derived := &config.Config{ApplicationPath: "/usr/bin/abt"}

			Expect(derived.IdentityIsNamed()).To(BeFalse())
			Expect(validateRunTarget(derived, false)).To(Succeed())
		})
	})

	Describe("Talking to a worker elsewhere", func() {
		It("Should need no key, since the worker holds one", func() {
			Expect(validateRunTarget(cfg, true)).To(Succeed())
		})

		// The transport queue groups on the identity, so an accidental name joins
		// somebody else's fleet rather than failing to resolve.
		It("Should refuse a configuration that names no agent", func() {
			err := validateRunTarget(&config.Config{}, true)
			Expect(err).To(MatchError(ContainSubstring("needs an agent to address")))
			Expect(err).To(MatchError(ContainSubstring("--identity")), "the escape that needs no file")
		})

		It("Should refuse the flags that describe the worker's own work", func() {
			for _, tc := range []struct {
				set  *bool
				flag string
			}{
				{&setAPIKey, "--api-key"},
				{&setBaseURL, "--base-url"},
				{&setTraceFile, "--trace"},
				{&setHTTPDebug, "--http-debug"},
				{&setVerbose, "--verbose"},
				{&setStateDir, "--state-dir"},
				{&setNoTelemetry, "--no-telemetry"},
			} {
				*tc.set = true
				err := validateRunTarget(cfg, true)
				*tc.set = false

				Expect(err).To(MatchError(ContainSubstring(tc.flag)), tc.flag)
				Expect(err).To(MatchError(ContainSubstring("drop --nats-context")), tc.flag)
			}
		})

		// The refusal is on the typing, not on the value: several of these carry
		// environment variables, and one exported in a shell profile is not a request.
		It("Should ignore a value that arrived from the environment", func() {
			verbose = true
			noTelemetry = true
			stateDirFlag = "/tmp/somewhere"
			DeferCleanup(func() {
				verbose = false
				noTelemetry = false
				stateDirFlag = ""
			})

			Expect(validateRunTarget(cfg, true)).To(Succeed())
		})
	})
})
