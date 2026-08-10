//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/choria-io/fisk"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
)

func TestServe(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Serve")
}

// servedApp is the wrapped application the served runs introspect. It carries one
// command so a run has a tool to call.
func servedApp() *fisk.Application {
	app := fisk.New("app", "an app")
	do := app.Command("do", "do a thing")
	do.Arg("subject", "the subject").Required().String()

	return app
}

// scriptedChannel hands out a fixed list of work and collects the outcomes, so a test
// can assert what the server did without standing anything up. It reports
// ErrChannelDone once the list is spent, which is what makes Serve return.
type scriptedChannel struct {
	name string
	work []*Work

	mu       sync.Mutex
	next     int
	outcomes []Outcome
}

func newScriptedChannel(name string, work ...*Work) *scriptedChannel {
	return &scriptedChannel{name: name, work: work}
}

func (c *scriptedChannel) Name() string { return c.name }

func (c *scriptedChannel) Next(_ context.Context) (*Work, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.next >= len(c.work) {
		return nil, ErrChannelDone
	}

	w := c.work[c.next]
	c.next++
	if w.Done == nil {
		w.Done = c.record
	}

	return w, nil
}

func (c *scriptedChannel) record(_ context.Context, out Outcome) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.outcomes = append(c.outcomes, out)

	return nil
}

