//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// The environment variables that configure transport. They are the standard OTEL_*
// names, so an operator who already runs OpenTelemetry configures this the way they
// configure everything else, and no bearer token ever has to appear in the agent's
// YAML.
const (
	// EnvEndpoint is the OTLP base endpoint for every signal.
	EnvEndpoint = "OTEL_EXPORTER_OTLP_ENDPOINT"
	// EnvTracesEndpoint is the trace-specific endpoint, which unlike EnvEndpoint is a
	// full URL including the signal path.
	EnvTracesEndpoint = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"
	// EnvMetricsEndpoint is the metric-specific endpoint, on the same terms.
	EnvMetricsEndpoint = "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"
	// EnvProtocol selects the OTLP transport. This build speaks HTTP only.
	EnvProtocol = "OTEL_EXPORTER_OTLP_PROTOCOL"
	// EnvTracesProtocol is the trace-specific transport selector.
	EnvTracesProtocol = "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"
	// EnvMetricsProtocol is the metric-specific transport selector.
	EnvMetricsProtocol = "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL"
	// EnvServiceName names the service in the exported resource.
	EnvServiceName = "OTEL_SERVICE_NAME"
	// EnvSDKDisabled is the standard absolute off switch.
	EnvSDKDisabled = "OTEL_SDK_DISABLED"
	// EnvNoTelemetry is this repository's own off switch, the environment binding of
	// the --no-telemetry flag.
	EnvNoTelemetry = "NO_TELEMETRY"
	// EnvSpanAttrValueLimit and EnvAttrValueLimit cap the length of an exported
	// attribute value, the second being the fallback the SDK applies when the
	// span-scoped one is unset.
	//
	// They matter to content capture and to nothing else here. The SDK enforces them
	// by cutting the value at that many characters, which for an ordinary attribute is
	// exactly right and for a JSON document is not: the cut lands mid-document and
	// leaves an unterminated string inside an unterminated array, so a backend that
	// parses the content attributes fails on precisely the largest and most
	// interesting ones. Rather than override the operator, content capture lowers its
	// own budget to fit, and shortens structurally so what arrives still parses.
	EnvSpanAttrValueLimit = "OTEL_SPAN_ATTRIBUTE_VALUE_LENGTH_LIMIT"
	EnvAttrValueLimit     = "OTEL_ATTRIBUTE_VALUE_LENGTH_LIMIT"
)

// headerEnvNames are the OTLP variables that carry a bearer token. They are listed
// here because two startup checks need them: rejecting a plain-http endpoint to a
// non-loopback host while one is set, and warning when transport is configured but
// telemetry is off.
//
// Only these four are here. The mTLS variables are not, and their absence is not an
// oversight: they name a client certificate and key used to establish TLS, so over a
// plain-http endpoint they are simply unused rather than sent in the clear, and the
// check above has nothing to say about them. config.CredentialEnvNames carries these
// four plus the mTLS set, because a tool subprocess should see none of them.
//
// That overlap is the duplication config cannot avoid: it is the lowest layer and
// imports nothing from the tree, the same trade it already makes for the exposable
// built-in names. HeaderEnvNames exists so config's spec can assert against this list
// rather than against a third hand-written copy of it.
var headerEnvNames = []string{
	"OTEL_EXPORTER_OTLP_HEADERS",
	"OTEL_EXPORTER_OTLP_TRACES_HEADERS",
	"OTEL_EXPORTER_OTLP_METRICS_HEADERS",
	"OTEL_EXPORTER_OTLP_LOGS_HEADERS",
}

// HeaderEnvNames returns the OTLP variables that carry a bearer token, the subset of
// the export credentials this package reasons about.
//
// It exists for the spec in config that pins config.CredentialEnvNames as a superset
// of this list, so adding a name here without adding it there fails a test rather
// than quietly leaving a credential readable by a tool subprocess. It returns a copy:
// a caller must not be able to shorten the list a security check runs over.
func HeaderEnvNames() []string {
	return slices.Clone(headerEnvNames)
}

