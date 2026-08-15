//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// Package telemetry exports OpenTelemetry traces and metrics for an agent run.
//
// It is a hard leaf: it imports the standard library and OpenTelemetry, and nothing
// else from this repository. Every other package imports this one and never
// go.opentelemetry.io/... directly, so nothing outside here knows OpenTelemetry is
// underneath and call sites read as domain operations rather than as tracing calls.
//
// That boundary is load-bearing rather than tidy. It makes an import cycle
// impossible whatever gets instrumented later: a package this one imported could
// never import it back, and the model backends are exactly the code that will want
// its own spans one day. The compiler enforces that half on its own, since the cycle
// announces itself as a build failure. The soft half, a package reaching for
// go.opentelemetry.io/... instead of this facade, is this comment and a code review.
//
// Two consequences shape the API. Constructors take primitives rather than domain
// types, so nothing here has to import config, llm or agent to describe a span. And
// error classes are ErrorClass values a caller cannot construct, rather than a
// classifier over domain sentinels that would drag half the tree in.
//
// The one exception is NewFromProviders, for a caller who already runs
// OpenTelemetry and hands in their own providers. That is their code importing
// OpenTelemetry, not this repository's.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"strings"
	"sync"

	"github.com/go-logr/stdr"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	// The resource is described at the schema the SDK's own detectors use, which is not
	// the schema the gen_ai keys come from. resource.Merge refuses to merge resources
	// whose schema URLs differ, so declaring an older one here fails every Setup with a
	// conflict the moment the SDK moves on. The span and metric attributes elsewhere in
	// this package stay on v1.41.0, which is the last version to ship the GenAI
	// conventions; see attrs.go.
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// scopeName is the instrumentation scope every span and metric this package emits
// is attributed to. It is the module path rather than this package's path, since
// what a backend should show is which software instrumented the call.
const scopeName = "github.com/choria-io/fisk-ai"

// Provider is the handle a run uses to record telemetry. Every method is safe to
// call on a nil Provider and returns a no-op, in the manner of util.Tracer, so the
// run path wires it up without asking whether telemetry is on. That is what keeps
// enable and disable branches out of the call sites, and it means the instrumented
// and uninstrumented paths cannot diverge.
//
// It carries its own tracer and meter and registers nothing globally: no
// otel.SetTracerProvider, no otel.SetMeterProvider, no global propagator. Consumers
// take it as an injection point, matching how the run already injects its store,
// connection and model backend, and specs get a recording provider without mutating
// process state that would leak across parallel packages.
type Provider struct {
	tracer trace.Tracer
	meter  metric.Meter

	// tp and mp are the SDK providers this package built, held so Shutdown can flush
	// them. Both are nil for a Provider built by NewFromProviders, whose providers the
	// embedder owns and shuts down themselves.
	tp *sdktrace.TracerProvider
	mp *sdkmetric.MeterProvider

	// instruments is nil when no metric pipeline runs, which is what makes every
	// recording path a no-op for a traces-only configuration.
	instruments *instruments

	delivery *delivery
	endpoint string

	// capture is the content-capture configuration, held here so a span method can
	// decide whether to invoke a ContentBuilder at all. A zero value captures nothing,
	// which is what a Provider built by NewFromProviders gets: an embedder owns their
	// own pipeline, so this package does not export their conversation on their behalf.
	capture capture
}

// capture is the resolved content-capture configuration.
type capture struct {
	enabled  bool
	full     bool
	maxBytes int
}

// Delivery is what actually reached the collector, so an operator can tell export
// that silently failed from export that worked. OTLP is fire and forget over HTTP,
// which means a wrong endpoint, an expired token and a healthy pipeline all look
// identical from inside the process unless something counts.
type Delivery struct {
	// Endpoint is where export was attempted, for the message.
	Endpoint string
	// SpansAttempted and SpansDelivered count spans handed to the exporter and spans
	// the collector accepted.
	SpansAttempted int64
	SpansDelivered int64
	// MetricExportsAttempted and MetricExportsDelivered count export requests rather
	// than data points: the metric exporter is handed one batch per collection cycle
	// and reporting a point count would mean walking the whole batch on every export
	// to produce a number no operator asked for.
	MetricExportsAttempted int64
	MetricExportsDelivered int64
	// Err is the first export failure, which is the one worth showing: the rest are
	// almost always the same failure repeating.
	Err error
}

