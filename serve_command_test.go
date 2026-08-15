//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/serve"
)

// The banner is an operator's only view of what a worker resolved before the log takes
// over, so what each line reports is the contract these cover.
var _ = Describe("fiskServeCommand", func() {
	Describe("workerOverride", func() {
		// The flag has a default, so its value alone cannot say whether it was typed.
		// Reading the value whenever it is non-zero would have the default silently beat
		// a configured count.
		It("Should be zero unless the flag was used", func() {
			c := &fiskServeCommand{workers: 8}
			Expect(c.workerOverride()).To(Equal(0), "a value nobody typed leaves the config alone")

			c.workersSet = true
			Expect(c.workerOverride()).To(Equal(8))
		})
	})

	Describe("workersDescription", func() {
		It("Should attribute the count to whichever source supplied it", func() {
			cfg := &config.Config{Expose: &config.ExposeConfig{
				Agent: &config.AgentExpose{Jobs: &config.ExposedJobsConfig{Workers: 3}},
			}}

			c := &fiskServeCommand{}
			Expect(c.workersDescription(cfg)).To(Equal("3 (config)"))

			c.workers = 9
			c.workersSet = true
			Expect(c.workersDescription(cfg)).To(Equal("9 (--workers)"))
		})
	})

	Describe("toolTimeout", func() {
		// Reporting the configured value alone would print zero for a worker that in
		// fact bounds every call, which is the opposite of what the line is for.
		It("Should report the bound a run will actually get", func() {
			c := &fiskServeCommand{}

			Expect(c.toolTimeout(&config.Config{})).To(Equal(serve.DefaultToolTimeout))

			cfg := &config.Config{}
			cfg.Harness.ToolTimeoutParsed = 90 * time.Second
			Expect(c.toolTimeout(cfg)).To(Equal(90 * time.Second))
		})
	})

	Describe("toolDirectory", func() {
		It("Should name the process directory unless the operator moved it", func() {
			wd, err := os.Getwd()
			Expect(err).ToNot(HaveOccurred())

			c := &fiskServeCommand{}
			Expect(c.toolDirectory()).To(Equal(wd))

			c.workDir = "/var/lib/fisk-ai"
			Expect(c.toolDirectory()).To(Equal("/var/lib/fisk-ai"))
		})
	})

	Describe("knowledgeDescription", func() {
		// A configuration can enable knowledge with nothing indexed, and the tool then
		// answers every search "not built" rather than failing. Nobody watches a worker,
		// so the difference has to be visible at startup.
		It("Should separate disabled from enabled with no index", func() {
			c := &fiskServeCommand{}

			Expect(c.knowledgeDescription(&config.Config{}, &serve.Resources{})).To(Equal("disabled"))

			cfg := &config.Config{}
			cfg.Harness.RAG = &config.RAGConfig{Enabled: true}
			Expect(c.knowledgeDescription(cfg, &serve.Resources{})).To(Equal("enabled, no index built yet"))
		})
	})

	Describe("noSurfaceError", func() {
		// A key name on its own is not enough to work out what goes under it, so the
		// message carries the block that fixes it and the defaults it implies.
		It("Should name the file and show the block that fixes it", func() {
			c := &fiskServeCommand{configFile: "worker.yaml"}
			err := c.noSurfaceError()

			Expect(err).To(MatchError(ContainSubstring("worker.yaml")))
			Expect(err).To(MatchError(ContainSubstring("expose:")))
			Expect(err).To(MatchError(ContainSubstring("jobs: {}")))
			Expect(err).To(MatchError(ContainSubstring(config.DefaultJobsQueue)))
			Expect(err).To(MatchError(ContainSubstring(config.DefaultJobsTaskType)))
		})
	})
})
