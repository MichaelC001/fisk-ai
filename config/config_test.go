// Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	// Imported by the test binary only, to assert this package's credential list stays
	// a superset of the one internal/telemetry checks. The config package itself
	// imports nothing from the tree and must stay that way.
	"github.com/choria-io/fisk-ai/internal/telemetry"
)

func TestConfig(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Config")
}

// minimalAgentConfig is the smallest config that validates in agent mode, for specs
// about one block that need the rest of the file to be valid and uninteresting. It
// ends in a newline so a block under test appends cleanly.
const minimalAgentConfig = `
identity: agent1
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
`

var _ = Describe("Config", func() {
	Describe("NewConfig", func() {
		It("Should apply LLM budget defaults", func() {
			cfg, err := NewConfig()
			Expect(err).ToNot(HaveOccurred())
			Expect(cfg).NotTo(BeNil())
			Expect(cfg.LLM.Budget.MaxTokens).To(Equal(int64(defaultLLMMaxTokens)))
			Expect(cfg.LLM.Budget.MaxIterations).To(Equal(int64(defaultLLMMaxIterations)))
			Expect(cfg.LLM.Budget.CallTimeoutParsed).To(Equal(defaultLLMCallTimeout))
		})
	})

	Describe("ParseConfigFile", func() {
		It("Should return an error when the file does not exist", func() {
			cfg, err := ParseConfigFile(filepath.Join(GinkgoT().TempDir(), "missing.yaml"))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("reading config"))
			Expect(cfg).To(BeNil())
		})

		It("Should read and parse a config from disk", func() {
			path := filepath.Join(GinkgoT().TempDir(), "config.yaml")
			Expect(os.WriteFile(path, []byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
`), 0o600)).To(Succeed())

			cfg, err := ParseConfigFile(path)
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.Identity).To(Equal("agent1"))
		})
	})

	Describe("ParseConfig", func() {
		It("Should return an error for invalid YAML", func() {
			cfg, err := ParseConfig([]byte("identity: [unterminated"))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("parsing config"))
			Expect(cfg).To(BeNil())
		})

		It("Should parse a minimal config and apply defaults", func() {
			cfg, err := ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
`))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.Identity).To(Equal("agent1"))
			Expect(cfg.ApplicationPath).To(Equal("/usr/bin/nats"))
			Expect(cfg.SystemPrompt).To(Equal("do the thing"))
			Expect(cfg.LLM.Model).To(Equal("claude-sonnet-4-6"))

			Expect(cfg.LLM.Budget.MaxTokens).To(Equal(int64(defaultLLMMaxTokens)))
			Expect(cfg.LLM.Budget.MaxIterations).To(Equal(int64(defaultLLMMaxIterations)))
			Expect(cfg.LLM.Budget.CallTimeoutParsed).To(Equal(defaultLLMCallTimeout))
		})

		It("Should parse all fields and durations", func() {
			cfg, err := ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
  budget:
    max_tokens: 1000
    max_iterations: 5
    call_timeout: 90s
exclude:
  tools:
    - "nats auth.*"
  tags:
    - admin
include:
  tags:
    - ""
nats_context: ngs
remote_agents:
  - name: remote1
    alias: r1
remote_tools:
  - name: host1
    alias: h1
    exclude:
      tools:
        - secret
expose:
  agent:
    a2a:
      serve_tools: true
    mcp:
      port: 8080
    tools:
      include:
        tags:
          - public
`))
			Expect(err).NotTo(HaveOccurred())

			Expect(cfg.LLM.Budget.MaxTokens).To(Equal(int64(1000)))
			Expect(cfg.LLM.Budget.MaxIterations).To(Equal(int64(5)))
			Expect(cfg.LLM.Budget.CallTimeoutParsed).To(Equal(90 * time.Second))

			Expect(cfg.Exclude.Tools).To(Equal([]string{"nats auth.*"}))
			Expect(cfg.Exclude.Tags).To(Equal([]string{"admin"}))

			Expect(cfg.Include.Tags).To(Equal([]string{""}))

			Expect(cfg.RemoteAgents).To(HaveLen(1))
			Expect(cfg.RemoteAgents[0].Name).To(Equal("remote1"))
			Expect(cfg.RemoteAgents[0].Alias).To(Equal("r1"))

			Expect(cfg.RemoteTools).To(HaveLen(1))
			Expect(cfg.NatsContext).To(Equal("ngs"))
			Expect(cfg.RemoteTools[0].Name).To(Equal("host1"))
			Expect(cfg.RemoteTools[0].Exclude.Tools).To(Equal([]string{"secret"}))

			Expect(cfg.Expose.Agent.A2A.ServeTools).To(BeTrue())
			Expect(cfg.Expose.Agent.MCP.Port).To(Equal(8080))
			Expect(cfg.Expose.Agent.Tools.Include.Tags).To(Equal([]string{"public"}))
		})

		It("Should reject unknown keys, including harness settings left at the top level", func() {
			_, err := ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
human_in_the_loop:
  enabled: true
llm:
  model: claude-sonnet-4-6
`))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("human_in_the_loop"))

			_, err = ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
no_such_key: true
llm:
  model: claude-sonnet-4-6
`))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("no_such_key"))
		})

		It("Should normalize confirm_tags by trimming, dropping empties and deduping", func() {
			cfg, err := ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
harness:
  confirm_tags:
    - "impact:rw"
    - " impact:rw "
    - ""
    - "   "
    - "admin"
`))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.ConfirmTags()).To(Equal([]string{"impact:rw", "admin"}))
		})

		It("Should normalize global_flags, stripping dashes and de-duplicating", func() {
			cfg, err := ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
global_flags:
  - "--context"
  - "context"
  - " server "
  - ""
`))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.GlobalFlagNames()).To(Equal([]string{"context", "server"}))
		})

		It("Should normalize confirm_over_mcp case and whitespace", func() {
			cfg, err := ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
expose:
  agent:
    mcp:
      port: 8080
      confirm_over_mcp: "  Always  "
`))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.ConfirmOverMCPMode()).To(Equal("always"))
		})

		It("Should default confirm_over_mcp to auto when unset", func() {
			cfg, err := ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
expose:
  agent:
    mcp:
      port: 8080
`))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.ConfirmOverMCPMode()).To(Equal("auto"))
		})

		It("Should reject an invalid confirm_over_mcp value", func() {
			_, err := ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
expose:
  agent:
    mcp:
      port: 8080
      confirm_over_mcp: sometimes
`))
			Expect(err).To(MatchError(ContainSubstring("invalid confirm_over_mcp")))
		})

		It("Should default harness.pii.mode to redact when the block is absent", func() {
			cfg, err := ParseConfig([]byte(minimalAgentConfig))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.PIIMode()).To(Equal(PIIModeRedact))
		})

		It("Should default harness.pii.mode to redact when the block is present but empty", func() {
			cfg, err := ParseConfig([]byte(minimalAgentConfig + `
harness:
  pii: {}
`))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.PIIMode()).To(Equal(PIIModeRedact))
		})

		It("Should read every harness.pii.mode value", func() {
			for _, mode := range []string{PIIModeRedact, PIIModeReject, PIIModeOff} {
				cfg, err := ParseConfig([]byte(minimalAgentConfig + `
harness:
  pii:
    mode: ` + mode + `
`))
				Expect(err).NotTo(HaveOccurred())
				Expect(cfg.PIIMode()).To(Equal(mode))
			}
		})

		It("Should normalize harness.pii.mode case and whitespace", func() {
			cfg, err := ParseConfig([]byte(minimalAgentConfig + `
harness:
  pii:
    mode: "  Reject  "
`))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.PIIMode()).To(Equal(PIIModeReject))
		})

		It("Should reject an invalid harness.pii.mode value", func() {
			_, err := ParseConfig([]byte(minimalAgentConfig + `
harness:
  pii:
    mode: disabled
`))
			Expect(err).To(MatchError(ContainSubstring("invalid harness.pii.mode")))
		})

		It("Should parse the MCP and a2a per-server tool limits", func() {
			cfg, err := ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
expose:
  agent:
    mcp:
      port: 8080
      max_concurrent_tools: 8
      tool_timeout: 45s
    a2a:
      serve_tools: true
      max_concurrent_tools: 4
      tool_timeout: 90s
`))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.MCPMaxConcurrentTools()).To(Equal(8))
			Expect(cfg.MCPToolTimeout()).To(Equal(45 * time.Second))
			Expect(cfg.A2AMaxConcurrentTools()).To(Equal(4))
			Expect(cfg.A2AToolTimeout()).To(Equal(90 * time.Second))
		})

		It("Should leave the tool limits at zero when unset, for the server to default", func() {
			cfg, err := ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
expose:
  agent:
    mcp:
      port: 8080
`))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.MCPMaxConcurrentTools()).To(Equal(0))
			Expect(cfg.MCPToolTimeout()).To(Equal(time.Duration(0)))
		})

		It("Should parse the harness tool timeout", func() {
			cfg, err := ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
harness:
  tool_timeout: 5m
`))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.ToolTimeout()).To(Equal(5 * time.Minute))
		})

		It("Should default the harness tool timeout when unset, and take 0s as unbounded", func() {
			base := `
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
`
			cfg, err := ParseConfig([]byte(base))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.ToolTimeout()).To(Equal(5 * time.Minute))

			// 0s is how an operator asks for no bound, which unset used to mean too.
			cfg, err = ParseConfig([]byte(base + "harness:\n  tool_timeout: 0s\n"))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.ToolTimeout()).To(Equal(time.Duration(0)))
		})

		It("Should accept the extended duration units fisk parses on every duration key", func() {
			cfg, err := ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
  budget:
    call_timeout: 2m
harness:
  tool_timeout: 1d
  knowledge:
    enabled: true
    embeddings:
      timeout: 1h
expose:
  agent:
    a2a:
      serve_tools: true
      tool_timeout: 1d
      request_timeout: 1h
`))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.ToolTimeout()).To(Equal(24 * time.Hour))
			Expect(cfg.A2AToolTimeout()).To(Equal(24 * time.Hour))
			Expect(cfg.A2ARequestTimeout()).To(Equal(time.Hour))
			Expect(cfg.LLM.Budget.CallTimeoutParsed).To(Equal(2 * time.Minute))
			Expect(cfg.Harness.RAG.Embeddings.TimeoutParsed).To(Equal(time.Hour))
		})

		It("Should reject an unparsable or negative harness tool timeout", func() {
			base := `
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
harness:
  tool_timeout: `

			_, err := ParseConfig([]byte(base + "soon\n"))
			Expect(err).To(MatchError(ContainSubstring(`invalid harness.tool_timeout "soon"`)))

			_, err = ParseConfig([]byte(base + "-5s\n"))
			Expect(err).To(MatchError(ContainSubstring("must not be negative")))
		})

		It("Should accept a zero max_concurrent_tools as unset rather than rejecting it", func() {
			cfg, err := ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
expose:
  agent:
    mcp:
      port: 8080
      max_concurrent_tools: 0
`))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.MCPMaxConcurrentTools()).To(Equal(0))
		})

		It("Should reject a negative max_concurrent_tools, naming the path", func() {
			_, err := ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
expose:
  agent:
    mcp:
      port: 8080
      max_concurrent_tools: -1
`))
			Expect(err).To(MatchError(ContainSubstring("invalid expose.agent.mcp.max_concurrent_tools")))
			Expect(err).To(MatchError(ContainSubstring("must not be negative")))
		})

		It("Should reject a max_concurrent_tools above the ceiling", func() {
			_, err := ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
expose:
  agent:
    mcp:
      port: 8080
      max_concurrent_tools: 100000
`))
			Expect(err).To(MatchError(ContainSubstring("must not exceed")))
		})

		It("Should reject an invalid a2a tool_timeout, naming the path", func() {
			_, err := ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
expose:
  agent:
    a2a:
      serve_tools: true
      tool_timeout: not-a-duration
`))
			Expect(err).To(MatchError(ContainSubstring("invalid expose.agent.a2a.tool_timeout")))
		})

		It("Should parse the a2a request timeout and default it when unset", func() {
			base := `
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
nats_context: peers
llm:
  model: claude-sonnet-4-6
`
			cfg, err := ParseConfig([]byte(base))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.A2ARequestTimeout()).To(Equal(120 * time.Second))

			cfg, err = ParseConfig([]byte(base + "expose:\n  agent:\n    a2a:\n      serve_tools: true\n      request_timeout: 45s\n"))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.A2ARequestTimeout()).To(Equal(45 * time.Second))
		})

		It("Should accept an a2a block carrying only a request timeout, since a caller exposes nothing", func() {
			cfg, err := ParseConfigForMode([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
nats_context: peers
llm:
  model: claude-sonnet-4-6
remote_tools:
  - name: dbagent
expose:
  agent:
    a2a:
      request_timeout: 30s
`), ModeServe)
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.A2ARequestTimeout()).To(Equal(30 * time.Second))
			Expect(cfg.A2AEnabled()).To(BeFalse())
		})

		It("Should reject an a2a block that sets neither an endpoint nor a request timeout", func() {
			_, err := ParseConfigForMode([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
nats_context: peers
llm:
  model: claude-sonnet-4-6
expose:
  agent:
    a2a:
      tool_timeout: 30s
`), ModeServe)
			Expect(err).To(MatchError(ContainSubstring("expose.agent.a2a enables nothing")))
		})

		It("Should reject an unparsable, zero or negative a2a request timeout", func() {
			base := `
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
nats_context: peers
llm:
  model: claude-sonnet-4-6
expose:
  agent:
    a2a:
      serve_tools: true
      request_timeout: `

			_, err := ParseConfig([]byte(base + "soon\n"))
			Expect(err).To(MatchError(ContainSubstring(`invalid expose.agent.a2a.request_timeout "soon"`)))

			// 0s reads as unbounded on harness.tool_timeout and cannot mean that here: a
			// transport handed zero applies a shorter bound of its own.
			_, err = ParseConfig([]byte(base + "0s\n"))
			Expect(err).To(MatchError(ContainSubstring("must be greater than zero")))

			_, err = ParseConfig([]byte(base + "-5s\n"))
			Expect(err).To(MatchError(ContainSubstring("must be greater than zero")))
		})

		It("Should reject a zero or negative llm call_timeout", func() {
			base := `
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
  budget:
    call_timeout: `

			_, err := ParseConfig([]byte(base + "0s\n"))
			Expect(err).To(MatchError(ContainSubstring("must be greater than zero")))

			_, err = ParseConfig([]byte(base + "-5s\n"))
			Expect(err).To(MatchError(ContainSubstring("must be greater than zero")))
		})

		It("Should return an error for an invalid llm call_timeout", func() {
			cfg, err := ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
  budget:
    call_timeout: not-a-duration
`))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("invalid llm call_timeout"))
			Expect(cfg).To(BeNil())
		})

		It("Should derive identity from the application_path base name when unset", func() {
			cfg, err := ParseConfig([]byte(`
application_path: /usr/bin/nats
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
`))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.Identity).To(Equal("nats"))
		})

		It("Should keep an explicit identity over the derived one", func() {
			cfg, err := ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
`))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.Identity).To(Equal("agent1"))
		})

		It("Should reject a derived identity whose basename carries illegal characters", func() {
			_, err := ParseConfig([]byte(`
application_path: /usr/bin/my.agent
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
`))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("is invalid"))
		})

		It("Should accept an agent config without application_path and default the identity", func() {
			cfg, err := ParseConfig([]byte(`
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
`))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.ApplicationPath).To(BeEmpty())
			Expect(cfg.Identity).To(Equal("fisk-ai"))
		})

		It("Should keep an explicit identity when application_path is unset", func() {
			cfg, err := ParseConfig([]byte(`
identity: agent1
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
`))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.Identity).To(Equal("agent1"))
		})

		It("Should return a validation error when llm.model is missing", func() {
			cfg, err := ParseConfig([]byte(`
identity: agent1
system_prompt: do the thing
`))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("llm.model is required"))
			Expect(cfg).To(BeNil())
		})

		It("Should reject global_flags without an application_path", func() {
			cfg, err := ParseConfig([]byte(`
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
global_flags:
  - context
`))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("global_flags is set but application_path is not"))
			Expect(cfg).To(BeNil())
		})

		It("Should require application_path for the tool endpoint", func() {
			cfg, err := ParseConfigForMode([]byte(`
identity: agent1
nats_context: ctx
expose:
  agent:
    a2a:
      serve_tools: true
`), ModeServe)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("application_path is required when expose.agent.a2a.serve_tools is set"))
			Expect(cfg).To(BeNil())
		})

		It("Should parse human_in_the_loop and report it enabled", func() {
			cfg, err := ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
harness:
  human_in_the_loop:
    enabled: true
llm:
  model: claude-sonnet-4-6
`))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.HumanInTheLoopEnabled()).To(BeTrue())
		})

		It("Should report human_in_the_loop disabled when absent or set to false", func() {
			cfg, err := ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
`))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.HumanInTheLoopEnabled()).To(BeFalse())

			cfg, err = ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
