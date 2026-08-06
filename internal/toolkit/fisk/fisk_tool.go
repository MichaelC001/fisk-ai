//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package fisk

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// A FiskCommandTool is a model-facing Tool that describes its own presentation and
// behavior, can require operator confirmation, and can pre-validate a call's required
// arguments.
var (
	_ toolkit.Tool              = (*FiskCommandTool)(nil)
	_ toolkit.Describer         = (*FiskCommandTool)(nil)
	_ toolkit.BehaviorDescriber = (*FiskCommandTool)(nil)
	_ toolkit.Confirmable       = (*FiskCommandTool)(nil)
	_ toolkit.ArgumentValidator = (*FiskCommandTool)(nil)
)

// Behavior is what running the command does to the world, as its author declared it
// through the reserved behavior tags. A command that carries none declares nothing,
// and every consumer applies its own conservative default. Contradictory tags are
// resolved conservatively rather than rejected, so one mistagged command in a wrapped
// binary cannot stop a run; toolkit.TagIssues reports the contradiction to a caller
// that can warn about it.
func (t *FiskCommandTool) Behavior() toolkit.Behavior {
	behavior, _ := toolkit.BehaviorFromTags(t.Tags())

	return behavior
}

// Describe presents the tool as an application command: its call is traced with the
// resolved command line and a short form with long argument values middle-elided, so
// a width-aware surface can fall back to the short line only when the full one would
// overflow. Both are already sanitized by TraceLine and TraceLineShort. It runs in
// the caller's per-run working directory so concurrent runs do not collide, and it
// never prompts.
func (t *FiskCommandTool) Describe(input json.RawMessage) toolkit.CallInfo {
	return toolkit.CallInfo{
		Present:      toolkit.PresentCommand,
		Kind:         toolkit.KindApplication,
		Display:      t.TraceLine(input),
		DisplayShort: t.TraceLineShort(input),
		NeedsWorkDir: true,
	}
}

// Definition renders the command tool as a neutral tool definition. A tool tagged
// ai:no_defer is always sent directly, even within a deferred set, so its deferral
// is suppressed here rather than in the caller.
func (t *FiskCommandTool) Definition(deferLoading bool) llm.ToolDef {
	return llm.ToolDef{
		Name:         t.Name(),
		Description:  t.ModelDescription(),
		InputSchema:  t.InputSchema(),
		DeferLoading: deferLoading && !slices.Contains(t.Tags(), noDeferTag),
	}
}

// Execute runs the command behind the tool and returns its outcome. A command that
// could not be run (a missing binary, a canceled context, unusable arguments) is an
// error; a command that ran, including one that exited non-zero, is a normal
// outcome carrying the exit code and output. It uses only the WorkDir from ExecDeps
// (a command tool never prompts), running the command in the caller's per-run
// directory so concurrent runs do not collide.
func (t *FiskCommandTool) Execute(ctx context.Context, input json.RawMessage, deps toolkit.ExecDeps) (*toolkit.Outcome, error) {
	result, err := t.RunCommand(ctx, input, deps.WorkDir)
	if err != nil {
		return nil, err
	}

	return &toolkit.Outcome{
		Output: result.Output,
		Exec: &toolkit.CommandExec{
			Command:   result.Command,
			ExitCode:  result.ExitCode,
			Truncated: result.Truncated,
		},
	}, nil
}

// MCPExposable reports that a wrapped application's command may be served over
// MCP: serving them is what the surface exists for. Which commands an operator
// actually serves is narrowed by the exposure selection and the ai:deny tag, not
// here.
func (t *FiskCommandTool) MCPExposable() bool { return true }

// A2AExposable reports that a wrapped application's command may be served over
// a2a, on the same terms as MCPExposable.
func (t *FiskCommandTool) A2AExposable() bool { return true }
