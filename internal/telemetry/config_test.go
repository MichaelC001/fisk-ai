//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"reflect"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Resolve", func() {
	Describe("enablement", func() {
		It("should stay off when the config never enabled it", func() {
			r, err := Resolve(Settings{}, envFrom(nil))
			Expect(err).ToNot(HaveOccurred())
			Expect(r.Enabled).To(BeFalse())
			Expect(r.NotEnabled).To(BeTrue())
			Expect(r.DisabledBy).To(BeEmpty(), "nothing vetoed it; it was never enabled")
		})

		It("should turn on for an enabled config", func() {
			r, err := Resolve(Settings{Enabled: true}, envFrom(nil))
			Expect(err).ToNot(HaveOccurred())
			Expect(r.Enabled).To(BeTrue())
			Expect(r.DisabledBy).To(BeEmpty())
		})

		It("should let the flag veto an enabled config", func() {
			r, err := Resolve(Settings{Enabled: true, DisabledBy: "--no-telemetry"}, envFrom(nil))
			Expect(err).ToNot(HaveOccurred())
			Expect(r.Enabled).To(BeFalse())
			Expect(r.DisabledBy).To(Equal("--no-telemetry"))
		})

		It("should let NO_TELEMETRY veto an enabled config", func() {
			r, err := Resolve(Settings{Enabled: true}, envFrom(map[string]string{EnvNoTelemetry: "1"}))
			Expect(err).ToNot(HaveOccurred())
			Expect(r.Enabled).To(BeFalse())
			Expect(r.DisabledBy).To(Equal(EnvNoTelemetry))
		})

		// NO_TELEMETRY follows the NO_COLOR convention where presence is the signal, but
		// an operator who writes an explicit false means it: silently ignoring that would
		// leave them with no way to re-enable from a shell that exports the variable.
		It("should honor an explicit false NO_TELEMETRY", func() {
			r, err := Resolve(Settings{Enabled: true}, envFrom(map[string]string{EnvNoTelemetry: "0"}))
			Expect(err).ToNot(HaveOccurred())
			Expect(r.Enabled).To(BeTrue())
		})

		It("should let OTEL_SDK_DISABLED veto an enabled config", func() {
			r, err := Resolve(Settings{Enabled: true}, envFrom(map[string]string{EnvSDKDisabled: "true"}))
			Expect(err).ToNot(HaveOccurred())
			Expect(r.Enabled).To(BeFalse())
			Expect(r.DisabledBy).To(Equal(EnvSDKDisabled))
		})

		// OTEL_SDK_DISABLED is specified to disable only on true, so an unparseable value
		// is ignored rather than treated as true. It is the standard's switch, not a
		// second NO_TELEMETRY with looser rules.
		It("should ignore an unparseable OTEL_SDK_DISABLED", func() {
			r, err := Resolve(Settings{Enabled: true}, envFrom(map[string]string{EnvSDKDisabled: "yes please"}))
			Expect(err).ToNot(HaveOccurred())
			Expect(r.Enabled).To(BeTrue())
		})

		// The case the whole enablement table exists for: a host-wide collector endpoint
		// must not turn every fisk-ai process on the box into an exporter.
		It("should not be enabled by transport variables alone", func() {
			env := envFrom(map[string]string{EnvEndpoint: "http://127.0.0.1:4318"})

			r, err := Resolve(Settings{}, env)
			Expect(err).ToNot(HaveOccurred())
			Expect(r.Enabled).To(BeFalse())
			Expect(r.TransportEnvSet).To(Equal([]string{EnvEndpoint}))
		})

		It("should report every transport variable that is set", func() {
			env := envFrom(map[string]string{
				EnvEndpoint:                  "http://127.0.0.1:4318",
				EnvProtocol:                  "http/protobuf",
				"OTEL_EXPORTER_OTLP_HEADERS": "authorization=Bearer t",
			})

			r, err := Resolve(Settings{}, env)
			Expect(err).ToNot(HaveOccurred())
			Expect(r.TransportEnvSet).To(ConsistOf(EnvEndpoint, EnvProtocol, "OTEL_EXPORTER_OTLP_HEADERS"))
			Expect(r.HeadersSet).To(BeTrue())
		})
	})

	Describe("endpoint", func() {
		It("should prefer the config file and mark it as such", func() {
			env := envFrom(map[string]string{EnvEndpoint: "http://from-env:4318"})

			r, err := Resolve(Settings{Enabled: true, Endpoint: "http://from-config:4318"}, env)
			Expect(err).ToNot(HaveOccurred())
			Expect(r.Endpoint.Value).To(Equal("http://from-config:4318"))
			Expect(r.Endpoint.Origin).To(Equal("telemetry.endpoint"))
			Expect(r.EndpointFromConfig).To(BeTrue())
		})

		// The signal-specific variable wins over the base one in the SDK itself, so what
		// is displayed and validated has to follow the same precedence or an operator
		// would be shown an endpoint their traces never go to.
		It("should prefer the traces endpoint over the base endpoint", func() {
			env := envFrom(map[string]string{
				EnvEndpoint:       "http://base:4318",
				EnvTracesEndpoint: "http://traces:4318/v1/traces",
			})

			r, err := Resolve(Settings{Enabled: true}, env)
			Expect(err).ToNot(HaveOccurred())
			Expect(r.Endpoint.Value).To(Equal("http://traces:4318/v1/traces"))
			Expect(r.Endpoint.Origin).To(Equal(EnvTracesEndpoint))
			Expect(r.EndpointFromConfig).To(BeFalse())
		})

		It("should fall back to the base endpoint", func() {
			env := envFrom(map[string]string{EnvEndpoint: "http://base:4318"})

			r, err := Resolve(Settings{Enabled: true}, env)
			Expect(err).ToNot(HaveOccurred())
			Expect(r.Endpoint.Value).To(Equal("http://base:4318"))
			Expect(r.Endpoint.Origin).To(Equal(EnvEndpoint))
		})

		It("should report the SDK default when nothing names one", func() {
			r, err := Resolve(Settings{Enabled: true}, envFrom(nil))
			Expect(err).ToNot(HaveOccurred())
			Expect(r.Endpoint.Value).To(Equal("http://localhost:4318"))
			Expect(r.Endpoint.Origin).To(Equal("default"))
			Expect(r.EndpointFromConfig).To(BeFalse())
		})
	})

	Describe("service name", func() {
		It("should prefer an explicit config value", func() {
			env := envFrom(map[string]string{EnvServiceName: "from-env"})

			r, err := Resolve(Settings{Enabled: true, ServiceName: "from-config", Identity: "ident"}, env)
			Expect(err).ToNot(HaveOccurred())
			Expect(r.ServiceName.Value).To(Equal("from-config"))
			Expect(r.ServiceName.Origin).To(Equal("telemetry.service_name"))
		})

		// An operator who set OTEL_SERVICE_NAME in a systemd unit or a Kubernetes
		// manifest said something explicit; the identity is only a good default for when
		// nobody did. The identity still reaches the backend as gen_ai.agent.name.
		It("should let OTEL_SERVICE_NAME beat the identity", func() {
			env := envFrom(map[string]string{EnvServiceName: "from-env"})

			r, err := Resolve(Settings{Enabled: true, Identity: "ident"}, env)
			Expect(err).ToNot(HaveOccurred())
			Expect(r.ServiceName.Value).To(Equal("from-env"))
			Expect(r.ServiceName.Origin).To(Equal(EnvServiceName))
		})

		It("should fall back to the identity", func() {
			r, err := Resolve(Settings{Enabled: true, Identity: "ident"}, envFrom(nil))
			Expect(err).ToNot(HaveOccurred())
			Expect(r.ServiceName.Value).To(Equal("ident"))
			Expect(r.ServiceName.Origin).To(Equal("identity"))
		})

		It("should fall back to fisk-ai when there is no identity", func() {
			r, err := Resolve(Settings{Enabled: true}, envFrom(nil))
			Expect(err).ToNot(HaveOccurred())
			Expect(r.ServiceName.Value).To(Equal("fisk-ai"))
			Expect(r.ServiceName.Origin).To(Equal("default"))
		})
	})

	Describe("sample ratio", func() {
		It("should default an absent key to sampling everything", func() {
			r, err := Resolve(Settings{Enabled: true}, envFrom(nil))
			Expect(err).ToNot(HaveOccurred())
			Expect(r.SampleRatio.Value).To(Equal(1.0))
			Expect(r.SampleRatio.Origin).To(Equal("default"))
		})

		// The reason the field is a pointer. A plain float64 would make an explicit zero
		// indistinguishable from an absent key, so "sample nothing" would be defaulted
		// straight back to sampling everything and every trace would reach a paid backend.
		It("should keep an explicit zero", func() {
			r, err := Resolve(Settings{Enabled: true, SampleRatio: ratio(0)}, envFrom(nil))
			Expect(err).ToNot(HaveOccurred())
			Expect(r.SampleRatio.Value).To(Equal(0.0))
			Expect(r.SampleRatio.Origin).To(Equal("telemetry.sample_ratio"))
		})

		It("should keep a fractional value", func() {
			r, err := Resolve(Settings{Enabled: true, SampleRatio: ratio(0.25)}, envFrom(nil))
			Expect(err).ToNot(HaveOccurred())
			Expect(r.SampleRatio.Value).To(Equal(0.25))
		})
	})

	Describe("metrics", func() {
		It("should be on with telemetry", func() {
			r, err := Resolve(Settings{Enabled: true}, envFrom(nil))
			Expect(err).ToNot(HaveOccurred())
			Expect(r.Metrics.Value).To(BeTrue())
			Expect(r.Metrics.Origin).To(Equal("default"))
		})

		It("should be turned off by no_metrics", func() {
			r, err := Resolve(Settings{Enabled: true, NoMetrics: true}, envFrom(nil))
			Expect(err).ToNot(HaveOccurred())
			Expect(r.Metrics.Value).To(BeFalse())
			Expect(r.Metrics.Origin).To(Equal("telemetry.no_metrics"))
		})
	})

	Describe("content capture", func() {
		It("should be off, delta and 8192 by default", func() {
			r, err := Resolve(Settings{Enabled: true}, envFrom(nil))
			Expect(err).ToNot(HaveOccurred())

			Expect(r.Capture.Value).To(BeFalse())
			Expect(r.Capture.Origin).To(Equal("default"))
			Expect(r.Messages.Value).To(Equal(MessagesDelta))
			Expect(r.MaxBytes.Value).To(Equal(defaultMaxContentBytes))
			Expect(r.MaxBytes.Origin).To(Equal("default"))
		})

		It("should report each value and where it came from", func() {
			r, err := Resolve(Settings{
				Enabled: true,
				Capture: CaptureSettings{Enabled: true, Messages: MessagesFull, MaxBytes: 4096},
			}, envFrom(nil))
			Expect(err).ToNot(HaveOccurred())

			Expect(r.Capture.Value).To(BeTrue())
			Expect(r.Capture.Origin).To(Equal("telemetry.capture.enabled"))
			Expect(r.Messages.Value).To(Equal(MessagesFull))
			Expect(r.Messages.Origin).To(Equal("telemetry.capture.messages"))
			Expect(r.MaxBytes.Value).To(Equal(4096))
			Expect(r.MaxBytes.Origin).To(Equal("telemetry.capture.max_bytes"))
		})

		// Defaulting has to happen before the range check, or every configuration that
		// enables capture without naming a cap fails at startup over a key the operator
		// never wrote. Unlike sample_ratio zero carries no meaning of its own here, so a
		// plain int with a default is right where that one needed a pointer.
		It("should default an unset cap rather than rejecting it", func() {
			r, err := Resolve(Settings{Enabled: true, Capture: CaptureSettings{Enabled: true}}, envFrom(nil))
			Expect(err).ToNot(HaveOccurred())
			Expect(r.MaxBytes.Value).To(Equal(defaultMaxContentBytes))
		})

		DescribeTable("should reject a cap outside its bounds",
			func(v int) {
				_, err := Resolve(Settings{Enabled: true, Capture: CaptureSettings{Enabled: true, MaxBytes: v}}, envFrom(nil))
				Expect(err).To(MatchError(ContainSubstring("telemetry.capture.max_bytes")))
				Expect(err).To(MatchError(ContainSubstring("default 8192")))
			},
			Entry("below the floor", 16),
			Entry("above the ceiling", 1<<20),
		)

		It("should reject a message scope that is neither delta nor full", func() {
			_, err := Resolve(Settings{Enabled: true, Capture: CaptureSettings{Enabled: true, Messages: ParseMessagesMode("everything")}}, envFrom(nil))
			Expect(err).To(MatchError(ContainSubstring("telemetry.capture.messages")))
			Expect(err).To(MatchError(ContainSubstring(`"delta"`)))
			Expect(err).To(MatchError(ContainSubstring(`"full"`)))
		})

		// An operator who turns capture off mid-incident must not be stopped by the
		// settings underneath it, which by then mean nothing. This is the same rule the
		// endpoint already follows when telemetry is off, and the reason capture is not
		// a set of flat keys needing a cross-field error to be legible.
		// Every operator-facing surface reports this value, so it has to describe what is
		// happening rather than what the file asked for. Left true under a veto, the
		// startup note would warn about an export that is not occurring and fisk info
		// would show capture on for a run whose spans go nowhere.
		DescribeTable("should report capture off whenever nothing is exported",
			func(s Settings, env map[string]string) {
				s.Enabled = true
				s.Capture = CaptureSettings{Enabled: true}

				r, err := Resolve(s, envFrom(env))
				Expect(err).ToNot(HaveOccurred())

				Expect(r.Enabled).To(BeFalse())
				Expect(r.Capture.Value).To(BeFalse())
				Expect(r.Capture.Origin).To(Equal(r.DisabledBy))
			},
			Entry("vetoed by the flag", Settings{DisabledBy: "--no-telemetry"}, nil),
			Entry("vetoed by the environment", Settings{}, map[string]string{EnvNoTelemetry: "1"}),
			Entry("disabled by the SDK switch", Settings{}, map[string]string{EnvSDKDisabled: "true"}),
		)

		It("should not validate the capture block when capture is off", func() {
			r, err := Resolve(Settings{
				Enabled: true,
				Capture: CaptureSettings{Enabled: false, Messages: ParseMessagesMode("nonsense"), MaxBytes: 3},
			}, envFrom(nil))
			Expect(err).ToNot(HaveOccurred())
			Expect(r.Capture.Value).To(BeFalse())
		})
	})

	// The SDK enforces an operator's attribute-value limit by cutting the value at a
	// character boundary, which is right for an ordinary attribute and wrong for a JSON
	// document: the cut lands mid-document and what arrives no longer parses, which is
	// the failure the structural truncation exists to prevent, appearing only in the
	// mature deployments that set OTel limits at all.
	Describe("the attribute value limit", func() {
		DescribeTable("should lower the content budget to fit rather than be overridden",
			func(name string) {
				r, err := Resolve(
					Settings{Enabled: true, Capture: CaptureSettings{Enabled: true}},
					envFrom(map[string]string{name: "2048"}),
				)
				Expect(err).ToNot(HaveOccurred())

				Expect(r.MaxBytes.Value).To(Equal(2048))
				Expect(r.MaxBytes.Origin).To(Equal(name))
			},
			Entry("the span-scoped variable", EnvSpanAttrValueLimit),
			Entry("the general variable", EnvAttrValueLimit),
		)

		It("should prefer the span-scoped variable, as the SDK does", func() {
			r, err := Resolve(
				Settings{Enabled: true, Capture: CaptureSettings{Enabled: true}},
				envFrom(map[string]string{EnvSpanAttrValueLimit: "1024", EnvAttrValueLimit: "4096"}),
			)
			Expect(err).ToNot(HaveOccurred())
			Expect(r.MaxBytes.Value).To(Equal(1024))
		})

		It("should keep the configured cap when the limit is larger", func() {
			r, err := Resolve(
				Settings{Enabled: true, Capture: CaptureSettings{Enabled: true, MaxBytes: 4096}},
				envFrom(map[string]string{EnvSpanAttrValueLimit: "60000"}),
			)
			Expect(err).ToNot(HaveOccurred())
			Expect(r.MaxBytes.Value).To(Equal(4096))
			Expect(r.MaxBytes.Origin).To(Equal("telemetry.capture.max_bytes"))
		})

		It("should ignore an unparsable limit, as the SDK does", func() {
			r, err := Resolve(
				Settings{Enabled: true, Capture: CaptureSettings{Enabled: true}},
				envFrom(map[string]string{EnvSpanAttrValueLimit: "lots"}),
			)
			Expect(err).ToNot(HaveOccurred())
			Expect(r.MaxBytes.Value).To(Equal(defaultMaxContentBytes))
		})
	})

	// contentAttrsPerSpan is the divisor in the batch arithmetic, so a span that grows
	// a third content attribute silently doubles what one export request can weigh. It
	// cannot be derived at run time, and a comment saying "two" is the shape section 8.3
	// exists to warn about, so this counts the ContentBuilder fields actually declared
	// on the span payload types and fails when the busiest one outgrows the constant.
	It("should keep contentAttrsPerSpan equal to the most content a span can carry", func() {
		count := func(v any) int {
			t := reflect.TypeOf(v)
			n := 0
			for i := range t.NumField() {
				if t.Field(i).Type == reflect.TypeOf(ContentBuilder(nil)) {
					n++
				}
			}
			return n
		}

		// A chat span's payload is split across the two types, so they are summed; a
		// tool span carries its content on the outcome alone.
		chat := count(ChatInfo{}) + count(ChatOutcome{})
		tool := count(ToolOutcome{})

		Expect(contentAttrsPerSpan).To(Equal(max(chat, tool)),
			"a span kind gained a content attribute; the export batch arithmetic divides by this")
	})

	Describe("the export batch size", func() {
		It("should leave the SDK's own handling alone when capture is off", func() {
			r, err := Resolve(Settings{Enabled: true}, envFrom(nil))
			Expect(err).ToNot(HaveOccurred())
			Expect(r.ExportBatch.Value).To(Equal(maxExportBatch))
			Expect(r.ExportBatch.Origin).To(Equal("default"))
		})

		// Derived rather than fixed, because the failure it prevents appears exactly
		// when an operator raises the cap, and a hardcoded number would quietly stop
		// protecting them at that moment. No attribute assertion can see any of this,
		// so the arithmetic is what is pinned.
		DescribeTable("should shrink as the content cap grows",
			func(maxBytes, expected int) {
				r, err := Resolve(
					Settings{Enabled: true, Capture: CaptureSettings{Enabled: true, MaxBytes: maxBytes}},
					envFrom(nil),
				)
				Expect(err).ToNot(HaveOccurred())
				Expect(r.ExportBatch.Value).To(Equal(expected))
				Expect(r.ExportBatch.Origin).To(ContainSubstring("derived"))
			},
			Entry("at the default cap", defaultMaxContentBytes, 128),
			Entry("at the ceiling", maxMaxContentBytes, minExportBatch),
			Entry("at the floor, clamped to the SDK maximum", minMaxContentBytes, maxExportBatch),
		)

		// Whatever the batch size, one request stays inside the target. This is the
		// property the arithmetic exists for, asserted directly so a change to the
		// formula that still passes the table above cannot pass this.
		DescribeTable("should keep one request inside the target size",
			func(maxBytes int) {
				r, err := Resolve(
					Settings{Enabled: true, Capture: CaptureSettings{Enabled: true, MaxBytes: maxBytes}},
					envFrom(nil),
				)
				Expect(err).ToNot(HaveOccurred())

				worst := r.ExportBatch.Value * contentAttrsPerSpan * r.MaxBytes.Value
				Expect(worst).To(BeNumerically("<=", targetBatchBytes))
			},
			Entry("the default cap", defaultMaxContentBytes),
			Entry("a raised cap", 32768),
			Entry("the ceiling", maxMaxContentBytes),
		)
	})

	// OTEL_RESOURCE_ATTRIBUTES is parsed here rather than by resource.WithFromEnv, so it
	// has to behave exactly as the SDK's own detector does; a difference would show up as
	// this build and every other OpenTelemetry process disagreeing about one string.
	Describe("the resource attributes", func() {
		attrs := func(value string) []ResourceAttribute {
			GinkgoHelper()

			r, err := Resolve(Settings{Enabled: true}, envFrom(map[string]string{EnvResourceAttributes: value}))
			Expect(err).ToNot(HaveOccurred())

			return r.ResourceAttributes
		}

		It("should read nothing from an empty variable", func() {
			Expect(attrs("")).To(BeEmpty())
			Expect(attrs("   ")).To(BeEmpty())
		})

		It("should read a pair", func() {
			Expect(attrs("a=1")).To(Equal([]ResourceAttribute{{Key: "a", Value: "1"}}))
		})

		// Percent encoding is how a value carrying a comma or an equals sign is written,
		// both being separators.
		It("should decode a percent-encoded value", func() {
			Expect(attrs("a=%20b%2Cc%3Dd")).To(Equal([]ResourceAttribute{{Key: "a", Value: " b,c=d"}}))
		})

		// The SDK decodes the value and leaves the key alone, so this does too.
		It("should leave a percent-encoded key encoded", func() {
			Expect(attrs("a%20b=1")).To(Equal([]ResourceAttribute{{Key: "a%20b", Value: "1"}}))
		})

		It("should keep a value whose escape will not decode", func() {
			Expect(attrs("a=%zz")).To(Equal([]ResourceAttribute{{Key: "a", Value: "%zz"}}))
		})

		It("should trim whitespace around the key and the value", func() {
			Expect(attrs("  a  =  1  ")).To(Equal([]ResourceAttribute{{Key: "a", Value: "1"}}))
		})

		It("should keep an empty value", func() {
			Expect(attrs("a=,b=2")).To(Equal([]ResourceAttribute{{Key: "a", Value: ""}, {Key: "b", Value: "2"}}))
		})

		// An attribute needs a key, and the SDK's own set builder drops one without.
		It("should drop an entry with an empty key", func() {
			Expect(attrs("=1,b=2")).To(Equal([]ResourceAttribute{{Key: "b", Value: "2"}}))
		})

		// Both are kept in order and the attribute set takes the last, which is what the
		// SDK does with a repeated key.
		It("should keep a repeated key in order", func() {
			Expect(attrs("a=1,a=2")).To(Equal([]ResourceAttribute{{Key: "a", Value: "1"}, {Key: "a", Value: "2"}}))
		})

		It("should split at the first equals sign", func() {
			Expect(attrs("a=b=c")).To(Equal([]ResourceAttribute{{Key: "a", Value: "b=c"}}))
		})

		// The SDK's detector returns an error for these and Setup made that error fatal,
		// so it stays fatal. The entries that did parse are on the returned Resolved,
		// which is complete whether or not the error is nil.
		DescribeTable("should refuse an entry that names no value",
			func(value string) {
				r, err := Resolve(Settings{Enabled: true}, envFrom(map[string]string{EnvResourceAttributes: value}))
				Expect(err).To(MatchError(ErrInvalidSetting))
				Expect(err).To(MatchError(ContainSubstring(EnvResourceAttributes)))
				Expect(r.ResourceAttributes).To(Equal([]ResourceAttribute{{Key: "a", Value: "1"}}))
			},
			Entry("an entry with no equals sign", "a=1,justkey"),
			Entry("a trailing comma", "a=1,"),
		)

		It("should not fail a configuration that is off over an entry with no value", func() {
			r, err := Resolve(Settings{}, envFrom(map[string]string{EnvResourceAttributes: "a=1,justkey"}))
			Expect(err).ToNot(HaveOccurred())
			Expect(r.ResourceAttributes).To(Equal([]ResourceAttribute{{Key: "a", Value: "1"}}))
		})
	})

	Describe("validation", func() {
		// A stale endpoint in a file with telemetry off must never fail a run: nothing
		// will be exported, so there is nothing to be wrong about.
		It("should not validate a configuration that is off", func() {
			r, err := Resolve(Settings{Endpoint: "ftp://nope:4317"}, envFrom(nil))
			Expect(err).ToNot(HaveOccurred())
			Expect(r.Enabled).To(BeFalse())
		})

		DescribeTable("should reject a gRPC protocol selection naming the HTTP port",
			func(name string) {
				_, err := Resolve(Settings{Enabled: true}, envFrom(map[string]string{name: "grpc"}))
				Expect(err).To(MatchError(ContainSubstring("this build speaks OTLP/HTTP only")))
				Expect(err).To(MatchError(ContainSubstring("4318")))
				Expect(err).To(MatchError(ContainSubstring(name)))
			},
			Entry("the shared variable", EnvProtocol),
			Entry("the traces variable", EnvTracesProtocol),
			Entry("the metrics variable", EnvMetricsProtocol),
		)

		It("should accept an explicit http/protobuf protocol", func() {
			_, err := Resolve(Settings{Enabled: true}, envFrom(map[string]string{EnvProtocol: "http/protobuf"}))
			Expect(err).ToNot(HaveOccurred())
		})

		DescribeTable("should reject a sample ratio outside 0 to 1",
			func(v float64) {
				_, err := Resolve(Settings{Enabled: true, SampleRatio: ratio(v)}, envFrom(nil))
				Expect(err).To(MatchError(ContainSubstring("telemetry.sample_ratio")))
				Expect(err).To(MatchError(ContainSubstring("must be between 0 and 1")))
			},
			Entry("negative", -0.5),
			Entry("above one", 1.5),
		)

		It("should reject an endpoint that is not http or https", func() {
			_, err := Resolve(Settings{Enabled: true, Endpoint: "grpc://collector:4318"}, envFrom(nil))
			Expect(err).To(MatchError(ContainSubstring("scheme must be http or https")))
		})

		It("should reject an endpoint embedding userinfo credentials", func() {
			_, err := Resolve(Settings{Enabled: true, Endpoint: "https://user:pass@collector:4318"}, envFrom(nil))
			Expect(err).To(MatchError(ContainSubstring("must not embed userinfo credentials")))
		})

		// The single most common mistake in this area, and one that otherwise fails as
		// something that looks like a network problem.
		It("should reject the OTLP/gRPC port by name", func() {
			_, err := Resolve(Settings{Enabled: true, Endpoint: "http://127.0.0.1:4317"}, envFrom(nil))
			Expect(err).To(MatchError(ContainSubstring("is the OTLP/gRPC port")))
			Expect(err).To(MatchError(ContainSubstring("use port 4318")))
			Expect(err).To(MatchError(ContainSubstring("telemetry.endpoint")))
		})

		It("should reject the gRPC port reached through the environment too", func() {
			env := envFrom(map[string]string{EnvEndpoint: "http://collector:4317"})

			_, err := Resolve(Settings{Enabled: true}, env)
			Expect(err).To(MatchError(ContainSubstring("is the OTLP/gRPC port")))
			Expect(err).To(MatchError(ContainSubstring(EnvEndpoint)))
		})

		It("should reject plain http to a remote host while a headers variable is set", func() {
			env := envFrom(map[string]string{"OTEL_EXPORTER_OTLP_HEADERS": "authorization=Bearer secret"})

			_, err := Resolve(Settings{Enabled: true, Endpoint: "http://collector.example.net:4318"}, env)
			Expect(err).To(MatchError(ContainSubstring("sends the credential in the clear")))
			Expect(err).To(MatchError(ContainSubstring("use https")))
		})

		DescribeTable("should allow plain http to a loopback collector even with headers set",
			func(endpoint string) {
				env := envFrom(map[string]string{"OTEL_EXPORTER_OTLP_HEADERS": "authorization=Bearer secret"})

				_, err := Resolve(Settings{Enabled: true, Endpoint: endpoint}, env)
				Expect(err).ToNot(HaveOccurred())
			},
			Entry("an IPv4 loopback address", "http://127.0.0.1:4318"),
			Entry("an IPv6 loopback address", "http://[::1]:4318"),
			Entry("the localhost name", "http://localhost:4318"),
		)

		// With neither a credential nor content in play there is nothing to leak, so
		// plain http to a remote collector is the operator's call to make.
		It("should allow plain http to a remote host when no headers are set", func() {
			_, err := Resolve(Settings{Enabled: true, Endpoint: "http://collector.example.net:4318"}, envFrom(nil))
			Expect(err).ToNot(HaveOccurred())
		})

		// The rule above is gated on a headers variable because it was written for one
		// threat, a bearer token in the clear. Content capture makes the payload itself
		// the secret, so that gate is the wrong one: an unauthenticated collector on an
		// internal network sets no headers variable at all, and it is the common shape.
		// Without this the whole conversation crosses the network in cleartext and
		// nothing says so.
		It("should reject plain http to a remote host when capture is on, with no headers set", func() {
			_, err := Resolve(Settings{
				Enabled:  true,
				Endpoint: "http://collector.example.net:4318",
				Capture:  CaptureSettings{Enabled: true},
			}, envFrom(nil))

			Expect(err).To(MatchError(ContainSubstring("sends prompts, tool arguments and tool results in the clear")))
			Expect(err).To(MatchError(ContainSubstring("telemetry.capture.enabled")))
			Expect(err).To(MatchError(ContainSubstring("use https")))
		})

		It("should allow plain http to a loopback collector with capture on", func() {
			_, err := Resolve(Settings{
				Enabled:  true,
				Endpoint: "http://127.0.0.1:4318",
				Capture:  CaptureSettings{Enabled: true},
			}, envFrom(nil))

			Expect(err).ToNot(HaveOccurred())
		})

		// Both rules can fire on one endpoint, and the operator should be told which
		// they hit: the fix is the same but the exposure is not, and an operator who
		// removes the headers variable expecting the run to start would otherwise be
		// refused again with no new information.
		It("should report the credential exposure first when both apply", func() {
			env := envFrom(map[string]string{"OTEL_EXPORTER_OTLP_HEADERS": "authorization=Bearer secret"})

			_, err := Resolve(Settings{
				Enabled:  true,
				Endpoint: "http://collector.example.net:4318",
				Capture:  CaptureSettings{Enabled: true},
			}, env)

			Expect(err).To(MatchError(ContainSubstring("sends the credential in the clear")))
		})

		// Resolve reports what it worked out even when it rejects it, so fisk info can
		// show an operator the resolved values alongside what was wrong with them.
		It("should return the resolved values alongside an error", func() {
			r, err := Resolve(Settings{Enabled: true, Endpoint: "http://127.0.0.1:4317", Identity: "ident"}, envFrom(nil))
			Expect(err).To(HaveOccurred())
			Expect(r.Endpoint.Value).To(Equal("http://127.0.0.1:4317"))
			Expect(r.ServiceName.Value).To(Equal("ident"))
		})
	})

	It("should tolerate a nil environment reader", func() {
		r, err := Resolve(Settings{Enabled: true}, nil)
		Expect(err).ToNot(HaveOccurred())
		Expect(r.Enabled).To(BeTrue())
	})
})

