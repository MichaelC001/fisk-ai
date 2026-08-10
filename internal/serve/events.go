//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package serve

import (
	"log/slog"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/remotetools"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/toolkit/fisk"
)

// eventRecorder wraps the events sink a channel supplied so the server sees a run's
// narration whatever the channel does with it.
//
// It exists for two things a channel cannot be relied on to keep. Warnings are logged
// unconditionally, because the advisory explaining why every gated tool was refused
// would otherwise be lost on exactly the channels that have no operator. And a panic
// stack is logged, because it reaches this sink and nowhere else.
//
// The stack is also passed on unchanged. The Events contract requires an
// implementation that forwards to a remote peer to keep the stack local and send only
// the generic message, but that is a rule for the channel that chose to forward, not
// for a local renderer that is entitled to print it.
//
// A nil inner sink discards, which is what a channel wanting no narration supplies.
type eventRecorder struct {
	inner agent.Events
	log   *slog.Logger
}

func newEventRecorder(inner agent.Events, log *slog.Logger) *eventRecorder {
	return &eventRecorder{inner: inner, log: log}
}

func (e *eventRecorder) Warn(w agent.Warning) {
	e.log.Warn("Run advisory", "kind", w.Kind, "name", w.Name, "count", w.Count, "error", w.Err)

	if e.inner != nil {
		e.inner.Warn(w)
	}
}

func (e *eventRecorder) Panicked(value any, stack []byte) {
	e.log.Error("Run crashed", "panic", value, "stack", string(stack))

	if e.inner != nil {
		e.inner.Panicked(value, stack)
	}
}

func (e *eventRecorder) Starting(info agent.RunInfo) {
	if e.inner != nil {
		e.inner.Starting(info)
	}
}

func (e *eventRecorder) RemoteHostNotes(notes []remotetools.HostImport) {
	if e.inner != nil {
		e.inner.RemoteHostNotes(notes)
	}
}

func (e *eventRecorder) ResumeTranscript(rs *runstate.RunState, tools map[string]*fisk.FiskCommandTool) {
	if e.inner != nil {
		e.inner.ResumeTranscript(rs, tools)
	}
}

func (e *eventRecorder) LLMRequest(summary string) {
	if e.inner != nil {
		e.inner.LLMRequest(summary)
	}
}

func (e *eventRecorder) ToolCall(t agent.ToolTrace) {
	if e.inner != nil {
		e.inner.ToolCall(t)
	}
}

func (e *eventRecorder) ToolResult(t agent.ToolResultTrace) {
	if e.inner != nil {
		e.inner.ToolResult(t)
	}
}

func (e *eventRecorder) Message(resp llm.Response, terminal bool) {
	if e.inner != nil {
		e.inner.Message(resp, terminal)
	}
}

func (e *eventRecorder) SessionRotated(prevID string) {
	if e.inner != nil {
		e.inner.SessionRotated(prevID)
	}
}
