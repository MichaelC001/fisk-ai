//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package multiplex

import (
	"context"
	"fmt"
	"os/exec"
	"time"
)

// Herdr sets HERDR_ENV to 1 in a pane it started, and names the pane, the socket it
// listens on and, where it exports one, the binary to report through. The socket takes
// the same reports the binary does and is not used.
const (
	herdrEnv     = "HERDR_ENV"
	herdrPaneID  = "HERDR_PANE_ID"
	herdrBinPath = "HERDR_BIN_PATH"
)

// herdrCommand is the binary to report through where the environment names none. Herdr
// documents HERDR_BIN_PATH as inherited, and a pane of a released herdr can carry the
// pane id and the socket without it, so the name is looked up on the path rather than
// leaving the integration silent in the terminal it was written for.
const herdrCommand = "herdr"

// herdrSource identifies these reports to herdr, which keeps a state per source and
// hands lifecycle authority back when that source releases it. It is fixed, since a
// source that varied between runs would leave herdr holding a state for every run this
// terminal ever had. It names the integration; what the pane is labeled with is the
// agent, which herdrAgent supplies only where the caller named none.
const (
	herdrSource = "custom:fisk-ai"
	herdrAgent  = "fisk-ai"
)

// herdrName is what the operator is shown when herdr claimed this process.
const herdrName = "herdr"

// herdrWaitDelay is how long a report that ignored its cancellation has to exit before
// its pipes are closed under it.
const herdrWaitDelay = time.Second

// detectHerdr claims the process when herdr started it.
func detectHerdr(env func(string) string, agent string) *reporter {
	h := newHerdr(env, agent, exec.LookPath, runCommand)
	if h == nil {
		return nil
	}

	return newReporter(herdrName, h.deliver, h.release)
}

// newHerdr reads the pane this process runs in, and returns nil when it runs in none.
//
// Both the switch and the pane are required, so the integration stays a no-op wherever
// either is missing: a shell that exported one by hand, or a process that inherited a
// stale environment from somewhere else, reports nothing. A pane with nothing to report
// through is the same no-op, since there is no way to say anything about it.
//
// lookPath finds the binary where the environment names none, and run executes it. Both
// are the os/exec pair for a caller and a recorder for a test.
func newHerdr(env func(string) string, agent string, lookPath func(string) (string, error), run func(ctx context.Context, bin string, args ...string) error) *herdr {
	if env(herdrEnv) != "1" {
		return nil
	}

	pane := env(herdrPaneID)
	if pane == "" {
		return nil
	}

	if agent == "" {
		agent = herdrAgent
	}

	bin := env(herdrBinPath)
	if bin == "" {
		found, err := lookPath(herdrCommand)
		if err != nil {
			return nil
		}

		bin = found
	}

	return &herdr{pane: pane, bin: bin, agent: agent, run: run}
}

// herdr reports through the CLI herdr documents for integrations, rather than through
// its socket, which takes the same three requests. The binary is the portable path: it
// is the one herdr's own integrations use, and its location is what HERDR_BIN_PATH is
// for.
type herdr struct {
	pane  string
	bin   string
	agent string
	run   func(ctx context.Context, bin string, args ...string) error
}

// deliver tells herdr what to show for this pane.
//
// No sequence number is sent. Herdr wants one where reports can arrive out of order, and
// these cannot: one goroutine delivers them, one at a time, in the order it took them.
// Sending one would introduce the failure it exists to prevent, since the source is fixed
// and a run killed outright never releases it: the next run in this pane would start its
// numbering below what herdr already had, and could have every report discarded as stale.
// The flag and its value are separate arguments: herdr refuses the joined form outright,
// answering "unknown option: --source=..." and reporting nothing. It reads a value that
// begins with a dash as the value, so a description quoting a command line arrives whole.
func (h *herdr) deliver(ctx context.Context, rep report) error {
	args := []string{
		"pane", "report-agent", h.pane,
		"--source", herdrSource,
		"--agent", h.agent,
		"--state", string(rep.state),
	}
	if rep.message != "" {
		args = append(args, "--message", rep.message)
	}

	return h.run(ctx, h.bin, args...)
}

// release hands this pane's lifecycle authority back, so herdr stops showing a state
// for a process that has left.
func (h *herdr) release(ctx context.Context) error {
	return h.run(ctx, h.bin,
		"pane", "release-agent", h.pane,
		"--source", herdrSource,
		"--agent", h.agent,
	)
}

// runCommand executes one report.
//
// The environment is inherited: the multiplexer started this process and already has it.
// The three streams are left nil, which os/exec wires to the null device, because the
// full-screen view owns the terminal for the length of the run and a child writing to
// the inherited descriptors would draw over it.
func runCommand(ctx context.Context, bin string, args ...string) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = herdrWaitDelay

	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("reporting to the multiplexer: %w", err)
	}

	return nil
}
