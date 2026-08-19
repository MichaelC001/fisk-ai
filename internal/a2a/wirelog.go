//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// wireLog records the messages a client sends and receives, verbatim.
//
// It exists because the conversation between a client and an agent is the one layer
// nothing else records: a model dump shows what the agent asked the model, and a log
// shows what the agent decided, and neither shows the request that arrived, the blocks
// that went back, the question nobody answered or the ending that was not the one a
// person expected.
//
// It records raw bodies rather than decoded messages, and records them before they are
// validated, because a message that fails validation is exactly the one somebody turned
// this on to see. A body is written as it crossed, so what is in the file is what was on
// the wire and not this program's opinion of it.
type wireLog struct {
	mu  sync.Mutex
	out io.Writer
	now func() time.Time

	// names renders the address a message traveled on, when the binding can say. An
	// operator reading this goes on to watch the same traffic with their own tools, and
	// the address is what they need to do it.
	names SubjectNamer
}

// WithWireLog records every message this client sends and every message it reads into
// out, for somebody debugging what an agent and a terminal actually said to each other.
//
// The caller owns out and everything about it: where it goes, when it is closed, and
// whether anybody was warned about what is in it. A reply set carries the conversation
// token, the prompts and the tool output, so a file holding one is as sensitive as the
// conversation itself.
//
// A nil writer records nothing, which is what a client that did not ask for this has.
func WithWireLog(out io.Writer) ClientOption {
	return func(c *Client) {
		if out == nil {
			return
		}

		c.wire = &wireLog{out: out, now: time.Now}

		// Optional: a binding whose addressing is not nameable leaves the address off
		// rather than stopping anything being recorded.
		namer, ok := c.transport.(SubjectNamer)
		if ok {
			c.wire.names = namer
		}
	}
}

// send records a message on its way out.
func (w *wireLog) send(op RouteHint, to, request string, body []byte) {
	w.record(">", w.address(op, to, request), body)
}

// recv records a message on its way in, before anything has decided whether it is
// valid.
//
// An inbound message arrives on a reply inbox the transport made rather than on the
// path the request went out on, so what is named is that path: it is what the exchange
// was about, and the inbox is a name nobody can subscribe to twice.
func (w *wireLog) recv(op RouteHint, from, request string, body []byte) {
	w.record("<", w.address(op, from, request), body)
}

// address is where the exchange happened, or the agent's name alone when the binding
// cannot say.
func (w *wireLog) address(op RouteHint, agent, request string) string {
	if w == nil || w.names == nil {
		return agent
	}

	subject := w.names.Subject(op, agent, request)
	if subject == "" {
		return agent
	}

	return subject
}

// record writes one line: a direction, where it happened, and the body as it crossed.
//
// Messages leave a client from more than one goroutine at once, since a task's reply
// set is read on one while its questions are answered and its acks sent on others, so
// the writer is guarded here rather than relied on to be safe.
func (w *wireLog) record(direction, peer string, body []byte) {
	if w == nil {
		return
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	// A failure to record is not a failure of the run. Somebody debugging notices a
	// short file; somebody working does not want the work to stop for the notes.
	_, _ = fmt.Fprintf(w.out, "%s %s %s %s\n", w.now().UTC().Format(time.RFC3339Nano), direction, peer, body)
}
