//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package toolkit

import "context"

// toolUseIDKey carries the call a tool is running under, so a prompter reached from
// inside that tool can say which call its question belongs to.
type toolUseIDKey struct{}

// ContextWithToolUseID marks ctx as running the named call. ExecuteUse sets it around
// every tool it runs, so nothing else has to thread the id through a Tool's own
// signature.
func ContextWithToolUseID(ctx context.Context, toolUseID string) context.Context {
	if toolUseID == "" {
		return ctx
	}

	return context.WithValue(ctx, toolUseIDKey{}, toolUseID)
}

// ToolUseIDFromContext reports the call a tool is running under, and "" outside one.
//
// A prompter that carries a question to somebody who may answer it after the run has
// ended needs it. The question is asked again on the next resume under a new question
// id, so the call is the only thing the asking end and the answering end can agree the
// answer belongs to. A prompter that answers in the moment has no use for it.
func ToolUseIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(toolUseIDKey{}).(string)

	return id
}
