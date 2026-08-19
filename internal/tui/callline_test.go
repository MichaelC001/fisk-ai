//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package tui_test

import (
	"encoding/json"
	"strings"
	"testing"

	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/tui"
)

func TestCallLine(t *testing.T) {
	g := NewWithT(t)

	line := func(name, input string) string {
		return tui.CallLine(name, json.RawMessage(input))
	}

	// A tool nobody listed shows everything it was called with, in name order, since a
	// decoded object has none of its own and a line that reordered between renderings
	// of one call would read as two calls.
	g.Expect(line("cowsay", `{"message":"Why did the cow go to the gym?"}`)).
		To(Equal(`cowsay(message:"Why did the cow go to the gym?")`))
	g.Expect(line("stream_rm", `{"stream":"ORDERS","force":true}`)).
		To(Equal(`stream_rm(force:true, stream:"ORDERS")`))

	// A call with no arguments is still a call.
	g.Expect(line("pr_list", `{}`)).To(Equal("pr_list()"))
	g.Expect(line("pr_list", ``)).To(Equal("pr_list()"))

	// A built-in shows what the list says and nothing else: the value a memory write
	// carries is the whole note, which is the case the list exists for.
	g.Expect(line("memory_write", `{"key":"jokes.history","content":"Joke 1: ...","overwrite":true}`)).
		To(Equal(`memory_write(key:"jokes.history")`))
	g.Expect(line("memory_read", `{"key":"jokes.history"}`)).
		To(Equal(`memory_read(key:"jokes.history")`))
}

// A value long enough to fill a screen is cut here rather than by the terminal, which
// would fold it over the lines around it.
func TestCallLine_CutsALongValue(t *testing.T) {
	g := NewWithT(t)

	input, err := json.Marshal(map[string]string{"message": strings.Repeat("x", 500)})
	g.Expect(err).NotTo(HaveOccurred())

	line := tui.CallLine("cowsay", input)
	g.Expect(len(line)).To(BeNumerically("<", 200))
	g.Expect(line).To(HavePrefix(`cowsay(message:"xxx`))
}

// Everything on the line came from a model, so an escape sequence in a name or a value
// reaches the renderer as text rather than as a cursor movement.
func TestCallLine_SanitizesWhatAModelWrote(t *testing.T) {
	g := NewWithT(t)

	input, err := json.Marshal(map[string]string{"stream": "\x1b[2JORDERS"})
	g.Expect(err).NotTo(HaveOccurred())

	line := tui.CallLine("stream_rm", input)
	g.Expect(line).NotTo(ContainSubstring("\x1b"))
	g.Expect(line).To(ContainSubstring("ORDERS"))
}

// Arguments this build cannot read are not worth failing a line over: the call still
// happened and its name is the useful half.
func TestCallLine_ToleratesArgumentsItCannotRead(t *testing.T) {
	g := NewWithT(t)

	g.Expect(tui.CallLine("stream_rm", json.RawMessage(`not json`))).To(Equal("stream_rm()"))
	g.Expect(tui.CallLine("stream_rm", json.RawMessage(`"a string"`))).To(Equal("stream_rm()"))
}
