//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package mcpclient

import (
	"regexp"
	"slices"
	"strings"

	"github.com/choria-io/fisk-ai/config"
)

// urlInText matches a url inside a longer piece of text. It stops at whitespace and at
// the quotes and brackets that usually delimit a url in a message, and it keeps braces,
// so a "${VAR}" reference survives to be recognized as one.
var urlInText = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.\-]*://[^\s"'<>\\]+`)

// urlTrailers are the punctuation that follows a url in a message rather than belonging
// to it. They are trimmed off a match before it is redacted, so a colon separating the
// url from the rest of the message is not read as part of the last query parameter.
const urlTrailers = `.,;:!)]`

// redactedValue replaces a credential, and is the word config.RedactURL writes, so one
// message that was redacted by both rules reads the same throughout.
const redactedValue = "REDACTED"

// minSecretLength is the shortest resolved "${VAR}" value that is redacted by value. A
// variable that resolves to "", "1" or "dev" is a switch, a port or a tenant rather
// than a credential, and replacing a string that short would blank the digits and words
// it happens to match all over an unrelated message, leaving an operator with an error
// they cannot read. Eight is under the length of every token an MCP service issues and
// over the length of the values that collide, so nothing an operator needs to keep is
// left printed and nothing they need to read is destroyed.
const minSecretLength = 8

// serverSecrets are the values the "${VAR}" references in a server's url resolve to,
// which is the credential an operator kept in a variable rather than in the file. They
// are what the redaction searches for, so a credential a service takes in the path,
// as Zapier's "/api/mcp/s/<token>/mcp" does, is caught where the structure of a url
// says nothing.
//
// A value shorter than minSecretLength and one the lookup does not have are left out,
// and the longest is searched for first, so a value that contains another is replaced
// whole rather than leaving a fragment of itself behind. A server with no url, and one
// whose url references nothing, has no secrets.
func serverSecrets(server config.MCPServer, lookup func(string) (string, bool)) []string {
	if server.URL == "" || lookup == nil {
		return nil
	}

	names, err := config.EnvReferences(server.URL)
	if err != nil {
		return nil
	}

	var out []string
	for _, name := range names {
		value, ok := lookup(name)
		if !ok || len(value) < minSecretLength || slices.Contains(out, value) {
			continue
		}

		out = append(out, value)
	}

	slices.SortStableFunc(out, func(a string, b string) int { return len(b) - len(a) })

	return out
}

// redacted returns an error whose message has every value in secrets and every url in
// it redacted, and which errors.Is and errors.As still see through to the error it
// wraps.
//
// The endpoint of a server that authenticates by query parameter or by a path segment
// holds the credential itself, and the errors that reach here were written by the SDK
// and by net/http, which quote the url they were dialing: a connection refused by
// "https://mcp.example.net/mcp/?apiKey=secret" reports that url in full. Only the
// message changes, so the deadline and cancellation checks the callers make still
// match.
//
// Pass the secrets of the server the error came from. A nil set is for a caller that
// has no server in hand, such as the import walking every server's outcome, and leaves
// the structural redaction of the url to do what it can: an error a session returned
// through Sessions.Use has already had that server's values replaced.
func redacted(err error, secrets []string) error {
	if err == nil {
		return nil
	}

	return &redactedError{err: err, secrets: secrets}
}

// redactedError is the error redacted returns.
type redactedError struct {
	err     error
	secrets []string
}

func (e *redactedError) Error() string { return redactEndpoints(e.err.Error(), e.secrets) }

func (e *redactedError) Unwrap() error { return e.err }

// redactEndpoints replaces every value in secrets wherever it appears in text, and
// redacts every url in what is left with config.RedactURL, so what an error shows and
// what a human-facing surface shows are the same rule.
//
// The two rules answer different halves of the problem. A value that came from a
// "${VAR}" reference is known exactly, so it goes from the path, the host and the
// message around the url as well as from the query. A credential written into the
// file as a literal has no reference to key off, so only the structure is left to go
// on: the userinfo, the query values and the fragment are replaced, and a literal in
// the path is printed as it stands.
func redactEndpoints(text string, secrets []string) string {
	for _, secret := range secrets {
		if len(secret) < minSecretLength {
			continue
		}

		text = strings.ReplaceAll(text, secret, redactedValue)
	}

	if !strings.Contains(text, "://") {
		return text
	}

	return urlInText.ReplaceAllStringFunc(text, func(match string) string {
		trimmed := strings.TrimRight(match, urlTrailers)

		return config.RedactURL(trimmed) + match[len(trimmed):]
	})
}
