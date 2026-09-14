//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package builtin

import (
	"encoding/json"
	"fmt"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/toolkit/functool"
)

// optInConstructor builds one opt-in tool's spec from the configuration and its
// harness.tools entry's raw options block.
type optInConstructor func(*config.Config, json.RawMessage) (functool.Spec, error)

// optInConstructors maps each name harness.tools accepts to its constructor. A spec
// pins that its keys and config.HarnessToolNames name the same set, since config
// refuses a name outside its list and this table would refuse one outside its own.
var optInConstructors = map[string]optInConstructor{
	readFileName:     readFileSpec,
	base64EncodeName: base64EncodeSpec,
}

// OptInTools returns the built-in tools harness.tools lists, in the listed order, or
// nil when it lists none. An entry with confirm set is gated behind the operator's
// approval, with the tool's trace as the approval summary, so the prompt and the trace
// show the same line; the entry never widens the tool's Expose declaration. An entry
// whose options its constructor refuses is an error carrying the entry's index and
// name, since the operator reads it against the file.
func OptInTools(cfg *config.Config) ([]*functool.Tool, error) {
	entries := cfg.HarnessTools()
	if len(entries) == 0 {
		return nil, nil
	}

	tools := make([]*functool.Tool, 0, len(entries))
	for i, entry := range entries {
		construct, ok := optInConstructors[entry.Name]
		if !ok {
			return nil, fmt.Errorf("harness.tools[%d] (%s): no built-in tool has that name", i, entry.Name)
		}

		spec, err := construct(cfg, entry.Options)
		if err != nil {
			return nil, fmt.Errorf("harness.tools[%d] (%s): %w", i, entry.Name, err)
		}

		if entry.Confirm {
			spec.Confirm = &functool.ConfirmSpec{Summary: spec.Trace}
		}

		tools = append(tools, mustNew(spec))
	}

	return tools, nil
}
