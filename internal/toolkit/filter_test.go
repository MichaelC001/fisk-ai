//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package toolkit

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
)

// namedTool is a Tool with a name and no tags, standing in for a built-in, an
// import or a custom tool, none of which implements Tagged.
type namedTool struct {
	outcomeTool

	name string
}

func (t *namedTool) Name() string { return t.name }

// taggedNamedTool is a namedTool that carries tags, standing in for a command.
type taggedNamedTool struct {
	namedTool

	tags []string
}

func (t *taggedNamedTool) Tags() []string { return t.tags }

var _ = Describe("FilterTools", func() {
	// fixture is two commands, one tagged and one not, and two tools of kinds that
	// carry no tags.
	fixture := func() []Tool {
		return []Tool{
			&taggedNamedTool{namedTool: namedTool{name: "auth_user_add"}, tags: []string{"admin"}},
			&taggedNamedTool{namedTool: namedTool{name: "auth_login"}},
			&namedTool{name: "knowledge_search"},
			&namedTool{name: "docs_search"},
		}
	}

	filterNames := func(tools []Tool) []string {
		out := make([]string, 0, len(tools))
		for _, t := range tools {
			out = append(out, t.Name())
		}

		return out
	}

	It("Should return the tools unchanged for a nil filter and for one with neither patterns nor tags", func() {
		tools, err := FilterTools(fixture(), nil, IncludeFilter)
		Expect(err).NotTo(HaveOccurred())
		Expect(filterNames(tools)).To(Equal([]string{"auth_user_add", "auth_login", "knowledge_search", "docs_search"}))

		tools, err = FilterTools(fixture(), &config.ToolFilter{}, IncludeFilter)
		Expect(err).NotTo(HaveOccurred())
		Expect(filterNames(tools)).To(Equal([]string{"auth_user_add", "auth_login", "knowledge_search", "docs_search"}))
	})

	It("Should include only the tools whose name matches a pattern, whatever their kind", func() {
		tools, err := FilterTools(fixture(), &config.ToolFilter{Tools: []string{"^auth", "^docs_"}}, IncludeFilter)
		Expect(err).NotTo(HaveOccurred())
		Expect(filterNames(tools)).To(Equal([]string{"auth_user_add", "auth_login", "docs_search"}))
	})

	It("Should exclude the tools whose name matches a pattern, whatever their kind", func() {
		tools, err := FilterTools(fixture(), &config.ToolFilter{Tools: []string{"^auth", "knowledge_search"}}, ExcludeFilter)
		Expect(err).NotTo(HaveOccurred())
		Expect(filterNames(tools)).To(Equal([]string{"docs_search"}))
	})

	It("Should include by tag and so remove every tool without tags", func() {
		tools, err := FilterTools(fixture(), &config.ToolFilter{Tags: []string{"admin"}}, IncludeFilter)
		Expect(err).NotTo(HaveOccurred())
		Expect(filterNames(tools)).To(Equal([]string{"auth_user_add"}))
	})

	It("Should exclude by tag and leave every tool without tags", func() {
		tools, err := FilterTools(fixture(), &config.ToolFilter{Tags: []string{"admin"}}, ExcludeFilter)
		Expect(err).NotTo(HaveOccurred())
		Expect(filterNames(tools)).To(Equal([]string{"auth_login", "knowledge_search", "docs_search"}))
	})

	It("Should match the empty tag to a tool with no tags, Tagged or not", func() {
		tools, err := FilterTools(fixture(), &config.ToolFilter{Tags: []string{""}}, IncludeFilter)
		Expect(err).NotTo(HaveOccurred())
		Expect(filterNames(tools)).To(Equal([]string{"auth_login", "knowledge_search", "docs_search"}))
	})

	It("Should keep a tool that matches either a pattern or a tag", func() {
		tools, err := FilterTools(fixture(), &config.ToolFilter{Tools: []string{"^knowledge_"}, Tags: []string{"admin"}}, IncludeFilter)
		Expect(err).NotTo(HaveOccurred())
		Expect(filterNames(tools)).To(Equal([]string{"auth_user_add", "knowledge_search"}))
	})

	It("Should return an error for an invalid pattern", func() {
		tools, err := FilterTools(fixture(), &config.ToolFilter{Tools: []string{"("}}, IncludeFilter)
		Expect(err).To(MatchError(ContainSubstring(`invalid tool filter pattern "("`)))
		Expect(tools).To(BeNil())
	})

	Describe("Filter", func() {
		It("Should keep every tool when nil", func() {
			var f *Filter
			for _, t := range fixture() {
				Expect(f.Keeps(t)).To(BeTrue())
			}
		})

		It("Should compile to nil for a filter with nothing to match", func() {
			f, err := NewFilter(&config.ToolFilter{}, IncludeFilter)
			Expect(err).NotTo(HaveOccurred())
			Expect(f).To(BeNil())
		})

		It("Should answer per tool for each mode", func() {
			inc, err := NewFilter(&config.ToolFilter{Tools: []string{"^auth"}}, IncludeFilter)
			Expect(err).NotTo(HaveOccurred())
			exc, err := NewFilter(&config.ToolFilter{Tools: []string{"^auth"}}, ExcludeFilter)
			Expect(err).NotTo(HaveOccurred())

			auth := &namedTool{name: "auth_login"}
			docs := &namedTool{name: "docs_search"}
			Expect(inc.Keeps(auth)).To(BeTrue())
			Expect(inc.Keeps(docs)).To(BeFalse())
			Expect(exc.Keeps(auth)).To(BeFalse())
			Expect(exc.Keeps(docs)).To(BeTrue())
		})
	})
})
