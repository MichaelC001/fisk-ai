//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/util"
)

var _ = Describe("UsageFrom", func() {
	It("Should report nothing for a run that never started", func() {
		Expect(UsageFrom(nil)).To(BeNil())
	})

	// The input total is assembled rather than copied: RunStats keeps the uncached
	// remainder in InTokens and the cached input beside it, so a caller handed InTokens
	// alone would be told a fraction of what the task was billed for.
	It("Should report total input, with the cache split kept alongside it", func() {
		usage := UsageFrom(&util.RunStats{
			InTokens:          10,
			OutTokens:         5,
			CacheReadTokens:   900,
			CacheCreateTokens: 90,
		})

		Expect(usage.InputTokens).To(Equal(int64(1000)), "everything the task consumed, cached or not")
		Expect(usage.OutputTokens).To(Equal(int64(5)))
		Expect(usage.CacheReadTokens).To(Equal(int64(900)))
		Expect(usage.CacheCreateTokens).To(Equal(int64(90)))
	})

	It("Should report what the run did as well as what it cost", func() {
		usage := UsageFrom(&util.RunStats{LlmCalls: 27, ToolCalls: 27})

		Expect(usage.LLMCalls).To(Equal(int64(27)))
		Expect(usage.ToolCalls).To(Equal(int64(27)))
	})

	It("Should produce a usage the v1 schema accepts", func() {
		validator, err := NewValidator()
		Expect(err).ToNot(HaveOccurred())

		res := NewResult(StopEndTurn)
		fillHeader(&res.Header)
		res.Usage = UsageFrom(&util.RunStats{InTokens: 1, OutTokens: 2, CacheReadTokens: 3, LlmCalls: 4, ToolCalls: 5})

		Expect(validator.ValidateMessage(res)).To(Succeed())
	})
})

var _ = Describe("StopReasonFor", func() {
	It("Should map every terminal reason onto the protocol vocabulary", func() {
		Expect(StopReasonFor(runstate.ReasonCompleted)).To(Equal(StopEndTurn))
		Expect(StopReasonFor(runstate.ReasonBudget)).To(Equal(StopBudgetExhausted))
		Expect(StopReasonFor(runstate.ReasonMaxIterations)).To(Equal(StopMaxIterations))
		Expect(StopReasonFor(runstate.ReasonSuspended)).To(Equal(StopSuspended))
		Expect(StopReasonFor(runstate.ReasonError)).To(Equal(StopError))
		Expect(StopReasonFor("something later")).To(Equal(StopError))
	})
})
