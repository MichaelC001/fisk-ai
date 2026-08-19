//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package tui

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/choria-io/fisk-ai/internal/util"
)

// maxCallValue bounds one rendered argument value. A call line is one line: an
// argument that would fill a screen is cut here rather than by the terminal, which
// would fold it over the lines around it.
const maxCallValue = 96

// callArgs names the arguments worth showing for the tools this program ships. They
// are the ones whose call line is dominated by a value nobody reads: a memory write
// carries the whole note, a knowledge document carries the document.
//
// The list is maintained here, in the client that renders, rather than described on a
// wire that every client would then have to speak. It is coupled to the built-ins on
// purpose: they ship from this repository, so a tool added here is a line changed here.
// A front end that is not this one makes its own choice, and one with more room than a
// terminal may well choose differently.
var callArgs = map[string][]string{
	"memory_read":      {"key"},
	"memory_write":     {"key"},
	"memory_delete":    {"key"},
	"knowledge_search": {"query"},
}

// CallLine renders a tool call the way a person reads one: the tool and the arguments
// it was called with.
//
//	memory_write(key:"jokes.history")
//	cowsay(message:"Why did the cow go to the gym? To build some moo-scle!")
//
// It renders from what a call is, so a run watched live, a run streamed from a worker
// and a run read back from a journal all produce the same line. Nothing about a tool
// beyond its name reaches it, which is what lets a journal render as well as a live
// run does.
//
// Arguments are sorted by name, since a decoded object has no order of its own and a
// line that changed between renderings of one call would read as two calls. A value
// too long for a line is cut. Everything is sanitized: a name and a value both come
// from a model.
func CallLine(name string, input json.RawMessage) string {
	rendered := callArguments(name, input)
	if len(rendered) == 0 {
		return util.SanitizeForTerminal(name, maxCallValue) + "()"
	}

	return fmt.Sprintf("%s(%s)", util.SanitizeForTerminal(name, maxCallValue), strings.Join(rendered, ", "))
}

// callArguments renders the arguments of one call, in name order, keeping the ones
// this tool is worth showing.
func callArguments(name string, input json.RawMessage) []string {
	var args map[string]json.RawMessage

	err := json.Unmarshal(input, &args)
	if err != nil || len(args) == 0 {
		return nil
	}

	keep, listed := callArgs[name]

	names := make([]string, 0, len(args))
	for arg := range args {
		if listed && !slices.Contains(keep, arg) {
			continue
		}
		names = append(names, arg)
	}
	slices.Sort(names)

	out := make([]string, 0, len(names))
	for _, arg := range names {
		out = append(out, fmt.Sprintf("%s:%s", util.SanitizeForTerminal(arg, maxCallValue), callValue(args[arg])))
	}

	return out
}

// callValue renders one argument. A JSON string is shown as its text in quotes rather
// than as escaped JSON, since a person reading a call wants the value the tool was
// given; anything else is shown as it was written.
func callValue(raw json.RawMessage) string {
	var text string

	err := json.Unmarshal(raw, &text)
	if err != nil {
		return util.SanitizeForTerminal(string(raw), maxCallValue)
	}

	return `"` + util.SanitizeForTerminal(text, maxCallValue) + `"`
}
