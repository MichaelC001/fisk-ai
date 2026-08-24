//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("MCP servers", func() {
	base := func(servers ...MCPServer) *Config {
		cfg := &Config{
			ApplicationPath: "/bin/true",
			Identity:        "agent",
			SystemPrompt:    "do things",
			MCPServers:      servers,
		}
		cfg.LLM.Model = ModelClaudeSonnet46

		return cfg
	}

	prepared := func(servers ...MCPServer) *Config {
		cfg := base(servers...)
		Expect(cfg.prepare()).To(Succeed())

		return cfg
	}

	Describe("MCPServer.EffectiveAlias", func() {
		It("Should use the alias when set", func() {
			Expect(MCPServer{Name: "filesystem", Alias: "fs"}.EffectiveAlias()).To(Equal("fs"))
		})

		It("Should fall back to the server name", func() {
			Expect(MCPServer{Name: "filesystem"}.EffectiveAlias()).To(Equal("filesystem"))
		})
	})

	Describe("MCPServer.ConnectTimeout", func() {
		It("Should default to 30 seconds when no timeout is set", func() {
			cfg := prepared(MCPServer{Name: "filesystem", Command: "npx"})
			Expect(cfg.MCPServers[0].ConnectTimeout()).To(Equal(30 * time.Second))
		})

		It("Should use an explicit timeout parsed by prepare", func() {
			cfg := prepared(MCPServer{Name: "filesystem", Command: "npx", TimeoutString: "5s"})
			Expect(cfg.MCPServers[0].TimeoutParsed).To(Equal(5 * time.Second))
			Expect(cfg.MCPServers[0].ConnectTimeout()).To(Equal(5 * time.Second))
		})

		It("Should reject an unparseable timeout", func() {
			cfg := base(MCPServer{Name: "filesystem", Command: "npx", TimeoutString: "soon"})
			Expect(cfg.prepare()).To(MatchError(ContainSubstring("invalid mcp_servers timeout \"soon\" on server \"filesystem\"")))
		})

		It("Should reject a zero timeout, which would leave the connect unbounded", func() {
			cfg := base(MCPServer{Name: "filesystem", Command: "npx", TimeoutString: "0s"})
			Expect(cfg.prepare()).To(MatchError(ContainSubstring("must be greater than zero")))
		})

		It("Should reject a negative timeout", func() {
			cfg := base(MCPServer{Name: "filesystem", Command: "npx", TimeoutString: "-1s"})
			Expect(cfg.prepare()).To(MatchError(ContainSubstring("must be greater than zero")))
		})
	})

	Describe("ValidateForMode", func() {
		It("Should accept a stdio entry", func() {
			cfg := prepared(MCPServer{
				Name:    "filesystem",
				Command: "npx",
				Args:    []string{"-y", "@modelcontextprotocol/server-filesystem", "/srv"},
				Env:     map[string]string{"FS_TOKEN": "${FS_TOKEN}"},
				Include: &ToolFilter{Tools: []string{"^read_"}},
			})
			Expect(ValidateForMode(cfg, ModeAgent)).To(Succeed())
		})

		It("Should accept an HTTP entry", func() {
			cfg := prepared(MCPServer{
				Name:    "docs",
				Alias:   "d",
				URL:     "https://mcp.example.net/mcp",
				Headers: map[string]string{"Authorization": "Bearer ${DOCS_TOKEN}"},
				Exclude: &ToolFilter{Tools: []string{"^danger_"}},
			})
			Expect(ValidateForMode(cfg, ModeAgent)).To(Succeed())
		})

		// The block is structural, so it fails the same way whichever command reads the
		// file rather than only where the servers are imported.
		It("Should validate in every mode", func() {
			cfg := prepared(MCPServer{Name: "filesystem"})
			Expect(ValidateForMode(cfg, ModeAgent)).To(MatchError(ContainSubstring("sets neither command nor url")))
			Expect(ValidateForMode(cfg, ModeMCP)).To(MatchError(ContainSubstring("sets neither command nor url")))
			Expect(ValidateForMode(cfg, ModeServe)).To(MatchError(ContainSubstring("sets neither command nor url")))
		})

		It("Should require a name", func() {
			cfg := prepared(MCPServer{Command: "npx"})
			Expect(ValidateForMode(cfg, ModeAgent)).To(MatchError(ContainSubstring("mcp_servers server is missing a name")))
		})

		It("Should reject a name that is not a legal tool-name token", func() {
			cfg := prepared(MCPServer{Name: "bad.name", Command: "npx"})
			Expect(ValidateForMode(cfg, ModeAgent)).To(MatchError(ContainSubstring("mcp_servers server name \"bad.name\" is invalid")))
		})

		It("Should reject an invalid alias", func() {
			cfg := prepared(MCPServer{Name: "filesystem", Alias: "has space", Command: "npx"})
			Expect(ValidateForMode(cfg, ModeAgent)).To(MatchError(ContainSubstring("has an invalid alias \"has space\"")))
		})

		It("Should reject a duplicate name", func() {
			cfg := prepared(
				MCPServer{Name: "filesystem", Command: "npx"},
				MCPServer{Name: "filesystem", URL: "https://mcp.example.net/mcp"},
			)
			Expect(ValidateForMode(cfg, ModeAgent)).To(MatchError(ContainSubstring("more than one server named \"filesystem\"")))
		})

		It("Should reject two servers whose effective aliases collide", func() {
			cfg := prepared(
				MCPServer{Name: "filesystem", Command: "npx"},
				MCPServer{Name: "docs", Alias: "filesystem", URL: "https://mcp.example.net/mcp"},
			)
			err := ValidateForMode(cfg, ModeAgent)
			Expect(err).To(MatchError(ContainSubstring("both use the alias \"filesystem\"")))
			Expect(err).To(MatchError(ContainSubstring("name the same tool twice")))
		})

		It("Should reject an entry setting neither command nor url", func() {
			cfg := prepared(MCPServer{Name: "filesystem"})
			Expect(ValidateForMode(cfg, ModeAgent)).To(MatchError(ContainSubstring("mcp_servers server \"filesystem\" sets neither command nor url")))
		})

		It("Should reject an entry setting both command and url", func() {
			cfg := prepared(MCPServer{Name: "filesystem", Command: "npx", URL: "https://mcp.example.net/mcp"})
			Expect(ValidateForMode(cfg, ModeAgent)).To(MatchError(ContainSubstring("sets both command and url")))
		})

		It("Should reject env on an HTTP entry", func() {
			cfg := prepared(MCPServer{Name: "docs", URL: "https://mcp.example.net/mcp", Env: map[string]string{"TOKEN": "${DOCS_TOKEN}"}})
			Expect(ValidateForMode(cfg, ModeAgent)).To(MatchError(ContainSubstring("sets env alongside url")))
		})

		It("Should reject headers on a stdio entry", func() {
			cfg := prepared(MCPServer{Name: "filesystem", Command: "npx", Headers: map[string]string{"Authorization": "${FS_TOKEN}"}})
			Expect(ValidateForMode(cfg, ModeAgent)).To(MatchError(ContainSubstring("sets headers alongside command")))
		})

		It("Should reject an include-by-tag filter, which MCP has no vocabulary for", func() {
			cfg := prepared(MCPServer{Name: "filesystem", Command: "npx", Include: &ToolFilter{Tags: []string{"impact:ro"}}})
			Expect(ValidateForMode(cfg, ModeAgent)).To(MatchError(ContainSubstring("include.tags filter, which cannot be honored")))
		})

		It("Should reject an exclude-by-tag filter, which MCP has no vocabulary for", func() {
			cfg := prepared(MCPServer{Name: "filesystem", Command: "npx", Exclude: &ToolFilter{Tags: []string{"impact:rw"}}})
			Expect(ValidateForMode(cfg, ModeAgent)).To(MatchError(ContainSubstring("exclude.tags filter, which cannot be honored")))
		})

		It("Should reject args on an HTTP entry", func() {
			cfg := prepared(MCPServer{Name: "docs", URL: "https://mcp.example.net/mcp", Args: []string{"-y"}})
			Expect(ValidateForMode(cfg, ModeAgent)).To(MatchError(ContainSubstring("sets args alongside url")))
		})

		It("Should reject a url with no scheme", func() {
			cfg := prepared(MCPServer{Name: "docs", URL: "localhost:9000"})
			err := ValidateForMode(cfg, ModeAgent)
			Expect(err).To(MatchError(ContainSubstring("mcp_servers server \"docs\" has an invalid url \"localhost:9000\"")))
			Expect(err).To(MatchError(ContainSubstring("http:// or https:// endpoint")))
		})

		It("Should reject a url whose scheme is not http or https", func() {
			cfg := prepared(MCPServer{Name: "docs", URL: "ftp://mcp.example.net/mcp"})
			Expect(ValidateForMode(cfg, ModeAgent)).To(MatchError(ContainSubstring("has an invalid url \"ftp://mcp.example.net/mcp\"")))
		})

		It("Should reject a url that is not a url at all", func() {
			cfg := prepared(MCPServer{Name: "docs", URL: "notaurl"})
			Expect(ValidateForMode(cfg, ModeAgent)).To(MatchError(ContainSubstring("has an invalid url \"notaurl\"")))
		})

		It("Should reject a url naming no host", func() {
			cfg := prepared(MCPServer{Name: "docs", URL: "https:///mcp"})
			Expect(ValidateForMode(cfg, ModeAgent)).To(MatchError(ContainSubstring("it names no host")))
		})

		It("Should accept an https url", func() {
			cfg := prepared(MCPServer{Name: "docs", URL: "https://mcp.example.net/mcp"})
			Expect(ValidateForMode(cfg, ModeAgent)).To(Succeed())
		})

		It("Should reject an illegal variable name in a reference", func() {
			cfg := prepared(MCPServer{Name: "filesystem", Command: "npx", Env: map[string]string{"FS_TOKEN": "${FS-TOKEN}"}})
			err := ValidateForMode(cfg, ModeAgent)
			Expect(err).To(MatchError(ContainSubstring("has an invalid env value for \"FS_TOKEN\"")))
			Expect(err).To(MatchError(ContainSubstring("\"FS-TOKEN\" is not a variable name")))
		})

		It("Should reject a reference that is never closed", func() {
			cfg := prepared(MCPServer{Name: "filesystem", Command: "npx", Env: map[string]string{"FS_TOKEN": "${FS_TOKEN"}})
			err := ValidateForMode(cfg, ModeAgent)
			Expect(err).To(MatchError(ContainSubstring("has an invalid env value for \"FS_TOKEN\"")))
			Expect(err).To(MatchError(ContainSubstring("is never closed")))
		})

		It("Should reject a reference naming no variable", func() {
			cfg := prepared(MCPServer{Name: "docs", URL: "https://mcp.example.net/mcp", Headers: map[string]string{"Authorization": "Bearer ${}"}})
			err := ValidateForMode(cfg, ModeAgent)
			Expect(err).To(MatchError(ContainSubstring("has an invalid headers value for \"Authorization\"")))
			Expect(err).To(MatchError(ContainSubstring("names no variable")))
		})

		It("Should accept references mixed with literal text", func() {
			cfg := prepared(MCPServer{Name: "docs", URL: "https://mcp.example.net/mcp", Headers: map[string]string{
				"Authorization": "Bearer ${DOCS_TOKEN}",
				"X-Tenant":      "${TENANT}-${REGION} team",
			}})
			Expect(ValidateForMode(cfg, ModeAgent)).To(Succeed())
		})

		It("Should accept a literal value carrying no reference", func() {
			cfg := prepared(MCPServer{Name: "docs", URL: "https://mcp.example.net/mcp", Headers: map[string]string{"X-Tenant": "acme"}})
			Expect(ValidateForMode(cfg, ModeAgent)).To(Succeed())
		})

		It("Should accept a bare $VAR as literal text", func() {
			cfg := prepared(MCPServer{Name: "filesystem", Command: "npx", Env: map[string]string{"FS_HOME": "$HOME/cache"}})
			Expect(ValidateForMode(cfg, ModeAgent)).To(Succeed())
		})
	})

	Describe("Environment references", func() {
		Describe("EnvReferences", func() {
			It("Should name the variables a value references, in order", func() {
				Expect(EnvReferences("${FS_TOKEN}")).To(Equal([]string{"FS_TOKEN"}))
				Expect(EnvReferences("Bearer ${DOCS_TOKEN}")).To(Equal([]string{"DOCS_TOKEN"}))
				Expect(EnvReferences("${SCHEME}://${HOST}/mcp")).To(Equal([]string{"SCHEME", "HOST"}))
			})

			It("Should name nothing in a value with no reference", func() {
				Expect(EnvReferences("literal")).To(BeEmpty())
				Expect(EnvReferences("$HOME/cache")).To(BeEmpty())
			})

			It("Should reject a reference the syntax does not close", func() {
				_, err := EnvReferences("Bearer ${DOCS_TOKEN")
				Expect(err).To(MatchError(ContainSubstring("is never closed")))
			})

			It("Should reject a reference naming no variable", func() {
				_, err := EnvReferences("Bearer ${}")
				Expect(err).To(MatchError(ContainSubstring("names no variable")))
			})

			It("Should reject a name the shell would not accept", func() {
				_, err := EnvReferences("${1TOKEN}")
				Expect(err).To(MatchError(ContainSubstring("\"1TOKEN\" is not a variable name")))
			})
		})

		Describe("ExpandEnvReferences", func() {
			lookup := func(values map[string]string) func(string) (string, bool) {
				return func(name string) (string, bool) {
					value, ok := values[name]

					return value, ok
				}
			}

			It("Should replace every reference and leave the literal text alone", func() {
				expanded, err := ExpandEnvReferences("Bearer ${DOCS_TOKEN}", lookup(map[string]string{"DOCS_TOKEN": "secret"}))
				Expect(err).ToNot(HaveOccurred())
				Expect(expanded).To(Equal("Bearer secret"))

				expanded, err = ExpandEnvReferences("${SCHEME}://${HOST}/mcp", lookup(map[string]string{"SCHEME": "https", "HOST": "mcp.example.net"}))
				Expect(err).ToNot(HaveOccurred())
				Expect(expanded).To(Equal("https://mcp.example.net/mcp"))
			})

			It("Should return a value with no reference unchanged", func() {
				expanded, err := ExpandEnvReferences("$HOME/cache", lookup(nil))
				Expect(err).ToNot(HaveOccurred())
				Expect(expanded).To(Equal("$HOME/cache"))
			})

			It("Should name the first variable that is not set", func() {
				_, err := ExpandEnvReferences("${FS_TOKEN} ${DOCS_TOKEN}", lookup(map[string]string{"DOCS_TOKEN": "secret"}))
				Expect(err).To(MatchError(ContainSubstring("environment variable \"FS_TOKEN\" is not set")))
			})

			It("Should reject a value whose reference syntax is wrong", func() {
				_, err := ExpandEnvReferences("Bearer ${DOCS_TOKEN", lookup(nil))
				Expect(err).To(MatchError(ContainSubstring("is never closed")))
			})

			It("Should read the process environment when passed os.LookupEnv", func() {
				GinkgoT().Setenv("FISK_AI_MCP_TEST_TOKEN", "secret")

				expanded, err := ExpandEnvReferences("Bearer ${FISK_AI_MCP_TEST_TOKEN}", os.LookupEnv)
				Expect(err).ToNot(HaveOccurred())
				Expect(expanded).To(Equal("Bearer secret"))
			})
		})

		// fisk info and fisk mcp parse a config they never connect with, so a credential
		// they do not use must not stop the file loading.
		It("Should parse a config whose reference names a variable that is not set", func() {
			_, set := os.LookupEnv("FISK_AI_MCP_TEST_ABSENT")
			Expect(set).To(BeFalse())

			cfg, err := ParseConfigForMode([]byte(`
identity: agent
system_prompt: do things
llm:
  model: `+ModelClaudeSonnet46+`
mcp_servers:
  - name: filesystem
    command: npx
    args:
      - "-y"
      - "@modelcontextprotocol/server-filesystem"
    env:
      FS_TOKEN: "${FISK_AI_MCP_TEST_ABSENT}"
    timeout: 10s
`), ModeAgent)
			Expect(err).ToNot(HaveOccurred())
			Expect(cfg.MCPServers).To(HaveLen(1))
			Expect(cfg.MCPServers[0].Env).To(Equal(map[string]string{"FS_TOKEN": "${FISK_AI_MCP_TEST_ABSENT}"}))
			Expect(cfg.MCPServers[0].Args).To(Equal([]string{"-y", "@modelcontextprotocol/server-filesystem"}))
			Expect(cfg.MCPServers[0].ConnectTimeout()).To(Equal(10 * time.Second))
		})
	})
})
