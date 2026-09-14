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
	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/mcpclient"
	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// CardPath is the segment the agent card answers on under the base path, so a channel
// on "/fisk/v1" answers GET /fisk/v1/card. A Format may not be mounted there.
const CardPath = "card"

// cardRefusal is the body a request gets when the card cannot be built, which is a
// configured icon url, display name, icon or sample prompt that a2a.BuildCard refuses.
// It names neither the value nor the worker, and the log line beside it names the key.
const cardRefusal = "this agent's card cannot be served; check its configured description, display name, icon, icon url and prompts"

// AgentTools are the tools an agent card lists and what could not be listed.
type AgentTools struct {
	// Tools are the agent's own tools, in the order a run assembles them: the
	// application's commands first, then the built-ins the configuration enables, then
	// the tools of each remote_tools host, then the tools of each configured MCP server.
	// They are listed whatever their tags say, confirm-gated commands included, since
	// the channel has an operator in front of it to approve one.
	Tools []toolkit.Tool

	// Notes name each source whose tools are missing, one sentence each, and go on the
	// card as wire.AgentCard.Notes. A console renders them beside the tool list, so a
	// tool that is not there reads as a source being down rather than a tool that was
	// removed.
	Notes []string
}

// ResolveAgentTools assembles the agent's tools outside a run, through agent.Assemble
// under the Lenient policy: the application's commands as the configuration filters
// them, the human-in-the-loop, memory and knowledge built-ins the configuration
// enables, the tools of every remote_tools host the client reaches, and the tools of
// every MCP server the sessions hold.
//
// The built-ins are enumerated with nil stores, since a card has no handler. Assemble
// claims names in a run's order, so a clashing remote or MCP tool gets the prefix a
// run gives it.
//
// It is called once, when the channel is built, and the card is assembled from what it
// returns on each request. The tools are never called through: a card has their
// names, descriptions, schemas and declared behavior.
//
// A host or an MCP server whose tools cannot be listed leaves its tools off the card
// and gets a note with its configured name, where a run refuses to start on the same
// failure. The two
// answers are right for different things: a prompt may depend on a tool that is not
// there, and a console showing a short tool list with the reason beside it is better
// than a console that will not load. A nil client with hosts configured, or nil
// sessions with servers configured, is such a failure and gets its note. The client
// and the sessions are borrowed and stay open for the runs.
//
// An error is an application that cannot be introspected or a built-in whose name a
// command took, which leaves no card worth serving.
func ResolveAgentTools(ctx context.Context, cfg *config.Config, remote *a2a.Client, sessions *mcpclient.Sessions) (AgentTools, error) {
	asm, err := agent.Assemble(ctx, cfg, agent.Sources{Remote: remote, MCP: []*mcpclient.Sessions{sessions}}, agent.SurfaceRun, agent.Lenient)
	if err != nil {
		return AgentTools{}, fmt.Errorf("resolving the agent's tools: %w", err)
	}

	out := AgentTools{Tools: make([]toolkit.Tool, 0, asm.Len())}
	for _, t := range asm.Tools {
		out.Tools = append(out.Tools, t.Tool)
	}

	for _, p := range asm.Problems {
		out.Notes = append(out.Notes, problemNote(p))
	}

	return out, nil
}

// problemNote is the card's line for a source that failed: the MCP server or the
// remote agent by its configured name, or every host of a block no client reached.
func problemNote(p agent.Problem) string {
	kind := "remote agent"
	if p.Source == agent.SourceMCPClients {
		kind = "mcp server"
	}

	if p.Alias == "" {
		return fmt.Sprintf("the tools of every %s could not be listed, so any tool one supplies is missing here", kind)
	}

	return fmt.Sprintf("the tools of the %s %q could not be listed, so any tool it supplies is missing here", kind, p.Alias)
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
