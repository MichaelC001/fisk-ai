//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"

	"github.com/choria-io/fisk"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// gatedApp is an application whose only command is confirmation-gated, so building a
// server over it logs one skip and nothing else.
func gatedApp() *fisk.Application {
	app := fisk.New("app", "an app")
	app.Command("gated", "needs approval").Tag("ai:confirm")

	return app
}

var _ = Describe("Validator sharing", func() {
	It("Should hold a client to the validator its caller supplied", func() {
		v, err := NewValidator()
		Expect(err).NotTo(HaveOccurred())

		c, err := NewClient(newFakeTransport(), "caller", WithValidator(v))
		Expect(err).NotTo(HaveOccurred())
		Expect(c.validator).To(BeIdenticalTo(v))
	})

	It("Should hold a server to the validator its caller supplied", func() {
		v, err := NewValidator()
		Expect(err).NotTo(HaveOccurred())

		s, err := NewServer(newFakeTransport(), nil, ServerOptions{Identity: "svc", Validator: v})
		Expect(err).NotTo(HaveOccurred())
		Expect(s.validator).To(BeIdenticalTo(v))
	})

	// The schema set is compiled once for the whole process rather than once per
	// endpoint, which is what a program hosting several agents pays for.
	It("Should give every client and server that supplied none the same validator", func() {
		c1, err := NewClient(newFakeTransport(), "caller")
		Expect(err).NotTo(HaveOccurred())

		c2, err := NewClient(newFakeTransport(), "caller")
		Expect(err).NotTo(HaveOccurred())

		s, err := NewServer(newFakeTransport(), nil, ServerOptions{Identity: "svc"})
		Expect(err).NotTo(HaveOccurred())

		Expect(c2.validator).To(BeIdenticalTo(c1.validator))
		Expect(s.validator).To(BeIdenticalTo(c1.validator))

		shared, err := sharedValidator()
		Expect(err).NotTo(HaveOccurred())
		Expect(shared).To(BeIdenticalTo(c1.validator))
	})

	// The prompts channel and the job worker validate what arrives themselves rather
	// than through a Client or a Server, so they reach the same one through the
	// exported accessor. Without it each compiles the set again and a serve process
	// pays for it three times.
	It("Should hand an outside caller the validator the constructors default to", func() {
		exported, err := SharedValidator()
		Expect(err).NotTo(HaveOccurred())

		c, err := NewClient(newFakeTransport(), "caller")
		Expect(err).NotTo(HaveOccurred())

		Expect(exported).To(BeIdenticalTo(c.validator))

		again, err := SharedValidator()
		Expect(err).NotTo(HaveOccurred())
		Expect(again).To(BeIdenticalTo(exported))
	})

	It("Should leave the shared validator alone when a caller supplies one", func() {
		v, err := NewValidator()
		Expect(err).NotTo(HaveOccurred())

		c, err := NewClient(newFakeTransport(), "caller", WithValidator(v))
		Expect(err).NotTo(HaveOccurred())

		shared, err := sharedValidator()
		Expect(err).NotTo(HaveOccurred())
		Expect(c.validator).NotTo(BeIdenticalTo(shared))
	})
})

var _ = Describe("ServerOptions logging", func() {
	It("Should write nothing to stderr when the caller supplied no logger", func() {
		r, w, err := os.Pipe()
		Expect(err).NotTo(HaveOccurred())

		saved := os.Stderr
		os.Stderr = w
		defer func() { os.Stderr = saved }()

		_, err = NewServer(newFakeTransport(), toolsFor(gatedApp()), ServerOptions{Identity: "svc"})
		Expect(err).NotTo(HaveOccurred())

		os.Stderr = saved
		Expect(w.Close()).To(Succeed())

		written, err := io.ReadAll(r)
		Expect(err).NotTo(HaveOccurred())
		Expect(written).To(BeEmpty())
		Expect(r.Close()).To(Succeed())
	})

	It("Should build a logger that is enabled at no level when neither Logger nor LogOutput is set", func() {
		opts := ServerOptions{Identity: "svc"}
		opts.applyDefaults()

		Expect(opts.Logger.Enabled(context.Background(), slog.LevelError)).To(BeFalse())
	})

	It("Should write to LogOutput when the caller supplied one", func() {
		out := &bytes.Buffer{}

		_, err := NewServer(newFakeTransport(), toolsFor(gatedApp()), ServerOptions{Identity: "svc", LogOutput: out})
		Expect(err).NotTo(HaveOccurred())
		Expect(out.String()).To(ContainSubstring("gated"))
	})
})
