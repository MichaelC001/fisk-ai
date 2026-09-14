//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package fisktool

import (
	"context"

	"github.com/choria-io/fisk-ai/config"
)

// LoadTools introspects the application and strips the commands tagged ai:deny. The
// strip is unconditional, so the tag holds with no filter set. The configured include and
// exclude filters are not applied here: agent.Assemble applies them to every tool of
// every kind, the commands included. ctx bounds the introspection subprocess (see
// FetchFiskAppModel).
func LoadTools(ctx context.Context, cfg *config.Config) ([]*CommandTool, error) {
	// With no wrapped application there is nothing to introspect; the agent runs on
	// its built-in and remote tools alone.
	if cfg.ApplicationPath == "" {
		return nil, nil
	}

	tools, err := ToolsForApp(ctx, cfg.ApplicationPath, cfg.RootDirectory, cfg.CredentialEnvNames(), cfg.GlobalFlagNames()...)
	if err != nil {
		return nil, err
	}

	return stripDenied(tools), nil
}

// ServedTools returns what LoadTools returns. A served surface's expose.agent.tools
// selection is applied by agent.Assemble to every tool it serves, the commands
// included.
func ServedTools(ctx context.Context, cfg *config.Config) ([]*CommandTool, error) {
	return LoadTools(ctx, cfg)
}