// transportEnvNames are the variables whose presence means an operator has
// configured OTLP transport somewhere in this process's environment. A run that
// resolves to off while one of them is set prints a note, because a host-wide
// endpoint that silently exports nothing is indistinguishable from one that works.
var transportEnvNames = append([]string{
	EnvEndpoint,
	EnvTracesEndpoint,
	EnvMetricsEndpoint,
	EnvProtocol,
	EnvTracesProtocol,
	EnvMetricsProtocol,
}, headerEnvNames...)

// defaultEndpoint is the OTLP/HTTP endpoint the OpenTelemetry SDK falls back to when
// nothing names one. It is repeated here so Resolve can report an effective value
// and its origin rather than an empty string.
const defaultEndpoint = "http://localhost:4318"

// defaultServiceName is the service name used when neither the config, the
// environment, nor an agent identity supplies one.
const defaultServiceName = "fisk-ai"

// otlpHTTPPort and otlpGRPCPort are the two standard OTLP ports. Pointing an
// OTLP/HTTP build at the gRPC port is the single most common mistake in this area
// and fails in a way that looks like a network problem, so it is checked by name.
const (
	otlpHTTPPort = "4318"
	otlpGRPCPort = "4317"
)

// Settings is the telemetry block as the agent configuration file states it, plus
// the two things the command resolves rather than the file: the agent identity used
// as a service-name fallback, and the --no-telemetry veto.
//
// It is primitives rather than a config type because this package imports nothing
// from the rest of the tree (see the package documentation). The caller maps its
// config object onto this, the same handful of fields the run path already maps for
// every other injected dependency.
type Settings struct {
	// Enabled is telemetry.enabled. Nothing is exported unless it is true.
	Enabled bool
	// Endpoint is telemetry.endpoint, an OTLP/HTTP base URL. Empty leaves the
	// endpoint to the standard environment variables.
	Endpoint string
	// ServiceName is telemetry.service_name. Empty falls back through
	// OTEL_SERVICE_NAME, then Identity, then fisk-ai.
	ServiceName string
	// SampleRatio is telemetry.sample_ratio. It is a pointer because zero is a
	// meaningful value: a plain float64 would make "sample nothing" indistinguishable
	// from an absent key and would be defaulted back to sampling everything.
	SampleRatio *float64
	// NoMetrics is telemetry.no_metrics. Metrics are on with telemetry; this is the
	// off switch.
	NoMetrics bool

	// Capture is telemetry.capture: whether the conversation itself is exported, and
	// how much of it. Everything under it is inert while Capture.Enabled is false.
	Capture CaptureSettings

	// Identity is the agent identity, used as the service name when neither the
	// config nor OTEL_SERVICE_NAME names one.
	Identity string

	// DisabledBy is a caller-supplied veto, and its value is the label the caller wants
	// that veto reported as: a CLI passes its flag name, a server passes whatever it
	// calls the switch. Empty means the caller is not vetoing. It is absolute, because
	// what has to be suppressible at the last minute is export, not configuration.
	//
	// It is a label rather than a bool because this package must not name a flag it
	// does not own. The previous shape hardcoded "--no-telemetry" here, which made the
	// library a place this repository's CLI vocabulary leaked into, and left a caller
	// with no way to describe its own veto.
	DisabledBy string
}

// MessagesMode selects how much of the conversation each model call carries.
//
// It is a struct rather than a defined string type for the reason section 3.1 of the
// plan records against ErrorClass: a defined string type is convertible from any string
// by any package, so MessagesMode("detla") would compile and the typo would survive to
// Resolve exactly as it does today. A struct with no exported field cannot be built by
// a caller at all, so the only ways to hold one are the two values below and
// ParseMessagesMode, which exists precisely because operator input can be wrong.
//
// The zero value means the operator chose nothing, which resolves to the delta.
type MessagesMode struct{ s string }

