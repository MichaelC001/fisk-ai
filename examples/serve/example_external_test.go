//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// These examples live in the external serve_test package on purpose: they can reach
// only serve's exported API, so they are proof that a server is drivable from outside
// the package, which is the premise of hosting an agent behind a channel someone else
// wrote. Example functions rather than specs, so go doc shows a reader a channel and a
// server rather than a list of type names.
package serve_test

import (
	"context"
	"fmt"

	"github.com/choria-io/fisk-ai/internal/serve"
	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// oneShotChannel is a channel with a single piece of work, which is the smallest thing
// that implements serve.Channel: a name, and a Next that eventually says it is done.
type oneShotChannel struct {
	work *serve.Work
	sent bool
}

func (c *oneShotChannel) Name() string { return "example" }

func (c *oneShotChannel) Next(context.Context) (*serve.Work, error) {
	if c.sent {
		return nil, serve.ErrChannelDone
	}
	c.sent = true

	return c.work, nil
}

// ExampleChannel shows the whole of what a channel must implement. Name and Next are
// the required half; everything else a channel can offer a run is an optional field on
// the Work it hands over, so a channel that can do less simply supplies less.
func ExampleChannel() {
	done := make(chan struct{})

	ch := &oneShotChannel{
		work: &serve.Work{
			Prompt: "summarize the log",

			// Prompter is what the run puts a question to: the confirm gate's
			// approval, and the three human-in-the-loop tools. Leaving it nil refuses
			// every confirmation-gated command, which is the right answer for a
			// channel with nobody behind it.
			Prompter: &exampleOperator{},

			// PromptsMayBlock says the operator is on the other end of a live
			// connection, so a question may hold the run open until the run context
			// ends. Left false, PromptWait bounds each question and the run gives its
			// worker back: a question a tool asked defers, and a gate question leaves
			// its call unanswered for the next resume to ask again.
			PromptsMayBlock: true,

			// Done is the one field a channel must fill in. Work without it is
			// dropped, since a run nobody can be told about is a run nobody asked for.
			Done: func(_ context.Context, out serve.Outcome) error {
				defer close(done)

				fmt.Printf("work %s finished: %v\n", out.ID, out.Reason)

				return nil
			},
		},
	}

	fmt.Println(ch.Name())

	// Output: example
}

// ExampleNew shows the options a server needs before it will start: at least one
// surface, and the configuration every run it hosts will use.
func ExampleNew() {
	_, err := serve.New(serve.Options{
		Channels: []serve.Channel{&oneShotChannel{}},
	})

	fmt.Println(err)

	// Output: a configuration is required
}

// exampleService is the whole of what a service must implement. It answers its callers
// on whatever transport it registered with when it was built, so there is no method
// here for the server to call: it hosts the service and releases it, and nothing else.
type exampleService struct{}

func (s *exampleService) Name() string { return "tools" }

func (s *exampleService) Close() error { return nil }

// ExampleService shows a surface that produces no work. A server hosting one and no
// channel has nothing to pull, so Serve holds itself open until it is drained, stopped
// or canceled, and Close is what stops the service answering.
func ExampleService() {
	var svc serve.Service = &exampleService{}

	fmt.Println(svc.Name(), svc.Close())

	// Output: tools <nil>
}

// ExampleConcurrentChannel shows a channel stating a bound of its own. A channel that
// claims work before a run starts has to size that claiming to something, and only it
// knows what, so it states the number rather than being told one.
func ExampleConcurrentChannel() {
	var ch serve.Channel = &boundedExampleChannel{}

	bounded, ok := ch.(serve.ConcurrentChannel)
	fmt.Println(ok, bounded.Concurrency())

	// Output: true 4
}

type boundedExampleChannel struct {
	oneShotChannel
}

func (c *boundedExampleChannel) Concurrency() int { return 4 }

// exampleOperator is a toolkit.Prompter reaching a person over whatever the channel
// holds: a terminal, a chat thread, a socket. It renders each question and returns the
// answer. What an unanswered question costs is the server's decision rather than this
// one's, from Work.PromptsMayBlock and Work.PromptWait, so every method here waits on
// the context it is given and returns when that context ends.
type exampleOperator struct{}

// CanPrompt reports whether a person can be reached at all. False refuses every
// confirmation-gated command without asking, so a channel that has lost its operator
// says so here rather than leaving each question to time out.
func (p *exampleOperator) CanPrompt() bool { return true }

func (p *exampleOperator) ApproveCommand(ctx context.Context, req toolkit.GateRequest) (toolkit.ConfirmChoice, error) {
	// req.Display is the command line the operator is approving, and req.Tag is what
	// gated it. Both are model-supplied text: sanitize before rendering.
	fmt.Printf("may I run %s (%s)?\n", req.Display, req.Tag)

	return toolkit.ConfirmOnce, ctx.Err()
}

func (p *exampleOperator) Confirm(ctx context.Context, question string) (bool, error) {
	return true, ctx.Err()
}

func (p *exampleOperator) Select(ctx context.Context, question string, options []string) (int, error) {
	return 0, ctx.Err()
}

func (p *exampleOperator) Input(ctx context.Context, question, def string) (string, error) {
	return def, ctx.Err()
}
