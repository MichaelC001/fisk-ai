//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/telemetry"
	"github.com/choria-io/fisk-ai/internal/telemetry/bootstrap"
	"github.com/choria-io/fisk-ai/internal/util"
)

// noTelemetryFlag is the label a veto by --no-telemetry is reported as. It is stated
// once here because it is what an operator sees in fisk info and in the startup note,
// and the library deliberately does not know this command's flag names.
const noTelemetryFlag = "--no-telemetry"

// noTelemetryLabel is the veto label, empty when the flag was not given.
func noTelemetryLabel(disabled bool) string {
	if !disabled {
		return ""
	}

	return noTelemetryFlag
}

// telemetrySetup is what resolving telemetry needs from the command doing it. Each
// command owns its own flags, so these are passed rather than read off the package:
// serve registers its own --no-telemetry and --verbose and reads a config file it was
// given, and none of those are the run command's variables.
type telemetrySetup struct {
	// ConfigFile names the file the configuration came from, for the note that says
	// nothing will be exported.
	ConfigFile string

	// TUI reports whether this command will render in the full-screen UI, which decides
	// only where OpenTelemetry's own diagnostics go: collected for the end, or straight
	// out as they happen. A long-lived process wants them as they happen, since "the
	// end" may be weeks away.
	TUI bool

	// Disabled is the --no-telemetry veto.
	Disabled bool

	// Verbose reports the delivery counts on a successful export. It is read when the
	// report runs rather than here, which is why it travels in this struct rather than
	// being captured from a flag variable at call time.
	Verbose bool
}

// telemetryErrorSink chooses where OpenTelemetry's diagnostics go, returning the writer
// to install and the buffer to drain afterwards, or a nil buffer when there is nothing
// to drain.
//
// It is a function of its own so the choice can be asserted. Getting it backwards is
// not a visible failure: errors would go to stderr under the full-screen UI, where the
// next frame paints over them, so the operator sees a flicker rather than a message and
// nothing reports that anything was lost.
func telemetryErrorSink(tui bool) (io.Writer, *telemetry.ErrorBuffer) {
	if !tui {
		return os.Stderr, nil
	}

	buf := &telemetry.ErrorBuffer{}

	return buf, buf
}

// setupTelemetry resolves the telemetry configuration and, when it is on, starts the
// export pipelines. It returns the provider to hand to the run and a function to call
// when the run is over, which flushes and reports what was delivered.
//
// A run that asks for the UI and then falls back to the line renderer still gets the
// diagnostics, just at the end, which is a better outcome than losing them.
//
// A configuration that resolves to off returns a nil provider, which every call site
// already treats as a no-op, and a report function that does nothing. An invalid
// configuration is an error: the philosophy everywhere else in this tree is that an
// operator mistake fails at startup with a message naming the fix, and OTLP being
// connectionless is a reason to validate what is knowable locally, not to validate
// nothing.
func setupTelemetry(cfg *config.Config, opts telemetrySetup) (*telemetry.Provider, func(), error) {
	noop := func() {}

	// OpenTelemetry hands its own diagnostics to a process-global destination, so this
	// is the one global this feature sets, and it is installed before anything can
	// export. It goes to stderr because that is this command's diagnostic channel and
	// stdout carries the agent's answer; a command speaking a protocol on stdout would
	// have to point it somewhere else.
	//
	// It stays the global rather than moving to the per-provider handler, and that is a
	// decision rather than an omission. The SDK hands export failures to the global one
	// whether or not a per-provider handler exists, so installing both would report every
	// failure twice, and installing neither lets the default write through the log
	// package's captured os.Stderr, which muzzleStderr cannot redirect and the next frame
	// paints over. A terminal-owning command wants exactly this one.
	errOut, collected := telemetryErrorSink(opts.TUI)
	telemetry.SetErrorHandler(errOut)

	tel, err := bootstrap.Start(context.Background(), bootstrap.Options{
		Config:     cfg,
		Version:    util.Version(),
		Env:        os.Getenv,
		DisabledBy: noTelemetryLabel(opts.Disabled),
	})
	if err != nil {
		return nil, noop, err
	}

	if !tel.Resolved.Enabled {
		note := telemetryOffNote(tel.Resolved, opts.ConfigFile)
		if note != "" {
			fmt.Fprintln(os.Stderr, note)
		}
		return nil, noop, nil
	}

	return tel.Provider, func() { reportTelemetryOutcome(tel, collected, opts.Verbose) }, nil
}

