//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/pii"
)

// piiGuard scans the text entering a conversation and acts on what it finds, as
// harness.pii asks.
//
// It is installed by the run rather than by a caller, over whatever hooks the caller
// supplied, and it runs after theirs at each point: a hook that rewrites a prompt hands
// its rewrite to the scan, so text a caller's own policy introduced is scanned like any
// other. That ordering is the whole reason this wraps rather than occupying the hook
// itself, which would make the two mutually exclusive.
//
// Everything it does happens before the text is appended to the conversation, journaled,
// traced or sent, so what it removes reaches neither the model nor the store nor a
// collector.
type piiGuard struct {
	scanner *pii.Scanner
	events  Events

	// toldRedacted and toldWithheld hold the once-per-run advisory. Every occurrence
	// still reaches the log and the span; only the operator-facing warning is limited,
	// since the renderer prints each warning twice and a run redacting a tool result on
	// every call would bury its own answer. Plain bools because every hook runs on the
	// one run goroutine.
	toldRedacted bool
	toldWithheld bool
}

// newPIIGuard builds the guard for a run, or nil when harness.pii.mode is off.
//
// A mode other than off that cannot build its scanner fails the run. The alternative is a
// run that carries on with the scanning silently not happening, which is the failure the
// feature exists to prevent, and the operator asked for it in a file rather than being
// given it by default.
func newPIIGuard(cfg *config.Config, events Events) (*piiGuard, error) {
	if cfg.PIIMode() == config.PIIModeOff {
		return nil, nil
	}

	scanner, err := pii.New(pii.Options{Mode: pii.Mode(cfg.PIIMode())})
	if err != nil {
		return nil, fmt.Errorf("harness.pii is %s but its scanner could not be built: %w", cfg.PIIMode(), err)
	}

	return &piiGuard{scanner: scanner, events: events}, nil
}

// close releases the scanner. It is nil-safe, so a run closes the guard without asking
// whether it has one.
func (g *piiGuard) close() {
	if g == nil {
		return
	}

	_ = g.scanner.Close()
}

// wrap returns h with the two scanned points wrapped. It is nil-safe and returns h
// untouched when there is no guard, so the caller wires this the same way whether or not
// the feature is on.
func (g *piiGuard) wrap(h Hooks) Hooks {
	if g == nil {
		return h
	}

	inner := h

	h.UserPromptSubmit = func(ctx context.Context, info UserPromptSubmitInfo) (UserPromptSubmitResult, error) {
		res, err := inner.fireUserPromptSubmit(ctx, info)
		if err != nil || res.Deny {
			return res, err
		}

		// What the caller's hook left behind, which is its rewrite where it made one.
		text := info.Text
		if res.Rewrite != "" {
			text = res.Rewrite
		}

		scan, serr := g.scanner.Scan(ctx, text)
		if serr != nil {
			g.warnWithheld("the prompt", pii.Result{}, serr)

			return UserPromptSubmitResult{
				Deny:       true,
				DenyReason: "it could not be scanned for personal data, and harness.pii does not let text through unscanned",
			}, nil
		}

		if !scan.Found() {
			return res, nil
		}

		if g.scanner.Mode() == pii.ModeReject {
			g.warnWithheld("the prompt", scan, nil)

			return UserPromptSubmitResult{
				Deny:       true,
				DenyReason: fmt.Sprintf("it contains personal data (%s) and harness.pii.mode is reject", strings.Join(scan.TypeNames(), ", ")),
			}, nil
		}

		g.warnRedacted("the prompt", scan)
		res.Rewrite = scan.Text

		return res, nil
	}

	h.PostToolUse = func(ctx context.Context, info PostToolUseInfo) (PostToolUseResult, error) {
		res, err := inner.firePostToolUse(ctx, info)
		if err != nil {
			return res, err
		}

		output, isError := info.Output, info.IsError
		if res.Replace {
			output, isError = res.Output, res.IsError
		}

		scan, serr := g.scanner.Scan(ctx, output)
		if serr != nil {
			g.warnWithheld(info.ToolName, pii.Result{}, serr)

			// The cause reaches the operator through the advisory and the log, not the
			// model: what the model can do about it is the same either way, and a tool
			// result is a place text ends up quoted.
			return PostToolUseResult{
				Replace: true,
				Output:  "This tool's output was withheld: it could not be scanned for personal data, and harness.pii does not let text through unscanned.",
				IsError: true,
			}, nil
		}

		if !scan.Found() {
			return res, nil
		}

		if g.scanner.Mode() == pii.ModeReject {
			g.warnWithheld(info.ToolName, scan, nil)

			return PostToolUseResult{
				Replace: true,
				Output:  fmt.Sprintf("This tool's output was withheld: it contains personal data (%s) and harness.pii.mode is reject. The values are not available to this run.", strings.Join(scan.TypeNames(), ", ")),
				IsError: true,
			}, nil
		}

		g.warnRedacted(info.ToolName, scan)

		return PostToolUseResult{Replace: true, Output: scan.Text, IsError: isError}, nil
	}

	return h
}

// warnRedacted raises the redaction advisory, once per run.
func (g *piiGuard) warnRedacted(where string, scan pii.Result) {
	if g.toldRedacted {
		return
	}
	g.toldRedacted = true

	g.events.Warn(Warning{Kind: WarnPIIRedacted, Name: where, Count: scan.Count, Params: scan.TypeNames()})
}

// warnWithheld raises the withholding advisory, once per run. A scan that failed carries
// its cause and no counts, which is how a reader tells "personal data was found and
// refused" from "the scanner broke".
func (g *piiGuard) warnWithheld(where string, scan pii.Result, err error) {
	if g.toldWithheld {
		return
	}
	g.toldWithheld = true

	g.events.Warn(Warning{Kind: WarnPIIWithheld, Name: where, Count: scan.Count, Params: scan.TypeNames(), Err: err})
}
