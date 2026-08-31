//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"

	. "github.com/onsi/ginkgo/v2"

	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// fakePrompter is a scripted toolkit.Prompter for tests: each interactive method
// delegates to a closure the test installs, so a spec drives the confirm gate and
// the human-in-the-loop handlers without a terminal. A method whose closure is
// unset fails the spec, since reaching it means the caller prompted when it should
// have resolved the outcome itself (for example when no terminal is attached).
//
// internal/toolkit/builtin has its own copy of this fake for its specs.
type fakePrompter struct {
	canPrompt   bool
	approveFn   func(toolkit.GateRequest) (toolkit.ConfirmChoice, error)
	confirmFn   func(string) (bool, error)
	selectFn    func(string, []string) (int, error)
	inputFn     func(string, string) (string, error)
	lastGateReq toolkit.GateRequest
}

// fakeApprovals is a GateApprovals a spec can inspect: granted seeds what the
// conversation already carries, calls seeds the one-shot approvals a supplied answer
// left, and recorded is what the gate wrote, in order.
type fakeApprovals struct {
	granted  map[string]bool
	calls    map[string]bool
	taken    []string
	recorded []string
	grantErr error
}

func (f *fakeApprovals) Granted(tool string) bool { return f.granted[tool] }

func (f *fakeApprovals) TakeCall(toolUseID string) bool {
	if !f.calls[toolUseID] {
		return false
	}

	delete(f.calls, toolUseID)
	f.taken = append(f.taken, toolUseID)

	return true
}

func (f *fakeApprovals) Grant(tool string) error {
	if f.grantErr != nil {
		return f.grantErr
	}

	if f.granted == nil {
		f.granted = map[string]bool{}
	}
	f.granted[tool] = true
	f.recorded = append(f.recorded, tool)

	return nil
}

func (f *fakePrompter) CanPrompt() bool { return f.canPrompt }

func (f *fakePrompter) ApproveCommand(_ context.Context, req toolkit.GateRequest) (toolkit.ConfirmChoice, error) {
	f.lastGateReq = req
	if f.approveFn == nil {
		Fail("ApproveCommand called but no approveFn was set")
	}
	return f.approveFn(req)
}

func (f *fakePrompter) Confirm(_ context.Context, question string) (bool, error) {
	if f.confirmFn == nil {
		Fail("Confirm called but no confirmFn was set")
	}
	return f.confirmFn(question)
}

func (f *fakePrompter) Select(_ context.Context, question string, options []string) (int, error) {
	if f.selectFn == nil {
		Fail("Select called but no selectFn was set")
	}
	return f.selectFn(question, options)
}

func (f *fakePrompter) Input(_ context.Context, question, def string) (string, error) {
	if f.inputFn == nil {
		Fail("Input called but no inputFn was set")
	}
	return f.inputFn(question, def)
}
