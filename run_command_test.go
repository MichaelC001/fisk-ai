//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
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
})

var _ = Describe("validateRunTarget", func() {
	var cfg *config.Config

	BeforeEach(func() {
		cfg = &config.Config{Identity: "worker1"}
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

		// A local run opens no bus, so the identity is only part of what names its
		// journals and a derived one costs nothing.
		It("Should not ask for a named identity", func() {
			apiKey = "sk-test"
			derived := &config.Config{}

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
			Expect(err).To(MatchError(ContainSubstring("names none")))
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
