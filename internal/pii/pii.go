//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// Package pii finds personal data in text and either redacts it or reports that it is
// there, so a caller can keep it out of somewhere it should not go: a model provider, a
// stored transcript, a telemetry collector.
//
// Detection is pattern matching in two passes: ferret-scan's validators, then a small set
// of credential patterns of this package's own, which cover the API keys and NATS
// credentials the validators find only when they appear in an assignment. Both are
// best-effort in both directions: they miss real values and they flag text that is no
// such thing. They lower what leaks; they do not gate it, and no decision to send data
// somewhere should rest on them.
//
// A Scanner holds a pre-built validator set, so build one and reuse it: construction
// costs about as much as several scans. It is safe for concurrent use.
package pii

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/awslabs/ferret-scan/v2/pkg/redact"
)

// Mode is what a caller does with the personal data a scan finds. It is the vocabulary
// the harness config uses, so a configured value converts to one directly.
type Mode string

const (
	// ModeRedact replaces each value found with a placeholder.
	ModeRedact Mode = "redact"
	// ModeReject reports the finding without producing usable text, for a caller that
	// refuses the whole input rather than rewriting it.
	ModeReject Mode = "reject"
	// ModeOff scans nothing. New refuses it rather than returning an inert Scanner: a
	// scanner that finds nothing and a scanner that was never asked to look are
	// different things, and only the caller can tell them apart.
	ModeOff Mode = "off"
)

// ErrModeOff is returned by New for ModeOff.
var ErrModeOff = errors.New("mode is off, so there is nothing to build a scanner for")

// DefaultChecks are the validators a Scanner runs when Options.Checks is empty.
//
// It is every validator ferret-scan offers on this path except three, each dropped for
// what it does to ordinary text rather than for what it costs:
//
//   - INTELLECTUAL_PROPERTY matches a copyright line, so it rewrites the license header
//     of every source file an agent reads.
//   - PERSON_NAME matches names in prose, and matches them unevenly: of three colleagues
//     named in one sentence it took two.
//   - IP_ADDRESS marked nothing in a server report, a Kubernetes service or a log line
//     full of addresses, so it earns no place in a set a caller cannot narrow.
//
// The names are ferret-scan's own validator IDs. A name this build does not recognize is
// dropped silently by the engine, so New checks them itself.
var DefaultChecks = []string{
	"BANK_ACCOUNT",
	"CLOUD_RESOURCES",
	"CREDIT_CARD",
	"DATE_OF_BIRTH",
	"DRIVERS_LICENSE",
	"EMAIL",
	"MEDICAL_ID",
	"OTP",
	"PASSPORT",
	"PHONE",
	"PHYSICAL_ADDRESS",
	"SECRETS",
	"SSN",
	"VIN",
}

// MaxTextBytes is the largest text a Scan accepts. Anything longer is an error rather
// than a truncated scan, since a scan of the first part of a value is a scan that missed
// the rest of it.
const MaxTextBytes = redact.MaxInputBytes

// Options configures a Scanner.
type Options struct {
	// Mode is what the caller will do with a finding. A Scanner does not act on it; it
	// carries the value so one object answers both what was found and what to do about
	// it.
	Mode Mode
	// Checks names the validators to run, using ferret-scan's validator IDs. Empty
	// takes DefaultChecks. Every name must be one ValidCheckNames reports, since a name
	// the engine does not recognize would otherwise be dropped and leave the caller
	// believing it was scanning for something it was not.
	Checks []string
}

// Scanner finds personal data in text. Build one with New and reuse it.
type Scanner struct {
	mode   Mode
	checks []string
	engine *redact.Engine
}

