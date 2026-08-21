//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package slack

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/serve"
)

// Builder describes this channel to serve.Endpoints, so a program that wants a Slack bot
// links it in and a program that does not never references this package at all.
func Builder() serve.EndpointBuilder {
	return serve.EndpointBuilder{
		Name:    "slack",
		Enabled: func(cfg *config.Config) bool { return cfg.SlackEnabled() },
		Build: func(cfg *config.Config, opts serve.BuildOptions) ([]serve.Endpoint, error) {
			ch, err := NewFromConfig(cfg, ConfigOptions{
				SuspendRequested: opts.SuspendRequested,
				Logger:           opts.Logger,
			})
			if err != nil {
				return nil, err
			}

			return []serve.Endpoint{ch}, nil
		},
	}
}

// ConfigOptions are what a configured channel needs that no configuration can state: what
// the process decided, and what it is holding.
type ConfigOptions struct {
	// SuspendRequested is handed to every run, so a worker draining stops its turns where
	// they can be resumed from.
	SuspendRequested func() bool

	// Logger receives the channel's own progress. Nil builds a text logger on stderr.
	Logger *slog.Logger
}

// NewFromConfig builds the Slack channel described by expose.agent.slack.
//
// The credentials come from the environment rather than the configuration, so a file that
// is committed and shared never holds one. A missing variable is refused here, naming
// which one to set: a worker that started without them would take mentions it could not
// answer.
//
// serve.BuildOptions carries no field for either token, and deliberately. Adding two would
// put a Slack concern in the package that is otherwise ignorant of which endpoints exist.
//
// The --workers flag does not reach this. It sizes the queue intake, and one flag setting
// two numbers could not be reported honestly on a startup banner.
func NewFromConfig(cfg *config.Config, opts ConfigOptions) (*Channel, error) {
	if !cfg.SlackEnabled() {
		return nil, fmt.Errorf("expose.agent.slack is not configured")
	}

	appToken := os.Getenv(AppTokenVar)
	if appToken == "" {
		return nil, fmt.Errorf("expose.agent.slack needs an app-level token in %s, which is where its credentials come from rather than the configuration file", AppTokenVar)
	}

	botToken := os.Getenv(BotTokenVar)
	if botToken == "" {
		return nil, fmt.Errorf("expose.agent.slack needs a bot token in %s, which is where its credentials come from rather than the configuration file", BotTokenVar)
	}

	return New(Options{
		AppToken:         appToken,
		BotToken:         botToken,
		Identity:         cfg.Identity,
		Workers:          cfg.SlackWorkers(),
		ContextLines:     cfg.SlackContextLines(),
		Progress:         cfg.SlackProgressEnabled(),
		AnswerGrace:      cfg.SlackAnswerGrace(),
		MaxWaiting:       cfg.SlackMaxWaiting(),
		MaxCoalesced:     cfg.SlackMaxCoalesced(),
		SuspendRequested: opts.SuspendRequested,
		Logger:           opts.Logger,
	})
}
