//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/conns"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/serve"
	"github.com/choria-io/fisk-ai/internal/serve/a2aendpoint"
	"github.com/choria-io/fisk-ai/internal/serve/embedded"
	"github.com/choria-io/fisk-ai/internal/telemetry"
)

// localIdentity is what an agent hosted for this terminal alone calls itself, when the
// configuration names no identity. It reaches nothing outside the process, so it needs
// to be a legal name rather than a unique one.
const localIdentity = "local"

// hostedAgent is an agent running in this process behind the prompts channel, and the
// client that talks to it.
//
// It exists so a terminal reaches its own agent the same way it reaches somebody else's:
// one client, one protocol, one set of rules about questions and cancels. What a
// terminal gains for the cost of a broker is that the local path is the path everybody
// exercises, rather than the one nobody tests until a peer complains.
type hostedAgent struct {
	broker   *embedded.Broker
	server   *serve.Server
	client   *a2a.Client
	identity string
	done     chan error

	// natsContext names the bus the agent is on, and is empty for an agent hosted here.
	// A conversation is resumed differently in the two cases, so what the resume hint
	// prints depends on it.
	natsContext string

	// conns and transport are held only by a handle that dialed somebody else's worker,
	// which owns the connection it made. A hosted agent borrows the broker's and closes
	// the broker instead.
	conns     *conns.Provider
	transport a2a.Transport

	// telemetry is the provider the runs were given, kept so a caller can report what
	// will actually be exported rather than what the configuration asked for. A veto or
	// a rejected endpoint leaves it disabled with the configuration still saying
	// enabled.
	telemetry *telemetry.Provider
}

// hostOptions is what a hosted agent needs beyond the configuration: what the process
// resolved, and what it is holding.
type hostOptions struct {
	Config     *config.Config
	ConfigFile string
	APIKey     string
	BaseURL    string
	WorkDir    string
	// Resources are the shared stores and the connection the configuration named. They
	// are built from the operator's own nats_context and stay on it: the broker below
	// has no JetStream and no peers, so a session store or a set of remote tools given
	// that connection would find nothing.
	Resources *serve.Resources
	// Sessions is the run-journal store the hosted agent and its channel share. The
	// run needs one to journal into and the channel needs one to read a conversation
	// back from, and they have to be the same store or a terminal would read a
	// conversation its own runs are not writing. Nil takes the one Resources carries,
	// and nil with no Resources leaves the run to open its own and the channel unable
	// to answer a read.
	Sessions  runstate.Store
	Telemetry *telemetry.Provider
	// Provider, when set, is the model provider every run uses instead of one built
	// from the configuration. Resources supplies one when it is present, so this is for
	// a caller that has neither: a test with a scripted model, or a program that built
	// its own.
	Provider llm.Provider
	// TraceFile, HTTPDebugOut and Verbose are the debugging surfaces this terminal
	// asked for, which the run needs because the run is no longer in this process's
	// foreground.
	TraceFile    string
	HTTPDebugOut io.Writer
	Verbose      bool
	// Logger receives what the worker says. A terminal wants it quiet: the run's
	// narration arrives as events on the wire, and this is the machinery behind it.
	Logger *slog.Logger
	// WireLog, when set, receives every a2a message this client sends and reads. The
	// caller owns the file and the warning about what is in it.
	WireLog io.Writer
}

// hostAgent starts the broker, the prompts channel and the server, and returns once the
// agent is answering.
//
// The caller closes it.
func hostAgent(ctx context.Context, opts hostOptions) (*hostedAgent, error) {
	cfg, identity := hostedConfig(opts.Config)

	sessions := opts.Sessions
	if sessions == nil && opts.Resources != nil {
		sessions = opts.Resources.SessionStore
	}

	broker, err := embedded.Start("run "+identity, opts.Logger)
	if err != nil {
		return nil, err
	}

	// Only the channel is given the in-process connection. Everything else this run
	// touches keeps the connection its configuration named.
	endpoints, err := a2aendpoint.NewFromConfig(cfg, a2aendpoint.ConfigOptions{
		Conns:      broker.Conns(),
		ConfigFile: opts.ConfigFile,
		Logger:     opts.Logger,
		Telemetry:  opts.Telemetry,
		Sessions:   sessions,
	})
	if err != nil {
		broker.Close()

		return nil, err
	}

	// The synthesized exposure asks for prompts and nothing else, so what comes back is
	// the one channel.
	channels := make([]serve.Channel, 0, len(endpoints))
	for _, ep := range endpoints {
		ch, ok := ep.(serve.Channel)
		if !ok {
			continue
		}

		channels = append(channels, ch)
	}

	srvOpts := serve.Options{
		Channels:     channels,
		Config:       cfg,
		ConfigFile:   opts.ConfigFile,
		WorkDir:      opts.WorkDir,
		APIKey:       opts.APIKey,
		BaseURL:      opts.BaseURL,
		Telemetry:    opts.Telemetry,
		Logger:       opts.Logger,
		TraceFile:    opts.TraceFile,
		HTTPDebugOut: opts.HTTPDebugOut,
		Verbose:      opts.Verbose,
	}
	if opts.Resources != nil {
		opts.Resources.ApplyTo(&srvOpts)
		// ApplyTo hands over the configured connection for the stores and the tools this
		// run imports, which is right, and it must not become the channel's: the channel
		// is already built on the broker above.
	}
	if opts.Provider != nil {
		srvOpts.Provider = opts.Provider
	}
	// Set after ApplyTo so the run journals into the store the channel reads back. The
	// two are the same value wherever Resources supplied it; this is what makes them the
	// same value when nothing did.
	srvOpts.SessionStore = sessions

	srv, err := serve.New(srvOpts)
	if err != nil {
		broker.Close()

		return nil, err
	}

	transport, err := a2a.NewTransport(cfg.A2ATransport(), broker.Conns(), a2a.TransportConfig{
		Identity: identity,
		Logger:   opts.Logger,
	})
	if err != nil {
		broker.Close()

		return nil, err
	}

	// The caller's own identity on this bus. It reaches nobody outside the process, so
	// it says what it is rather than claiming to be anyone.
	client, err := a2a.NewClient(transport, clientSender, a2a.WithWireLog(opts.WireLog))
	if err != nil {
		broker.Close()

		return nil, err
	}

	host := &hostedAgent{
		broker:    broker,
		server:    srv,
		client:    client,
		identity:  identity,
		telemetry: opts.Telemetry,
		done:      make(chan error, 1),
	}

	go func() { host.done <- srv.Serve(ctx) }()

	return host, nil
}

