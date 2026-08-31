// Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
// SPDX-License-Identifier: Apache-2.0

// These tests build a Config the way an embedder does, as a struct literal using
// only exported identifiers, and check that Prepare fills the derived fields the
// YAML path gets for free.
package config_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
)

var _ = Describe("Config.Prepare", func() {
	newLiteral := func() *config.Config {
		return &config.Config{
			ApplicationPath: "/usr/bin/abt",
			SystemPrompt:    "p",
			LLM: config.LLMConfig{
				Model:  "m",
				Budget: config.LLMBudget{CallTimeoutString: "45s"},
			},
			Harness: config.HarnessConfig{
				ToolTimeoutString: "90s",
				RAG: &config.RAGConfig{
					Enabled: true,
					Citations: []config.RAGCitationRule{
						{Pattern: `^docs/(.+)\.md$`, Replace: "https://example.net/$1"},
					},
					Embeddings: &config.RAGEmbeddingsConfig{
						BaseURL:       "http://127.0.0.1:1234/v1",
						Model:         "e",
						TimeoutString: "15s",
					},
				},
			},
			MCPClients: []config.MCPServer{
				{Name: "filesystem", Command: "/usr/bin/fs", TimeoutString: "20s"},
			},
			Expose: &config.ExposeConfig{
				Agent: &config.AgentExpose{
					Slack: &config.ExposedSlackConfig{AnswerGraceString: "25s"},
					A2A: &config.ExposedA2AConfig{
						ToolTimeoutString:    "30s",
						RequestTimeoutString: "35s",
					},
					MCP: &config.ExposedMCPConfig{ToolTimeoutString: "40s"},
				},
			},
		}
	}

	It("Should compile citation patterns and parse every duration string", func() {
		cfg := newLiteral()

		Expect(cfg.Harness.RAG.Citations[0].PatternCompiled).To(BeNil(), "a literal starts with nothing derived")
		Expect(cfg.Harness.ToolTimeoutParsed).To(BeZero())

		Expect(cfg.Prepare()).To(Succeed())

		rule := cfg.Harness.RAG.Citations[0]
		Expect(rule.PatternCompiled).ToNot(BeNil())
		Expect(rule.PatternCompiled.ReplaceAllString("docs/guide/intro.md", "https://example.net/$1")).To(Equal("https://example.net/guide/intro"))

		Expect(cfg.Harness.ToolTimeoutParsed).To(Equal(90 * time.Second))
		Expect(cfg.LLM.Budget.CallTimeoutParsed).To(Equal(45 * time.Second))
		Expect(cfg.Harness.RAG.Embeddings.TimeoutParsed).To(Equal(15 * time.Second))
		Expect(cfg.MCPClients[0].TimeoutParsed).To(Equal(20 * time.Second))
		Expect(cfg.Expose.Agent.Slack.AnswerGraceParsed).To(Equal(25 * time.Second))
		Expect(cfg.Expose.Agent.A2A.ToolTimeoutParsed).To(Equal(30 * time.Second))
		Expect(cfg.Expose.Agent.A2A.RequestTimeoutParsed).To(Equal(35 * time.Second))
		Expect(cfg.Expose.Agent.MCP.ToolTimeoutParsed).To(Equal(40 * time.Second))
	})

	It("Should derive the identity and apply budget defaults", func() {
		cfg := &config.Config{ApplicationPath: "/usr/bin/abt", SystemPrompt: "p", LLM: config.LLMConfig{Model: "m"}}

		Expect(cfg.Prepare()).To(Succeed())

		Expect(cfg.Identity).To(Equal("abt"))
		Expect(cfg.LLM.Budget.MaxTokens).To(Equal(int64(500000)))
		Expect(cfg.LLM.Budget.MaxIterations).To(Equal(int64(50)))
		Expect(cfg.LLM.Budget.CallTimeoutParsed).To(Equal(120 * time.Second))
		Expect(cfg.Harness.ToolTimeoutParsed).To(Equal(5 * time.Minute))
	})

	It("Should leave the same values behind when called twice", func() {
		once := newLiteral()
		Expect(once.Prepare()).To(Succeed())

		twice := newLiteral()
		Expect(twice.Prepare()).To(Succeed())
		Expect(twice.Prepare()).To(Succeed())

		Expect(twice.Identity).To(Equal(once.Identity))
		Expect(twice.Harness.ToolTimeoutParsed).To(Equal(once.Harness.ToolTimeoutParsed))
		Expect(twice.LLM.Budget).To(Equal(once.LLM.Budget))
		Expect(twice.MCPClients[0].TimeoutParsed).To(Equal(once.MCPClients[0].TimeoutParsed))
		Expect(twice.Expose.Agent.Slack.AnswerGraceParsed).To(Equal(once.Expose.Agent.Slack.AnswerGraceParsed))
		Expect(twice.Harness.RAG.Citations[0].PatternCompiled.String()).To(Equal(once.Harness.RAG.Citations[0].PatternCompiled.String()))
	})

	It("Should re-derive an identity it derived itself", func() {
		cfg, err := config.NewConfig()
		Expect(err).ToNot(HaveOccurred())
		Expect(cfg.Identity).To(Equal("fisk-ai"))
		Expect(cfg.IdentityIsNamed()).To(BeFalse())

		cfg.ApplicationPath = "/usr/bin/abt"
		Expect(cfg.Prepare()).To(Succeed())

		Expect(cfg.Identity).To(Equal("abt"))
		Expect(cfg.IdentityIsNamed()).To(BeFalse())
	})

	It("Should keep an identity ApplyIdentity chose", func() {
		cfg, err := config.NewConfig()
		Expect(err).ToNot(HaveOccurred())
		Expect(cfg.ApplyIdentity("orders")).To(Succeed())

		cfg.ApplicationPath = "/usr/bin/abt"
		Expect(cfg.Prepare()).To(Succeed())

		Expect(cfg.Identity).To(Equal("orders"))
		Expect(cfg.IdentityIsNamed()).To(BeTrue())
	})

	It("Should keep an identity assigned over one it derived", func() {
		cfg, err := config.NewConfig()
		Expect(err).ToNot(HaveOccurred())
		Expect(cfg.Identity).To(Equal("fisk-ai"))

		cfg.Identity = "orders"
		cfg.ApplicationPath = "/usr/bin/abt"
		Expect(cfg.Prepare()).To(Succeed())

		Expect(cfg.Identity).To(Equal("orders"))
		Expect(cfg.IdentityIsNamed()).To(BeTrue())

		Expect(cfg.Prepare()).To(Succeed())
		Expect(cfg.Identity).To(Equal("orders"))
		Expect(cfg.IdentityIsNamed()).To(BeTrue())
	})

	It("Should keep an identity set on the literal", func() {
		cfg := &config.Config{ApplicationPath: "/usr/bin/abt", Identity: "orders"}
		Expect(cfg.Prepare()).To(Succeed())

		Expect(cfg.Identity).To(Equal("orders"))
		Expect(cfg.IdentityIsNamed()).To(BeTrue())
	})

	It("Should report a citation rule that does not compile", func() {
		cfg := &config.Config{
			Harness: config.HarnessConfig{
				RAG: &config.RAGConfig{
					Enabled:   true,
					Citations: []config.RAGCitationRule{{Pattern: "([unclosed", Replace: "x"}},
				},
			},
		}

		Expect(cfg.Prepare()).To(MatchError(ContainSubstring("invalid harness.knowledge.citations[0] pattern")))
	})
})