// Attempted reports whether anything was handed to the exporters at all. A run with
// nothing attempted is not a failure, it just had nothing to say.
func (d Delivery) Attempted() bool {
	return d.SpansAttempted > 0 || d.MetricExportsAttempted > 0
}

// Complete reports whether everything attempted was delivered.
func (d Delivery) Complete() bool {
	return d.Err == nil && d.SpansDelivered == d.SpansAttempted && d.MetricExportsDelivered == d.MetricExportsAttempted
}

// Setup builds a Provider exporting over OTLP/HTTP for a resolved configuration. A
// configuration that resolves to off returns a nil Provider and a nil error, which is
// the no-op every call site already handles.
//
// version is passed in rather than read here because this package imports nothing
// from the rest of the tree; it becomes service.version on the resource, where a
// version belongs, rather than an attribute repeated on every span.
//
// An explicit endpoint is passed to the exporters only when the config file named
// one. When it did not, no endpoint option is set at all and the SDK applies its own
// standard OTEL_EXPORTER_OTLP_* handling, including the signal-specific variables and
// their precedence, which is exactly the behavior an operator running OpenTelemetry
// already expects.
func Setup(ctx context.Context, r Resolved, version string, opts ...Option) (*Provider, error) {
	if !r.Enabled {
		return nil, nil
	}

	// The delivery tally is built before anything that could write to it, and the
	// options are applied to it here, so an export error handler is in place before the
	// first export rather than attached to a pipeline already running.
	d := &delivery{}
	p := &Provider{delivery: d, endpoint: r.Endpoint.Value}
	for _, opt := range opts {
		opt(p)
	}

	res, err := resource.New(ctx,
		resource.WithTelemetrySDK(),
		resource.WithFromEnv(),
		resource.WithSchemaURL(semconv.SchemaURL),
		// Applied last so the resolved service name wins over OTEL_SERVICE_NAME,
		// which resolveServiceName has already taken into account at its own
		// precedence.
		resource.WithAttributes(
			semconv.ServiceName(r.ServiceName.Value),
			semconv.ServiceVersion(version),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("building the telemetry resource: %w", err)
	}

	traceExp, err := otlptracehttp.New(ctx, traceOptions(r)...)
	if err != nil {
		return nil, fmt.Errorf("building the OTLP/HTTP trace exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler(r.SampleRatio.Value)),
		sdktrace.WithBatcher(&countingSpanExporter{SpanExporter: traceExp, delivery: d}, batchOptions(r)...),
	)

	p.tracer = tp.Tracer(scopeName)
	p.tp = tp
	p.capture = capture{
		enabled:  r.Capture.Value,
		full:     r.Messages.Value == MessagesFull,
		maxBytes: r.MaxBytes.Value,
	}

	if !r.Metrics.Value {
		return p, nil
	}

	metricExp, err := otlpmetrichttp.New(ctx, metricOptions(r)...)
	if err != nil {
		// The trace pipeline is already running, so tear it down rather than leak its
		// batch processor goroutine on the way out.
		_ = tp.Shutdown(ctx)
		return nil, fmt.Errorf("building the OTLP/HTTP metric exporter: %w", err)
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(&countingMetricExporter{Exporter: metricExp, delivery: d})),
	)
	p.mp = mp
	p.meter = mp.Meter(scopeName)

	p.instruments, err = newInstruments(p.meter)
	if err != nil {
		_ = mp.Shutdown(ctx)
		_ = tp.Shutdown(ctx)
		return nil, fmt.Errorf("building the metric instruments: %w", err)
	}

	return p, nil
}

// sampler builds the trace sampler for a resolved ratio.
//
// A remote parent that says it was sampled is followed, which is what makes a
// delegation one trace: overriding it with the local ratio would drop spans out of the
// middle of a trace an operator is following.
//
// A remote parent that says it was NOT sampled is not followed, which is the default
// this overrides. An a2a message carries its sender's trace context in the body and
// the transport authenticates nobody, so under the default any peer that could reach
// the subject could switch this process's recording off for a served call and
// everything under it. The local ratio decides instead, and the trace ids are adopted
// either way, so linkage is unaffected.
func sampler(ratio float64) sdktrace.Sampler {
	ratioBased := sdktrace.TraceIDRatioBased(ratio)

	return sdktrace.ParentBased(ratioBased, sdktrace.WithRemoteParentNotSampled(ratioBased))
}

// batchOptions builds the batch processor options.
//
// Only content capture needs any: without it a span is a few hundred bytes and the
// SDK's defaults are right, so nothing is passed and an operator's own OTEL_BSP_*
// variables keep working. With it on, the batch size is bounded so one export request
// cannot exceed what a collector will accept, since a refused request loses the whole
// batch rather than the span that made it too big.
//
// The queue size is deliberately left alone. Lowering it would make drops more likely,
// and under delta message capture a dropped chat span is a hole no other span can fill;
// raising it raises resident memory, which with capture on is already up to the queue
// size times two content attributes. The docs state both rather than this guessing.
func batchOptions(r Resolved) []sdktrace.BatchSpanProcessorOption {
	if !r.Capture.Value {
		return nil
	}

	return []sdktrace.BatchSpanProcessorOption{sdktrace.WithMaxExportBatchSize(r.ExportBatch.Value)}
}

// traceOptions builds the trace exporter options. See Setup for why an endpoint is
// set only when the config named one.
func traceOptions(r Resolved) []otlptracehttp.Option {
	var opts []otlptracehttp.Option

	// Content capture is the only thing here that makes a request large enough for
	// compression to matter, and JSON transcripts compress several-fold. It buys more
	// headroom against a collector's request limit than a smaller batch does, and
	// unlike a smaller batch it does not increase how often the span queue overflows.
	if r.Capture.Value {
		opts = append(opts, otlptracehttp.WithCompression(otlptracehttp.GzipCompression))
	}

	if !r.EndpointFromConfig {
		return opts
	}

	return append(opts, otlptracehttp.WithEndpointURL(signalEndpoint(r.Endpoint.Value, tracesPath)))
}

// metricOptions is traceOptions for the metric signal.
func metricOptions(r Resolved) []otlpmetrichttp.Option {
	if !r.EndpointFromConfig {
		return nil
	}

	return []otlpmetrichttp.Option{otlpmetrichttp.WithEndpointURL(signalEndpoint(r.Endpoint.Value, metricsPath))}
}

// The per-signal paths OTLP/HTTP defines, appended to the configured base URL.
const (
	tracesPath  = "/v1/traces"
	metricsPath = "/v1/metrics"
)

// signalEndpoint appends a signal's path to the configured base endpoint.
//
// telemetry.endpoint is a base URL, matching OTEL_EXPORTER_OTLP_ENDPOINT, so the signal
// path is this package's to add. The exporters will not do it: WithEndpointURL treats a
// URL with no path as targeting the root, deliberately, so handing it a bare base URL
// posts every export to / and a collector answers 404. That reads as a broken collector
// rather than a misconfigured client, and OTLP being fire and forget it is invisible
// without the delivery counts.
//
// A base URL carrying a path prefix keeps it, so a collector mounted at /otlp receives
// /otlp/v1/traces, which is what the OTLP specification says the base variable means.
// An unparseable endpoint is returned unchanged rather than mangled; Resolve has already
// rejected those, and this is not the place to discover it.
func signalEndpoint(base string, signalPath string) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}

	u.Path = strings.TrimSuffix(u.Path, "/") + signalPath

	return u.String()
}

