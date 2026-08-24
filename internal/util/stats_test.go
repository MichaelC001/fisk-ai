//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package util

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

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
})
