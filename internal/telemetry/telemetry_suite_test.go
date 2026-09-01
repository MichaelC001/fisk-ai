//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"io"
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

// The specs that drive a refused export would otherwise print through OpenTelemetry's
// default handler, which writes to the log package's own os.Stderr and so lands in the
// suite's output rather than anywhere a spec can see. A per-spec handler cannot close
// that: containers are shuffled on every run, so any spec can be ordered into a
// position where nothing is installed, which is why the leak came and went with the
// seed. The sibling Bootstrap suite installs the same one for the same reason. Specs
// asserting what a handler receives install their own over this and restore to it.
var _ = BeforeSuite(func() {
	SetErrorHandler(io.Discard)
})

// envFrom returns an environment reader over a fixed map, which is how a spec supplies
// a variable without setting one on the process. The two specs covering the SDK's own
// merge of resource.Environment() do set one, through GinkgoT().Setenv, which restores
// it when the spec ends; Ginkgo runs parallel specs as separate processes, so neither
// reaches another suite.
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
