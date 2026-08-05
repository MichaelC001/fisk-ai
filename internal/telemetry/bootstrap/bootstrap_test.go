//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/telemetry"
)

var _ = Describe("SettingsFrom", func() {
	// The mapping is by hand because internal/telemetry imports nothing from this tree,
	// so nothing but a spec catches a field that stops being carried across. A dropped
	// endpoint or ratio does not fail anything; it silently exports to the wrong place,
	// or exports everything to a paid backend.
	It("should carry every telemetry field across", func() {
		ratio := 0.25
		cfg := &config.Config{
			Identity: "demo",
			Telemetry: config.TelemetryConfig{
				Enabled:     true,
				Endpoint:    "http://127.0.0.1:4318",
				ServiceName: "svc",
				SampleRatio: &ratio,
				NoMetrics:   true,
			},
		}

		settings := SettingsFrom(cfg, "")
		Expect(settings.Enabled).To(BeTrue())
		Expect(settings.Endpoint).To(Equal("http://127.0.0.1:4318"))
		Expect(settings.ServiceName).To(Equal("svc"))
		Expect(settings.SampleRatio).To(Equal(&ratio))
		Expect(settings.NoMetrics).To(BeTrue())
		Expect(settings.Identity).To(Equal("demo"))
		Expect(settings.DisabledBy).To(BeEmpty())
	})

	// An explicit zero must survive the mapping as a pointer. Copying it through a
	// float64 anywhere on this path would collapse it to "unset" and turn "sample
	// nothing" into "sample everything".
	It("should carry an explicit zero sample ratio as a pointer", func() {
		zero := 0.0
		cfg := &config.Config{Telemetry: config.TelemetryConfig{Enabled: true, SampleRatio: &zero}}

		settings := SettingsFrom(cfg, "")
		Expect(settings.SampleRatio).ToNot(BeNil())
		Expect(*settings.SampleRatio).To(Equal(0.0))
	})

	It("should carry the veto label", func() {
		cfg := &config.Config{Telemetry: config.TelemetryConfig{Enabled: true}}

		Expect(SettingsFrom(cfg, "--no-telemetry").DisabledBy).To(Equal("--no-telemetry"))
	})

	It("should carry the capture block", func() {
		cfg := &config.Config{Telemetry: config.TelemetryConfig{
			Enabled: true,
			Capture: &config.TelemetryCaptureConfig{Enabled: true, Messages: telemetry.MessagesFull.String(), MaxBytes: 4096},
		}}

		settings := SettingsFrom(cfg, "")
		Expect(settings.Capture.Enabled).To(BeTrue())
		Expect(settings.Capture.Messages).To(Equal(telemetry.MessagesFull))
		Expect(settings.Capture.MaxBytes).To(Equal(4096))
	})
})

