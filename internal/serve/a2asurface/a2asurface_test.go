//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2asurface

import (
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/choria-io/fisk"
	natsd "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	natstransport "github.com/choria-io/fisk-ai/internal/a2a/nats"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/conns"
)

func TestA2ASurface(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Serve/A2ASurface")
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// servedApp is the application whose commands are served. Two commands, so a spec can
// narrow the served set to one of them and see the other go.
func servedApp() *fisk.Application {
	app := fisk.New("app", "an app")
	app.Command("backup", "back a thing up")
	app.Command("restore", "restore a thing")

	return app
}

// The server, the connection and the fake application are built once for the suite.
// Every spec closes the surface it built, so nothing is left registered between them,
// and introspecting the application redirects os.Stdout, which has to happen serially
// anyway.
var (
	provider *conns.Provider
	nc       *nats.Conn
	appPath  string
)

var _ = BeforeSuite(func() {
	ns, err := natsd.NewServer(&natsd.Options{Host: "127.0.0.1", Port: -1})
	Expect(err).ToNot(HaveOccurred())

	go ns.Start()
	Expect(ns.ReadyForConnections(10 * time.Second)).To(BeTrue())
	DeferCleanup(ns.Shutdown)

	nc, err = nats.Connect(ns.ClientURL())
	Expect(err).ToNot(HaveOccurred())
	DeferCleanup(nc.Close)

	provider = conns.New(conns.WithNats(nc))
	appPath = agenttest.NewFakeApp(GinkgoTB(), servedApp()).Path
})

var _ = Describe("A2A surface", func() {

	// The config is parsed rather than assembled so the accessors the surface reads are
	// the ones prepare produced, which is where the tool filters and the tags are
	// normalized.
	parse := func(extra string) *config.Config {
		GinkgoHelper()

		body := fmt.Sprintf("identity: agent1\napplication_path: %s\nnats_context: ctx\nexpose:\n  agent:\n    agent_to_agent: true\n%s", appPath, extra)

		cfg, err := config.ParseConfigForMode([]byte(body), config.ModeServe)
		Expect(err).ToNot(HaveOccurred())

		return cfg
	}

	Describe("The builder", func() {
		It("Should be enabled only by expose.agent.agent_to_agent", func() {
			b := Builder()
			Expect(b.Name).To(Equal("a2a"))
			Expect(b.Enabled(parse(""))).To(BeTrue())

			off, err := config.ParseConfigForMode([]byte("identity: agent1\napplication_path: /bin/true\n"), config.ModeServe)
			Expect(err).ToNot(HaveOccurred())
			Expect(b.Enabled(off)).To(BeFalse())
		})
	})

	Describe("NewFromConfig", func() {
		It("Should refuse a configuration that does not enable serving", func() {
			cfg, err := config.ParseConfigForMode([]byte("identity: agent1\napplication_path: /bin/true\n"), config.ModeServe)
			Expect(err).ToNot(HaveOccurred())

			_, err = NewFromConfig(cfg, ConfigOptions{Conns: provider})
			Expect(err).To(MatchError(ContainSubstring("expose.agent.agent_to_agent is not enabled")))
		})

		It("Should refuse a build with no connection", func() {
			_, err := NewFromConfig(parse(""), ConfigOptions{})
			Expect(err).To(MatchError(ContainSubstring("needs a NATS connection")))
		})

		It("Should refuse a filter that leaves nothing, naming the file", func() {
			cfg := parse("include:\n  tools:\n    - ^nothing_matches\n")

			_, err := NewFromConfig(cfg, ConfigOptions{Conns: provider, ConfigFile: "worker.yaml"})
			Expect(err).To(MatchError(ContainSubstring("no tools available after filtering")))
			Expect(err).To(MatchError(ContainSubstring("worker.yaml")))
		})

		It("Should serve the set expose.agent.tools selects", func() {
			svc, err := NewFromConfig(parse("    tools:\n      include:\n        tools:\n          - ^backup$\n"), ConfigOptions{Conns: provider, Logger: quietLogger()})
			Expect(err).ToNot(HaveOccurred())
			DeferCleanup(svc.Close)

			Expect(svc.ExposedTools()).To(Equal([]string{"backup"}))
		})

		It("Should describe itself", func() {
			svc, err := NewFromConfig(parse(""), ConfigOptions{Conns: provider, Logger: quietLogger()})
			Expect(err).ToNot(HaveOccurred())
			DeferCleanup(svc.Close)

			Expect(svc.Name()).To(Equal("a2a"))
			Expect(svc.ExposedTools()).To(ConsistOf("backup", "restore"))
			Expect(svc.Describe()).To(HaveLen(2))
			Expect(svc.WithheldBuiltins()).To(BeEmpty(), "this configuration enables no built-in")
		})
	})

	Describe("Closing", func() {
		// A drain closes the surface while the process keeps running, so what proves it
		// worked is that the identity has left its queue group and a caller is told there
		// is nobody there rather than waiting.
		It("Should stop answering and be harmless a second time", func() {
			svc, err := NewFromConfig(parse(""), ConfigOptions{Conns: provider, Logger: quietLogger()})
			Expect(err).ToNot(HaveOccurred())

			subject := natstransport.DiscoverySubject("agent1")

			// The body is not a discovery request, so the answer is an error message. That
			// there is an answer at all is the assertion: something is subscribed.
			_, err = nc.Request(subject, []byte("{}"), 2*time.Second)
			Expect(err).ToNot(HaveOccurred())

			Expect(svc.Close()).To(Succeed())

			_, err = nc.Request(subject, []byte("{}"), 500*time.Millisecond)
			Expect(err).To(MatchError(nats.ErrNoResponders))

			Expect(svc.Close()).To(Succeed(), "a drain and a stop both reach it")
		})
	})
})
