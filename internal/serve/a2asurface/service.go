//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2asurface

import (
	"context"
	"fmt"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/serve"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/toolkit/builtin"
	fisktool "github.com/choria-io/fisk-ai/internal/toolkit/fisk"
	"github.com/choria-io/fisk-ai/internal/util"
)

// A tool service answers its callers directly and produces no work.
var _ serve.Service = (*Service)(nil)

// Service is the tool-serving surface: an a2a server, the transport it answers on, and
// what a program needs to describe it at startup.
type Service struct {
	srv      *a2a.Server
	held     *sharedTransport
	exposed  []string
	withheld []string
	describe []a2a.DescLine
}

// newService builds the tool-serving surface described by expose.agent.a2a.serve_tools
// over the transport its siblings share.
//
// The tool set is loaded before anything is registered, so a configuration whose
// filters leave nothing is refused rather than served as an agent with no tools.
func newService(cfg *config.Config, held *sharedTransport, opts ConfigOptions) (*Service, error) {
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

	svc := &Service{
		held:     held,
		withheld: builtin.WithheldFromA2A(cfg),
		describe: held.transport.Describe(cfg.Identity),
	}

	svc.srv, err = a2a.NewServer(held.transport, toolkit.Tools(tools), a2a.ServerOptions{
		Identity:    cfg.Identity,
		Version:     util.Version(),
		ConfirmTags: cfg.ConfirmTags(),
		Concurrency: cfg.A2AMaxConcurrentTools(),
		CallTimeout: cfg.A2AToolTimeout(),
		Logger:      opts.Logger,
		Telemetry:   opts.Telemetry,
	})
	if err != nil {
		return nil, err
	}

	svc.exposed = svc.srv.ExposedTools()
	if len(svc.exposed) == 0 {
		return nil, fmt.Errorf("no tools available to serve over a2a; all were filtered or confirmation-gated")
	}

	return svc, nil
}

// Name identifies the surface. There is one tool service per identity and the identity
// is what a program has already named by the time it prints this, so nothing qualifies
// it further.
func (s *Service) Name() string { return "a2a" }

// Close stops answering, which stops the micro service and takes this identity out of
// its queue group, so a peer calling during a drain is routed to a sibling rather than
// left waiting. The prompt channel beside it stops answering at the same moment, both
// being paths of the one service.
//
// A call already in flight is not covered. The a2a server answers on a goroutine of its
// own, bounded by expose.agent.a2a.tool_timeout and nothing else, so a command it
// started keeps running with nowhere to reply to.
func (s *Service) Close() error { return s.held.Close() }

// ExposedTools are the tools this surface serves, in card order.
func (s *Service) ExposedTools() []string { return s.exposed }

// WithheldBuiltins names the built-in tools this configuration enables that are not
// served, which is all of them: no built-in declares a2a exposure. An operator who
// enabled some would otherwise see a served set that silently excludes them.
func (s *Service) WithheldBuiltins() []string { return s.withheld }

// Describe returns the subjects this surface is reached on, for display.
func (s *Service) Describe() []a2a.DescLine { return s.describe }
