//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/mcpclient"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/toolkit/fisktool"
)

// CardPath is the segment the agent card answers on under the base path, so a channel
// on "/fisk/v1" answers GET /fisk/v1/card. A Format may not be mounted there.
const CardPath = "card"

// cardRefusal is the body a request gets when the card cannot be built, which is a
// configured icon url, display name or icon that a2a.BuildCard refuses. It names
// neither the value nor the worker, and the log line beside it names the key.
const cardRefusal = "this agent's card cannot be served; check its configured description, display name, icon and icon url"

// AgentTools are the tools an agent card lists and what could not be listed.
type AgentTools struct {
	// Tools are the agent's own tools, in the order they were resolved: the
	// application's commands first, then the tools of each configured MCP server. They
	// are listed whatever their tags say, confirm-gated commands included, since the
	// channel has an operator in front of it to approve one.
	Tools []toolkit.Tool

	// Notes name each source whose tools are missing, one sentence each, and go on the
	// card as wire.AgentCard.Notes. A console renders them beside the tool list, so a
	// tool that is not there reads as a source being down rather than a tool that was
	// removed.
	Notes []string
}

// ResolveAgentTools names the agent's tools outside a run: the application's commands
// as the configuration filters them, and the tools of every connected MCP server.
//
// It is called once, when the channel is built, and the card is assembled from what it
// returns on each request. The tools are never called through: a card carries their
// names, descriptions, schemas and declared behavior.
//
// An MCP server whose tools cannot be listed leaves its tools off the card and gets a
// note naming it, where a run refuses to start on the same failure. The two answers are
// right for different things: a prompt may depend on a tool that is not there, and a
// console showing a short tool list with the reason beside it is better than a console
// that will not load. Sessions are borrowed and stay open for the runs.
//
// An error is the application's own commands failing to load, which leaves no card
// worth serving.
func ResolveAgentTools(ctx context.Context, cfg *config.Config, sessions *mcpclient.Sessions) (AgentTools, error) {
	commands, err := fisktool.LoadTools(ctx, cfg)
	if err != nil {
		return AgentTools{}, fmt.Errorf("loading the application's tools: %w", err)
	}

	out := AgentTools{Tools: toolkit.Tools(commands)}

	if sessions == nil {
		return out, nil
	}

	taken := make(map[string]bool, len(commands))
	for _, t := range commands {
		taken[t.Name()] = true
	}

	// The error is read off each server's own outcome rather than from the return: the
	// return reports the first server that failed, and this card carries the tools of
	// the ones that answered along with a note for each that did not.
	imported, _ := mcpclient.Import(ctx, sessions, mcpclient.NewClaimedNames(taken, nil))
	for _, server := range imported.Servers {
		if server.Err != nil {
			out.Notes = append(out.Notes, fmt.Sprintf("the tools of the mcp server %q could not be listed, so any tool it supplies is missing here", server.Server.Name))

			continue
		}

		out.Tools = append(out.Tools, toolkit.Tools(server.Tools)...)
	}

	return out, nil
}

// serveCard answers the agent card.
//
// The card is built per request rather than at construction, so a worker restarted with
// a different model or a different configuration answers with what it is running now
// rather than with what the process that bound this listener was.
func (c *Channel) serveCard(w http.ResponseWriter, r *http.Request) {
	card, err := a2a.BuildCard(c.card, c.cardTools)
	if err != nil {
		c.log.Error("Refusing to serve the agent card", "error", err, "remote", r.RemoteAddr)
		http.Error(w, cardRefusal, http.StatusInternalServerError)

		return
	}

	body, err := json.Marshal(card)
	if err != nil {
		c.log.Error("Encoding the agent card failed", "error", err, "remote", r.RemoteAddr)
		http.Error(w, cardRefusal, http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	_, err = w.Write(body)
	if err != nil {
		c.log.Warn("Writing the agent card failed", "error", err, "remote", r.RemoteAddr)
	}
}
