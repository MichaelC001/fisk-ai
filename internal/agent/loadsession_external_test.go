//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// These tests hold LoadSession to the journal Run wrote. Each spec runs first and then
// reads with Options.SessionOptions() from the same Options, so a read resolving the
// store from anywhere else finds nothing for a session that exists.
package agent_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	natsd "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/conns"
	"github.com/choria-io/fisk-ai/internal/runstate"
)

// journaledRun completes one run under the given name, filling in the prompt, provider
// and checkpoint around the session inputs the spec set, and returns what a read of that
// journal takes.
func journaledRun(opts agent.Options, id string) agent.SessionOptions {
	GinkgoHelper()

	opts.ConfigFile = "agent.yaml"
	opts.Prompt = []string{"go"}
	opts.Provider = agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done"))
	opts.Checkpoint = agent.Checkpoint{ResumeID: id, CreateIfMissing: true}

	res, err := agent.Run(context.Background(), opts, agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
	Expect(err).NotTo(HaveOccurred())
	Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

	return opts.SessionOptions()
}

var _ = Describe("LoadSession", func() {
	// A run with StoreDir set journals under that directory, so the pre-flight read has
	// to look there rather than in the XDG state directory.
	It("Should read the store dir the run journaled into", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		dir := GinkgoT().TempDir()

		read := journaledRun(agent.Options{Config: cfg, StoreDir: dir}, "load-1")
		Expect(read.StoreDir).To(Equal(dir))
		Expect(filepath.Join(dir, "runs")).To(BeADirectory())

		rs, err := agent.LoadSession(context.Background(), cfg, "load-1", read)
		Expect(err).NotTo(HaveOccurred())
		Expect(rs.RunID).To(Equal("load-1"))
		Expect(rs.Prompt).To(Equal("go"))
		Expect(rs.Completed()).To(BeTrue())

		// Another base holds no such run: the read found it under the directory it was
		// given.
		_, err = agent.LoadSession(context.Background(), cfg, "load-1", agent.SessionOptions{StoreDir: GinkgoT().TempDir()})
		Expect(err).To(MatchError(runstate.ErrNotFound))
	})

	// A caller sharing one store across runs pre-flights through that store. The store
	// dir points at a directory holding nothing, so only the injected store can answer.
	It("Should read through an injected session store", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		store := agenttest.NewFakeSessionStore(GinkgoTB())
		empty := GinkgoT().TempDir()

		read := journaledRun(agent.Options{Config: cfg, StoreDir: empty, SessionStore: store}, "load-2")
		Expect(read.SessionStore).To(BeIdenticalTo(store))

		rs, err := agent.LoadSession(context.Background(), cfg, "load-2", read)
		Expect(err).NotTo(HaveOccurred())
		Expect(rs.RunID).To(Equal("load-2"))
		Expect(rs.Completed()).To(BeTrue())

		_, err = agent.LoadSession(context.Background(), cfg, "load-2", agent.SessionOptions{StoreDir: empty})
		Expect(err).To(MatchError(runstate.ErrNotFound))
	})

	// The context is the caller's limit on a pre-flight that hangs.
	It("Should end on a canceled context", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		store := agenttest.NewFakeSessionStore(GinkgoTB())

		read := journaledRun(agent.Options{Config: cfg, SessionStore: store}, "load-3")

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := agent.LoadSession(ctx, cfg, "load-3", read)
		Expect(err).To(MatchError(context.Canceled))
	})
})

// sessionJetStream starts an embedded JetStream NATS server, creates the stream the
// jetstream session backend binds, and returns the server together with a client
// connection. Both are torn down when the spec ends.
func sessionJetStream(stream string) (*natsd.Server, *nats.Conn) {
	GinkgoHelper()

	ns, err := natsd.NewServer(&natsd.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: GinkgoT().TempDir()})
	Expect(err).NotTo(HaveOccurred())

	go ns.Start()
	Expect(ns.ReadyForConnections(10 * time.Second)).To(BeTrue())
	DeferCleanup(ns.Shutdown)

	nc, err := nats.Connect(ns.ClientURL(), nats.Name("spec setup"))
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(nc.Close)

	js, err := jetstream.New(nc)
	Expect(err).NotTo(HaveOccurred())

	_, err = js.CreateStream(context.Background(), jetstream.StreamConfig{
		Name:                 stream,
		Subjects:             []string{"fisk.sessions.>"},
		MaxMsgsPerSubject:    1,
		Discard:              jetstream.DiscardNew,
		DiscardNewPerSubject: true,
	})
	Expect(err).NotTo(HaveOccurred())

	return ns, nc
}

