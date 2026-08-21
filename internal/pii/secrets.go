//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package pii

import (
	"regexp"
	"strings"
)

// The credential patterns this package applies on top of ferret-scan's validators.
//
// They exist because of what the SECRETS validator keys off. For a key with no internal
// structure it matches the assignment rather than the value: OPENROUTER_API_KEY=sk-or-v1-...
// is found, and so are api_key: <value> and {"api_key": "<value>"}, while the same key
// pasted bare into a prompt, written into a sentence, or sent as an Authorization header
// is not. Formats carrying their own structure (a JWT, an AKIA... access key, a PEM
// private key block) are found wherever they appear.
//
// The case that goes unfound is the one most worth having: an operator pasting a key into
// a prompt, or a tool printing one in prose. These patterns anchor on the value itself, so
// the context around it does not matter.
//
// They are deliberately anchored on a distinctive prefix or an envelope, never on
// "something that looks random". A base64 blob, a git SHA and a UUID are all
// indistinguishable from an opaque key by shape alone, and a rule broad enough to catch
// the key redacts all three.
//
// This is a supplement, not a secret scanner. A credential shape that is not listed here
// and that ferret-scan does not recognize passes through.
var secretPatterns = []struct {
	// Type is the name reported in Result.Types and written into the placeholder.
	Type string
	// Re matches the whole credential, since the match is what gets replaced.
	Re *regexp.Regexp
}{
	// The NATS envelopes come first so that a seed inside one is taken as part of the
	// block rather than separately. Both are what a credentials file holds, and the
	// closing line carries a different number of dashes to the opening one.
	{"NATS_CREDENTIALS", regexp.MustCompile(`(?s)-{3,}\s*BEGIN NATS USER JWT\s*-{3,}.*?-{3,}\s*END NATS USER JWT\s*-{3,}`)},
	{"NATS_SEED", regexp.MustCompile(`(?s)-{3,}\s*BEGIN [A-Z ]*NKEY SEED\s*-{3,}.*?-{3,}\s*END [A-Z ]*NKEY SEED\s*-{3,}`)},
	// A bare nkey seed: S, the entity letter, then 56 base32 characters. The leading S
	// is what makes it a seed rather than the public key beside it, which is not secret.
	{"NATS_SEED", regexp.MustCompile(`\bS[AUONC][A-Z2-7]{56}\b`)},

	// Provider API keys, longest prefix first so the specific name is the one reported.
	{"API_KEY", regexp.MustCompile(`\bsk-or-v1-[A-Za-z0-9]{32,}\b`)},
	{"API_KEY", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{24,}`)},
	{"API_KEY", regexp.MustCompile(`\bsk-proj-[A-Za-z0-9_-]{24,}`)},
	{"API_KEY", regexp.MustCompile(`\bsk-[A-Za-z0-9]{32,}\b`)},
	{"API_KEY", regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}`)},
	{"API_KEY", regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`)},
	{"API_KEY", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`)},

	// An Authorization header names its own value a credential, so the shape of that
	// value does not have to be recognized. The length floor keeps the placeholder text
	// of an example ("Bearer YOUR_TOKEN_HERE") out of it.
	{"BEARER_TOKEN", regexp.MustCompile(`(?i)\b(bearer|token)\s+[A-Za-z0-9._~+/=-]{24,}`)},
}

// scanSecrets replaces every credential these patterns recognize, returning the text with
// each match replaced, the counts by type, and the total.
//
// It runs over text ferret-scan has already redacted, so a credential both find is
// replaced once: what this sees in that case is a placeholder, which matches nothing here.
func scanSecrets(text string) (string, map[string]int, int) {
	if text == "" {
		return text, nil, 0
	}

	types := map[string]int{}
	total := 0

	for _, p := range secretPatterns {
		text = p.Re.ReplaceAllStringFunc(text, func(match string) string {
			replaced, ok := replaceMatch(p.Type, match)
			if !ok {
				return match
			}

			types[p.Type]++
			total++

			return replaced
		})
	}

	if total == 0 {
		return text, nil, 0
	}

	return text, types, total
}

// replaceMatch returns what one match is replaced with, and whether it is replaced at all.
//
// A bearer match keeps the word that introduced it and replaces only the credential after
// it, so the line still reads as the header it was. It is also the one pattern that can
// decline: it recognizes its value by the word in front of it rather than by any shape, so
// it insists on a digit somewhere in that value. Without it "token" followed by a long
// enough identifier is a match, and this codebase is full of long enough identifiers.
func replaceMatch(kind string, match string) (string, bool) {
	if kind != "BEARER_TOKEN" {
		return placeholder(kind), true
	}

	word, value, found := strings.Cut(match, " ")
	if !found || !strings.ContainsAny(value, "0123456789") {
		return "", false
	}

	return word + " " + placeholder(kind), true
}

// placeholder is what a redacted value is replaced with, in the form ferret-scan's own
// simple strategy uses, so one piece of text does not carry two conventions.
func placeholder(kind string) string {
	return "[" + kind + "-REDACTED]"
}
