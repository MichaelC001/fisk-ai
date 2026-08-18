//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package serve

import (
	"log/slog"
	"strings"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/remotetools"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/toolkit/fisk"
)

// maxToolErrorLine bounds a failed tool's output on the log line that reports it. The
// whole output is in the journal; this is only enough to recognize the failure.
const maxToolErrorLine = 200

// eventRecorder wraps the events sink a channel supplied so the server sees a run's
// narration whatever the channel does with it.
//
// It exists for what a channel cannot be relied on to keep. Warnings are logged
// unconditionally, because the advisory explaining why every gated tool was refused
// would otherwise be lost on exactly the channels that have no operator. A panic stack
// is logged, because it reaches this sink and nowhere else. And tool calls are logged,
// because a hosted run is unattended: without them the only record of what a job did to
// the world is a journal nobody opens until afterwards.
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

// The two optional halves of agent.Events are implemented here unconditionally and
// forwarded only to a channel's sink that wants them, so the recorder is transparent:
// a channel that renders for a person keeps hearing them, and one that does not is not
// made to implement them by sitting behind this.
func (e *eventRecorder) RemoteHostNotes(notes []remotetools.HostImport) {
	reporter, ok := e.inner.(agent.RemoteHostReporter)
	if ok {
		reporter.RemoteHostNotes(notes)
	}
}

func (e *eventRecorder) ResumeTranscript(rs *runstate.RunState, tools map[string]*fisk.FiskCommandTool) {
	replayer, ok := e.inner.(agent.TranscriptReplayer)
	if ok {
		replayer.ResumeTranscript(rs, tools)
	}
}

func (e *eventRecorder) LLMRequest(summary string) {
	if e.inner != nil {
		e.inner.LLMRequest(summary)
	}
}

// ToolCall logs what the run is about to do to the world.
//
// A hosted run has nobody watching it, so without this the only record of what a job
// did is its journal, which nobody opens until something has already gone wrong. It
// logs the short display rather than the full one: an argument list can be long and can
// carry whatever a caller put in a prompt, and the point here is what was invoked and
// how often, not a transcript.
func (e *eventRecorder) ToolCall(t agent.ToolTrace) {
	e.log.Info("Tool call", "tool", t.Name, "kind", t.ProviderKind, "agent", t.Agent, "call", t.DisplayShort)

	if e.inner != nil {
		e.inner.ToolCall(t)
	}
}

// ToolResult logs whether the call worked, without its output. The output is the
// unbounded part and belongs in the journal; what an operator reading a log needs is
// which calls failed.
func (e *eventRecorder) ToolResult(t agent.ToolResultTrace) {
	if t.IsError {
		e.log.Warn("Tool call failed", "kind", t.ProviderKind, "output", firstLine(t.Output))
	} else {
		e.log.Debug("Tool call completed", "kind", t.ProviderKind)
	}

	if e.inner != nil {
		e.inner.ToolResult(t)
	}
}

// firstLine keeps a failure readable on one log line. A tool's error output can run to
// pages, and the whole of it is in the journal already.
func firstLine(s string) string {
	s = strings.TrimSpace(s)

	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		s = s[:idx]
	}

	if len(s) > maxToolErrorLine {
		return s[:maxToolErrorLine] + "..."
	}

	return s
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
