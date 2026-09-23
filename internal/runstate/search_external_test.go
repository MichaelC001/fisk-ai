//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package runstate_test

import (
	"regexp"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/runstate"
)

var _ = Describe("SearchFilter", func() {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

	infos := []runstate.RunInfo{
		{RunID: "a", Agent: "ops", Caller: "peer1", Model: "claude-opus-4-8", Prompt: "list the streams", Terminal: runstate.ReasonCompleted, Created: now.Add(-2 * time.Hour)},
		{RunID: "b", Agent: "ops", Caller: "peer2", Model: "claude-sonnet-4-6", Prompt: "Remove the ORDERS stream", Created: now.Add(-3 * 24 * time.Hour)},
		{RunID: "c", Agent: "billing", Caller: "peer1", Model: "claude-opus-4-8", Prompt: "raise an invoice", Terminal: runstate.ReasonBudget, Created: now.Add(-10 * 24 * time.Hour)},
		{RunID: "d", Model: "claude-opus-4-8", Prompt: "list the consumers", Terminal: runstate.ReasonCompleted, Created: now.Add(-30 * time.Minute)},
	}

	selected := func(f runstate.SearchFilter) ([]string, int) {
		found, noAgent := f.Select(infos)

		var ids []string
		for _, info := range found {
			ids = append(ids, info.RunID)
		}

		return ids, noAgent
	}

	It("Should select every run when zero", func() {
		ids, noAgent := selected(runstate.SearchFilter{})
		Expect(ids).To(Equal([]string{"a", "b", "c", "d"}))
		Expect(noAgent).To(BeZero())
	})

	It("Should exclude a run carrying no agent from an agent filter and count it", func() {
		ids, noAgent := selected(runstate.SearchFilter{Agent: "ops"})
		Expect(ids).To(Equal([]string{"a", "b"}))
		Expect(noAgent).To(Equal(1))

		Expect(runstate.SearchFilter{Agent: "ops"}.Matches(infos[3])).To(BeFalse())
		Expect(runstate.ListFilter{Agent: "ops"}.MatchesAgent(infos[3].Agent)).To(BeTrue(), "the list filter keeps including it")
	})

	It("Should count only the runs without an agent that every other filter took", func() {
		ids, noAgent := selected(runstate.SearchFilter{Agent: "ops", Status: runstate.StatusOpen})
		Expect(ids).To(Equal([]string{"b"}))
		Expect(noAgent).To(BeZero())
	})

	It("Should select by model and by caller", func() {
		ids, _ := selected(runstate.SearchFilter{Model: "claude-opus-4-8"})
		Expect(ids).To(Equal([]string{"a", "c", "d"}))

		ids, _ = selected(runstate.SearchFilter{Caller: "peer1"})
		Expect(ids).To(Equal([]string{"a", "c"}))
	})

	It("Should select by status, open or the reason the run ended", func() {
		ids, _ := selected(runstate.SearchFilter{Status: runstate.StatusOpen})
		Expect(ids).To(Equal([]string{"b"}))

		ids, _ = selected(runstate.SearchFilter{Status: string(runstate.ReasonBudget)})
		Expect(ids).To(Equal([]string{"c"}))
	})

	It("Should take Since inclusively and Until exclusively against the created time", func() {
		ids, _ := selected(runstate.SearchFilter{Since: now.Add(-2 * time.Hour)})
		Expect(ids).To(Equal([]string{"a", "d"}))

		ids, _ = selected(runstate.SearchFilter{Until: now.Add(-2 * time.Hour)})
		Expect(ids).To(Equal([]string{"b", "c"}))

		ids, _ = selected(runstate.SearchFilter{Since: now.Add(-4 * 24 * time.Hour), Until: now.Add(-time.Hour)})
		Expect(ids).To(Equal([]string{"a", "b"}))
	})

	It("Should match the prompt", func() {
		ids, _ := selected(runstate.SearchFilter{Prompt: regexp.MustCompile("(?i)orders")})
		Expect(ids).To(Equal([]string{"b"}))

		ids, _ = selected(runstate.SearchFilter{Prompt: regexp.MustCompile("^list the")})
		Expect(ids).To(Equal([]string{"a", "d"}))
	})

	It("Should combine the filters as AND", func() {
		f := runstate.SearchFilter{
			Model:  "claude-opus-4-8",
			Status: string(runstate.ReasonCompleted),
			Prompt: regexp.MustCompile("streams"),
			Since:  now.Add(-24 * time.Hour),
		}

		ids, _ := selected(f)
		Expect(ids).To(Equal([]string{"a"}))
		Expect(f.Matches(infos[0])).To(BeTrue())
		Expect(f.Matches(infos[3])).To(BeFalse())
	})
})
