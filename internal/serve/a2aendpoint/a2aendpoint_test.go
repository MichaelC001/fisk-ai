//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2aendpoint

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
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

func TestA2AEndpoint(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Serve/A2AEndpoint")
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
// Every spec closes the endpoints it built, so nothing is left registered between them,
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

// parseConfig builds a served configuration from a body, so the accessors the endpoints
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

// serviceOf and channelOf pick one kind of endpoint out of what a builder returned.
func serviceOf(built []serve.Endpoint) *Service {
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

func channelOf(built []serve.Endpoint) *Channel {
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

// closeAll releases whatever a spec built, whichever endpoints those were.
func closeAll(built []serve.Endpoint) {
	for _, s := range built {
		closer, ok := s.(interface{ Close() error })
		if ok {
			Expect(closer.Close()).To(Succeed())
		}
	}
}

var _ = Describe("A2A endpoint", func() {
	Describe("The builder", func() {
		It("Should be enabled by either endpoint and by neither when the block is absent", func() {
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
			Expect(svc.Heading()).To(Equal("Serving tools over a2a"))
			Expect(svc.Describe()).To(HaveLen(4))
			Expect(svc.WithheldBuiltins()).To(BeEmpty(), "this configuration enables no built-in")
		})

		// A configuration that sets neither leaves the a2a server to its own defaults, so
		// reporting the configured values would print a concurrency and a timeout of zero
		// for a worker that in fact paces and stops every served call.
		It("Should describe the limits a served call will actually get", func() {
			built, err := NewFromConfig(toolsConfig(""), ConfigOptions{Conns: provider, Logger: quietLogger()})
			Expect(err).ToNot(HaveOccurred())
			DeferCleanup(closeAll, built)

			Expect(serviceOf(built).Describe()).To(ContainElements(
				serve.DescLine{Label: "Concurrency", Value: strconv.Itoa(a2a.DefaultConcurrency())},
				serve.DescLine{Label: "Tool Timeout", Value: a2a.DefaultCallTimeout.String()},
			))

			built, err = NewFromConfig(toolsConfig("      max_concurrent_tools: 7\n      tool_timeout: 90s\n"), ConfigOptions{Conns: provider, Logger: quietLogger()})
			Expect(err).ToNot(HaveOccurred())
			DeferCleanup(closeAll, built)

			Expect(serviceOf(built).Describe()).To(ContainElements(
				serve.DescLine{Label: "Concurrency", Value: "7"},
				serve.DescLine{Label: "Tool Timeout", Value: "1m30s"},
			))
		})

		// An agent that answers prompts needs no application, so the tool endpoint's own
		// requirement must not reach it.
		It("Should build a prompt channel with no application to serve", func() {
			built, err := NewFromConfig(promptsConfig("        workers: 3\n"), ConfigOptions{Conns: provider, Logger: quietLogger()})
			Expect(err).ToNot(HaveOccurred())
			DeferCleanup(closeAll, built)

			ch := channelOf(built)
			Expect(built).To(HaveLen(1), "no tool service was asked for")
			Expect(ch.Name()).To(Equal("a2a/prompts"))
			Expect(ch.Concurrency()).To(Equal(3))
			Expect(ch.Heading()).To(Equal("Answering prompts over a2a"))
			Expect(ch.Describe()).To(Equal([]serve.DescLine{
				{Label: "Requests", Value: natstransport.TaskSubject("agent1")},
				{Label: "Cancels", Value: natstransport.CancelSubject("agent1", "*")},
				{Label: "Workers", Value: "3"},
			}), "a channel that asks nothing advertises no answer address")
		})

		// Discovery is one route on the one micro service an identity registers, so an
		// agent that only takes prompts still has to answer it or a peer cannot tell it
		// from an agent that is not there.
		Describe("Discovery", func() {
			discover := func(built []serve.Endpoint) a2a.AgentCard {
				GinkgoHelper()

				DeferCleanup(closeAll, built)

				transport, err := a2a.NewTransport(config.A2ATransportName, provider, a2a.TransportConfig{Identity: "caller1", Timeout: 5 * time.Second})
				Expect(err).ToNot(HaveOccurred())
				DeferCleanup(transport.Close)

				client, err := a2a.NewClient(transport, "caller1")
				Expect(err).ToNot(HaveOccurred())

				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				DeferCleanup(cancel)

				card, err := client.Discover(ctx, "agent1")
				Expect(err).ToNot(HaveOccurred())

				return *card
			}

			It("Should answer for an agent that serves no tools", func() {
				built, err := NewFromConfig(promptsConfig("        workers: 1\n"), ConfigOptions{Conns: provider, Logger: quietLogger()})
				Expect(err).ToNot(HaveOccurred())

				card := discover(built)
				Expect(card.Name).To(Equal("agent1"))
				Expect(card.Tools).To(BeEmpty(), "it serves none to peers")
				Expect(card.Protocols).To(ConsistOf(a2a.ProtocolNamespace))
			})

			// Registering the route twice would not fail: micro subscribes again on the
			// same subject in the same queue group, and a peer gets whichever card NATS
			// picked. So an agent with both endpoints answers from the one with tools.
			It("Should answer once, with tools, when both endpoints are built", func() {
				cfg := toolsConfig("      prompts: {}\nsystem_prompt: do the thing\nllm:\n  model: claude-sonnet-4-6\n")

				built, err := NewFromConfig(cfg, ConfigOptions{Conns: provider, Logger: quietLogger()})
				Expect(err).ToNot(HaveOccurred())

				for range 5 {
					Expect(discover(built).Tools).ToNot(BeEmpty(), "every answer is the card with tools on it")
				}
			})

			// The configuration that picks the model is on the worker, so a person holding
			// a conversation with an agent somebody else runs has no other way to see what
			// is answering them.
			It("Should name the model an agent answers a prompt with", func() {
				built, err := NewFromConfig(promptsConfig("        workers: 1\n"), ConfigOptions{Conns: provider, Logger: quietLogger()})
				Expect(err).ToNot(HaveOccurred())

				Expect(discover(built).Model).To(Equal("claude-sonnet-4-6"))
			})

			// Serving tools runs no model, so a card naming one would say something about
			// this agent that is not true.
			It("Should name no model for an agent that only serves tools", func() {
				built, err := NewFromConfig(toolsConfig(""), ConfigOptions{Conns: provider, Logger: quietLogger()})
				Expect(err).ToNot(HaveOccurred())

				Expect(discover(built).Model).To(BeEmpty())
			})

			It("Should name the model of an agent that both serves tools and takes prompts", func() {
				cfg := toolsConfig("      prompts: {}\nsystem_prompt: do the thing\nllm:\n  model: claude-sonnet-4-6\n")

				built, err := NewFromConfig(cfg, ConfigOptions{Conns: provider, Logger: quietLogger()})
				Expect(err).ToNot(HaveOccurred())

				Expect(discover(built).Model).To(Equal("claude-sonnet-4-6"))
			})

			// A caller should know before it sends a prompt whether the words travel to
			// somebody's collector, and it can only know by being told.
			It("Should say nothing about telemetry when the agent exports none", func() {
				built, err := NewFromConfig(promptsConfig("        workers: 1\n"), ConfigOptions{Conns: provider, Logger: quietLogger()})
				Expect(err).ToNot(HaveOccurred())

				card := discover(built)
				Expect(card.Telemetry).To(BeFalse())
				Expect(card.TelemetryContent).To(BeFalse())
			})
		})

		It("Should describe the answer address when it asks its callers questions", func() {
			built, err := NewFromConfig(promptsConfig("        elicit: true\n"), ConfigOptions{Conns: provider, Logger: quietLogger()})
			Expect(err).ToNot(HaveOccurred())
			DeferCleanup(closeAll, built)

			Expect(channelOf(built).Describe()).To(ContainElement(
				serve.DescLine{Label: "Answers", Value: natstransport.ElicitSubject("agent1", "*")},
			))
		})

		It("Should build both endpoints over one transport, the channel first", func() {
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
		// A drain closes the endpoint while the process keeps running, so what proves it
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

		// Both endpoints are paths of one micro service, so releasing either takes the
		// identity out of its queue group for all of them. The second close reports the
		// first one's answer rather than a failure, so a clean shutdown prints no error.
		It("Should stop both endpoints whichever of them is closed", func() {
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
