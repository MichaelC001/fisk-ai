//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agenttest

import (
	"errors"
	"fmt"
)

// ErrNotScripted is what a fake returns when it is reached with nothing to answer
// with: a ScriptedPrompter method whose closure the spec never installed, or nil work
// handed to Queue.Submit. The fake records a ScriptingFault, returns an error wrapping
// this sentinel, and lets the run continue to its natural end, so a spec driving several
// runs at once reports the scripting mistake rather than the test framework reporting
// a failure raised from the wrong goroutine.
var ErrNotScripted = errors.New("agenttest: not scripted")

// ScriptingFault is one call a fake could not answer. Both ScriptedPrompter and Queue
// record them and return them from ScriptingFaults, so a spec asserts after the run what
// the run asked for and the script did not have. It is also the error the call itself
// returns, wrapped in ErrNotScripted, so the two read the same.
//
// The name says scripting because serve.FaultingEndpoint calls something else a fault:
// a channel's own error stream, reported by a Faults method the Queue does not implement.
type ScriptingFault struct {
	// Call names the method that was reached: "ApproveCommand", "Confirm", "Select",
	// "Input" or "Submit".
	Call string

	// Subject is what the call was about: the gated command for ApproveCommand, the
	// question put to the operator for Confirm, Select and Input, and the channel name
	// for Submit.
	Subject string

	// Missing names what the spec did not supply: the closure field for a prompter
	// method, and the position of the nil work for Submit.
	Missing string
}

// Error names the call, its subject and what was missing.
func (f ScriptingFault) Error() string {
	return fmt.Sprintf("%s %q: %s", f.Call, f.Subject, f.Missing)
}
