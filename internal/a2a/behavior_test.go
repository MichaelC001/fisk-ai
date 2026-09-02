//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	wire "github.com/choria-io/fisk-ai/internal/a2a/wire/v1"
	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// wire.ToolBehavior replaced toolkit.Behavior on ToolDescriptor so that renaming a tag in
// toolkit is not a protocol change. These pin the bytes it took over, since the swap is
// only safe if it wrote the same document.
var _ = Describe("ToolBehavior", func() {
	It("Should write the same JSON the toolkit type wrote", func() {
		b := toolBehavior(toolkit.Behavior{
			ReadOnly:    toolkit.HintTrue,
			Destructive: toolkit.HintFalse,
			Idempotent:  toolkit.HintTrue,
			OpenWorld:   toolkit.HintFalse,
		})

		data, err := json.Marshal(b)
		Expect(err).ToNot(HaveOccurred())
		Expect(string(data)).To(Equal(`{"read_only":true,"destructive":false,"idempotent":true,"open_world":false}`))
	})

	// An undeclared hint is absent rather than null, which is what tells a receiver the
	// serving agent asserted nothing about that aspect.
	It("Should omit an aspect the serving agent declared nothing about", func() {
		b := toolBehavior(toolkit.Behavior{ReadOnly: toolkit.HintTrue})

		data, err := json.Marshal(b)
		Expect(err).ToNot(HaveOccurred())
		Expect(string(data)).To(Equal(`{"read_only":true}`))
	})

	It("Should omit the property entirely when nothing was declared", func() {
		desc := wire.ToolDescriptor{Name: "ping", Behavior: toolBehavior(toolkit.Behavior{})}

		data, err := json.Marshal(desc)
		Expect(err).ToNot(HaveOccurred())
		Expect(string(data)).To(Equal(`{"name":"ping"}`))
	})

	It("Should carry every declaration back to the toolkit type", func() {
		for _, want := range []toolkit.Behavior{
			{},
			{ReadOnly: toolkit.HintTrue},
			{ReadOnly: toolkit.HintFalse, Destructive: toolkit.HintTrue},
			{ReadOnly: toolkit.HintTrue, Destructive: toolkit.HintFalse, Idempotent: toolkit.HintTrue, OpenWorld: toolkit.HintFalse},
		} {
			Expect(BehaviorOf(toolBehavior(want))).To(Equal(want))
		}
	})

	// A hint outside the three toolkit names travels as no claim, since a receiver
	// reading a value it cannot name learns nothing from it either.
	It("Should carry a hint it cannot name as no claim", func() {
		b := toolBehavior(toolkit.Behavior{ReadOnly: toolkit.Hint(99)})

		Expect(b.ReadOnly).To(BeNil())
		Expect(b.IsZero()).To(BeTrue())
	})
})