var _ = Describe("Start", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("should require a config rather than dereference nil", func() {
		_, err := Start(ctx, Options{})
		Expect(err).To(MatchError(ContainSubstring("requires a config")))
	})

	// Off is not an error, and the returned value is still usable: a caller that has to
	// nil-check before reading Resolved or calling Close is a caller who will forget.
	It("should return a usable Telemetry with no provider when telemetry is off", func() {
		tel, err := Start(ctx, Options{Config: &config.Config{Identity: "demo"}, Env: noEnv})
		Expect(err).ToNot(HaveOccurred())
		Expect(tel.Provider).To(BeNil())
		Expect(tel.Resolved.Enabled).To(BeFalse())
		Expect(tel.Resolved.NotEnabled).To(BeTrue())

		delivery, err := tel.Close()
		Expect(err).ToNot(HaveOccurred())
		Expect(delivery.Attempted()).To(BeFalse())
	})

	It("should report a veto as the label the caller supplied", func() {
		tel, err := Start(ctx, Options{
			Config:     &config.Config{Identity: "demo", Telemetry: config.TelemetryConfig{Enabled: true}},
			Env:        noEnv,
			DisabledBy: "--no-telemetry",
		})
		Expect(err).ToNot(HaveOccurred())
		Expect(tel.Provider).To(BeNil())
		Expect(tel.Resolved.DisabledBy).To(Equal("--no-telemetry"))
		Expect(tel.Resolved.NotEnabled).To(BeFalse())
	})

	// A misconfiguration fails at startup naming the fix, the same contract as every
	// other setting in this tree. OTLP being connectionless is a reason to validate what
	// is knowable locally, not a reason to validate nothing.
	It("should reject a misconfigured endpoint", func() {
		_, err := Start(ctx, Options{
			Config: &config.Config{
				Identity:  "demo",
				Telemetry: config.TelemetryConfig{Enabled: true, Endpoint: "http://127.0.0.1:4317"},
			},
			Env: noEnv,
		})
		Expect(err).To(MatchError(ContainSubstring("is the OTLP/gRPC port")))
	})

	It("should start the pipelines for a valid enabled config", func() {
		tel, err := Start(ctx, Options{
			Config: &config.Config{
				Identity:  "demo",
				Telemetry: config.TelemetryConfig{Enabled: true, Endpoint: "http://127.0.0.1:4318"},
			},
			Env: noEnv,
		})
		Expect(err).ToNot(HaveOccurred())
		Expect(tel.Provider.Enabled()).To(BeTrue())

		// Nothing was recorded, so this flushes an empty pipeline against an endpoint
		// with no collector behind it and must still return promptly and quietly.
		delivery, err := tel.Close()
		Expect(err).ToNot(HaveOccurred())
		Expect(delivery.Attempted()).To(BeFalse())
	})

	// A nil Env means no environment, matching Resolve. Defaulting it to os.Getenv would
	// make every spec in this package read the developer's shell.
	It("should treat a nil Env as no environment", func() {
		tel, err := Start(ctx, Options{Config: &config.Config{Identity: "demo"}})
		Expect(err).ToNot(HaveOccurred())
		Expect(tel.Resolved.TransportEnvSet).To(BeEmpty())
	})

	It("should read transport variables through the supplied Env", func() {
		tel, err := Start(ctx, Options{
			Config: &config.Config{Identity: "demo"},
			Env:    envWith(map[string]string{telemetry.EnvEndpoint: "http://collector:4318"}),
		})
		Expect(err).ToNot(HaveOccurred())
		Expect(tel.Resolved.TransportEnvSet).To(ContainElement(telemetry.EnvEndpoint))
	})

	// The export error handler is per Provider rather than the process global, which is
	// what lets several agents in one process each report their own failures. Asserting
	// it needs a real refused export, since nothing else drives the exporter.
	It("should report export failures to the handler the caller supplied", func() {
		refused := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		DeferCleanup(refused.Close)

		var buf telemetry.ErrorBuffer

		tel, err := Start(ctx, Options{
			Config: &config.Config{
				Identity:  "demo",
				Telemetry: config.TelemetryConfig{Enabled: true, Endpoint: refused.URL},
			},
			Env:          noEnv,
			ExportErrors: func(err error) { fmt.Fprintln(&buf, err) },
		})
		Expect(err).ToNot(HaveOccurred())

		_, span := tel.Provider.StartStartup(ctx, telemetry.StartupInfo{Identity: "demo"})
		span.End()

		delivery, err := tel.Close()
		Expect(err).ToNot(HaveOccurred())

		// The counts are what a caller must branch on: shutdown returns no error for a
		// pipeline that was refused every span.
		Expect(delivery.Attempted()).To(BeTrue())
		Expect(delivery.Complete()).To(BeFalse())
		Expect(delivery.Err).To(HaveOccurred())

		Expect(buf.Count()).To(BeNumerically(">", 0))

		out := &strings.Builder{}
		_, err = buf.WriteTo(out)
		Expect(err).ToNot(HaveOccurred())
		Expect(out.String()).To(ContainSubstring("401"))
	})

	It("should start without a handler when the caller supplied none", func() {
		tel, err := Start(ctx, Options{
			Config: &config.Config{
				Identity:  "demo",
				Telemetry: config.TelemetryConfig{Enabled: true, Endpoint: "http://127.0.0.1:4318"},
			},
			Env: noEnv,
		})
		Expect(err).ToNot(HaveOccurred())

		_, err = tel.Close()
		Expect(err).ToNot(HaveOccurred())
	})
})

var _ = Describe("Telemetry.Close", func() {
	It("should be safe on a nil receiver", func() {
		var tel *Telemetry

		delivery, err := tel.Close()
		Expect(err).ToNot(HaveOccurred())
		Expect(delivery.Attempted()).To(BeFalse())
	})

	// Close builds its own context precisely so an interrupted run still flushes. Were
	// it derived from the run's, a Ctrl-C would cancel the flush and discard exactly the
	// trace the operator went looking for. Verified by deriving it: the delivery then
	// reports nothing attempted.
	It("should flush after the run's context is canceled", func() {
		collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		DeferCleanup(collector.Close)

		runCtx, cancel := context.WithCancel(context.Background())

		tel, err := Start(runCtx, Options{
			Config: &config.Config{
				Identity:  "demo",
				Telemetry: config.TelemetryConfig{Enabled: true, Endpoint: collector.URL},
			},
			Env: noEnv,
		})
		Expect(err).ToNot(HaveOccurred())

		_, span := tel.Provider.StartStartup(runCtx, telemetry.StartupInfo{Identity: "demo"})
		span.End()

		// The run is interrupted, which is the case this exists for.
		cancel()

		delivery, err := tel.Close()
		Expect(err).ToNot(HaveOccurred())
		Expect(errors.Is(err, context.Canceled)).To(BeFalse())
		Expect(delivery.SpansAttempted).To(BeNumerically(">", 0))
		Expect(delivery.SpansDelivered).To(Equal(delivery.SpansAttempted))
	})
})
