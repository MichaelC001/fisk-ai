//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// These examples live in the external asyncjobs_test package on purpose: they can
// reach only the exported API, so they are proof that a worker is assemblable from
// outside the package by a program that has no configuration file.
//
// They carry no Output comment, so they are compiled and type-checked but not run. Each
// needs a NATS server with a work queue already on it, which is an integration concern;
// what they are here to prove is that the composition holds together, and compiling
// them is what checks it.
package asyncjobs_test

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"

	"github.com/nats-io/nats.go"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/serve"
	"github.com/choria-io/fisk-ai/internal/serve/asyncjobs"
)

// exampleConfig builds the agent a worker will host without reading a file. The fields
// are the ones agent.Options documents, so a program that already builds an agent
// configures a worker the same way.
//
// The expose.agent.jobs block exists so an operator can describe a worker in YAML. A Go
// caller states the same things directly and never needs one.
func exampleConfig() *config.Config {
	cfg := &config.Config{
		Identity:        "worker",
		ApplicationPath: "/usr/local/bin/nats",
		SystemPrompt:    "You inspect NATS servers on behalf of an operator.",
		Include:         &config.ToolFilter{Tools: []string{"^stream_"}},
	}
	cfg.LLM.Model = "claude-sonnet-5"
	cfg.LLM.Budget.MaxIterations = 20

	return cfg
}

// exampleWorker assembles a worker in code: the resources every run shares, a queue
// channel, and the server that ties them together. The returned release closes what
// the caller owns, which is everything except the channel: New took that.
//
// suspend is polled by every run at a loop boundary, so a drain stops runs where their
// journals can be picked up rather than wherever they happened to be. Nil never
// suspends, which makes a drain a hard stop.
func exampleWorker(ctx context.Context, suspend func() bool) (*serve.Server, func(), error) {
	cfg := exampleConfig()

	// The queue engine requires this connection option and nothing else wants it, which
	// is why a channel dials its own rather than borrowing the agent's.
	nc, err := nats.Connect(nats.DefaultURL, nats.UseOldRequestStyle())
	if err != nil {
		return nil, nil, err
	}

	// The provider, session store and anything else expensive to build per job. The
	// caller owns them and closes them after Serve has returned.
	res, err := serve.NewResources(ctx, cfg, serve.ResourceOptions{APIKey: os.Getenv("ANTHROPIC_API_KEY")})
	if err != nil {
		nc.Close()
		return nil, nil, err
	}

	// The queue must already exist. This binds to it and reads its run time, try limit
	// and concurrency off the consumer it bound.
	channel, err := asyncjobs.New(asyncjobs.Options{
		Conn:             nc,
		Queue:            "FISK_AI",
		TaskType:         "fisk-ai:run",
		Identity:         cfg.Identity,
		Concurrency:      4,
		SuspendRequested: suspend,
	})
	if err != nil {
		res.Close()
		nc.Close()

		return nil, nil, err
	}

	opts := serve.Options{
		Channels: []serve.Channel{channel},
		Config:   cfg,

		// Read the bound back from the channel rather than writing 4 twice. A queue
		// bounds every worker on it together, so a server admitting fewer runs than the
		// channel claims holds claims against work it will not start.
		Concurrency: channel.Concurrency(),
	}
	res.ApplyTo(&opts)

	srv, err := serve.New(opts)
	if err != nil {
		// New releases the channels itself when it refuses the options, so there is no
		// third outcome where the caller cleans up after a constructor that failed.
		res.Close()
		nc.Close()

		return nil, nil, err
	}

	return srv, func() {
		// Stop releases the channels the server took, so it comes before the resources
		// the runs were using.
		_ = srv.Stop()
		res.Close()
		nc.Close()
	}, nil
}

// Example_programmatic runs a worker with no configuration file, no flags and no
// signal handling, which is the smallest thing that takes queued work.
func Example_programmatic() {
	srv, release, err := exampleWorker(context.Background(), nil)
	if err != nil {
		panic(err)
	}
	defer release()

	// Serve returns when the channels are finished or the context ends, and not before
	// every run it started has reported.
	err = srv.Serve(context.Background())
	if err != nil {
		panic(err)
	}
}

// Example_drainOnSignal shows the shutdown a worker under a process supervisor wants.
// The first signal stops new work and lets what is running reach a point it can resume
// from; the second stops at once.
//
// Signals are the program's to own. A library calling signal.Notify would take SIGTERM
// from an embedder's supervisor with no way to decline, so serve offers the two verbs
// and the program decides which signal means which.
func Example_drainOnSignal() {
	var suspend atomic.Bool

	srv, release, err := exampleWorker(context.Background(), suspend.Load)
	if err != nil {
		panic(err)
	}
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		<-signals
		fmt.Println("draining")
		suspend.Store(true)

		// Drain blocks for as long as the work in flight does, so it runs on a
		// goroutine of its own rather than on the signal handler.
		go func() {
			err := srv.Drain()
			if err != nil {
				fmt.Println("drain failed:", err)
			}
		}()

		<-signals
		cancel()
	}()

	// Serve ends by itself once the channels are drained and their runs have reported,
	// with nothing canceled.
	err = srv.Serve(ctx)
	if err != nil {
		panic(err)
	}
}
