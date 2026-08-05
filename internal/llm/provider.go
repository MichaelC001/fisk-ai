//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package llm

import "context"

// Provider is one model backend. It turns a neutral Request into a neutral
// Response, translating to and from its own wire format internally, and reports
// the capabilities that shape how a request must be built for it. A Provider is
// the single seam the agent loop calls: it is the only place a concrete SDK is
// spoken on the request path.
type Provider interface {
	// Call issues one model request and returns the assistant turn, why it stopped,
	// and what it cost. It owns the wire call end to end, including any per-call
	// timeout, so the caller hands it a neutral value and gets a neutral value back.
	Call(ctx context.Context, req Request) (*Response, error)

	// Capabilities reports the provider's declared capabilities. They are declared,
	// not discovered: neither Anthropic nor OpenAI expose capability flags at runtime,
	// so a provider states them from static knowledge of its backend.
	Capabilities() Caps
}

// Caps is a provider's declared capability set. It is deliberately small; it grows
// as a second provider makes a real capability difference concrete rather than
// predicted.
type Caps struct {
	// Provider is the neutral provider id, the value stamped into the run fingerprint
	// so a resume against a different provider is refused.
	Provider string

	// SemconvProvider is the gen_ai.provider.name value the OpenTelemetry GenAI
	// semantic conventions define for this backend, reported on traces and metrics.
	// It is declared here, next to the neutral id, so everything a maintainer needs to
	// know when adding a backend is in that backend's own file, the same way its
	// credential environment variables are declared at Register rather than in a list
	// somewhere else.
	//
	// It is separate from Provider because the two vocabularies do not always agree
	// and answer to different owners: Provider is this repository's to choose, while
	// this one is the conventions' registry value and changes only when that registry
	// does. A backend this repository would naturally call "bedrock" or "azure" is
	// "aws.bedrock" and "azure.ai.openai" to the conventions.
	//
	// Declare it as a constant in the backend's own package with a comment citing the
	// registry entry, so nothing outside internal/telemetry has to import
	// OpenTelemetry to state it. Leave it empty when the conventions define no value
	// for the backend; SemconvProviderName then falls back to the neutral id, which
	// the conventions' open enum allows.
	SemconvProvider string

	// SupportsToolSearch reports whether the provider offers server-side tool search
	// (deferred tool loading). A provider without it must send every tool directly.
	SupportsToolSearch bool

	// MaxOutputTokens is the provider's ceiling on a single response, or 0 when it is
	// not known or not enforced.
	MaxOutputTokens int64
}

// SemconvProviderName is the gen_ai.provider.name value to report for this backend:
// the declared conventions value, or the neutral id when the backend declares none.
//
// It reads off the capabilities of the provider actually in use rather than off the
// configuration, which matters because an injected provider bypasses the registry
// entirely: the config would say what was asked for, this says what ran.
func (c Caps) SemconvProviderName() string {
	if c.SemconvProvider != "" {
		return c.SemconvProvider
	}

	return c.Provider
}
