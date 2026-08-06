//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package toolkit

import (
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// taggedTool is a Tool that carries tags and derives its behavior from them,
// standing in for a command tool without importing the fisk package. It borrows the
// rest of the interface from outcomeTool, which the same package already defines.
type taggedTool struct {
	outcomeTool

	tags []string
}

func (t *taggedTool) Tags() []string { return t.tags }

func (t *taggedTool) Behavior() Behavior {
	behavior, _ := BehaviorFromTags(t.tags)

	return behavior
}

var _ = Describe("Behavior", func() {
	Describe("Hint", func() {
		It("Should be unset at the zero value", func() {
			var zero Hint
			Expect(zero).To(Equal(HintUnset))

			value, declared := zero.Bool()
			Expect(value).To(BeFalse())
			Expect(declared).To(BeFalse())
		})

		It("Should report a declared value and whether it was declared", func() {
			value, declared := HintTrue.Bool()
			Expect(value).To(BeTrue())
			Expect(declared).To(BeTrue())

			value, declared = HintFalse.Bool()
			Expect(value).To(BeFalse())
			Expect(declared).To(BeTrue())
		})

		It("Should map a bool to a definite hint", func() {
			Expect(HintOf(true)).To(Equal(HintTrue))
			Expect(HintOf(false)).To(Equal(HintFalse))
		})

		It("Should give every hint a distinct token and fall back for an unknown value", func() {
			Expect(HintUnset.String()).To(Equal("unset"))
			Expect(HintTrue.String()).To(Equal("true"))
			Expect(HintFalse.String()).To(Equal("false"))
			Expect(Hint(99).String()).To(Equal("unset"))
		})

		It("Should marshal as a JSON boolean and unset as null", func() {
			data, err := json.Marshal(HintTrue)
			Expect(err).ToNot(HaveOccurred())
			Expect(string(data)).To(Equal("true"))

			data, err = json.Marshal(HintFalse)
			Expect(err).ToNot(HaveOccurred())
			Expect(string(data)).To(Equal("false"))

			data, err = json.Marshal(HintUnset)
			Expect(err).ToNot(HaveOccurred())
			Expect(string(data)).To(Equal("null"))
		})

		It("Should read a boolean back and treat null as unset", func() {
			var h Hint
			Expect(json.Unmarshal([]byte("true"), &h)).To(Succeed())
			Expect(h).To(Equal(HintTrue))

			Expect(json.Unmarshal([]byte("false"), &h)).To(Succeed())
			Expect(h).To(Equal(HintFalse))

			Expect(json.Unmarshal([]byte("null"), &h)).To(Succeed())
			Expect(h).To(Equal(HintUnset))
		})

		It("Should refuse a hint that is not a boolean rather than silently unsetting it", func() {
			var h Hint
			err := json.Unmarshal([]byte(`"yes"`), &h)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("must be a boolean"))
		})
	})

	Describe("Behavior", func() {
		It("Should declare nothing at the zero value", func() {
			var zero Behavior
			Expect(zero.IsZero()).To(BeTrue())
			Expect(zero.String()).To(BeEmpty())
		})

		It("Should omit an undeclared behavior from a document entirely", func() {
			type carrier struct {
				Behavior Behavior `json:"behavior,omitzero"`
			}

			data, err := json.Marshal(carrier{})
			Expect(err).ToNot(HaveOccurred())
			Expect(string(data)).To(Equal("{}"))
		})

		It("Should carry only the hints that were declared", func() {
			type carrier struct {
				Behavior Behavior `json:"behavior,omitzero"`
			}

			data, err := json.Marshal(carrier{Behavior: Behavior{ReadOnly: HintTrue, Destructive: HintFalse}})
			Expect(err).ToNot(HaveOccurred())
			Expect(string(data)).To(Equal(`{"behavior":{"read_only":true,"destructive":false}}`))
		})

		It("Should round trip through JSON without losing the difference between unset and false", func() {
			in := Behavior{ReadOnly: HintFalse, Idempotent: HintTrue}

			data, err := json.Marshal(in)
			Expect(err).ToNot(HaveOccurred())

			var out Behavior
			Expect(json.Unmarshal(data, &out)).To(Succeed())
			Expect(out).To(Equal(in))
			Expect(out.Destructive).To(Equal(HintUnset))
		})

		It("Should summarize the declared assertions for a person", func() {
			Expect(Behavior{ReadOnly: HintTrue, Idempotent: HintTrue}.String()).To(Equal("read only, idempotent"))
			Expect(Behavior{Destructive: HintTrue}.String()).To(Equal("destructive"))
			Expect(Behavior{Destructive: HintFalse, OpenWorld: HintFalse}.String()).To(Equal("additive, closed world"))
		})

		It("Should resolve a contradiction toward the more dangerous reading", func() {
			resolved := Behavior{ReadOnly: HintTrue, Destructive: HintTrue}.Resolve()
			Expect(resolved.ReadOnly).To(Equal(HintFalse))
			Expect(resolved.Destructive).To(Equal(HintTrue))
		})

		It("Should leave a behavior that does not contradict itself alone", func() {
			in := Behavior{ReadOnly: HintTrue, Idempotent: HintTrue, OpenWorld: HintFalse}
			Expect(in.Resolve()).To(Equal(in))
		})
	})

	Describe("BehaviorFromTags", func() {
		It("Should declare nothing for a tool with no behavior tags", func() {
			behavior, conflicts := BehaviorFromTags([]string{"impact:rw", ConfirmTag})
			Expect(behavior.IsZero()).To(BeTrue())
			Expect(conflicts).To(BeEmpty())
		})

		It("Should set only the aspect a tag names", func() {
			behavior, _ := BehaviorFromTags([]string{ReadOnlyTag})
			Expect(behavior).To(Equal(Behavior{ReadOnly: HintTrue}))
		})

		It("Should read a stated write mode as a statement that the command writes", func() {
			behavior, _ := BehaviorFromTags([]string{DestructiveTag})
			Expect(behavior).To(Equal(Behavior{ReadOnly: HintFalse, Destructive: HintTrue}))

			behavior, _ = BehaviorFromTags([]string{AdditiveTag})
			Expect(behavior).To(Equal(Behavior{ReadOnly: HintFalse, Destructive: HintFalse}))
		})

		It("Should carry idempotence independently", func() {
			behavior, _ := BehaviorFromTags([]string{AdditiveTag, IdempotentTag})
			Expect(behavior).To(Equal(Behavior{ReadOnly: HintFalse, Destructive: HintFalse, Idempotent: HintTrue}))
		})

		It("Should never set the open world hint from a tag", func() {
			behavior, _ := BehaviorFromTags([]string{ReadOnlyTag, DestructiveTag, AdditiveTag, IdempotentTag})
			Expect(behavior.OpenWorld).To(Equal(HintUnset))
		})

		It("Should resolve read-only against destructive toward destructive and report both", func() {
			behavior, conflicts := BehaviorFromTags([]string{ReadOnlyTag, DestructiveTag})
			Expect(behavior.ReadOnly).To(Equal(HintFalse))
			Expect(behavior.Destructive).To(Equal(HintTrue))
			Expect(conflicts).To(ConsistOf(ReadOnlyTag, DestructiveTag))
		})

		It("Should drop a read-only claim from a command that also says it writes additively", func() {
			behavior, conflicts := BehaviorFromTags([]string{ReadOnlyTag, AdditiveTag})
			Expect(behavior.ReadOnly).To(Equal(HintFalse))
			Expect(behavior.Destructive).To(Equal(HintFalse))
			Expect(conflicts).To(ConsistOf(ReadOnlyTag, AdditiveTag))
		})

		It("Should resolve destructive against additive toward destructive and report both", func() {
			behavior, conflicts := BehaviorFromTags([]string{AdditiveTag, DestructiveTag})
			Expect(behavior.Destructive).To(Equal(HintTrue))
			Expect(conflicts).To(ConsistOf(DestructiveTag, AdditiveTag))
		})

		It("Should not depend on the order the author wrote the tags in", func() {
			first, _ := BehaviorFromTags([]string{ReadOnlyTag, DestructiveTag, IdempotentTag})
			second, _ := BehaviorFromTags([]string{IdempotentTag, DestructiveTag, ReadOnlyTag})
			Expect(first).To(Equal(second))
		})
	})

	Describe("UnknownReservedTags", func() {
		It("Should accept every reserved tag", func() {
			Expect(UnknownReservedTags(ReservedTags())).To(BeEmpty())
		})

		It("Should ignore a tag outside the reserved namespace", func() {
			Expect(UnknownReservedTags([]string{"impact:rw", "team:platform"})).To(BeEmpty())
		})

		It("Should report a misspelled reserved tag", func() {
			Expect(UnknownReservedTags([]string{"ai:readonly", ReadOnlyTag})).To(Equal([]string{"ai:readonly"}))
		})

		It("Should report a tag once however often it appears", func() {
			Expect(UnknownReservedTags([]string{"ai:nope", "ai:nope"})).To(Equal([]string{"ai:nope"}))
		})

		It("Should return a copy of the reserved set a caller cannot corrupt", func() {
			tags := ReservedTags()
			tags[0] = "ai:mutated"
			Expect(ReservedTags()).To(ContainElement(DenyTag))
		})
	})

	Describe("BehaviorOf and TagIssues", func() {
		It("Should report nothing for a tool that declares no behavior", func() {
			Expect(BehaviorOf(outcomeTool{})).To(Equal(Behavior{}))
			Expect(TagsOf(outcomeTool{})).To(BeNil())

			unknown, conflicting := TagIssues(outcomeTool{})
			Expect(unknown).To(BeEmpty())
			Expect(conflicting).To(BeEmpty())
		})

		It("Should read the behavior a tool declares", func() {
			tool := &taggedTool{tags: []string{ReadOnlyTag, IdempotentTag}}
			Expect(BehaviorOf(tool)).To(Equal(Behavior{ReadOnly: HintTrue, Idempotent: HintTrue}))
		})

		It("Should report both the unknown and the contradictory tags of one tool", func() {
			tool := &taggedTool{tags: []string{"ai:readonly", ReadOnlyTag, DestructiveTag}}

			unknown, conflicting := TagIssues(tool)
			Expect(unknown).To(Equal([]string{"ai:readonly"}))
			Expect(conflicting).To(ConsistOf(ReadOnlyTag, DestructiveTag))
		})
	})
})
