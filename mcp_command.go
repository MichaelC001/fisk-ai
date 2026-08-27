//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/choria-io/fisk"
	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/mcpserver"
	"github.com/choria-io/fisk-ai/internal/telemetry"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/toolkit/builtin"
	fisktool "github.com/choria-io/fisk-ai/internal/toolkit/fisk"
	"github.com/choria-io/fisk-ai/internal/util"
)

// defaultMCPPort is the TCP port the MCP server listens on when neither the
// --port flag nor expose.agent.mcp.port in the config sets one.
const defaultMCPPort = 8080

// defaultMCPAddress is the host the MCP server binds to when neither the
// --address flag nor expose.agent.mcp.address in the config sets one. It is
// loopback so the server is not reachable off the host by default; set an
// address explicitly (e.g. 0.0.0.0) to expose it more widely.
const defaultMCPAddress = "127.0.0.1"

func registerMcpAction(cmd *fisk.Application) {
	mcpCmd := cmd.Command("mcp", "Serves the tools over the Model Context Protocol").Action(mcpAction)
	mcpCmd.Flag("config", "Path to the agent configuration file").Default("agent.yaml").StringVar(&configFile)
	mcpCmd.Flag("port", "TCP port to listen on; overrides expose.agent.mcp.port").Envar("FISK_AI_MCP_PORT").IntVar(&mcpPort)
	mcpCmd.Flag("address", "Host or IP to bind to (default 127.0.0.1; use 0.0.0.0 for all interfaces); overrides expose.agent.mcp.address").Envar("FISK_AI_MCP_ADDRESS").StringVar(&mcpAddress)
}

// mcpAction serves the configured tools over MCP instead of running the agent.
// It is opt-in: the config must carry an expose.agent.mcp block or the command
// refuses to start. It needs only the application and tool filters; the prompt
// and model are not used. The served set is the agent's tools narrowed by
// expose.agent.tools when set. All progress goes to stderr; the MCP protocol owns
// the HTTP response bodies.
func mcpAction(_ *fisk.ParseContext) error {
	ctx, cancel := interruptContext()
	defer cancel()

	cfg, err := config.ParseConfigFileForMode(configFile, config.ModeMCP)
	if err != nil {
		return err
	}

	if !cfg.MCPEnabled() {
		return fmt.Errorf("fisk mcp requires an expose.agent.mcp block in %q; add expose.agent.mcp (optionally with a port) to serve tools over MCP", configFile)
	}

	if cfg.ApplicationPath == "" {
		fmt.Fprintln(os.Stderr, "note: no wrapped application configured (application_path unset); serving built-in tools only")
	}

	// Telemetry is resolved before anything is opened so a bad endpoint fails here
	// rather than after the listener is up. There is no full-screen UI on this path, so
	// the SDK's diagnostics go straight to stderr, which is where this command's notes
	// already go and is never its protocol channel.
	//
	// The provider rides the context rather than being handed to Serve: the knowledge
	// tools this command serves read it off the context, which is what internal/rag
	// needs to open a retrieval span for a search that arrived over MCP.
	tel, reportTelemetry, err := setupTelemetry(cfg, telemetrySetup{ConfigFile: configFile})
	if err != nil {
		return err
	}
	defer reportTelemetry()

	ctx = telemetry.ContextWithProvider(ctx, tel)

	// Derived from each tool's own exposure declaration rather than written out per
	// feature, so this cannot claim a tool is withheld after it stops being. A config
	// that enables memory for agent runs and is also served over MCP is correct, so
	// this is a note about where those tools are reachable, not a warning.
	if withheld := builtin.WithheldFromMCP(cfg); len(withheld) > 0 {
		fmt.Fprintf(os.Stderr, "note: %d built-in tool(s) this config enables are not served over MCP: %s. They need operator state or an operator at a terminal, so they are reachable only in an agent run\n", len(withheld), strings.Join(withheld, ", "))
	}

	// The refusal is structural rather than a policy this command applies, so this is a
	// note about where those tools are reachable. A client here cannot tell which of
	// the tools it is offered belong to the wrapped application and which came from a
	// third party the operator wired in, which is why they are not offered at all.
	if len(cfg.MCPClients) > 0 {
		servers := make([]string, 0, len(cfg.MCPClients))
		for _, server := range cfg.MCPClients {
			servers = append(servers, server.Name)
		}
		fmt.Fprintf(os.Stderr, "note: the %d server(s) in mcp_clients are not served over MCP: %s. Their tools are imported into an agent run, so they are reachable only there\n", len(servers), strings.Join(servers, ", "))
	}

	tools, err := fisktool.ServedTools(ctx, cfg)
	if err != nil {
		return err
	}

	ragBuiltins, ragStore, err := builtin.MCPKnowledgeBuiltins(ctx, cfg, os.Stderr)
	if err != nil {
		return err
	}
	// Close after Serve returns (Serve below is the final call, so this deferred
	// close runs only once graceful shutdown has drained in-flight tool calls),
	// never concurrently with a live query.
	if ragStore != nil {
		defer ragStore.Close()
	}

	if len(tools)+len(ragBuiltins) == 0 {
		return fmt.Errorf("no tools available after filtering; check include/exclude in %q", configFile)
	}

	port := mcpPort
	if port == 0 {
		port = cfg.MCPPort()
	}
	if port == 0 {
		port = defaultMCPPort
	}

	address := mcpAddress
	if address == "" {
		address = cfg.MCPAddress()
	}
	if address == "" {
		address = defaultMCPAddress
	}

	// The wrapped application's commands are listed first so one of them keeps a
	// name a built-in would also claim, rather than being shadowed by it.
	served := append(toolkit.Tools(tools), toolkit.Tools(ragBuiltins)...)

	return mcpserver.Serve(ctx, served, mcpserver.Options{
		Name:         cfg.Identity,
		Version:      util.Version(),
		Addr:         net.JoinHostPort(address, strconv.Itoa(port)),
		Instructions: cfg.MCPInstructions(),
		ConfirmTags:  cfg.ConfirmTags(),
		ConfirmMode:  mcpserver.ConfirmMode(cfg.ConfirmOverMCPMode()),
		Concurrency:  cfg.MCPMaxConcurrentTools(),
		CallTimeout:  cfg.MCPToolTimeout(),
	})
}
