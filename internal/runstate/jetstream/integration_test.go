//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package jetstream

import (
	"context"
	"encoding/json"
	"fmt"
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

			infos, err := store.List(ctx)
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

			infos, err := store.List(canceled)
			Expect(err).To(MatchError(context.Canceled))
			Expect(infos).To(BeEmpty())

			infos, err = store.List(ctx)
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

			infos, err := store.List(ctx)
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

		It("Should report the ending off the last record", func() {
			id := newID()
			j, err := store.Create(ctx, id, newMeta(id))
			Expect(err).ToNot(HaveOccurred())
			Expect(j.Append(ctx, 2, assistantRec(0))).To(Succeed())
			Expect(j.Append(ctx, 3, terminalRec(runstate.ReasonCompleted))).To(Succeed())
			Expect(j.Close()).To(Succeed())

			infos, err := store.List(ctx)
			Expect(err).ToNot(HaveOccurred())
			Expect(infos).To(HaveLen(1))
			Expect(infos[0].Terminal).To(Equal(runstate.ReasonCompleted))
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

			infos, err := store.List(ctx)
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

			infos, err := store.List(ctx)
			Expect(err).ToNot(HaveOccurred())
			Expect(infos).To(HaveLen(1))
			Expect(infos[0].RunID).To(Equal(id))
			Expect(infos[0].Prompt).To(Equal("hello"))
			Expect(infos[0].Model).To(Equal("claude-opus-4-8"))
			Expect(infos[0].Terminal).To(Equal(runstate.ReasonSuspended))
			Expect(infos[0].Updated).To(BeTemporally(">=", infos[0].Created))
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

			Expect(jsRS).To(Equal(fileRS))
		})
	})
})
