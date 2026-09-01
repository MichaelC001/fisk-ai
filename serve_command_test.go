//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/serve"
	"github.com/choria-io/fisk-ai/internal/telemetry"
)

// jobsConfig is a worker serving a queue, which is the shape most of the banner lines
// are written against.
func jobsConfig() *config.Config {
	cfg := &config.Config{Identity: "worker", NatsContext: "production"}
	cfg.LLM.Model = "claude-sonnet-5"
	cfg.Expose = &config.ExposeConfig{Agent: &config.AgentExpose{Jobs: &config.ExposedJobsConfig{}}}

	return cfg
}

// describedChannel is a channel this command was not written against, so what reaches
// the banner from it is whatever serve.DescribedEndpoint carries.
type describedChannel struct {
	name    string
	heading string
}

func (c *describedChannel) Name() string    { return c.name }
func (c *describedChannel) Heading() string { return c.heading }

func (c *describedChannel) Next(context.Context) (*serve.Work, error) {
	return nil, serve.ErrChannelDone
}

func (c *describedChannel) Describe() []serve.DescLine {
	return []serve.DescLine{{Label: "Address", Value: c.name + ".request"}}
}

// describedService describes itself and serves tools to peers, which is the pair of
// answers the a2a tool endpoint gives.
type describedService struct {
	name     string
	heading  string
	exposed  []string
	withheld []string
}

func (s *describedService) Name() string               { return s.name }
func (s *describedService) Heading() string            { return s.heading }
func (s *describedService) Close() error               { return nil }
func (s *describedService) ExposedTools() []string     { return s.exposed }
func (s *describedService) WithheldBuiltins() []string { return s.withheld }

func (s *describedService) Describe() []serve.DescLine {
	return []serve.DescLine{{Label: "Discovery", Value: s.name + ".discover"}}
}

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

	Describe("describeEndpoints", func() {
		// An endpoint states its own heading and lines, so a channel written outside this
		// repository reaches the banner without this command knowing its type.
		It("Should print a section for every endpoint that describes itself", func() {
			c := &fiskServeCommand{}
			res := &serve.Resources{SessionStore: agenttest.NewFakeSessionStore(GinkgoTB())}

			out := c.banner(jobsConfig(),
				[]serve.Channel{&describedChannel{name: "widgets", heading: "Answering widgets"}},
				nil, res, &telemetry.Provider{}).String()

			Expect(out).To(ContainSubstring("Answering widgets"))
			Expect(out).To(ContainSubstring("Address"))
			Expect(out).To(ContainSubstring("widgets.request"))
		})

		// A channel with nothing to say does not implement the interface, and a heading
		// with nothing under it would read as a banner that lost its lines.
		It("Should skip an endpoint that describes nothing", func() {
			c := &fiskServeCommand{}
			res := &serve.Resources{SessionStore: agenttest.NewFakeSessionStore(GinkgoTB())}

			out := c.banner(jobsConfig(),
				[]serve.Channel{agenttest.NewScriptedChannel(GinkgoTB(), "asyncjobs")},
				nil, res, &telemetry.Provider{}).String()

			Expect(out).To(ContainSubstring("asyncjobs"), "it is still named among the endpoints")
			Expect(out).ToNot(ContainSubstring("Answering"))
		})

		// The two lists are values under one label rather than a label and a value, so
		// they are asked for separately and printed inside the endpoint's own section.
		It("Should print the served and withheld tools of an endpoint that serves tools", func() {
			c := &fiskServeCommand{}
			res := &serve.Resources{SessionStore: agenttest.NewFakeSessionStore(GinkgoTB())}

			svc := &describedService{
				name:     "a2a",
				heading:  "Serving tools over a2a",
				exposed:  []string{"backup", "restore"},
				withheld: []string{"knowledge"},
			}

			out := c.banner(jobsConfig(), nil, []serve.Service{svc}, res, &telemetry.Provider{}).String()

			Expect(out).To(ContainSubstring("Serving tools over a2a"))
			Expect(out).To(ContainSubstring("backup"))
			Expect(out).To(ContainSubstring("restore"))
			Expect(out).To(ContainSubstring("knowledge"))
			Expect(out).To(ContainSubstring("declare no a2a exposure"))
		})
	})

	Describe("banner", func() {
		// Every one of these describes a run or the queue it comes off, and a worker
		// serving only tools has neither. Left in, the queue context would name a queue
		// that is not there and the tool directory one that served calls do not use.
		It("Should print the loop's lines only when a channel is hosted", func() {
			c := &fiskServeCommand{}
			res := &serve.Resources{SessionStore: agenttest.NewFakeSessionStore(GinkgoTB())}

			hosted := c.banner(jobsConfig(), []serve.Channel{agenttest.NewScriptedChannel(GinkgoTB(), "asyncjobs")}, nil, res, &telemetry.Provider{}).String()
			Expect(hosted).To(ContainSubstring("Model"))
			Expect(hosted).To(ContainSubstring("Queue Context"))
			Expect(hosted).To(ContainSubstring("Queue Workers"))
			Expect(hosted).To(ContainSubstring("asyncjobs"))

			cfg := &config.Config{Identity: "worker", NatsContext: "production"}
			cfg.Expose = &config.ExposeConfig{Agent: &config.AgentExpose{A2A: &config.ExposedA2AConfig{ServeTools: true}}}

			toolsOnly := c.banner(cfg, nil, []serve.Service{agenttest.NewService(GinkgoTB(), "a2a")}, res, &telemetry.Provider{}).String()
			Expect(toolsOnly).To(ContainSubstring("a2a"))
			Expect(toolsOnly).To(ContainSubstring("Agent Context"))
			Expect(toolsOnly).ToNot(ContainSubstring("Queue Context"))
			Expect(toolsOnly).ToNot(ContainSubstring("Workers"))
			Expect(toolsOnly).ToNot(ContainSubstring("Tool Directory"))
		})

		It("Should name every endpoint it hosts", func() {
			c := &fiskServeCommand{}
			res := &serve.Resources{SessionStore: agenttest.NewFakeSessionStore(GinkgoTB())}

			cfg := jobsConfig()
			cfg.Expose.Agent.A2A = &config.ExposedA2AConfig{ServeTools: true}

			out := c.banner(cfg,
				[]serve.Channel{agenttest.NewScriptedChannel(GinkgoTB(), "asyncjobs")},
				[]serve.Service{agenttest.NewService(GinkgoTB(), "a2a")},
				res, &telemetry.Provider{}).String()

			Expect(out).To(ContainSubstring("asyncjobs"))
			Expect(out).To(ContainSubstring("a2a"))
		})
	})

	Describe("noEndpointError", func() {
		// A key name on its own is not enough to work out what goes under it, so the
		// message carries the blocks that fix it and the defaults they imply.
		It("Should name the file and show the blocks that fix it", func() {
			c := &fiskServeCommand{configFile: "worker.yaml"}
			err := c.noEndpointError()

			Expect(err).To(MatchError(ContainSubstring("worker.yaml")))
			Expect(err).To(MatchError(ContainSubstring("expose:")))
			Expect(err).To(MatchError(ContainSubstring("jobs: {}")))
			Expect(err).To(MatchError(ContainSubstring("serve_tools: true")))
			Expect(err).To(MatchError(ContainSubstring("prompts: {}")))
			Expect(err).To(MatchError(ContainSubstring(config.DefaultJobsQueue)))
			Expect(err).To(MatchError(ContainSubstring(config.DefaultJobsTaskType)))
		})
	})
})
