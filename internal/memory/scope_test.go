//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Scope", func() {
	It("Should grant nothing until a revision is remembered", func() {
		scope := NewScope()

		_, ok := scope.Revision("notes")
		Expect(ok).To(BeFalse())

		scope.Remember("notes", 7)

		rev, ok := scope.Revision("notes")
		Expect(ok).To(BeTrue())
		Expect(rev).To(Equal(uint64(7)))
	})

	It("Should drop authority when a revision is forgotten", func() {
		scope := NewScope()
		scope.Remember("notes", 7)
		scope.Forget("notes")

		_, ok := scope.Revision("notes")
		Expect(ok).To(BeFalse())
	})

	It("Should keep each key's revision apart", func() {
		scope := NewScope()
		scope.Remember("notes", 7)
		scope.Remember("plans", 9)

		rev, _ := scope.Revision("plans")
		Expect(rev).To(Equal(uint64(9)))

		scope.Forget("plans")

		rev, ok := scope.Revision("notes")
		Expect(ok).To(BeTrue())
		Expect(rev).To(Equal(uint64(7)))
	})

	// A backend calls these without knowing whether a host supplied a scope, so a nil
	// one has to work rather than panic. It grants nothing, which is the safe answer:
	// an overwrite that cannot prove a read is refused.
	It("Should be usable when nil", func() {
		var scope *Scope

		Expect(func() { scope.Remember("notes", 7) }).ToNot(Panic())
		Expect(func() { scope.Forget("notes") }).ToNot(Panic())

		_, ok := scope.Revision("notes")
		Expect(ok).To(BeFalse())
	})

	It("Should be safe for concurrent use", func() {
		scope := NewScope()

		var wg sync.WaitGroup
		for i := range 8 {
			wg.Go(func() {
				scope.Remember("notes", uint64(i))
				scope.Revision("notes")
				scope.Remember("plans", uint64(i))
				scope.Forget("plans")
			})
		}
		wg.Wait()

		_, ok := scope.Revision("notes")
		Expect(ok).To(BeTrue())
	})
})

var _ = Describe("Scope on a context", func() {
	It("Should return the scope it was given", func() {
		scope := NewScope()
		ctx := WithScope(context.Background(), scope)

		Expect(ScopeFrom(ctx)).To(BeIdenticalTo(scope))
	})

	// A caller that never heard of scopes is not broken by their existence: the backend
	// finds none and falls back to state of its own, which for a store built per run
	// means the same thing.
	It("Should report none on a context that carries none", func() {
		Expect(ScopeFrom(context.Background())).To(BeNil())
	})

	// Two runs sharing one store must not share authority: this is the whole reason the
	// record travels on the context rather than living on the store.
	It("Should keep two runs' authority apart", func() {
		first := WithScope(context.Background(), NewScope())
		second := WithScope(context.Background(), NewScope())

		ScopeFrom(first).Remember("notes", 7)

		_, ok := ScopeFrom(second).Revision("notes")
		Expect(ok).To(BeFalse())
	})
})
