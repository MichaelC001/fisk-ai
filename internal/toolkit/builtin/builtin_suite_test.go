//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package builtin

import (
	"context"
	"encoding/json"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/toolkit/functool"
)

func TestBuiltin(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Toolkit/Builtin")
}

// callTool runs a built-in and returns its output string, which is the shape these
// specs assert on; a built-in's outcome carries no exec metadata.
func callTool(t *functool.Tool, ctx context.Context, input json.RawMessage, prompter toolkit.Prompter) (string, error) {
	out, err := t.Execute(ctx, input, toolkit.ExecDeps{Prompter: prompter})
	if err != nil {
		return "", err
	}

	return out.Output, nil
}
