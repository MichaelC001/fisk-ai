//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agent

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/toolkit"
)

var _ = Describe("RunStats", func() {
	Describe("CountToolKind", func() {
		It("allocates on first use and accumulates per kind", func() {
			s := &RunStats{}
			Expect(s.ToolCallsByKind).To(BeNil())

			s.CountToolKind(toolkit.KindApplication)
			s.CountToolKind(toolkit.KindApplication)
			s.CountToolKind(toolkit.KindBuiltin)

			Expect(s.ToolCallsByKind).To(Equal(map[toolkit.Kind]int64{
				toolkit.KindApplication: 2,
				toolkit.KindBuiltin:     1,
			}))
		})

		// The buckets count what the model asked for and the two totals count what was
		// dispatched, so counting a call by kind must not move either total: the caller
		// increments those where it dispatches a call.
		It("leaves the remote and MCP totals to the caller", func() {
			s := &RunStats{}
			s.CountToolKind(toolkit.KindMCP)
			s.CountToolKind(toolkit.KindRemote)
			s.CountToolKind(toolkit.KindMCP)

			Expect(s.ToolCallsByKind[toolkit.KindMCP]).To(Equal(int64(2)))
			Expect(s.MCPToolCalls).To(BeZero())
			Expect(s.RemoteToolCalls).To(BeZero())
		})
	})

	Describe("Usage", func() {
		It("Should report nothing for a run that never started", func() {
			var s *RunStats
			Expect(s.Usage()).To(BeNil())
		})

		// The input total is assembled rather than copied: RunStats keeps the uncached
		// remainder in InTokens and the cached input beside it, so a caller handed InTokens
		// alone would be told a fraction of what the task was billed for.
		It("Should report total input, with the cache split kept alongside it", func() {
			usage := (&RunStats{
				InTokens:          10,
				OutTokens:         5,
				CacheReadTokens:   900,
				CacheCreateTokens: 90,
			}).Usage()

			Expect(usage.InputTokens).To(Equal(int64(1000)), "everything the task consumed, cached or not")
			Expect(usage.OutputTokens).To(Equal(int64(5)))
			Expect(usage.CacheReadTokens).To(Equal(int64(900)))
			Expect(usage.CacheCreateTokens).To(Equal(int64(90)))
		})

		It("Should report what the run did as well as what it cost", func() {
			usage := (&RunStats{LlmCalls: 27, ToolCalls: 27}).Usage()

			Expect(usage.LLMCalls).To(Equal(int64(27)))
			Expect(usage.ToolCalls).To(Equal(int64(27)))
		})

		It("Should produce a usage the v1 schema accepts", func() {
			validator, err := a2a.NewValidator()
			Expect(err).ToNot(HaveOccurred())

			res := a2a.NewResult(a2a.StopEndTurn)
			res.Header.ID = a2a.NewID()
			res.Header.Request = res.Header.ID
			res.Header.Conversation = a2a.NewID()
			res.Header.Sequence = 1
			res.Header.Time = time.Now().UTC()
			res.Header.Sender = a2a.Identity{Name: "agent-a"}
			res.Usage = (&RunStats{InTokens: 1, OutTokens: 2, CacheReadTokens: 3, LlmCalls: 4, ToolCalls: 5}).Usage()

			Expect(validator.ValidateMessage(res)).To(Succeed())
		})
	})
})
