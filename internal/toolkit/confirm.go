//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package toolkit

import "slices"

// NeedsConfirm reports whether a tool carrying toolTags must be approved by the
// operator before it runs: it carries the always-on ConfirmTag, or any of the extra
// confirm tags the operator configured (extraTags). ConfirmTag is matched
// unconditionally, so omitting it from extraTags can never weaken the guarantee.
func NeedsConfirm(toolTags, extraTags []string) bool {
	for _, tag := range toolTags {
		if tag == ConfirmTag || slices.Contains(extraTags, tag) {
			return true
		}
	}

	return false
}

// ConfirmTrigger returns the tag that gates a tool carrying toolTags, named to the
// operator in the approval prompt. The always-on ConfirmTag takes precedence when
// present, since it is the strongest, mode-independent signal; otherwise the first
// of the tool's own tags that appears in extraTags, in the tool's tag order, is
// returned so the message is deterministic. It returns an empty string when the tool
// is not gated, which NeedsConfirm reports first, so callers consult it only for a
// gated tool.
func ConfirmTrigger(toolTags, extraTags []string) string {
	if slices.Contains(toolTags, ConfirmTag) {
		return ConfirmTag
	}

	for _, tag := range toolTags {
		if slices.Contains(extraTags, tag) {
			return tag
		}
	}

	return ""
}
