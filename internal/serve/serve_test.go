//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
)

func TestServe(t *testing.T) {
	RegisterFailHandler(Fail)

	// The specs here wait on runs executing on goroutines the server owns, and go test
	// runs packages in parallel, so Gomega's one second measures the machine's load
	// rather than this code. Waiting longer costs nothing when the assertion holds,
	// since Eventually returns as soon as it is satisfied.
	SetDefaultEventuallyTimeout(30 * time.Second)

	RunSpecs(t, "Serve")
}

// servedConfig is a parsed configuration for a served agent.
//
// It is built here rather than with agenttest.Config because agenttest implements
// serve.Channel, so a test inside this package importing it would be an import cycle.
// What is left in this package are the specs reaching unexported methods, and none of
// them run an agent, so a configuration naming no application is enough.
func servedConfig() *config.Config {
	cfg := &config.Config{Identity: "agent"}
	cfg.LLM.Model = "test-model"
	cfg.LLM.Budget.MaxIterations = 20

	return cfg
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// idleChannel offers no work at all, which is all a spec reaching an unexported method
// on a constructed Server needs a channel for.
type idleChannel struct {
	name string
}

func (c *idleChannel) Name() string { return c.name }

func (c *idleChannel) Next(context.Context) (*Work, error) { return nil, ErrChannelDone }

// boundedChannel states a concurrency of its own, which is what a channel claiming
// work before a run starts has to do.
type boundedChannel struct {
	idleChannel

	concurrency int
}

func (c *boundedChannel) Concurrency() int { return c.concurrency }

var _ = Describe("Budget clamping", func() {
	It("Should lower a configured limit but never raise it", func() {
		cfg := servedConfig()
		cfg.LLM.Budget.MaxIterations = 10

		srv, err := New(Options{
			Channels: []Channel{&idleChannel{name: "c"}},
			Config:   cfg,
			Logger:   quietLogger(),
		})
		Expect(err).ToNot(HaveOccurred())

		By("lowering a limit the work asks to lower")
		Expect(srv.clampedConfig(Budget{MaxIterations: 3}).LLM.Budget.MaxIterations).To(BeNumerically("==", 3))

		By("ignoring a limit above the configured ceiling")
		Expect(srv.clampedConfig(Budget{MaxIterations: 99}).LLM.Budget.MaxIterations).To(BeNumerically("==", 10))

		By("leaving the configuration alone when nothing is asked")
		Expect(srv.clampedConfig(Budget{})).To(BeIdenticalTo(srv.opts.Config))

		By("never mutating the shared configuration")
		Expect(srv.opts.Config.LLM.Budget.MaxIterations).To(BeNumerically("==", 10))
	})
})

var _ = Describe("Run options", func() {
	// A channel says who its caller is once, on the work. The server is what carries
	// that to the run, which records it in the journal it creates, so an operator
	// reading the store can say whose conversation a journal is.
	It("Should carry the caller a channel reported to the checkpoint", func() {
		srv, err := New(Options{
			Channels: []Channel{&idleChannel{name: "c"}},
			Config:   servedConfig(),
			Logger:   quietLogger(),
		})
		Expect(err).ToNot(HaveOccurred())

		opts := srv.runOptions(&Work{ID: "w1", Prompt: "hello", Caller: Caller{Name: "peer1"}})
		Expect(opts.Checkpoint.Caller).To(Equal("peer1"))

		By("leaving it empty for a channel that knows no caller")
		opts = srv.runOptions(&Work{ID: "w2", Prompt: "hello"})
		Expect(opts.Checkpoint.Caller).To(BeEmpty())
	})
})

var _ = Describe("Tool timeout", func() {
	newServer := func(cfg *config.Config, opts ...func(*Options)) *Server {
		GinkgoHelper()

		o := Options{
			Channels: []Channel{&idleChannel{name: "c"}},
			Config:   cfg,
			Logger:   quietLogger(),
		}
		for _, opt := range opts {
			opt(&o)
		}

		srv, err := New(o)
		Expect(err).ToNot(HaveOccurred())

		return srv
	}

	It("Should bound a hosted run by default, unlike a run at a terminal", func() {
		srv := newServer(servedConfig())

		Expect(srv.opts.ToolTimeout).To(Equal(DefaultToolTimeout))
		Expect(srv.withToolTimeout(srv.opts.Config).ToolTimeout()).To(Equal(DefaultToolTimeout))

		By("never mutating the shared configuration")
		Expect(srv.opts.Config.ToolTimeout()).To(Equal(time.Duration(0)))
	})

	It("Should leave a configured timeout alone even when it is longer", func() {
		cfg := servedConfig()
		cfg.Harness.ToolTimeoutParsed = time.Hour
		srv := newServer(cfg)

		Expect(srv.withToolTimeout(cfg)).To(BeIdenticalTo(cfg), "nothing to fill in, so nothing to copy")
		Expect(srv.withToolTimeout(cfg).ToolTimeout()).To(Equal(time.Hour))
	})

	It("Should honor an embedder's own default over the package one", func() {
		srv := newServer(servedConfig(), func(o *Options) { o.ToolTimeout = 90 * time.Second })

		Expect(srv.withToolTimeout(srv.opts.Config).ToolTimeout()).To(Equal(90 * time.Second))
	})

	It("Should apply both the clamp and the fill to one piece of work", func() {
		cfg := servedConfig()
		cfg.LLM.Budget.MaxIterations = 10
		srv := newServer(cfg)

		clamped := srv.withToolTimeout(srv.clampedConfig(Budget{MaxIterations: 3}))
		Expect(clamped.LLM.Budget.MaxIterations).To(BeNumerically("==", 3))
		Expect(clamped.ToolTimeout()).To(Equal(DefaultToolTimeout))
	})
})
