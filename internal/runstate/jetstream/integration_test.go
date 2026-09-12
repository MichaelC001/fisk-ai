//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package jetstream

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	natsd "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/segmentio/ksuid"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/runstate/file"
)

// runJetStream starts an embedded JetStream-enabled NATS server on a random port and
// returns a client connection. Both are torn down when the spec ends. The Describe
// carries Label("integration") so the unit suite (ginkgo --label-filter='!integration')
// does not run it.
func runJetStream() *nats.Conn {
	GinkgoHelper()

	ns, err := natsd.NewServer(&natsd.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: GinkgoT().TempDir()})
	Expect(err).NotTo(HaveOccurred())

	go ns.Start()
	Expect(ns.ReadyForConnections(10 * time.Second)).To(BeTrue())
	DeferCleanup(ns.Shutdown)

	nc, err := nats.Connect(ns.ClientURL())
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(nc.Close)

	return nc
}

func newID() string {
	return ksuid.New().String()
}

func assistantRec(iter int64, toolIDs ...string) runstate.Record {
	content := []llm.ContentBlock{{Text: &llm.TextBlock{Text: "working"}}}
	for _, id := range toolIDs {
		content = append(content, llm.ContentBlock{ToolUse: &llm.ToolUseBlock{ID: id, Name: "shell", Input: json.RawMessage(`{"x":1}`)}})
	}

	return runstate.Record{
		Protocol:  runstate.AssistantProtocol,
		Assistant: &runstate.AssistantRecord{Iteration: iter, Message: llm.Message{Role: llm.RoleAssistant, Content: content}, InTokens: 10, OutTokens: 5},
	}
}

func toolResultRec(id string) runstate.Record {
	return runstate.Record{
		Protocol:   runstate.ToolResultProtocol,
		ToolResult: &runstate.ToolResultRecord{ToolUseID: id, Result: llm.ToolResultBlock{ToolUseID: id, Content: "ok"}},
	}
}

func claimRec(by string) runstate.Record {
	return runstate.Record{
		Protocol: runstate.ClaimProtocol,
		Claim:    &runstate.ClaimRecord{By: by, Claimed: time.Now().UTC()},
	}
}

func terminalRec(reason runstate.TerminalReason) runstate.Record {
	return runstate.Record{
		Protocol: runstate.TerminalProtocol,
		Terminal: &runstate.TerminalRecord{Reason: reason},
	}
}

// publishUnstamped puts a run's records on the stream directly, bypassing Append, so a
// spec can hold a journal whose records carry no time. Every journal written before
// Record.Time existed is that shape.
func publishUnstamped(ctx context.Context, s runstate.Store, js jetstream.JetStream, id string, recs ...runstate.Record) {
	GinkgoHelper()

	backend, ok := s.(*store)
	Expect(ok).To(BeTrue())

	for _, rec := range recs {
		Expect(rec.Time).To(BeZero())

		body, err := json.Marshal(rec)
		Expect(err).ToNot(HaveOccurred())

		_, err = js.Publish(ctx, backend.subjectForSeq(id, rec.Seq), body)
		Expect(err).ToNot(HaveOccurred())
	}
}

// countRecordReads runs work and reports how many stored records it read and how many
// consumers it created. A read is one request on '$JS.API.STREAM.MSG.GET.<stream>', or on
// '$JS.API.DIRECT.GET.<stream>' where the stream allows a direct get, and a consumer is
// one request on '$JS.API.CONSUMER.CREATE.<stream>', so a wildcard subscription counts
// both.
//
// The sentinel is the barrier. Every read the work made had its reply in hand before the
// sentinel was published, and the server fans out to this subscription in the order it
// processed the requests, so the sentinel arrives after the reads it followed.
func countRecordReads(nc *nats.Conn, work func()) (int, int) {
	GinkgoHelper()

	const sentinel = "fisk.test.reads.sentinel"

	var reads, consumers atomic.Int64
	done := make(chan struct{})

	sub, err := nc.Subscribe(">", func(m *nats.Msg) {
		switch {
		case m.Subject == sentinel:
			close(done)
		case strings.HasPrefix(m.Subject, "$JS.API.STREAM.MSG.GET."), strings.HasPrefix(m.Subject, "$JS.API.DIRECT.GET."):
			reads.Add(1)
		case strings.HasPrefix(m.Subject, "$JS.API.CONSUMER.CREATE."):
			consumers.Add(1)
		}
	})
	Expect(err).ToNot(HaveOccurred())
	defer sub.Unsubscribe()
	Expect(nc.Flush()).To(Succeed())

	work()

	Expect(nc.Publish(sentinel, nil)).To(Succeed())
	Expect(nc.Flush()).To(Succeed())
	Eventually(done).Should(BeClosed())

	return int(reads.Load()), int(consumers.Load())
}

// goodStream is a stream configuration the backend accepts: a single <prefix>.>
// wildcard, write-once subjects, and no expiry.
func goodStream(name string, subjects ...string) jetstream.StreamConfig {
	return jetstream.StreamConfig{
		Name:                 name,
		Subjects:             subjects,
		MaxMsgsPerSubject:    1,
		Discard:              jetstream.DiscardNew,
		DiscardNewPerSubject: true,
	}
}

