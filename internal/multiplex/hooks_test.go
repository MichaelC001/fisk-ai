//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package multiplex

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/a2a"
	wire "github.com/choria-io/fisk-ai/internal/a2a/wire/v1"
)

// fakeReporter records what it was told, in order. The hooks fire it from one goroutine
// per spec, so it needs no lock.
type fakeReporter struct {
	states  []string
	reasons []string
	closed  bool
}

func (f *fakeReporter) Name() string { return "fake" }
func (f *fakeReporter) Working()     { f.states = append(f.states, "working") }
func (f *fakeReporter) Idle()        { f.states = append(f.states, "idle") }
func (f *fakeReporter) Close()       { f.closed = true }

func (f *fakeReporter) Blocked(reason string) {
	f.states = append(f.states, "blocked")
	f.reasons = append(f.reasons, reason)
}

var _ = Describe("ClientHooks", func() {
	var (
		rep   *fakeReporter
		hooks a2a.ClientHooks
		ctx   context.Context
	)

	BeforeEach(func() {
		rep = &fakeReporter{}
		hooks = ClientHooks(rep)
		ctx = context.Background()
	})

	// The states a turn passes through, in the order the client fires them.
	It("Should follow a turn from the prompt to its end", func() {
		_, err := hooks.PromptSubmit(ctx, a2a.ClientPromptSubmitInfo{Prompt: "remove the stream"})
		Expect(err).ToNot(HaveOccurred())

		hooks.TurnAccepted(ctx, a2a.TurnAcceptedInfo{})
		hooks.QuestionAsked(ctx, a2a.QuestionAskedInfo{Kind: wire.ElicitConfirm, Question: "remove ORDERS?"})
		hooks.QuestionAnswered(ctx, a2a.QuestionAnsweredInfo{Answered: true})
		hooks.TurnEnd(ctx, a2a.ClientTurnEndInfo{Answered: true})

		Expect(rep.states).To(HaveExactElements("working", "working", "blocked", "working", "idle"))
	})

	// The prompt has left, so the wait for a person is over before the agent acks it.
	It("Should report work from the moment the prompt is sent", func() {
		_, err := hooks.PromptSubmit(ctx, a2a.ClientPromptSubmitInfo{Prompt: "remove the stream"})
		Expect(err).ToNot(HaveOccurred())

		Expect(rep.states).To(HaveExactElements("working"))
	})

	// Whatever the turn was doing, it has stopped, and what follows is the operator's.
	DescribeTable("Should report a turn that ended as idle",
		func(info a2a.ClientTurnEndInfo) {
			hooks.TurnEnd(ctx, info)

			Expect(rep.states).To(HaveExactElements("idle"))
		},
		Entry("an answer", a2a.ClientTurnEndInfo{Answered: true}),
		Entry("a failure", a2a.ClientTurnEndInfo{Code: wire.CodeCapacity}),
		Entry("a run somebody canceled", a2a.ClientTurnEndInfo{Err: context.Canceled}),
		Entry("a set that stopped early", a2a.ClientTurnEndInfo{Err: a2a.ErrIncompleteStream}),
	)

	// Nobody is waiting on a person any more, whether they answered or the run gave the
	// question up under them.
	DescribeTable("Should report work once a question is done with",
		func(info a2a.QuestionAnsweredInfo) {
			hooks.QuestionAnswered(ctx, info)

			Expect(rep.states).To(HaveExactElements("working"))
		},
		Entry("answered", a2a.QuestionAnsweredInfo{Answered: true}),
		Entry("nobody answered", a2a.QuestionAnsweredInfo{}),
		Entry("answered too late to deliver", a2a.QuestionAnsweredInfo{Held: true}),
	)

	Describe("What a blocked pane shows", func() {
		It("Should show the question of a question", func() {
			hooks.QuestionAsked(ctx, a2a.QuestionAskedInfo{
				Kind:     wire.ElicitConfirm,
				Question: "remove ORDERS?",
			})

			Expect(rep.reasons).To(HaveExactElements("remove ORDERS?"))
		})

		// An approve question carries the command rather than a question, and on a list
		// of panes a bare command line does not say that a decision is what is wanted.
		It("Should label the command of an approval", func() {
			hooks.QuestionAsked(ctx, a2a.QuestionAskedInfo{
				Kind:    wire.ElicitApprove,
				Display: "stream rm ORDERS",
			})

			Expect(rep.reasons).To(HaveExactElements("approve: stream rm ORDERS"))
		})
	})

	// A caller wires the same option whether or not a multiplexer claimed the process.
	It("Should fire nothing without a reporter", func() {
		Expect(ClientHooks(nil)).To(Equal(a2a.ClientHooks{}))
	})
})
