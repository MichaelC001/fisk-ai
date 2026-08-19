//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// Package a2aendpoint answers other agents over a2a, as endpoints a serve.Server hosts
// rather than as a command of its own.
//
// Two kinds of caller arrive on one transport. A peer that invokes a tool discovers a
// card and calls it directly: no prompt is involved and no agent loop runs, which makes
// it cheaper than handing that peer a prompt and gives it a different security posture,
// since the caller reaches the tools an operator chose to expose and nothing else. A
// peer that sends a prompt gets an agent run: it is acked, the events of the run stream
// back as the loop produces them, and a result or an error closes it.
//
// The two share one transport and one identity, since discovery, tools and tasks are
// paths of a single micro service. Builder opens that transport once and returns
// whichever endpoints the configuration asks for; the first of them to close stops the
// service answering, so one identity leaves its queue group once.
package a2aendpoint

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/conns"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/serve"
	"github.com/choria-io/fisk-ai/internal/telemetry"
	"github.com/choria-io/fisk-ai/internal/util"
)

// Builder describes these endpoints to serve.Endpoints, so a program that wants to
// answer peers links it in and a program that does not never references this package
// at all.
func Builder() serve.EndpointBuilder {
	return serve.EndpointBuilder{
		Name:    "a2a",
		Enabled: func(cfg *config.Config) bool { return cfg.A2AEnabled() },
		Build: func(cfg *config.Config, opts serve.BuildOptions) ([]serve.Endpoint, error) {
			return NewFromConfig(cfg, ConfigOptions{
				Conns:      opts.Conns,
				ConfigFile: opts.ConfigFile,
				Logger:     opts.Logger,
				Telemetry:  opts.Telemetry,
				Sessions:   opts.Sessions,
			})
		},
	}
}

// ConfigOptions are what a configured endpoint needs that no configuration can state:
// what the process decided, and what it is holding.
type ConfigOptions struct {
	// Conns is the NATS connection to serve on. It is borrowed and never closed here,
	// since the runs and the stores share it.
	Conns *conns.Provider

	// ConfigFile names the file the configuration was read from, so a refusal can name
	// the file to edit.
	ConfigFile string

	// Logger receives the endpoints' progress, which is a line per served call and per
	// prompt. Nil leaves it to each server's own default.
	Logger *slog.Logger

	// Telemetry, when non-nil, receives a span per served call and reaches the tools
	// those calls run. It is the process's provider, borrowed like the connection: the
	// program that built it flushes it.
	Telemetry *telemetry.Provider

	// Sessions is the run-journal store, borrowed like the connection. The prompt
	// channel reads it to answer a caller that asks only to be told what a conversation
	// holds, which takes no turn and calls no model. Nil refuses those requests and
	// changes nothing else: every other shape reaches the store through the run.
	Sessions runstate.Store
}

// NewFromConfig builds the endpoints expose.agent.a2a asks for: the tool service under
// serve_tools, the prompt channel under prompts, and both when the block carries both.
//
// It refuses a configuration that enables neither, since building an endpoint nobody
// asked for would put an application's commands on the network on the strength of a
// linked builder. It owns the transport it opens, and hands it to the endpoints it
// returns, whose first close stops it.
//
// The endpoints are returned in the order a server hosts them, the channel first, so a
// worker's banner reads in the order work arrives.
func NewFromConfig(cfg *config.Config, opts ConfigOptions) ([]serve.Endpoint, error) {
	if !cfg.A2AEnabled() {
		return nil, fmt.Errorf("expose.agent.a2a enables neither serve_tools nor prompts")
	}
	if opts.Conns == nil {
		return nil, fmt.Errorf("expose.agent.a2a needs a NATS connection, which nats_context is what supplies")
	}

	// The fault channel is built before the transport, since the transport reports
	// through it and may do so before this function returns.
	held := &sharedTransport{faults: make(chan error, 1)}

	transport, err := a2a.NewTransport(cfg.A2ATransport(), opts.Conns, a2a.TransportConfig{
		Identity: cfg.Identity,
		Logger:   opts.Logger,
		OnFault:  held.fault,
	})
	if err != nil {
		return nil, err
	}

	held.transport = transport

	var built []serve.Endpoint

	if cfg.A2APromptsEnabled() {
		ch, err := newChannel(cfg, held, opts)
		if err != nil {
			held.closeQuietly(opts.Logger)
			return nil, err
		}

		built = append(built, ch)
	}

	// Discovery is one route on the one micro service this identity registers, so
	// whoever answers it answers for every endpoint here. The tool service owns it when
	// there is one, since its card is the one with tools on it; an identity that only
	// takes prompts still has to say what it is, and gets a card with no tools rather
	// than no answer at all. Registering it twice would not fail: micro would subscribe
	// again on the same subject in the same queue group, and a peer would get whichever
	// card NATS picked.
	if cfg.A2AServeToolsEnabled() {
		svc, err := newService(cfg, held, opts)
		if err != nil {
			held.closeQuietly(opts.Logger)
			return nil, err
		}

		built = append(built, svc)
	} else {
		err = serveCard(cfg, held, opts)
		if err != nil {
			held.closeQuietly(opts.Logger)
			return nil, err
		}
	}

	return built, nil
}

// serveCard answers discovery for an identity that serves no tools, so a peer can ask
// what it is and a caller can read what it does with a conversation before sending one.
//
// It registers the discovery route and nothing else. The server it builds owns no tools
// and answers no tool calls, and it is released with the transport rather than being an
// endpoint of its own, since it produces no work and has nothing to close.
func serveCard(cfg *config.Config, held *sharedTransport, opts ConfigOptions) error {
	_, err := a2a.NewServer(held.transport, nil, a2a.ServerOptions{
		Identity:      cfg.Identity,
		Version:       util.Version(),
		Logger:        opts.Logger,
		Telemetry:     opts.Telemetry,
		DiscoveryOnly: true,
	})

	return err
}

// sharedTransport is the one transport both endpoints answer on, closed once however
// many of them ask.
//
// Closing it stops the micro service, which takes this identity out of its queue group
// for every path at once. A drain wants exactly that, and the second endpoint's close
// reports the first one's answer rather than a failure, so a clean shutdown prints no
// error for having released one thing twice.
//
// A prompt already accepted is unaffected: its reply inbox and its cancel subscription
// belong to the NATS connection rather than to the service registration, so it goes on
// streaming and still sends its terminal message.
type sharedTransport struct {
	transport a2a.Transport
	once      sync.Once
	err       error

	// faults carries the transport's report that it has stopped serving. It is
	// buffered by one and sent to once, since the first fault is what ends the worker
	// and the transport may report the same stop through more than one handler.
	faults    chan error
	faultOnce sync.Once
}

func (s *sharedTransport) Close() error {
	s.once.Do(func() { s.err = s.transport.Close() })

	return s.err
}

// fault records that this identity has stopped answering. It is called from the
// binding's own goroutine, so it must not block: the channel is buffered and written
// once.
func (s *sharedTransport) fault(err error) {
	s.faultOnce.Do(func() { s.faults <- err })
}

// closeQuietly gives the transport back when an endpoint failed to build, reporting a
// second failure to the log: the error that caused the teardown is the one the caller
// needs.
func (s *sharedTransport) closeQuietly(log *slog.Logger) {
	err := s.Close()
	if err != nil && log != nil {
		log.Error("Releasing the a2a transport failed", "error", err)
	}
}
