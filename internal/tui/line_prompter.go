//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/AlecAivazis/survey/v2"
	"github.com/AlecAivazis/survey/v2/terminal"
	"golang.org/x/term"

	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// linePrompter is the line-oriented toolkit.Prompter used by the default CLI. It
// wraps AlecAivazis/survey, rendering prompts and traces on stderr so stdout stays
// clean for a piped final answer. survey turns an interrupt (Ctrl-C) or a closed
// input into an error, which is reported as toolkit.ErrPromptAborted so the caller
// does not record it as a decline; it cannot be canceled mid-prompt through ctx, so
// the caller performs the authoritative context and no-terminal deny checks before a
// prompt is ever shown, and tcellPrompter (which can select on ctx) is used when the
// full-screen view owns the screen.
//
// survey holds the terminal in raw mode while a prompt is up, so an interrupt there
// is delivered to survey rather than as a signal: the process never sees SIGINT and
// this error is the only evidence the operator meant to stop.
type linePrompter struct {
	// out is where prompt headers and command traces are written: os.Stderr in a
	// real run, redirected in tests to keep their output quiet. The interactive
	// survey widgets themselves render on os.Stderr, since survey needs a real
	// terminal file for its cursor control.
	out io.Writer
}

// NewLinePrompter returns the line-oriented CLI Prompter, writing its prompt headers
// and command traces to stderr.
func NewLinePrompter() toolkit.Prompter {
	return &linePrompter{out: os.Stderr}
}

// CanPrompt reports whether stdin is an interactive terminal: survey reads and draws
// on the real terminal, so it can only ask an operator when one is attached. This is
// the single place the CLI's terminal check now lives; the agent, the confirm gate,
// and the human-in-the-loop tools consult it through the Prompter rather than testing
// the terminal themselves.
func (p *linePrompter) CanPrompt() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// ApproveCommand renders the confirm-gate header and command trace, then asks the
// operator to allow the command. The safe option (No) is listed first so survey
// highlights it and a reflexive Enter declines; an interrupt or closed input
// returns an error the caller treats as a denial.
func (p *linePrompter) ApproveCommand(_ context.Context, req toolkit.GateRequest) (toolkit.ConfirmChoice, error) {
	printGateHeader(p.out, req)

	options := []string{
		"No, do not run it",
		"Yes, run it once",
		fmt.Sprintf("Yes, and allow %q (any arguments) for the rest of this conversation", req.Command),
	}

	idx := 0
	err := survey.AskOne(
		&survey.Select{Message: "Run this command?", Options: options},
		&idx,
		survey.WithStdio(os.Stdin, os.Stderr, os.Stderr),
	)
	if err != nil {
		return toolkit.ConfirmNo, promptError(err)
	}

	switch idx {
	case 1:
		return toolkit.ConfirmOnce, nil
	case 2:
		printAlwaysNote(p.out, req.Command)
		return toolkit.ConfirmAlways, nil
	default:
		return toolkit.ConfirmNo, nil
	}
}

// Confirm prompts for a yes/no answer. The bound value starts false and the prompt
// defaults to No, so an operator who simply presses Enter declines; survey returns
// an error on Ctrl-C or a closed input, which the caller treats as a denial.
func (p *linePrompter) Confirm(_ context.Context, question string) (bool, error) {
	printPromptSeparator(p.out)

	confirmed := false
	err := survey.AskOne(
		&survey.Confirm{Message: question, Default: false},
		&confirmed,
		survey.WithStdio(os.Stdin, os.Stderr, os.Stderr),
	)
	if err != nil {
		return false, promptError(err)
	}

	return confirmed, nil
}

// Select prompts the operator to choose one of options and returns its index. The
// index starts at -1, so a Ctrl-C or closed input (survey returns an error) leaves
// no choice rather than defaulting to the first option.
func (p *linePrompter) Select(_ context.Context, question string, options []string) (int, error) {
	printPromptSeparator(p.out)

	idx := -1
	err := survey.AskOne(
		&survey.Select{Message: question, Options: options},
		&idx,
		survey.WithStdio(os.Stdin, os.Stderr, os.Stderr),
	)
	if err != nil {
		return -1, promptError(err)
	}

	return idx, nil
}

// Input prompts the operator for a free-text value, pre-filled with def (which may
// be empty). survey returns an error on Ctrl-C or a closed input.
func (p *linePrompter) Input(_ context.Context, question, def string) (string, error) {
	printPromptSeparator(p.out)

	answer := ""
	err := survey.AskOne(
		&survey.Input{Message: question, Default: def},
		&answer,
		survey.WithStdio(os.Stdin, os.Stderr, os.Stderr),
	)
	if err != nil {
		return "", promptError(err)
	}

	return answer, nil
}

// promptError classifies what survey reported. An interrupt or a closed input is an
// operator who did not answer, which the caller must not record as a decision; any
// other failure is a prompt that could not be put, which is a denial like any other.
func promptError(err error) error {
	if err == nil {
		return nil
	}

	if errors.Is(err, terminal.InterruptErr) || errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: %w", toolkit.ErrPromptAborted, err)
	}

	return err
}

// printGateHeader writes the confirm-gate approval header and command trace: a
// separator to set the question apart from the model's preceding narration, a line
// naming the command and the tag that gated it, and the sanitized command line.
func printGateHeader(out io.Writer, req toolkit.GateRequest) {
	printPromptSeparator(out)
	fmt.Fprintf(out, "confirmation required: %q carries tag %q\n", req.Command, req.Tag)
	fmt.Fprintf(out, "-> %s\n", req.Display)
}

// printAlwaysNote confirms to the operator that a standing allow was recorded, so
// they know the tool will not be asked about again in this conversation.
func printAlwaysNote(out io.Writer, commandPath string) {
	fmt.Fprintf(out, "confirmation: will not ask again for %q in this conversation\n", commandPath)
}

// printPromptSeparator writes a blank line to w before an interactive prompt, so a
// question put to the operator is visually set apart from the model's preceding
// narration or a tool's output rather than running straight on from it.
func printPromptSeparator(w io.Writer) {
	fmt.Fprintln(w)
}
