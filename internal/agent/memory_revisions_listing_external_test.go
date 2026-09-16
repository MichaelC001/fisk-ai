//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// These specs list a conversation that touched memory through the JetStream session
// store, whose listing reads a run's ending off its last record.
package agent_test

import (
	"context"
	"encoding/json"
	"time"

	natsd "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/runstate"
)

// runJetStreamStore starts an embedded JetStream-enabled NATS server on a random port,
// creates the RUNS stream and opens the JetStream session store on it through the
// public constructor. Server and connection are torn down when the spec ends.
func runJetStreamStore(ctx context.Context) runstate.Store {
	GinkgoHelper()

	ns, err := natsd.NewServer(&natsd.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: GinkgoT().TempDir()})
	Expect(err).NotTo(HaveOccurred())

	go ns.Start()
	Expect(ns.ReadyForConnections(10 * time.Second)).To(BeTrue())
	DeferCleanup(ns.Shutdown)

	nc, err := nats.Connect(ns.ClientURL())
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(nc.Close)

	js, err := jetstream.New(nc)
	Expect(err).NotTo(HaveOccurred())

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:                 "RUNS",
		Subjects:             []string{"runs.>"},
		MaxMsgsPerSubject:    1,
		Discard:              jetstream.DiscardNew,
		DiscardNewPerSubject: true,
	})
	Expect(err).NotTo(HaveOccurred())

	store, err := runstate.New(runstate.BackendJetStream, json.RawMessage(`{"stream":"RUNS"}`), runstate.RuntimeEnv{Nats: nc})
	Expect(err).NotTo(HaveOccurred())

	return store
}

// expectListedAsCompleted reads the run's row the two ways the web listing does, ListPage
// and Describe, and asserts each says the run completed and carries a summary.
func expectListedAsCompleted(ctx context.Context, store runstate.Store, id string) {
	GinkgoHelper()

	page, err := store.ListPage(ctx, runstate.ListFilter{}, 10, "")
	Expect(err).NotTo(HaveOccurred())

	var row *runstate.RunInfo
	for i := range page.Runs {
		if page.Runs[i].RunID == id {
			row = &page.Runs[i]
		}
	}
	Expect(row).NotTo(BeNil(), "ListPage holds no row for %s", id)
	Expect(row.Terminal).To(Equal(runstate.ReasonCompleted), "ListPage row for %s", id)
	Expect(row.Summary).NotTo(BeNil(), "ListPage row for %s", id)

	rows, err := store.Describe(ctx, runstate.ListFilter{}, []string{id})
	Expect(err).NotTo(HaveOccurred())
	Expect(rows).To(HaveLen(1))
	Expect(rows[0].Terminal).To(Equal(runstate.ReasonCompleted), "Describe row for %s", id)
	Expect(rows[0].Summary).NotTo(BeNil(), "Describe row for %s", id)
}

// Labeled integration because it runs an embedded NATS server, the same as the
// JetStream store's own spec, so ginkgo --label-filter='!integration' skips it.
//
// This is the defect the spec pins. The JetStream listing takes terminal and summary off
// a run's last record, and a run that ended on its memory revisions record was listed as
// still open with no summary. The second turn is the shape seen in the field: a resumed
// run that read no memory itself still held the revisions seeded from the journal, so
// it wrote the record too.
var _ = Describe("memory revisions in the JetStream listing", Label("integration"), func() {
	It("Should list a run that touched memory as completed with a summary", func() {
		ctx := context.Background()

		store := runJetStreamStore(ctx)

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())

		opts := func(provider *agenttest.ScriptedProvider, cp agent.Checkpoint) agent.Options {
			return agent.Options{
				Config:       agenttest.Config(GinkgoTB(), app),
				ConfigFile:   "agent.yaml",
				Prompt:       []string{"edit the note"},
				Provider:     provider,
				SessionStore: store,
				Checkpoint:   cp,
				CustomTools:  scopeTools(GinkgoTB()),
			}
		}

		res1, err := agent.Run(ctx, opts(
			agenttest.NewScriptedProvider(GinkgoTB(),
				agenttest.ToolUseResponse("c1", "scope_read", json.RawMessage(`{}`)),
				agenttest.TextResponse("read it"),
			),
			agent.Checkpoint{Enabled: true},
		), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res1.Reason).To(Equal(runstate.ReasonCompleted))

		expectListedAsCompleted(ctx, store, res1.SessionID)

		res2, err := agent.Run(ctx, opts(
			agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("wrote it")),
			agent.Checkpoint{ResumeID: res1.SessionID, FollowUp: true},
		), agenttest.NewRecordingEvents(), agenttest.NewScriptedPrompter(GinkgoTB()))
		Expect(err).NotTo(HaveOccurred())
		Expect(res2.Reason).To(Equal(runstate.ReasonCompleted))
		Expect(res2.SessionID).To(Equal(res1.SessionID))

		expectListedAsCompleted(ctx, store, res1.SessionID)
	})
})
