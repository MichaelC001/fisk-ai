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
	"github.com/choria-io/fisk-ai/internal/a2a"
	natstransport "github.com/choria-io/fisk-ai/internal/a2a/nats"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/conns"
	"github.com/choria-io/fisk-ai/internal/serve"
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
// Every spec closes the surfaces it built, so nothing is left registered between them,
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

// parseConfig builds a served configuration from a body, so the accessors the surfaces
// read are the ones prepare produced rather than fields assembled by hand.
func parseConfig(body string) *config.Config {
	GinkgoHelper()

	cfg, err := config.ParseConfigForMode([]byte(body), config.ModeServe)
	Expect(err).ToNot(HaveOccurred())

	return cfg
}

// toolsConfig serves tools and nothing else. extra is appended to the file, so a spec
// can narrow the served set or add a block of its own.
func toolsConfig(extra string) *config.Config {
	GinkgoHelper()

	return parseConfig(fmt.Sprintf("identity: agent1\napplication_path: %s\nnats_context: ctx\nexpose:\n  agent:\n    a2a:\n      serve_tools: true\n%s", appPath, extra))
}

// promptsConfig answers prompts and serves no tools, which is a configuration with no
// application at all.
func promptsConfig(extra string) *config.Config {
	GinkgoHelper()

	return parseConfig(fmt.Sprintf("identity: agent1\nsystem_prompt: do the thing\nnats_context: ctx\nllm:\n  model: claude-sonnet-4-6\nexpose:\n  agent:\n    a2a:\n      prompts:\n%s", extra))
}

// serviceOf and channelOf pick one kind of surface out of what a builder returned.
func serviceOf(built []serve.Surface) *Service {
	GinkgoHelper()

	for _, s := range built {
		svc, ok := s.(*Service)
		if ok {
			return svc
		}
	}

	Fail("no tool service was built")

	return nil
}

func channelOf(built []serve.Surface) *Channel {
	GinkgoHelper()

	for _, s := range built {
		ch, ok := s.(*Channel)
		if ok {
			return ch
		}
	}

	Fail("no prompt channel was built")

	return nil
}

// closeAll releases whatever a spec built, whichever surfaces those were.
func closeAll(built []serve.Surface) {
	for _, s := range built {
		closer, ok := s.(interface{ Close() error })
		if ok {
			Expect(closer.Close()).To(Succeed())
		}
	}
}

