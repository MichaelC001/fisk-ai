//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agenttest_test

import (
	"context"
	"errors"
	"fmt"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/memory"
)

var _ = Describe("FakeMemoryStore", func() {
	var (
		ctx   context.Context
		store *agenttest.FakeMemoryStore
	)

	BeforeEach(func() {
		ctx = context.Background()
		store = agenttest.NewFakeMemoryStore(GinkgoTB())
	})

	It("Should implement memory.Store", func() {
		var s memory.Store = agenttest.BuildFakeMemoryStore()
		Expect(s.Info().Backend).To(Equal("fake"))
	})

	It("Should read back what was written", func() {
		Expect(store.Write(ctx, "notes", "some notes", "the body", false)).To(Succeed())

		description, content, err := store.Read(ctx, "notes")
		Expect(err).ToNot(HaveOccurred())
		Expect(description).To(Equal("some notes"))
		Expect(content).To(Equal("the body"))
	})

	It("Should report a key that was never written", func() {
		_, _, err := store.Read(ctx, "missing")
		Expect(err).To(MatchError(memory.ErrNotExist))
	})

	It("Should refuse to overwrite unless asked to", func() {
		Expect(store.Write(ctx, "notes", "first", "one", false)).To(Succeed())
		Expect(store.Write(ctx, "notes", "second", "two", false)).To(MatchError(memory.ErrExists))
		Expect(store.Write(ctx, "notes", "second", "two", true)).To(Succeed())

		description, content, err := store.Read(ctx, "notes")
		Expect(err).ToNot(HaveOccurred())
		Expect(description).To(Equal("second"))
		Expect(content).To(Equal("two"))
	})

	It("Should list every memory by key", func() {
		Expect(store.Write(ctx, "zebra", "last", "z", false)).To(Succeed())
		Expect(store.Write(ctx, "alpha", "first", "a", false)).To(Succeed())

		items, err := store.List(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(items).To(Equal([]memory.Item{
			{Key: "alpha", Description: "first"},
			{Key: "zebra", Description: "last"},
		}))
	})

	It("Should report whether a delete removed anything", func() {
		Expect(store.Write(ctx, "notes", "some notes", "the body", false)).To(Succeed())

		removed, err := store.Delete(ctx, "notes")
		Expect(err).ToNot(HaveOccurred())
		Expect(removed).To(BeTrue())

		removed, err = store.Delete(ctx, "notes")
		Expect(err).ToNot(HaveOccurred())
		Expect(removed).To(BeFalse())
	})

	It("Should report the backend a spec told it to claim", func() {
		store.SetInfo(memory.Info{Backend: "jetstream", Location: "MEMORIES"})

		Expect(store.Info()).To(Equal(memory.Info{Backend: "jetstream", Location: "MEMORIES"}))
	})

	// A listing failure is advisory rather than fatal, so a store that could not produce
	// one would leave that path looking covered when it is not.
	It("Should fail a listing while a spec asks it to", func() {
		unreachable := errors.New("the backend is unreachable")
		store.SetListError(unreachable)

		items, err := store.List(ctx)
		Expect(items).To(BeNil())
		Expect(err).To(MatchError(unreachable))

		store.SetListError(nil)
		_, err = store.List(ctx)
		Expect(err).ToNot(HaveOccurred())
	})

	It("Should serve runs sharing one store", func() {
		const runs = 8
		const each = 20

		var wg sync.WaitGroup
		for i := 0; i < runs; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer GinkgoRecover()

				for j := 0; j < each; j++ {
					key := fmt.Sprintf("run-%d-%d", i, j)

					Expect(store.Write(ctx, key, "written", "body", false)).To(Succeed())

					_, _, err := store.Read(ctx, key)
					Expect(err).ToNot(HaveOccurred())

					_, err = store.List(ctx)
					Expect(err).ToNot(HaveOccurred())

					store.Info()
				}
			}()
		}

		wg.Wait()

		items, err := store.List(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(items).To(HaveLen(runs * each))
	})
})
