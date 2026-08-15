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

	// newGate builds a gate wired to the spec's fakePrompter.
	newGate := func() *ConfirmGate {
		return NewConfirmGate(prompter)
	}

	Describe("Approve", func() {
		It("Should allow once without remembering the tool", func() {
			calls := 0
			prompter.approveFn = func(toolkit.GateRequest) (toolkit.ConfirmChoice, error) {
				calls++
				return toolkit.ConfirmOnce, nil
			}
			gate := newGate()

			allowed, reason, aerr := gate.Approve(context.Background(), "stream_rm", "stream rm", "stream rm ORDERS", "ai:confirm")
			Expect(aerr).ToNot(HaveOccurred())
			Expect(allowed).To(BeTrue())
			Expect(reason).To(BeEmpty())

			// A second call prompts again because "once" is not remembered.
			allowed, _, _ = gate.Approve(context.Background(), "stream_rm", "stream rm", "stream rm BILLING", "ai:confirm")
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

			allowed, _, _ := gate.Approve(context.Background(), "stream_rm", "stream rm", "stream rm ORDERS", "ai:confirm")
			Expect(allowed).To(BeTrue())

			// A later call with different arguments is allowed without re-prompting; the
			// gate emits no trace of its own, the caller renders the command's line.
			allowed, _, _ = gate.Approve(context.Background(), "stream_rm", "stream rm", "stream rm EVERYTHING", "ai:confirm")
			Expect(allowed).To(BeTrue())
			Expect(calls).To(Equal(1))

			// A different tool is still asked about.
			allowed, _, _ = gate.Approve(context.Background(), "server_run", "server run", "server run", "ai:confirm")
			Expect(allowed).To(BeTrue())
			Expect(calls).To(Equal(2))
		})

		It("Should pass the triggering tag and command line to the prompter", func() {
			prompter.approveFn = func(toolkit.GateRequest) (toolkit.ConfirmChoice, error) { return toolkit.ConfirmOnce, nil }
			gate := newGate()

			allowed, _, _ := gate.Approve(context.Background(), "stream_rm", "stream rm", "stream rm ORDERS", "impact:rw")
			Expect(allowed).To(BeTrue())
			Expect(prompter.lastGateReq).To(Equal(toolkit.GateRequest{Command: "stream rm", Display: "stream rm ORDERS", Tag: "impact:rw"}))
		})

		It("Should decline when the operator says no, and re-prompt next time", func() {
			calls := 0
			prompter.approveFn = func(toolkit.GateRequest) (toolkit.ConfirmChoice, error) {
				calls++
				return toolkit.ConfirmNo, nil
			}
			gate := newGate()

			allowed, reason, aerr := gate.Approve(context.Background(), "stream_rm", "stream rm", "stream rm ORDERS", "ai:confirm")
			Expect(aerr).ToNot(HaveOccurred())
			Expect(allowed).To(BeFalse())
			Expect(reason).To(ContainSubstring("declined"))
			Expect(reason).To(ContainSubstring("do not retry"))

			// No is not sticky: the same command is asked about again.
			gate.Approve(context.Background(), "stream_rm", "stream rm", "stream rm ORDERS", "ai:confirm")
			Expect(calls).To(Equal(2))
		})

		// A prompt that could not be rendered is not an operator who walked away, so it
		// stays a denial: the command is gated and nothing established that it may run.
		It("Should deny by default when the prompt fails for any other reason", func() {
			prompter.approveFn = func(toolkit.GateRequest) (toolkit.ConfirmChoice, error) {
				return toolkit.ConfirmNo, errors.New("the terminal went away")
			}
			gate := newGate()

			allowed, reason, aerr := gate.Approve(context.Background(), "stream_rm", "stream rm", "stream rm ORDERS", "ai:confirm")
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

			allowed, reason, aerr := gate.Approve(context.Background(), "stream_rm", "stream rm", "stream rm ORDERS", "ai:confirm")
			Expect(aerr).To(MatchError(toolkit.ErrPromptAborted))
			Expect(allowed).To(BeFalse())
			Expect(reason).To(BeEmpty())
		})

		It("Should deny with the no-terminal reason when no operator is reachable", func() {
			prompter.canPrompt = false
			prompter.approveFn = func(toolkit.GateRequest) (toolkit.ConfirmChoice, error) {
				Fail("must not prompt when no operator is reachable")
				return toolkit.ConfirmNo, nil
			}
			gate := newGate()

			allowed, reason, aerr := gate.Approve(context.Background(), "stream_rm", "stream rm", "stream rm ORDERS", "ai:confirm")
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

			allowed, reason, aerr := gate.Approve(ctx, "stream_rm", "stream rm", "stream rm ORDERS", "ai:confirm")
			Expect(aerr).To(MatchError(toolkit.ErrPromptAborted))
			Expect(aerr).To(MatchError(context.Canceled))
			Expect(allowed).To(BeFalse())
			Expect(reason).To(BeEmpty())
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
