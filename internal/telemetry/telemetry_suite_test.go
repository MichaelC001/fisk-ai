//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestTelemetry(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Internal/Telemetry")
}

// envFrom returns an environment reader over a fixed map, the injection point every
// spec here uses instead of setting process environment variables. Nothing in this
// package reads os.Getenv, so specs never mutate process state and stay safe to run
// in parallel with everything else.
func envFrom(vars map[string]string) func(string) string {
	return func(name string) string {
		return vars[name]
	}
}

// ratio returns a pointer to v, for the sample_ratio field whose whole purpose is
// telling an explicit zero from an absent key.
func ratio(v float64) *float64 {
	return &v
}

// recording returns a Provider writing to an in-memory exporter, built through the
// embedder constructor rather than through Setup. Specs get to inspect exactly what
// would reach the wire without a collector, and because the providers are per spec
// rather than process globals nothing leaks between specs or between packages.
func recording() (*Provider, *tracetest.InMemoryExporter) {
	exp := tracetest.NewInMemoryExporter()

	return NewFromProviders(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp)), nil), exp
}

// attrOf returns the value of one attribute on a recorded span.
func attrOf(stub tracetest.SpanStub, key attribute.Key) (attribute.Value, bool) {
	for _, kv := range stub.Attributes {
		if kv.Key == key {
			return kv.Value, true
		}
	}

	return attribute.Value{}, false
}
