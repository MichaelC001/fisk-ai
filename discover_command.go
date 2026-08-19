//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"time"

	"github.com/choria-io/fisk"
	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/a2a"
	_ "github.com/choria-io/fisk-ai/internal/a2a/nats"
	"github.com/choria-io/fisk-ai/internal/conns"
	"github.com/choria-io/fisk-ai/internal/util"
	"github.com/choria-io/ui/columns"
	"github.com/choria-io/ui/table"
)

// maxDiscoverDescriptionLen is the width at which tool descriptions are truncated
// in the discover table.
const maxDiscoverDescriptionLen = 60

var (
	discoverAgent   string
	natsContextFlag string
)

func registerDiscoverAction(cmd *fisk.Application) {
	discover := cmd.Command("discover", "Discovers a remote agent over NATS and prints its tools").Action(discoverAction)
	discover.Arg("agent", "Identity of the agent to discover").Required().StringVar(&discoverAgent)
	discover.Flag("config", "Path to the agent configuration file to read nats_context from").Default("agent.yaml").StringVar(&configFile)
	discover.Flag("context", "NATS context name to use, instead of reading it from the config").StringVar(&natsContextFlag)
}

// discoverAction sends a discovery request to a named agent and prints its agent
// card: a quick way to confirm an agent is reachable and see the tools it exposes
// before wiring it into remote_tools. The NATS context comes from --context when
// given, otherwise from nats_context in the config file.
func discoverAction(_ *fisk.ParseContext) error {
	ctx, cancel := interruptContext()
	defer cancel()

	contextName, sender, wait, err := discoverContext()
	if err != nil {
		return err
	}

	provider, err := conns.Connect(contextName, sender)
	if err != nil {
		return err
	}
	defer provider.Close()

	transport, err := a2a.NewTransport(config.A2ATransportName, provider, a2a.TransportConfig{Identity: sender, Timeout: wait})
	if err != nil {
		return err
	}

	client, err := a2a.NewClient(transport, sender, a2a.WithIdleTimeout(wait))
	if err != nil {
		return err
	}

	card, err := client.Discover(ctx, discoverAgent)
	if err != nil {
		return err
	}

	c := columns.New()
	c.Headingf("Agent Card for {bold}%s{/bold}", discoverAgent)
	c.Item("Agent", card.Name)
	c.Item("Version", card.Version)
	c.ItemUnlessZero("Description", card.Description)
	c.ItemUnlessZero("Protocols", card.Protocols)
	// Said only when true, and said in full. Somebody reading a card before they send a
	// prompt is deciding whether to send it, and "telemetry: true" does not tell them
	// that the words themselves travel.
	if card.TelemetryContent {
		c.Item("Conversation content", "exported to a telemetry collector")
	}
	fmt.Println(c.String())

	if len(card.Tools) == 0 {
		// An agent that answers prompts and serves no tools to peers is a working agent,
		// not a broken one, so this says what is true rather than what is missing.
		fmt.Println("The agent serves no tools to peers.")
		return nil
	}

	tbl := table.NewTableWriter("")
	defer tbl.WriteTo(os.Stdout)

	tbl.AddHeaders("Tool", "Description")
	for _, t := range card.Tools {
		tbl.AddRow(t.Name, util.TruncateString(t.Description, maxDiscoverDescriptionLen))
	}

	return nil
}

// discoverContext resolves the NATS context name, the sender identity and how long to
// wait for the card. A --context flag takes precedence and needs no config file;
// otherwise the config file is read for nats_context and the agent's identity is used
// as the sender. The sender defaults to "fisk-ai" when no config identity is available.
//
// The wait comes from the same key a run waits on, so this command and `fisk info`
// give a peer the same time to answer the same request. With --context there is no
// configuration to read one from, and the transport applies its own default.
func discoverContext() (contextName string, sender string, wait time.Duration, err error) {
	if natsContextFlag != "" {
		return natsContextFlag, "fisk-ai", 0, nil
	}

	cfg, err := config.ParseConfigFileForMode(configFile, config.ModeMCP)
	if err != nil {
		return "", "", 0, fmt.Errorf("reading %q for nats_context (or pass --context): %w", configFile, err)
	}
	if cfg.NatsContext == "" {
		return "", "", 0, fmt.Errorf("no nats_context in %q; set it or pass --context", configFile)
	}

	sender = cfg.Identity
	if sender == "" {
		sender = "fisk-ai"
	}

	return cfg.NatsContext, sender, cfg.A2ARequestTimeout(), nil
}