// MessagesMode is a struct rather than a defined string type for the reason ErrorClass
// is: a defined string type is convertible from any string by any package, so the typo
// this exists to close would still compile.
var _ = Describe("MessagesMode", func() {
	It("should report the configured word", func() {
		Expect(MessagesDelta.String()).To(Equal("delta"))
		Expect(MessagesFull.String()).To(Equal("full"))
	})

	It("should treat the zero value as nothing configured", func() {
		var m MessagesMode

		Expect(m.Set()).To(BeFalse())
		Expect(m.String()).To(BeEmpty())
		Expect(m.Valid()).To(BeFalse())
	})

	It("should recognize the two modes this build understands", func() {
		Expect(ParseMessagesMode("delta")).To(Equal(MessagesDelta))
		Expect(ParseMessagesMode("full")).To(Equal(MessagesFull))
		Expect(ParseMessagesMode("delta").Valid()).To(BeTrue())
	})

	// Parse accepts anything so Resolve stays the one place that validates, and so the
	// error it raises can quote what the operator actually wrote.
	It("should preserve an unrecognized value rather than rejecting or discarding it", func() {
		m := ParseMessagesMode("detla")

		Expect(m.Set()).To(BeTrue())
		Expect(m.Valid()).To(BeFalse())
		Expect(m.String()).To(Equal("detla"))
	})

	It("should name the bad value and the two accepted ones when Resolve rejects it", func() {
		_, err := Resolve(Settings{
			Enabled: true,
			Capture: CaptureSettings{Enabled: true, Messages: ParseMessagesMode("detla")},
		}, envFrom(nil))

		Expect(err).To(MatchError(ContainSubstring(`"detla"`)))
		Expect(err).To(MatchError(ContainSubstring(`"delta"`)))
		Expect(err).To(MatchError(ContainSubstring(`"full"`)))
	})
})

