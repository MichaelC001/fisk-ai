//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2aendpoint_test

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/a2a"
	wire "github.com/choria-io/fisk-ai/internal/a2a/wire/v1"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/serve"
	"github.com/choria-io/fisk-ai/internal/serve/a2aendpoint"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/toolkit/functool"
)

// streamingFake registers whatever handler it is given and answers nothing, which is all
// a prompt channel asks of a transport while it is being built. agenttest.FakeTransport
// carries a reply set and no task path, so a channel cannot be built over it.
type streamingFake struct {
	mu       sync.Mutex
	closes   int
	serveErr error
}

func (f *streamingFake) RoundTrip(context.Context, string, a2a.RouteHint, []byte) ([]byte, error) {
	return nil, errors.New("this transport sends nothing")
}

func (f *streamingFake) Stream(context.Context, string, a2a.RouteHint, []byte) (a2a.Reader, error) {
	return nil, errors.New("this transport sends nothing")
}

func (f *streamingFake) Serve(a2a.RouteHint, a2a.Handler) error { return nil }

func (f *streamingFake) ServeReplySet(a2a.RouteHint, a2a.ReplySetHandler) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.serveErr
}

func (f *streamingFake) WatchCancel(string, a2a.Handler) (a2a.TaskWatch, error) {
	return nil, errors.New("this transport watches nothing")
}

func (f *streamingFake) SendCancel(context.Context, string, string, []byte) ([]byte, error) {
	return nil, errors.New("this transport sends nothing")
}

func (f *streamingFake) WatchElicitReplies(string, a2a.Handler) (a2a.TaskWatch, error) {
	return nil, errors.New("this transport watches nothing")
}

func (f *streamingFake) SendElicitReply(context.Context, string, string, []byte) ([]byte, error) {
	return nil, errors.New("this transport sends nothing")
}

func (f *streamingFake) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++

	return nil
}

func (f *streamingFake) Closes() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.closes
}

// servedTool is a tool the a2a server will carry: it declares a2a exposure, it is not
// confirmation-gated, and it advertises a description.
func servedTool(name string, expose bool) toolkit.Tool {
	GinkgoHelper()

	spec := functool.Spec{
		Name:        name,
		Description: "does a thing",
		Schema:      map[string]any{"type": "object"},
		Handler: func(context.Context, json.RawMessage, *functool.CallContext) (string, error) {
			return "ok", nil
		},
	}
	if expose {
		spec.Expose = &functool.ExposeSpec{A2A: true}
	}

	tool, err := functool.New(spec)
	Expect(err).ToNot(HaveOccurred())

	return tool
}

// toolTransport answers a peer directly and carries no task path, which is what a tool
// service needs and all it needs.
func toolTransport() *agenttest.FakeTransport {
	return agenttest.BuildFakeTransport(wire.AgentCard{Name: "agent1", Version: "1.2.3"})
}

