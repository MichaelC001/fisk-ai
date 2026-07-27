//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package functool

import (
	"context"
	"encoding/json"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/toolkit"
)

func TestFunctool(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Internal/Toolkit/Functool")
}

// callTool runs a tool and returns its output string, which is the shape these
// specs assert on; Execute's outcome carries no exec metadata for a function tool.
func callTool(t *Tool, ctx context.Context, input json.RawMessage, prompter toolkit.Prompter) (string, error) {
	out, err := t.Execute(ctx, input, toolkit.ExecDeps{Prompter: prompter})
	if err != nil {
		return "", err
	}

	return out.Output, nil
}
