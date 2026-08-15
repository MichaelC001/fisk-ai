//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package serve

import (
	"fmt"
	"log/slog"

	"github.com/choria-io/fisk-ai/config"
)

// ChannelBuilder constructs one configured channel. A program links the builders for
// the surfaces it wants and passes them to Channels, which decides from the
// configuration which of them to call.
//
// It exists so this package never imports the packages that implement channels. A
// channel is a Go interface rather than a name in a configuration file, so an embedder
// adds one by writing a builder or by putting a value straight on Options.Channels, and
// neither route goes through a registry here.
type ChannelBuilder struct {
	// Name identifies the surface in diagnostics, not the channel it builds; the
	// channel names itself.
	Name string

	// Enabled reports whether this configuration asks for the surface. Presence of a
	// configuration block is what enables a surface, as it already is for serving.
	Enabled func(*config.Config) bool

	// Build constructs the channel. It is called only when Enabled said so.
	Build func(*config.Config, BuildOptions) (Channel, error)
}

// BuildOptions are what a channel needs that no configuration can state: what the
// process decided and what it is holding.
type BuildOptions struct {
	// Workers overrides a configured worker count when greater than zero, since the
	// number is a property of the process rather than of the agent.
	Workers int

	// SuspendRequested is handed to every run so a draining worker stops its runs
	// where they can be resumed from.
	SuspendRequested func() bool

	// Logger receives the channels' progress. Nil leaves each to its own default.
	Logger *slog.Logger
}

// Channels builds the channels a configuration enables, in the order the builders were
// given. It returns an empty slice when nothing is enabled, which the caller reports
// rather than serving nothing.
//
// A channel that fails to build takes the ones already built down with it: several of
// them hold connections, and returning an error while leaving those open would leak
// them somewhere the caller cannot reach.
func Channels(cfg *config.Config, builders []ChannelBuilder, opts BuildOptions) ([]Channel, error) {
	var built []Channel

	for _, b := range builders {
		if b.Enabled == nil || !b.Enabled(cfg) {
			continue
		}

		ch, err := b.Build(cfg, opts)
		if err != nil {
			closeChannels(built, opts.Logger)
			return nil, fmt.Errorf("building the %s channel: %w", b.Name, err)
		}

		built = append(built, ch)
	}

	return built, nil
}

// closeChannels releases channels built before one of their siblings failed. A channel
// with nothing to release does not implement ReleasableChannel and is skipped.
func closeChannels(channels []Channel, log *slog.Logger) {
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
}
