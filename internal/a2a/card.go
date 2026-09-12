//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	"slices"

	wire "github.com/choria-io/fisk-ai/internal/a2a/wire/v1"
	"github.com/choria-io/fisk-ai/internal/telemetry"
	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// CardOptions is what an agent says about itself on a card, apart from its tools.
//
// It is separate from ServerOptions because a card is published by more than the a2a
// server: an HTTP channel serving a console publishes one for the same agent without
// serving a tool to any peer, and it has no concurrency limit, call timeout or
// transport to state.
type CardOptions struct {
	// Identity is the agent's identity, the same string a caller addresses it by. It
	// becomes AgentCard.Name.
	Identity string

	// Version is the agent's own version. Empty publishes "dev", so a card always
	// carries one.
	Version string

	// Model is the model the agent answers a prompt with, as its own configuration
	// names it. Empty publishes no model, which is what an agent taking no prompts
	// says.
	Model string

	// Description is what the agent is for, in the operator's own words. Whoever
	// displays it truncates it.
	Description string

	// DisplayName is the human name for the agent, where Identity is the routing one.
	// Empty leaves a reader displaying the identity.
	DisplayName string

	// Icon is an emoji to draw beside the name.
	Icon string

	// IconURL is an https URL of an image to draw beside the name. BuildCard checks it
	// with wire.CheckIconURL and refuses a card carrying anything else, since the card
	// reaches a browser.
	IconURL string

	// Prompts are things a person can ask this agent, in the operator's own words, for
	// a reader to offer somebody who has not used it before. BuildCard checks them with
	// wire.CheckPrompts.
	Prompts []string

	// Notes are what could not be put on this card, one sentence each: an MCP server
	// whose tools could not be listed names that server, so a tool missing from the
	// card reads as a source being down rather than a tool that was removed.
	Notes []string

	// Telemetry is the resolved provider, which the card reports export off. It is read
	// rather than a configuration value, so a veto or an endpoint that was refused does
	// not leave the card promising an export that will not happen. Nil reports neither.
	Telemetry *telemetry.Provider
}

// BuildCard assembles an agent card from opts and tools. Both the a2a server and an
// HTTP channel publish it, so one place decides what a card contains.
//
// It applies no exposure policy: every tool given is listed, confirm-gated tools
// included. a2a filters its own set with selectExposed before it gets here, because a
// served agent has no operator to approve a gated command; a channel with an operator
// in front of it passes the agent's own set.
//
// It fails on an IconURL a browser must not be handed, and on a display name, icon or
// sample prompt the discovery reply schema will not carry: a card the schema refuses is
// one every caller fails to discover, and failing here puts that in front of the
// operator who wrote the value.
func BuildCard(opts CardOptions, tools []toolkit.Tool) (wire.AgentCard, error) {
	err := wire.CheckIconURL(opts.IconURL)
	if err != nil {
		return wire.AgentCard{}, err
	}

	err = wire.CheckDisplayName(opts.DisplayName)
	if err != nil {
		return wire.AgentCard{}, err
	}

	err = wire.CheckIcon(opts.Icon)
	if err != nil {
		return wire.AgentCard{}, err
	}

	err = wire.CheckPrompts(opts.Prompts)
	if err != nil {
		return wire.AgentCard{}, err
	}

	card := wire.AgentCard{
		Name:             opts.Identity,
		Version:          versionOrDev(opts.Version),
		Model:            opts.Model,
		Description:      opts.Description,
		DisplayName:      opts.DisplayName,
		Icon:             opts.Icon,
		IconURL:          opts.IconURL,
		Prompts:          slices.Clone(opts.Prompts),
		Notes:            slices.Clone(opts.Notes),
		Protocols:        []string{wire.ProtocolNamespace},
		Telemetry:        opts.Telemetry.Enabled(),
		TelemetryContent: opts.Telemetry.CaptureEnabled(),
	}

	for _, t := range tools {
		card.Tools = append(card.Tools, wire.ToolDescriptor{
			Name:        t.Name(),
			Description: t.ModelDescription(),
			InputSchema: marshalSchema(t.InputSchema()),
			Behavior:    toolBehavior(toolkit.BehaviorOf(t)),
		})
	}

	return card, nil
}
