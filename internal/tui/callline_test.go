//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package tui_test

import (
	"encoding/json"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/tui"
)

var _ = Describe("CallLine", func() {
	line := func(name, input string) string {
		return tui.CallLine(name, json.RawMessage(input))
	}

	// A decoded object has no order of its own, and a line that reordered between
	// renderings of one call would read as two calls.
	It("Should show every argument of a tool nobody listed, in name order", func() {
		Expect(line("cowsay", `{"message":"Why did the cow go to the gym?"}`)).
			To(Equal(`cowsay(message:"Why did the cow go to the gym?")`))
		Expect(line("stream_rm", `{"stream":"ORDERS","force":true}`)).
			To(Equal(`stream_rm(force:true, stream:"ORDERS")`))
	})

	It("Should render a call with no arguments as a call", func() {
		Expect(line("pr_list", `{}`)).To(Equal("pr_list()"))
		Expect(line("pr_list", ``)).To(Equal("pr_list()"))
	})

	// The value a memory write carries is the whole note, which is the case the list
	// exists for.
	It("Should show only what the list names for a built-in", func() {
		Expect(line("memory_write", `{"key":"jokes.history","content":"Joke 1: ...","overwrite":true}`)).
			To(Equal(`memory_write(key:"jokes.history")`))
		Expect(line("memory_read", `{"key":"jokes.history"}`)).
			To(Equal(`memory_read(key:"jokes.history")`))
	})

	// A value long enough to fill a screen is cut here rather than by the terminal, which
	// would fold it over the lines around it.
	It("Should cut a long value", func() {
		input, err := json.Marshal(map[string]string{"message": strings.Repeat("x", 500)})
		Expect(err).NotTo(HaveOccurred())

		out := tui.CallLine("cowsay", input)
		Expect(len(out)).To(BeNumerically("<", 200))
		Expect(out).To(HavePrefix(`cowsay(message:"xxx`))
	})

	// Everything on the line came from a model, so an escape sequence in a name or a value
	// reaches the renderer as text rather than as a cursor movement.
	It("Should sanitize what a model wrote", func() {
		input, err := json.Marshal(map[string]string{"stream": "\x1b[2JORDERS"})
		Expect(err).NotTo(HaveOccurred())

		out := tui.CallLine("stream_rm", input)
		Expect(out).NotTo(ContainSubstring("\x1b"))
		Expect(out).To(ContainSubstring("ORDERS"))
	})

	// Arguments this build cannot read are not worth failing a line over: the call still
	// happened and its name is the useful half.
	It("Should tolerate arguments it cannot read", func() {
		Expect(tui.CallLine("stream_rm", json.RawMessage(`not json`))).To(Equal("stream_rm()"))
		Expect(tui.CallLine("stream_rm", json.RawMessage(`"a string"`))).To(Equal("stream_rm()"))
	})
})