// NewFromProviders builds a Provider over providers the caller already runs. It is
// the one place OpenTelemetry types appear in this package's exported API, which is
// what lets an embedder who does not run OpenTelemetry avoid importing it at all.
//
// The caller owns both providers: Shutdown flushes nothing here and returns an empty
// Delivery, since this package neither built the exporters nor can count what they
// sent. Passing nil for either leaves that signal unrecorded.
func NewFromProviders(tp trace.TracerProvider, mp metric.MeterProvider, opts ...Option) *Provider {
	p := &Provider{delivery: &delivery{}}
	for _, opt := range opts {
		opt(p)
	}
	if tp != nil {
		p.tracer = tp.Tracer(scopeName)
	}
	if mp != nil {
		p.meter = mp.Meter(scopeName)
		// An instrument creation failure here is swallowed rather than returned: this
		// constructor has no error to give, and an embedder who handed in a working meter
		// provider should not lose their traces because one histogram name was rejected.
		// A nil instruments set makes every recording path a no-op.
		p.instruments, _ = newInstruments(p.meter)
	}

	return p
}

// Option configures a Provider built by NewFromProviders. The CLI does not use these:
// it resolves a configuration and calls Setup, which is where every value gets an
// origin an operator can be shown.
type Option func(*Provider)

// ContentCapture asks for the conversation itself to be exported, not only the
// structure and timing of a run.
type ContentCapture struct {
	// Full exports the whole conversation on every model call rather than each call's
	// delta. It is quadratic in the length of the conversation; see the config
	// documentation for why the delta is the default.
	Full bool
	// MaxBytes caps each content attribute. Zero takes the default.
	MaxBytes int
}