// The values telemetry.capture.messages accepts. They are the same two words the
// resolved configuration reports, deliberately: an operator who sees a delta in a
// backend can search their config file for the literal word rather than having to
// translate a boolean into it.
var (
	// MessagesDelta exports only what each model call added to the conversation.
	MessagesDelta = MessagesMode{"delta"}
	// MessagesFull exports the whole conversation on every model call.
	MessagesFull = MessagesMode{"full"}
)

// ParseMessagesMode maps a configured value onto a mode.
//
// It accepts anything, including a value this build does not understand, and preserves
// the text. That is deliberate: the value comes from a file an operator wrote, so
// rejecting it here would either lose what they typed or move validation away from
// Resolve, which is the one place this package validates anything. Validity is Valid's
// question and reporting it is validateCapture's.
func ParseMessagesMode(s string) MessagesMode {
	return MessagesMode{s}
}

// String returns the configured word, or "" when nothing was configured.
func (m MessagesMode) String() string { return m.s }

// Set reports whether a mode was configured at all.
func (m MessagesMode) Set() bool { return m.s != "" }

// Valid reports whether this is a mode this build understands.
func (m MessagesMode) Valid() bool { return m == MessagesDelta || m == MessagesFull }

// The bounds on telemetry.capture.max_bytes. The floor is where a document becomes
// more truncation marker than content; the ceiling is where one attribute starts to
// threaten a collector's request limit whatever the batch size.
const (
	defaultMaxContentBytes = 8192
	minMaxContentBytes     = 256
	maxMaxContentBytes     = 65536
)

// CaptureSettings is the telemetry.capture block as the file states it.
//
// Content capture is the only part of this work that sends the conversation off-box,
// so every field here is inert unless Enabled is true. That includes the validation:
// a file with capture off and a nonsense max_bytes starts fine, on the same reasoning
// section 8 applies to a stale endpoint. An operator turning capture off mid-incident
// must not be stopped by a key that no longer means anything.
type CaptureSettings struct {
	// Enabled exports prompts, model output, tool arguments and tool results.
	Enabled bool
	// Messages selects the whole conversation or each call's delta. The zero value
	// means the delta, which is the default because the alternative re-exports a
	// growing transcript on every iteration. A caller mapping a config file onto this
	// uses ParseMessagesMode.
	Messages MessagesMode
	// MaxBytes caps each content attribute. Zero means unset, and resolves to the
	// default rather than to no cap: an unbounded content attribute is how a batch
	// gets refused whole.
	MaxBytes int
}

// Setting is one effective value and where it came from, so fisk info can show an
// operator not just what telemetry will do but which config key or environment
// variable decided it.
type Setting[T any] struct {
	Value T
	// Origin is a display label naming what decided Value: an environment variable
	// name, the string "default", or one of this repository's telemetry config keys
	// such as "telemetry.sample_ratio".
	//
	// Those key paths are the documented names of the fields on config.Config that a
	// caller filled in, not a file this package assumes exists, so they read correctly
	// whether the value came from YAML or was set in Go. It is for showing to a person
	// and nothing branches on it: an origin is not a stable identifier and its wording
	// may change.
	Origin string
}

