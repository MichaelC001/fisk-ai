//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package pii_test

import (
	"context"
	"strings"
	"testing"

	"github.com/awslabs/ferret-scan/v2/pkg/redact"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/pii"
)

func TestPII(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "PII")
}

// An address is what the detector is most dependable on, so the specs that need
// something found use one. They assert that the value is gone rather than matching the
// placeholder, which is the detector's wording and not ours to pin.
const withEmail = "mail the invoice to alice.smith@example.com when you get a moment"

var _ = Describe("Scanner", func() {
	var scanner *pii.Scanner

	newScanner := func(opts pii.Options) *pii.Scanner {
		s, err := pii.New(opts)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(s.Close()).To(Succeed()) })

		return s
	}

	Describe("New", func() {
		It("Should refuse the off mode", func() {
			s, err := pii.New(pii.Options{Mode: pii.ModeOff})
			Expect(err).To(MatchError(pii.ErrModeOff))
			Expect(s).To(BeNil())
		})

		It("Should refuse an unknown mode", func() {
			s, err := pii.New(pii.Options{Mode: pii.Mode("scrub")})
			Expect(err).To(MatchError(ContainSubstring(`unknown pii mode "scrub"`)))
			Expect(s).To(BeNil())
		})

		It("Should refuse a check the engine would drop", func() {
			s, err := pii.New(pii.Options{Mode: pii.ModeRedact, Checks: []string{"EMAIL", "EMIAL"}})
			Expect(err).To(MatchError(ContainSubstring(`unknown pii check "EMIAL"`)))
			Expect(s).To(BeNil())
		})

		It("Should take the default checks when none are named", func() {
			scanner = newScanner(pii.Options{Mode: pii.ModeRedact})
			Expect(scanner.Checks()).To(Equal(pii.DefaultChecks))
			Expect(scanner.Mode()).To(Equal(pii.ModeRedact))
		})

		It("Should take the checks it is given", func() {
			scanner = newScanner(pii.Options{Mode: pii.ModeReject, Checks: []string{"EMAIL"}})
			Expect(scanner.Checks()).To(Equal([]string{"EMAIL"}))
			Expect(scanner.Mode()).To(Equal(pii.ModeReject))
		})
	})

	Describe("Scan", func() {
		BeforeEach(func() {
			scanner = newScanner(pii.Options{Mode: pii.ModeRedact})
		})

		It("Should find nothing in empty text", func() {
			res, err := scanner.Scan(context.Background(), "")
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Found()).To(BeFalse())
			Expect(res.Count).To(Equal(0))
			Expect(res.Text).To(BeEmpty())
		})

		It("Should leave text with nothing personal in it alone", func() {
			const text = "restart the payments service and tell me what the logs say"

			res, err := scanner.Scan(context.Background(), text)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Found()).To(BeFalse())
			Expect(res.Text).To(Equal(text))
			Expect(res.TypeNames()).To(BeEmpty())
		})

		It("Should replace what it finds and report the type without the value", func() {
			res, err := scanner.Scan(context.Background(), withEmail)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Found()).To(BeTrue())
			Expect(res.Count).To(Equal(1))
			Expect(res.Text).NotTo(ContainSubstring("alice.smith@example.com"))
			Expect(res.Text).To(ContainSubstring("mail the invoice to"))
			Expect(res.TypeNames()).To(HaveLen(1))
			Expect(strings.Join(res.TypeNames(), "")).NotTo(ContainSubstring("alice"))
		})

		It("Should error once the scanner is closed", func() {
			s, err := pii.New(pii.Options{Mode: pii.ModeRedact})
			Expect(err).NotTo(HaveOccurred())
			Expect(s.Close()).To(Succeed())

			_, err = s.Scan(context.Background(), withEmail)
			Expect(err).To(MatchError(ContainSubstring("scanning for personal data")))
		})
	})

	// The credential pass exists for the values ferret-scan finds only in an assignment.
	// Every key here is a made-up value in a real shape.
	Describe("credentials", func() {
		const (
			openRouter = "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"
			anthropic  = "sk-ant-api03-AbCdEf123456GhIjKlMnOpQrStUvWxYz0123456789AbCdEfGhIj-AAAAAA"
			seed       = "SUAGZWSNK2QQ4Q7HTLNQZKWZ7HZMD5EFHU5S6ELFQKMBNMHQFVQPBCPRAA"
		)

		BeforeEach(func() {
			scanner = newScanner(pii.Options{Mode: pii.ModeRedact})
		})

		It("Should find a key wherever it appears, not only in an assignment", func() {
			for _, text := range []string{
				openRouter,
				"use " + openRouter + " for the call",
				"my openrouter key is " + openRouter + ", try it",
				"OPENROUTER_API_KEY=" + openRouter,
				`curl -H "Authorization: Bearer ` + openRouter + `"`,
			} {
				res, err := scanner.Scan(context.Background(), text)
				Expect(err).NotTo(HaveOccurred())
				Expect(res.Found()).To(BeTrue(), "for %q", text)
				Expect(res.Text).NotTo(ContainSubstring(openRouter), "for %q", text)
			}
		})

		It("Should find the other credential shapes it claims", func() {
			for name, text := range map[string]string{
				"anthropic": "the key is " + anthropic,
				"openai":    "sk-AbCdEf123456GhIjKlMnOpQrStUvWxYz0123456789",
				"slack":     "posted with xoxb-123456789012-1234567890123-AbCdEfGhIjKlMnOpQrStUvWx",
				"github":    "clone with ghp_AbCdEf123456GhIjKlMnOpQrStUvWxYz0123",
				"nats seed": "the seed is " + seed,
				"nats creds": `-----BEGIN NATS USER JWT-----
eyJ0eXAiOiJKV1QiLCJhbGciOiJlZDI1NTE5LW5rZXkifQ
------END NATS USER JWT------`,
			} {
				res, err := scanner.Scan(context.Background(), text)
				Expect(err).NotTo(HaveOccurred())
				Expect(res.Found()).To(BeTrue(), "for %s", name)
			}
		})

		It("Should keep the word that introduced a bearer token", func() {
			res, err := scanner.Scan(context.Background(), "Authorization: Bearer "+openRouter)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Text).To(HavePrefix("Authorization: Bearer "))
			Expect(res.Text).NotTo(ContainSubstring(openRouter))
		})

		It("Should report the type without the value", func() {
			res, err := scanner.Scan(context.Background(), "the key is "+openRouter)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.TypeNames()).To(ContainElement("API_KEY"))
			Expect(strings.Join(res.TypeNames(), " ")).NotTo(ContainSubstring("sk-or"))
		})

		// Anchored on a prefix or an envelope, never on "looks random": a git SHA, a
		// UUID and a base64 blob are indistinguishable from an opaque key by shape.
		It("Should leave things that merely look random alone", func() {
			for name, text := range map[string]string{
				"git sha":     "commit 647cc907a2b8e4f1c39d8a5b2e7f0c4d9a1b3e6f is the merge",
				"uuid":        "session 21665e15-643d-4b57-a3fd-af02b215d9f2 ended",
				"base64":      "payload eyJoZWxsbyI6IndvcmxkIiwiY291bnQiOjQyfQ== decoded fine",
				"hex digest":  "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
				"nkey public": "the user is UDXU4RCSJNZOIQHZNWXHXORDPRTGNJAHAHFRGZNEEJCPQTT2M7NLUHBP",
				"placeholder": "curl -H 'Authorization: Bearer YOUR_TOKEN_HERE' https://api.example.com",
				"go code":     "func (s *Scanner) Scan(ctx context.Context, text string) (Result, error) {",
				"long ident":  "the token TokenBucketRateLimiterConfiguration governs it",
				"prose":       "the token expires after an hour and has to be refreshed",
			} {
				res, err := scanner.Scan(context.Background(), text)
				Expect(err).NotTo(HaveOccurred())
				Expect(res.Text).To(Equal(text), "for %s", name)
			}
		})
	})

	Describe("DefaultChecks", func() {
		// The four validators left out are left out for what they do to ordinary text an
		// agent reads all day, and each of these is the sample that decided it.
		BeforeEach(func() {
			scanner = newScanner(pii.Options{Mode: pii.ModeRedact})
		})

		It("Should not name VIN", func() {
			Expect(pii.DefaultChecks).NotTo(ContainElement("VIN"))
		})

		It("Should leave a license header alone", func() {
			const header = `//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0`

			res, err := scanner.Scan(context.Background(), header)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Text).To(Equal(header))
		})

		It("Should leave people named in prose alone", func() {
			const text = "ask Sarah Johnson whether Michael Chen approved the rollout"

			res, err := scanner.Scan(context.Background(), text)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Text).To(Equal(text))
		})

		It("Should leave a server report full of addresses alone", func() {
			const report = `Server: NODE-1  Cluster: c1  Host: 203.0.113.44:4222
Route: 198.51.100.7:6222  Connections: 412`

			res, err := scanner.Scan(context.Background(), report)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Text).To(Equal(report))
		})

		// runagent138245072 is a temp directory name of the shape the item measured, and
		// its position-9 digit is the check digit ISO 3779 computes over the other
		// sixteen, so ferret-scan's VIN validator matches it. A scan with that validator
		// enabled replaces it; the default set leaves it.
		It("Should leave a token that passes the VIN check digit alone", func() {
			eng, err := redact.NewEngine(redact.EngineOptions{Checks: []string{"VIN"}, Strategy: redact.Simple})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { Expect(eng.Close()).To(Succeed()) })

			for _, text := range []string{
				"runagent138245072",
				"the note landed in /tmp/runagent138245072/docs/note.md at 10:04",
				"the build ran in runagent138245072 and finished",
			} {
				vin, err := eng.Redact(context.Background(), redact.Request{Text: text})
				Expect(err).NotTo(HaveOccurred())
				Expect(vin.AuditRecord().FindingsByType).To(HaveKeyWithValue("VIN", 1), "for %q", text)

				res, err := scanner.Scan(context.Background(), text)
				Expect(err).NotTo(HaveOccurred())
				Expect(res.Text).To(Equal(text), "for %q", text)
				Expect(res.Found()).To(BeFalse(), "for %q", text)
			}
		})
	})
})
