//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	wire "github.com/choria-io/fisk-ai/internal/a2a/wire/v1"
	"github.com/choria-io/fisk-ai/internal/runstate"
)

var _ = Describe("StopReasonFor", func() {
	It("Should map every terminal reason onto the protocol vocabulary", func() {
		Expect(StopReasonFor(runstate.ReasonCompleted)).To(Equal(wire.StopEndTurn))
		Expect(StopReasonFor(runstate.ReasonBudget)).To(Equal(wire.StopBudgetExhausted))
		Expect(StopReasonFor(runstate.ReasonMaxIterations)).To(Equal(wire.StopMaxIterations))
		Expect(StopReasonFor(runstate.ReasonSuspended)).To(Equal(wire.StopSuspended))
		Expect(StopReasonFor(runstate.ReasonError)).To(Equal(wire.StopError))
		Expect(StopReasonFor("something later")).To(Equal(wire.StopError))
	})
})
