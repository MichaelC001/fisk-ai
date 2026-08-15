//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package toolkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/llm"
)

var _ = Describe("DeferResult", func() {
	It("Should be reachable through both the sentinel and the detail", func() {
		err := DeferResult("waiting on change request", "CHG-1234")

		Expect(errors.Is(err, ErrDeferredResult)).To(BeTrue())

		var d *DeferredResult
		Expect(errors.As(err, &d)).To(BeTrue())
		Expect(d.Note).To(Equal("waiting on change request"))
		Expect(d.Handle).To(Equal("CHG-1234"))
	})

	It("Should survive being wrapped, which is how a handler reports its own context", func() {
		err := fmt.Errorf("filing the request: %w", DeferResult("queued", "REQ-9"))

		d, ok := IsDeferred(err)
		Expect(ok).To(BeTrue())
		Expect(d.Handle).To(Equal("REQ-9"))
	})

	// The note and handle are journaled and later printed, so a tool cannot use them
	// to put a payload where a description belongs.
	It("Should cap the note and the handle", func() {
		err := DeferResult(strings.Repeat("n", maxDeferralNoteRunes+50), strings.Repeat("h", maxDeferralHandleRunes+50))

		var d *DeferredResult
		Expect(errors.As(err, &d)).To(BeTrue())
		Expect([]rune(d.Note)).To(HaveLen(maxDeferralNoteRunes))
		Expect([]rune(d.Handle)).To(HaveLen(maxDeferralHandleRunes))
	})

	It("Should report a bare sentinel as a deferral with no detail", func() {
		d, ok := IsDeferred(ErrDeferredResult)
		Expect(ok).To(BeTrue())
		Expect(d.Note).To(BeEmpty())
	})

	It("Should not report an ordinary error as a deferral", func() {
		_, ok := IsDeferred(errors.New("boom"))
		Expect(ok).To(BeFalse())
	})
})

var _ = Describe("ExecuteUse deferral", func() {
	ctx := context.Background()
	use := llm.ToolUseBlock{ID: "tu_1", Name: "probe", Input: json.RawMessage(`{}`)}

	// A deferral has no result: there is nothing to send the model and nothing to
	// journal, so it must not become an error result the way every other failure does.
	// The caller decides what to do with it, which on the run path is to suspend.
	It("Should return a deferral as an error and no result block", func() {
		tool := outcomeTool{err: DeferResult("waiting on a human", "TKT-1")}

		res, exec, err := ExecuteUse(tool, ctx, use, ExecDeps{})
		Expect(errors.Is(err, ErrDeferredResult)).To(BeTrue())
		Expect(exec).To(BeNil())
		Expect(res).To(Equal(llm.ToolResultBlock{}))
	})

	It("Should still turn every other failure into an error result", func() {
		tool := outcomeTool{err: errors.New("could not run")}

		res, _, err := ExecuteUse(tool, ctx, use, ExecDeps{})
		Expect(err).ToNot(HaveOccurred())
		Expect(res.IsError).To(BeTrue())
		Expect(res.Content).To(Equal("could not run"))
	})
})