// WithExportErrorHandler reports each export failure to fn as it happens.
//
// It is per Provider, and that is the reason it exists rather than being left to
// OpenTelemetry's own handler: that handler is process-global, so in a process running
// several agents the last one started silently takes over the reporting of every other,
// and a run whose export is broken reports it through a sink belonging to a different
// run. Nothing here registers anything globally, and this keeps the diagnostics on the
// same footing as the spans.
//
// fn is called from the exporter's goroutine, once per failed export, with the delivery
// lock held. It must not call back into this Provider and it must be safe for concurrent
// use if the same fn is given to more than one.
//
// It does not carry OpenTelemetry's internal logging, which has no per-provider channel
// at all. See SetErrorHandler for that and for what it costs.
func WithExportErrorHandler(fn func(error)) Option {
	return func(p *Provider) {
		p.delivery.onError = fn
	}
}

// WithContentCapture turns content capture on for an embedder's own Provider.
//
// It exists as an explicit option rather than as a field because of what it does: the
// system prompt, the conversation, the model's replies, tool arguments and tool
// results are exported as span attributes, so whoever can read the traces can read the
// conversation, and an export cannot be recalled. Tool results are the verbatim output
// of whatever command the model ran. An embedder has to ask for that by name.
//
// A Provider built without it captures nothing, which is why this package does not
// export an embedder's conversation on their behalf just because they handed in a
// tracer.
func WithContentCapture(c ContentCapture) Option {
	return func(p *Provider) {
		p.capture = capture{enabled: true, full: c.Full, maxBytes: c.MaxBytes}
		if p.capture.maxBytes <= 0 {
			p.capture.maxBytes = defaultMaxContentBytes
		}
	}
}

// Enabled reports whether this Provider records anything. It exists for the
// occasional caller that wants to skip building an argument that would be discarded,
// never as a gate on whether to call a facade method.
func (p *Provider) Enabled() bool {
	return p != nil && p.tracer != nil
}

// CaptureEnabled reports whether this Provider exports the conversation itself and not
// only the structure and timing of a run.
//
// It is here for the operator-facing surfaces that have to say so: the startup card and
// the run summary. They read it off the Provider rather than off the config for the
// reason gen_ai.provider.name is read off the backend, and it matters more here: a
// --no-telemetry veto, a rejected endpoint or an embedder's own Provider all make the
// config and what is actually happening disagree, and a privacy marker that reports the
// config is a privacy marker that can be wrong in the direction that matters.
func (p *Provider) CaptureEnabled() bool {
	return p.Enabled() && p.capture.enabled
}

