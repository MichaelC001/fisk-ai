//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	wire "github.com/choria-io/fisk-ai/internal/a2a/wire/v1"
	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// toolBehavior converts a toolkit declaration to the wire shape. A hint outside the
// three toolkit names travels as no claim, which is what toolkit.Hint reads an
// unrecognized value as.
func toolBehavior(b toolkit.Behavior) wire.ToolBehavior {
	return wire.ToolBehavior{
		ReadOnly:    wireHint(b.ReadOnly),
		Destructive: wireHint(b.Destructive),
		Idempotent:  wireHint(b.Idempotent),
		OpenWorld:   wireHint(b.OpenWorld),
	}
}

// BehaviorOf converts what a peer claimed back to a toolkit declaration, for an importer
// building a local tool from a descriptor.
func BehaviorOf(b wire.ToolBehavior) toolkit.Behavior {
	return toolkit.Behavior{
		ReadOnly:    toolkitHint(b.ReadOnly),
		Destructive: toolkitHint(b.Destructive),
		Idempotent:  toolkitHint(b.Idempotent),
		OpenWorld:   toolkitHint(b.OpenWorld),
	}
}

func wireHint(h toolkit.Hint) *bool {
	switch h {
	case toolkit.HintTrue:
		yes := true
		return &yes
	case toolkit.HintFalse:
		no := false
		return &no
	default:
		return nil
	}
}

func toolkitHint(v *bool) toolkit.Hint {
	switch {
	case v == nil:
		return toolkit.HintUnset
	case *v:
		return toolkit.HintTrue
	default:
		return toolkit.HintFalse
	}
}
