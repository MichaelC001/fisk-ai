//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// Package bootstrap starts and stops telemetry export for a program holding a
// fisk-ai configuration.
//
// It is a subpackage rather than part of internal/telemetry because that package is a
// hard leaf: it imports the standard library and OpenTelemetry and nothing else from
// this repository, so that no future instrumentation can create an import cycle. This
// one needs config, so it lives one level down where importing both is allowed, on the
// precedent of internal/telemetry/genai. Nothing here imports OpenTelemetry.
//
// What it is for is a ratio. Building an agent programmatically takes about a dozen
// lines; exporting its telemetry took about sixty more, and four of the five rules
// those lines encoded failed silently when broken. Start owns all five, so a caller
// writes one call and a deferred close:
//
//	tel, err := bootstrap.Start(ctx, bootstrap.Options{Config: cfg, Version: v, Env: os.Getenv})
//	if err != nil {
//		return err
//	}
//	defer tel.Close()
//
//	opts.Telemetry = tel.Provider
//
// What it deliberately does not own is presentation. Where messages go, whether a
// success line is printed at all, and what any of it says are the caller's, because
// those are the parts that differ between a CLI, a server and an embedder. Start takes
// destinations as parameters and returns values; it prints nothing.
//
// One thing it cannot do for a caller: OpenTelemetry hands export failures to a
// process-global handler as well as to this package's per-provider one, and with
// nothing installed the default writes through the log package's captured os.Stderr. A
// program that owns a terminal must call telemetry.SetErrorHandler itself. That is a
// process-wide decision and this package takes no process-wide decisions.
package bootstrap

import (
	"context"
	"fmt"
	"time"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/telemetry"
)

// FlushTimeout bounds the final flush.
//
// It is generous because the alternative to waiting is discarding the run's whole
// trace, and it is bounded because a collector that has gone away must not hold a
// program open.
const FlushTimeout = 10 * time.Second

// Options configures Start.
//
// It is a struct rather than a set of With functions because telemetry.Option already
// exists, for NewFromProviders, and two option types whose constructors look identical
// at a call site while not being interchangeable is the API mistake most likely to be
// regretted once these packages are public.
type Options struct {
	// Config is the agent configuration whose telemetry block is resolved. Required.
	Config *config.Config

	// Version becomes service.version on the resource, where a version belongs, rather
	// than an attribute repeated on every span. It is the caller's own version: a
	// program embedding these libraries is not fisk-ai and should not report itself as
	// fisk-ai's build.
	Version string

	// Env reads one environment variable. A nil Env means no environment at all, which
	// matches telemetry.Resolve and is what lets a spec resolve a configuration without
	// reading the developer's shell. A program that wants the standard OTEL_* handling
	// passes os.Getenv.
	//
	// It is not defaulted, and the omission is deliberate. With no environment the
	// resolved endpoint falls back to the local default and is not passed to the
	// exporters, which leaves the SDK reading OTEL_EXPORTER_OTLP_ENDPOINT on its own:
	// validation would then have run against one endpoint while export went to another,
	// silently skipping the checks that keep a credential and the conversation off a
	// cleartext connection to a remote host. Passing nil is safe only for a caller that
	// means it.
	Env func(string) string

	// DisabledBy labels a last-minute veto and is empty when there is none. It is a
	// label rather than a bool because a library must not name a switch it does not own,
	// and because an operator who sees export turned off needs to know which switch did
	// it. A CLI passes its flag name here.
	DisabledBy string

	// ExportErrors receives each export failure as it happens, and may be nil.
	//
	// It is called from the exporter's goroutine, once per failed export, with a lock
	// held, so it must be safe for concurrent use and must not call back into the
	// provider. telemetry.ErrorBuffer satisfies both and is the destination for a caller
	// that cannot be written to at an arbitrary moment, such as one holding a
	// full-screen display.
	ExportErrors func(error)
}

