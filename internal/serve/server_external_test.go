//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package serve_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/choria-io/fisk"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/serve"
)

// servedApp is the wrapped application the served runs introspect. It carries one
// command so a run has a tool to call.
func servedApp() *fisk.Application {
	app := fisk.New("app", "an app")
	do := app.Command("do", "do a thing")
	do.Arg("subject", "the subject").Required().String()

	return app
}

// servedConfig is a configuration for an agent these specs actually run, so unlike the
// one inside the package it needs an application to introspect.
func servedConfig(opts ...agenttest.ConfigOption) *config.Config {
	GinkgoHelper()

	return agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), servedApp()), opts...)
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// startProbe is an events sink that only cares that a run began, so a spec can see when
// one is live. It embeds a real recorder rather than a nil interface, or every other
// method would panic into the run's barrier and turn each run into a crash the spec
// never notices.
type startProbe struct {
	agent.Events

	onStart func()
}

func newStartProbe(onStart func()) *startProbe {
	return &startProbe{Events: agenttest.NewRecordingEvents(), onStart: onStart}
}

func (p *startProbe) Starting(agent.RunInfo) { p.onStart() }

// noDoneChannel supplies one piece of work with no Done callback, then reports it is
// finished, so a spec can prove the server drops it rather than running it. The fakes
// fill a missing Done in, which is the convenience being defeated here on purpose.
type noDoneChannel struct {
	calls int
}

func (c *noDoneChannel) Name() string { return "no-done" }

func (c *noDoneChannel) Next(context.Context) (*serve.Work, error) {
	c.calls++
	if c.calls == 1 {
		return &serve.Work{ID: "orphan", Prompt: "go"}, nil
	}

	return nil, serve.ErrChannelDone
}

// panicNextChannel panics every time it is asked for work, which is what a third-party
// channel with a bug in it does. It counts the calls so a spec can prove the server
// stopped asking rather than retrying the same bug every five seconds.
type panicNextChannel struct {
	mu    sync.Mutex
	calls int
}

func (c *panicNextChannel) Name() string { return "broken" }

func (c *panicNextChannel) Next(context.Context) (*serve.Work, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()

	panic("taking work is broken")
}

func (c *panicNextChannel) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.calls
}

// panicDoneChannel hands over two pieces of work whose Done panics, so a spec can watch
// a worker carry on past an outcome it could not report. It counts the Next and Done
// calls it received, and stores no outcomes: the panic destroys each one.
type panicDoneChannel struct {
	mu    sync.Mutex
	calls int
	dones int
}

func (c *panicDoneChannel) Name() string { return "broken-done" }

func (c *panicDoneChannel) Next(context.Context) (*serve.Work, error) {
	c.mu.Lock()
	c.calls++
	n := c.calls
	c.mu.Unlock()

	if n > 2 {
		return nil, serve.ErrChannelDone
	}

	return &serve.Work{ID: fmt.Sprintf("job-%d", n), Prompt: "go", Done: c.done}, nil
}

func (c *panicDoneChannel) done(context.Context, serve.Outcome) error {
	c.mu.Lock()
	c.dones++
	c.mu.Unlock()

	panic("reporting is broken")
}

func (c *panicDoneChannel) Counts() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.calls, c.dones
}