// reportTelemetryOutcome flushes the pipelines and says what happened, in the order an
// operator reads it: what went wrong during the run, then what went wrong ending it,
// then the summary of what arrived.
//
// The drain happens after the flush rather than before, which is not cosmetic. The
// batch processor exports on a five second timer, so a short run's spans go out in the
// final drain, and a buffer read before the flush is a buffer read before the failures
// that matter have happened. Under the full-screen UI those lines then sat in a buffer
// nobody looked at again.
//
// Some overlap with the delivery line is expected and is not duplication: these say how
// often export failed, that says how much was lost, and an error reported here that
// leaves the counts intact is one that never reached an exporter at all.
func reportTelemetryOutcome(tel *bootstrap.Telemetry, collected *telemetry.ErrorBuffer, verbose bool) {
	delivery, err := tel.Close()

	if collected != nil {
		_, writeErr := collected.WriteTo(os.Stderr)
		if writeErr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not report telemetry errors: %v\n", writeErr)
		}
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: telemetry shutdown failed: %v\n", err)
	}

	// A run that recorded nothing is not a failure; it had nothing to say.
	if !delivery.Attempted() {
		return
	}

	if !delivery.Complete() {
		fmt.Fprintf(os.Stderr, "warning: telemetry export to %s failed%s; %s delivered\n",
			delivery.Endpoint, telemetryFailureDetail(delivery), telemetryDeliveryCounts(delivery))
		return
	}

	if verbose {
		fmt.Fprintf(os.Stderr, "telemetry: delivered %s to %s\n",
			telemetryDeliveryCounts(delivery), delivery.Endpoint)
	}
}

// telemetryOffNote is the note printed when transport is configured while nothing
// will be exported, or "" when there is nothing to say.
//
// An operator who set a collector endpoint host-wide and then sees no traces has no
// way to tell a broken pipeline from a run that never enabled one. The rule that
// causes it, that OTEL_* alone never turns export on, is deliberate: a host-wide
// endpoint must not silently make every fisk process on the box an exporter. A
// deliberate surprise still needs saying out loud.
//
// It names which variables were seen and which switch is responsible, because those
// are the two things the operator needs and neither is guessable from silence.
func telemetryOffNote(resolved telemetry.Resolved, cfgFile string) string {
	// A run that is exporting has nothing to apologize for, and the transport variables
	// are set on exactly the runs that work: OTEL_EXPORTER_OTLP_HEADERS is how the docs
	// tell an operator to reach a hosted collector. Without this the note would fire on
	// every successful run of that setup, and read as "disabled by " with nothing after
	// it, since nothing disabled anything.
	if resolved.Enabled {
		return ""
	}

	if len(resolved.TransportEnvSet) == 0 {
		return ""
	}

	// The config key and the file it lives in are this command's vocabulary, so the
	// library reports that nothing enabled telemetry and the sentence is written here.
	reason := fmt.Sprintf("telemetry is disabled by %s", resolved.DisabledBy)
	if resolved.NotEnabled {
		reason = fmt.Sprintf("telemetry.enabled is false in %q", cfgFile)
	}

	return fmt.Sprintf("note: %s is set but %s; nothing is exported",
		strings.Join(resolved.TransportEnvSet, ", "), reason)
}

// telemetryDeliveryCounts renders what was delivered, naming only the signals that
// were actually attempted.
//
// Reporting both unconditionally produces lines like "0 of 0 spans delivered" on a run
// where only the metric export failed, which reads as a span problem and sends the
// operator looking in the wrong place.
func telemetryDeliveryCounts(delivery telemetry.Delivery) string {
	var parts []string

	if delivery.SpansAttempted > 0 {
		parts = append(parts, fmt.Sprintf("%d of %d spans", delivery.SpansDelivered, delivery.SpansAttempted))
	}
	if delivery.MetricExportsAttempted > 0 {
		parts = append(parts, fmt.Sprintf("%d of %d metric batches", delivery.MetricExportsDelivered, delivery.MetricExportsAttempted))
	}

	return strings.Join(parts, ", ")
}

// telemetryFailureDetail renders the first export error for the warning line, or
// nothing when the counts fall short with no error to name.
func telemetryFailureDetail(delivery telemetry.Delivery) string {
	if delivery.Err == nil {
		return ""
	}

	return ": " + delivery.Err.Error()
}
