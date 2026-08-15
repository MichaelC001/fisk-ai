//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// Package a2asurface serves an agent's tools to other agents over a2a, as a surface a
// serve.Server hosts rather than as a command of its own.
//
// A peer discovers a card and invokes a tool. No prompt is involved and no agent loop
// runs, which is what makes this cheaper than handing that peer a task and gives it a
// different security posture: the caller reaches the tools an operator chose to expose
// and nothing else.
//
// The synchronous task surface, where a prompt travels over a2a and the loop does run,
// belongs in this package when it lands. It is a serve.Channel rather than a
// serve.Service, and the two share one transport under one identity, since discovery,
// tools and tasks are route hints on one micro service.
package a2asurface

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/conns"
	"github.com/choria-io/fisk-ai/internal/serve"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/toolkit/builtin"
	fisktool "github.com/choria-io/fisk-ai/internal/toolkit/fisk"
	"github.com/choria-io/fisk-ai/internal/util"
)

// Builder describes this surface to serve.Surfaces, so a program that wants to serve
// tools links it in and a program that does not never references this package at all.
func Builder() serve.ServiceBuilder {
	return serve.ServiceBuilder{
		Name:    "a2a",
		Enabled: func(cfg *config.Config) bool { return cfg.A2AEnabled() },
		Build: func(cfg *config.Config, opts serve.BuildOptions) (serve.Service, error) {
			return NewFromConfig(cfg, ConfigOptions{
				Conns:      opts.Conns,
				ConfigFile: opts.ConfigFile,
				Logger:     opts.Logger,
			})
		},
	}
}

// ConfigOptions are what a configured surface needs that no configuration can state:
// what the process decided, and what it is holding.
type ConfigOptions struct {
	// Conns is the NATS connection to serve on. It is borrowed and never closed here,
	// since the runs and the stores share it.
	Conns *conns.Provider

	// ConfigFile names the file the configuration was read from, so a refusal can name
	// the file to edit.
	ConfigFile string

	// Logger receives the a2a server's progress, which is a line per served call. Nil
	// leaves it to the a2a server's own default.
	Logger *slog.Logger
}

// Service is the tool-serving surface: an a2a server, the transport it answers on, and
// what a program needs to describe it at startup.
type Service struct {
	srv       *a2a.Server
	transport a2a.Transport
	exposed   []string
	withheld  []string
	describe  []a2a.DescLine
	closeOnce sync.Once
	closeErr  error
}

// NewFromConfig builds the tool-serving surface described by
// expose.agent.agent_to_agent.
//
// It refuses a configuration that does not enable serving, since building a surface
// nobody asked for would expose an application's commands to the network on the
// strength of a linked builder. It owns the transport it opens and stops it on Close.
//
// The tool set is loaded before anything is registered, so a configuration whose
// filters leave nothing is refused rather than served as an agent with no tools.
func NewFromConfig(cfg *config.Config, opts ConfigOptions) (*Service, error) {
	if !cfg.A2AEnabled() {
		return nil, fmt.Errorf("expose.agent.agent_to_agent is not enabled")
	}
	if opts.Conns == nil {
		return nil, fmt.Errorf("expose.agent.agent_to_agent needs a NATS connection, which nats_context is what supplies")
	}

	// Loaded on a background context: the process installs its signal handling after
	// the surfaces are built, so a context passed in here is one nothing would cancel.
	// Introspecting the application carries a bound of its own.
	tools, err := fisktool.ServedTools(context.Background(), cfg)
	if err != nil {
		return nil, err
	}
	if len(tools) == 0 {
		return nil, fmt.Errorf("no tools available after filtering; check include/exclude in %q", opts.ConfigFile)
	}

	transport, err := a2a.NewTransport(cfg.A2ATransport(), opts.Conns, a2a.TransportConfig{Identity: cfg.Identity})
	if err != nil {
		return nil, err
	}

	svc := &Service{
		transport: transport,
		withheld:  builtin.WithheldFromA2A(cfg),
		describe:  transport.Describe(cfg.Identity),
	}

	// From here the transport is registered, so every failure gives it back rather than
	// leaving a micro service behind for a surface the caller never received.
	svc.srv, err = a2a.NewServer(transport, toolkit.Tools(tools), a2a.ServerOptions{
		Identity:    cfg.Identity,
		Version:     util.Version(),
		ConfirmTags: cfg.ConfirmTags(),
		Concurrency: cfg.A2AMaxConcurrentTools(),
		CallTimeout: cfg.A2AToolTimeout(),
		Logger:      opts.Logger,
	})
	if err != nil {
		svc.closeQuietly(opts.Logger)
		return nil, err
	}

	svc.exposed = svc.srv.ExposedTools()
	if len(svc.exposed) == 0 {
		svc.closeQuietly(opts.Logger)
		return nil, fmt.Errorf("no tools available to serve over a2a; all were filtered or confirmation-gated")
	}

	return svc, nil
}

// Name identifies the surface. There is one a2a surface per identity and the identity
// is what a program has already named by the time it prints this, so nothing qualifies
// it further.
func (s *Service) Name() string { return "a2a" }

// Close stops answering, which stops the micro service and takes this identity out of
// its queue group, so a peer calling during a drain is routed to a sibling rather than
// left waiting.
//
// A call already in flight is not covered. The a2a server answers on a goroutine of its
// own, bounded by expose.agent.a2a.tool_timeout and nothing else, so a command it
// started keeps running with nowhere to reply to.
func (s *Service) Close() error {
	s.closeOnce.Do(func() { s.closeErr = s.srv.Stop() })

	return s.closeErr
}

// ExposedTools are the tools this surface serves, in card order.
func (s *Service) ExposedTools() []string { return s.exposed }

// WithheldBuiltins names the built-in tools this configuration enables that are not
// served, which is all of them: no built-in declares a2a exposure. An operator who
// enabled some would otherwise see a served set that silently excludes them.
func (s *Service) WithheldBuiltins() []string { return s.withheld }

// Describe returns the subjects this surface is reached on, for display.
func (s *Service) Describe() []a2a.DescLine { return s.describe }

// closeQuietly gives the transport back on a construction failure, reporting a second
// failure to the log: the error that caused the teardown is the one the caller needs.
func (s *Service) closeQuietly(log *slog.Logger) {
	err := s.transport.Close()
	if err != nil && log != nil {
		log.Error("Releasing the a2a transport failed", "error", err)
	}
}