// Resolved is the effective telemetry configuration: what Setup will do, each value
// paired with its origin.
type Resolved struct {
	// Enabled reports whether anything is exported at all.
	Enabled bool
	// DisabledBy names what actively turned telemetry off: a standard environment
	// variable, or the label the caller passed as Settings.DisabledBy. It is empty both
	// when telemetry is on and when it was simply never enabled.
	DisabledBy string
	// NotEnabled reports that nothing vetoed telemetry, it was just never turned on.
	//
	// It is separate from DisabledBy because they are the same outcome and very
	// different mistakes, and because only the caller can name the setting that should
	// have enabled it or the file that setting lives in.
	NotEnabled bool

	Endpoint    Setting[string]
	ServiceName Setting[string]
	SampleRatio Setting[float64]
	// Metrics reports whether the metric pipeline runs.
	Metrics Setting[bool]

	// Capture reports whether the conversation is exported, how much of each call is
	// carried, and the per-attribute cap. Each is resolved and defaulted here so fisk
	// info can show an operator what will actually be sent rather than what the file
	// happened to say.
	Capture  Setting[bool]
	Messages Setting[MessagesMode]
	MaxBytes Setting[int]
	// ExportBatch is how many spans go in one export request. It is derived rather
	// than configured, and it is on Resolved so fisk info can show it: a computed
	// value that nothing displays is one an operator cannot reason about, and this one
	// changes underneath them when they raise the content cap.
	ExportBatch Setting[int]

	// TransportEnvSet lists the OTEL_EXPORTER_OTLP_* variables found set in the
	// environment, in the order of transportEnvNames. It drives the note printed when
	// transport is configured but telemetry resolved to off.
	TransportEnvSet []string
	// HeadersSet reports whether any variable carrying a bearer token is set, which
	// is what makes a plain-http endpoint to a remote host a credential leak.
	HeadersSet bool
	// EndpointFromConfig reports that the endpoint came from the config file rather
	// than the environment. Setup passes an explicit endpoint to the exporters only in
	// that case, leaving the full standard environment handling to the SDK otherwise.
	EndpointFromConfig bool
}

// Resolve computes the effective telemetry configuration from the config block and
// the environment, and validates it. env reads one environment variable, injected so
// a caller can resolve against something other than the process environment and so
// specs never mutate process state.
//
// Enablement is absolute and has no middle ground: --no-telemetry, NO_TELEMETRY or
// OTEL_SDK_DISABLED=true turns it off whatever the config says; telemetry.enabled
// turns it on; anything else leaves it off even when OTEL_EXPORTER_OTLP_* is set,
// because a host-wide collector endpoint must not silently turn every fisk-ai
// process into an exporter.
//
// Validation runs only for a configuration that resolves to on, so a stale endpoint
// in a file with telemetry off never fails a run. The returned Resolved is complete
// even when the error is non-nil, so fisk info can show what was resolved alongside
// what was wrong with it.
func Resolve(s Settings, env func(string) string) (Resolved, error) {
	if env == nil {
		env = func(string) string { return "" }
	}

	r := Resolved{
		Enabled: s.Enabled,
		Metrics: Setting[bool]{Value: !s.NoMetrics, Origin: "default"},
	}
	if s.NoMetrics {
		r.Metrics.Origin = "telemetry.no_metrics"
	}

	for _, name := range transportEnvNames {
		if env(name) != "" {
			r.TransportEnvSet = append(r.TransportEnvSet, name)
		}
	}
	for _, name := range headerEnvNames {
		if env(name) != "" {
			r.HeadersSet = true
			break
		}
	}

	switch {
	case envIsTrue(env(EnvNoTelemetry)):
		r.Enabled = false
		r.DisabledBy = EnvNoTelemetry
	case s.DisabledBy != "":
		r.Enabled = false
		r.DisabledBy = s.DisabledBy
	case sdkDisabled(env(EnvSDKDisabled)):
		r.Enabled = false
		r.DisabledBy = EnvSDKDisabled
	case !s.Enabled:
		// Nothing vetoed it; it was simply never turned on. That is reported as its own
		// state rather than as a DisabledBy naming a config key, because the key's name
		// and the file it lives in belong to the caller, and a caller that had to
		// string-match this value to tell the two apart was matching on a display string
		// nobody had promised to keep.
		r.NotEnabled = true
	}

	r.Endpoint, r.EndpointFromConfig = resolveEndpoint(s, env)
	r.ServiceName = resolveServiceName(s, env)
	r.SampleRatio = resolveSampleRatio(s)
	r.Capture, r.Messages, r.MaxBytes = resolveCapture(s, env)

	// Capture without export does nothing, so a run that is not exporting is not
	// capturing however the file reads. This is not tidiness: every operator-facing
	// surface reports this value, so leaving it true under a veto would warn at startup
	// about an export that is not happening and show "content capture: on" in fisk info
	// for a run whose spans go nowhere. A privacy marker that overstates is as broken as
	// one that understates.
	if !r.Enabled {
		r.Capture = Setting[bool]{Origin: r.DisabledBy}
	}

	r.ExportBatch = resolveExportBatch(r)

	if !r.Enabled {
		return r, nil
	}

	err := validate(r, env)
	if err != nil {
		return r, err
	}

	return r, nil
}

