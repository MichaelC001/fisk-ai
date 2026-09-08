//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/runstate"
)

var _ = Describe("Sessions", func() {
	Describe("SessionFor", func() {
		It("Should hash the identity and the thread under the web prefix", func() {
			id := SessionFor("agent1", "thread-1")
			Expect(id).To(HavePrefix("w-"))
			Expect(id).To(HaveLen(66))
			Expect(runstate.ValidateID(id)).To(Succeed())

			Expect(SessionFor("agent1", "thread-1")).To(Equal(id), "the same thread reaches the same journal")
			Expect(SessionFor("agent2", "thread-1")).ToNot(Equal(id), "two agents keep their conversations apart")
			Expect(SessionFor("agent1", "thread-2")).ToNot(Equal(id))
		})
	})

	Describe("checkpointFor", func() {
		// The three shapes: create, resume with the prompt as the next turn, and resume on
		// an answer alone. Force belongs to every resuming shape, since an operator
		// restarting with another model would otherwise end every open thread.
		It("Should create a thread the store does not hold", func() {
			Expect(checkpointFor("w-1", false, "hello")).To(Equal(agent.Checkpoint{ResumeID: "w-1", CreateIfMissing: true}))
		})

		It("Should resume a held thread with the prompt as a follow-up", func() {
			Expect(checkpointFor("w-1", true, "hello")).To(Equal(agent.Checkpoint{ResumeID: "w-1", FollowUp: true, Force: true}))
		})

		It("Should resume a held thread carrying only an answer with neither", func() {
			Expect(checkpointFor("w-1", true, "")).To(Equal(agent.Checkpoint{ResumeID: "w-1", Force: true}))
		})
	})

	Describe("held", func() {
		// The store answers rather than a map in memory, so a second process serving the
		// thread's next turn sees the conversation the first one opened.
		It("Should read the store", func() {
			store := agenttest.NewFakeSessionStore(GinkgoTB())
			opts := testOptions()
			opts.Sessions = store
			ch := newTestChannel(opts)

			id := SessionFor("agent1", "thread-1")

			held, err := ch.held(context.Background(), id)
			Expect(err).ToNot(HaveOccurred())
			Expect(held).To(BeFalse())

			j, err := store.Create(context.Background(), id, runstate.MetaRecord{RunID: id})
			Expect(err).ToNot(HaveOccurred())
			Expect(j.Close()).To(Succeed())

			held, err = ch.held(context.Background(), id)
			Expect(err).ToNot(HaveOccurred())
			Expect(held).To(BeTrue())
		})
	})
})