// Shutdown flushes and stops the pipelines and reports what was delivered.
//
// The returned error is about shutting down, not about exporting. A collector that
// rejected the data does not surface here at all: the batch processor hands its
// export errors to the process-global error handler and returns nothing to the
// caller, so a run whose every span was refused with a 401 shuts down with a nil
// error. That silent swallow is the whole reason the returned Delivery exists, and a
// caller that checks only the error learns nothing. Check Delivery.Complete.
//
// Its context must never be derived from the run's: deriving it would mean an
// interrupt cancels the flush and loses exactly the run the operator wanted to see.
// The caller passes a fresh context with its own timeout.
func (p *Provider) Shutdown(ctx context.Context) (Delivery, error) {
	if p == nil {
		return Delivery{}, nil
	}

	var errs []error
	if p.tp != nil {
		err := p.tp.Shutdown(ctx)
		if err != nil {
			errs = append(errs, err)
		}
	}
	if p.mp != nil {
		err := p.mp.Shutdown(ctx)
		if err != nil {
			errs = append(errs, err)
		}
	}

	d := p.delivery.snapshot()
	d.Endpoint = p.endpoint

	return d, errors.Join(errs...)
}

// SetErrorHandler routes OpenTelemetry's own diagnostics to w.
//
// This is the only process-global state this package touches and nothing here calls it.
// Calling it replaces the handler and the logger for the whole process, so in a process
// running several agents the last caller wins and every other agent's diagnostics go to
// that caller's writer. A caller running several agents should reach for
// WithExportErrorHandler, which is per Provider.
//
// A caller that owns the terminal should install this anyway, and that is the case the
// per-provider handler does not cover. The SDK hands export failures to the global
// handler independently of any per-provider channel, and with nothing installed the
// default writes them through the log package, whose standard logger captured os.Stderr
// at its own init. A program that replaces the os.Stderr variable to keep writes off a
// full-screen display therefore does not redirect them. Installing both channels is not
// the answer either: the same failure would then be reported twice, once here and once
// through WithExportErrorHandler.
//
// w must never be a command's protocol channel; under a stdio protocol that would
// corrupt the stream. ErrorBuffer is the destination for a caller that cannot be written
// to at an arbitrary moment.
//
// It sets both of OpenTelemetry's diagnostic channels. The error handler is the one
// that carries a failing export, and is what an operator actually meets. The internal
// logger is set for the same redirection reason rather than for its payload: its
// default is built at package init as stdr over log.New(os.Stderr, ...)
// (otel/internal/global/internal_logging.go), capturing the os.Stderr *value*, so a
// caller who later replaces the os.Stderr variable does not redirect it. A full-screen
// UI does exactly that to keep writes off the alt-screen, so anything the SDK logs
// internally would otherwise reach the real terminal, corrupt the display, and be
// painted over before it could be read.
//
// How little that logger actually says is worth recording, because the obvious guess is
// wrong. stdr runs at verbosity 0, so the SDK's V(1) and V(8) calls never emit, and
// spans the batch processor dropped on a full queue are among them: a drop is a counter
// increment reported only through a V(8) line nobody sees. What reaches verbosity 0 is
// the SDK's own error-level logging, chiefly exporter environment misconfiguration.
//
// Not asserted by a spec, and worth saying so rather than writing one that passes for
// the wrong reason: the SDK reaches its internal logger through an internal package,
// so nothing here can make it log on demand, and a spec driving a real export failure
// exercises the error handler instead and would pass with this line deleted.
//
// The returned restore puts the error handler back to what it was, which is what a test
// or a short-lived embedded run needs so its writer stops being written to after it goes
// away. It is not symmetric, and the asymmetry is OpenTelemetry's: there is an exported
// getter for the error handler and none for the logger, so the previous logger cannot be
// read and therefore cannot be put back. Restore points the logger at io.Discard
// instead, on the reasoning that a logger writing nowhere is always safe while one
// writing to a dead buffer is the failure this exists to prevent. Calling restore is
// optional; a command that owns the process to its end has nothing to restore to.
func SetErrorHandler(w io.Writer) (restore func()) {
	previous := otel.GetErrorHandler()

	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		fmt.Fprintf(w, "warning: telemetry export error: %v\n", err)
	}))

	// No flags: the timestamp and file position the SDK's default carries are noise
	// next to the run's own output, and the line is already prefixed as a warning.
	otel.SetLogger(stdr.New(log.New(w, "warning: telemetry: ", 0)))

	return func() {
		otel.SetErrorHandler(previous)
		otel.SetLogger(stdr.New(log.New(io.Discard, "", 0)))
	}
}

