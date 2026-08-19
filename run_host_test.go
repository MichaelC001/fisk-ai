//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/choria-io/fisk"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/agenttest"
)

// hostedLogger keeps the worker's own logging out of a spec's output. What a terminal
// wants from a hosted agent is the run's events, not the machinery behind them.
func hostedLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// renderingHandler is a client that keeps what it was shown, which is what a terminal
// does with the same messages.
type renderingHandler struct {
	blocks []a2a.Block
}

func (h *renderingHandler) Block(b a2a.Block) { h.blocks = append(h.blocks, b) }

func (h *renderingHandler) Question(_ context.Context, ask *a2a.ElicitRequest) (*a2a.ElicitReply, error) {
	return a2a.NewNoOperatorReply(ask, "terminal"), nil
}

func (h *renderingHandler) texts() []string {
	var out []string
	for _, b := range h.blocks {
		text, ok := b.Content().(a2a.TextBlock)
		if !ok {
			continue
		}
		out = append(out, text.Text)
	}

	return out
}

var _ = Describe("hostAgent", func() {
	var (
		cfg  *config.Config
		host *hostedAgent
	)

	BeforeEach(func() {
		app := fisk.New("app", "an app")
		app.Command("backup", "back a thing up")

		path := agenttest.NewFakeApp(GinkgoTB(), app).Path

		var err error
		cfg, err = config.ParseConfigForMode([]byte(fmt.Sprintf(
			"identity: hosted1\napplication_path: %s\nsystem_prompt: you are a test agent\nllm:\n  model: claude-opus-4-8\n", path)),
			config.ModeAgent)
		Expect(err).ToNot(HaveOccurred())
	})

	AfterEach(func() {
		if host != nil {
			Expect(host.Close()).To(Succeed())
			host = nil
		}
	})

	start := func(provider *agenttest.ScriptedProvider) {
		GinkgoHelper()

		var err error
		host, err = hostAgent(context.Background(), hostOptions{
			Config:   cfg,
			Provider: provider,
			Logger:   hostedLogger(),
		})
		Expect(err).ToNot(HaveOccurred())
	}

	// The whole point of the phase in one spec: a prompt goes out over the wire, an
	// agent in this process answers it, and what comes back is what a remote worker
	// would have sent.
	It("Should answer a prompt over the wire from inside this process", func() {
		start(agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("there are three streams")))

		handler := &renderingHandler{}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		out, err := host.client.RunTask(ctx, host.identity, a2a.NewRequest("how many streams are there"), handler)
		Expect(err).ToNot(HaveOccurred())

		Expect(out.Error).To(BeNil())
		Expect(out.Result).ToNot(BeNil())
		Expect(out.Result.Text).To(Equal("there are three streams"))
		Expect(out.Ack.ConversationToken).ToNot(BeEmpty(), "a terminal keeps this to add a turn")

		// The same text arrives as the final block, which is how a client renders an
		// answer as it is produced rather than waiting for the terminal message.
		Expect(handler.texts()).To(ContainElement("there are three streams"))
	})

	// That the exchange above reaches nothing outside this process is the embedded
	// broker's own claim, asserted there against the server's listeners. A connection
	// made through the server object reports a placeholder address, so nothing here
	// could tell the difference.

	// The configuration a terminal runs under says nothing about serving anything, so
	// the exposure is synthesized, and an operator's own exposure is replaced rather
	// than added to: hosting a run here registers no tool service.
	It("Should host prompts and nothing else", func() {
		cfg.Identity = ""
		cfg.Expose = &config.ExposeConfig{Agent: &config.AgentExpose{
			A2A: &config.ExposedA2AConfig{ServeTools: true},
		}}

		hosted, identity := hostedConfig(cfg)

		Expect(identity).To(Equal(localIdentity), "a run with no identity of its own still needs a name")
		Expect(hosted.A2APromptsEnabled()).To(BeTrue())
		Expect(hosted.A2AServeToolsEnabled()).To(BeFalse())
		Expect(hosted.A2APromptsElicit()).To(BeTrue(), "the person is right here to be asked")
		Expect(cfg.Expose.Agent.A2A.ServeTools).To(BeTrue(), "the operator's own configuration is left alone")
	})
})