func (c *scriptedChannel) Outcomes() []Outcome {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]Outcome(nil), c.outcomes...)
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
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
			app := agenttest.NewFakeApp(GinkgoTB(), servedApp())

			_, err := New(Options{Config: agenttest.Config(GinkgoTB(), app)})
			Expect(err).To(MatchError(ContainSubstring("at least one channel")))

			_, err = New(Options{Channels: []Channel{newScriptedChannel("c")}})
			Expect(err).To(MatchError(ContainSubstring("configuration is required")))
		})

		It("Should reject a channel with no name", func() {
			app := agenttest.NewFakeApp(GinkgoTB(), servedApp())

			_, err := New(Options{
				Channels: []Channel{newScriptedChannel("")},
				Config:   agenttest.Config(GinkgoTB(), app),
			})
			Expect(err).To(MatchError(ContainSubstring("has no name")))
		})

		It("Should reject a work directory that is not an absolute path", func() {
			app := agenttest.NewFakeApp(GinkgoTB(), servedApp())

			_, err := New(Options{
				Channels: []Channel{newScriptedChannel("c")},
				Config:   agenttest.Config(GinkgoTB(), app),
				WorkDir:  "relative",
			})
			Expect(err).To(MatchError(ContainSubstring("not an absolute path")))
		})
	})

	Describe("Serving work", func() {
		It("Should run the work and report the answer", func() {
			app := agenttest.NewFakeApp(GinkgoTB(), servedApp())
			ch := newScriptedChannel("jobs", &Work{ID: "job-1", Prompt: "go"})

			srv, err := New(Options{
				Channels: []Channel{ch},
				Config:   agenttest.Config(GinkgoTB(), app),
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
			app := agenttest.NewFakeApp(GinkgoTB(), servedApp())
			ch := newScriptedChannel("jobs", &Work{Prompt: "go"})

			srv, err := New(Options{
				Channels: []Channel{ch},
				Config:   agenttest.Config(GinkgoTB(), app),
				Provider: agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("ok")),
				Logger:   quietLogger(),
			})
			Expect(err).ToNot(HaveOccurred())
			Expect(srv.Serve(ctx)).To(Succeed())

			Expect(ch.Outcomes()[0].ID).ToNot(BeEmpty())
		})

		It("Should report a failed run without treating it as a crash", func() {
			app := agenttest.NewFakeApp(GinkgoTB(), servedApp())
			ch := newScriptedChannel("jobs", &Work{ID: "job-1", Prompt: "go"})

			srv, err := New(Options{
				Channels: []Channel{ch},
				Config:   agenttest.Config(GinkgoTB(), app, agenttest.WithMaxIterations(1)),
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
			app := agenttest.NewFakeApp(GinkgoTB(), servedApp())
			a := newScriptedChannel("a", &Work{ID: "a-1", Prompt: "go"})
			b := newScriptedChannel("b", &Work{ID: "b-1", Prompt: "go"})

			srv, err := New(Options{
				Channels: []Channel{a, b},
				Config:   agenttest.Config(GinkgoTB(), app),
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
			app := agenttest.NewFakeApp(GinkgoTB(), servedApp())
			ch := &noDoneChannel{}

			srv, err := New(Options{
				Channels: []Channel{ch},
				Config:   agenttest.Config(GinkgoTB(), app),
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
			app := agenttest.NewFakeApp(GinkgoTB(), servedApp())

			work := make([]*Work, 6)
			for i := range work {
				work[i] = &Work{ID: fmt.Sprintf("job-%d", i), Prompt: "go"}
			}
			ch := newScriptedChannel("jobs", work...)

			var mu sync.Mutex
			var live, peak int

			responses := make([]*llm.Response, len(work))
			for i := range responses {
				responses[i] = agenttest.TextResponse("ok")
			}

			srv, err := New(Options{
				Channels:    []Channel{ch},
				Config:      agenttest.Config(GinkgoTB(), app),
				Provider:    agenttest.NewScriptedProvider(GinkgoTB(), responses...),
				Concurrency: 2,
				Logger:      quietLogger(),
			})
			Expect(err).ToNot(HaveOccurred())

			// A run is live from the moment it reports Starting, which the agent emits
			// once before its loop, until its outcome is reported.
			for _, w := range work {
				done := ch.record
				w.Done = func(ctx context.Context, out Outcome) error {
					mu.Lock()
					live--
					mu.Unlock()

					return done(ctx, out)
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
			app := agenttest.NewFakeApp(GinkgoTB(), servedApp())

			started := make(chan struct{})
			release := make(chan struct{})

			// The first run parks inside Starting, so it holds the only slot until the
			// test lets go. The puller then has the second piece of work in hand and is
			// waiting for a slot that cannot free, which is the state a shutdown has to
			// report rather than silently drop.
			first := &Work{ID: "first", Prompt: "go", Events: newStartProbe(func() {
				close(started)
				<-release
			})}
			second := &Work{ID: "second", Prompt: "go"}
			ch := newScriptedChannel("jobs", first, second)

			srv, err := New(Options{
				Channels:    []Channel{ch},
				Config:      agenttest.Config(GinkgoTB(), app),
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

	Describe("Budget clamping", func() {
		It("Should lower a configured limit but never raise it", func() {
			app := agenttest.NewFakeApp(GinkgoTB(), servedApp())

			srv, err := New(Options{
				Channels: []Channel{newScriptedChannel("c")},
				Config:   agenttest.Config(GinkgoTB(), app, agenttest.WithMaxIterations(10)),
				Logger:   quietLogger(),
			})
			Expect(err).ToNot(HaveOccurred())

			By("lowering a limit the work asks to lower")
			Expect(srv.clampedConfig(Budget{MaxIterations: 3}).LLM.Budget.MaxIterations).To(BeNumerically("==", 3))

			By("ignoring a limit above the configured ceiling")
			Expect(srv.clampedConfig(Budget{MaxIterations: 99}).LLM.Budget.MaxIterations).To(BeNumerically("==", 10))

			By("leaving the configuration alone when nothing is asked")
			Expect(srv.clampedConfig(Budget{})).To(BeIdenticalTo(srv.opts.Config))

			By("never mutating the shared configuration")
			Expect(srv.opts.Config.LLM.Budget.MaxIterations).To(BeNumerically("==", 10))
		})
	})
})

// startProbe is an events sink that only cares that a run began, so a test can see
// when one is live. It embeds a real recorder rather than a nil interface, or every
// other method would panic into the run's barrier and turn each run into a crash the
// test never notices.
type startProbe struct {
	agent.Events
	onStart func()
}

func newStartProbe(onStart func()) *startProbe {
	return &startProbe{Events: agenttest.NewRecordingEvents(), onStart: onStart}
}

func (p *startProbe) Starting(agent.RunInfo) { p.onStart() }

// noDoneChannel supplies one piece of work with no Done callback, then reports it is
// finished, so a test can prove the server drops it rather than running it.
type noDoneChannel struct {
	calls int
}

func (c *noDoneChannel) Name() string { return "no-done" }

func (c *noDoneChannel) Next(_ context.Context) (*Work, error) {
	c.calls++
	if c.calls == 1 {
		return &Work{ID: "orphan", Prompt: "go"}, nil
	}

	return nil, ErrChannelDone
}
