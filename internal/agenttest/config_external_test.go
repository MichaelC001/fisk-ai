//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agenttest_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/agenttest"
)

var _ = Describe("Config", func() {
	It("Should set the fields a run needs and leave the rest zero", func() {
		cfg := agenttest.BuildConfig(nil)

		Expect(cfg.Identity).To(Equal("agent"))
		Expect(cfg.LLM.Model).To(Equal("test-model"))
		Expect(cfg.LLM.Budget.MaxIterations).To(Equal(int64(20)))
		Expect(cfg.Harness.PII.Mode).To(Equal(config.PIIModeOff))
		Expect(cfg.Harness.RAG).To(BeNil())
		Expect(cfg.Harness.Memory).To(BeNil())
		Expect(cfg.Harness.HumanInTheLoop).To(BeNil())
	})

	It("Should leave ApplicationPath empty for a run that wraps no application", func() {
		Expect(agenttest.BuildConfig(nil).ApplicationPath).To(BeEmpty())
	})

	It("Should point ApplicationPath at the fake application", func() {
		app := &agenttest.FakeApp{Path: "/tmp/fake/app"}

		Expect(agenttest.BuildConfig(app).ApplicationPath).To(Equal("/tmp/fake/app"))
	})

	It("Should return a fresh config each call so concurrent runs share no pointer", func() {
		first := agenttest.Config(GinkgoTB(), nil)
		second := agenttest.Config(GinkgoTB(), nil)

		Expect(first).ToNot(BeIdenticalTo(second))

		first.LLM.Model = "rewritten"
		Expect(second.LLM.Model).To(Equal("test-model"))
	})

	It("Should apply each option", func() {
		cfg := agenttest.BuildConfig(nil,
			agenttest.WithMaxIterations(3),
			agenttest.WithMaxTokens(1000),
			agenttest.WithToolTimeout(2*time.Second),
			agenttest.WithPII(config.PIIModeRedact),
			agenttest.WithRAG(),
			agenttest.WithMemory(),
			agenttest.WithHITL())

		Expect(cfg.LLM.Budget.MaxIterations).To(Equal(int64(3)))
		Expect(cfg.LLM.Budget.MaxTokens).To(Equal(int64(1000)))
		Expect(cfg.Harness.ToolTimeoutParsed).To(Equal(2 * time.Second))
		Expect(cfg.Harness.PII.Mode).To(Equal(config.PIIModeRedact))
		Expect(cfg.Harness.RAG.Enabled).To(BeTrue())
		Expect(cfg.Harness.Memory.Enabled).To(BeTrue())
		Expect(cfg.Harness.HumanInTheLoop.Enabled).To(BeTrue())
	})
})
