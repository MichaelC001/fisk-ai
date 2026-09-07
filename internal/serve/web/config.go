//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/serve"
)

// Builder describes this channel to serve.Endpoints, so a program that wants a browser
// in front of its agent links it in and a program that does not never references this
// package at all.
func Builder() serve.EndpointBuilder {
	return serve.EndpointBuilder{
		Name:    channelName,
		Enabled: func(cfg *config.Config) bool { return cfg.WebEnabled() },
		// The listener binds here, so a busy port fails before the banner rather than
		// after it. Nothing in the build reaches the network, so the context is unread.
		Build: func(_ context.Context, cfg *config.Config, opts serve.BuildOptions) ([]serve.Endpoint, error) {
			ch, err := NewFromConfig(cfg, ConfigOptions{
				Sessions:         opts.Sessions,
				SuspendRequested: opts.SuspendRequested,
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
	// Sessions is the process's run-journal store, borrowed and never closed here. It is
	// required: a thread is a conversation, and this channel reads the store to tell a
	// thread it holds from one it is opening.
	Sessions runstate.Store

	// SuspendRequested is handed to every run, so a worker draining stops its turns
	// where they can be resumed from.
	SuspendRequested func() bool

	// Logger receives the channel's own progress. Nil builds a text logger on stderr.
	Logger *slog.Logger
}

// NewFromConfig builds the web channel described by expose.agent.web.
//
// No format is mounted yet, so the channel it builds refuses every path. The listener
// still binds, so a busy port and a bad address fail at startup.
func NewFromConfig(cfg *config.Config, opts ConfigOptions) (*Channel, error) {
	if !cfg.WebEnabled() {
		return nil, fmt.Errorf("expose.agent.web is not configured")
	}

	if opts.Sessions == nil {
		return nil, fmt.Errorf("expose.agent.web needs a session store: a thread is a conversation, so a worker with nowhere to journal one would answer a first request and nothing after it")
	}

	return New(Options{
		Listen:           cfg.WebListen(),
		BasePath:         cfg.WebBasePath(),
		Origins:          cfg.WebOrigins(),
		Identity:         cfg.Identity,
		Sessions:         opts.Sessions,
		SuspendRequested: opts.SuspendRequested,
		Logger:           opts.Logger,
	})
}
