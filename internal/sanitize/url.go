//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package sanitize

import (
	"fmt"
	"net/url"
)

// BaseURL requires raw to be a well-formed http or https URL with no embedded
// userinfo. label names the setting so callers get an error that points at the
// knob they set.
//
// The scheme is not otherwise constrained. Plain http reaches a local model
// runner, an embeddings server on a host gateway, or a service on a private
// network, all of which are ordinary deployments. An operator who sets a base URL
// has chosen their own trust boundary, and this check does not stand in for it.
func BaseURL(label, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid %s %q: %w", label, raw, err)
	}
	if u.User != nil {
		return fmt.Errorf("invalid %s %q: must not embed userinfo credentials", label, raw)
	}
	if u.Host == "" {
		return fmt.Errorf("invalid %s %q: must name a host", label, raw)
	}

	switch u.Scheme {
	case "https", "http":
		return nil
	default:
		return fmt.Errorf("invalid %s %q: scheme must be http or https", label, raw)
	}
}
