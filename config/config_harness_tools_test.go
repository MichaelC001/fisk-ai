// Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("The harness.tools block", func() {
	parse := func(tools string) (*Config, error) {
		return ParseConfig([]byte(`
identity: agent1
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
harness:
  tools:
` + tools))
	}

	It("Should round-trip a listing from YAML with options arriving as JSON", func() {
		cfg, err := parse(`
    - name: base64_encode
    - name: read_file
      confirm: true
      options:
        root: /srv/corpus
        max_bytes: 1048576
`)
		Expect(err).ToNot(HaveOccurred())

		tools := cfg.HarnessTools()
		Expect(tools).To(HaveLen(2))
		Expect(tools[0].Name).To(Equal(Base64EncodeToolName))
		Expect(tools[0].Confirm).To(BeFalse())
		Expect(tools[0].Options).To(BeNil())
		Expect(tools[1].Name).To(Equal(ReadFileToolName))
		Expect(tools[1].Confirm).To(BeTrue())
		Expect(string(tools[1].Options)).To(MatchJSON(`{"root":"/srv/corpus","max_bytes":1048576}`))
	})

	It("Should trim a name", func() {
		cfg, err := parse("    - name: ' read_file '\n")
		Expect(err).ToNot(HaveOccurred())
		Expect(cfg.HarnessTools()).To(Equal([]HarnessToolConfig{{Name: ReadFileToolName}}))
	})

	It("Should refuse an unknown name, naming the accepted set", func() {
		_, err := parse("    - name: frobnicate\n")
		Expect(err).To(MatchError(ContainSubstring(`harness.tools: "frobnicate" is not a built-in an entry can enable`)))
		Expect(err).To(MatchError(ContainSubstring("accepted: read_file, base64_encode")))
	})

	It("Should refuse a built-in another block enables", func() {
		_, err := parse("    - name: knowledge_search\n")
		Expect(err).To(MatchError(ContainSubstring(`"knowledge_search" is not a built-in an entry can enable`)))
	})

	It("Should refuse a duplicate", func() {
		_, err := parse("    - name: read_file\n    - name: read_file\n")
		Expect(err).To(MatchError(ContainSubstring(`harness.tools: "read_file" is listed twice`)))
	})

	It("Should refuse an empty name", func() {
		_, err := parse("    - name: base64_encode\n    - confirm: true\n")
		Expect(err).To(MatchError(ContainSubstring("harness.tools: entry 2 has no name")))
	})

	It("Should refuse an unknown key inside an entry", func() {
		_, err := parse("    - name: read_file\n      enabled: true\n")
		Expect(err).To(MatchError(ContainSubstring("enabled")))
	})

	It("Should return nil when none are listed", func() {
		cfg := &Config{}
		Expect(cfg.Prepare()).To(Succeed())
		Expect(cfg.HarnessTools()).To(BeNil())

		cfg = &Config{Harness: HarnessConfig{Tools: []HarnessToolConfig{}}}
		Expect(cfg.Prepare()).To(Succeed())
		Expect(cfg.HarnessTools()).To(BeNil())
	})

	It("Should report the accepted names as a copy", func() {
		names := HarnessToolNames()
		Expect(names).To(Equal([]string{ReadFileToolName, Base64EncodeToolName}))

		names[0] = "changed"
		Expect(HarnessToolNames()).To(Equal([]string{ReadFileToolName, Base64EncodeToolName}))
	})
})
