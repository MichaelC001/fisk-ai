//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package builtin_test

import (
	"fmt"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/toolkit/builtin"
)

// ExampleOptInTools builds the harness.tools family from a configuration that lists
// both opt-in tools, with read_file gated behind the operator's approval. read_file
// reads under the process working directory here, since its entry sets no root and
// the configuration has no root_directory.
func ExampleOptInTools() {
	cfg := &config.Config{
		Harness: config.HarnessConfig{
			Tools: []config.HarnessToolConfig{
				{Name: config.ReadFileToolName, Confirm: true},
				{Name: config.Base64EncodeToolName},
			},
		},
	}

	tools, err := builtin.OptInTools(cfg)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	for _, t := range tools {
		fmt.Printf("%s confirm=%t\n", t.Name(), t.NeedsConfirm(nil))
	}

	// Output:
	// read_file confirm=true
	// base64_encode confirm=false
}