var _ = Describe("A2A endpoints built in Go", func() {
	Describe("New", func() {
		It("Should refuse options no endpoint can be built from and release the transport", func() {
			prompts := func() *a2aendpoint.PromptOptions {
				return &a2aendpoint.PromptOptions{Workers: 1, PromptWait: time.Second, Model: "claude-sonnet-4-6"}
			}

			cases := []struct {
				desc  string
				opts  func(*streamingFake) a2aendpoint.Options
				match string
			}{
				{
					desc: "no transport",
					opts: func(*streamingFake) a2aendpoint.Options {
						return a2aendpoint.Options{Identity: "agent1", Prompts: prompts()}
					},
					match: "a Transport is required",
				},
				{
					desc: "an identity the wire format rejects",
					opts: func(f *streamingFake) a2aendpoint.Options {
						return a2aendpoint.Options{Transport: f, Identity: "agent one", Prompts: prompts()}
					},
					match: "is not an agent name",
				},
				{
					desc: "neither kind of endpoint",
					opts: func(f *streamingFake) a2aendpoint.Options {
						return a2aendpoint.Options{Transport: f, Identity: "agent1"}
					},
					match: "set Tools, Prompts or both",
				},
				{
					desc: "a tool service with no tools",
					opts: func(f *streamingFake) a2aendpoint.Options {
						return a2aendpoint.Options{Transport: f, Identity: "agent1", Tools: &a2aendpoint.ToolOptions{}}
					},
					match: "Tools.Tools is empty",
				},
				{
					desc: "a prompt channel with no workers",
					opts: func(f *streamingFake) a2aendpoint.Options {
						p := prompts()
						p.Workers = 0

						return a2aendpoint.Options{Transport: f, Identity: "agent1", Prompts: p}
					},
					match: "Prompts.Workers must be greater than zero",
				},
				{
					desc: "a prompt channel that holds a question open for no time",
					opts: func(f *streamingFake) a2aendpoint.Options {
						p := prompts()
						p.PromptWait = 0

						return a2aendpoint.Options{Transport: f, Identity: "agent1", Prompts: p}
					},
					match: "Prompts.PromptWait must be greater than zero",
				},
				{
					desc: "a prompt channel that names no model",
					opts: func(f *streamingFake) a2aendpoint.Options {
						p := prompts()
						p.Model = ""

						return a2aendpoint.Options{Transport: f, Identity: "agent1", Prompts: p}
					},
					match: "Prompts.Model is required",
				},
			}

			for _, tc := range cases {
				fake := &streamingFake{}

				eps, err := a2aendpoint.New(tc.opts(fake))
				Expect(err).To(MatchError(ContainSubstring(tc.match)), tc.desc)
				Expect(eps).To(BeNil(), tc.desc)

				if tc.match != "a Transport is required" {
					Expect(fake.Closes()).To(Equal(1), tc.desc)
				}
			}
		})

		It("Should build a tool service alone", func() {
			transport := toolTransport()

			eps, err := a2aendpoint.New(a2aendpoint.Options{
				Transport: transport,
				Identity:  "agent1",
				Version:   "1.2.3",
				Tools: &a2aendpoint.ToolOptions{
					Tools:            []toolkit.Tool{servedTool("backup", true)},
					WithheldBuiltins: []string{"ask_human_confirm"},
				},
			})
			Expect(err).ToNot(HaveOccurred())
			DeferCleanup(eps.Close)

			Expect(eps.Channel).To(BeNil(), "no prompts were asked for")
			Expect(eps.Service).ToNot(BeNil())
			Expect(eps.Service.ExposedTools()).To(Equal([]string{"backup"}))
			Expect(eps.Service.WithheldBuiltins()).To(Equal([]string{"ask_human_confirm"}))
		})

		// A caller that set neither leaves the a2a server to its own defaults, so
		// reporting the numbers it was given would print a concurrency and a timeout of
		// zero for a service that in fact paces and stops every served call.
		It("Should describe the limits a served call will actually get", func() {
			eps, err := a2aendpoint.New(a2aendpoint.Options{
				Transport: toolTransport(),
				Identity:  "agent1",
				Tools:     &a2aendpoint.ToolOptions{Tools: []toolkit.Tool{servedTool("backup", true)}},
			})
			Expect(err).ToNot(HaveOccurred())
			DeferCleanup(eps.Close)

			Expect(eps.Service.Describe()).To(ContainElements(
				serve.DescLine{Label: "Concurrency", Value: strconv.Itoa(a2a.DefaultConcurrency())},
				serve.DescLine{Label: "Tool Timeout", Value: a2a.DefaultCallTimeout.String()},
			))
		})

		It("Should build a prompt channel alone", func() {
			transport := &streamingFake{}

			eps, err := a2aendpoint.New(a2aendpoint.Options{
				Transport: transport,
				Identity:  "agent1",
				Prompts: &a2aendpoint.PromptOptions{
					Workers:    3,
					PromptWait: 45 * time.Second,
					Model:      "claude-sonnet-4-6",
				},
			})
			Expect(err).ToNot(HaveOccurred())

			Expect(eps.Service).To(BeNil(), "no tools were asked for")
			Expect(eps.Channel).ToNot(BeNil())
			Expect(eps.Channel.Name()).To(Equal("a2a/prompts"))
			Expect(eps.Channel.Concurrency()).To(Equal(3))

			Expect(eps.Close()).To(Succeed())
			Expect(transport.Closes()).To(Equal(1))
		})

		// serve.Endpoints sorts a typed nil into its channels and the server then calls
		// Next on a nil receiver, so an endpoint that was not built is left out rather
		// than handed over.
		It("Should leave out the endpoint it did not build", func() {
			tools, err := a2aendpoint.New(a2aendpoint.Options{
				Transport: toolTransport(),
				Identity:  "agent1",
				Tools:     &a2aendpoint.ToolOptions{Tools: []toolkit.Tool{servedTool("backup", true)}},
			})
			Expect(err).ToNot(HaveOccurred())
			DeferCleanup(tools.Close)

			Expect(tools.All()).To(HaveLen(1))
			Expect(tools.All()[0]).To(BeIdenticalTo(tools.Service))

			prompts, err := a2aendpoint.New(a2aendpoint.Options{
				Transport: &streamingFake{},
				Identity:  "agent1",
				Prompts:   &a2aendpoint.PromptOptions{Workers: 1, PromptWait: time.Second, Model: "claude-sonnet-4-6"},
			})
			Expect(err).ToNot(HaveOccurred())
			DeferCleanup(prompts.Close)

			Expect(prompts.All()).To(HaveLen(1))
			Expect(prompts.All()[0]).To(BeIdenticalTo(prompts.Channel))
		})

		It("Should build both endpoints over one transport, the channel first", func() {
			eps, err := a2aendpoint.New(a2aendpoint.Options{
				Transport: &streamingFake{},
				Identity:  "agent1",
				Tools:     &a2aendpoint.ToolOptions{Tools: []toolkit.Tool{servedTool("backup", true)}},
				Prompts:   &a2aendpoint.PromptOptions{Workers: 1, PromptWait: time.Second, Model: "claude-sonnet-4-6"},
			})
			Expect(err).ToNot(HaveOccurred())
			DeferCleanup(eps.Close)

			all := eps.All()
			Expect(all).To(HaveLen(2))
			Expect(all[0].Name()).To(Equal("a2a/prompts"), "work arrives before tools on the banner")
			Expect(all[1].Name()).To(Equal("a2a"))
		})

		It("Should release the transport when an endpoint cannot be built", func() {
			single := toolTransport()

			_, err := a2aendpoint.New(a2aendpoint.Options{
				Transport: single,
				Identity:  "agent1",
				Prompts:   &a2aendpoint.PromptOptions{Workers: 1, PromptWait: time.Second, Model: "claude-sonnet-4-6"},
			})
			Expect(err).To(MatchError(ContainSubstring("cannot answer prompts")))
			Expect(single.Closed()).To(BeTrue(), "a transport that carries no reply set is still ours to release")

			// The channel is built first and registered the task route, so New closes the
			// channel rather than the transport under it when the service then fails: a
			// prompt admitted in between is ended with a terminal message and waited for,
			// where closing the transport alone would leave the peer holding an ack.
			// Channel.Close closes the transport once on its way through.
			partial := &streamingFake{}

			_, err = a2aendpoint.New(a2aendpoint.Options{
				Transport: partial,
				Identity:  "agent1",
				Tools:     &a2aendpoint.ToolOptions{Tools: []toolkit.Tool{servedTool("backup", false)}},
				Prompts:   &a2aendpoint.PromptOptions{Workers: 1, PromptWait: time.Second, Model: "claude-sonnet-4-6"},
			})
			Expect(err).To(MatchError(ContainSubstring("no tools available to serve over a2a")))
			Expect(partial.Closes()).To(Equal(1))
		})
	})

	Describe("Faults", func() {
		It("Should report what the sink was told", func() {
			sink := a2aendpoint.NewFaultSink()

			eps, err := a2aendpoint.New(a2aendpoint.Options{
				Transport: toolTransport(),
				Faults:    sink,
				Identity:  "agent1",
				Tools:     &a2aendpoint.ToolOptions{Tools: []toolkit.Tool{servedTool("backup", true)}},
			})
			Expect(err).ToNot(HaveOccurred())
			DeferCleanup(eps.Close)

			dropped := errors.New("the registration was dropped")
			sink.Report(dropped)
			sink.Report(errors.New("and again"))

			var got error
			Eventually(eps.Service.Faults()).Should(Receive(&got))
			Expect(got).To(MatchError(dropped), "the first fault is what ends the worker")
			Consistently(eps.Service.Faults(), 50*time.Millisecond).ShouldNot(Receive())
		})

		It("Should answer with a sink nothing writes to when it was given none", func() {
			eps, err := a2aendpoint.New(a2aendpoint.Options{
				Transport: toolTransport(),
				Identity:  "agent1",
				Tools:     &a2aendpoint.ToolOptions{Tools: []toolkit.Tool{servedTool("backup", true)}},
			})
			Expect(err).ToNot(HaveOccurred())
			DeferCleanup(eps.Close)

			Expect(eps.Service.Faults()).ToNot(BeNil())
			Consistently(eps.Service.Faults(), 50*time.Millisecond).ShouldNot(Receive())
		})
	})

	Describe("The tool service", func() {
		It("Should answer with copies a caller can sort or truncate", func() {
			eps, err := a2aendpoint.New(a2aendpoint.Options{
				Transport: toolTransport(),
				Identity:  "agent1",
				Tools: &a2aendpoint.ToolOptions{
					Tools:            []toolkit.Tool{servedTool("backup", true), servedTool("restore", true)},
					WithheldBuiltins: []string{"ask_human_confirm"},
				},
			})
			Expect(err).ToNot(HaveOccurred())
			DeferCleanup(eps.Close)

			eps.Service.ExposedTools()[0] = "clobbered"
			eps.Service.WithheldBuiltins()[0] = "clobbered"
			eps.Service.Describe()[0] = serve.DescLine{Label: "clobbered"}

			Expect(eps.Service.ExposedTools()).To(Equal([]string{"backup", "restore"}))
			Expect(eps.Service.WithheldBuiltins()).To(Equal([]string{"ask_human_confirm"}))
			Expect(eps.Service.Describe()[0].Label).To(Equal("Concurrency"))
		})
	})
})
