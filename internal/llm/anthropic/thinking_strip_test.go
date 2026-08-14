//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package anthropic

import (
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/llm"
)

// A resumed conversation can hold reasoning a previous run produced, and a call that
// was told not to think has no use for the signatures on it. These pin what is dropped,
// what is kept, and the two states that must not drop anything.
var _ = Describe("stripping thinking from a request", func() {
	thinking := func() llm.ContentBlock {
		return llm.ContentBlock{Thinking: &llm.ThinkingBlock{Text: "because", Signature: []byte("sig")}}
	}

	redacted := func() llm.ContentBlock {
		return llm.ContentBlock{Provider: &llm.ProviderBlock{
			Kind: redactedThinkingKind,
			Raw:  json.RawMessage(`{"type":"redacted_thinking","data":"opaque"}`),
		}}
	}

	text := func(s string) llm.ContentBlock {
		return llm.ContentBlock{Text: &llm.TextBlock{Text: s}}
	}

	toolUse := func() llm.ContentBlock {
		return llm.ContentBlock{ToolUse: &llm.ToolUseBlock{ID: "tu-1", Name: "shell", Input: []byte(`{}`)}}
	}

	Describe("withoutThinking", func() {
		It("Should drop a thinking block and keep everything else in order", func() {
			m := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
				thinking(), text("answer"), toolUse(),
			}}

			out := withoutThinking(m)
			Expect(out.Content).To(HaveLen(2))
			Expect(out.Content[0].Text.Text).To(Equal("answer"))
			Expect(out.Content[1].ToolUse.ID).To(Equal("tu-1"))
		})

		// Redacted reasoning is a provider block rather than a ThinkingBlock, so a strip
		// that only looked at the named kind would leave the signature it carries behind
		// and not actually remove the hazard.
		It("Should drop a redacted thinking block too", func() {
			m := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
				redacted(), text("answer"),
			}}

			out := withoutThinking(m)
			Expect(out.Content).To(HaveLen(1))
			Expect(out.Content[0].Text.Text).To(Equal("answer"))
		})

		// An assistant turn with no content is rejected outright, so a turn that was
		// nothing but reasoning is left as it stands rather than made invalid.
		It("Should leave a message that is entirely thinking alone", func() {
			m := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{thinking(), redacted()}}

			out := withoutThinking(m)
			Expect(out.Content).To(HaveLen(2))
		})

		It("Should not disturb a message with no thinking in it", func() {
			m := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{text("go")}}

			out := withoutThinking(m)
			Expect(out.Content).To(HaveLen(1))
		})

		// The conversation belongs to the runner and is sent again on the next iteration,
		// so stripping a copy must not empty the original.
		It("Should not mutate the message it was given", func() {
			m := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{thinking(), text("answer")}}

			withoutThinking(m)
			Expect(m.Content).To(HaveLen(2))
			Expect(m.Content[0].Thinking).ToNot(BeNil())
		})
	})

	Describe("buildParams", func() {
		var p *Provider

		BeforeEach(func() {
			p = &Provider{}
		})

		reqWith := func(mode llm.ThinkingMode) llm.Request {
			return llm.Request{
				Model: "test-model",
				Messages: []llm.Message{
					{Role: llm.RoleUser, Content: []llm.ContentBlock{text("go")}},
					{Role: llm.RoleAssistant, Content: []llm.ContentBlock{thinking(), text("answer")}},
				},
				Thinking: mode,
			}
		}

		It("Should strip the reasoning when thinking is off", func() {
			params, err := p.buildParams(reqWith(llm.ThinkingOff))
			Expect(err).NotTo(HaveOccurred())
			Expect(params.Messages[1].Content).To(HaveLen(1))
		})

		It("Should replay the reasoning when thinking is on", func() {
			params, err := p.buildParams(reqWith(llm.ThinkingOn))
			Expect(err).NotTo(HaveOccurred())
			Expect(params.Messages[1].Content).To(HaveLen(2))
		})

		// The case that would be a bug rather than a nicety. An unset mode leaves the
		// model to its own behavior, which may be to think, and the signature on a
		// thinking block has to come back with the tool_use it accompanied. Stripping
		// here would break that chain inside a single run.
		It("Should replay the reasoning when the mode is unset", func() {
			params, err := p.buildParams(reqWith(llm.ThinkingUnset))
			Expect(err).NotTo(HaveOccurred())
			Expect(params.Messages[1].Content).To(HaveLen(2))
		})
	})
})