// resolveEndpoint picks the endpoint traces will go to and reports whether it came
// from the config file. The trace-specific variable is preferred over the base one
// because that is the precedence the SDK itself applies, so what is displayed and
// validated is what will actually be used.
func resolveEndpoint(s Settings, env func(string) string) (Setting[string], bool) {
	if s.Endpoint != "" {
		return Setting[string]{Value: s.Endpoint, Origin: "telemetry.endpoint"}, true
	}
	if v := env(EnvTracesEndpoint); v != "" {
		return Setting[string]{Value: v, Origin: EnvTracesEndpoint}, false
	}
	if v := env(EnvEndpoint); v != "" {
		return Setting[string]{Value: v, Origin: EnvEndpoint}, false
	}

	return Setting[string]{Value: defaultEndpoint, Origin: "default"}, false
}

// resolveServiceName applies the service-name precedence. OTEL_SERVICE_NAME sits
// above the identity because an operator who set it in a systemd unit or a Kubernetes
// manifest said something explicit, while the identity is only a good default for
// when nobody did; an explicit telemetry.service_name is more specific still. The
// identity reaches the backend either way, as gen_ai.agent.name.
func resolveServiceName(s Settings, env func(string) string) Setting[string] {
	if s.ServiceName != "" {
		return Setting[string]{Value: s.ServiceName, Origin: "telemetry.service_name"}
	}
	if v := env(EnvServiceName); v != "" {
		return Setting[string]{Value: v, Origin: EnvServiceName}
	}
	if s.Identity != "" {
		return Setting[string]{Value: s.Identity, Origin: "identity"}
	}

	return Setting[string]{Value: defaultServiceName, Origin: "default"}
}

// resolveSampleRatio reads the sampling ratio, treating an absent key as 1.0. The
// pointer is what makes that possible: an explicit 0 means sample nothing and must
// survive, where a plain float64 would arrive as the zero value and be defaulted
// straight back to sampling everything, sending every trace to a paid backend.
func resolveSampleRatio(s Settings) Setting[float64] {
	if s.SampleRatio == nil {
		return Setting[float64]{Value: 1.0, Origin: "default"}
	}

	return Setting[float64]{Value: *s.SampleRatio, Origin: "telemetry.sample_ratio"}
}

// resolveCapture reads the content-capture block and defaults it.
//
// Defaulting happens here, before validation, and that ordering is the point: an
// absent max_bytes arrives as the Go zero value, so validating first would reject
// every configuration that did not set a key it had no reason to set. Unlike
// sample_ratio, zero carries no meaning of its own here, so a plain int with a default
// is right where that one needed a pointer.
func resolveCapture(s Settings, env func(string) string) (Setting[bool], Setting[MessagesMode], Setting[int]) {
	capture := Setting[bool]{Value: s.Capture.Enabled, Origin: "default"}
	if s.Capture.Enabled {
		capture.Origin = "telemetry.capture.enabled"
	}

	messages := Setting[MessagesMode]{Value: MessagesDelta, Origin: "default"}
	if s.Capture.Messages.Set() {
		messages = Setting[MessagesMode]{Value: s.Capture.Messages, Origin: "telemetry.capture.messages"}
	}

	maxBytes := Setting[int]{Value: defaultMaxContentBytes, Origin: "default"}
	if s.Capture.MaxBytes != 0 {
		maxBytes = Setting[int]{Value: s.Capture.MaxBytes, Origin: "telemetry.capture.max_bytes"}
	}

	// An operator's attribute-value limit lowers the budget rather than being
	// overridden by it. The SDK would otherwise enforce that limit itself by cutting
	// the encoded document at a character boundary, which keeps the string valid UTF-8
	// and leaves the JSON unparseable; shortening structurally to fit means what
	// arrives is both within their limit and still a document. The comparison is safe
	// across units because the budget is bytes and the limit is characters, and a
	// document within N bytes is within N characters.
	limit, name := attrValueLimit(env)
	if limit > 0 && limit < maxBytes.Value {
		maxBytes = Setting[int]{Value: limit, Origin: name}
	}

	return capture, messages, maxBytes
}