// A caller has to be able to tell a sample ratio out of range from a credential headed
// for a plain-http endpoint, and matching English is not a way to do it. Setup's own
// ErrPipeline is driven in the Setup specs, since it needs a pipeline to fail.
var _ = Describe("the failure sentinels", func() {
	DescribeTable("should be reachable from the failure that returns it",
		func(s Settings, env map[string]string, sentinel error) {
			_, err := Resolve(s, envFrom(env))
			Expect(err).To(MatchError(sentinel))
		},
		Entry("a sample ratio out of range",
			Settings{Enabled: true, SampleRatio: ratio(2)}, nil, ErrInvalidSetting),
		Entry("a capture message mode this build does not know",
			Settings{Enabled: true, Capture: CaptureSettings{Enabled: true, Messages: ParseMessagesMode("detla")}},
			nil, ErrInvalidSetting),
		Entry("a content cap below the floor",
			Settings{Enabled: true, Capture: CaptureSettings{Enabled: true, MaxBytes: 1}}, nil, ErrInvalidSetting),
		Entry("a resource attribute entry that names no value",
			Settings{Enabled: true}, map[string]string{EnvResourceAttributes: "justkey"}, ErrInvalidSetting),
		Entry("an endpoint that will not parse",
			Settings{Enabled: true, Endpoint: "http://[::1"}, nil, ErrInvalidEndpoint),
		Entry("an endpoint embedding userinfo credentials",
			Settings{Enabled: true, Endpoint: "https://user:pass@collector:4318"}, nil, ErrInvalidEndpoint),
		Entry("an endpoint on a scheme that is neither http nor https",
			Settings{Enabled: true, Endpoint: "ftp://collector:4318"}, nil, ErrInvalidEndpoint),
		Entry("plain http to a remote host while a headers variable is set",
			Settings{Enabled: true, Endpoint: "http://collector.example.net:4318"},
			map[string]string{"OTEL_EXPORTER_OTLP_HEADERS": "authorization=Bearer secret"}, ErrInsecureEndpoint),
		Entry("plain http to a remote host while content capture is on",
			Settings{Enabled: true, Endpoint: "http://collector.example.net:4318", Capture: CaptureSettings{Enabled: true}},
			nil, ErrInsecureEndpoint),
		Entry("a grpc protocol selection",
			Settings{Enabled: true}, map[string]string{EnvProtocol: "grpc"}, ErrProtocolUnsupported),
		Entry("an endpoint on the OTLP/gRPC port",
			Settings{Enabled: true, Endpoint: "http://127.0.0.1:4317"}, nil, ErrProtocolUnsupported),
	)
})
