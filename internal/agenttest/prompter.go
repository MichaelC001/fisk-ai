//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agenttest

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// ScriptedPrompter is a toolkit.Prompter whose four interactive methods each defer
// to a closure the spec installs, so a test drives the confirm gate and the
// human-in-the-loop handlers without a terminal. A method reached with no closure set
// records a ScriptingFault and returns an error wrapping ErrNotScripted, since reaching
// it means the run prompted where it should have resolved the outcome itself (for example
// on the no-operator path). The run continues to its natural end and the spec asserts
// ScriptingFaults afterward. It replaces the per-package fakePrompter copies the tree had
// grown.
type ScriptedPrompter struct {
	tb testing.TB

	// canPrompt is what CanPrompt reports; true by default, since a scripted operator
	// is present. NoOperator flips it to model the no-operator path.
	canPrompt bool

	// ApproveFn answers the confirm-gate approval; ConfirmFn, SelectFn and InputFn
	// answer the ask_human_confirm, ask_human_select and ask_human_input builtins.
	// Install only the ones a spec expects to reach.
	ApproveFn func(toolkit.GateRequest) (toolkit.ConfirmChoice, error)
	ConfirmFn func(question string) (bool, error)
	SelectFn  func(question string, options []string) (int, error)
	InputFn   func(question, def string) (string, error)

	// LastGateRequest is the last request ApproveCommand received, for assertion.
	LastGateRequest toolkit.GateRequest

	mu     sync.Mutex
	faults []ScriptingFault
}

// NewScriptedPrompter returns a prompter with no closures installed and CanPrompt
// reporting true; set the fields for the interactions the spec drives.
func NewScriptedPrompter(tb testing.TB) *ScriptedPrompter {
	tb.Helper()

	p := BuildScriptedPrompter()
	p.tb = tb

	return p
}

// BuildScriptedPrompter is NewScriptedPrompter without a testing.TB, for a func Example
// or any other caller outside a test. Either constructor produces a prompter that reports
// an unscripted call the same way, through the returned error and ScriptingFaults.
func BuildScriptedPrompter() *ScriptedPrompter {
	return &ScriptedPrompter{canPrompt: true}
}

// CanPrompt reports whether an operator is reachable; true unless NoOperator was set.
func (p *ScriptedPrompter) CanPrompt() bool { return p.canPrompt }

// NoOperator makes CanPrompt report false, modeling a run with no operator (the
// default-deny path the confirm gate and human-in-the-loop tools take).
func (p *ScriptedPrompter) NoOperator() *ScriptedPrompter {
	p.canPrompt = false
	return p
}

// ApproveCommand answers the confirm gate through ApproveFn, and with no ApproveFn
// installed records a ScriptingFault and declines.
func (p *ScriptedPrompter) ApproveCommand(_ context.Context, req toolkit.GateRequest) (toolkit.ConfirmChoice, error) {
	p.LastGateRequest = req
	if p.ApproveFn == nil {
		return toolkit.ConfirmNo, p.recordFault("ApproveCommand", req.Command, "no ApproveFn was set")
	}

	return p.ApproveFn(req)
}

// Confirm answers ask_human_confirm through ConfirmFn, and with no ConfirmFn installed
// records a ScriptingFault and answers false.
func (p *ScriptedPrompter) Confirm(_ context.Context, question string) (bool, error) {
	if p.ConfirmFn == nil {
		return false, p.recordFault("Confirm", question, "no ConfirmFn was set")
	}

	return p.ConfirmFn(question)
}

// Select answers ask_human_select through SelectFn, and with no SelectFn installed
// records a ScriptingFault and answers -1, which no option index can be mistaken for.
func (p *ScriptedPrompter) Select(_ context.Context, question string, options []string) (int, error) {
	if p.SelectFn == nil {
		return -1, p.recordFault("Select", question, "no SelectFn was set")
	}

	return p.SelectFn(question, options)
}

// Input answers ask_human_input through InputFn, and with no InputFn installed records
// a ScriptingFault and answers the empty string.
func (p *ScriptedPrompter) Input(_ context.Context, question, def string) (string, error) {
	if p.InputFn == nil {
		return "", p.recordFault("Input", question, "no InputFn was set")
	}

	return p.InputFn(question, def)
}

// ScriptingFaults returns every call the prompter could not answer, in the order the run
// made them. A spec that scripted every interaction its run reaches gets an empty slice.
// The copy is safe to read from the spec goroutine while a run is still in flight.
func (p *ScriptedPrompter) ScriptingFaults() []ScriptingFault {
	p.mu.Lock()
	defer p.mu.Unlock()

	return append([]ScriptingFault(nil), p.faults...)
}

// recordFault keeps the fault for ScriptingFaults and returns the error the call answers
// with.
func (p *ScriptedPrompter) recordFault(call string, subject string, missing string) error {
	fault := ScriptingFault{Call: call, Subject: subject, Missing: missing}

	p.mu.Lock()
	p.faults = append(p.faults, fault)
	p.mu.Unlock()

	return fmt.Errorf("%w: %w", ErrNotScripted, fault)
}
