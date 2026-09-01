//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agenttest_test

import (
	"context"
	"errors"
	"fmt"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/toolkit"
)

var _ = Describe("ScriptedPrompter", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("Should implement toolkit.Prompter", func() {
		var p toolkit.Prompter = agenttest.NewScriptedPrompter(GinkgoTB())
		Expect(p).ToNot(BeNil())
	})

	It("Should answer each interaction through the closure the spec installed", func() {
		p := agenttest.NewScriptedPrompter(GinkgoTB())

		p.ApproveFn = func(req toolkit.GateRequest) (toolkit.ConfirmChoice, error) {
			Expect(req.Command).To(Equal("stream rm"))
			return toolkit.ConfirmOnce, nil
		}
		p.ConfirmFn = func(question string) (bool, error) {
			Expect(question).To(Equal("proceed?"))
			return true, nil
		}
		p.SelectFn = func(_ string, options []string) (int, error) {
			Expect(options).To(Equal([]string{"a", "b"}))
			return 1, nil
		}
		p.InputFn = func(_, def string) (string, error) {
			return def + "!", nil
		}

		choice, err := p.ApproveCommand(ctx, toolkit.GateRequest{Command: "stream rm", Tag: "ai:confirm"})
		Expect(err).ToNot(HaveOccurred())
		Expect(choice).To(Equal(toolkit.ConfirmOnce))

		yes, err := p.Confirm(ctx, "proceed?")
		Expect(err).ToNot(HaveOccurred())
		Expect(yes).To(BeTrue())

		idx, err := p.Select(ctx, "which?", []string{"a", "b"})
		Expect(err).ToNot(HaveOccurred())
		Expect(idx).To(Equal(1))

		answer, err := p.Input(ctx, "name?", "default")
		Expect(err).ToNot(HaveOccurred())
		Expect(answer).To(Equal("default!"))

		Expect(p.ScriptingFaults()).To(BeEmpty())
	})

	It("Should record a scripting fault and decline where no closure was installed", func() {
		p := agenttest.NewScriptedPrompter(GinkgoTB())

		choice, err := p.ApproveCommand(ctx, toolkit.GateRequest{Command: "stream rm"})
		Expect(choice).To(Equal(toolkit.ConfirmNo))
		Expect(err).To(MatchError(agenttest.ErrNotScripted))

		yes, err := p.Confirm(ctx, "proceed?")
		Expect(yes).To(BeFalse())
		Expect(err).To(MatchError(agenttest.ErrNotScripted))

		idx, err := p.Select(ctx, "which?", []string{"a"})
		Expect(idx).To(Equal(-1), "no option index can be mistaken for it")
		Expect(err).To(MatchError(agenttest.ErrNotScripted))

		answer, err := p.Input(ctx, "name?", "default")
		Expect(answer).To(BeEmpty())
		Expect(err).To(MatchError(agenttest.ErrNotScripted))

		faults := p.ScriptingFaults()
		Expect(faults).To(HaveLen(4))
		Expect(faults[0]).To(Equal(agenttest.ScriptingFault{
			Call: "ApproveCommand", Subject: "stream rm", Missing: "no ApproveFn was set",
		}))
		Expect(faults[1]).To(Equal(agenttest.ScriptingFault{
			Call: "Confirm", Subject: "proceed?", Missing: "no ConfirmFn was set",
		}))
		Expect(faults[2].Call).To(Equal("Select"))
		Expect(faults[3].Call).To(Equal("Input"))
	})

	It("Should return a copy of the scripting faults", func() {
		p := agenttest.BuildScriptedPrompter()

		_, err := p.Confirm(ctx, "proceed?")
		Expect(err).To(HaveOccurred())

		faults := p.ScriptingFaults()
		faults[0].Call = "rewritten"

		Expect(p.ScriptingFaults()[0].Call).To(Equal("Confirm"))
	})

	It("Should report an operator until NoOperator says otherwise", func() {
		p := agenttest.NewScriptedPrompter(GinkgoTB())
		Expect(p.CanPrompt()).To(BeTrue())

		Expect(p.NoOperator()).To(BeIdenticalTo(p), "it chains off the constructor")
		Expect(p.CanPrompt()).To(BeFalse())
	})

	It("Should report the last gate request it was asked about", func() {
		p := agenttest.NewScriptedPrompter(GinkgoTB())
		Expect(p.LastGateRequest()).To(Equal(toolkit.GateRequest{}), "no gate has been reached")

		p.ApproveFn = func(toolkit.GateRequest) (toolkit.ConfirmChoice, error) {
			return toolkit.ConfirmNo, nil
		}

		_, err := p.ApproveCommand(ctx, toolkit.GateRequest{ToolUseID: "call-1", Command: "stream rm"})
		Expect(err).ToNot(HaveOccurred())
		_, err = p.ApproveCommand(ctx, toolkit.GateRequest{ToolUseID: "call-2", Command: "stream purge"})
		Expect(err).ToNot(HaveOccurred())

		Expect(p.LastGateRequest().ToolUseID).To(Equal("call-2"))
		Expect(p.LastGateRequest().Command).To(Equal("stream purge"))
	})

	It("Should report a gate request the spec never scripted an answer for", func() {
		p := agenttest.BuildScriptedPrompter()

		_, err := p.ApproveCommand(ctx, toolkit.GateRequest{ToolUseID: "call-1", Command: "stream rm"})
		Expect(err).To(MatchError(agenttest.ErrNotScripted))

		Expect(p.LastGateRequest().Command).To(Equal("stream rm"),
			"the request is recorded whether or not a closure answered it")
	})

	// A spec driving concurrent runs shares one prompter across them and reads the gate
	// request and the faults while the runs are in flight.
	It("Should answer concurrent runs while the spec reads what it recorded", func() {
		const runs = 8
		const each = 25

		p := agenttest.BuildScriptedPrompter()
		p.ApproveFn = func(toolkit.GateRequest) (toolkit.ConfirmChoice, error) {
			return toolkit.ConfirmOnce, nil
		}

		var wg sync.WaitGroup
		for i := 0; i < runs; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer GinkgoRecover()

				for j := 0; j < each; j++ {
					_, err := p.ApproveCommand(ctx, toolkit.GateRequest{Command: fmt.Sprintf("cmd %d", j)})
					Expect(err).ToNot(HaveOccurred())

					_, err = p.Confirm(ctx, "unscripted")
					Expect(err).To(MatchError(agenttest.ErrNotScripted))

					p.CanPrompt()
				}
			}()
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer GinkgoRecover()

			for i := 0; i < runs*each; i++ {
				p.LastGateRequest()
				p.ScriptingFaults()
			}
		}()

		wg.Wait()

		Expect(p.ScriptingFaults()).To(HaveLen(runs * each))
		Expect(p.LastGateRequest().Command).To(HavePrefix("cmd "))
	})

	It("Should wrap the fault it records in the error the call answers with", func() {
		p := agenttest.BuildScriptedPrompter()

		_, err := p.Select(ctx, "which?", nil)

		var fault agenttest.ScriptingFault
		Expect(errors.As(err, &fault)).To(BeTrue())
		Expect(fault).To(Equal(p.ScriptingFaults()[0]))
	})
})
