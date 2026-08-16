//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package serve

import (
	"fmt"
	"log/slog"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/conns"
	"github.com/choria-io/fisk-ai/internal/telemetry"
)

// Surface is a calling surface a server hosts. Every Channel and every Service is one,
// and a builder returns them under this name so a single configuration block can
// produce both.
//
// A returned value must implement Channel or Service. Surfaces sorts each one into the
// list it belongs in, taking Channel first: Service asks only for Name and Close, so a
// channel that can be released satisfies it too, and asking both questions would host
// one value twice.
type Surface interface {
	// Name identifies the surface in logs, metrics and on a program's startup banner.
	Name() string
}

// SurfaceBuilder constructs the surfaces one configuration block asks for. A program
// links the builders for the surfaces it wants and passes them to Surfaces, which
// decides from the configuration which of them to call.
//
// It exists so this package never imports the packages that implement surfaces. A
// channel is a Go interface rather than a name in a configuration file, so an embedder
// adds one by writing a builder or by putting a value straight on Options.Channels, and
// neither route goes through a registry here.
type SurfaceBuilder struct {
	// Name identifies the builder in diagnostics, not the surfaces it builds; each
	// surface names itself.
	Name string

	// Enabled reports whether this configuration asks for these surfaces. Presence of
	// a configuration block enables a surface, as it already does for serving. A nil
	// Enabled never builds, which lets a program link a builder it has not configured
	// a switch for.
	Enabled func(*config.Config) bool

	// Build constructs the surfaces, in the order they are to be hosted. It is called
	// only when Enabled said so. Returning none is allowed and hosts nothing; a
	// service it returns is already answering by the time it arrives here.
	Build func(*config.Config, BuildOptions) ([]Surface, error)
}

// BuildOptions are what a surface needs that no configuration can state: what the
// process decided and what it is holding.
type BuildOptions struct {
	// Workers overrides a configured worker count when greater than zero, since the
	// number is a property of the process rather than of the agent.
	Workers int

	// SuspendRequested is handed to every run so a draining worker stops its runs
	// where they can be resumed from.
	SuspendRequested func() bool

	// Conns is the process's shared NATS connection, or nil when the configuration
	// needed none. It is borrowed: a surface may use it and must not close it, since
	// the stores and the runs are using the same one.
	//
	// A surface that needs a connection of its own dials it in its builder and owns it
	// from there. The queued-jobs channel does exactly that, because the queue engine
	// requires a connection option nothing else wants and a deployment may keep its
	// queue on another cluster.
	Conns *conns.Provider

	// ConfigFile names the file the configuration was read from, so a builder that
	// refuses can name the file to edit. Diagnostics only.
	ConfigFile string

	// Logger receives the surfaces' progress. Nil leaves each to its own default.
	Logger *slog.Logger

	// Telemetry is the process's provider, or nil when telemetry is off. A surface
	// that answers calls uses it to open a span per call; the channels do not need it,
	// since a run is handed one through the server's own options.
	Telemetry *telemetry.Provider
}

// Surfaces builds the surfaces a configuration enables, in the order the builders were
// given, and sorts them into the channels and the services a Server takes. It returns
// empty slices when nothing is enabled, which the caller reports rather than serving
// nothing.
//
// A surface that fails to build takes the ones already built down with it. Everything
// is built through one call rather than a call per kind because releasing them needs
// them together: a channel set built by one call and a service that failed in a second
// would leave a queue channel holding a NATS connection it dialed itself, with no
// handle left anywhere to release it.
func Surfaces(cfg *config.Config, opts BuildOptions, builders []SurfaceBuilder) ([]Channel, []Service, error) {
	var (
		builtChannels []Channel
		builtServices []Service
	)

	fail := func(err error) ([]Channel, []Service, error) {
		releaseSurfaces(builtChannels, builtServices, opts.Logger)

		return nil, nil, err
	}

	for _, b := range builders {
		if b.Enabled == nil || !b.Enabled(cfg) {
			continue
		}

		built, err := b.Build(cfg, opts)
		if err != nil {
			return fail(fmt.Errorf("building the %s surface: %w", b.Name, err))
		}

		for _, s := range built {
			switch surface := s.(type) {
			case Channel:
				builtChannels = append(builtChannels, surface)
			case Service:
				builtServices = append(builtServices, surface)
			default:
				return fail(fmt.Errorf("the %s surface built %T, which is neither a Channel nor a Service", b.Name, s))
			}
		}
	}

	return builtChannels, builtServices, nil
}

// releaseSurfaces gives back what a partially built or refused set holds. A channel
// with nothing to release does not implement ReleasableChannel and is skipped; every
// service is closed, since Close is not optional there.
func releaseSurfaces(channels []Channel, services []Service, log *slog.Logger) {
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
