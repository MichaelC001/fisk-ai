//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agent

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/llm"
)

// Thinking has three states and the configuration carries the third in the presence
// of its block, so these pin the two translations that state passes through: the
// neutral mode a provider is handed, and the fingerprint value a resume compares.
var _ = Describe("thinking mode", func() {
	unset := func() *config.Config {
		return &config.Config{Identity: "demo"}
	}

	on := func() *config.Config {
		cfg := unset()
		cfg.LLM.Thinking = &config.ThinkingConfig{Enabled: true}

		return cfg
	}

	off := func() *config.Config {
		cfg := unset()
		cfg.LLM.Thinking = &config.ThinkingConfig{Enabled: false}

		return cfg
	}

	Describe("thinkingMode", func() {
		It("Should ask for nothing when the configuration names no block", func() {
			Expect(thinkingMode(unset())).To(Equal(llm.ThinkingUnset))
		})

		It("Should ask for thinking when the block enables it", func() {
			Expect(thinkingMode(on())).To(Equal(llm.ThinkingOn))
		})

		// The distinction the third state exists for: a block that enables nothing is a
		// request to stop reasoning, not the silence of no block at all.
		It("Should ask for thinking off when the block disables it", func() {
			Expect(thinkingMode(off())).To(Equal(llm.ThinkingOff))
		})
	})

	Describe("the fingerprint value", func() {
		mode := func(cfg *config.Config) string {
			GinkgoHelper()

			fp, err := computeFingerprint(cfg, "anthropic", []string{"sys"}, nil)
			Expect(err).ToNot(HaveOccurred())

			return fp.ThinkingMode
		}

		// "off" is what a configuration saying nothing about thinking recorded before the
		// third state existed. It has to keep meaning that, or every session journaled
		// until now stops resuming against a configuration nobody changed.
		It("Should still record off for a configuration that says nothing", func() {
			Expect(mode(unset())).To(Equal("off"))
		})

		It("Should record summarized when thinking is on", func() {
			Expect(mode(on())).To(Equal("summarized"))
		})

		// The fingerprint asks what the journal can hold, not what the file says. Neither
		// of these adds a thinking block to the conversation, so telling them apart would
		// refuse a resume over an edit that cannot have made the stored turns incoherent
		// — and on a queue, where nothing can force a resume, that refusal costs the job.
		It("Should record the same value for asking nothing and asking for off", func() {
			Expect(mode(off())).To(Equal("off"))
			Expect(mode(off())).To(Equal(mode(unset())))
		})

		It("Should still separate thinking on from both of them", func() {
			Expect(mode(on())).ToNot(Equal(mode(off())))
			Expect(mode(on())).ToNot(Equal(mode(unset())))
		})
	})
})