// Telemetry is a started export pipeline together with the configuration it was
// started from.
//
// Start always returns one on success, including for a configuration that resolves to
// off, so a caller never branches on nil to find out whether it may read Resolved or
// call Close.
type Telemetry struct {
	// Provider is the handle to give a run, for example agent.Options.Telemetry. It is
	// nil when the configuration resolved to off, which every call site treats as a
	// no-op rather than as an error.
	Provider *telemetry.Provider

	// Resolved is the effective configuration, each value paired with the config key or
	// environment variable that decided it, so a caller can report not just what
	// telemetry will do but why.
	Resolved telemetry.Resolved
}

// Start resolves the telemetry configuration and, when it is on, starts the export
// pipelines.
//
// A configuration that resolves to off is not an error: it returns a Telemetry whose
// Provider is nil and whose Close does nothing. An invalid configuration is an error,
// because a caller that fails at startup naming the fix is the philosophy everywhere
// else in this tree, and OTLP being connectionless is a reason to validate what is
// knowable locally rather than a reason to validate nothing.
func Start(ctx context.Context, o Options) (*Telemetry, error) {
	if o.Config == nil {
		return nil, fmt.Errorf("telemetry bootstrap requires a config")
	}

	resolved, err := telemetry.Resolve(SettingsFrom(o.Config, o.DisabledBy), o.Env)
	if err != nil {
		return nil, err
	}

	t := &Telemetry{Resolved: resolved}
	if !resolved.Enabled {
		return t, nil
	}

	var opts []telemetry.Option
	if o.ExportErrors != nil {
		opts = append(opts, telemetry.WithExportErrorHandler(o.ExportErrors))
	}

	t.Provider, err = telemetry.Setup(ctx, resolved, o.Version, opts...)
	if err != nil {
		return nil, err
	}

	return t, nil
}

// Close flushes and stops the pipelines and reports what actually reached the
// collector.
//
// It builds its own context with its own timeout and never derives one from the
// caller's. That is the reason this is a method here rather than left to the caller:
// deriving it would mean an interrupt cancels the flush and discards exactly the run
// worth looking at, and as a rule in a doc comment that survives only as long as the
// next caller remembers it.
//
// A caller that checks only the returned error learns nothing about export. OTLP is
// fire and forget over HTTP and the SDK reports export failures to its own handler
// rather than to the caller, so a run whose every span was refused with a 401 shuts
// down with a nil error here. The returned Delivery is what that silence hides; check
// Delivery.Complete.
func (t *Telemetry) Close() (telemetry.Delivery, error) {
	if t == nil || t.Provider == nil {
		return telemetry.Delivery{}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), FlushTimeout)
	defer cancel()

	return t.Provider.Shutdown(ctx)
}

// SettingsFrom maps a fisk-ai configuration onto the settings the telemetry package
// resolves.
//
// It is the whole mapping, done by hand: internal/telemetry imports nothing from this
// tree, so the config type never crosses into it. Nothing but a spec catches a field
// that stops being carried across, because a dropped field does not fail anything. It
// silently exports to the wrong place, or exports the conversation to a backend that
// was not meant to have it.
//
// disabledBy labels a last-minute veto, empty when there is none. What a caller needs
// to suppress at the last minute is export, not configuration, which is why it is a
// parameter here rather than something read out of the config block.
func SettingsFrom(cfg *config.Config, disabledBy string) telemetry.Settings {
	return telemetry.Settings{
		Enabled:     cfg.TelemetryEnabled(),
		Endpoint:    cfg.Telemetry.Endpoint,
		ServiceName: cfg.Telemetry.ServiceName,
		SampleRatio: cfg.Telemetry.SampleRatio,
		NoMetrics:   cfg.Telemetry.NoMetrics,
		Capture: telemetry.CaptureSettings{
			Enabled:  cfg.TelemetryCaptureEnabled(),
			Messages: telemetry.ParseMessagesMode(cfg.TelemetryCaptureMessages()),
			MaxBytes: cfg.TelemetryCaptureMaxBytes(),
		},
		Identity:   cfg.Identity,
		DisabledBy: disabledBy,
	}
}
