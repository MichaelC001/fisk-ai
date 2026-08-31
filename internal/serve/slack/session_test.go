//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package slack

import (
	"context"
	"fmt"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/runstate"
)

var _ = Describe("SessionFor", func() {
	It("Should derive a stable id from the identity and the three Slack identifiers", func() {
		id := SessionFor("agent1", "T1", "C1", "1700000000.000100")

		Expect(id).To(HavePrefix("s-"))
		Expect(id).To(HaveLen(66), "the prefix and a hex SHA-256")
		Expect(SessionFor("agent1", "T1", "C1", "1700000000.000100")).To(Equal(id), "the same thread reaches the same journal on every turn")
	})

	// The thread timestamp is unique only within a channel, so a worker answering in two
	// workspaces, or in two channels that minted the same timestamp, would otherwise
	// merge their conversations.
	It("Should separate conversations on every field it hashes", func() {
		base := SessionFor("agent1", "T1", "C1", "1700000000.000100")

		Expect(SessionFor("agent2", "T1", "C1", "1700000000.000100")).ToNot(Equal(base), "another agent")
		Expect(SessionFor("agent1", "T2", "C1", "1700000000.000100")).ToNot(Equal(base), "another workspace")
		Expect(SessionFor("agent1", "T1", "C2", "1700000000.000100")).ToNot(Equal(base), "another channel")
		Expect(SessionFor("agent1", "T1", "C1", "1700000000.000200")).ToNot(Equal(base), "another thread")
	})

	// Nothing a person writes reaches the store, which is what stops one member of a
	// workspace naming another's journal.
	It("Should produce an id the store accepts", func() {
		Expect(runstate.ValidateID(SessionFor("agent1", "T1", "C1", "1700000000.000100"))).To(Succeed())
	})
})

var _ = Describe("held", func() {
	var (
		store *agenttest.FakeSessionStore
		ch    *Channel
	)

	BeforeEach(func() {
		store = agenttest.NewFakeSessionStore(GinkgoTB())

		opts := testOptions()
		opts.Sessions = store

		ch = newTestChannel(opts, newFakeAPI(), newFakeSocket())
	})

	It("Should report a thread nobody has answered as not held", func() {
		held, err := ch.held(context.Background(), SessionFor("test.agent", "T1", "C1", "1700000000.000100"))
		Expect(err).ToNot(HaveOccurred())
		Expect(held).To(BeFalse())
	})

	It("Should report a thread with a journal as held", func() {
		id := SessionFor("test.agent", "T1", "C1", "1700000000.000100")

		j, err := store.Create(context.Background(), id, runstate.MetaRecord{Version: runstate.Version, RunID: id})
		Expect(err).ToNot(HaveOccurred())
		Expect(j.Close()).To(Succeed())

		held, err := ch.held(context.Background(), id)
		Expect(err).ToNot(HaveOccurred())
		Expect(held).To(BeTrue())
	})

	// A store that cannot be read is not the same answer as a thread that is not there:
	// treating it as absent would create a second journal beside one that exists.
	It("Should report a store failure rather than answering not held", func() {
		opts := testOptions()
		opts.Sessions = &failingStore{err: fmt.Errorf("the store is unreachable")}

		broken := newTestChannel(opts, newFakeAPI(), newFakeSocket())

		_, err := broken.held(context.Background(), "s-whatever")
		Expect(err).To(MatchError(ContainSubstring("the store is unreachable")))
	})
})

var _ = Describe("checkpointFor", func() {
	It("Should create the journal for a thread nobody has answered", func() {
		cp := checkpointFor("s-1", false)

		Expect(cp.ResumeID).To(Equal("s-1"))
		Expect(cp.CreateIfMissing).To(BeTrue())
		Expect(cp.FollowUp).To(BeFalse())
		Expect(cp.Force).To(BeFalse(), "there is nothing yet to force a resume against")
	})

	It("Should add a turn to a thread it holds", func() {
		cp := checkpointFor("s-1", true)

		Expect(cp.ResumeID).To(Equal("s-1"))
		Expect(cp.FollowUp).To(BeTrue())
		Expect(cp.CreateIfMissing).To(BeFalse(), "agent.Run refuses the two together by name")
	})

	// A thread's next turn may arrive days later and across a deploy. Without Force the
	// resume is refused whenever the model or the prompt has moved, which would kill every
	// open thread in the workspace on one configuration edit.
	It("Should force a resume so a deploy does not end every open thread", func() {
		Expect(checkpointFor("s-1", true).Force).To(BeTrue())
	})
})

// failingStore answers every read with one error, for the case where the store is
// reachable at construction and not afterwards.
type failingStore struct {
	runstate.Store

	err error
}

func (f *failingStore) Load(context.Context, string) (*runstate.RunState, error) {
	return nil, f.err
}

var _ = Describe("seen", func() {
	It("Should take a message once and refuse it afterwards", func() {
		s := newSeen(0)

		Expect(s.take("C1", "1700000000.000100")).To(BeTrue())
		Expect(s.take("C1", "1700000000.000100")).To(BeFalse(), "a redelivery of the same message")
		Expect(s.take("C1", "1700000000.000200")).To(BeTrue(), "a different message")
	})

	// A message timestamp is unique within its channel and nowhere else.
	It("Should key on the channel as well as the timestamp", func() {
		s := newSeen(0)

		Expect(s.take("C1", "1700000000.000100")).To(BeTrue())
		Expect(s.take("C2", "1700000000.000100")).To(BeTrue())
	})

	It("Should forget the oldest once it is full, and stay bounded", func() {
		s := newSeen(2)

		Expect(s.take("C1", "1")).To(BeTrue())
		Expect(s.take("C1", "2")).To(BeTrue())
		Expect(s.take("C1", "3")).To(BeTrue())

		Expect(s.take("C1", "1")).To(BeTrue(), "the oldest was evicted, so it is taken again")
		Expect(s.take("C1", "3")).To(BeFalse(), "the newest is still remembered")
		Expect(s.ids).To(HaveLen(2))
		Expect(s.order).To(HaveLen(2))
	})

	// The reading goroutine takes a message while a run's ending and a drain touch the
	// channel, so the set is reached from more than one goroutine at a time.
	It("Should take each message exactly once under concurrency", func() {
		s := newSeen(0)

		var (
			wg    sync.WaitGroup
			mu    sync.Mutex
			count int
		)

		for range 8 {
			wg.Go(func() {
				for j := range 50 {
					if s.take("C1", fmt.Sprintf("m%d", j)) {
						mu.Lock()
						count++
						mu.Unlock()
					}
				}
			})
		}
		wg.Wait()

		Expect(count).To(Equal(50), "fifty messages, however many goroutines raced for them")
	})
})
