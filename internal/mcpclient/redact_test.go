//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package mcpclient

import (
	"context"
	"errors"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
)

var _ = Describe("Redaction", func() {
	lookup := func(values map[string]string) func(string) (string, bool) {
		return func(name string) (string, bool) {
			value, ok := values[name]
			return value, ok
		}
	}

	Describe("serverSecrets", func() {
		It("should resolve the references in the url", func() {
			secrets := serverSecrets(config.MCPServer{
				Name: "zapier",
				URL:  "https://mcp.zapier.com/api/mcp/s/${ZAPIER_KEY}/mcp",
			}, lookup(map[string]string{"ZAPIER_KEY": "abc123secretkey"}))

			Expect(secrets).To(Equal([]string{"abc123secretkey"}))
		})

		It("should search for the longest value first", func() {
			secrets := serverSecrets(config.MCPServer{
				Name: "docs",
				URL:  "https://mcp.example.net/${SHORT}/mcp?apiKey=${LONG}",
			}, lookup(map[string]string{"SHORT": "abc123secret", "LONG": "abc123secret-and-more"}))

			Expect(secrets).To(Equal([]string{"abc123secret-and-more", "abc123secret"}))
		})

		// A variable holding a port, a tenant or a switch is not a credential, and
		// replacing a string that short would blank the digits and words it matches
		// elsewhere in a message an operator has to read.
		It("should leave out a value too short to search for", func() {
			secrets := serverSecrets(config.MCPServer{
				Name: "docs",
				URL:  "http://${HOST}:${PORT}/${MODE}/mcp?apiKey=${TOKEN}&debug=${OFF}",
			}, lookup(map[string]string{
				"HOST":  "127.0.0.1",
				"PORT":  "9000",
				"MODE":  "1",
				"TOKEN": "abc123secret",
				"OFF":   "",
			}))

			Expect(secrets).To(Equal([]string{"abc123secret", "127.0.0.1"}))
		})

		It("should have nothing for a server with no url and none for a variable that is not set", func() {
			Expect(serverSecrets(config.MCPServer{Name: "docs", Command: "npx"}, lookup(nil))).To(BeEmpty())
			Expect(serverSecrets(config.MCPServer{Name: "docs", URL: "https://mcp.example.net/${ABSENT}/mcp"}, lookup(nil))).To(BeEmpty())
			Expect(serverSecrets(config.MCPServer{Name: "docs", URL: "https://mcp.example.net/mcp"}, lookup(nil))).To(BeEmpty())
		})
	})

	Describe("redactEndpoints", func() {
		It("should redact a url quoted inside a message", func() {
			Expect(redactEndpoints(`Post "https://mcp.example.net/mcp?apiKey=abc123": dial tcp: refused`, nil)).
				To(Equal(`Post "https://mcp.example.net/mcp?apiKey=REDACTED": dial tcp: refused`))
		})

		It("should keep the punctuation that follows a url out of its query", func() {
			Expect(redactEndpoints("dialing https://mcp.example.net/mcp?apiKey=abc123: refused", nil)).
				To(Equal("dialing https://mcp.example.net/mcp?apiKey=REDACTED: refused"))
		})

		It("should redact the userinfo of a url in a message", func() {
			Expect(redactEndpoints("reaching https://operator:hunter2@mcp.example.net/mcp failed", nil)).
				To(Equal("reaching https://REDACTED@mcp.example.net/mcp failed"))
		})

		It("should leave a message with no url alone", func() {
			Expect(redactEndpoints(`connecting to mcp server "docs": no such host`, nil)).
				To(Equal(`connecting to mcp server "docs": no such host`))
		})

		// Zapier and Composio take the credential in a path segment, where the shape of
		// a url says nothing about what is a token and what is a route, so the value the
		// reference resolved to is what is searched for.
		It("should redact a known value in the path", func() {
			Expect(redactEndpoints(`Post "https://mcp.zapier.com/api/mcp/s/abc123secret/mcp": dial tcp: refused`, []string{"abc123secret"})).
				To(Equal(`Post "https://mcp.zapier.com/api/mcp/s/REDACTED/mcp": dial tcp: refused`))
		})

		It("should redact a known value wherever it appears, url or not", func() {
			Expect(redactEndpoints("the key abc123secret was refused by https://ops:abc123secret@mcp.example.net/abc123secret", []string{"abc123secret"})).
				To(Equal("the key REDACTED was refused by https://REDACTED@mcp.example.net/REDACTED"))
		})

		// This is the residue the two rules leave: a literal in the path has no
		// reference to key off and no structure to give it away, so it is printed. It is
		// pinned here so the limit is a decision rather than a surprise.
		It("should print a literal credential in the path", func() {
			Expect(redactEndpoints(`Post "https://mcp.zapier.com/api/mcp/s/abc123secret/mcp": refused`, nil)).
				To(Equal(`Post "https://mcp.zapier.com/api/mcp/s/abc123secret/mcp": refused`))
		})

		It("should refuse to search for a value too short, however it arrived", func() {
			Expect(redactEndpoints("listening on 127.0.0.1:1 in mode 1: refused", []string{"1", ""})).
				To(Equal("listening on 127.0.0.1:1 in mode 1: refused"))
		})
	})

	Describe("redacted", func() {
		It("should redact the message and keep the error reachable", func() {
			inner := fmt.Errorf("dialing https://mcp.example.net/mcp?apiKey=abc123: %w", context.DeadlineExceeded)
			err := redacted(fmt.Errorf(`connecting to mcp server "docs": %w`, inner), nil)

			Expect(err).To(MatchError(context.DeadlineExceeded))
			Expect(err.Error()).ToNot(ContainSubstring("abc123"))
			Expect(err).To(MatchError(ContainSubstring("apiKey=REDACTED")))
			Expect(errors.Unwrap(err).Error()).To(ContainSubstring("abc123"))
		})

		It("should redact the server's own values and keep the error reachable", func() {
			inner := fmt.Errorf("dialing https://mcp.zapier.com/api/mcp/s/abc123secret/mcp: %w", context.DeadlineExceeded)
			err := redacted(fmt.Errorf(`connecting to mcp server "zapier": %w`, inner), []string{"abc123secret"})

			Expect(err).To(MatchError(context.DeadlineExceeded))
			Expect(err.Error()).ToNot(ContainSubstring("abc123secret"))
			Expect(err).To(MatchError(ContainSubstring("/api/mcp/s/REDACTED/mcp")))
			Expect(errors.Unwrap(err).Error()).To(ContainSubstring("abc123secret"))
		})

		It("should pass a nil error through", func() {
			Expect(redacted(nil, []string{"abc123secret"})).To(BeNil())
		})
	})
})
