//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package util

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/choria-io/fisk-ai/internal/toolkit"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("ConfirmGate", func() {
	// Whether an operator is reachable is now the prompter's own report, so a spec
	// drives it through the fakePrompter (interactive by default) rather than stubbing a
	// process-global terminal check.
	var prompter *fakePrompter
	BeforeEach(func() {
		prompter = &fakePrompter{canPrompt: true}
	})

	// newGate builds a gate wired to the spec's fakePrompter, keeping its grants in
	// memory as an un-checkpointed run does.
	newGate := func() *ConfirmGate {
		return NewConfirmGate(prompter, nil)
	}

	Describe("Approve", func() {
		It("Should allow once without remembering the tool", func() {
			calls := 0
			prompter.approveFn = func(toolkit.GateRequest) (toolkit.ConfirmChoice, error) {
				calls++
				return toolkit.ConfirmOnce, nil
			}
			gate := newGate()

			allowed, reason, aerr := gate.Approve(context.Background(), "use-1", "stream_rm", "stream rm", "stream rm ORDERS", "ai:confirm")
			Expect(aerr).ToNot(HaveOccurred())
			Expect(allowed).To(BeTrue())
			Expect(reason).To(BeEmpty())

			// A second call prompts again because "once" is not remembered.
			allowed, _, _ = gate.Approve(context.Background(), "use-1", "stream_rm", "stream rm", "stream rm BILLING", "ai:confirm")
			Expect(allowed).To(BeTrue())
			Expect(calls).To(Equal(2))
		})

		It("Should remember an always answer by tool name for any arguments", func() {
			calls := 0
			prompter.approveFn = func(toolkit.GateRequest) (toolkit.ConfirmChoice, error) {
				calls++
				return toolkit.ConfirmAlways, nil
			}
			gate := newGate()

			allowed, _, _ := gate.Approve(context.Background(), "use-1", "stream_rm", "stream rm", "stream rm ORDERS", "ai:confirm")
			Expect(allowed).To(BeTrue())

			// A later call with different arguments is allowed without re-prompting; the
			// gate emits no trace of its own, the caller renders the command's line.
			allowed, _, _ = gate.Approve(context.Background(), "use-1", "stream_rm", "stream rm", "stream rm EVERYTHING", "ai:confirm")
			Expect(allowed).To(BeTrue())
			Expect(calls).To(Equal(1))

			// A different tool is still asked about.
			allowed, _, _ = gate.Approve(context.Background(), "use-2", "server_run", "server run", "server run", "ai:confirm")
			Expect(allowed).To(BeTrue())
			Expect(calls).To(Equal(2))
		})

		It("Should pass the triggering tag and command line to the prompter", func() {
			prompter.approveFn = func(toolkit.GateRequest) (toolkit.ConfirmChoice, error) { return toolkit.ConfirmOnce, nil }
			gate := newGate()

			allowed, _, _ := gate.Approve(context.Background(), "use-1", "stream_rm", "stream rm", "stream rm ORDERS", "impact:rw")
			Expect(allowed).To(BeTrue())
			Expect(prompter.lastGateReq).To(Equal(toolkit.GateRequest{ToolUseID: "use-1", Command: "stream rm", Display: "stream rm ORDERS", Tag: "impact:rw"}))
		})

		It("Should decline when the operator says no, and re-prompt next time", func() {
			calls := 0
			prompter.approveFn = func(toolkit.GateRequest) (toolkit.ConfirmChoice, error) {
				calls++
				return toolkit.ConfirmNo, nil
			}
			gate := newGate()

			allowed, reason, aerr := gate.Approve(context.Background(), "use-1", "stream_rm", "stream rm", "stream rm ORDERS", "ai:confirm")
			Expect(aerr).ToNot(HaveOccurred())
			Expect(allowed).To(BeFalse())
			Expect(reason).To(ContainSubstring("declined"))
			Expect(reason).To(ContainSubstring("do not retry"))

			// No is not sticky: the same command is asked about again.
			gate.Approve(context.Background(), "use-1", "stream_rm", "stream rm", "stream rm ORDERS", "ai:confirm")
			Expect(calls).To(Equal(2))
		})

		// A prompt that could not be rendered is not an operator who walked away, so it
		// stays a denial: the command is gated and nothing established that it may run.
		It("Should deny by default when the prompt fails for any other reason", func() {
			prompter.approveFn = func(toolkit.GateRequest) (toolkit.ConfirmChoice, error) {
				return toolkit.ConfirmNo, errors.New("the terminal went away")
			}
			gate := newGate()

			allowed, reason, aerr := gate.Approve(context.Background(), "use-1", "stream_rm", "stream rm", "stream rm ORDERS", "ai:confirm")
			Expect(aerr).ToNot(HaveOccurred())
			Expect(allowed).To(BeFalse())
			Expect(reason).To(ContainSubstring("the terminal went away"))
		})

		// The operator was asked and did not answer. Recording a denial here would put a
		// decision in the journal that nobody made, and every later resume would replay
		// it as theirs.
		It("Should report an aborted prompt as an error rather than a denial", func() {
			prompter.approveFn = func(toolkit.GateRequest) (toolkit.ConfirmChoice, error) {
				return toolkit.ConfirmNo, fmt.Errorf("%w: interrupt", toolkit.ErrPromptAborted)
			}
			gate := newGate()

			allowed, reason, aerr := gate.Approve(context.Background(), "use-1", "stream_rm", "stream rm", "stream rm ORDERS", "ai:confirm")
			Expect(aerr).To(MatchError(toolkit.ErrPromptAborted))
			Expect(allowed).To(BeFalse())
			Expect(reason).To(BeEmpty())
		})

		// A prompter whose operator is reached over something slower than a terminal
		// answers later. Folding that into a denial would refuse a command the operator
		// is still deciding on, and the model would be told the refusal is final.
		It("Should report a deferred question as an error rather than a denial", func() {
			prompter.approveFn = func(toolkit.GateRequest) (toolkit.ConfirmChoice, error) {
				return toolkit.ConfirmNo, toolkit.DeferResult("asked the caller", "q-1")
			}
			src := &fakeApprovals{}
			gate := NewConfirmGate(prompter, src)

			allowed, reason, aerr := gate.Approve(context.Background(), "use-1", "stream_rm", "stream rm", "stream rm ORDERS", "ai:confirm")
			Expect(aerr).To(MatchError(toolkit.ErrDeferredResult))
			Expect(allowed).To(BeFalse())
			Expect(reason).To(BeEmpty())
			Expect(src.recorded).To(BeEmpty())
		})

		It("Should deny with the no-terminal reason when no operator is reachable", func() {
			prompter.canPrompt = false
			prompter.approveFn = func(toolkit.GateRequest) (toolkit.ConfirmChoice, error) {
				Fail("must not prompt when no operator is reachable")
				return toolkit.ConfirmNo, nil
			}
			gate := newGate()

			allowed, reason, aerr := gate.Approve(context.Background(), "use-1", "stream_rm", "stream rm", "stream rm ORDERS", "ai:confirm")
			Expect(aerr).ToNot(HaveOccurred())
			Expect(allowed).To(BeFalse())
			Expect(reason).To(Equal(NoTerminalReason))
		})

		// A run whose context is over is not asked and is not answered either. It used to
		// manufacture a refusal, which on a checkpointed run outlived the process.
		It("Should report an already-canceled run as an error without prompting", func() {
			prompter.approveFn = func(toolkit.GateRequest) (toolkit.ConfirmChoice, error) {
				Fail("must not prompt once the run is canceled")
				return toolkit.ConfirmNo, nil
			}
			gate := newGate()

			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			allowed, reason, aerr := gate.Approve(ctx, "use-1", "stream_rm", "stream rm", "stream rm ORDERS", "ai:confirm")
			Expect(aerr).To(MatchError(toolkit.ErrPromptAborted))
			Expect(aerr).To(MatchError(context.Canceled))
			Expect(allowed).To(BeFalse())
			Expect(reason).To(BeEmpty())
		})
	})

	Describe("Standing approvals", func() {
		It("Should honor a grant the source already holds without prompting", func() {
			prompter.approveFn = func(toolkit.GateRequest) (toolkit.ConfirmChoice, error) {
				Fail("must not prompt for a tool that carries a standing approval")
				return toolkit.ConfirmNo, nil
			}
			src := &fakeApprovals{granted: map[string]bool{"stream_rm": true}}
			gate := NewConfirmGate(prompter, src)

			allowed, reason, aerr := gate.Approve(context.Background(), "use-1", "stream_rm", "stream rm", "stream rm ORDERS", "ai:confirm")
			Expect(aerr).ToNot(HaveOccurred())
			Expect(allowed).To(BeTrue())
			Expect(reason).To(BeEmpty())
			Expect(src.recorded).To(BeEmpty())
		})

		It("Should record a grant only for an always answer", func() {
			choice := toolkit.ConfirmOnce
			prompter.approveFn = func(toolkit.GateRequest) (toolkit.ConfirmChoice, error) { return choice, nil }
			src := &fakeApprovals{}
			gate := NewConfirmGate(prompter, src)

			gate.Approve(context.Background(), "use-1", "stream_rm", "stream rm", "stream rm ORDERS", "ai:confirm")
			Expect(src.recorded).To(BeEmpty())

			choice = toolkit.ConfirmNo
			gate.Approve(context.Background(), "use-1", "stream_rm", "stream rm", "stream rm ORDERS", "ai:confirm")
			Expect(src.recorded).To(BeEmpty())

			choice = toolkit.ConfirmAlways
			gate.Approve(context.Background(), "use-1", "stream_rm", "stream rm", "stream rm ORDERS", "ai:confirm")
			Expect(src.recorded).To(Equal([]string{"stream_rm"}))
		})

		It("Should record nothing when the operator never answered", func() {
			prompter.approveFn = func(toolkit.GateRequest) (toolkit.ConfirmChoice, error) {
				return toolkit.ConfirmAlways, fmt.Errorf("%w: interrupt", toolkit.ErrPromptAborted)
			}
			src := &fakeApprovals{}
			gate := NewConfirmGate(prompter, src)

			_, _, aerr := gate.Approve(context.Background(), "use-1", "stream_rm", "stream rm", "stream rm ORDERS", "ai:confirm")
			Expect(aerr).To(MatchError(toolkit.ErrPromptAborted))
			Expect(src.recorded).To(BeEmpty())
		})

		It("Should end the run when the grant cannot be recorded", func() {
			prompter.approveFn = func(toolkit.GateRequest) (toolkit.ConfirmChoice, error) { return toolkit.ConfirmAlways, nil }
			src := &fakeApprovals{grantErr: errors.New("journal is gone")}
			gate := NewConfirmGate(prompter, src)

			allowed, reason, aerr := gate.Approve(context.Background(), "use-1", "stream_rm", "stream rm", "stream rm ORDERS", "ai:confirm")
			Expect(aerr).To(MatchError(src.grantErr))
			Expect(allowed).To(BeFalse())
			Expect(reason).To(BeEmpty())
		})

		// A grant can outlive the process that recorded it, so it is consulted below the
		// two checks that establish somebody is there to be asked. Honoring one first
		// would run a gated command on a resume with no operator present.
		It("Should not honor a grant with no operator reachable", func() {
			prompter.canPrompt = false
			src := &fakeApprovals{granted: map[string]bool{"stream_rm": true}}
			gate := NewConfirmGate(prompter, src)

			allowed, reason, aerr := gate.Approve(context.Background(), "use-1", "stream_rm", "stream rm", "stream rm ORDERS", "ai:confirm")
			Expect(aerr).ToNot(HaveOccurred())
			Expect(allowed).To(BeFalse())
			Expect(reason).To(Equal(NoTerminalReason))
		})

		It("Should not honor a grant once the run is canceled", func() {
			src := &fakeApprovals{granted: map[string]bool{"stream_rm": true}}
			gate := NewConfirmGate(prompter, src)

			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			allowed, _, aerr := gate.Approve(ctx, "use-1", "stream_rm", "stream rm", "stream rm ORDERS", "ai:confirm")
			Expect(aerr).To(MatchError(toolkit.ErrPromptAborted))
			Expect(allowed).To(BeFalse())
		})

		// A one-shot approval is the answer an operator gave for a call the run had
		// suspended on. It authorizes that dispatch and is spent by it.
		It("Should honor a one-shot approval for its own call and spend it", func() {
			calls := 0
			prompter.approveFn = func(toolkit.GateRequest) (toolkit.ConfirmChoice, error) {
				calls++
				return toolkit.ConfirmNo, nil
			}
			src := &fakeApprovals{calls: map[string]bool{"use-1": true}}
			gate := NewConfirmGate(prompter, src)

			allowed, reason, aerr := gate.Approve(context.Background(), "use-1", "stream_rm", "stream rm", "stream rm ORDERS", "ai:confirm")
			Expect(aerr).ToNot(HaveOccurred())
			Expect(allowed).To(BeTrue())
			Expect(reason).To(BeEmpty())
			Expect(calls).To(Equal(0))
			Expect(src.taken).To(Equal([]string{"use-1"}))

			// The same tool called again is a call nobody approved.
			allowed, _, aerr = gate.Approve(context.Background(), "use-2", "stream_rm", "stream rm", "stream rm BILLING", "ai:confirm")
			Expect(aerr).ToNot(HaveOccurred())
			Expect(allowed).To(BeFalse())
			Expect(calls).To(Equal(1))
		})

		It("Should not honor a one-shot approval with no operator reachable", func() {
			prompter.canPrompt = false
			src := &fakeApprovals{calls: map[string]bool{"use-1": true}}
			gate := NewConfirmGate(prompter, src)

			allowed, reason, aerr := gate.Approve(context.Background(), "use-1", "stream_rm", "stream rm", "stream rm ORDERS", "ai:confirm")
			Expect(aerr).ToNot(HaveOccurred())
			Expect(allowed).To(BeFalse())
			Expect(reason).To(Equal(NoTerminalReason))
			Expect(src.taken).To(BeEmpty(), "an approval nobody could act on is not spent")
		})
	})

	Describe("ConfirmDeniedResult", func() {
		It("Should be a non-error tool_result carrying the reason", func() {
			block := ConfirmDeniedResult("tool-1", "the operator declined")
			Expect(block.ToolUseID).To(Equal("tool-1"))
			Expect(block.IsError).To(BeFalse())

			var outcome confirmDeniedOutcome
			Expect(json.Unmarshal([]byte(block.Content), &outcome)).To(Succeed())
			Expect(outcome.Allowed).To(BeFalse())
			Expect(outcome.Reason).To(Equal("the operator declined"))
		})
	})

	Describe("SanitizeCommandLine", func() {
		It("Should strip terminal escape sequences from model-supplied argument values", func() {
			Expect(SanitizeCommandLine("stream rm \x1b[31mORDERS\x1b[0m")).To(Equal("stream rm ORDERS"))
		})
	})
})