harness:
  human_in_the_loop:
    enabled: false
llm:
  model: claude-sonnet-4-6
`))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.HumanInTheLoopEnabled()).To(BeFalse())
		})
	})

	Describe("Slack", func() {
		It("Should be off unless the block is present", func() {
			cfg, err := ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
`))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.SlackEnabled()).To(BeFalse())
			Expect(cfg.SlackProgressEnabled()).To(BeFalse())
		})

		It("Should default every field so an empty block works", func() {
			cfg, err := ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
expose:
  agent:
    slack: {}
llm:
  model: claude-sonnet-4-6
`))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.SlackEnabled()).To(BeTrue())
			Expect(cfg.SlackWorkers()).To(Equal(DefaultSlackWorkers))
			Expect(cfg.SlackContextLines()).To(Equal(DefaultSlackContextLines))
			Expect(cfg.SlackMaxCoalesced()).To(Equal(DefaultSlackMaxCoalesced))
			Expect(cfg.SlackAnswerGrace()).To(Equal(30 * time.Second))
			Expect(cfg.SlackMaxWaiting()).To(Equal(DefaultSlackWorkers*2), "it derives from the worker count")
			Expect(cfg.SlackProgressEnabled()).To(BeTrue(), "the status message is on unless no_progress says otherwise")
		})

		It("Should take what the block sets", func() {
			cfg, err := ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
expose:
  agent:
    slack:
      workers: 2
      context_lines: 50
      no_progress: true
      answer_grace: 5s
      max_waiting: 3
      max_coalesced: 9
llm:
  model: claude-sonnet-4-6
`))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.SlackWorkers()).To(Equal(2))
			Expect(cfg.SlackContextLines()).To(Equal(50))
			Expect(cfg.SlackProgressEnabled()).To(BeFalse())
			Expect(cfg.SlackAnswerGrace()).To(Equal(5 * time.Second))
			Expect(cfg.SlackMaxWaiting()).To(Equal(3), "its own value wins over the derived one")
			Expect(cfg.SlackMaxCoalesced()).To(Equal(9))
		})

		// prepare() never runs for a Config an embedder builds in process, so the default
		// has to survive a zero parsed value rather than living only in prepare().
		It("Should default the grace window on a config prepare never ran over", func() {
			cfg := &Config{Expose: &ExposeConfig{Agent: &AgentExpose{Slack: &ExposedSlackConfig{}}}}

			Expect(cfg.SlackAnswerGrace()).To(Equal(30 * time.Second))
		})

		It("Should refuse a grace window of zero or less", func() {
			for _, grace := range []string{"0s", "-5s"} {
				_, err := ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
expose:
  agent:
    slack:
      answer_grace: ` + grace + `
llm:
  model: claude-sonnet-4-6
`))
				Expect(err).To(MatchError(ContainSubstring("answer_grace")), "grace %q", grace)
				Expect(err).To(MatchError(ContainSubstring("greater than zero")), "grace %q", grace)
			}
		})

		// The identity names the journals this bot's threads run in, so a name derived
		// from the application's basename is not enough: two agents that never chose one
		// would share their conversations.
		It("Should require what a run needs, naming the block that asked", func() {
			_, err := ParseConfigForMode([]byte(`
application_path: /usr/bin/nats
system_prompt: do the thing
expose:
  agent:
    slack: {}
llm:
  model: claude-sonnet-4-6
`), ModeServe)
			Expect(err).To(MatchError(ContainSubstring("identity is required when expose.agent.slack is set")))

			_, err = ParseConfigForMode([]byte(`
identity: agent1
application_path: /usr/bin/nats
expose:
  agent:
    slack: {}
llm:
  model: claude-sonnet-4-6
`), ModeServe)
			Expect(err).To(MatchError(ContainSubstring("prompt is required when expose.agent.slack is set")))

			_, err = ParseConfigForMode([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
expose:
  agent:
    slack: {}
llm: {}
`), ModeServe)
			Expect(err).To(MatchError(ContainSubstring("llm.model is required when expose.agent.slack is set")))
		})

		// An MCP-only agent needs neither identity nor prompt because it runs no agent
		// loop. A Slack turn runs the whole loop, so the waiver must not reach a config
		// that carries both.
		It("Should not inherit the MCP waiver on identity and prompt", func() {
			_, err := ParseConfig([]byte(`
application_path: /usr/bin/nats
expose:
  agent:
    mcp:
      port: 8080
    slack: {}
llm:
  model: claude-sonnet-4-6
`))
			Expect(err).To(MatchError(ContainSubstring("prompt is required unless exposed over MCP")))
		})
	})

	Describe("Jobs", func() {
		It("Should be off unless the block is present", func() {
			cfg, err := ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
`))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.JobsEnabled()).To(BeFalse())
			Expect(cfg.JobsQueue()).To(BeEmpty())
			Expect(cfg.JobsTaskType()).To(BeEmpty())
		})

		// Every field defaults, so presence alone is a working configuration. Both ends
		// of a task type must agree and nothing validates the pairing, which is why the
		// default is the value the documentation submits with.
		It("Should default every field so an empty block works", func() {
			cfg, err := ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
nats_context: ngs
expose:
  agent:
    jobs: {}
llm:
  model: claude-sonnet-4-6
`))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.JobsEnabled()).To(BeTrue())
			Expect(cfg.JobsQueue()).To(Equal(DefaultJobsQueue))
			Expect(cfg.JobsTaskType()).To(Equal(DefaultJobsTaskType))
			Expect(cfg.JobsWorkers()).To(Equal(DefaultJobsWorkers))
			Expect(cfg.JobsMaxPayload()).To(Equal(0))
			Expect(cfg.JobsNatsContext()).To(Equal("ngs"), "it falls back to the top-level context")
		})

		It("Should take what the block sets", func() {
			cfg, err := ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
nats_context: ngs
expose:
  agent:
    jobs:
      queue: SLOW
      task_type: fisk-ai:slow
      workers: 4
      max_payload: 2048
      nats_context: jobs_cluster
llm:
  model: claude-sonnet-4-6
`))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.JobsQueue()).To(Equal("SLOW"))
			Expect(cfg.JobsTaskType()).To(Equal("fisk-ai:slow"))
			Expect(cfg.JobsWorkers()).To(Equal(4))
			Expect(cfg.JobsMaxPayload()).To(Equal(2048))
			Expect(cfg.JobsNatsContext()).To(Equal("jobs_cluster"), "its own context wins over the top-level one")
		})

		It("Should require a way to reach the queue", func() {
			_, err := ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
expose:
  agent:
    jobs: {}
llm:
  model: claude-sonnet-4-6
`))
			Expect(err).To(MatchError(ContainSubstring("nats_context is required when expose.agent.jobs is set")))
		})

		// An MCP-only agent needs neither identity nor prompt because it runs no agent
		// loop. A jobs intake runs the whole loop, so the waiver must not reach a config
		// that carries both; without this it parses clean and fails later inside the
		// channel, naming no key in the file. Identity is not what fails here only
		// because it defaults to the application's basename.
		It("Should not inherit the MCP waiver on identity and prompt", func() {
			_, err := ParseConfig([]byte(`
application_path: /usr/bin/nats
nats_context: ngs
expose:
  agent:
    mcp:
      port: 8080
    jobs: {}
llm:
  model: claude-sonnet-4-6
`))
			Expect(err).To(MatchError(ContainSubstring("prompt is required")))
		})

		It("Should still waive them for an MCP-only agent", func() {
			cfg, err := ParseConfig([]byte(`
application_path: /usr/bin/nats
expose:
  agent:
    mcp:
      port: 8080
llm:
  model: claude-sonnet-4-6
`))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.JobsEnabled()).To(BeFalse())
		})
	})

	Describe("TUIDisabled", func() {
		It("Should report the TUI disabled when no_tui is true", func() {
			cfg, err := ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
harness:
  no_tui: true
llm:
  model: claude-sonnet-4-6
`))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.TUIDisabled()).To(BeTrue())
		})

		It("Should report the TUI enabled when no_tui is absent or false", func() {
			cfg, err := ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
`))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.TUIDisabled()).To(BeFalse())
		})
	})

	Describe("BellEnabled", func() {
		It("Should ring the bell by default when no_bell is absent", func() {
			cfg, err := ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
`))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.BellEnabled()).To(BeTrue())
		})

		It("Should silence the bell when no_bell is true", func() {
			cfg, err := ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
harness:
  no_bell: true
llm:
  model: claude-sonnet-4-6
`))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.BellEnabled()).To(BeFalse())
		})
	})

	Describe("LLMProvider", func() {
		It("Should default to anthropic when llm.provider is unset", func() {
			cfg, err := ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
`))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.LLMProvider()).To(Equal("anthropic"))
		})

		It("Should return the configured provider when llm.provider is set", func() {
			cfg, err := ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
  provider: openai
`))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.LLMProvider()).To(Equal("openai"))
		})
	})

	Describe("ToolSearchEnabled", func() {
		It("Should allow tool search by default when no_tool_search is absent", func() {
			cfg, err := ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
`))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.ToolSearchEnabled()).To(BeTrue())
		})

		It("Should disable tool search when no_tool_search is true", func() {
			cfg, err := ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
  no_tool_search: true
`))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.ToolSearchEnabled()).To(BeFalse())
		})
	})

	Describe("Expose helpers", func() {
		It("Should report a2a enabled when the block serves tools", func() {
			cfg, err := ParseConfigForMode([]byte(`
identity: agent1
application_path: /usr/bin/nats
nats_context: ngs
expose:
  agent:
    a2a:
      serve_tools: true
`), ModeServe)
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.A2AEnabled()).To(BeTrue())
			Expect(cfg.A2AServeToolsEnabled()).To(BeTrue())
			Expect(cfg.A2APromptsEnabled()).To(BeFalse())
			Expect(cfg.A2APromptsWorkers()).To(Equal(0), "an endpoint that is off bounds nothing")
			Expect(cfg.MCPEnabled()).To(BeFalse())
		})

		// The two endpoints are independent, and either one on its own is what puts this
		// agent on the network.
		It("Should report a2a enabled when the block answers prompts alone", func() {
			cfg, err := ParseConfigForMode([]byte(`
identity: agent1
system_prompt: do the thing
nats_context: ngs
llm:
  model: claude-sonnet-4-6
expose:
  agent:
    a2a:
      prompts: {}
`), ModeServe)
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.A2AEnabled()).To(BeTrue())
			Expect(cfg.A2AServeToolsEnabled()).To(BeFalse())
			Expect(cfg.A2APromptsEnabled()).To(BeTrue())
			Expect(cfg.A2APromptsWorkers()).To(Equal(DefaultPromptsWorkers), "an empty block is a working configuration")
		})

		It("Should report a2a disabled when the expose block is absent or serves nothing", func() {
			cfg, err := ParseConfigForMode([]byte(`
application_path: /usr/bin/nats
`), ModeMCP)
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.A2AEnabled()).To(BeFalse())

			cfg, err = ParseConfigForMode([]byte(`
application_path: /usr/bin/nats
expose:
  agent:
    a2a:
      serve_tools: false
`), ModeMCP)
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.A2AEnabled()).To(BeFalse())
		})

		It("Should report MCP enabled when the expose.agent.mcp block is present", func() {
			cfg, err := ParseConfigForMode([]byte(`
application_path: /usr/bin/nats
expose:
  agent:
    mcp:
      port: 9000
`), ModeMCP)
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.MCPEnabled()).To(BeTrue())
			Expect(cfg.MCPPort()).To(Equal(9000))
			Expect(cfg.A2AEnabled()).To(BeFalse())
		})

		It("Should report MCP enabled for a portless mcp block, leaving the default to the caller", func() {
			cfg, err := ParseConfigForMode([]byte(`
application_path: /usr/bin/nats
expose:
  agent:
    mcp: {}
`), ModeMCP)
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.MCPEnabled()).To(BeTrue())
			Expect(cfg.MCPPort()).To(Equal(0))
		})

		It("Should report the configured MCP bind address, defaulting to empty", func() {
			cfg, err := ParseConfigForMode([]byte(`
application_path: /usr/bin/nats
expose:
  agent:
    mcp:
      port: 9000
      address: 127.0.0.1
`), ModeMCP)
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.MCPAddress()).To(Equal("127.0.0.1"))

			cfg, err = ParseConfigForMode([]byte(`
application_path: /usr/bin/nats
expose:
  agent:
    mcp:
      port: 9000
`), ModeMCP)
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.MCPAddress()).To(Equal(""))
		})

		It("Should report MCP disabled when the expose block is absent", func() {
			cfg, err := ParseConfigForMode([]byte(`
application_path: /usr/bin/nats
`), ModeMCP)
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.MCPEnabled()).To(BeFalse())
		})
	})

	Describe("Validate", func() {
		var cfg *Config

		BeforeEach(func() {
			cfg = &Config{
				Identity:        "agent1",
				ApplicationPath: "/usr/bin/nats",
				SystemPrompt:    "do the thing",
				LLM:             LLMConfig{Model: "claude-sonnet-4-6"},
			}
		})

		It("Should pass with a complete config", func() {
			Expect(Validate(cfg)).To(Succeed())
		})

		It("Should fail when the config is nil", func() {
			err := Validate(nil)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("config is nil"))
		})

		It("Should pass in agent mode when application_path is missing", func() {
			cfg.ApplicationPath = ""
			Expect(Validate(cfg)).To(Succeed())
		})

		It("Should fail in serve mode when the tool endpoint has no application_path", func() {
			cfg.ApplicationPath = ""
			cfg.NatsContext = "ctx"
			cfg.Expose = &ExposeConfig{Agent: &AgentExpose{A2A: &ExposedA2AConfig{ServeTools: true}}}
			err := ValidateForMode(cfg, ModeServe)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("application_path is required when expose.agent.a2a.serve_tools is set"))
		})

		It("Should fail in serve mode when the a2a block asks for no endpoint", func() {
			cfg.NatsContext = "ctx"
			cfg.Expose = &ExposeConfig{Agent: &AgentExpose{A2A: &ExposedA2AConfig{ToolTimeoutString: "60s"}}}
			err := ValidateForMode(cfg, ModeServe)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("expose.agent.a2a enables nothing"))
		})

		It("Should fail in serve mode when a prompt endpoint has no way to reach NATS", func() {
			cfg.NatsContext = ""
			cfg.Expose = &ExposeConfig{Agent: &AgentExpose{A2A: &ExposedA2AConfig{Prompts: &ExposedPromptsConfig{}}}}
			err := ValidateForMode(cfg, ModeServe)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("nats_context is required when expose.agent.a2a is set"))
		})

		// Answering a prompt runs the whole loop, so it needs what a queued job needs
		// rather than what a served tool call needs.
		It("Should fail in serve mode when a prompt endpoint has no model", func() {
			cfg.NatsContext = "ctx"
			cfg.LLM.Model = ""
			cfg.Expose = &ExposeConfig{Agent: &AgentExpose{A2A: &ExposedA2AConfig{Prompts: &ExposedPromptsConfig{}}}}
			err := ValidateForMode(cfg, ModeServe)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("llm.model is required when expose.agent.a2a.prompts is set"))
		})

		// On a bus the identity is the subject an agent answers on and the queue group it
		// joins, so a name arrived at by accident does not fail to resolve: it joins
		// somebody else's fleet. A basename is such an accident, since a shared
		// executable whose behavior comes from its directory gives a fleet of unrelated
		// agents the same one.
		It("Should fail in serve mode when the identity was not named", func() {
			for _, expose := range []*ExposeConfig{
				{Agent: &AgentExpose{A2A: &ExposedA2AConfig{Prompts: &ExposedPromptsConfig{}}}},
				{Agent: &AgentExpose{A2A: &ExposedA2AConfig{ServeTools: true}}},
				{Agent: &AgentExpose{Jobs: &ExposedJobsConfig{}}},
			} {
				derived := &Config{
					ApplicationPath: "/usr/bin/abt",
					SystemPrompt:    "p",
					NatsContext:     "ctx",
					LLM:             LLMConfig{Model: "m"},
					Expose:          expose,
				}
				Expect(derived.Prepare()).To(Succeed())
				Expect(derived.Identity).To(Equal("abt"), "the basename is still what it runs as")
				Expect(derived.IdentityIsNamed()).To(BeFalse())

				err := ValidateForMode(derived, ModeServe)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("identity is required"))
			}
		})

		It("Should accept a prompt endpoint with no application at all", func() {
			cfg.ApplicationPath = ""
			cfg.NatsContext = "ctx"
			cfg.Expose = &ExposeConfig{Agent: &AgentExpose{A2A: &ExposedA2AConfig{Prompts: &ExposedPromptsConfig{Workers: 4}}}}
			Expect(ValidateForMode(cfg, ModeServe)).To(Succeed())
			Expect(cfg.A2APromptsWorkers()).To(Equal(4))
		})

		It("Should fail when global_flags is set without application_path", func() {
			cfg.ApplicationPath = ""
			cfg.GlobalFlags = []string{"context"}
			err := Validate(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("global_flags is set but application_path is not"))
		})

		It("Should fail when identity is missing and not exposed over MCP", func() {
			cfg.Identity = ""
			err := Validate(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("identity is required unless exposed over MCP"))
		})

		It("Should fail when prompt is missing and not exposed over MCP", func() {
			cfg.SystemPrompt = ""
			err := Validate(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("prompt is required unless exposed over MCP"))
		})

		It("Should accept an identity of letters, digits, '-' and '_'", func() {
			cfg.Identity = "agent_1-prod"
			Expect(Validate(cfg)).To(Succeed())
		})

		It("Should reject an identity with characters illegal in a NATS queue group", func() {
			for _, bad := range []string{"agent 1", "agent.1", "agent*", "agent>", "ag/ent"} {
				cfg.Identity = bad
				err := Validate(cfg)
				Expect(err).To(HaveOccurred(), "identity %q should be rejected", bad)
				Expect(err.Error()).To(ContainSubstring("is invalid"))
			}
		})

		It("Should reject an illegal identity even when exposed over MCP", func() {
			cfg.Identity = "agent.1"
			err := ValidateForMode(cfg, ModeMCP)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("is invalid"))
		})

		It("Should fail when llm.model is missing", func() {
			cfg.LLM.Model = ""
			err := Validate(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("llm.model is required"))
		})

		It("Should not require identity or prompt when exposed over MCP", func() {
			cfg.Identity = ""
			cfg.SystemPrompt = ""
			cfg.Expose = &ExposeConfig{Agent: &AgentExpose{MCP: &ExposedMCPConfig{Port: 8080}}}
			Expect(Validate(cfg)).To(Succeed())
		})

		It("Should still require identity and prompt when exposed only as an agent without MCP", func() {
			cfg.Identity = ""
			cfg.Expose = &ExposeConfig{Agent: &AgentExpose{A2A: &ExposedA2AConfig{ServeTools: true}}}
			err := Validate(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("identity is required unless exposed over MCP"))
		})
	})

	Describe("LLMBudget prepare", func() {
		It("Should keep explicit values and parse the call timeout", func() {
			b := &LLMBudget{
				MaxTokens:         10,
				MaxIterations:     30,
				CallTimeoutString: "45s",
			}
			Expect(b.prepare()).To(Succeed())
			Expect(b.MaxTokens).To(Equal(int64(10)))
			Expect(b.MaxIterations).To(Equal(int64(30)))
			Expect(b.CallTimeoutParsed).To(Equal(45 * time.Second))
		})

		It("Should apply defaults when values are unset", func() {
			b := &LLMBudget{}
			Expect(b.prepare()).To(Succeed())
			Expect(b.MaxTokens).To(Equal(int64(defaultLLMMaxTokens)))
			Expect(b.MaxIterations).To(Equal(int64(defaultLLMMaxIterations)))
			Expect(b.CallTimeoutParsed).To(Equal(defaultLLMCallTimeout))
		})

		It("Should error on an invalid call timeout", func() {
			b := &LLMBudget{CallTimeoutString: "soon"}
			err := b.prepare()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("invalid llm call_timeout"))
		})

		It("Should error on a negative max_tokens", func() {
			b := &LLMBudget{MaxTokens: -1}
			err := b.prepare()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("invalid llm max_tokens"))
		})

		It("Should error on a negative max_iterations", func() {
			b := &LLMBudget{MaxIterations: -1}
			err := b.prepare()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("invalid llm max_iterations"))
		})

		It("Should error on a negative max_output_tokens", func() {
			b := &LLMBudget{MaxOutputTokens: -1}
			err := b.prepare()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("invalid llm max_output_tokens"))
		})

		It("Should leave max_output_tokens unset so the agent applies its default", func() {
			b := &LLMBudget{MaxOutputTokens: 4096}
			Expect(b.prepare()).To(Succeed())
			Expect(b.MaxOutputTokens).To(Equal(int64(4096)))

			b = &LLMBudget{}
			Expect(b.prepare()).To(Succeed())
			Expect(b.MaxOutputTokens).To(Equal(int64(0)))
		})
	})

	Describe("Memory", func() {
		parseWithMemory := func(memory string) (*Config, error) {
			return ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
harness:
  memory:
` + memory))
		}

		It("Should leave memory off when the block is absent", func() {
			cfg, err := parseWithMemory("    enabled: false")
			Expect(err).ToNot(HaveOccurred())
			Expect(cfg.MemoryEnabled()).To(BeFalse())
			Expect(cfg.MemoryBackend()).To(BeEmpty())
			Expect(cfg.MemoryRawOptions()).To(BeNil())
		})

		It("Should default the backend to file and enable the index", func() {
			cfg, err := parseWithMemory("    enabled: true")
			Expect(err).ToNot(HaveOccurred())
			Expect(cfg.MemoryEnabled()).To(BeTrue())
			Expect(cfg.MemoryBackend()).To(Equal("file"))
			Expect(cfg.MemoryIndexEnabled()).To(BeTrue())
		})

		It("Should honor no_index as an opt-out", func() {
			cfg, err := parseWithMemory("    enabled: true\n    no_index: true")
			Expect(err).ToNot(HaveOccurred())
			Expect(cfg.MemoryEnabled()).To(BeTrue())
			Expect(cfg.MemoryIndexEnabled()).To(BeFalse())
		})

		It("Should capture the options block as canonical JSON for a per-backend decode", func() {
			cfg, err := parseWithMemory("    enabled: true\n    options:\n      directory: /tmp/mem")
			Expect(err).ToNot(HaveOccurred())
			Expect(string(cfg.MemoryRawOptions())).To(MatchJSON(`{"directory":"/tmp/mem"}`))
		})

		It("Should reject an unknown key inside the memory block", func() {
			_, err := parseWithMemory("    enabled: true\n    bogus: 1")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("bogus"))
		})
	})

	Describe("Sessions", func() {
		It("Should default an unset config to the file backend with no options", func() {
			cfg := &Config{}
			Expect(cfg.SessionBackend()).To(Equal("file"))
			Expect(cfg.SessionRawOptions()).To(BeNil())
		})

		It("Should default a nil SessionConfig via the nil-safe accessors", func() {
			var sc *SessionConfig
			Expect(sc.BackendName()).To(Equal("file"))
			Expect(sc.RawOptions()).To(BeNil())
		})

		It("Should synthesize the file backend with no options for an empty state dir", func() {
			sc := SessionConfigFromStateDir("")
			Expect(sc.BackendName()).To(Equal("file"))
			Expect(sc.RawOptions()).To(BeNil())
		})

		It("Should synthesize the directory option from a set state dir", func() {
			sc := SessionConfigFromStateDir("/tmp/runs")
			Expect(sc.BackendName()).To(Equal("file"))
			Expect(string(sc.RawOptions())).To(MatchJSON(`{"directory":"/tmp/runs"}`))
		})

		It("Should parse a file sessions block and capture its options as canonical JSON", func() {
			cfg, err := ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
harness:
  sessions:
    backend: file
    options:
      directory: /tmp/runs
`))
			Expect(err).ToNot(HaveOccurred())
			Expect(cfg.SessionBackend()).To(Equal("file"))
			Expect(string(cfg.SessionRawOptions())).To(MatchJSON(`{"directory":"/tmp/runs"}`))
		})

		It("Should parse a jetstream sessions block", func() {
			cfg, err := ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
harness:
  sessions:
    backend: jetstream
    options:
      stream: FISK_SESSIONS
`))
			Expect(err).ToNot(HaveOccurred())
			Expect(cfg.SessionBackend()).To(Equal("jetstream"))
			Expect(string(cfg.SessionRawOptions())).To(MatchJSON(`{"stream":"FISK_SESSIONS"}`))
		})

		It("Should reject an unknown key inside the sessions block", func() {
			_, err := ParseConfig([]byte(`
identity: agent1
application_path: /usr/bin/nats
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
harness:
  sessions:
    backend: file
    bogus: 1
`))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("bogus"))
		})
	})

	Describe("ApplyStateDir", func() {
		It("Should be a no-op for an empty dir, leaving the nil default", func() {
			cfg := &Config{}
			Expect(cfg.ApplyStateDir("")).To(Succeed())
			Expect(cfg.Harness.Sessions).To(BeNil())
			Expect(cfg.SessionBackend()).To(Equal("file"))
		})

		It("Should fold a set dir into the file backend directory option", func() {
			cfg := &Config{}
			Expect(cfg.ApplyStateDir("/tmp/runs")).To(Succeed())
			Expect(cfg.SessionBackend()).To(Equal("file"))
			Expect(string(cfg.SessionRawOptions())).To(MatchJSON(`{"directory":"/tmp/runs"}`))
		})

		It("Should override a configured file directory, the flag winning last", func() {
			cfg := &Config{Harness: HarnessConfig{Sessions: SessionConfigFromStateDir("/from/config")}}
			Expect(cfg.ApplyStateDir("/from/flag")).To(Succeed())
			Expect(string(cfg.SessionRawOptions())).To(MatchJSON(`{"directory":"/from/flag"}`))
		})

		It("Should hard error when combined with a non-file backend and leave it untouched", func() {
			cfg := &Config{Harness: HarnessConfig{Sessions: &SessionConfig{Backend: "jetstream"}}}
			err := cfg.ApplyStateDir("/tmp/runs")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(`backend is "jetstream"`))
			Expect(cfg.SessionBackend()).To(Equal("jetstream"))
		})

		It("Should leave a non-file backend untouched for an empty dir", func() {
			cfg := &Config{Harness: HarnessConfig{Sessions: &SessionConfig{Backend: "jetstream"}}}
			Expect(cfg.ApplyStateDir("")).To(Succeed())
			Expect(cfg.SessionBackend()).To(Equal("jetstream"))
		})
	})

	Describe("ApplyIdentity", func() {
		It("Should be a no-op for an empty name, leaving what was there", func() {
			cfg := &Config{Identity: "orders"}
			Expect(cfg.ApplyIdentity("")).To(Succeed())
			Expect(cfg.Identity).To(Equal("orders"))
		})

		It("Should override the configured name, the flag winning last", func() {
			cfg := &Config{Identity: "orders"}
			Expect(cfg.ApplyIdentity("billing")).To(Succeed())
			Expect(cfg.Identity).To(Equal("billing"))
		})

		// A name typed at a command line is one a person chose, which is the whole
		// question IdentityIsNamed answers for anything using it as an address.
		It("Should record the name as chosen rather than derived", func() {
			cfg, err := ParseConfig([]byte("system_prompt: hi\nllm:\n  model: opus\n"))
			Expect(err).ToNot(HaveOccurred())
			Expect(cfg.IdentityIsNamed()).To(BeFalse(), "left at the default rather than chosen")

			Expect(cfg.ApplyIdentity("billing")).To(Succeed())
			Expect(cfg.IdentityIsNamed()).To(BeTrue())
		})

		It("Should refuse a name that is not a legal subject token, leaving it untouched", func() {
			cfg := &Config{Identity: "orders"}
			err := cfg.ApplyIdentity("billing.eu")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("is invalid"))
			Expect(cfg.Identity).To(Equal("orders"))
		})
	})

	Describe("CredentialEnvNames", func() {
		It("Should carry no operator-named variable when the vector tier is off", func() {
			cfg := &Config{Harness: HarnessConfig{RAG: &RAGConfig{Enabled: true}}}
			Expect(cfg.CredentialEnvNames()).To(Equal(otlpCredentialEnvNames))
		})

		It("Should return the embeddings api_key_env when the vector tier is on", func() {
			cfg := &Config{Harness: HarnessConfig{RAG: &RAGConfig{
				Enabled:    true,
				Embeddings: &RAGEmbeddingsConfig{APIKeyEnv: "MY_EMBED_KEY"},
			}}}
			Expect(cfg.CredentialEnvNames()).To(ContainElement("MY_EMBED_KEY"))
		})

		It("Should trim and drop an empty or whitespace api_key_env", func() {
			cfg := &Config{Harness: HarnessConfig{RAG: &RAGConfig{
				Enabled:    true,
				Embeddings: &RAGEmbeddingsConfig{APIKeyEnv: "   "},
			}}}
			Expect(cfg.CredentialEnvNames()).To(Equal(otlpCredentialEnvNames))
		})

		// The OpenTelemetry export credentials are ambient operator variables, present
		// regardless of what one agent's config says. Gating them on telemetry being
		// enabled would mean --no-telemetry re-exposes the token to every tool
		// subprocess, which is the opposite of what an operator reaches for it to do.
		DescribeTable("Should strip the OpenTelemetry export credentials unconditionally",
			func(cfg *Config) {
				names := cfg.CredentialEnvNames()

				for _, name := range otlpCredentialEnvNames {
					Expect(names).To(ContainElement(name))
				}
				Expect(names).To(ContainElement("OTEL_EXPORTER_OTLP_HEADERS"))
				Expect(names).To(ContainElement("OTEL_EXPORTER_OTLP_LOGS_HEADERS"))
				Expect(names).To(ContainElement("OTEL_EXPORTER_OTLP_CLIENT_KEY"))
			},
			Entry("an empty config", &Config{}),
			Entry("telemetry never mentioned", &Config{Identity: "demo"}),
			Entry("telemetry explicitly off", &Config{Telemetry: TelemetryConfig{Enabled: false}}),
			Entry("telemetry on", &Config{Telemetry: TelemetryConfig{Enabled: true}}),
		)

		// internal/telemetry lists the bearer-token variables again for its own startup
		// checks, because neither package can import the other in production code. This
		// is the spec that stops the two copies drifting apart, and it reads the real
		// list rather than restating it: a third hand-written copy here would assert
		// nothing, since adding a name in telemetry would leave all three of us agreeing
		// with each other and disagreeing with the code.
		//
		// The direction is one way on purpose. config holds the superset, adding the mTLS
		// variables that telemetry has no reason to know about, so this asserts inclusion
		// rather than equality.
		It("Should be a superset of the names internal/telemetry checks", func() {
			cfg := &Config{}

			shared := telemetry.HeaderEnvNames()
			Expect(shared).ToNot(BeEmpty())

			for _, name := range shared {
				Expect(cfg.CredentialEnvNames()).To(ContainElement(name))
			}
		})

		It("Should not return duplicates when an api_key_env repeats a known name", func() {
			cfg := &Config{Harness: HarnessConfig{RAG: &RAGConfig{
				Enabled:    true,
				Embeddings: &RAGEmbeddingsConfig{APIKeyEnv: "OTEL_EXPORTER_OTLP_HEADERS"},
			}}}

			names := cfg.CredentialEnvNames()
			Expect(names).To(HaveLen(len(otlpCredentialEnvNames)))
		})
	})

	Describe("Telemetry", func() {
		It("Should be off with no telemetry block", func() {
			cfg, err := ParseConfig([]byte(minimalAgentConfig))
			Expect(err).ToNot(HaveOccurred())
			Expect(cfg.TelemetryEnabled()).To(BeFalse())
			Expect(cfg.TelemetryMetricsEnabled()).To(BeTrue())
			Expect(cfg.Telemetry.SampleRatio).To(BeNil())
		})

		It("Should parse a full telemetry block", func() {
			cfg, err := ParseConfig([]byte(minimalAgentConfig + `
telemetry:
  enabled: true
  endpoint: http://127.0.0.1:4318
  service_name: demo-agent
  sample_ratio: 0.25
  no_metrics: true
`))
			Expect(err).ToNot(HaveOccurred())
			Expect(cfg.TelemetryEnabled()).To(BeTrue())
			Expect(cfg.Telemetry.Endpoint).To(Equal("http://127.0.0.1:4318"))
			Expect(cfg.Telemetry.ServiceName).To(Equal("demo-agent"))
			Expect(cfg.Telemetry.SampleRatio).ToNot(BeNil())
			Expect(*cfg.Telemetry.SampleRatio).To(Equal(0.25))
			Expect(cfg.TelemetryMetricsEnabled()).To(BeFalse())
		})

		// The reason sample_ratio is a pointer. An explicit zero means sample nothing,
		// and a plain float64 would make it indistinguishable from an absent key, so it
		// would be defaulted back to sampling everything and every trace would reach a
		// paid backend. Nothing may collapse it to the Go zero value on the way through.
		It("Should keep an explicit zero sample_ratio distinct from an absent one", func() {
			cfg, err := ParseConfig([]byte(minimalAgentConfig + `
telemetry:
  enabled: true
  sample_ratio: 0
`))
			Expect(err).ToNot(HaveOccurred())
			Expect(cfg.Telemetry.SampleRatio).ToNot(BeNil())
			Expect(*cfg.Telemetry.SampleRatio).To(Equal(0.0))
		})

		It("Should reject an unknown key in the telemetry block", func() {
			_, err := ParseConfig([]byte(minimalAgentConfig + `
telemetry:
  enabled: true
  capture_content: true
`))
			Expect(err).To(HaveOccurred())
		})

		// Validation belongs to internal/telemetry, not to prepare: the config object
		// stays literal, and a stale endpoint in a file with telemetry off must never
		// fail a run that will export nothing.
		It("Should not validate the endpoint or the ratio at parse time", func() {
			cfg, err := ParseConfig([]byte(minimalAgentConfig + `
telemetry:
  endpoint: not-a-url
  sample_ratio: 7
`))
			Expect(err).ToNot(HaveOccurred())
			Expect(cfg.Telemetry.Endpoint).To(Equal("not-a-url"))
		})
	})

	Describe("memory read_only", func() {
		It("Should be off unless the block asks for it", func() {
			cfg, err := ParseConfig([]byte(minimalAgentConfig + `
harness:
  memory:
    enabled: true
`))
			Expect(err).ToNot(HaveOccurred())
			Expect(cfg.MemoryReadOnly()).To(BeFalse())
		})

		It("Should report read only when the block asks for it", func() {
			cfg, err := ParseConfig([]byte(minimalAgentConfig + `
harness:
  memory:
    enabled: true
    read_only: true
`))
			Expect(err).ToNot(HaveOccurred())
			Expect(cfg.MemoryReadOnly()).To(BeTrue())
		})

		// There are no tools to narrow when memory is off, so this must not report a
		// restriction that would read as a memory feature being present.
		It("Should be off when memory itself is off", func() {
			cfg, err := ParseConfig([]byte(minimalAgentConfig + `
harness:
  memory:
    enabled: false
    read_only: true
`))
			Expect(err).ToNot(HaveOccurred())
			Expect(cfg.MemoryReadOnly()).To(BeFalse())
		})
	})

	// The block's presence is what carries the third state, so these assert the two
	// accessors against all three configurations rather than only the two obvious ones.
	Describe("thinking", func() {
		It("Should ask for nothing when the block is absent", func() {
			cfg, err := ParseConfig([]byte(minimalAgentConfig))
			Expect(err).ToNot(HaveOccurred())

			Expect(cfg.LLM.Thinking).To(BeNil())
			Expect(cfg.ThinkingEnabled()).To(BeFalse())
			Expect(cfg.ThinkingDisabled()).To(BeFalse())
		})

		It("Should ask for thinking when the block enables it", func() {
			cfg, err := ParseConfig([]byte(`
identity: agent1
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
  thinking:
    enabled: true
`))
			Expect(err).ToNot(HaveOccurred())

			Expect(cfg.ThinkingEnabled()).To(BeTrue())
			Expect(cfg.ThinkingDisabled()).To(BeFalse())
		})

		// The state this distinction exists for: enabled false is a request to turn
		// reasoning off, not the silence an absent block leaves behind.
		It("Should ask for thinking off when the block disables it", func() {
			cfg, err := ParseConfig([]byte(`
identity: agent1
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
  thinking:
    enabled: false
`))
			Expect(err).ToNot(HaveOccurred())

			Expect(cfg.LLM.Thinking).ToNot(BeNil())
			Expect(cfg.ThinkingEnabled()).To(BeFalse())
			Expect(cfg.ThinkingDisabled()).To(BeTrue())
		})

		It("Should carry an effort level without saying anything about thinking", func() {
			cfg, err := ParseConfig([]byte(`
identity: agent1
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
  reasoning_effort: XHigh
`))
			Expect(err).ToNot(HaveOccurred())

			Expect(cfg.ReasoningEffort()).To(Equal("xhigh"))
			Expect(cfg.LLM.Thinking).To(BeNil(), "an effort is not a thinking block")
			Expect(cfg.ThinkingEnabled()).To(BeFalse())
			Expect(cfg.ThinkingDisabled()).To(BeFalse())
		})

		// Which levels exist is the model's, so a level this build has never heard of is
		// carried to the provider rather than refused here.
		It("Should carry a level it does not recognize", func() {
			cfg, err := ParseConfig([]byte(`
identity: agent1
system_prompt: do the thing
llm:
  model: some-model-from-next-year
  reasoning_effort: ludicrous
`))
			Expect(err).ToNot(HaveOccurred())
			Expect(cfg.ReasoningEffort()).To(Equal("ludicrous"))
		})

		It("Should refuse a value no provider could take", func() {
			for _, effort := range []string{"high effort", "high\nlow", "high!", strings.Repeat("x", 33)} {
				_, err := ParseConfig([]byte(fmt.Sprintf("identity: agent1\nsystem_prompt: do the thing\nllm:\n  model: m\n  reasoning_effort: %q\n", effort)))
				Expect(err).To(MatchError(ContainSubstring("reasoning_effort")), "accepted %q", effort)
			}
		})

		It("Should be nil-safe on a zero configuration", func() {
			cfg := &Config{}

			Expect(cfg.ThinkingEnabled()).To(BeFalse())
			Expect(cfg.ThinkingDisabled()).To(BeFalse())
		})
	})
})