var _ = Describe("Integration: jetstream session", Label("integration"), func() {
	var (
		ctx context.Context
		nc  *nats.Conn
		js  jetstream.JetStream
	)

	newStoreFor := func(stream string) (runstate.Store, error) {
		return newStore(runstate.RuntimeEnv{Nats: nc}, []byte(fmt.Sprintf(`{"stream":%q}`, stream)))
	}

	createStream := func(cfg jetstream.StreamConfig) {
		GinkgoHelper()
		_, err := js.CreateStream(ctx, cfg)
		Expect(err).ToNot(HaveOccurred())
	}

	// No Version: Create stamps it, so every run here carries the version the store put
	// there rather than one the test supplied.
	newMeta := func(id string) runstate.MetaRecord {
		return runstate.MetaRecord{
			RunID:       id,
			Created:     time.Unix(1700000000, 0).UTC(),
			Prompt:      "hello",
			Fingerprint: runstate.Fingerprint{Model: "claude-opus-4-8"},
		}
	}

	BeforeEach(func() {
		ctx = context.Background()
		nc = runJetStream()
		var err error
		js, err = jetstream.New(nc)
		Expect(err).ToNot(HaveOccurred())
	})

	Describe("binding", func() {
		It("Should fail when the stream does not exist", func() {
			_, err := newStoreFor("MISSING")
			Expect(err).To(MatchError(ContainSubstring("does not exist")))
		})

		It("Should reject a stream that binds no wildcard subject", func() {
			createStream(jetstream.StreamConfig{Name: "LIT", Subjects: []string{"literal.subject"}})
			_, err := newStoreFor("LIT")
			Expect(err).To(MatchError(ContainSubstring("binds no wildcard subject")))
		})

		It("Should reject a max-msgs-per-subject other than 1", func() {
			createStream(jetstream.StreamConfig{Name: "MANY", Subjects: []string{"runs.>"}, MaxMsgsPerSubject: 5, Discard: jetstream.DiscardNew, DiscardNewPerSubject: true})
			_, err := newStoreFor("MANY")
			Expect(err).To(MatchError(ContainSubstring("max messages per subject")))
		})

		It("Should reject a discard policy other than DiscardNew", func() {
			createStream(jetstream.StreamConfig{Name: "OLD", Subjects: []string{"runs.>"}, MaxMsgsPerSubject: 1, Discard: jetstream.DiscardOld})
			_, err := newStoreFor("OLD")
			Expect(err).To(MatchError(ContainSubstring("not DiscardNew")))
		})

		It("Should reject a stream without discard-new-per-subject", func() {
			createStream(jetstream.StreamConfig{Name: "NOPS", Subjects: []string{"runs.>"}, MaxMsgsPerSubject: 1, Discard: jetstream.DiscardNew, DiscardNewPerSubject: false})
			_, err := newStoreFor("NOPS")
			Expect(err).To(MatchError(ContainSubstring("discard new per subject")))
		})

		It("Should reject a stream with a max age", func() {
			cfg := goodStream("AGED", "runs.>")
			cfg.MaxAge = time.Hour
			createStream(cfg)
			_, err := newStoreFor("AGED")
			Expect(err).To(MatchError(ContainSubstring("max age")))
		})

		It("Should reject a max message size below the record floor", func() {
			cfg := goodStream("TINY", "runs.>")
			cfg.MaxMsgSize = 1024
			createStream(cfg)
			_, err := newStoreFor("TINY")
			Expect(err).To(MatchError(ContainSubstring("max message size")))
		})

		It("Should derive the run prefix from a well-formed stream", func() {
			createStream(goodStream("SESSIONS", "ops.audit", "runs.>"))
			s, err := newStoreFor("SESSIONS")
			Expect(err).ToNot(HaveOccurred())
			Expect(s.(*store).prefix).To(Equal("runs"))
		})

		It("Should report the backend and the bound stream, and not the subject prefix", func() {
			createStream(goodStream("SESSIONS", "ops.audit", "runs.>"))
			s, err := newStoreFor("SESSIONS")
			Expect(err).ToNot(HaveOccurred())
			Expect(s.Info().Backend).To(Equal(runstate.BackendJetStream))
			Expect(s.Info().Location).To(Equal("SESSIONS"))
			Expect(s.Info().Location).ToNot(ContainSubstring("runs"))
		})
	})

	Describe("CRUD and resume", func() {
		var store runstate.Store

		BeforeEach(func() {
			createStream(goodStream("SESSIONS", "runs.>"))
			var err error
			store, err = newStoreFor("SESSIONS")
			Expect(err).ToNot(HaveOccurred())
		})

		It("Should create, append, and fold back a run", func() {
			id := newID()
			j, err := store.Create(ctx, id, newMeta(id))
			Expect(err).ToNot(HaveOccurred())
			Expect(j.Append(ctx, 2, assistantRec(0, "tu_1"))).To(Succeed())
			Expect(j.Append(ctx, 3, toolResultRec("tu_1"))).To(Succeed())
			Expect(j.LastSeq()).To(Equal(uint64(3)))
			Expect(j.Close()).To(Succeed())

			rs, err := store.Load(ctx, id)
			Expect(err).ToNot(HaveOccurred())
			Expect(rs.RunID).To(Equal(id))
			Expect(rs.Messages).To(HaveLen(3))
			Expect(rs.NextIteration).To(Equal(int64(1)))
			Expect(rs.Counters.ToolCalls).To(Equal(int64(1)))
		})

		It("Should stamp the record version and leave the caller's meta record alone", func() {
			id := newID()
			meta := newMeta(id)

			j, err := store.Create(ctx, id, meta)
			Expect(err).ToNot(HaveOccurred())
			Expect(j.Close()).To(Succeed())
			Expect(meta.Version).To(BeZero())

			rs, err := store.Load(ctx, id)
			Expect(err).ToNot(HaveOccurred())
			Expect(rs.Version).To(Equal(runstate.Version))
		})

		It("Should refuse a meta record carrying a version it does not write", func() {
			id := newID()
			meta := newMeta(id)
			meta.Version = runstate.Version + 1

			_, err := store.Create(ctx, id, meta)
			Expect(err).To(MatchError(runstate.ErrVersion))

			_, err = store.Load(ctx, id)
			Expect(err).To(MatchError(runstate.ErrNotFound))
		})

		// The backend derives every JetStream call from the caller's context rather than
		// a root of its own, so canceling the caller fails the call in flight instead of
		// waiting out opTimeout. The run is untouched and reads back on a live context.
		It("Should fail a load and an append when the caller's context is canceled", func() {
			id := newID()
			j, err := store.Create(ctx, id, newMeta(id))
			Expect(err).ToNot(HaveOccurred())
			Expect(j.Append(ctx, 2, assistantRec(0))).To(Succeed())

			canceled, cancel := context.WithCancel(ctx)
			cancel()

			_, err = store.Load(canceled, id)
			Expect(err).To(MatchError(context.Canceled))

			Expect(j.Append(canceled, 3, toolResultRec("tu_1"))).To(MatchError(context.Canceled))
			Expect(j.LastSeq()).To(Equal(uint64(2)), "the refused append did not advance the journal")

			Expect(j.Append(ctx, 3, toolResultRec("tu_1"))).To(Succeed())
			Expect(j.Close()).To(Succeed())

			rs, err := store.Load(ctx, id)
			Expect(err).ToNot(HaveOccurred())
			Expect(rs.Counters.ToolCalls).To(Equal(int64(1)))
		})

		It("Should refuse to create a run that already exists", func() {
			id := newID()
			j, err := store.Create(ctx, id, newMeta(id))
			Expect(err).ToNot(HaveOccurred())
			Expect(j.Close()).To(Succeed())

			_, err = store.Create(ctx, id, newMeta(id))
			Expect(err).To(MatchError(runstate.ErrExists))
		})

		It("Should return ErrNotFound for an absent run", func() {
			id := newID()
			_, err := store.Open(ctx, id)
			Expect(err).To(MatchError(runstate.ErrNotFound))
			_, err = store.Load(ctx, id)
			Expect(err).To(MatchError(runstate.ErrNotFound))
		})

		It("Should open a meta-only run and continue its sequence", func() {
			id := newID()
			j, err := store.Create(ctx, id, newMeta(id))
			Expect(err).ToNot(HaveOccurred())
			Expect(j.Close()).To(Succeed())

			j2, err := store.Open(ctx, id)
			Expect(err).ToNot(HaveOccurred())
			Expect(j2.LastSeq()).To(Equal(uint64(1)))
			Expect(j2.Append(ctx, 2, assistantRec(0))).To(Succeed())
			Expect(j2.Close()).To(Succeed())

			rs, err := store.Load(ctx, id)
			Expect(err).ToNot(HaveOccurred())
			Expect(rs.Counters.LlmCalls).To(Equal(int64(1)))
		})

		It("Should treat a duplicate seq as an idempotent no-op and reject gaps", func() {
			id := newID()
			j, err := store.Create(ctx, id, newMeta(id))
			Expect(err).ToNot(HaveOccurred())
			defer j.Close()

			Expect(j.Append(ctx, 2, assistantRec(0, "tu_1"))).To(Succeed())
			// Re-append the same seq (crash-retry): no error, no duplicate.
			Expect(j.Append(ctx, 2, assistantRec(0, "tu_1"))).To(Succeed())
			// A seq that skips ahead is a gap.
			Expect(j.Append(ctx, 5, toolResultRec("tu_1"))).To(MatchError(runstate.ErrSeqGap))

			recs, err := j.Records(ctx)
			Expect(err).ToNot(HaveOccurred())
			Expect(recs).To(HaveLen(2))
		})

		It("Should adopt its own lost-ack record instead of duplicating it", func() {
			id := newID()
			jr, err := store.Create(ctx, id, newMeta(id))
			Expect(err).ToNot(HaveOccurred())
			j := jr.(*journal)

			// Simulate a landed-but-unacked publish of record 2: publish it directly with
			// the journal's own msg id and the fence it would have used, so the journal's
			// tail view is now stale (it never saw the ack and did not advance).
			rec := assistantRec(0, "tu_1")
			rec.Seq = 2
			body, err := json.Marshal(rec)
			Expect(err).ToNot(HaveOccurred())
			_, err = js.Publish(ctx, j.store.subjectForSeq(id, 2), body,
				jetstream.WithMsgID(fmt.Sprintf("%s-%d", j.nonce, 2)),
				jetstream.WithExpectLastSequenceForSubject(j.tailStreamSeq, j.store.runWildcard(id)))
			Expect(err).ToNot(HaveOccurred())

			// The retry hits the fence, recognizes its own record, and adopts it: no
			// error and no duplicate, and the sequence continues cleanly.
			Expect(j.Append(ctx, 2, rec)).To(Succeed())
			Expect(j.LastSeq()).To(Equal(uint64(2)))
			Expect(j.Append(ctx, 3, toolResultRec("tu_1"))).To(Succeed())

			recs, err := j.Records(ctx)
			Expect(err).ToNot(HaveOccurred())
			Expect(recs).To(HaveLen(3))
		})

		It("Should fence a second writer out with ErrLocked", func() {
			id := newID()
			jA, err := store.Create(ctx, id, newMeta(id))
			Expect(err).ToNot(HaveOccurred())
			Expect(jA.Append(ctx, 2, assistantRec(0, "tu_1"))).To(Succeed())

			jB, err := store.Open(ctx, id)
			Expect(err).ToNot(HaveOccurred())
			Expect(jB.LastSeq()).To(Equal(uint64(2)))

			// Writer A advances the run, moving the tail under B.
			Expect(jA.Append(ctx, 3, toolResultRec("tu_1"))).To(Succeed())

			// B's next append collides with A's tail move and is safely rejected.
			err = jB.Append(ctx, 3, toolResultRec("tu_1"))
			Expect(err).To(MatchError(runstate.ErrLocked))
		})

		It("Should report a run as held until another writer takes it, then refuse it", func() {
			id := newID()
			jA, err := store.Create(ctx, id, newMeta(id))
			Expect(err).ToNot(HaveOccurred())
			Expect(jA.Append(ctx, 2, assistantRec(0, "tu_1"))).To(Succeed())
			Expect(jA.CheckHeld(ctx)).To(Succeed(), "nobody else has written, so A still holds it")

			// B takes the run the way a resume does, by writing before it does anything.
			jB, err := store.Open(ctx, id)
			Expect(err).ToNot(HaveOccurred())
			Expect(jB.Append(ctx, 3, claimRec("worker-b"))).To(Succeed())

			// A now finds out without having appended, which is the whole point: it can
			// stop before its next tool rather than after it.
			Expect(jA.CheckHeld(ctx)).To(MatchError(runstate.ErrLocked))
			Expect(jB.CheckHeld(ctx)).To(Succeed())

			// And the fence still holds against A's own next write.
			Expect(jA.Append(ctx, 3, toolResultRec("tu_1"))).To(MatchError(runstate.ErrLocked))
		})

		// The listing consults the context when a run fails to summarize, so that a
		// cancel is not read as a run this build cannot summarize. This pins the other
		// side of that branch: an unsummarizable run on a live context is still left out
		// and the runs around it are still listed.
		It("Should leave a run of an unsupported version out of the listing and list the rest", func() {
			good, bad := newID(), newID()
			jg, err := store.Create(ctx, good, newMeta(good))
			Expect(err).ToNot(HaveOccurred())

			// Create refuses a version this build does not write, so the unreadable run is
			// published straight onto the stream. What the listing has to survive is a
			// record already stored, whichever build stored it.
			meta := newMeta(bad)
			meta.Version = runstate.Version + 1
			body, err := json.Marshal(runstate.Record{Seq: 1, Protocol: runstate.MetaProtocol, Meta: &meta})
			Expect(err).ToNot(HaveOccurred())
			_, err = js.Publish(ctx, jg.(*journal).store.metaSubject(bad), body)
			Expect(err).ToNot(HaveOccurred())
			Expect(jg.Close()).To(Succeed())

			infos, err := store.List(ctx, runstate.ListFilter{})
			Expect(err).ToNot(HaveOccurred())
			Expect(infos).To(HaveLen(1))
			Expect(infos[0].RunID).To(Equal(good))
		})

		// A canceled caller gets an error rather than a listing missing whatever it did
		// not reach. Two runs are stored so a listing that swallowed the cancel would
		// have something to return.
		It("Should fail a listing when the caller's context is canceled, not return a short one", func() {
			idA, idB := newID(), newID()
			jA, err := store.Create(ctx, idA, newMeta(idA))
			Expect(err).ToNot(HaveOccurred())
			Expect(jA.Close()).To(Succeed())
			jB, err := store.Create(ctx, idB, newMeta(idB))
			Expect(err).ToNot(HaveOccurred())
			Expect(jB.Close()).To(Succeed())

			canceled, cancel := context.WithCancel(ctx)
			cancel()

			infos, err := store.List(canceled, runstate.ListFilter{})
			Expect(err).To(MatchError(context.Canceled))
			Expect(infos).To(BeEmpty())

			infos, err = store.List(ctx, runstate.ListFilter{})
			Expect(err).ToNot(HaveOccurred())
			Expect(infos).To(HaveLen(2), "both runs are there, so the refusal was the cancel")
		})

		It("Should list runs with their metadata", func() {
			idA, idB := newID(), newID()
			jA, err := store.Create(ctx, idA, newMeta(idA))
			Expect(err).ToNot(HaveOccurred())
			Expect(jA.Close()).To(Succeed())
			jB, err := store.Create(ctx, idB, newMeta(idB))
			Expect(err).ToNot(HaveOccurred())
			Expect(jB.Append(ctx, 2, assistantRec(0))).To(Succeed())
			Expect(jB.Close()).To(Succeed())

			infos, err := store.List(ctx, runstate.ListFilter{})
			Expect(err).ToNot(HaveOccurred())
			Expect(infos).To(HaveLen(2))

			ids := []string{infos[0].RunID, infos[1].RunID}
			Expect(ids).To(ConsistOf(idA, idB))
			for _, in := range infos {
				Expect(in.Model).To(Equal("claude-opus-4-8"))
				Expect(in.Prompt).To(Equal("hello"))
				Expect(in.Created).ToNot(BeZero())
				Expect(in.Updated).ToNot(BeZero())
			}
		})

		// Two agents on one operator-owned stream is the deployment the agent field
		// exists for. The subjects carry the run id and the seq and nothing else, so the
		// filter is answered from the meta record the listing already reads.
		Describe("listing by agent", func() {
			createFor := func(agent string) string {
				GinkgoHelper()

				id := newID()
				meta := newMeta(id)
				meta.Agent = agent

				j, err := store.Create(ctx, id, meta)
				Expect(err).ToNot(HaveOccurred())
				Expect(j.Close()).To(Succeed())

				return id
			}

			agentsByID := func(infos []runstate.RunInfo) map[string]string {
				out := map[string]string{}
				for _, info := range infos {
					out[info.RunID] = info.Agent
				}

				return out
			}

			It("Should carry the agent on every row and list every run for a zero filter", func() {
				a := createFor("agent-a")
				b := createFor("agent-b")
				unstamped := createFor("")

				infos, err := store.List(ctx, runstate.ListFilter{})
				Expect(err).ToNot(HaveOccurred())
				Expect(agentsByID(infos)).To(Equal(map[string]string{a: "agent-a", b: "agent-b", unstamped: ""}))
			})

			It("Should list one agent's runs and the runs nobody recorded an agent for", func() {
				a := createFor("agent-a")
				createFor("agent-b")
				unstamped := createFor("")

				infos, err := store.List(ctx, runstate.ListFilter{Agent: "agent-a"})
				Expect(err).ToNot(HaveOccurred())
				Expect(agentsByID(infos)).To(Equal(map[string]string{a: "agent-a", unstamped: ""}))
			})
		})

		It("Should report the ending off the last record", func() {
			id := newID()
			j, err := store.Create(ctx, id, newMeta(id))
			Expect(err).ToNot(HaveOccurred())
			Expect(j.Append(ctx, 2, assistantRec(0))).To(Succeed())
			Expect(j.Append(ctx, 3, terminalRec(runstate.ReasonCompleted))).To(Succeed())
			Expect(j.Close()).To(Succeed())

			infos, err := store.List(ctx, runstate.ListFilter{})
			Expect(err).ToNot(HaveOccurred())
			Expect(infos).To(HaveLen(1))
			Expect(infos[0].Terminal).To(Equal(runstate.ReasonCompleted))
		})

		Describe("listing the conversation summary", func() {
			It("Should carry the summary off the terminal record", func() {
				want := &runstate.ConversationSummary{
					Turns:         3,
					ContextTokens: 4096,
					Counters:      runstate.Counters{LlmCalls: 5, ToolCalls: 2, InTokens: 900},
				}

				id := newID()
				j, err := store.Create(ctx, id, newMeta(id))
				Expect(err).ToNot(HaveOccurred())
				Expect(j.Append(ctx, 2, assistantRec(0))).To(Succeed())

				rec := terminalRec(runstate.ReasonCompleted)
				rec.Terminal.Summary = want
				Expect(j.Append(ctx, 3, rec)).To(Succeed())
				Expect(j.Close()).To(Succeed())

				infos, err := store.List(ctx, runstate.ListFilter{})
				Expect(err).ToNot(HaveOccurred())
				Expect(infos).To(HaveLen(1))
				Expect(infos[0].Summary).To(Equal(want))
			})

			It("Should report no summary for a conversation whose last turn wrote none", func() {
				id := newID()
				j, err := store.Create(ctx, id, newMeta(id))
				Expect(err).ToNot(HaveOccurred())
				Expect(j.Append(ctx, 2, terminalRec(runstate.ReasonCompleted))).To(Succeed())
				Expect(j.Close()).To(Succeed())

				infos, err := store.List(ctx, runstate.ListFilter{})
				Expect(err).ToNot(HaveOccurred())
				Expect(infos).To(HaveLen(1))
				Expect(infos[0].Terminal).To(Equal(runstate.ReasonCompleted))
				Expect(infos[0].Summary).To(BeNil())
			})
		})

		// A meta record's stream sequence is assigned when the run is created and the
		// stream forbids rewriting the subject, so enumerating the meta subjects forward
		// is creation order.
		Describe("paging the listing", func() {
			createFor := func(agent string) string {
				GinkgoHelper()

				id := newID()
				meta := newMeta(id)
				meta.Agent = agent

				j, err := store.Create(ctx, id, meta)
				Expect(err).ToNot(HaveOccurred())
				Expect(j.Append(ctx, 2, terminalRec(runstate.ReasonCompleted))).To(Succeed())
				Expect(j.Close()).To(Succeed())

				return id
			}

			idsOf := func(page runstate.RunPage) []string {
				out := make([]string, 0, len(page.Runs))
				for _, info := range page.Runs {
					out = append(out, info.RunID)
				}

				return out
			}

			// walk pages the whole store the way a caller does: it hands back the cursor
			// it was given until a page carries none.
			walk := func(filter runstate.ListFilter, limit int) []string {
				GinkgoHelper()

				var out []string
				cursor := ""
				for range 20 {
					page, err := store.ListPage(ctx, filter, limit, cursor)
					Expect(err).ToNot(HaveOccurred())
					out = append(out, idsOf(page)...)
					if page.Cursor == "" {
						return out
					}
					cursor = page.Cursor
				}

				Fail("the walk never reached a page without a cursor")

				return nil
			}

			It("Should return a page smaller than the store, oldest first, with a cursor", func() {
				first := createFor("")
				second := createFor("")
				createFor("")

				page, err := store.ListPage(ctx, runstate.ListFilter{}, 2, "")
				Expect(err).ToNot(HaveOccurred())
				Expect(idsOf(page)).To(Equal([]string{first, second}))
				Expect(page.Cursor).ToNot(BeEmpty())
			})

			It("Should continue at the run the page ended on, repeating none and skipping none", func() {
				var want []string
				for range 5 {
					want = append(want, createFor(""))
				}

				Expect(walk(runstate.ListFilter{}, 2)).To(Equal(want))
				Expect(walk(runstate.ListFilter{}, 3)).To(Equal(want))
				Expect(walk(runstate.ListFilter{}, 1)).To(Equal(want))
			})

			It("Should carry the same row a full listing does", func() {
				createFor("agent-a")

				page, err := store.ListPage(ctx, runstate.ListFilter{}, 10, "")
				Expect(err).ToNot(HaveOccurred())

				infos, err := store.List(ctx, runstate.ListFilter{})
				Expect(err).ToNot(HaveOccurred())
				Expect(infos).To(HaveLen(1))
				Expect(page.Runs).To(Equal(infos))
			})

			It("Should end the walk on a page carrying no cursor", func() {
				createFor("")
				createFor("")
				third := createFor("")

				page, err := store.ListPage(ctx, runstate.ListFilter{}, 2, "")
				Expect(err).ToNot(HaveOccurred())

				page, err = store.ListPage(ctx, runstate.ListFilter{}, 2, page.Cursor)
				Expect(err).ToNot(HaveOccurred())
				Expect(idsOf(page)).To(Equal([]string{third}))
				Expect(page.Cursor).To(BeEmpty())
			})

			// A page that fills at the newest run cannot tell that nothing follows
			// without another fetch, so it carries a cursor and the caller finds an
			// empty page there.
			It("Should hand a full page a cursor at the end of the store and answer it with an empty page", func() {
				createFor("")
				createFor("")

				page, err := store.ListPage(ctx, runstate.ListFilter{}, 2, "")
				Expect(err).ToNot(HaveOccurred())
				Expect(page.Runs).To(HaveLen(2))
				Expect(page.Cursor).ToNot(BeEmpty())

				page, err = store.ListPage(ctx, runstate.ListFilter{}, 2, page.Cursor)
				Expect(err).ToNot(HaveOccurred())
				Expect(page.Runs).To(BeEmpty())
				Expect(page.Cursor).To(BeEmpty())
			})

			It("Should skip another agent's runs without shortening the page", func() {
				a1 := createFor("agent-a")
				createFor("agent-b")
				a2 := createFor("agent-a")
				createFor("agent-b")
				a3 := createFor("agent-a")

				page, err := store.ListPage(ctx, runstate.ListFilter{Agent: "agent-a"}, 2, "")
				Expect(err).ToNot(HaveOccurred())
				Expect(idsOf(page)).To(Equal([]string{a1, a2}))

				Expect(walk(runstate.ListFilter{Agent: "agent-a"}, 2)).To(Equal([]string{a1, a2, a3}))
			})

			// One store is handed to every channel, and a run id is a subject token, so
			// a channel listing on its own prefix drops another channel's run from the
			// delivered meta record without reading anything else.
			It("Should skip a run outside the prefix without shortening the page", func() {
				createID := func(prefix string) string {
					GinkgoHelper()

					id := prefix + newID()
					meta := newMeta(id)

					j, err := store.Create(ctx, id, meta)
					Expect(err).ToNot(HaveOccurred())
					Expect(j.Close()).To(Succeed())

					return id
				}

				w1 := createID("w-")
				createID("slack-")
				w2 := createID("w-")
				createID("slack-")
				w3 := createID("w-")

				page, err := store.ListPage(ctx, runstate.ListFilter{Prefix: "w-"}, 2, "")
				Expect(err).ToNot(HaveOccurred())
				Expect(idsOf(page)).To(Equal([]string{w1, w2}))

				Expect(walk(runstate.ListFilter{Prefix: "w-"}, 2)).To(Equal([]string{w1, w2, w3}))

				infos, err := store.List(ctx, runstate.ListFilter{Prefix: "w-"})
				Expect(err).ToNot(HaveOccurred())
				Expect(infos).To(HaveLen(3))
			})

			It("Should return an empty page and no cursor for an empty store", func() {
				page, err := store.ListPage(ctx, runstate.ListFilter{}, 20, "")
				Expect(err).ToNot(HaveOccurred())
				Expect(page.Runs).To(BeEmpty())
				Expect(page.Cursor).To(BeEmpty())
			})

			It("Should return the whole store, and no cursor, under a limit larger than it", func() {
				var want []string
				for range 3 {
					want = append(want, createFor(""))
				}

				page, err := store.ListPage(ctx, runstate.ListFilter{}, 20, "")
				Expect(err).ToNot(HaveOccurred())
				Expect(idsOf(page)).To(Equal(want))
				Expect(page.Cursor).To(BeEmpty())
			})

			It("Should leave out a run deleted between two pages", func() {
				first := createFor("")
				second := createFor("")
				third := createFor("")

				page, err := store.ListPage(ctx, runstate.ListFilter{}, 1, "")
				Expect(err).ToNot(HaveOccurred())
				Expect(idsOf(page)).To(Equal([]string{first}))

				Expect(store.Delete(ctx, second)).To(Succeed())

				page, err = store.ListPage(ctx, runstate.ListFilter{}, 1, page.Cursor)
				Expect(err).ToNot(HaveOccurred())
				Expect(idsOf(page)).To(Equal([]string{third}))
			})

			// A conversation started after the page was read sits after its cursor, so a
			// caller holding one reads what has been created since.
			It("Should enumerate a run created after the page at the cursor it returned", func() {
				createFor("")
				createFor("")

				page, err := store.ListPage(ctx, runstate.ListFilter{}, 2, "")
				Expect(err).ToNot(HaveOccurred())
				Expect(page.Cursor).ToNot(BeEmpty())

				later := createFor("")

				page, err = store.ListPage(ctx, runstate.ListFilter{}, 2, page.Cursor)
				Expect(err).ToNot(HaveOccurred())
				Expect(idsOf(page)).To(Equal([]string{later}))
			})

			It("Should leave a run of an unsupported version out of a page without shortening it", func() {
				good, bad, alsoGood := newID(), newID(), newID()

				jg, err := store.Create(ctx, good, newMeta(good))
				Expect(err).ToNot(HaveOccurred())
				Expect(jg.Close()).To(Succeed())

				meta := newMeta(bad)
				meta.Version = runstate.Version + 1
				body, err := json.Marshal(runstate.Record{Seq: 1, Protocol: runstate.MetaProtocol, Meta: &meta})
				Expect(err).ToNot(HaveOccurred())
				_, err = js.Publish(ctx, jg.(*journal).store.metaSubject(bad), body)
				Expect(err).ToNot(HaveOccurred())

				ja, err := store.Create(ctx, alsoGood, newMeta(alsoGood))
				Expect(err).ToNot(HaveOccurred())
				Expect(ja.Close()).To(Succeed())

				page, err := store.ListPage(ctx, runstate.ListFilter{}, 2, "")
				Expect(err).ToNot(HaveOccurred())
				Expect(idsOf(page)).To(Equal([]string{good, alsoGood}))
			})

			It("Should refuse a limit no page can hold", func() {
				_, err := store.ListPage(ctx, runstate.ListFilter{}, 0, "")
				Expect(err).To(MatchError(runstate.ErrInvalidLimit))

				_, err = store.ListPage(ctx, runstate.ListFilter{}, -1, "")
				Expect(err).To(MatchError(runstate.ErrInvalidLimit))
			})

			// The file backend's cursor is a base64 position, which is no stream
			// sequence, so it is refused rather than read as the start of the stream.
			It("Should refuse a cursor it did not mint", func() {
				createFor("")

				for _, cursor := range []string{"MTcwMDAwMDAwMHxydW4x", "not a cursor", "0"} {
					_, err := store.ListPage(ctx, runstate.ListFilter{}, 2, cursor)
					Expect(err).To(MatchError(runstate.ErrInvalidCursor), cursor)
				}
			})

			It("Should refuse a page on a context canceled before the call", func() {
				createFor("")

				canceled, cancel := context.WithCancel(ctx)
				cancel()

				_, err := store.ListPage(canceled, runstate.ListFilter{}, 2, "")
				Expect(err).To(MatchError(context.Canceled))
			})
		})

		// A caller holding the ids enumerates nothing: no consumer is built, no subject is
		// listed, and each row is the two reads a listing row already makes.
		Describe("describing a named set of runs", func() {
			createEnded := func(prefix, agent string, summary *runstate.ConversationSummary) string {
				GinkgoHelper()

				id := prefix + newID()
				meta := newMeta(id)
				meta.Agent = agent

				j, err := store.Create(ctx, id, meta)
				Expect(err).ToNot(HaveOccurred())

				rec := terminalRec(runstate.ReasonCompleted)
				rec.Terminal.Summary = summary
				Expect(j.Append(ctx, 2, rec)).To(Succeed())
				Expect(j.Close()).To(Succeed())

				return id
			}

			idsOf := func(infos []runstate.RunInfo) []string {
				out := make([]string, len(infos))
				for i, info := range infos {
					out[i] = info.RunID
				}

				return out
			}

			It("Should answer in the order the caller named the ids", func() {
				one := createEnded("w-", "agent-a", nil)
				two := createEnded("w-", "agent-a", nil)
				three := createEnded("w-", "agent-a", nil)

				infos, err := store.Describe(ctx, runstate.ListFilter{}, []string{three, one, two})
				Expect(err).ToNot(HaveOccurred())
				Expect(idsOf(infos)).To(Equal([]string{three, one, two}))
			})

			It("Should carry the times, the model, the ending and the summary", func() {
				want := &runstate.ConversationSummary{
					Turns:         3,
					ContextTokens: 4096,
					Counters:      runstate.Counters{LlmCalls: 5, ToolCalls: 2},
				}
				id := createEnded("w-", "agent-a", want)

				infos, err := store.Describe(ctx, runstate.ListFilter{}, []string{id})
				Expect(err).ToNot(HaveOccurred())
				Expect(infos).To(HaveLen(1))
				Expect(infos[0].Created).ToNot(BeZero())
				Expect(infos[0].Updated).To(BeTemporally(">=", infos[0].Created))
				Expect(infos[0].Model).To(Equal("claude-opus-4-8"))
				Expect(infos[0].Prompt).To(Equal("hello"))
				Expect(infos[0].Terminal).To(Equal(runstate.ReasonCompleted))
				Expect(infos[0].Summary).To(Equal(want))
			})

			It("Should report no summary for a conversation whose last turn wrote none", func() {
				id := createEnded("w-", "agent-a", nil)

				infos, err := store.Describe(ctx, runstate.ListFilter{}, []string{id})
				Expect(err).ToNot(HaveOccurred())
				Expect(infos).To(HaveLen(1))
				Expect(infos[0].Summary).To(BeNil())
			})

			It("Should carry the same row a full listing does", func() {
				id := createEnded("w-", "agent-a", nil)

				infos, err := store.Describe(ctx, runstate.ListFilter{}, []string{id})
				Expect(err).ToNot(HaveOccurred())

				listed, err := store.List(ctx, runstate.ListFilter{})
				Expect(err).ToNot(HaveOccurred())
				Expect(infos).To(Equal(listed))
			})

			It("Should leave out an id outside the prefix, another agent's run and a run it holds none of", func() {
				mine := createEnded("w-", "agent-a", nil)
				other := createEnded("slack-", "agent-a", nil)
				theirs := createEnded("w-", "agent-b", nil)
				unstamped := createEnded("w-", "", nil)

				filter := runstate.ListFilter{Agent: "agent-a", Prefix: "w-"}
				infos, err := store.Describe(ctx, filter, []string{mine, other, theirs, "w-" + newID(), unstamped})
				Expect(err).ToNot(HaveOccurred())
				Expect(idsOf(infos)).To(Equal([]string{mine, unstamped}))
			})

			It("Should describe an id named twice once, where it was first named", func() {
				one := createEnded("w-", "agent-a", nil)
				two := createEnded("w-", "agent-a", nil)

				infos, err := store.Describe(ctx, runstate.ListFilter{}, []string{one, two, one})
				Expect(err).ToNot(HaveOccurred())
				Expect(idsOf(infos)).To(Equal([]string{one, two}))
			})

			// The claim the endpoint above this is built on, counted rather than asserted:
			// a row is the meta record and the run's last record, a run the filter
			// excludes by its id is read at all, and the caller's ids leave nothing to
			// enumerate, so no consumer is created.
			It("Should read two records per row, nothing for an excluded id and create no consumer", func() {
				named := []string{
					createEnded("w-", "agent-a", nil),
					createEnded("w-", "agent-a", nil),
					createEnded("w-", "agent-a", nil),
				}
				excluded := createEnded("slack-", "agent-a", nil)

				reads, consumers := countRecordReads(nc, func() {
					infos, err := store.Describe(ctx, runstate.ListFilter{Prefix: "w-"}, append(named, excluded))
					Expect(err).ToNot(HaveOccurred())
					Expect(infos).To(HaveLen(3))
				})

				Expect(reads).To(Equal(2*len(named)), "the meta record and the last record of each named run")
				Expect(consumers).To(BeZero(), "the ids came from the caller, so nothing is enumerated")
			})

			// Both listings page past a run this build cannot read, and every run stored
			// under an earlier version is one, so naming such a run among ids that are
			// fine reads the other rows rather than failing the request.
			It("Should leave out a run of a version it does not read and one whose meta record is unreadable", func() {
				one := createEnded("w-", "agent-a", nil)
				two := createEnded("w-", "agent-a", nil)

				// Create refuses a version this build does not write, so both bad runs are
				// published straight onto the stream. What Describe has to survive is a
				// record already stored, whichever build stored it.
				throwaway := newID()
				j, err := store.Create(ctx, throwaway, newMeta(throwaway))
				Expect(err).ToNot(HaveOccurred())
				subjects := j.(*journal).store
				Expect(j.Close()).To(Succeed())

				old := "w-" + newID()
				meta := newMeta(old)
				meta.Version = runstate.Version + 1
				body, err := json.Marshal(runstate.Record{Seq: 1, Protocol: runstate.MetaProtocol, Meta: &meta})
				Expect(err).ToNot(HaveOccurred())
				_, err = js.Publish(ctx, subjects.metaSubject(old), body)
				Expect(err).ToNot(HaveOccurred())

				garbled := "w-" + newID()
				_, err = js.Publish(ctx, subjects.metaSubject(garbled), []byte("not json at all"))
				Expect(err).ToNot(HaveOccurred())

				infos, err := store.Describe(ctx, runstate.ListFilter{Prefix: "w-"}, []string{one, old, garbled, two})
				Expect(err).ToNot(HaveOccurred())
				Expect(idsOf(infos)).To(Equal([]string{one, two}))

				listed, err := store.List(ctx, runstate.ListFilter{Prefix: "w-"})
				Expect(err).ToNot(HaveOccurred())
				Expect(idsOf(listed)).To(ConsistOf(one, two), "the listing skips the same two runs")
			})

			// A caller naming twelve ids and reading eight rows takes the four it did not
			// read as runs this store has nothing readable under, so a read that failed for
			// any other reason fails the call instead of shortening the answer.
			It("Should fail the call when a read fails for any other reason", func() {
				id := createEnded("w-", "agent-a", nil)

				Expect(js.DeleteStream(ctx, "SESSIONS")).To(Succeed())

				_, err := store.Describe(ctx, runstate.ListFilter{}, []string{id})
				Expect(err).To(HaveOccurred())
				Expect(err).ToNot(MatchError(runstate.ErrCorrupt))
			})

			It("Should refuse a call on a context canceled before it", func() {
				id := createEnded("w-", "agent-a", nil)

				canceled, cancel := context.WithCancel(ctx)
				cancel()

				_, err := store.Describe(canceled, runstate.ListFilter{}, []string{id})
				Expect(err).To(HaveOccurred())
			})
		})

		// The one thing a listing reads differently from a fold. A fold keeps the last
		// terminal record it saw until another replaces it, so a conversation whose next
		// turn is under way still reads as completed; the last record says what is
		// actually true of it now.
		It("Should report a conversation with a turn in flight as open", func() {
			id := newID()
			j, err := store.Create(ctx, id, newMeta(id))
			Expect(err).ToNot(HaveOccurred())
			Expect(j.Append(ctx, 2, assistantRec(0))).To(Succeed())
			Expect(j.Append(ctx, 3, terminalRec(runstate.ReasonCompleted))).To(Succeed())
			// The next turn starts: a resume claims the journal before anything runs.
			Expect(j.Append(ctx, 4, claimRec("worker-a"))).To(Succeed())
			Expect(j.Append(ctx, 5, assistantRec(1))).To(Succeed())
			Expect(j.Close()).To(Succeed())

			infos, err := store.List(ctx, runstate.ListFilter{})
			Expect(err).ToNot(HaveOccurred())
			Expect(infos).To(HaveLen(1))
			Expect(infos[0].Terminal).To(BeEmpty())

			// The fold still carries the earlier ending, so the two differ on purpose
			// rather than by one of them losing the record.
			rs, err := store.Load(ctx, id)
			Expect(err).ToNot(HaveOccurred())
			Expect(rs.Terminal.Reason).To(Equal(runstate.ReasonCompleted))
		})

		// The listing must not grow a dependency on the middle of a journal again: every
		// column comes from the first record and the last, so a long conversation lists
		// exactly as a short one does.
		It("Should list a long conversation from its two ends", func() {
			id := newID()
			j, err := store.Create(ctx, id, newMeta(id))
			Expect(err).ToNot(HaveOccurred())

			seq := uint64(2)
			for i := range 40 {
				Expect(j.Append(ctx, seq, assistantRec(int64(i), "tu_x"))).To(Succeed())
				seq++
				Expect(j.Append(ctx, seq, toolResultRec("tu_x"))).To(Succeed())
				seq++
			}
			Expect(j.Append(ctx, seq, terminalRec(runstate.ReasonSuspended))).To(Succeed())
			Expect(j.Close()).To(Succeed())

			infos, err := store.List(ctx, runstate.ListFilter{})
			Expect(err).ToNot(HaveOccurred())
			Expect(infos).To(HaveLen(1))
			Expect(infos[0].RunID).To(Equal(id))
			Expect(infos[0].Prompt).To(Equal("hello"))
			Expect(infos[0].Model).To(Equal("claude-opus-4-8"))
			Expect(infos[0].Terminal).To(Equal(runstate.ReasonSuspended))
			Expect(infos[0].Updated).To(BeTemporally(">=", infos[0].Created))
		})

		Describe("the time a record is appended at", func() {
			It("Should stamp every record it publishes and keep a time the caller stamped", func() {
				at := time.Unix(1700000000, 0).UTC()

				id := newID()
				j, err := store.Create(ctx, id, newMeta(id))
				Expect(err).ToNot(HaveOccurred())
				Expect(j.Append(ctx, 2, assistantRec(0, "tu_1"))).To(Succeed())

				answered := toolResultRec("tu_1")
				answered.Time = at
				Expect(j.Append(ctx, 3, answered)).To(Succeed())

				recs, err := j.Records(ctx)
				Expect(err).ToNot(HaveOccurred())
				Expect(j.Close()).To(Succeed())

				Expect(recs).To(HaveLen(3))
				Expect(recs[0].Time).ToNot(BeZero(), "the meta record is stamped too")
				Expect(recs[1].Time).ToNot(BeZero())
				Expect(recs[1].Time.Location()).To(Equal(time.UTC))
				Expect(recs[2].Time).To(BeTemporally("==", at))
			})

			It("Should date a listing row from the last record", func() {
				at := time.Unix(1700000000, 0).UTC()

				id := newID()
				j, err := store.Create(ctx, id, newMeta(id))
				Expect(err).ToNot(HaveOccurred())

				ending := terminalRec(runstate.ReasonCompleted)
				ending.Time = at
				Expect(j.Append(ctx, 2, ending)).To(Succeed())
				Expect(j.Close()).To(Succeed())

				infos, err := store.List(ctx, runstate.ListFilter{})
				Expect(err).ToNot(HaveOccurred())
				Expect(infos).To(HaveLen(1))
				Expect(infos[0].Updated).To(BeTemporally("==", at))
			})

			// Every journal written before the field existed is this shape, and the
			// stream's store time is what this store has always reported for one.
			It("Should date a journal whose records carry no time from the stream", func() {
				id := newID()
				meta := newMeta(id)
				meta.Version = runstate.Version

				before := time.Now().UTC()
				publishUnstamped(ctx, store, js, id,
					runstate.Record{Seq: 1, Protocol: runstate.MetaProtocol, Meta: &meta},
					runstate.Record{Seq: 2, Protocol: runstate.TerminalProtocol, Terminal: &runstate.TerminalRecord{Reason: runstate.ReasonCompleted}},
				)
				after := time.Now().UTC()

				infos, err := store.List(ctx, runstate.ListFilter{})
				Expect(err).ToNot(HaveOccurred())
				Expect(infos).To(HaveLen(1))
				Expect(infos[0].Terminal).To(Equal(runstate.ReasonCompleted))
				Expect(infos[0].Updated).To(BeTemporally(">=", before))
				Expect(infos[0].Updated).To(BeTemporally("<=", after))
			})
		})

		It("Should delete a run idempotently", func() {
			id := newID()
			j, err := store.Create(ctx, id, newMeta(id))
			Expect(err).ToNot(HaveOccurred())
			Expect(j.Append(ctx, 2, assistantRec(0))).To(Succeed())
			Expect(j.Close()).To(Succeed())

			Expect(store.Delete(ctx, id)).To(Succeed())
			_, err = store.Load(ctx, id)
			Expect(err).To(MatchError(runstate.ErrNotFound))

			// Purging an absent run is a no-op.
			Expect(store.Delete(ctx, id)).To(Succeed())
		})

		It("Should fold identically to the file backend", func() {
			id := newID()
			meta := newMeta(id)
			appends := []struct {
				seq uint64
				rec runstate.Record
			}{
				{2, assistantRec(0, "tu_1")},
				{3, toolResultRec("tu_1")},
				{4, assistantRec(1)},
			}

			jj, err := store.Create(ctx, id, meta)
			Expect(err).ToNot(HaveOccurred())
			for _, a := range appends {
				Expect(jj.Append(ctx, a.seq, a.rec)).To(Succeed())
			}
			Expect(jj.Close()).To(Succeed())
			jsRS, err := store.Load(ctx, id)
			Expect(err).ToNot(HaveOccurred())

			fstore, err := file.NewFileStore(GinkgoT().TempDir())
			Expect(err).ToNot(HaveOccurred())
			fj, err := fstore.Create(ctx, id, meta)
			Expect(err).ToNot(HaveOccurred())
			for _, a := range appends {
				Expect(fj.Append(ctx, a.seq, a.rec)).To(Succeed())
			}
			Expect(fj.Close()).To(Succeed())
			fileRS, err := fstore.Load(ctx, id)
			Expect(err).ToNot(HaveOccurred())

			// Each store stamped its own records as it appended them, so the two runs are
			// dated the milliseconds apart they were written. The times are compared for
			// shape and then dropped, leaving the conversation both folds derived from the
			// same records.
			Expect(jsRS.Times).To(HaveLen(len(fileRS.Times)))
			Expect(jsRS.ResultTimes).To(HaveLen(len(fileRS.ResultTimes)))
			jsRS.Times, fileRS.Times = nil, nil
			jsRS.ResultTimes, fileRS.ResultTimes = nil, nil

			Expect(jsRS).To(Equal(fileRS))
		})
	})

	// The saving the paged listing exists for, counted rather than asserted. A row costs
	// one read, the run's last record, because the meta record arrives as the fetched
	// message. List costs two reads of every stored run before the caller sees a row.
	Describe("what a page costs", func() {
		fill := func(stream, prefix string, runs int) runstate.Store {
			GinkgoHelper()

			createStream(goodStream(stream, prefix+".>"))
			store, err := newStoreFor(stream)
			Expect(err).ToNot(HaveOccurred())

			for range runs {
				id := newID()
				j, err := store.Create(ctx, id, newMeta(id))
				Expect(err).ToNot(HaveOccurred())
				Expect(j.Append(ctx, 2, terminalRec(runstate.ReasonCompleted))).To(Succeed())
				Expect(j.Close()).To(Succeed())
			}

			return store
		}

		It("Should read a page of rows rather than a store of runs", func() {
			const (
				small = 5
				large = 40
				page  = 5
			)

			smallStore := fill("SMALL", "small", small)
			largeStore := fill("LARGE", "large", large)

			pageOnSmall, pageConsumers := countRecordReads(nc, func() {
				_, err := smallStore.ListPage(ctx, runstate.ListFilter{}, page, "")
				Expect(err).ToNot(HaveOccurred())
			})
			pageOnLarge, _ := countRecordReads(nc, func() {
				_, err := largeStore.ListPage(ctx, runstate.ListFilter{}, page, "")
				Expect(err).ToNot(HaveOccurred())
			})
			doublePage, _ := countRecordReads(nc, func() {
				_, err := largeStore.ListPage(ctx, runstate.ListFilter{}, 2*page, "")
				Expect(err).ToNot(HaveOccurred())
			})
			listSmall, _ := countRecordReads(nc, func() {
				_, err := smallStore.List(ctx, runstate.ListFilter{})
				Expect(err).ToNot(HaveOccurred())
			})
			listLarge, _ := countRecordReads(nc, func() {
				_, err := largeStore.List(ctx, runstate.ListFilter{})
				Expect(err).ToNot(HaveOccurred())
			})

			// The consumer a page builds to enumerate is what Describe is counted
			// against, so the count that has to be zero there fires here.
			Expect(pageConsumers).To(BeNumerically(">", 0), "a page enumerates the meta subjects through a consumer")

			Expect(pageOnLarge).To(Equal(page), "one read per row, the run's last record")
			Expect(pageOnSmall).To(Equal(pageOnLarge), "a page of five costs five reads whether the store holds five runs or forty")
			Expect(doublePage-pageOnLarge).To(Equal(page), "each further row costs one further read")

			Expect(listSmall).To(Equal(2*small), "a full listing reads the meta record and the last record of every run")
			Expect(listLarge).To(Equal(2*large), "and the store is what it grows with")
			Expect(pageOnLarge).To(BeNumerically("<", listLarge))
		})

		// A conversation that ran for a while costs a page no more than a conversation
		// that answered once: neither the fetch nor the tail read grows with the journal.
		It("Should read a long conversation and a short one at the same cost", func() {
			createStream(goodStream("MIXED", "mixed.>"))
			store, err := newStoreFor("MIXED")
			Expect(err).ToNot(HaveOccurred())

			id := newID()
			j, err := store.Create(ctx, id, newMeta(id))
			Expect(err).ToNot(HaveOccurred())
			seq := uint64(2)
			for i := range 40 {
				Expect(j.Append(ctx, seq, assistantRec(int64(i), "tu_x"))).To(Succeed())
				seq++
				Expect(j.Append(ctx, seq, toolResultRec("tu_x"))).To(Succeed())
				seq++
			}
			Expect(j.Append(ctx, seq, terminalRec(runstate.ReasonCompleted))).To(Succeed())
			Expect(j.Close()).To(Succeed())

			reads, _ := countRecordReads(nc, func() {
				page, err := store.ListPage(ctx, runstate.ListFilter{}, 5, "")
				Expect(err).ToNot(HaveOccurred())
				Expect(page.Runs).To(HaveLen(1))
				Expect(page.Runs[0].Terminal).To(Equal(runstate.ReasonCompleted))
			})

			Expect(reads).To(Equal(1), "eighty one records in the journal, one read to summarize it")
		})
	})
})
