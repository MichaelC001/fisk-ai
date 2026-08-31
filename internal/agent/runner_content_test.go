//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
)

// noopJournal is a journal that accepts everything and stores nothing, so a spec can
// drive rotateSession without a store behind it. rotateSession finalizes the outgoing
// journal and closes it, so the field cannot simply be nil.
type noopJournal struct{}

func (noopJournal) Append(context.Context, uint64, runstate.Record) error { return nil }
func (noopJournal) Records(context.Context) ([]runstate.Record, error)    { return nil, nil }
func (noopJournal) LastSeq() uint64                                       { return 0 }
func (noopJournal) CheckHeld(context.Context) error                       { return nil }
func (noopJournal) Close() error                                          { return nil }

var _ runstate.Journal = noopJournal{}

// errStoreUnavailable stands in for a session store that cannot create the journal a
// rotation needs.
var errStoreUnavailable = errors.New("session store unavailable")

// toolResultsMsg is the user-role message that answers a batch of tool calls. It is a
// user message, which is what makes the fold case below subtle.
func toolResultsMsg(id string) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{
		{ToolResult: &llm.ToolResultBlock{ToolUseID: id, Content: "ok"}},
	}}
}

// These specs settle what contentFrom means at every point the conversation changes,
// with no telemetry attached. It is deliberately not tested through a span: the
// question is a property of the runner's own state, the answer has to be right before
// anything reads it, and an assertion on an exported attribute would be testing the
// renderer at the same time as the arithmetic.
var _ = Describe("runner content baseline", func() {
	Describe("appendUserPrompt", func() {
		It("leaves the baseline alone when the follow-up becomes its own turn", func() {
			r := &runner{
				messages:    []llm.Message{userMsg("first"), assistantTextMsg("reply")},
				contentFrom: 2,
			}

			r.appendUserPrompt("second")

			// The new message sits at index 2, which the next capture already covers.
			Expect(r.contentFrom).To(Equal(2))
			Expect(r.messages).To(HaveLen(3))
		})

		// The fold mutates a message the previous model call already carried, so the
		// baseline has to reach back to include it or the added block is exported
		// nowhere at all.
		It("reaches back when the follow-up is folded into a trailing user turn", func() {
			r := &runner{
				messages:    []llm.Message{userMsg("first")},
				contentFrom: 1,
			}

			r.appendUserPrompt("more")

			Expect(r.messages).To(HaveLen(1))
			Expect(r.messages[0].Content).To(HaveLen(2))
			Expect(r.contentFrom).To(BeZero())
		})

		// This is the case that makes the rule a clamp rather than a subtraction, and
		// it is reachable: appendUserPrompt folds into ANY trailing user message, a
		// tool-results batch is a user message, and a turn that ends at the iteration
		// cap is continuable, so the operator's next prompt lands on one.
		//
		// With [user, assistant, results, assistant, results] the baseline is 3 and the
		// fold mutates index 4. Stepping back by one would give 2 and re-export the
		// results message the previous call already carried.
		It("clamps rather than stepping back by one", func() {
			r := &runner{
				messages: []llm.Message{
					userMsg("go"),
					assistantTextMsg("calling a tool"),
					toolResultsMsg("call_1"),
					assistantTextMsg("calling another"),
					toolResultsMsg("call_2"),
				},
				contentFrom: 3,
			}

			r.appendUserPrompt("stop and summarize")

			Expect(r.contentFrom).To(Equal(3))
		})

		It("folds into an empty conversation without reaching below zero", func() {
			r := &runner{}

			r.appendUserPrompt("first")

			Expect(r.contentFrom).To(BeZero())
			Expect(r.messages).To(HaveLen(1))
		})
	})

	// Both sites replace the conversation wholesale, so a baseline carried across
	// either is an index into a slice that no longer exists. The builder clamps too,
	// but that is the backstop; this is the fix.
	Describe("resetting the conversation", func() {
		It("resets the baseline when the context is cleared", func() {
			r := &runner{
				messages:    []llm.Message{userMsg("a"), assistantTextMsg("b"), userMsg("c")},
				contentFrom: 3,
			}

			r.resetContext()

			Expect(r.messages).To(BeEmpty())
			Expect(r.contentFrom).To(BeZero())
		})

		It("resets the baseline when the session rotates", func() {
			r := &runner{
				messages:    []llm.Message{userMsg("a"), assistantTextMsg("b"), userMsg("c")},
				contentFrom: 3,
				journal:     noopJournal{},
				events:      nopEvents{},
				newSession: func(context.Context, string) (runstate.Journal, string, error) {
					return noopJournal{}, "session-2", nil
				},
			}

			Expect(r.rotateSession(context.Background(), "fresh start")).To(Succeed())

			Expect(r.messages).To(HaveLen(1))
			Expect(r.contentFrom).To(BeZero())
		})

		// A rotation that fails leaves the original conversation in place, so resetting
		// the baseline on the intent to rotate rather than on the assignment would
		// re-export the whole transcript on the next call.
		It("leaves the baseline alone when the rotation failed", func() {
			r := &runner{
				messages:    []llm.Message{userMsg("a"), assistantTextMsg("b"), userMsg("c")},
				contentFrom: 3,
				journal:     noopJournal{},
				events:      nopEvents{},
				newSession: func(context.Context, string) (runstate.Journal, string, error) {
					return nil, "", errStoreUnavailable
				},
			}

			Expect(r.rotateSession(context.Background(), "fresh start")).To(MatchError(errStoreUnavailable))

			Expect(r.messages).To(HaveLen(3))
			Expect(r.contentFrom).To(Equal(3))
		})
	})
})