// delivery tallies what the exporters were handed and what they got through. It is
// written from the batch processor's own goroutine and read at shutdown, so it takes
// a lock rather than assuming the run goroutine's single-writer invariant.
type delivery struct {
	mu                     sync.Mutex
	spansAttempted         int64
	spansDelivered         int64
	metricExportsAttempted int64
	metricExportsDelivered int64
	err                    error

	// onError reports each export failure as it happens, and is nil when nobody asked.
	// It is per Provider, which is the whole point: a process running several agents
	// gives each its own, where OpenTelemetry's process-global handler would have the
	// last one started silently take over the reporting of every other.
	//
	// It is called with the lock held, so an implementation must not call back into
	// this Provider. That is stated rather than defended against: the callers are a
	// writer and a buffer, and a lock-free version would have to copy the handler on
	// every export to avoid racing the constructor.
	onError func(error)
}

// report hands an export failure to the per-provider handler.
func (d *delivery) report(err error) {
	if d.onError == nil {
		return
	}

	d.onError(err)
}

// recordSpans tallies one span export attempt and its outcome.
func (d *delivery) recordSpans(n int64, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.spansAttempted += n
	if err != nil {
		if d.err == nil {
			d.err = err
		}
		d.report(err)
		return
	}
	d.spansDelivered += n
}

// recordMetricExport tallies one metric export request and its outcome.
func (d *delivery) recordMetricExport(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.metricExportsAttempted++
	if err != nil {
		if d.err == nil {
			d.err = err
		}
		d.report(err)
		return
	}
	d.metricExportsDelivered++
}

// snapshot copies the tally for reporting.
func (d *delivery) snapshot() Delivery {
	d.mu.Lock()
	defer d.mu.Unlock()

	return Delivery{
		SpansAttempted:         d.spansAttempted,
		SpansDelivered:         d.spansDelivered,
		MetricExportsAttempted: d.metricExportsAttempted,
		MetricExportsDelivered: d.metricExportsDelivered,
		Err:                    d.err,
	}
}

// countingSpanExporter counts what passes through the real exporter. The OTLP
// exporters report neither a delivered count nor a first error to the application, so
// without this a 401 from the collector is invisible to the run that caused it.
type countingSpanExporter struct {
	sdktrace.SpanExporter
	delivery *delivery
}

func (e *countingSpanExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	err := e.SpanExporter.ExportSpans(ctx, spans)
	e.delivery.recordSpans(int64(len(spans)), err)

	return err
}

// countingMetricExporter is countingSpanExporter for the metric signal, and it also
// drops empty collections before they reach the wire.
type countingMetricExporter struct {
	sdkmetric.Exporter
	delivery *delivery
}

// Export sends a collection on, unless it carries no data points.
//
// The periodic reader collects on a timer and hands over whatever it found, including
// nothing, and the OTLP exporter posts that empty payload rather than skipping it.
// Collectors are entitled to reject it: telemetry.dev answers 400 "Request contains no
// metric data points", so a run that recorded no metrics ends with an export failure
// warning that describes nothing wrong. Dropping the empty collection here also spares
// a pointless request every collection interval on a quiet run.
//
// It is not counted either way. An export that was never worth making is not an
// attempt, and counting it would make Delivery.Attempted true for a run that recorded
// nothing, which is exactly the case the caller uses that to stay quiet about.
func (e *countingMetricExporter) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	if !hasMetricData(rm) {
		return nil
	}

	err := e.Exporter.Export(ctx, rm)
	e.delivery.recordMetricExport(err)

	return err
}

// hasMetricData reports whether a collection carries at least one metric. A reader
// with no instruments produces no scopes; one whose instruments recorded nothing this
// interval can produce a scope with no metrics in it, so both are checked.
func hasMetricData(rm *metricdata.ResourceMetrics) bool {
	if rm == nil {
		return false
	}

	for _, scope := range rm.ScopeMetrics {
		if len(scope.Metrics) > 0 {
			return true
		}
	}

	return false
}