var _ = Describe("A2A surface", func() {
	Describe("The builder", func() {
		It("Should be enabled by either surface and by neither when the block is absent", func() {
			b := Builder()
			Expect(b.Name).To(Equal("a2a"))
			Expect(b.Enabled(toolsConfig(""))).To(BeTrue())
			Expect(b.Enabled(promptsConfig("        workers: 2\n"))).To(BeTrue())

			off := parseConfig("identity: agent1\napplication_path: /bin/true\n")
			Expect(b.Enabled(off)).To(BeFalse())
		})
	})

	Describe("NewFromConfig", func() {
		It("Should refuse a configuration that answers nothing", func() {
			cfg := parseConfig("identity: agent1\napplication_path: /bin/true\n")

			_, err := NewFromConfig(cfg, ConfigOptions{Conns: provider})
			Expect(err).To(MatchError(ContainSubstring("enables neither serve_tools nor prompts")))
		})

		It("Should refuse a build with no connection", func() {
			_, err := NewFromConfig(toolsConfig(""), ConfigOptions{})
			Expect(err).To(MatchError(ContainSubstring("needs a NATS connection")))
		})

		It("Should refuse a filter that leaves nothing, naming the file", func() {
			cfg := toolsConfig("include:\n  tools:\n    - ^nothing_matches\n")

			_, err := NewFromConfig(cfg, ConfigOptions{Conns: provider, ConfigFile: "worker.yaml"})
			Expect(err).To(MatchError(ContainSubstring("no tools available after filtering")))
			Expect(err).To(MatchError(ContainSubstring("worker.yaml")))
		})

		It("Should serve the set expose.agent.tools selects", func() {
			built, err := NewFromConfig(toolsConfig("    tools:\n      include:\n        tools:\n          - ^backup$\n"), ConfigOptions{Conns: provider, Logger: quietLogger()})
			Expect(err).ToNot(HaveOccurred())
			DeferCleanup(closeAll, built)

			Expect(serviceOf(built).ExposedTools()).To(Equal([]string{"backup"}))
		})

		It("Should describe the tool service", func() {
			built, err := NewFromConfig(toolsConfig(""), ConfigOptions{Conns: provider, Logger: quietLogger()})
			Expect(err).ToNot(HaveOccurred())
			DeferCleanup(closeAll, built)

			svc := serviceOf(built)
			Expect(built).To(HaveLen(1), "no prompt channel was asked for")
			Expect(svc.Name()).To(Equal("a2a"))
			Expect(svc.ExposedTools()).To(ConsistOf("backup", "restore"))
			Expect(svc.Describe()).To(HaveLen(2))
			Expect(svc.WithheldBuiltins()).To(BeEmpty(), "this configuration enables no built-in")
		})

		// An agent that answers prompts needs no application, so the tool surface's own
		// requirement must not reach it.
		It("Should build a prompt channel with no application to serve", func() {
			built, err := NewFromConfig(promptsConfig("        workers: 3\n"), ConfigOptions{Conns: provider, Logger: quietLogger()})
			Expect(err).ToNot(HaveOccurred())
			DeferCleanup(closeAll, built)

			ch := channelOf(built)
			Expect(built).To(HaveLen(1), "no tool service was asked for")
			Expect(ch.Name()).To(Equal("a2a/prompts"))
			Expect(ch.Concurrency()).To(Equal(3))
			Expect(ch.Describe()).To(ConsistOf(
				a2a.DescLine{Label: "Requests", Value: natstransport.TaskSubject("agent1")},
				a2a.DescLine{Label: "Cancels", Value: natstransport.CancelSubject("agent1", "*")},
			))
		})

		It("Should build both surfaces over one transport, the channel first", func() {
			cfg := toolsConfig("      prompts: {}\nsystem_prompt: do the thing\nllm:\n  model: claude-sonnet-4-6\n")

			built, err := NewFromConfig(cfg, ConfigOptions{Conns: provider, Logger: quietLogger()})
			Expect(err).ToNot(HaveOccurred())
			DeferCleanup(closeAll, built)

			Expect(built).To(HaveLen(2))
			Expect(built[0].Name()).To(Equal("a2a/prompts"), "work arrives before tools on the banner")
			Expect(built[1].Name()).To(Equal("a2a"))
			Expect(channelOf(built).held).To(BeIdenticalTo(serviceOf(built).held), "one transport, one identity")
		})
	})

	Describe("Closing", func() {
		// A drain closes the surface while the process keeps running, so what proves it
		// worked is that the identity has left its queue group and a caller is told there
		// is nobody there rather than waiting.
		It("Should stop answering and be harmless a second time", func() {
			built, err := NewFromConfig(toolsConfig(""), ConfigOptions{Conns: provider, Logger: quietLogger()})
			Expect(err).ToNot(HaveOccurred())

			svc := serviceOf(built)
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

		// Both surfaces are paths of one micro service, so releasing either takes the
		// identity out of its queue group for all of them. The second close reports the
		// first one's answer rather than a failure, so a clean shutdown prints no error.
		It("Should stop both surfaces whichever of them is closed", func() {
			cfg := toolsConfig("      prompts: {}\nsystem_prompt: do the thing\nllm:\n  model: claude-sonnet-4-6\n")

			built, err := NewFromConfig(cfg, ConfigOptions{Conns: provider, Logger: quietLogger()})
			Expect(err).ToNot(HaveOccurred())

			Expect(channelOf(built).Close()).To(Succeed())

			_, err = nc.Request(natstransport.ToolSubject("agent1"), []byte("{}"), 500*time.Millisecond)
			Expect(err).To(MatchError(nats.ErrNoResponders))

			Expect(serviceOf(built).Close()).To(Succeed())
		})
	})
})
