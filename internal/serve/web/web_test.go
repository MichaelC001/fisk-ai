//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"net"
	"net/http"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/serve"
	"github.com/choria-io/fisk-ai/internal/telemetry"
)

var _ = Describe("Options", func() {
	refused := func(mutate func(*Options)) string {
		GinkgoHelper()

		opts := testOptions()
		mutate(&opts)
		opts.applyDefaults()

		err := opts.validate()
		Expect(err).To(HaveOccurred())

		return err.Error()
	}

	It("Should require an identity, a store and at least one origin", func() {
		Expect(refused(func(o *Options) { o.Identity = "" })).To(ContainSubstring("identity is required"))
		Expect(refused(func(o *Options) { o.Sessions = nil })).To(ContainSubstring("session store is required"))
		Expect(refused(func(o *Options) { o.Origins = nil })).To(ContainSubstring("origin is required"))
	})

	// ServeMux reads a pattern without a leading slash as a host and a path, registers
	// it, and then 404s every request, so the mistake has to be refused here.
	It("Should refuse a base path without a leading slash", func() {
		Expect(refused(func(o *Options) { o.BasePath = "fisk/v1" })).To(ContainSubstring("must start with a slash"))
	})

	// The list is the whole of what decides which page may read an answer, so an entry
	// that could match anything, or could never match, is refused.
	It("Should refuse a wildcard origin and one a browser would never send", func() {
		Expect(refused(func(o *Options) { o.Origins = []string{"*"} })).To(ContainSubstring("any page"))
		Expect(refused(func(o *Options) { o.Origins = []string{"http://localhost:5173/app"} })).To(ContainSubstring("as a browser sends it"))
		Expect(refused(func(o *Options) { o.Origins = []string{"localhost:5173"} })).To(ContainSubstring("as a browser sends it"))
	})

	It("Should refuse a mount that is not one path segment, or that repeats", func() {
		f := &fakeFormat{}

		Expect(refused(func(o *Options) { o.Formats = []Mount{{Path: "a/b", Format: f}} })).To(ContainSubstring("not a single path segment"))
		Expect(refused(func(o *Options) { o.Formats = []Mount{{Path: "", Format: f}} })).To(ContainSubstring("not a single path segment"))
		Expect(refused(func(o *Options) { o.Formats = []Mount{{Path: "a"}} })).To(ContainSubstring("is nil"))
		Expect(refused(func(o *Options) { o.Formats = []Mount{{Path: "a", Format: f}, {Path: "a", Format: f}} })).To(ContainSubstring("two formats are mounted"))
	})

	// An embedder building Options in process never runs the configuration's prepare,
	// so the safe listen address and the worker count have to come from here.
	It("Should default the listen address, the base path and the workers", func() {
		opts := Options{}
		opts.applyDefaults()

		Expect(opts.Listen).To(Equal(DefaultListen))
		Expect(opts.BasePath).To(Equal(DefaultBasePath))
		Expect(opts.Workers).To(Equal(DefaultWorkers))
	})
})

