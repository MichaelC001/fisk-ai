//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/mcpclient"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/serve"
	"github.com/choria-io/fisk-ai/internal/telemetry"
)

// Builder describes this channel to serve.Endpoints, so a program that wants a browser
// in front of its agent links it in and a program that does not never references this
// package at all.
//
// The formats are the caller's: this package never names one, and a program mounting a
// third links its package in and passes one more Mount.
func Builder(formats ...Mount) serve.EndpointBuilder {
	return serve.EndpointBuilder{
		Name:    channelName,
		Enabled: func(cfg *config.Config) bool { return cfg.WebEnabled() },
		// The listener binds here, so a busy port fails before the banner rather than
		// after it. The context governs the introspection of the application and the
		// listing of the MCP servers the card names, so an operator who gives up while a
		// server is answering nothing gets the error back.
		Build: func(ctx context.Context, cfg *config.Config, opts serve.BuildOptions) ([]serve.Endpoint, error) {
			ch, err := NewFromConfig(ctx, cfg, ConfigOptions{
				Formats:          formats,
				Sessions:         opts.Sessions,
				MCPSessions:      opts.MCPSessions,
				Telemetry:        opts.Telemetry,
				SuspendRequested: opts.SuspendRequested,
				Version:          opts.Version,
				Logger:           opts.Logger,
			})
			if err != nil {
				return nil, err
			}

			return []serve.Endpoint{ch}, nil
		},
	}
}

// ConfigOptions are what a configured channel needs that no configuration can state:
// what the process decided, and what it is holding.
type ConfigOptions struct {
	// Formats are the frontend protocols to mount, each on its path under the
	// configured base path. The configuration chooses none: turning a format off
	// prevents nothing an enabled one allows.
	Formats []Mount

	// Sessions is the process's run-journal store, borrowed and never closed here. It is
	// required: a thread is a conversation, and this channel reads the store to tell a
	// thread it holds from one it is opening.
	Sessions runstate.Store

	// MCPSessions are the process's live sessions with the configured MCP servers,
	// borrowed and never closed here. Nil lists no MCP tool on the card, which is right
	// for a configuration that declares no server and leaves the card short for one that
	// does.
	MCPSessions *mcpclient.Sessions

	// Telemetry is the process's resolved provider, or nil when telemetry is off. The
	// card reports export off it rather than off the configuration, so an endpoint that
	// was refused leaves the card promising nothing. A worker exporting traces whose
	// channel is handed nil publishes a card saying the conversation reaches no
	// collector.
	Telemetry *telemetry.Provider

	// SuspendRequested is handed to every run, so a worker draining stops its turns
	// where they can be resumed from.
	SuspendRequested func() bool

	// Version is the calling program's build version, published on the agent card. An
	// empty one publishes the card as "dev".
	Version string

	// Logger receives the channel's own progress. Nil builds a text logger on stderr.
	Logger *slog.Logger
}

// NewFromConfig builds the web channel described by expose.agent.web, mounting the
// formats the caller passes. A channel given none takes no turn and answers the agent
// card alone, and its listener still binds, so a busy port and a bad address fail at
// startup.
//
// It resolves the agent's tools once, here, so the card names what a run would offer
// the model rather than what this channel could work out on its own. The context
// governs that: introspecting the application starts a subprocess, and listing an MCP
// server's tools is a round trip to it.
func NewFromConfig(ctx context.Context, cfg *config.Config, opts ConfigOptions) (*Channel, error) {
	if !cfg.WebEnabled() {
		return nil, fmt.Errorf("expose.agent.web is not configured")
	}

	if opts.Sessions == nil {
		return nil, fmt.Errorf("expose.agent.web needs a session store: a thread is a conversation, so a worker with nowhere to journal one would answer a first request and nothing after it")
	}

	tools, err := ResolveAgentTools(ctx, cfg, opts.MCPSessions)
	if err != nil {
		return nil, err
	}

	return New(Options{
		Listen:   cfg.WebListen(),
		BasePath: cfg.WebBasePath(),
		Origins:  cfg.WebOrigins(),
		Identity: cfg.Identity,
		Formats:  opts.Formats,
		Card: a2a.CardOptions{
			Version:     opts.Version,
			Model:       cfg.LLM.Model,
			Description: cfg.Description,
			DisplayName: cfg.DisplayName,
			Icon:        cfg.Icon,
			IconURL:     cfg.IconURL,
			Notes:       tools.Notes,
			Telemetry:   opts.Telemetry,
		},
		CardTools:        tools.Tools,
		ConfirmTags:      cfg.ConfirmTags(),
		Sessions:         opts.Sessions,
		SuspendRequested: opts.SuspendRequested,
		Logger:           opts.Logger,
	})
}
