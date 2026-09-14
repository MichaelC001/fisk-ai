//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package builtin

import (
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/toolkit/functool"
)

var _ = Describe("OptInTools", func() {
	var root string

	BeforeEach(func() {
		root = GinkgoT().TempDir()
	})

	// listing is a configuration whose harness.tools holds the entries given, with
	// read_file rooted at the spec's temporary directory.
	listing := func(entries ...config.HarnessToolConfig) *config.Config {
		return &config.Config{RootDirectory: root, Harness: config.HarnessConfig{Tools: entries}}
	}

	names := func(tools []*functool.Tool) []string {
		out := make([]string, 0, len(tools))
		for _, t := range tools {
			out = append(out, t.Name())
		}

		return out
	}

	It("Should return nil when nothing is listed", func() {
		tools, err := OptInTools(&config.Config{})
		Expect(err).ToNot(HaveOccurred())
		Expect(tools).To(BeNil())
	})

	It("Should build every listed tool in the listed order", func() {
		tools, err := OptInTools(listing(
			config.HarnessToolConfig{Name: config.Base64EncodeToolName},
			config.HarnessToolConfig{Name: config.ReadFileToolName},
		))
		Expect(err).ToNot(HaveOccurred())
		Expect(names(tools)).To(Equal([]string{"base64_encode", "read_file"}))

		tools, err = OptInTools(listing(
			config.HarnessToolConfig{Name: config.ReadFileToolName},
			config.HarnessToolConfig{Name: config.Base64EncodeToolName},
		))
		Expect(err).ToNot(HaveOccurred())
		Expect(names(tools)).To(Equal([]string{"read_file", "base64_encode"}))
	})

	It("Should gate an entry with confirm set and show the trace as the approval summary", func() {
		tools, err := OptInTools(listing(
			config.HarnessToolConfig{Name: config.ReadFileToolName, Confirm: true},
			config.HarnessToolConfig{Name: config.Base64EncodeToolName},
		))
		Expect(err).ToNot(HaveOccurred())
		Expect(tools).To(HaveLen(2))

		Expect(tools[0].NeedsConfirm(nil)).To(BeTrue())
		Expect(tools[0].TraceLine(json.RawMessage(`{"path":"x.md"}`))).To(Equal("read_file x.md"))
		Expect(tools[0].MCPExposable()).To(BeTrue(), "the wrap leaves Expose alone")

		Expect(tools[1].NeedsConfirm(nil)).To(BeFalse())
		Expect(tools[1].TraceLine(json.RawMessage(`{"text":"abc"}`))).To(Equal("base64_encode (3 bytes)"))
	})

	It("Should carry the entry's index and name on an options error", func() {
		_, err := OptInTools(listing(
			config.HarnessToolConfig{Name: config.Base64EncodeToolName},
			config.HarnessToolConfig{Name: config.ReadFileToolName, Options: json.RawMessage(`{"roots": "x"}`)},
		))
		Expect(err).To(MatchError(HavePrefix("harness.tools[1] (read_file): ")))
		Expect(err).To(MatchError(ContainSubstring("roots")))
	})

	It("Should refuse a name the table lacks", func() {
		_, err := OptInTools(listing(config.HarnessToolConfig{Name: "nope"}))
		Expect(err).To(MatchError(HavePrefix("harness.tools[0] (nope): ")))
	})

	It("Should hold a constructor for every name config accepts and no other", func() {
		table := make([]string, 0, len(optInConstructors))
		for name := range optInConstructors {
			table = append(table, name)
		}
		Expect(table).To(ConsistOf(config.HarnessToolNames()))
	})
})
