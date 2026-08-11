//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/telemetry"
)

var _ = Describe("telemetryOffNote", func() {
	It("Should say nothing when no transport variable is set", func() {
		resolved, err := telemetry.Resolve(telemetry.Settings{}, func(string) string { return "" })
		Expect(err).ToNot(HaveOccurred())

		Expect(telemetryOffNote(resolved, "agent.yaml")).To(BeEmpty())
	})

	It("Should name the config key when the file never enabled telemetry", func() {
		env := func(name string) string {
			if name == telemetry.EnvEndpoint {
				return "http://collector:4318"
			}
			return ""
		}

		resolved, err := telemetry.Resolve(telemetry.Settings{}, env)
		Expect(err).ToNot(HaveOccurred())

		note := telemetryOffNote(resolved, "agent.yaml")
		Expect(note).To(ContainSubstring("OTEL_EXPORTER_OTLP_ENDPOINT is set"))
		Expect(note).To(ContainSubstring(`telemetry.enabled is false in "agent.yaml"`))
		Expect(note).To(ContainSubstring("nothing is exported"))
	})

	// Naming the veto rather than the config key matters here: an operator who enabled
	// telemetry in the file and then sees nothing exported is looking at a different
	// problem from one who never enabled it, and only the note can tell them which.
	It("Should name the veto when a switch turned telemetry off", func() {
		env := func(name string) string {
			if name == telemetry.EnvEndpoint {
				return "http://collector:4318"
			}
			return ""
		}

		resolved, err := telemetry.Resolve(telemetry.Settings{Enabled: true, DisabledBy: noTelemetryFlag}, env)
		Expect(err).ToNot(HaveOccurred())

		note := telemetryOffNote(resolved, "agent.yaml")
		Expect(note).To(ContainSubstring("disabled by --no-telemetry"))
		Expect(note).ToNot(ContainSubstring("telemetry.enabled is false"))
	})

	It("Should list every transport variable it found", func() {
		env := func(name string) string {
			switch name {
			case telemetry.EnvEndpoint:
				return "http://collector:4318"
			case "OTEL_EXPORTER_OTLP_HEADERS":
				return "authorization=Bearer t"
			default:
				return ""
			}
		}

		resolved, err := telemetry.Resolve(telemetry.Settings{}, env)
		Expect(err).ToNot(HaveOccurred())

		note := telemetryOffNote(resolved, "agent.yaml")
		Expect(note).To(ContainSubstring("OTEL_EXPORTER_OTLP_ENDPOINT"))
		Expect(note).To(ContainSubstring("OTEL_EXPORTER_OTLP_HEADERS"))
	})

	// The note reads the transport variables, which are set on exactly the runs that
	// work: pointing OTEL_EXPORTER_OTLP_HEADERS at a hosted collector is what the docs
	// tell an operator to do. Without the enabled check this fires on every successful
	// run of that setup and reads as "disabled by " with nothing after it, because
	// nothing disabled anything.
	It("Should say nothing on a working run that configured transport in the environment", func() {
		env := func(name string) string {
			if name == "OTEL_EXPORTER_OTLP_HEADERS" {
				return "authorization=Bearer t"
			}
			return ""
		}

		resolved, err := telemetry.Resolve(telemetry.Settings{
			Enabled:  true,
			Endpoint: "https://otel.example.net:4318",
		}, env)
		Expect(err).ToNot(HaveOccurred())
		Expect(resolved.Enabled).To(BeTrue())
		Expect(resolved.TransportEnvSet).ToNot(BeEmpty())

		Expect(telemetryOffNote(resolved, "agent.yaml")).To(BeEmpty())
	})
})

// Where OpenTelemetry's diagnostics go is the CLI's decision because only the CLI knows
// what is on the terminal. Backwards, this fails invisibly: the errors go to stderr
// under the full-screen UI, the next frame paints over them, and the operator sees a
// flicker rather than a message with nothing reporting that anything was lost.
var _ = Describe("telemetryErrorSink", func() {
	It("Should collect under the full-screen UI and write straight out otherwise", func() {
		w, collected := telemetryErrorSink(true)
		Expect(collected).ToNot(BeNil())
		Expect(w).To(BeIdenticalTo(collected))

		w, collected = telemetryErrorSink(false)
		Expect(collected).To(BeNil())
		Expect(w).To(BeIdenticalTo(os.Stderr))
	})
})

var _ = Describe("setupTelemetry", func() {
	It("Should build nothing for a config that never enabled it", func() {
		provider, report, err := setupTelemetry(&config.Config{Identity: "demo"}, telemetrySetup{})
		Expect(err).ToNot(HaveOccurred())
		Expect(provider).To(BeNil())
		Expect(report).ToNot(BeNil())

		// The report function is called from a defer on every exit path, so it has to be
		// safe when telemetry never started.
		Expect(report).ToNot(Panic())
	})

	It("Should build nothing when --no-telemetry vetoes an enabled config", func() {
		provider, _, err := setupTelemetry(&config.Config{
			Identity:  "demo",
			Telemetry: config.TelemetryConfig{Enabled: true, Endpoint: "http://127.0.0.1:4318"},
		}, telemetrySetup{Disabled: true})
		Expect(err).ToNot(HaveOccurred())
		Expect(provider).To(BeNil())
	})

	// A misconfiguration fails at startup naming the fix, the same contract as every
	// other setting in this tree. OTLP being connectionless is a reason to validate what
	// is knowable locally, not a reason to validate nothing.
	It("Should reject a misconfigured endpoint before the run starts", func() {
		_, _, err := setupTelemetry(&config.Config{
			Identity:  "demo",
			Telemetry: config.TelemetryConfig{Enabled: true, Endpoint: "http://127.0.0.1:4317"},
		}, telemetrySetup{})
		Expect(err).To(MatchError(ContainSubstring("is the OTLP/gRPC port")))
	})

	It("Should start the pipelines for a valid enabled config", func() {
		provider, report, err := setupTelemetry(&config.Config{
			Identity:  "demo",
			Telemetry: config.TelemetryConfig{Enabled: true, Endpoint: "http://127.0.0.1:4318"},
		}, telemetrySetup{})
		Expect(err).ToNot(HaveOccurred())
		Expect(provider.Enabled()).To(BeTrue())

		// Nothing was recorded, so this flushes an empty pipeline against an endpoint
		// with no collector behind it and must still return promptly and quietly.
		report()
	})
})

// Every channel reporting content capture reads the resolved value, so it has to say
// what a run will actually export rather than what the file asked for. A veto leaves the
// file saying capture is on while nothing is exported at all.
var _ = Describe("content capture resolution", func() {
	It("Should resolve capture off when a veto turned telemetry off", func() {
		resolved, err := telemetry.Resolve(telemetry.Settings{
			Enabled:    true,
			Capture:    telemetry.CaptureSettings{Enabled: true},
			DisabledBy: noTelemetryFlag,
		}, func(string) string { return "" })
		Expect(err).ToNot(HaveOccurred())

		Expect(resolved.Capture.Value).To(BeFalse())
	})

	It("Should resolve capture on for an enabled run that asked for it", func() {
		resolved, err := telemetry.Resolve(telemetry.Settings{
			Enabled:  true,
			Endpoint: "https://otel.example.net:4318",
			Capture:  telemetry.CaptureSettings{Enabled: true},
		}, func(string) string { return "" })
		Expect(err).ToNot(HaveOccurred())

		Expect(resolved.Capture.Value).To(BeTrue())
	})
})