// natsContextFor writes a NATS context pointing at ns where natscontext.Connect looks
// and returns its name, so a spec can have a library dial for itself rather than hand
// it a connection.
func natsContextFor(ns *natsd.Server) string {
	GinkgoHelper()

	home := GinkgoT().TempDir()
	GinkgoT().Setenv("XDG_CONFIG_HOME", home)

	dir := filepath.Join(home, "nats", "context")
	Expect(os.MkdirAll(dir, 0o700)).To(Succeed())

	body, err := json.Marshal(map[string]string{"url": ns.ClientURL()})
	Expect(err).NotTo(HaveOccurred())
	Expect(os.WriteFile(filepath.Join(dir, "spectest.json"), body, 0o600)).To(Succeed())

	return "spectest"
}

// announcedNames are the connection names ns has been given, open and closed alike. A
// pre-flight read dials, reads and releases, so by the time it returns its connection
// is among the closed ones.
func announcedNames(ns *natsd.Server) []string {
	GinkgoHelper()

	connz, err := ns.Connz(&natsd.ConnzOptions{State: natsd.ConnAll})
	Expect(err).NotTo(HaveOccurred())

	var out []string
	for _, c := range connz.Conns {
		out = append(out, c.Name)
	}

	return out
}

var _ = Describe("Integration: LoadSession dialing for itself", Label("integration"), func() {
	// A read with no injected connection dials from the configuration, so the product
	// there is what an operator sees for the pre-flight as well as for the run. Each
	// case dials a real server and asks it what name it got.
	DescribeTable("Should announce the product the configuration names",
		func(product string, productVersion string, want string) {
			ns, _ := sessionJetStream("FISK_SESSIONS")

			cfg := agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), exampleApp()))
			cfg.Identity = "worker-3"
			cfg.NatsContext = natsContextFor(ns)
			cfg.Product = product
			cfg.ProductVersion = productVersion
			cfg.Harness.Sessions = &config.SessionConfig{
				Backend: runstate.BackendJetStream,
				Options: json.RawMessage(`{"stream":"FISK_SESSIONS"}`),
			}

			_, err := agent.LoadSession(context.Background(), cfg, "no-such-run", agent.SessionOptions{})
			Expect(err).To(MatchError(runstate.ErrNotFound))

			Expect(announcedNames(ns)).To(ContainElement(want))
		},
		Entry("unset", "", "", "fisk-ai worker-3"),
		Entry("product and version", "acme-agent", "4.5", "acme-agent/4.5 worker-3"),
		Entry("product alone", "acme-agent", "", "acme-agent worker-3"),
	)
})

var _ = Describe("Integration: LoadSession over a jetstream store", Label("integration"), func() {
	// A jetstream backend reads the journal off whichever server the connection reaches,
	// so an injected connection has to be borrowed rather than replaced by a dial. The
	// configured NATS context does not exist, so a dial fails and only the injected
	// connection can reach the stream the run wrote to.
	It("Should borrow the injected connection", func() {
		_, nc := sessionJetStream("FISK_SESSIONS")

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		cfg := agenttest.Config(GinkgoTB(), app)
		cfg.NatsContext = "fisk-ai-no-such-context"
		cfg.Harness.Sessions = &config.SessionConfig{
			Backend: runstate.BackendJetStream,
			Options: json.RawMessage(`{"stream":"FISK_SESSIONS"}`),
		}

		provider := conns.New(conns.WithNats(nc))

		read := journaledRun(agent.Options{Config: cfg, Conns: provider}, "load-4")
		Expect(read.Conns).To(BeIdenticalTo(provider))
		Expect(read.SessionStore).To(BeNil())

		rs, err := agent.LoadSession(context.Background(), cfg, "load-4", read)
		Expect(err).NotTo(HaveOccurred())
		Expect(rs.RunID).To(Equal("load-4"))
		Expect(rs.Completed()).To(BeTrue())

		// The connection is the caller's, so the read leaves it open.
		Expect(nc.IsConnected()).To(BeTrue())
	})
})
