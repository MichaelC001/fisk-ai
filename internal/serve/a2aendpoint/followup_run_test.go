//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2aendpoint

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/choria-io/fisk"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/serve"
)

// streamsApp is an application with one plain command, so a run has a tool set without
// anything in it needing an operator.
func streamsApp() *fisk.Application {
	app := fisk.New("app", "an app")
	app.Command("streams", "list the streams")

	return app
}

// This is the whole path in one spec: two turns of one conversation over the wire, each
// a run of its own on a worker that holds nothing between them. The specs beside it
// stop at the work the channel produces, and the agent package proves the conversation
// folding without a wire.
var _ = Describe("A conversation served over two turns", func() {
	It("Should carry the first turn into the second", func() {
		// A run needs a tool set, and this one needs nothing of it: the model answers
		// both turns from the conversation.
		app := agenttest.NewFakeApp(GinkgoTB(), streamsApp())

		cfg := parseConfig(fmt.Sprintf(`
identity: agent1
application_path: %s
system_prompt: answer about streams
nats_context: ctx
llm:
  model: claude-sonnet-4-6
expose:
  agent:
    a2a:
      prompts:
        workers: 1
`, app.Path))

		built, err := NewFromConfig(cfg, ConfigOptions{Conns: provider, Logger: quietLogger()})
		Expect(err).ToNot(HaveOccurred())

		model := agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.TextResponse("there are three streams"),
			agenttest.TextResponse("the first one is ORDERS"),
		)

		srv, err := serve.New(serve.Options{
			Channels:   []serve.Channel{channelOf(built)},
			Config:     cfg,
			ConfigFile: "agent.yaml",
			StoreDir:   GinkgoT().TempDir(),
			Provider:   model,
			Logger:     quietLogger(),
		})
		Expect(err).ToNot(HaveOccurred())

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		DeferCleanup(cancel)

		served := make(chan error, 1)
		go func() {
			defer GinkgoRecover()

			served <- srv.Serve(ctx)
		}()
		DeferCleanup(func() {
			Expect(srv.Stop()).To(Succeed())
			Eventually(served, 10*time.Second).Should(Receive(Succeed()))
		})

		transport, err := a2a.NewTransport("nats", provider, a2a.TransportConfig{Identity: "caller1", Timeout: 5 * time.Second})
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(transport.Close)

		client, err := a2a.NewClient(transport, "caller1")
		Expect(err).ToNot(HaveOccurred())

		// turn sends a request and reads its set to the terminal message, returning the
		// ack it was accepted with and the answer it ended on.
		turn := func(req *a2a.Request) (*a2a.Ack, *a2a.Result) {
			GinkgoHelper()

			stream, serr := client.Task(ctx, "agent1", req)
			Expect(serr).ToNot(HaveOccurred())
			DeferCleanup(stream.Close)

			var (
				ack    *a2a.Ack
				result *a2a.Result
			)

			for result == nil {
				msg, nerr := stream.Next(ctx)
				if nerr == io.EOF {
					break
				}
				Expect(nerr).ToNot(HaveOccurred())

				switch m := msg.(type) {
				case *a2a.Ack:
					ack = m
				case *a2a.ErrorMessage:
					Fail(fmt.Sprintf("the turn ended in error: %s (%s)", m.Err, m.Code))
				case *a2a.Result:
					result = m
				}
			}

			Expect(ack).ToNot(BeNil(), "the set carried no ack")
			Expect(result).ToNot(BeNil(), "the set ended with no result")

			return ack, result
		}

		ack, first := turn(a2a.NewRequest("how many streams are there"))
		Expect(first.Text).To(Equal("there are three streams"))
		Expect(ack.ConversationToken).ToNot(BeEmpty())

		echoed, second := turn(a2a.NewFollowUp(ack, "what is the first one called"))
		Expect(second.Text).To(Equal("the first one is ORDERS"))
		Expect(echoed.ConversationToken).To(Equal(ack.ConversationToken))

		// The caller's own correlation tag came with the token, so both turns of the
		// conversation carry one tag.
		Expect(echoed.Conversation).To(Equal(ack.Conversation))

		// The second run rehydrated the first turn from the store: the model saw the
		// whole conversation rather than a prompt on its own.
		Expect(model.Requests()).To(HaveLen(2))
		Expect(userTexts(model.Requests()[0].Messages)).To(Equal([]string{"how many streams are there"}))
		Expect(userTexts(model.Requests()[1].Messages)).To(Equal([]string{"how many streams are there", "what is the first one called"}))
		Expect(model.Requests()[1].Messages).To(HaveLen(3), "user, assistant, user")
	})
})

// userTexts is the text of every user message in a conversation, in order, so a spec
// can assert which turns a model call carried.
func userTexts(msgs []llm.Message) []string {
	var out []string

	for _, m := range msgs {
		if m.Role != llm.RoleUser {
			continue
		}
		for _, b := range m.Content {
			if b.Text != nil {
				out = append(out, b.Text.Text)
			}
		}
	}

	return out
}
