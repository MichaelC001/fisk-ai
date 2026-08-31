//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// This example hosts an agent behind a channel: it builds the resources every run
// shares, hands a server one channel carrying a single piece of work, serves until
// the channel is finished, and prints the outcome the channel was given.
//
// A channel is a calling endpoint. A work queue, a chat integration and an
// in-process caller are all channels and differ only in what they supply on the work
// they hand over. This one supplies the least a channel can: a prompt, and a way to
// report the outcome.
//
// It reaches no network and no broker: the model provider answers from a script and
// the configuration selects no store that is reached over NATS.
package hostagent_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/serve"
)

// providerName is the llm.Provider registration the hosted runs answer from, named
// in the configuration as llm.provider. serve.NewResources builds the provider by
// looking this name up, so a program supplying its own registers it rather than
// passing an instance.
const providerName = "example-scripted"

func init() {
	llm.Register(providerName, func(llm.Config) (llm.Provider, error) {
		return newScriptedProvider(), nil
	}, nil)
}

// Example drives one unit of work through a hosted agent.
func Example() {
	err := hostAgent()
	if err != nil {
		fmt.Println("error:", err)
	}

	// Output:
	// answer: Two alarms cleared, one pump still offline, nothing outstanding for the next shift.
	// outcome: completed
	// llm calls: 1
	// tool calls: 0
}

func hostAgent() error {
	ctx := context.Background()

	// The run journals and the memory store live under here, so the example leaves
	// nothing behind.
	root, err := os.MkdirTemp("", "hostagent")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)

	// The server narrates its progress to a logger. This one discards it so the
	// example's own output is all that is printed; a worker sends it to its log.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	cfg, err := buildConfig()
	if err != nil {
		return err
	}

	// The resources every run shares rather than building for itself: the model
	// provider, the memory store and the session store here, plus the NATS connection,
	// the knowledge index and the MCP sessions a fuller configuration calls for. They
	// are closed after Serve returns and never before, since a run in flight is still
	// using them.
	resources, err := serve.NewResources(cfg, serve.ResourceOptions{
		ConfigFile: "(built in Go)",
		StoreDir:   root,
		Logger:     log,
	})
	if err != nil {
		return err
	}
	defer resources.Close()

	// The channel reports the outcome through Done, so the example reads it from
	// there: the outcome belongs to whoever asked for the work rather than to the
	// server.
	outcomes := make(chan serve.Outcome, 1)

	channel := &oneShotChannel{
		work: &serve.Work{
			Prompt: "summarize the shift handover",
			// Done is the one field a channel must fill in. Work without it is dropped.
			Done: func(_ context.Context, out serve.Outcome) error {
				outcomes <- out

				return nil
			},
		},
	}

	opts := serve.Options{
		Channels:   []serve.Channel{channel},
		Config:     cfg,
		ConfigFile: "(built in Go)",
		StoreDir:   root,
		Logger:     log,
	}
	// ApplyTo overwrites every field it sets, so a caller keeping one of its own
	// assigns it after this call rather than before.
	resources.ApplyTo(&opts)

	// New takes over releasing the channels: it closes them itself when it refuses
	// these options, so a channel holding a connection is not left open by a
	// constructor that failed.
	server, err := serve.New(opts)
	if err != nil {
		return err
	}

	// Serve returns once every channel is finished and every run it started has
	// reported its outcome. Stop then releases what the endpoints hold.
	err = server.Serve(ctx)
	if err != nil {
		return err
	}

	err = server.Stop()
	if err != nil {
		return err
	}

	out := <-outcomes

	// Stats is nil when the run failed before it started, and a run stopped by its
	// budget reports both a reason and an error, so the error is read first.
	if out.Err != nil {
		return out.Err
	}

	fmt.Println("answer:", out.Text)
	fmt.Println("outcome:", out.Reason)
	fmt.Println("llm calls:", out.Stats.LlmCalls)
	fmt.Println("tool calls:", out.Stats.ToolCalls)

	return nil
}

// buildConfig assembles the configuration in Go rather than reading a file. Prepare
// derives the identity and fills the default budgets, so it runs after the last
// field is set.
func buildConfig() (*config.Config, error) {
	cfg, err := config.NewConfig()
	if err != nil {
		return nil, err
	}

	cfg.Identity = "hostagent-example"
	cfg.SystemPrompt = "You summarize shift handovers for the operations team."
	cfg.LLM.Provider = providerName
	cfg.LLM.Model = "example-model"
	cfg.LLM.Budget.MaxIterations = 10
	// A run refuses to start with no tool at all. This configuration wraps no
	// application, so the memory built-ins are what it runs on; a worker hosting a
	// real agent sets application_path, or enables knowledge, remote tools or MCP
	// clients.
	cfg.Harness.Memory = &config.MemoryConfig{Enabled: true}

	err = cfg.Prepare()
	if err != nil {
		return nil, err
	}

	return cfg, nil
}

// oneShotChannel hands over one piece of work and then reports that it is finished,
// which is the smallest thing that implements serve.Channel. A queue channel instead
// blocks in Next until an item arrives and never finishes.
type oneShotChannel struct {
	work *serve.Work
	sent bool
}

func (c *oneShotChannel) Name() string { return "example" }

// Next is called from one goroutine at a time. ErrChannelDone ends this channel
// without the server treating it as a failure, which is how a finite channel stops.
func (c *oneShotChannel) Next(context.Context) (*serve.Work, error) {
	if c.sent {
		return nil, serve.ErrChannelDone
	}
	c.sent = true

	return c.work, nil
}

// scriptedProvider answers each model call with the next response in a fixed list,
// which keeps the example off the network. Every run this server hosts shares one
// provider, so a real one must be safe for concurrent use.
type scriptedProvider struct {
	responses []*llm.Response
	idx       int
}

func newScriptedProvider() *scriptedProvider {
	return &scriptedProvider{responses: []*llm.Response{
		text("Two alarms cleared, one pump still offline, nothing outstanding for the next shift."),
	}}
}

func (p *scriptedProvider) Call(context.Context, llm.Request) (*llm.Response, error) {
	if p.idx >= len(p.responses) {
		return nil, fmt.Errorf("the script has %d responses and the loop asked for %d", len(p.responses), p.idx+1)
	}

	resp := p.responses[p.idx]
	p.idx++

	return resp, nil
}

func (p *scriptedProvider) Capabilities() llm.Caps {
	return llm.Caps{Provider: providerName, SemconvProvider: providerName}
}

func text(s string) *llm.Response {
	return &llm.Response{
		StopReason: llm.StopEndTurn,
		Content:    []llm.ContentBlock{{Text: &llm.TextBlock{Text: s}}},
	}
}
