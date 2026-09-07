//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// callCtx is the context a tool runs under, naming the call any question it asks
// belongs to.
func callCtx(toolUseID string) context.Context {
	return toolkit.ContextWithToolUseID(context.Background(), toolUseID)
}

// The prompter is the whole of the interaction model: it ends a turn on a question and
// answers the next turn's question from what the request carried.
var _ = Describe("Prompter", func() {
	var p *prompter

	BeforeEach(func() {
		p = newPrompter(quietLogger())
	})

	It("Should report that the page can be asked", func() {
		Expect(p.CanPrompt()).To(BeTrue())
	})

	Describe("An unheld question", func() {
		It("Should record an approval and leave its call unanswered", func() {
			choice, err := p.ApproveCommand(context.Background(), toolkit.GateRequest{ToolUseID: "c1", Command: "stream rm", Display: "stream rm ORDERS", Tag: "ai:confirm"})
			Expect(err).To(MatchError(toolkit.ErrPromptAborted))
			Expect(choice).To(Equal(toolkit.ConfirmNo))

			q := p.question()
			Expect(q).ToNot(BeNil())
			Expect(*q).To(Equal(Question{Kind: KindApprove, ToolUseID: "c1", Command: "stream rm", Display: "stream rm ORDERS", Tag: "ai:confirm"}))
		})

		It("Should record each of the three tool questions under its call", func() {
			_, err := p.Confirm(callCtx("c2"), "Proceed?")
			Expect(err).To(MatchError(toolkit.ErrPromptAborted))
			Expect(*p.question()).To(Equal(Question{Kind: KindConfirm, ToolUseID: "c2", Text: "Proceed?"}))

			p = newPrompter(quietLogger())
			_, err = p.Select(callCtx("c3"), "Which one?", []string{"east", "west"})
			Expect(err).To(MatchError(toolkit.ErrPromptAborted))
			Expect(*p.question()).To(Equal(Question{Kind: KindSelect, ToolUseID: "c3", Text: "Which one?", Options: []string{"east", "west"}}))

			p = newPrompter(quietLogger())
			_, err = p.Input(callCtx("c4"), "Which subject?", "orders.>")
			Expect(err).To(MatchError(toolkit.ErrPromptAborted))
			Expect(*p.question()).To(Equal(Question{Kind: KindInput, ToolUseID: "c4", Text: "Which subject?", Default: "orders.>"}))
		})

		// The abort ends the run, so a second question on one turn only arises on a
		// resume where the first was answered from the request. The first recorded one is
		// what the page sees; the second is put on the next request.
		It("Should keep the first question when a second is asked", func() {
			_, err := p.Confirm(callCtx("c1"), "First?")
			Expect(err).To(MatchError(toolkit.ErrPromptAborted))

			_, err = p.Confirm(callCtx("c2"), "Second?")
			Expect(err).To(MatchError(toolkit.ErrPromptAborted))

			Expect(p.question().ToolUseID).To(Equal("c1"))
		})

		// The page answers by naming the call, so a question naming none could never be
		// answered and would be asked again on every resume. The tool turns the error into
		// its own null result rather than the run suspending on it.
		It("Should refuse a tool question outside a call without aborting", func() {
			_, err := p.Confirm(context.Background(), "Proceed?")
			Expect(err).To(MatchError(ContainSubstring("outside a tool call")))
			Expect(err).ToNot(MatchError(toolkit.ErrPromptAborted))
			Expect(p.question()).To(BeNil())
		})
	})

	Describe("A held answer", func() {
		It("Should answer an approval for its call and spend it", func() {
			p.hold(Answer{ToolUseID: "c1", Kind: KindApprove, Approval: toolkit.ConfirmAlways})

			choice, err := p.ApproveCommand(context.Background(), toolkit.GateRequest{ToolUseID: "c1", Command: "stream rm"})
			Expect(err).ToNot(HaveOccurred())
			Expect(choice).To(Equal(toolkit.ConfirmAlways))
			Expect(p.question()).To(BeNil(), "an answered question is not asked")

			_, err = p.ApproveCommand(context.Background(), toolkit.GateRequest{ToolUseID: "c1", Command: "stream rm"})
			Expect(err).To(MatchError(toolkit.ErrPromptAborted), "the answer is spent on the first question about the call")
		})

		It("Should answer each of the three tool questions", func() {
			p.hold(Answer{ToolUseID: "c2", Kind: KindConfirm, Confirmed: true})
			yes, err := p.Confirm(callCtx("c2"), "Proceed?")
			Expect(err).ToNot(HaveOccurred())
			Expect(yes).To(BeTrue())

			p.hold(Answer{ToolUseID: "c3", Kind: KindSelect, Index: 1})
			idx, err := p.Select(callCtx("c3"), "Which one?", []string{"east", "west"})
			Expect(err).ToNot(HaveOccurred())
			Expect(idx).To(Equal(1))

			p.hold(Answer{ToolUseID: "c4", Kind: KindInput, Value: "orders.new"})
			value, err := p.Input(callCtx("c4"), "Which subject?", "")
			Expect(err).ToNot(HaveOccurred())
			Expect(value).To(Equal("orders.new"))
		})

		// A resent or stale answer names a call the resume is not asking about, so it is
		// left in place and the question asked is recorded rather than answered from it.
		It("Should not answer a question about another call", func() {
			p.hold(Answer{ToolUseID: "c1", Kind: KindApprove, Approval: toolkit.ConfirmOnce})

			_, err := p.ApproveCommand(context.Background(), toolkit.GateRequest{ToolUseID: "c9", Command: "stream rm"})
			Expect(err).To(MatchError(toolkit.ErrPromptAborted))
			Expect(p.question().ToolUseID).To(Equal("c9"))
		})

		It("Should not answer a question of another kind about the same call", func() {
			p.hold(Answer{ToolUseID: "c1", Kind: KindConfirm, Confirmed: true})

			_, err := p.ApproveCommand(context.Background(), toolkit.GateRequest{ToolUseID: "c1", Command: "stream rm"})
			Expect(err).To(MatchError(toolkit.ErrPromptAborted))
			Expect(p.question().Kind).To(Equal(KindApprove))
		})

		It("Should report a choice outside the options rather than clamp it", func() {
			p.hold(Answer{ToolUseID: "c3", Kind: KindSelect, Index: 5})

			idx, err := p.Select(callCtx("c3"), "Which one?", []string{"east", "west"})
			Expect(err).To(MatchError(ContainSubstring("option 5 of 2")))
			Expect(err).ToNot(MatchError(toolkit.ErrPromptAborted))
			Expect(idx).To(Equal(-1))
		})
	})
})
