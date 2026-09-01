//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package serve_test

import (
	"context"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/conns"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/serve"
)

var _ = Describe("NewResources", func() {
	var cfg *config.Config

	BeforeEach(func() {
		cfg = servedConfig()
	})

	It("Should require a configuration", func() {
		_, err := serve.NewResources(context.Background(), nil, serve.ResourceOptions{})
		Expect(err).To(MatchError(ContainSubstring("a configuration is required")))
	})

	// The file backends reach nothing, so a laptop deployment builds its whole resource
	// set without a broker. Dialing here would make a working configuration fail.
	It("Should build the provider and session store without dialing for a file-backed configuration", func() {
		res, err := serve.NewResources(context.Background(), cfg, serve.ResourceOptions{ConfigFile: "agent.yaml"})
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(func() { Expect(res.Close()).To(Succeed()) })

		Expect(res.Conns).To(BeNil())
		Expect(res.Provider).ToNot(BeNil())
		Expect(res.SessionStore).ToNot(BeNil())
		Expect(res.SessionStore.Info().Backend).To(Equal(runstate.BackendFile))
	})

	It("Should leave the memory store nil when memory is disabled", func() {
		res, err := serve.NewResources(context.Background(), cfg, serve.ResourceOptions{})
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(func() { Expect(res.Close()).To(Succeed()) })

		Expect(res.MemoryStore).To(BeNil())
	})

	It("Should build the memory store when memory is enabled", func() {
		cfg.Harness.Memory = &config.MemoryConfig{Enabled: true}

		res, err := serve.NewResources(context.Background(), cfg, serve.ResourceOptions{StoreDir: GinkgoT().TempDir()})
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(func() { Expect(res.Close()).To(Succeed()) })

		Expect(res.MemoryStore).ToNot(BeNil())
	})

	It("Should leave the knowledge store nil when knowledge is disabled", func() {
		res, err := serve.NewResources(context.Background(), cfg, serve.ResourceOptions{})
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(func() { Expect(res.Close()).To(Succeed()) })

		Expect(res.RAGStore).To(BeNil())
	})

	// A store opened against an index that does not exist reports "not built" for as
	// long as it is held, so sharing one would have a worker started before the index
	// was written answer every search that way until it was restarted. Leaving it nil
	// hands the question back to the runs, which each open their own.
	It("Should not share a knowledge store when no index has been built", func() {
		dir := GinkgoT().TempDir()
		cfg.Harness.RAG = &config.RAGConfig{Enabled: true, Directory: filepath.Join(dir, "knowledge")}

		res, err := serve.NewResources(context.Background(), cfg, serve.ResourceOptions{})
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(func() { Expect(res.Close()).To(Succeed()) })

		Expect(res.RAGStore).To(BeNil())
	})

	// Everything the configuration can be wrong about on its own is settled before a
	// connection is opened, so an operator who selected a backend reached over NATS and
	// named no context is told which key is missing rather than how the dial failed.
	It("Should name the missing context rather than dialing", func() {
		cfg.Harness.Sessions = &config.SessionConfig{Backend: "jetstream"}
		cfg.NatsContext = ""

		_, err := serve.NewResources(context.Background(), cfg, serve.ResourceOptions{ConfigFile: "agent.yaml"})
		Expect(err).To(MatchError(ContainSubstring(`nats_context is required in "agent.yaml"`)))
	})

	// The failure that matters is the session store's, and the knowledge index opened
	// before it must not be left holding a file handle nobody can reach.
	It("Should release what it built when a later resource fails", func() {
		cfg.Harness.Sessions = &config.SessionConfig{Backend: "nonesuch"}

		res, err := serve.NewResources(context.Background(), cfg, serve.ResourceOptions{ConfigFile: "agent.yaml"})
		Expect(err).To(MatchError(ContainSubstring("building the session store")))
		Expect(res).To(BeNil())
	})

	// A connection the caller established outlives this set: they may be serving other
	// things with it, and conns.Provider cannot tell "I own my connection" from "I am
	// owned by the caller who handed me over".
	It("Should leave a supplied connection open on Close", func() {
		supplied := conns.New()

		res, err := serve.NewResources(context.Background(), cfg, serve.ResourceOptions{Conns: supplied})
		Expect(err).ToNot(HaveOccurred())
		Expect(res.Conns).To(BeIdenticalTo(supplied))

		Expect(res.Close()).To(Succeed())
		Expect(res.Conns).To(BeIdenticalTo(supplied))
	})

	It("Should be safe to Close twice", func() {
		res, err := serve.NewResources(context.Background(), cfg, serve.ResourceOptions{})
		Expect(err).ToNot(HaveOccurred())

		Expect(res.Close()).To(Succeed())
		Expect(res.Close()).To(Succeed())
	})
})

var _ = Describe("Resources.ApplyTo", func() {
	It("Should set every field a run takes from the shared set", func() {
		cfg := servedConfig()
		cfg.Harness.Memory = &config.MemoryConfig{Enabled: true}

		res, err := serve.NewResources(context.Background(), cfg, serve.ResourceOptions{
			Conns:    conns.New(),
			StoreDir: GinkgoT().TempDir(),
		})
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(func() { Expect(res.Close()).To(Succeed()) })

		var opts serve.Options
		res.ApplyTo(&opts)

		Expect(opts.Provider).To(BeIdenticalTo(res.Provider))
		Expect(opts.SessionStore).To(BeIdenticalTo(res.SessionStore))
		Expect(opts.Conns).To(BeIdenticalTo(res.Conns))
		Expect(opts.MemoryStore).To(BeIdenticalTo(res.MemoryStore))

		// Knowledge is off in this configuration, so the field is carried across as the
		// nil it is rather than left at whatever the caller had.
		Expect(opts.RAGStore).To(BeNil())
	})

	// It is a setter rather than a merge, which a caller keeping one of their own has to
	// know: assigning after this keeps theirs, assigning before loses it.
	It("Should overwrite a field the caller already set", func() {
		res, err := serve.NewResources(context.Background(), servedConfig(), serve.ResourceOptions{})
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(func() { Expect(res.Close()).To(Succeed()) })

		opts := serve.Options{SessionStore: nil}
		res.ApplyTo(&opts)

		Expect(opts.SessionStore).To(BeIdenticalTo(res.SessionStore))
	})

	It("Should do nothing for a nil set or nil options", func() {
		var res *serve.Resources
		Expect(func() { res.ApplyTo(&serve.Options{}) }).ToNot(Panic())

		built, err := serve.NewResources(context.Background(), servedConfig(), serve.ResourceOptions{})
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(func() { Expect(built.Close()).To(Succeed()) })

		Expect(func() { built.ApplyTo(nil) }).ToNot(Panic())
	})
})
