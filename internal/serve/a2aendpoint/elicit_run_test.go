//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2aendpoint

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/choria-io/fisk"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/a2a"
	natstransport "github.com/choria-io/fisk-ai/internal/a2a/nats"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/serve"
)

// gatedApp is an application whose one command is confirmation-gated, so a run that
// calls it reaches the gate before the command executes.
func gatedApp() *fisk.Application {
	app := fisk.New("app", "an app")
	app.Command("wipe", "delete everything").Tag("ai:confirm")

	return app
}

// This is the whole path in one spec, which no other spec covers: the specs beside it
// drive the prompter directly, and the agent package proves the gate without a wire.
var _ = Describe("A gated command in a served run", func() {
	It("Should ask the caller and run the command on its answer", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), gatedApp())

		cfg := parseConfig(fmt.Sprintf(`
identity: agent1
application_path: %s
system_prompt: do the thing
nats_context: ctx
llm:
  model: claude-sonnet-4-6
expose:
  agent:
    a2a:
      prompts:
        workers: 1
        elicit: true
`, app.Path))

		built, err := NewFromConfig(cfg, ConfigOptions{Conns: provider, Logger: quietLogger()})
		Expect(err).ToNot(HaveOccurred())

		ch := channelOf(built)

		// How long one question is held before the worker gives up on it, shortened so
		// the caller below can outlast it rather than the spec sitting out two minutes.
		ch.promptWait = 500 * time.Millisecond

		// The model calls the gated command, then answers once it has its result.
		srv, err := serve.New(serve.Options{
			Channels:   []serve.Channel{ch},
			Config:     cfg,
			ConfigFile: "agent.yaml",
			StoreDir:   GinkgoT().TempDir(),
			Provider: agenttest.NewScriptedProvider(GinkgoTB(),
				agenttest.ToolUseResponse("c1", "wipe", json.RawMessage(`{}`)),
				agenttest.TextResponse("everything is gone"),
			),
			Logger: quietLogger(),
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

		transport, err := a2a.NewTransport("nats", a2a.TransportConfig{Resources: provider, Identity: "caller1", Timeout: 5 * time.Second})
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(transport.Close)

		client, err := a2a.NewClient(transport, "caller1")
		Expect(err).ToNot(HaveOccurred())

		stream, err := client.Task(ctx, "agent1", a2a.NewRequest("wipe it"))
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(stream.Close)

		// The caller reads its set, answers the question that arrives in it, and keeps
		// reading to the terminal message.
		var (
			asked  *a2a.ElicitRequest
			result *a2a.Result
		)

		for result == nil {
			msg, nerr := stream.Next(ctx)
			if nerr == io.EOF {
				break
			}
			Expect(nerr).ToNot(HaveOccurred())

			switch m := msg.(type) {
			case *a2a.ElicitRequest:
				asked = m
				Expect(m.WaitMS).To(Equal(int64(500)), "the caller is told what it has to beat")

				// A person reading the command takes longer than one window. Saying so
				// buys another, twice, so the approval below lands past the point an
				// unattended question would have been given up on.
				for range 2 {
					time.Sleep(300 * time.Millisecond)

					held, merr := json.Marshal(a2a.NewWaitingAck(m, "caller1"))
					Expect(merr).ToNot(HaveOccurred())

					acked, rerr := nc.Request(natstransport.ElicitSubject("agent1", m.Request), held, 5*time.Second)
					Expect(rerr).ToNot(HaveOccurred())
					Expect(acked.Header.Get("Nats-Service-Error-Code")).To(BeEmpty())
				}

				body, merr := json.Marshal(a2a.NewApproveReply(m, "caller1", a2a.ChoiceOnce))
				Expect(merr).ToNot(HaveOccurred())

				answered, rerr := nc.Request(natstransport.ElicitSubject("agent1", m.Request), body, 5*time.Second)
				Expect(rerr).ToNot(HaveOccurred())
				Expect(answered.Header.Get("Nats-Service-Error-Code")).To(BeEmpty())

			case *a2a.Result:
				result = m
			}
		}

		Expect(asked).ToNot(BeNil(), "the run put no question to the caller")
		Expect(asked.Kind).To(Equal(a2a.ElicitApprove))
		Expect(asked.Command).To(Equal("wipe"))
		Expect(asked.Tag).To(Equal("ai:confirm"))

		Expect(result).ToNot(BeNil(), "the set ended with no result")
		Expect(result.StopReason).To(Equal(a2a.StopEndTurn))
		Expect(result.Text).To(Equal("everything is gone"))
	})
})
