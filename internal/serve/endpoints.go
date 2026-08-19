//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package serve

import (
	"fmt"
	"log/slog"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/conns"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/telemetry"
)

// Endpoint is one thing a server answers on. Every Channel and every Service is one,
// and a builder returns them under this name so a single configuration block can
// produce both.
//
// A returned value must implement Channel or Service. Endpoints sorts each one into the
// list it belongs in, taking Channel first: Service asks only for Name and Close, so a
// channel that can be released satisfies it too, and asking both questions would run
// one value twice.
type Endpoint interface {
	// Name identifies the endpoint in logs, metrics and on a program's startup banner.
	Name() string
}

// EndpointBuilder constructs the endpoints one configuration block asks for. A program
// links the builders for the endpoints it wants and passes them to Endpoints, which
// decides from the configuration which of them to call.
//
// It exists so this package never imports the packages that implement endpoints. A
// channel is a Go interface rather than a name in a configuration file, so an embedder
// adds one by writing a builder or by putting a value straight on Options.Channels, and
// neither route goes through a registry here.
type EndpointBuilder struct {
	// Name identifies the builder in diagnostics, not the endpoints it builds; each
	// endpoint names itself.
	Name string

	// Enabled reports whether this configuration asks for these endpoints. Presence of
	// a configuration block enables an endpoint, as it already does for serving. A nil
	// Enabled never builds, which lets a program link a builder it has not configured
	// a switch for.
	Enabled func(*config.Config) bool

	// Build constructs the endpoints, in the order they are to be run. It is called
	// only when Enabled said so. Returning none is allowed and runs nothing; a
	// service it returns is already answering by the time it arrives here.
	Build func(*config.Config, BuildOptions) ([]Endpoint, error)
}

// BuildOptions are what an endpoint needs that no configuration can state: what the
// process decided and what it is holding.
type BuildOptions struct {
	// Workers overrides a configured worker count when greater than zero, since the
	// number is a property of the process rather than of the agent.
	Workers int

	// SuspendRequested is handed to every run so a draining worker stops its runs
	// where they can be resumed from.
	SuspendRequested func() bool

	// Conns is the process's shared NATS connection, or nil when the configuration
	// needed none. It is borrowed: an endpoint may use it and must not close it, since
	// the stores and the runs are using the same one.
	//
	// An endpoint that needs a connection of its own dials it in its builder and owns it
	// from there. The queued-jobs channel does exactly that, because the queue engine
	// requires a connection option nothing else wants and a deployment may keep its
	// queue on another cluster.
	Conns *conns.Provider

	// ConfigFile names the file the configuration was read from, so a builder that
	// refuses can name the file to edit. Diagnostics only.
	ConfigFile string

	// Logger receives each endpoint's progress. Nil leaves each to its own default.
	Logger *slog.Logger

	// Telemetry is the process's provider, or nil when telemetry is off. An endpoint
	// that answers calls uses it to open a span per call; the channels do not need it,
	// since a run is handed one through the server's own options.
	Telemetry *telemetry.Provider

	// Sessions is the process's run-journal store, or nil when the caller built none.
	// It is borrowed like the connection: an endpoint may read it and must not close
	// it, since the runs write to the same one.
	//
	// A channel that only produces work never needs it, the run reaching the store
	// through the server. It is here for an endpoint that answers a caller about a
	// conversation without running one.
	Sessions runstate.Store
}

// Endpoints builds the endpoints a configuration enables, in the order the builders were
// given, and sorts them into the channels and the services a Server takes. It returns
// empty slices when nothing is enabled, which the caller reports rather than serving
// nothing.
//
// An endpoint that fails to build takes the ones already built down with it. Everything
// is built through one call rather than a call per kind because releasing them needs
// them together: a channel set built by one call and a service that failed in a second
// would leave a queue channel holding a NATS connection it dialed itself, with no
// handle left anywhere to release it.
func Endpoints(cfg *config.Config, opts BuildOptions, builders []EndpointBuilder) ([]Channel, []Service, error) {
	var (
		builtChannels []Channel
		builtServices []Service
	)

	fail := func(err error) ([]Channel, []Service, error) {
		releaseEndpoints(builtChannels, builtServices, opts.Logger)

		return nil, nil, err
	}

	for _, b := range builders {
		if b.Enabled == nil || !b.Enabled(cfg) {
			continue
		}

		built, err := b.Build(cfg, opts)
		if err != nil {
			return fail(fmt.Errorf("building the %s endpoint: %w", b.Name, err))
		}

		for _, s := range built {
			switch endpoint := s.(type) {
			case Channel:
				builtChannels = append(builtChannels, endpoint)
			case Service:
				builtServices = append(builtServices, endpoint)
			default:
				return fail(fmt.Errorf("the %s endpoint built %T, which is neither a Channel nor a Service", b.Name, s))
			}
		}
	}

	return builtChannels, builtServices, nil
}

// releaseEndpoints gives back what a partially built or refused set holds. A channel
// with nothing to release does not implement ReleasableChannel and is skipped; every
// service is closed, since Close is not optional there.
func releaseEndpoints(channels []Channel, services []Service, log *slog.Logger) {
	for _, ch := range channels {
		closer, ok := ch.(ReleasableChannel)
		if !ok {
			continue
		}

		err := closer.Close()
		if err != nil && log != nil {
			log.Error("Releasing a channel failed", "channel", ch.Name(), "error", err)
		}
	}

	for _, svc := range services {
		if svc == nil {
			continue
		}

		err := svc.Close()
		if err != nil && log != nil {
			log.Error("Releasing a service failed", "service", svc.Name(), "error", err)
		}
	}
}