// dialAgent reaches an agent somebody else is running, on the NATS context named.
//
// Nothing is hosted: no broker, no server, no model provider and no journal on this
// machine. What comes back is the same handle a hosted agent produces, so a client
// written against one talks to the other without knowing which it has.
func dialAgent(cfg *config.Config, natsContext string, logger *slog.Logger, wire io.Writer) (*hostedAgent, error) {
	provider, err := conns.Connect(natsContext, cfg.Identity)
	if err != nil {
		return nil, err
	}

	transport, err := a2a.NewTransport(cfg.A2ATransport(), provider, a2a.TransportConfig{
		Identity: clientSender,
		Timeout:  cfg.A2ARequestTimeout(),
		Logger:   logger,
	})
	if err != nil {
		provider.Close()

		return nil, err
	}

	client, err := a2a.NewClient(transport, clientSender, a2a.WithWireLog(wire))
	if err != nil {
		transport.Close()
		provider.Close()

		return nil, err
	}

	return &hostedAgent{
		conns:       provider,
		transport:   transport,
		client:      client,
		identity:    cfg.Identity,
		natsContext: natsContext,
	}, nil
}

// cardProbeWait bounds the one round trip a terminal makes before it draws anything.
//
// It is short and is not the request timeout, which bounds a turn and defaults to two
// minutes: a person waiting that long at a blank terminal before the screen opens would
// reasonably conclude the program had hung. An agent that has not answered in this long
// is one whose card the run proceeds without.
const cardProbeWait = 5 * time.Second

// probeAgent asks the agent what it is, before anything is drawn.
//
// An agent nothing answers for is fatal: the transport reports a no-responder in
// milliseconds, and that is proof the run cannot happen rather than a card that could
// not be filled in. Opening the full-screen view to send a prompt that fails the same
// way, and tearing it down again, tells a person less than an error naming what was
// addressed.
//
// Any other failure returns no card and no error. The agent is there and did not answer
// in time, which costs the caller what the card would have told it and nothing else.
func probeAgent(ctx context.Context, host *hostedAgent, natsContext string) (*a2a.AgentCard, error) {
	ctx, cancel := context.WithTimeout(ctx, cardProbeWait)
	defer cancel()

	card, err := host.client.Discover(ctx, host.identity)
	if err == nil {
		return card, nil
	}

	// Nothing listening on the subject at all, which is settled: the agent is not there
	// and the run cannot happen. A responder that exists and did not answer in time falls
	// through, since that agent is running and the card is all that was lost.
	if errors.Is(err, a2a.ErrNoResponders) {
		if natsContext != "" {
			return nil, fmt.Errorf("no agent answering as %q on NATS context %q: check the identity and that it is running there, which 'fisk discover %s' probes directly", host.identity, natsContext, host.identity)
		}

		return nil, fmt.Errorf("the agent hosted for this run is not answering as %q", host.identity)
	}

	return nil, nil
}

// Close releases whatever this handle holds, which differs by how it was made.
//
// A hosted agent is the run's own worker, so the interrupt contract a terminal has
// always had still holds: leaving takes it with you. A dialed one owns a connection and
// a transport and no run at all, so closing it detaches and leaves the work running
// wherever it is.
func (h *hostedAgent) Close() error {
	var err error

	if h.server != nil {
		err = h.server.Stop()
	}
	if h.broker != nil {
		h.broker.Close()
	}
	if h.transport != nil {
		closeErr := h.transport.Close()
		if err == nil {
			err = closeErr
		}
	}
	if h.conns != nil {
		h.conns.Close()
	}

	select {
	case serveErr := <-h.done:
		if err == nil {
			err = serveErr
		}
	default:
	}

	return err
}

// hostedConfig is the configuration the hosted agent runs under: this terminal's own,
// with the prompts channel switched on.
//
// It is synthesized rather than required. Nothing about running an agent at a terminal
// should have to be configured for a surface the operator never asked for, and an
// operator who did ask for one, by exposing tools or a queue, is not asking for it here:
// the exposure is replaced rather than added to, so hosting a run in this process
// registers no micro service and takes no jobs.
//
// The identity is the configured one where there is one, since it is what the journal
// name is derived from and changing it would put every stored conversation out of reach.
func hostedConfig(cfg *config.Config) (*config.Config, string) {
	hosted := *cfg

	identity := hosted.Identity
	if identity == "" {
		identity = localIdentity
		hosted.Identity = identity
	}

	hosted.Expose = &config.ExposeConfig{
		Agent: &config.AgentExpose{
			A2A: &config.ExposedA2AConfig{
				Prompts: &config.ExposedPromptsConfig{
					// One run at a time, which is what a terminal is.
					Workers: 1,
					// The person is right here, so the run asks them rather than
					// refusing every confirmation-gated tool.
					Elicit: true,
				},
			},
		},
	}

	return &hosted, identity
}