// Result is what one scan found. It carries no matched value: Text is the text with each
// value replaced, and Types counts the findings by the type the validator assigned, which
// is what an advisory or an audit record may safely repeat.
//
// The type names come from whichever pass found the value. ferret-scan's are narrower
// than the check names that select them: an EMAIL check reports BUSINESS or PERSONAL, and
// CREDIT_CARD reports the card brand. This package's own credential pass reports
// API_KEY, BEARER_TOKEN, NATS_CREDENTIALS and NATS_SEED.
type Result struct {
	// Text is the text with each value found replaced by a placeholder naming its type.
	// It is the original text when nothing was found.
	Text string
	// Types counts the findings by type.
	Types map[string]int
	// Count is how many values were found.
	Count int
}

// Found reports whether the scan found anything.
func (r Result) Found() bool { return r.Count > 0 }

// TypeNames lists the types found, sorted, for a message that says what was in the text
// without repeating any of it.
func (r Result) TypeNames() []string {
	out := make([]string, 0, len(r.Types))
	for name := range r.Types {
		out = append(out, name)
	}
	slices.Sort(out)

	return out
}

// New builds a Scanner. Close it when the caller is done with it.
//
// It refuses ModeOff (ErrModeOff) and any mode it does not know, and it refuses a check
// name the engine would drop, so a Scanner that exists is one that scans for what it was
// asked to scan for.
func New(opts Options) (*Scanner, error) {
	switch opts.Mode {
	case ModeRedact, ModeReject:
	case ModeOff:
		return nil, ErrModeOff
	default:
		return nil, fmt.Errorf("unknown pii mode %q: must be redact, reject or off", opts.Mode)
	}

	checks := opts.Checks
	if len(checks) == 0 {
		checks = DefaultChecks
	}

	valid := redact.ValidCheckNames()
	for _, name := range checks {
		if !slices.Contains(valid, name) {
			return nil, fmt.Errorf("unknown pii check %q: accepted checks are %v", name, valid)
		}
	}

	// Simple names the type in the placeholder ([EMAIL-REDACTED]) where the
	// format-preserving default would emit a masked value like ****-****-****-0004. A
	// model reads the first as an absence and the second as something it may use.
	engine, err := redact.NewEngine(redact.EngineOptions{
		Checks:   slices.Clone(checks),
		Strategy: redact.Simple,
	})
	if err != nil {
		return nil, fmt.Errorf("building the pii scanner: %w", err)
	}

	return &Scanner{mode: opts.Mode, checks: slices.Clone(checks), engine: engine}, nil
}

// Mode is the mode the Scanner was built with.
func (s *Scanner) Mode() Mode { return s.mode }

// Checks lists the validators this Scanner runs, for a caller reporting what is in
// effect.
func (s *Scanner) Checks() []string { return slices.Clone(s.checks) }

// Close releases the Scanner. Scans after it return an error.
func (s *Scanner) Close() error {
	return s.engine.Close()
}

// Scan finds the personal data in text.
//
// Empty text is not scanned and comes back as an empty Result, which is the same answer
// a scan would give and is the common case for a caller feeding it every tool result.
// Text over MaxTextBytes is an error rather than a partial scan.
//
// Two passes run: ferret-scan's validators, then this package's own credential patterns
// over what they left. See secretPatterns for what the second pass is for.
func (s *Scanner) Scan(ctx context.Context, text string) (Result, error) {
	if text == "" {
		return Result{}, nil
	}

	res, err := s.engine.Redact(ctx, redact.Request{Text: text})
	if err != nil {
		return Result{}, fmt.Errorf("scanning for personal data: %w", err)
	}

	out := Result{
		Text:  res.Redacted,
		Types: res.AuditRecord().FindingsByType,
		Count: len(res.Findings()),
	}

	redacted, types, count := scanSecrets(out.Text)
	if count == 0 {
		return out, nil
	}

	out.Text = redacted
	out.Count += count

	// AuditRecord's map is the engine's to keep, so the merged counts go in one of ours.
	merged := make(map[string]int, len(out.Types)+len(types))
	maps.Copy(merged, out.Types)
	for name, n := range types {
		merged[name] += n
	}
	out.Types = merged

	return out, nil
}
