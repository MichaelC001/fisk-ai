//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package asyncjobs

import (
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/conns"
	"github.com/choria-io/fisk-ai/internal/serve"
)

// Builder describes this channel to serve.Channels, so a program that wants the
// queued-jobs surface links it in and a program that does not never references this
// package at all.
func Builder() serve.ChannelBuilder {
	return serve.ChannelBuilder{
		Name:    "jobs",
		Enabled: func(cfg *config.Config) bool { return cfg.JobsEnabled() },
		Build: func(cfg *config.Config, opts serve.BuildOptions) (serve.Channel, error) {
			return NewFromConfig(cfg, ConfigOptions{
				Workers:          opts.Workers,
				SuspendRequested: opts.SuspendRequested,
				Logger:           opts.Logger,
			})
		},
	}
}

// ConfigOptions are the things a configured channel needs that no configuration can
// state: what the process decided, and what it is holding.
type ConfigOptions struct {
	// Workers overrides the configured worker count when it is greater than zero,
	// because the number is a property of the process rather than of the agent and a
	// caller reading a command line knows it and the file does not.
	Workers int

	// SuspendRequested is handed to every run, so a worker draining stops its runs
	// where they can be resumed from. See Options.SuspendRequested.
	SuspendRequested func() bool

	// Logger receives the channel's own progress and the engine's, bridged. Nil builds
	// a text logger on stderr.
	Logger *slog.Logger
}

// NewFromConfig builds the queued-jobs channel described by expose.agent.jobs.
//
// It dials its own NATS connection rather than borrowing the shared one, and owns it:
// the queue engine requires nats.UseOldRequestStyle, which nothing else wants, and
// separating them also lets the queue live on a cluster of its own. Everything else
// about the connection comes from conns, so it differs in that one respect and no
// other. Close releases it.
//
// It returns an error when the configuration does not enable the intake, since
// building a channel nothing asked for would start a worker on a queue the operator
// never named.
func NewFromConfig(cfg *config.Config, opts ConfigOptions) (*Channel, error) {
	if !cfg.JobsEnabled() {
		return nil, fmt.Errorf("expose.agent.jobs is not configured")
	}

	natsContext := cfg.JobsNatsContext()
	if natsContext == "" {
		return nil, fmt.Errorf("expose.agent.jobs needs a NATS context, either nats_context at the top level or under the block")
	}

	workers := cfg.JobsWorkers()
	if opts.Workers > 0 {
		workers = opts.Workers
	}

	provider, err := conns.Connect(natsContext, "jobs "+cfg.Identity, nats.UseOldRequestStyle())
	if err != nil {
		return nil, err
	}

	ch, err := New(Options{
		Conn:             provider.Nats(),
		Queue:            cfg.JobsQueue(),
		TaskType:         cfg.JobsTaskType(),
		Identity:         cfg.Identity,
		Concurrency:      workers,
		MaxPayload:       cfg.JobsMaxPayload(),
		SuspendRequested: opts.SuspendRequested,
		Logger:           opts.Logger,
	})
	if err != nil {
		provider.Close()
		return nil, err
	}

	// The channel owns the connection from here, so its Close releases it. Handing it
	// over rather than closing it here is what makes a failure above release it and a
	// success keep it.
	ch.ownConn = provider

	return ch, nil
}
