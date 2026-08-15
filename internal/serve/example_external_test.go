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
// channel, and the configuration every run it hosts will use.
func ExampleNew() {
	_, err := serve.New(serve.Options{
		Channels: []serve.Channel{&oneShotChannel{}},
	})

	fmt.Println(err)

	// Output: a configuration is required
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