// The export batch sizing. contentAttrsPerSpan is the most content attributes any one
// span carries: two on a chat span (input and output), two on a tool span (arguments
// and result), and one on startup. It is a constant here rather than a number restated
// in a comment because it is the divisor below, so a fifth content attribute added
// later must change it; the spec counts the content fields actually declared and fails
// when it drifts.
const (
	contentAttrsPerSpan = 2
	targetBatchBytes    = 2 << 20
	minExportBatch      = 16
	maxExportBatch      = 512
)

// resolveExportBatch derives how many spans may go in one export request.
//
// The SDK's default is 512 and otlptracehttp does not split a batch, so with content
// capture on, 512 spans carrying two 8 KiB attributes each is an 8 MB request. A
// collector that refuses it refuses the whole batch, and OTLP being fire and forget
// that is very close to invisible.
//
// It is computed rather than fixed because the failure appears exactly when an operator
// raises the content cap, which is when a hardcoded number would quietly stop
// protecting them. With capture off nothing here applies and the SDK's own handling,
// including OTEL_BSP_MAX_EXPORT_BATCH_SIZE, is left alone.
func resolveExportBatch(r Resolved) Setting[int] {
	if !r.Capture.Value {
		return Setting[int]{Value: maxExportBatch, Origin: "default"}
	}

	n := targetBatchBytes / (contentAttrsPerSpan * r.MaxBytes.Value)
	n = min(max(n, minExportBatch), maxExportBatch)

	return Setting[int]{Value: n, Origin: "derived from " + r.MaxBytes.Origin}
}

// attrValueLimit reads the SDK's attribute-value length limit and names which
// variable supplied it, applying the SDK's own precedence. A missing or unparsable
// value is no limit, matching the SDK's default of unlimited.
func attrValueLimit(env func(string) string) (int, string) {
	for _, name := range []string{EnvSpanAttrValueLimit, EnvAttrValueLimit} {
		raw := strings.TrimSpace(env(name))
		if raw == "" {
			continue
		}

		v, err := strconv.Atoi(raw)
		if err != nil {
			continue
		}

		return v, name
	}

	return 0, ""
}

// validate applies the startup checks. OTLP/HTTP is connectionless, so nothing here
// proves the collector is reachable; these are the mistakes that are knowable
// locally, and each message names the fix rather than only the problem.
func validate(r Resolved, env func(string) string) error {
	err := validateProtocol(env)
	if err != nil {
		return err
	}

	if r.SampleRatio.Value < 0 || r.SampleRatio.Value > 1 {
		return fmt.Errorf("invalid %s %v: must be between 0 and 1", r.SampleRatio.Origin, r.SampleRatio.Value)
	}

	err = validateCapture(r)
	if err != nil {
		return err
	}

	return validateEndpoint(r)
}

// validateCapture checks the content-capture block, and only when capture is on: with
// it off none of these values will be read, and failing a run over a setting that does
// nothing punishes the operator who just turned capture off.
func validateCapture(r Resolved) error {
	if !r.Capture.Value {
		return nil
	}

	if !r.Messages.Value.Valid() {
		return fmt.Errorf("invalid %s %q: must be %q or %q", r.Messages.Origin, r.Messages.Value, MessagesDelta, MessagesFull)
	}

	if r.MaxBytes.Value < minMaxContentBytes || r.MaxBytes.Value > maxMaxContentBytes {
		return fmt.Errorf("invalid %s %d: must be between %d and %d, default %d", r.MaxBytes.Origin, r.MaxBytes.Value, minMaxContentBytes, maxMaxContentBytes, defaultMaxContentBytes)
	}

	return nil
}

