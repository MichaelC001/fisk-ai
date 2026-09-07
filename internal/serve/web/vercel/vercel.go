//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// Package vercel renders a turn as the AI SDK UI message stream, the protocol the
// Vercel AI SDK's useChat and the AI Elements components read. It is a web.Format,
// mounted on MountPath under the channel's base path.
//
// # The response
//
// The five headers the AI SDK's own server sets, one server-sent event per stream
// part, a flush after each one, and "data: [DONE]" to end the body. Every tool part
// carries dynamic, since a page cannot know an agent's tool names when it is built
// and that flag is what makes the client render a dynamic tool part rather than a
// typed one it has no type for.
//
// # The request
//
// Decode reads the chat id and the newest user message and takes everything else
// from the journal. The AI SDK's documented contract has the server rebuild the
// conversation from the messages the client sent; doing that here would let a page
// rewrite history the worker is authoritative for.
//
// # Questions
//
// An approval goes out as a tool-approval-request, which mutates the tool part its
// toolCallId names. The confirm gate runs before the run traces the call, so that
// part does not exist and the client would drop the approval in silence; the format
// synthesizes the tool-input-available first, carrying the gate's rendered command
// line and the tag that gated it. On the turn that resumes, the approved call is
// traced and would produce a second tool-input-available under the same toolCallId,
// which the writer suppresses from the id the answering request carried.
//
// The AI SDK has no shape for the three human-in-the-loop questions, so each goes as
// a data part the page reads without a schema, and the page answers it in a
// fiskAnswer field on its next POST. The standing allow of a confirm gate travels
// there too, having no boolean in the SDK's own approval to travel in. A page that
// never sets fiskAnswer answers an approval through the SDK's own approval-responded
// part and gets a two-way allow or decline.
package vercel

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/choria-io/fisk-ai/internal/serve/web"
)

// MountPath is the path segment this format answers on under a channel's base path,
// so a channel with a base path of /fisk/v1 serves it at POST /fisk/v1/vercel.
const MountPath = "vercel"

// ErrBadRequest is what Decode refuses a request with. The channel answers it as a
// 400 carrying the message, which describes the request and never this worker.
var ErrBadRequest = errors.New("the request is not a chat this agent can answer")

// Format renders the AI SDK UI message stream. It holds nothing between requests and
// is safe for concurrent use, every request being decoded into a writer of its own.
type Format struct{}

// New returns the format to mount.
func New() *Format { return &Format{} }

// Mount is this format on MountPath, for a channel's Options.Formats or the web
// channel's Builder.
func Mount() web.Mount { return web.Mount{Path: MountPath, Format: New()} }

// Decode reads the AI SDK's POST body into the turn it asks for.
//
// A body carrying an answer, either in fiskAnswer or as an approval the client
// answered in the newest assistant message, asks for that answer alone: useChat
// resends the whole history on every request, so the newest user message in one is a
// turn the journal already holds. A body carrying no answer asks for its newest user
// message as the next turn.
//
// The writer holds w and writes nothing until Open is called on it.
func (f *Format) Decode(w http.ResponseWriter, r *http.Request) (web.Turn, web.TurnWriter, error) {
	var body request

	err := json.NewDecoder(r.Body).Decode(&body)
	if err != nil {
		return web.Turn{}, nil, fmt.Errorf("%w: %w", ErrBadRequest, err)
	}

	turn, err := body.turn()
	if err != nil {
		return web.Turn{}, nil, err
	}

	answered := ""
	if turn.Answer != nil {
		answered = turn.Answer.ToolUseID
	}

	return turn, newTurnWriter(w, answered, body.continues()), nil
}

// Replayer returns the writer a stored conversation is written into. Nothing has been
// answered and nothing continues, so the writer suppresses no part and mints its ids from
// the start.
func (f *Format) Replayer(w http.ResponseWriter, _ *http.Request) web.TurnWriter {
	return newTurnWriter(w, "", "")
}
