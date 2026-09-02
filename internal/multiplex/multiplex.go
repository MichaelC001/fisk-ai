//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// Package multiplex tells a terminal multiplexer hosting this process what the run
// inside it is doing.
//
// A multiplexer that arranges agents in panes shows each one as working, waiting for a
// person, or needing a decision, so somebody watching several can see which wants them.
// It cannot tell any of that from the pane's output, so the agent says so itself.
//
// Which multiplexer, if any, is answered by the environment: each one exports variables
// naming itself and the pane, and Detect claims the process for the first that did.
// Outside a pane it claims nothing and a caller wires nothing.
//
// Reporting is best effort and it never fails a run. Every call returns before the
// report reaches the multiplexer, a report can be superseded by a newer one before it is
// sent, and a delivery that fails is dropped: a supervisor that went away must not
// affect the conversation it was watching.
//
// A report is delivered by running the multiplexer's own CLI, so Detect looks the binary
// up on PATH where the environment does not name it, and each state change starts a short
// lived process, which is the integration herdr documents. A program that must spawn
// nothing calls Detect only where a multiplexer is expected.
package multiplex

// StateReporter is told what a run is doing, so a multiplexer hosting it can show its
// pane as working, idle or blocked. It is safe for concurrent use.
//
// Working and Idle are the two halves of a turn: a run works between the moment it takes
// a prompt and the moment its turn ends, and it is idle from then until the next prompt,
// which is where a person is being waited for. Blocked is narrower and it is what the
// pane exists to surface: the run has stopped to ask somebody something and cannot go on
// until they answer.
//
// Use the ClientHooks a caller hands to an a2a client, or drive these directly, and not
// both: two writers with no ordering between them leave the pane showing whichever won.
type StateReporter interface {
	// Name is the multiplexer being reported to, for a caller that shows the operator
	// which one claimed the process. Reporting is invisible from inside a pane, so
	// without it there is no telling an integration that works from one that is absent.
	Name() string

	// Working reports that the agent is doing the work.
	Working()

	// Blocked reports that the run has stopped for a person to decide something, with a
	// short description of what is waiting. The multiplexer shows the reason in a list
	// beside other panes, so it is sanitized and cut to one short line.
	Blocked(reason string)

	// Idle reports that the agent is waiting for a person rather than working, which is
	// what a finished turn leaves behind.
	Idle()

	// Close gives up this process's claim on the pane, so the multiplexer stops showing
	// a state for a program that has gone. Reports still in flight are delivered or
	// dropped first, so none can arrive after it and claim the pane again. Reports made
	// after it are ignored.
	Close()
}

// detectors are consulted in order, and the first to recognize its own environment
// claims the process. A new multiplexer is a new function here.
var detectors = []func(env func(string) string, agent string) *reporter{
	detectHerdr,
}

// Detect returns the reporter for the multiplexer hosting this process, and nil when no
// multiplexer does, which is the ordinary case of a terminal nobody is supervising.
//
// env reads the environment, which is os.Getenv for a caller and a table for a test. It
// must not be nil.
//
// agent labels the pane with the agent running in it, which is the agent's identity
// rather than the name of this program: somebody watching six panes is watching six
// agents, and being told each of them is fisk-ai tells them nothing. Empty falls back to
// the program's own name.
//
// Detect runs exec.LookPath and then a process. Where the environment names a pane but
// no binary to report through, it searches PATH for the multiplexer's CLI and claims
// nothing when it is not installed. A reporter it returns has already posted an idle
// report, and its worker delivers that report by running the CLI once: a process that
// has just started is waiting on the person who started it, and the pane says so from
// the moment it is claimed.
func Detect(env func(string) string, agent string) StateReporter {
	for _, detect := range detectors {
		r := detect(env, agent)
		if r != nil {
			return r
		}
	}

	return nil
}
