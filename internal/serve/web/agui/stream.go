//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agui

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/events"
	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/encoding/sse"
)

// streamHeaders are what an AG-UI event stream is served with. x-accel-buffering is
// there for the proxy that would otherwise hold every event until the response ended.
var streamHeaders = map[string]string{
	"content-type":      "text/event-stream",
	"cache-control":     "no-cache",
	"connection":        "keep-alive",
	"x-accel-buffering": "no",
}

// stream writes AG-UI events as server-sent events.
//
// Every event is written by the goroutine running one turn, in the order the run
// produced them, so nothing here takes a lock.
//
// The first write failure is recorded and every write after it is skipped. A browser
// going away mid-turn is the ordinary case rather than a fault, and the turn is worth
// running out either way: the journal holds the conversation, not this response.
type stream struct {
	w      http.ResponseWriter
	writer *sse.SSEWriter
	err    error
}

// newStream holds the response the events are written to. Nothing is written until open
// is called.
//
// The SDK's writer logs a failed write, and a format reaches none of the channel's
// loggers, so it is given one that discards: the first failure is recorded here and the
// channel reports the turn's own outcome.
func newStream(w http.ResponseWriter) *stream {
	return &stream{
		w:      w,
		writer: sse.NewSSEWriter().WithLogger(slog.New(slog.DiscardHandler)),
	}
}

// open sends the response head. Nothing may set a header after this.
func (s *stream) open() {
	for name, value := range streamHeaders {
		s.w.Header().Set(name, value)
	}

	s.w.WriteHeader(http.StatusOK)

	flusher, ok := s.w.(http.Flusher)
	if ok {
		flusher.Flush()
	}
}

// event sends one AG-UI event as its own server-sent event, and flushes: without a
// flush per event the response is buffered until the handler returns, and a turn that
// took a minute arrives all at once at the end of it.
//
// The timestamp the SDK stamps when it builds an event is cleared first. It says when
// this process made the value, which on a replayed conversation is now rather than when
// the conversation happened, and the field is optional in the protocol.
func (s *stream) event(e events.Event) {
	if s.err != nil {
		return
	}

	e.GetBaseEvent().TimestampMs = nil

	body, err := e.ToJSON()
	if err != nil {
		s.err = err

		return
	}

	s.err = s.writer.WriteBytes(context.Background(), s.w, body)
}