var _ = Describe("New", func() {
	It("Should bind the listener at construction, so a busy port fails before the banner", func() {
		taken, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(taken.Close)

		opts := testOptions()
		opts.Listen = taken.Addr().String()

		_, err = New(opts)
		Expect(err).To(MatchError(ContainSubstring("binding " + taken.Addr().String())))
	})

	It("Should describe the bound address, the origins, the routes and the workers", func() {
		opts := testOptions()
		opts.Formats = []Mount{{Path: "fake", Format: &fakeFormat{}}}
		ch := newTestChannel(opts)

		Expect(ch.Name()).To(Equal("web"))
		Expect(ch.Concurrency()).To(Equal(2))
		Expect(ch.Heading()).To(Equal("Answering in a browser"))
		Expect(ch.Addr()).To(HavePrefix("127.0.0.1:"))
		Expect(ch.Addr()).ToNot(HaveSuffix(":0"), "the port the system chose rather than the one configured")
		Expect(ch.Describe()).To(Equal([]serve.DescLine{
			{Label: "Listen", Value: ch.Addr()},
			{Label: "Base Path", Value: "/fisk/v1"},
			{Label: "Origins", Value: testOrigin},
			{Label: "Routes", Value: "POST /fisk/v1/fake, GET /fisk/v1/card"},
			{Label: "Workers", Value: "2"},
		}))
	})

	// A channel with no format takes no turn and still says what the agent is.
	It("Should describe the card route alone when nothing is mounted", func() {
		ch := newTestChannel(testOptions())

		Expect(ch.Describe()).To(ContainElement(serve.DescLine{Label: "Routes", Value: "GET /fisk/v1/card"}))
	})

	It("Should drop a trailing slash from the base path", func() {
		opts := testOptions()
		opts.BasePath = "/fisk/v1/"
		opts.Formats = []Mount{{Path: "fake", Format: &fakeFormat{}}}
		ch := newTestChannel(opts)

		Expect(ch.Describe()).To(ContainElement(serve.DescLine{Label: "Routes", Value: "POST /fisk/v1/fake, GET /fisk/v1/card"}))
	})
})

var _ = Describe("Closing", func() {
	It("Should release the listener and be harmless a second time", func() {
		ch, err := New(testOptions())
		Expect(err).ToNot(HaveOccurred())

		addr := ch.Addr()
		Expect(ch.Close()).To(Succeed())
		Expect(ch.Close()).To(Succeed(), "a drain and a stop both reach it")

		_, err = net.Dial("tcp", addr)
		Expect(err).To(HaveOccurred(), "nothing is listening any more")

		_, err = ch.Next(context.Background())
		Expect(err).To(MatchError(serve.ErrChannelDone))
	})

	It("Should end a channel that was being served", func() {
		ch, err := New(testOptions())
		Expect(err).ToNot(HaveOccurred())

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		next := make(chan error, 1)
		go func() {
			defer GinkgoRecover()

			_, nerr := ch.Next(ctx)
			next <- nerr
		}()

		Eventually(ch.started.Load).Should(BeTrue())
		Expect(ch.Close()).To(Succeed())
		Eventually(next).Should(Receive(MatchError(serve.ErrChannelDone)))
	})
})

