//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agenttest_test

import (
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/serve"
)

var _ = Describe("ScriptingFault", func() {
	It("Should name the call, its subject and what was missing", func() {
		fault := agenttest.ScriptingFault{
			Call: "ApproveCommand", Subject: "stream rm", Missing: "no ApproveFn was set",
		}

		Expect(fault.Error()).To(Equal(`ApproveCommand "stream rm": no ApproveFn was set`))
	})

	It("Should read the same as the error the call returned", func() {
		q := agenttest.NewQueue(GinkgoTB(), "queue")

		err := q.Submit(nil)
		Expect(err).To(MatchError(agenttest.ErrNotScripted))

		var fault agenttest.ScriptingFault
		Expect(errors.As(err, &fault)).To(BeTrue())
		Expect(fault).To(Equal(q.ScriptingFaults()[0]))
		Expect(err.Error()).To(ContainSubstring(fault.Error()))
	})

	// The Queue records scripting faults and does not implement the interface a server
	// reads an endpoint's own error stream from, so the two names stay apart.
	It("Should not make the Queue a faulting endpoint", func() {
		var c serve.Channel = agenttest.NewQueue(GinkgoTB(), "queue")

		_, faulting := c.(serve.FaultingEndpoint)
		Expect(faulting).To(BeFalse())
	})
})