var _ = Describe("Server", func() {
	var (
		ctx    context.Context
		cancel context.CancelFunc
	)

	BeforeEach(func() {
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		DeferCleanup(cancel)
	})

	Describe("New", func() {
		It("Should require a channel and a configuration", func() {
			_, err := serve.New(serve.Options{Config: servedConfig()})
			Expect(err).To(MatchError(serve.ErrInvalidOptions))
			Expect(err).To(MatchError(ContainSubstring("at least one channel")))

			_, err = serve.New(serve.Options{
				Channels: []serve.Channel{agenttest.NewScriptedChannel(GinkgoTB(), "c")},
			})
			Expect(err).To(MatchError(serve.ErrConfigRequired))
			Expect(err).To(MatchError(ContainSubstring("configuration is required")))
		})

		It("Should reject a channel with no name", func() {
			_, err := serve.New(serve.Options{
				Channels: []serve.Channel{agenttest.NewScriptedChannel(GinkgoTB(), "")},
				Config:   servedConfig(),
			})
			Expect(err).To(MatchError(serve.ErrInvalidOptions))
			Expect(err).To(MatchError(ContainSubstring("has no name")))
		})

		It("Should reject a work directory that is not an absolute path", func() {
			_, err := serve.New(serve.Options{
				Channels: []serve.Channel{agenttest.NewScriptedChannel(GinkgoTB(), "c")},
				Config:   servedConfig(),
				WorkDir:  "relative",
			})
			Expect(err).To(MatchError(serve.ErrInvalidOptions))
			Expect(err).To(MatchError(ContainSubstring("not an absolute path")))
		})

		// A refused set of options leaves the caller holding no Server, so nothing they
		// have can release the channels they handed over. Several of them own a
		// connection, so leaving them open leaks it somewhere unreachable.
		It("Should release the channels when it refuses the options", func() {
			q := agenttest.NewQueue(GinkgoTB(), "c")

			_, err := serve.New(serve.Options{
				Channels: []serve.Channel{q},
				Config:   servedConfig(),
				WorkDir:  "relative",
			})
			Expect(err).To(HaveOccurred())
			Expect(q.Closes()).To(Equal(1))
		})

		// The channel that cannot be released is the ordinary case, and a nil one is
		// what the validation is refusing, so neither may take the teardown down with it.
		It("Should survive releasing a nil channel and one with no Close", func() {
			q := agenttest.NewQueue(GinkgoTB(), "c")

			_, err := serve.New(serve.Options{
				Channels: []serve.Channel{q, agenttest.NewScriptedChannel(GinkgoTB(), "plain"), nil},
				Config:   servedConfig(),
			})
			Expect(err).To(MatchError(ContainSubstring("is nil")))
			Expect(q.Closes()).To(Equal(1))
		})

		It("Should not release the channels of a server it returned", func() {
			q := agenttest.NewQueue(GinkgoTB(), "c")

			srv, err := serve.New(serve.Options{
				Channels: []serve.Channel{q},
				Config:   servedConfig(),
			})
			Expect(err).ToNot(HaveOccurred())
			Expect(q.Closes()).To(Equal(0))

			Expect(srv.Stop()).To(Succeed())
			Expect(q.Closes()).To(Equal(1))
		})
	})

	Describe("Serving work", func() {
		It("Should run the work and report the answer", func() {
			ch := agenttest.NewScriptedChannel(GinkgoTB(), "jobs", &serve.Work{ID: "job-1", Prompt: "go"})

			srv, err := serve.New(serve.Options{
				Channels: []serve.Channel{ch},
				Config:   servedConfig(),
				Provider: agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("all done")),
				Logger:   quietLogger(),
			})
			Expect(err).ToNot(HaveOccurred())
			Expect(srv.Serve(ctx)).To(Succeed())

			outcomes := ch.Outcomes()
			Expect(outcomes).To(HaveLen(1))
			Expect(outcomes[0].ID).To(Equal("job-1"))
			Expect(outcomes[0].Err).ToNot(HaveOccurred())
			Expect(outcomes[0].Reason).To(Equal(runstate.ReasonCompleted))
			Expect(outcomes[0].Text).To(Equal("all done"))
			Expect(outcomes[0].Stats).ToNot(BeNil())
			Expect(outcomes[0].Crashed).To(BeFalse())
		})

		It("Should mint an id for work that carries none", func() {
			ch := agenttest.NewScriptedChannel(GinkgoTB(), "jobs", &serve.Work{Prompt: "go"})

			srv, err := serve.New(serve.Options{
				Channels: []serve.Channel{ch},
				Config:   servedConfig(),
				Provider: agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("ok")),
				Logger:   quietLogger(),
			})
			Expect(err).ToNot(HaveOccurred())
			Expect(srv.Serve(ctx)).To(Succeed())

			Expect(ch.Outcomes()[0].ID).ToNot(BeEmpty())
		})

		It("Should report a failed run without treating it as a crash", func() {
			ch := agenttest.NewScriptedChannel(GinkgoTB(), "jobs", &serve.Work{ID: "job-1", Prompt: "go"})

			srv, err := serve.New(serve.Options{
				Channels: []serve.Channel{ch},
				Config:   servedConfig(agenttest.WithMaxIterations(1)),
				Provider: agenttest.NewScriptedProvider(GinkgoTB(), &llm.Response{
					StopReason: llm.StopToolUse,
					Content: []llm.ContentBlock{
						{Text: &llm.TextBlock{Text: "still working"}},
						{ToolUse: &llm.ToolUseBlock{ID: "c1", Name: "do", Input: json.RawMessage(`{"subject":"x"}`)}},
					},
				}),
				Logger: quietLogger(),
			})
			Expect(err).ToNot(HaveOccurred())
			Expect(srv.Serve(ctx)).To(Succeed())

			out := ch.Outcomes()[0]
			Expect(out.Err).To(MatchError(ContainSubstring("max iterations")))
			Expect(out.Reason).To(Equal(runstate.ReasonMaxIterations))
			Expect(out.Crashed).To(BeFalse())
			Expect(out.Text).To(Equal("still working"), "the last thing said survives a non-terminal ending")
		})

		It("Should serve every channel it is given", func() {
			a := agenttest.NewScriptedChannel(GinkgoTB(), "a", &serve.Work{ID: "a-1", Prompt: "go"})
			b := agenttest.NewScriptedChannel(GinkgoTB(), "b", &serve.Work{ID: "b-1", Prompt: "go"})

			srv, err := serve.New(serve.Options{
				Channels: []serve.Channel{a, b},
				Config:   servedConfig(),
				Provider: agenttest.NewScriptedProvider(GinkgoTB(),
					agenttest.TextResponse("one"), agenttest.TextResponse("two")),
				Logger: quietLogger(),
			})
			Expect(err).ToNot(HaveOccurred())
			Expect(srv.Serve(ctx)).To(Succeed())

			Expect(a.Outcomes()).To(HaveLen(1))
			Expect(b.Outcomes()).To(HaveLen(1))
		})

		It("Should discard work that has no way to report an outcome", func() {
			ch := &noDoneChannel{}

			srv, err := serve.New(serve.Options{
				Channels: []serve.Channel{ch},
				Config:   servedConfig(),
				Provider: agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("ok")),
				Logger:   quietLogger(),
			})
			Expect(err).ToNot(HaveOccurred())
			Expect(srv.Serve(ctx)).To(Succeed())

			Expect(ch.calls).To(Equal(2), "the work is dropped and the channel is asked again")
		})
	})

	Describe("Admission", func() {
		It("Should bound how many runs execute at once", func() {
			work := make([]*serve.Work, 6)
			for i := range work {
				work[i] = &serve.Work{ID: fmt.Sprintf("job-%d", i), Prompt: "go"}
			}
			ch := agenttest.NewScriptedChannel(GinkgoTB(), "jobs", work...)

			var mu sync.Mutex
			var live, peak int

			responses := make([]*llm.Response, len(work))
			for i := range responses {
				responses[i] = agenttest.TextResponse("ok")
			}

			srv, err := serve.New(serve.Options{
				Channels:    []serve.Channel{ch},
				Config:      servedConfig(),
				Provider:    agenttest.NewScriptedProvider(GinkgoTB(), responses...),
				Concurrency: 2,
				Logger:      quietLogger(),
			})
			Expect(err).ToNot(HaveOccurred())

			// A run is live from the moment it reports Starting, which the agent emits
			// once before its loop, until its outcome is reported. The channel records
			// the outcome underneath whatever Done the work carries, so watching a run
			// end here does not cost the record Outcomes is asserted on.
			for _, w := range work {
				w.Done = func(context.Context, serve.Outcome) error {
					mu.Lock()
					live--
					mu.Unlock()

					return nil
				}
				w.Events = newStartProbe(func() {
					mu.Lock()
					live++
					if live > peak {
						peak = live
					}
					mu.Unlock()

					// Hold the run open briefly so a bound higher than the real
					// concurrency would show up as an overlap rather than being
					// hidden by runs finishing before their siblings begin.
					time.Sleep(20 * time.Millisecond)
				})
			}

			Expect(srv.Serve(ctx)).To(Succeed())
			Expect(ch.Outcomes()).To(HaveLen(len(work)))

			mu.Lock()
			defer mu.Unlock()
			Expect(peak).To(BeNumerically("<=", 2))

			for _, out := range ch.Outcomes() {
				Expect(out.Crashed).To(BeFalse(), "a crashed run would still be counted, hiding a broken bound")
			}
		})

		It("Should report work taken but never started when the server stops", func() {
			started := make(chan struct{})
			release := make(chan struct{})

			// The first run parks inside Starting, so it holds the only slot until the
			// spec lets go. The puller then has the second piece of work in hand and is
			// waiting for a slot that cannot free, which is the state a shutdown has to
			// report rather than silently drop.
			first := &serve.Work{ID: "first", Prompt: "go", Events: newStartProbe(func() {
				close(started)
				<-release
			})}
			second := &serve.Work{ID: "second", Prompt: "go"}
			ch := agenttest.NewScriptedChannel(GinkgoTB(), "jobs", first, second)

			srv, err := serve.New(serve.Options{
				Channels:    []serve.Channel{ch},
				Config:      servedConfig(),
				Provider:    agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("ok")),
				Concurrency: 1,
				Logger:      quietLogger(),
			})
			Expect(err).ToNot(HaveOccurred())

			runCtx, stop := context.WithCancel(ctx)
			served := make(chan error, 1)
			go func() { served <- srv.Serve(runCtx) }()

			Eventually(started).Should(BeClosed())
			stop()

			Eventually(ch.Outcomes).Should(ContainElement(And(
				HaveField("ID", Equal("second")),
				HaveField("Abandoned", BeTrue()),
				HaveField("Reason", BeEmpty()),
				HaveField("Stats", BeNil()),
			)), "nothing ran for it, so a retry elsewhere is safe")

			close(release)
			Eventually(served).Should(Receive(BeNil()))
		})
	})

	// A channel is somebody else's code. These drive the two calls the server makes into
	// one, Next and Work.Done, with an implementation that panics in each.
	Describe("A channel that panics", func() {
		// A worker with one channel would otherwise end its only puller, return nil and
		// exit zero with its queue served by nobody.
		It("Should end the puller and fault the server", func() {
			broken := &panicNextChannel{}
			good := agenttest.NewQueue(GinkgoTB(), "good")

			srv, err := serve.New(serve.Options{
				Channels: []serve.Channel{broken, good},
				Config:   servedConfig(),
				Provider: agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("ok")),
				Logger:   quietLogger(),
			})
			Expect(err).ToNot(HaveOccurred())

			err = srv.Serve(ctx)
			Expect(err).To(MatchError(serve.ErrChannelPanic))
			Expect(err).To(MatchError(ContainSubstring("taking work is broken")), "the panic value reaches the program that has to report it")

			Expect(broken.Calls()).To(Equal(1), "a panic is a bug in the channel, so the same call is not made again")
			Expect(good.Closes()).To(BeNumerically(">=", 1), "the fault drains the endpoints that were still working")
		})

		It("Should lose the outcome and take the next piece of work", func() {
			broken := &panicDoneChannel{}

			srv, err := serve.New(serve.Options{
				Channels:    []serve.Channel{broken},
				Config:      servedConfig(),
				Provider:    agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("one"), agenttest.TextResponse("two")),
				Concurrency: 1,
				Logger:      quietLogger(),
			})
			Expect(err).ToNot(HaveOccurred())
			Expect(srv.Serve(ctx)).To(Succeed())

			calls, dones := broken.Counts()
			Expect(dones).To(Equal(2), "the second run needs the slot the first one held, so a slot lost to the panic would have deadlocked here")
			Expect(calls).To(Equal(3), "the channel is asked again after the panic and reports it is finished")
		})

		It("Should survive a panic on the abandon path", func() {
			started := make(chan struct{})
			release := make(chan struct{})

			var mu sync.Mutex
			var abandoned serve.Outcome
			var dones int

			// The first run parks inside Starting and holds the only slot, so the second
			// piece of work is taken and then abandoned when the context ends. Its Done
			// runs on the puller's own goroutine, outside the recover that guards a run.
			first := &serve.Work{ID: "first", Prompt: "go", Events: newStartProbe(func() {
				close(started)
				<-release
			})}
			second := &serve.Work{ID: "second", Prompt: "go", Done: func(_ context.Context, out serve.Outcome) error {
				mu.Lock()
				abandoned = out
				dones++
				mu.Unlock()

				panic("reporting is broken")
			}}
			ch := agenttest.NewScriptedChannel(GinkgoTB(), "jobs", first, second)

			srv, err := serve.New(serve.Options{
				Channels:    []serve.Channel{ch},
				Config:      servedConfig(),
				Provider:    agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("ok")),
				Concurrency: 1,
				Logger:      quietLogger(),
			})
			Expect(err).ToNot(HaveOccurred())

			runCtx, stop := context.WithCancel(ctx)
			served := make(chan error, 1)
			go func() { served <- srv.Serve(runCtx) }()

			Eventually(started).Should(BeClosed())
			stop()

			Eventually(func() int {
				mu.Lock()
				defer mu.Unlock()

				return dones
			}).Should(Equal(1))

			close(release)
			Eventually(served).Should(Receive(BeNil()))

			mu.Lock()
			defer mu.Unlock()
			Expect(abandoned.ID).To(Equal("second"))
			Expect(abandoned.Abandoned).To(BeTrue())
		})
	})
})