var _ = Describe("The builder", func() {
	webConfig := func(extra string) *config.Config {
		GinkgoHelper()

		cfg, err := config.ParseConfigForMode([]byte("identity: agent1\nsystem_prompt: do the thing\nllm:\n  model: claude-sonnet-4-6\nexpose:\n  agent:\n    web:\n      listen: 127.0.0.1:0\n      origins:\n        - "+testOrigin+"\n"+extra), config.ModeServe)
		Expect(err).ToNot(HaveOccurred())

		return cfg
	}

	It("Should be enabled by the web block alone", func() {
		b := Builder()
		Expect(b.Name).To(Equal("web"))
		Expect(b.Enabled(webConfig(""))).To(BeTrue())

		off, err := config.ParseConfigForMode([]byte("identity: agent1\napplication_path: /bin/true\n"), config.ModeServe)
		Expect(err).ToNot(HaveOccurred())
		Expect(b.Enabled(off)).To(BeFalse())
	})

	It("Should build a channel that mounts nothing when it is given no format", func() {
		built, err := Builder().Build(context.Background(), webConfig(""), serve.BuildOptions{
			Sessions: agenttest.NewFakeSessionStore(GinkgoTB()),
			Logger:   quietLogger(),
		})
		Expect(err).ToNot(HaveOccurred())
		Expect(built).To(HaveLen(1))

		ch, ok := built[0].(*Channel)
		Expect(ok).To(BeTrue())
		DeferCleanup(func() { Expect(ch.Close()).To(Succeed()) })

		Expect(ch.Concurrency()).To(Equal(DefaultWorkers))
		Expect(ch.Describe()).To(ContainElements(
			serve.DescLine{Label: "Base Path", Value: config.DefaultWebBasePath},
			serve.DescLine{Label: "Origins", Value: testOrigin},
			serve.DescLine{Label: "Routes", Value: "GET " + config.DefaultWebBasePath + "/card"},
		))
	})

	It("Should mount the formats the builder was given", func() {
		built, err := Builder(Mount{Path: "fake", Format: &fakeFormat{}}).Build(context.Background(), webConfig(""), serve.BuildOptions{
			Sessions: agenttest.NewFakeSessionStore(GinkgoTB()),
			Logger:   quietLogger(),
		})
		Expect(err).ToNot(HaveOccurred())

		ch, ok := built[0].(*Channel)
		Expect(ok).To(BeTrue())
		DeferCleanup(func() { Expect(ch.Close()).To(Succeed()) })

		Expect(ch.Describe()).To(ContainElement(
			serve.DescLine{Label: "Routes", Value: "POST " + config.DefaultWebBasePath + "/fake, GET " + config.DefaultWebBasePath + "/card"},
		))
	})

	It("Should refuse a build with no session store", func() {
		_, err := NewFromConfig(context.Background(), webConfig(""), ConfigOptions{Logger: quietLogger()})
		Expect(err).To(MatchError(ContainSubstring("needs a session store")))
	})

	It("Should refuse a configuration with no web block", func() {
		off, err := config.ParseConfigForMode([]byte("identity: agent1\napplication_path: /bin/true\n"), config.ModeServe)
		Expect(err).ToNot(HaveOccurred())

		_, err = NewFromConfig(context.Background(), off, ConfigOptions{Sessions: agenttest.NewFakeSessionStore(GinkgoTB())})
		Expect(err).To(MatchError(ContainSubstring("not configured")))
	})

	// The card reports export off the provider the process resolved, so a worker whose
	// endpoint was refused publishes no promise of an export. A channel handed nothing
	// would tell a console the conversation reaches no collector while the worker was
	// exporting it.
	It("Should carry the process's telemetry provider to the card", func() {
		provider := &telemetry.Provider{}

		built, err := Builder().Build(context.Background(), webConfig(""), serve.BuildOptions{
			Sessions:  agenttest.NewFakeSessionStore(GinkgoTB()),
			Telemetry: provider,
			Logger:    quietLogger(),
		})
		Expect(err).ToNot(HaveOccurred())

		ch, ok := built[0].(*Channel)
		Expect(ok).To(BeTrue())
		DeferCleanup(func() { Expect(ch.Close()).To(Succeed()) })

		Expect(ch.card.Telemetry).To(BeIdenticalTo(provider))
	})

	It("Should put what the operator wrote about the agent on the card", func() {
		built, err := Builder().Build(context.Background(), webConfig("description: manages nats auth\ndisplay_name: NATS Auth\nicon: \"\\U0001f510\"\nicon_url: https://example.net/agent.png\n"), serve.BuildOptions{
			Sessions: agenttest.NewFakeSessionStore(GinkgoTB()),
			Version:  "1.2.3",
			Logger:   quietLogger(),
		})
		Expect(err).ToNot(HaveOccurred())

		ch, ok := built[0].(*Channel)
		Expect(ok).To(BeTrue())
		DeferCleanup(func() { Expect(ch.Close()).To(Succeed()) })

		card := readCard(ch, http.StatusOK)
		Expect(card.Name).To(Equal("agent1"))
		Expect(card.Version).To(Equal("1.2.3"))
		Expect(card.Model).To(Equal("claude-sonnet-4-6"))
		Expect(card.Description).To(Equal("manages nats auth"))
		Expect(card.DisplayName).To(Equal("NATS Auth"))
		Expect(card.Icon).To(Equal("\U0001f510"))
		Expect(card.IconURL).To(Equal("https://example.net/agent.png"))
	})
})
