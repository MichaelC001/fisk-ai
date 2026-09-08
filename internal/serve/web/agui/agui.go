//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// Package agui renders a turn as AG-UI events, the protocol assistant-ui and
// CopilotKit read. It is a web.Format, mounted on MountPath under the channel's base
// path.
//
// # The response
//
// One server-sent event per AG-UI event, framed by the protocol SDK's own writer, with
// a flush after each one. Every event is written without the timestamp the SDK stamps
// on it: that timestamp is when this process built the value, which for a replayed
// conversation is now rather than when the conversation happened.
//
// # The request
//
// Decode reads the AG-UI run input: the thread id, the run id, the resume entries and
// the newest user message. Everything else comes from the journal. Both web protocols
// invite a server to rebuild the conversation from the messages the client sent, and
// doing that here would let a page rewrite history the worker is authoritative for.
//
// # Questions
//
// A question ends the run as an AG-UI interrupt, carried on the run-finished event, and
// the client starts a new run whose resume array answers it. Each interrupt names the
// call it belongs to, carries the question in words, and carries a JSON Schema for the
// payload that answers it, so a client renders the question without knowing this agent.
//
// The interrupt id is derived from the question's kind and the call it is about, so the
// process serving the answering run is free to be a different one. A resume entry the
// client canceled carries no answer: a decline goes inside the payload as
// {"approval":"no"}, and the run that resumes puts the question again.
//
// A run answering one of the three human-in-the-loop questions leaves out the call it
// was about. The tool ran on the turn that asked, so the client holds that call and
// accumulates its argument fragments rather than replacing them, and the redispatch
// would write the arguments into the thread a second time. An approval's call has not
// been sent when the interrupt goes out, the gate running before the run traces it, so
// the run that answers sends it for the first time.
//
// The three-way approval travels natively. The schema asks for no, once or always,
// where the AI SDK's own approval is a boolean and the standing allow needs a field of
// this agent's own. A client that ignores the schema and sends the {"approved": bool}
// AG-UI recommends gets a two-way allow or decline.
//
// # A stored conversation
//
// Replay writes the conversation back as one messages snapshot: the user turns, the
// assistant turns, the calls and their results, in the order the journal holds them. A
// conversation the run left waiting on a question ends on the interrupt it stopped at,
// which is Ask writing after Replay.
package agui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/choria-io/fisk-ai/internal/serve/web"
)

// MountPath is the path segment this format answers on under a channel's base path, so
// a channel with a base path of /fisk/v1 serves it at POST /fisk/v1/agui.
const MountPath = "agui"

// ErrBadRequest is what Decode refuses a request with. The channel answers it as a 400
// carrying the message, which describes the request and never this worker.
var ErrBadRequest = errors.New("the request is not a run this agent can answer")

// Format renders AG-UI events. It holds nothing between requests and is safe for
// concurrent use, every request being decoded into a writer of its own.
type Format struct{}

// New returns the format to mount.
func New() *Format { return &Format{} }

// Mount is this format on MountPath, for a channel's Options.Formats or the web
// channel's Builder.
func Mount() web.Mount { return web.Mount{Path: MountPath, Format: New()} }

// Decode reads an AG-UI RunAgentInput into the turn it asks for.
//
// A body carrying a resume entry this agent minted the interrupt for asks for that
// answer alone: an AG-UI client sends the whole thread on every run, so the newest user
// message in one is a turn the journal already holds. A body carrying no such entry
// asks for its newest user message as the next turn.
//
// The writer holds w and writes nothing until Open is called on it.
func (f *Format) Decode(w http.ResponseWriter, r *http.Request) (web.Turn, web.TurnWriter, error) {
	var body request

	err := json.NewDecoder(r.Body).Decode(&body)
	if err != nil {
		return web.Turn{}, nil, fmt.Errorf("%w: %w", ErrBadRequest, err)
	}

	turn, resent, err := body.turn()
	if err != nil {
		return web.Turn{}, nil, err
	}

	return turn, newTurnWriter(w, turn.ThreadID, body.runID(), resent), nil
}

// Replayer returns the writer a stored conversation is written into.
//
// The run frame the events sit in names the conversation the request asked for, since a
// page opening one from the rail sends no run of its own. The run id is derived from
// that same id: a second open of one conversation rebuilds the thread from the snapshot
// it reads, so nothing needs the two opens to differ.
func (f *Format) Replayer(w http.ResponseWriter, r *http.Request) web.TurnWriter {
	id := r.PathValue("id")

	return newTurnWriter(w, id, "open-"+id, "")
}
