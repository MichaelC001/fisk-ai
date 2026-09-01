//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package fisktool

import (
	"github.com/choria-io/fisk"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("CommandTool.NeedsConfirm", func() {
	It("Should report the always-on ai:confirm tag regardless of configured tags", func() {
		confirm := &CommandTool{Path: []string{"stream", "rm"}, Model: &fisk.CmdModel{Tags: []string{confirmTag}}}
		plain := &CommandTool{Path: []string{"stream", "info"}, Model: &fisk.CmdModel{}}

		Expect(confirm.NeedsConfirm(nil)).To(BeTrue())
		Expect(plain.NeedsConfirm(nil)).To(BeFalse())
	})

	It("Should report a tool carrying a configured extra confirm tag", func() {
		tool := &CommandTool{Path: []string{"stream", "rm"}, Model: &fisk.CmdModel{Tags: []string{"impact:rw"}}}

		Expect(tool.NeedsConfirm([]string{"impact:rw"})).To(BeTrue())
		Expect(tool.NeedsConfirm([]string{"impact:ro"})).To(BeFalse())
		Expect(tool.NeedsConfirm(nil)).To(BeFalse())
	})
})

var _ = Describe("CommandTool.ConfirmTrigger", func() {
	It("Should prefer the always-on ai:confirm tag when several match", func() {
		tool := &CommandTool{Path: []string{"stream", "rm"}, Model: &fisk.CmdModel{Tags: []string{"impact:rw", confirmTag}}}
		Expect(tool.ConfirmTrigger([]string{"impact:rw"})).To(Equal(confirmTag))
	})

	It("Should name the first matching configured tag in command tag order", func() {
		tool := &CommandTool{Path: []string{"stream", "rm"}, Model: &fisk.CmdModel{Tags: []string{"impact:rw", "admin"}}}
		Expect(tool.ConfirmTrigger([]string{"admin", "impact:rw"})).To(Equal("impact:rw"))
	})

	It("Should be empty for an ungated command", func() {
		tool := &CommandTool{Path: []string{"stream", "info"}, Model: &fisk.CmdModel{Tags: []string{"impact:ro"}}}
		Expect(tool.ConfirmTrigger([]string{"impact:rw"})).To(BeEmpty())
	})
})