// validateProtocol rejects a gRPC transport selection. This build speaks OTLP/HTTP
// only, so a grpc value would otherwise be silently ignored and the operator would
// watch traffic go to the wrong port.
func validateProtocol(env func(string) string) error {
	for _, name := range []string{EnvProtocol, EnvTracesProtocol, EnvMetricsProtocol} {
		v := strings.TrimSpace(env(name))
		if v == "" || !strings.EqualFold(v, "grpc") {
			continue
		}

		return fmt.Errorf("%s is %q but this build speaks OTLP/HTTP only; set it to http/protobuf and use the OTLP/HTTP port (%s)", name, v, otlpHTTPPort)
	}

	return nil
}

// validateEndpoint checks the endpoint the exporters will use. The loopback rule is
// the same one util.ValidateBaseURL applies to --base-url, restated here rather than
// imported because this package stays a leaf; the wording is kept close so an
// operator meets one rule, not two.
func validateEndpoint(r Resolved) error {
	raw := r.Endpoint.Value
	label := r.Endpoint.Origin
	if label == "default" {
		label = "telemetry endpoint"
	}

	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid %s %q: %w", label, raw, err)
	}
	if u.User != nil {
		return fmt.Errorf("invalid %s %q: must not embed userinfo credentials", label, raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("invalid %s %q: scheme must be http or https", label, raw)
	}

	if u.Port() == otlpGRPCPort {
		return fmt.Errorf("%s %q is the OTLP/gRPC port; this build speaks OTLP/HTTP, use port %s", label, raw, otlpHTTPPort)
	}

	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		// Two reasons to refuse the same endpoint, kept apart because they protect
		// different things and an operator should be told which one they hit.
		if r.HeadersSet {
			return fmt.Errorf("%s %q uses http to a non-loopback host while an OTLP headers variable is set, which sends the credential in the clear; use https, or a loopback address (127.0.0.1, ::1, localhost) for a local collector", label, raw)
		}

		// Content capture makes the payload the secret, so the header rule above is
		// the wrong gate for it: the common shape is an unauthenticated collector on
		// an internal network, where no headers variable is set at all and the whole
		// conversation crosses the wire in cleartext.
		if r.Capture.Value {
			return fmt.Errorf("%s %q uses http to a non-loopback host while %s is set, which sends prompts, tool arguments and tool results in the clear; use https, or a loopback address (127.0.0.1, ::1, localhost) for a local collector", label, raw, r.Capture.Origin)
		}
	}

	return nil
}

// isLoopbackHost reports whether host is a loopback address or the localhost name.
// Like util.ValidateBaseURL's own check it does not resolve names, so a hostname that
// happens to resolve to a loopback address is not treated as loopback.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}

	ip := net.ParseIP(host)
	if ip != nil {
		return ip.IsLoopback()
	}

	return false
}

// envIsTrue reads this repository's own off switches, where presence is the signal.
// It follows the NO_COLOR convention: any value turns the switch on except one that
// explicitly parses as false, so NO_TELEMETRY=1, =true and = yes all suppress export
// while NO_TELEMETRY=0 and =false do not.
func envIsTrue(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}

	b, err := strconv.ParseBool(v)
	if err != nil {
		return true
	}

	return b
}

// sdkDisabled reads OTEL_SDK_DISABLED, which unlike the switches above is specified
// by OpenTelemetry to disable only on a value of true. An unparseable value is
// ignored rather than treated as true, so this stays the standard's switch and not a
// second NO_TELEMETRY.
func sdkDisabled(v string) bool {
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return false
	}

	return b
}
