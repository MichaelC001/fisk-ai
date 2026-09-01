//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agenttest_test

import (
	"context"
	"fmt"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/runstate"
)

var _ = Describe("FakeSessionStore", func() {
	var (
		ctx   context.Context
		store *agenttest.FakeSessionStore
	)

	BeforeEach(func() {
		ctx = context.Background()
		store = agenttest.NewFakeSessionStore(GinkgoTB())
	})

	create := func(id, prompt string) runstate.Journal {
		GinkgoHelper()

		j, err := store.Create(ctx, id, runstate.MetaRecord{RunID: id, Prompt: prompt})
		Expect(err).ToNot(HaveOccurred())

		return j
	}

	It("Should implement runstate.Store", func() {
		var s runstate.Store = agenttest.BuildFakeSessionStore()
		Expect(s.Info().Backend).To(Equal("fake"))
	})

	It("Should stamp the record version on the meta record it frames a run with", func() {
		j := create("run1", "do the thing")

		Expect(j.LastSeq()).To(Equal(uint64(1)))

		records, err := j.Records(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(records).To(HaveLen(1))
		Expect(records[0].Protocol).To(Equal(runstate.MetaProtocol))
		Expect(records[0].Seq).To(Equal(uint64(1)))
		Expect(records[0].Meta.Version).To(Equal(runstate.Version))
		Expect(records[0].Meta.Prompt).To(Equal("do the thing"))
	})

	// The caller's meta record is not written through, so a caller that reuses one is not
	// handed a version stamped by a store it no longer talks to.
	It("Should leave the caller's meta record alone", func() {
		meta := runstate.MetaRecord{RunID: "run1", Prompt: "do the thing"}

		_, err := store.Create(ctx, "run1", meta)
		Expect(err).ToNot(HaveOccurred())

		Expect(meta.Version).To(Equal(0))
	})

	It("Should refuse a second run under the same id", func() {
		create("run1", "do the thing")

		_, err := store.Create(ctx, "run1", runstate.MetaRecord{RunID: "run1"})
		Expect(err).To(MatchError(runstate.ErrExists))
	})

	It("Should refuse an id no backend would accept", func() {
		_, err := store.Create(ctx, "../escape", runstate.MetaRecord{})
		Expect(err).To(MatchError(runstate.ErrInvalidID))

		_, err = store.Open(ctx, "../escape")
		Expect(err).To(MatchError(runstate.ErrInvalidID))

		_, err = store.Load(ctx, "../escape")
		Expect(err).To(MatchError(runstate.ErrInvalidID))
	})

	It("Should report a run nobody created", func() {
		_, err := store.Open(ctx, "missing")
		Expect(err).To(MatchError(runstate.ErrNotFound))

		_, err = store.Load(ctx, "missing")
		Expect(err).To(MatchError(runstate.ErrNotFound))
	})

	It("Should hold a run's lock until the journal is closed", func() {
		j := create("run1", "do the thing")

		_, err := store.Open(ctx, "run1")
		Expect(err).To(MatchError(runstate.ErrLocked))

		Expect(j.Close()).To(Succeed())

		reopened, err := store.Open(ctx, "run1")
		Expect(err).ToNot(HaveOccurred())
		Expect(reopened.LastSeq()).To(Equal(uint64(1)))
	})

	// A journal on a shared store discovers it lost the run when it writes, so Evict is
	// how a spec reaches the take-over path with one writer.
	It("Should refuse a journal whose run was taken over", func() {
		j := create("run1", "do the thing")

		Expect(j.CheckHeld(ctx)).To(Succeed())

		store.Evict("run1")

		Expect(j.CheckHeld(ctx)).To(MatchError(runstate.ErrLocked))
		Expect(j.Append(ctx, 2, runstate.Record{Protocol: runstate.ClaimProtocol,
			Claim: &runstate.ClaimRecord{By: "worker"}})).To(MatchError(runstate.ErrLocked))
	})

	It("Should ignore an evict for an id it never created", func() {
		Expect(func() { store.Evict("missing") }).ToNot(Panic())
	})

	// A closed journal is still the store's, so evicting its run reaches it: the run opens
	// again and the journal that opens it holds nothing.
	It("Should take over a run whose journal was closed", func() {
		j := create("run1", "do the thing")
		Expect(j.Close()).To(Succeed())

		store.Evict("run1")

		reopened, err := store.Open(ctx, "run1")
		Expect(err).ToNot(HaveOccurred())
		Expect(reopened.CheckHeld(ctx)).To(MatchError(runstate.ErrLocked))
		Expect(reopened.Append(ctx, 2, runstate.Record{Protocol: runstate.ClaimProtocol,
			Claim: &runstate.ClaimRecord{By: "worker"}})).To(MatchError(runstate.ErrLocked))
	})

	It("Should fold a duplicate seq and refuse one that skips ahead", func() {
		j := create("run1", "do the thing")

		claim := runstate.Record{Protocol: runstate.ClaimProtocol, Claim: &runstate.ClaimRecord{By: "worker"}}

		Expect(j.Append(ctx, 2, claim)).To(Succeed())
		Expect(j.Append(ctx, 2, claim)).To(Succeed(), "a crash-retry of the last event is a no-op")
		Expect(j.LastSeq()).To(Equal(uint64(2)))

		Expect(j.Append(ctx, 5, claim)).To(MatchError(runstate.ErrSeqGap))
		Expect(j.LastSeq()).To(Equal(uint64(2)))

		records, err := j.Records(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(records).To(HaveLen(2))
	})

	It("Should load and list the runs it holds", func() {
		create("run1", "the first prompt")
		create("run2", "the second prompt")

		rs, err := store.Load(ctx, "run1")
		Expect(err).ToNot(HaveOccurred())
		Expect(rs.Prompt).To(Equal("the first prompt"))

		infos, err := store.List(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(infos).To(ConsistOf(
			runstate.RunInfo{RunID: "run1", Prompt: "the first prompt"},
			runstate.RunInfo{RunID: "run2", Prompt: "the second prompt"},
		))
	})

	It("Should forget a deleted run", func() {
		create("run1", "do the thing")

		Expect(store.Delete(ctx, "run1")).To(Succeed())

		_, err := store.Load(ctx, "run1")
		Expect(err).To(MatchError(runstate.ErrNotFound))
	})

	It("Should report the backend a spec told it to claim", func() {
		store.SetInfo(runstate.Info{Backend: "jetstream", Location: "RUNS"})

		Expect(store.Info()).To(Equal(runstate.Info{Backend: "jetstream", Location: "RUNS"}))
	})

	// The cancellation contract says every method returns the context's error before it
	// touches the map, so a spec can cancel a caller and see the store refuse.
	It("Should refuse every call made on a canceled context", func() {
		create("run1", "do the thing")

		canceled, cancel := context.WithCancel(ctx)
		cancel()

		_, err := store.Create(canceled, "run2", runstate.MetaRecord{})
		Expect(err).To(MatchError(context.Canceled))

		_, err = store.Open(canceled, "run1")
		Expect(err).To(MatchError(context.Canceled))

		_, err = store.Load(canceled, "run1")
		Expect(err).To(MatchError(context.Canceled))

		_, err = store.List(canceled)
		Expect(err).To(MatchError(context.Canceled))

		Expect(store.Delete(canceled, "run1")).To(MatchError(context.Canceled))
	})

	It("Should refuse a journal call made on a canceled context", func() {
		j := create("run1", "do the thing")

		canceled, cancel := context.WithCancel(ctx)
		cancel()

		Expect(j.Append(canceled, 2, runstate.Record{Protocol: runstate.ClaimProtocol,
			Claim: &runstate.ClaimRecord{By: "worker"}})).To(MatchError(context.Canceled))

		_, err := j.Records(canceled)
		Expect(err).To(MatchError(context.Canceled))

		Expect(j.CheckHeld(canceled)).To(MatchError(context.Canceled))
	})

	It("Should serve runs sharing one store", func() {
		const runs = 8

		var wg sync.WaitGroup
		for i := 0; i < runs; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer GinkgoRecover()

				id := fmt.Sprintf("run%d", i)

				j, err := store.Create(ctx, id, runstate.MetaRecord{RunID: id, Prompt: "concurrent"})
				Expect(err).ToNot(HaveOccurred())

				Expect(j.Append(ctx, 2, runstate.Record{Protocol: runstate.ClaimProtocol,
					Claim: &runstate.ClaimRecord{By: "worker"}})).To(Succeed())

				_, err = store.Load(ctx, id)
				Expect(err).ToNot(HaveOccurred())

				_, err = store.List(ctx)
				Expect(err).ToNot(HaveOccurred())

				Expect(j.Close()).To(Succeed())
			}()
		}

		wg.Wait()

		infos, err := store.List(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(infos).To(HaveLen(runs))
	})
})
