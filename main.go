//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/choria-io/fisk"

	"github.com/choria-io/fisk-ai/config"

	// Link the file session backend in so it registers itself. The run path links it
	// transitively through the agent package; importing it here as well keeps the
	// session subcommands, which construct the store directly, self-sufficient.
	_ "github.com/choria-io/fisk-ai/internal/runstate/file"
)

// version is the build version shown on the splash card and reported by the MCP and
// A2A identities. It defaults to "devel" and is overridden at release time: goreleaser's
// default build ldflags set -X main.version=<tag>, so no extra config is needed.
var version = "devel"

// versionedConfig records this program's build version on a configuration as it is
// loaded, so every connection dialed from it announces "fisk-ai/<version>" to a NATS
// server. Product is left empty, which announces "fisk-ai". Wrapping the load is what
// covers every command: a configuration that reaches a dial without going through here
// would announce the product with no version.
//
// It is also where --root-dir is folded onto the parsed configuration and where the
// root is checked, which for the same reason covers run, serve, knowledge, mcp, info,
// session and discover from one call site. The check cannot live in ValidateForMode,
// which runs inside the parse and would refuse a file whose relative root_directory
// the flag is about to replace.
func versionedConfig(cfg *config.Config, err error) (*config.Config, error) {
	if err != nil {
		return nil, err
	}

	cfg.ProductVersion = version

	err = cfg.ApplyRootDir(rootDirFlag)
	if err != nil {
		return nil, err
	}

	return cfg, nil
}

var (
	configFile  string
	apiKey      string
	baseURL     string
	q           []string
	httpDebug   bool
	noColor     bool
	mcpPort     int
	mcpAddress  string
	verbose     bool
	noTUI       bool
	traceFile   string
	noTelemetry bool

	showToolOutput bool
	showThinking   bool

	rootDirFlag string
	setRootDir  bool

	resumeID          string
	forceResume       bool
	stateDirFlag      string
	sessionConfigFile string
	sessionArgID      string
	sessionTranscript bool
)

// interruptContext returns a context canceled on the first Ctrl-C (SIGINT) or
// SIGTERM, the shared interrupt contract for the one-shot commands. SIGTERM is
// included so a server (mcp, a2a) shuts down cleanly under systemd or a
// container stop; a second signal falls through to the default disposition and
// terminates the process. The run command keeps its own signal handling because
// it layers a graceful-suspend contract on top.
func interruptContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func main() {
	cmd := fisk.New("fisk", "Fisk AI Toolkit")
	cmd.Version(version)

	// Registered on the application rather than on each command, so every command
	// carries it and none can be forgotten. An application flag joins the parse context
	// before any command is matched, and interspersed parsing is on, so it binds on
	// either side of a command's own arguments.
	cmd.Flag("root-dir", "Directory the configuration's relative paths resolve under and the directory tools run in (default: this process's working directory)").
		PlaceHolder("DIR").
		Envar("FISK_AI_ROOT").
		IsSetByUser(&setRootDir).
		StringVar(&rootDirFlag)

	registerRunCommand(cmd)
	registerSessionCommand(cmd)
	registerInfoAction(cmd)
	registerRAGCommand(cmd)
	registerMcpAction(cmd)
	registerServeCommand(cmd)
	registerDiscoverAction(cmd)

	cmd.MustParseWithUsage(os.Args[1:])
}
