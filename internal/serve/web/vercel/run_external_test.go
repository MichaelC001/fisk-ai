//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package vercel_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/choria-io/fisk"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/serve"
	"github.com/choria-io/fisk-ai/internal/serve/web"
	"github.com/choria-io/fisk-ai/internal/serve/web/vercel"
)

// gatedApp is an application of three commands, two confirmation-gated so a run that
// calls one reaches the gate before the command executes, and one that runs on its own.
//
// The gated subcommand is the tool stream_rm running the command "stream rm", so a spec
// on the card a conversation stopped at tells the model's name for the tool from the
// command path the operator approves.
func gatedApp() *fisk.Application {
	app := fisk.New("app", "an app")
	app.Command("wipe", "delete everything").Tag("ai:confirm")
	app.Command("list", "list everything")
	app.Command("stream", "manage streams").Command("rm", "delete a stream").Tag("ai:confirm")

	return app
}

// servedChannel is a web channel with this format mounted, sharing the store a spec
// passes. It resolves the agent's tools the way a configured channel does, since a
// conversation opened on an outstanding approval is rendered from the tool that runs the
// command.
func servedChannel(cfg *config.Config, store runstate.Store) *web.Channel {
	GinkgoHelper()

	tools, err := web.ResolveAgentTools(context.Background(), cfg, nil)
	Expect(err).ToNot(HaveOccurred())

	ch, err := web.New(web.Options{
		Listen:      "127.0.0.1:0",
		BasePath:    "/fisk/v1",
		Origins:     []string{"http://localhost:5173"},
		Identity:    cfg.Identity,
		Workers:     2,
		Formats:     []web.Mount{vercel.Mount()},
		CardTools:   tools.Tools,
		ConfirmTags: cfg.ConfirmTags(),
		Sessions:    store,
		Logger:      quietLogger(),
	})
	Expect(err).ToNot(HaveOccurred())
	DeferCleanup(func() { Expect(ch.Close()).To(Succeed()) })

	return ch
}

// serveAll hosts the channel on a server driven by the scripted provider, and stops it
// when the spec ends.
func serveAll(cfg *config.Config, store runstate.Store, provider llm.Provider, channels ...*web.Channel) {
	GinkgoHelper()

	hosted := make([]serve.Channel, 0, len(channels))
	for _, ch := range channels {
		hosted = append(hosted, ch)
	}

	srv, err := serve.New(serve.Options{
		Channels:     hosted,
		Config:       cfg,
		ConfigFile:   "agent.yaml",
		StoreDir:     GinkgoT().TempDir(),
		Provider:     provider,
		SessionStore: store,
		Logger:       quietLogger(),
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
}

// post sends one body to the mounted format and returns the status with the response
// as the page read it, the minted message id pinned.
func post(ch *web.Channel, body string) (int, string) {
	GinkgoHelper()

	resp := send(ch, body)
	defer resp.Body.Close()

	read, err := io.ReadAll(resp.Body)
	Expect(err).ToNot(HaveOccurred())

	return resp.StatusCode, pinned(string(read))
}

// send posts a body and returns the response for a spec that reads its headers.
func send(ch *web.Channel, body string) *http.Response {
	GinkgoHelper()

	req, err := http.NewRequest(http.MethodPost, "http://"+ch.Addr()+"/fisk/v1/vercel", bytes.NewReader([]byte(body)))
	Expect(err).ToNot(HaveOccurred())
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	Expect(err).ToNot(HaveOccurred())

	return resp
}

// prompt is the body useChat posts for a typed message, whose history this format
// reads the newest user message out of and nothing else.
func prompt(chat string, text string) string {
	return `{"id":"` + chat + `","messages":[{"role":"user","parts":[{"type":"text","text":"` + text + `"}]}]}`
}

// reopen is a page picking a conversation out of the rail: the session the thread ran in,
// read back through the format that renders it.
func reopen(cfg *config.Config, ch *web.Channel, thread string) (int, string) {
	GinkgoHelper()

	url := "http://" + ch.Addr() + "/fisk/v1/vercel/sessions/" + web.SessionFor(cfg.Identity, thread)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	Expect(err).ToNot(HaveOccurred())

	resp, err := http.DefaultClient.Do(req)
	Expect(err).ToNot(HaveOccurred())
	defer resp.Body.Close()

	read, err := io.ReadAll(resp.Body)
	Expect(err).ToNot(HaveOccurred())

	return resp.StatusCode, pinned(string(read))
}

// These drive the whole path: a page posts to the hosted channel, the run executes
// against the scripted model and the fake application, and the bytes the page read are
// what is asserted on.
var _ = Describe("A served turn", func() {
	var (
		store *agenttest.FakeSessionStore
		cfg   *config.Config
	)

	BeforeEach(func() {
		store = agenttest.NewFakeSessionStore(GinkgoTB())
		cfg = agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), gatedApp()), agenttest.WithHITL())
	})

	It("Should stream an answer as the model writes it", func() {
		ch := servedChannel(cfg, store)
		serveAll(cfg, store, agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("hello there")), ch)

		resp := send(ch, prompt("t1", "say hello"))
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		Expect(resp.Header.Get("Content-Type")).To(Equal("text/event-stream"))
		Expect(resp.Header.Get("X-Vercel-Ai-Ui-Message-Stream")).To(Equal("v1"))
		Expect(resp.Header.Get("X-Accel-Buffering")).To(Equal("no"))

		read, err := io.ReadAll(resp.Body)
		Expect(err).ToNot(HaveOccurred())
		Expect(resp.Body.Close()).To(Succeed())

		Expect(pinned(string(read))).To(Equal(sse(
			`{"type":"start","messageId":"MID"}`,
			`{"type":"text-start","id":"1"}`,
			`{"type":"text-delta","id":"1","delta":"hello "}`,
			`{"type":"text-delta","id":"1","delta":"there"}`,
			`{"type":"text-end","id":"1"}`,
			`{"type":"finish","finishReason":"stop"}`,
			`[DONE]`,
		)))
	})

	It("Should render a call and its result on one tool part", func() {
		ch := servedChannel(cfg, store)
		serveAll(cfg, store, agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("c1", "list", json.RawMessage(`{}`)),
			agenttest.TextResponse("done"),
		), ch)

		status, body := post(ch, prompt("t1", "list it"))
		Expect(status).To(Equal(http.StatusOK))
		Expect(body).To(ContainSubstring(`data: {"type":"tool-input-available","toolCallId":"c1","toolName":"list","input":{},"dynamic":true}`))
		Expect(body).To(ContainSubstring(`data: {"type":"tool-output-available","toolCallId":"c1","output":"`), "the call and its result share one toolCallId")
		Expect(body).To(ContainSubstring(`data: {"type":"finish","finishReason":"stop"}`))
	})

	// The gate asks before the run traces the call, so the tool part the approval
	// mutates is sent here or the client drops the approval in silence.
	It("Should end the turn on the approval of a gated command", func() {
		ch := servedChannel(cfg, store)
		serveAll(cfg, store, agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("c1", "wipe", json.RawMessage(`{}`)),
			agenttest.TextResponse("everything is gone"),
		), ch)

		status, body := post(ch, prompt("t2", "wipe it"))
		Expect(status).To(Equal(http.StatusOK))
		Expect(body).To(Equal(sse(
			`{"type":"start","messageId":"MID"}`,
			`{"type":"tool-input-available","toolCallId":"c1","toolName":"wipe","input":{"command":"wipe","tag":"ai:confirm"},"dynamic":true}`,
			`{"type":"tool-approval-request","approvalId":"approval-c1","toolCallId":"c1","reason":"wipe"}`,
			`{"type":"finish","finishReason":"tool-calls"}`,
			`[DONE]`,
		)))
	})

	// The resume traces the call the answer named, and a second tool-input-available
	// under that toolCallId would draw a second card for the one command.
	It("Should leave out the traced call the answer named and run it", func() {
		first := servedChannel(cfg, store)
		second := servedChannel(cfg, store)
		serveAll(cfg, store, agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("c1", "wipe", json.RawMessage(`{}`)),
			agenttest.TextResponse("everything is gone"),
		), first, second)

		status, _ := post(first, prompt("t3", "wipe it"))
		Expect(status).To(Equal(http.StatusOK))

		// The answer goes to a second channel on the same store, which is what a second
		// process behind a load balancer is.
		status, body := post(second, `{"id":"t3","messages":[{"role":"user","parts":[{"type":"text","text":"wipe it"}]}],"fiskAnswer":{"toolUseId":"c1","kind":"approve","approval":"once"}}`)
		Expect(status).To(Equal(http.StatusOK))
		Expect(body).ToNot(ContainSubstring(`"type":"tool-input-available"`), "the card the asking turn drew is the only one")
		Expect(body).To(ContainSubstring(`data: {"type":"tool-output-available","toolCallId":"c1","output":"`), "the command ran")
		Expect(body).To(ContainSubstring(`"delta":"everything "`))
		Expect(body).To(ContainSubstring(`data: {"type":"finish","finishReason":"stop"}`))
	})

	// The client keeps building the assistant message it holds while a question is
	// outstanding, and matches the id in the response head against it. A head naming a
	// new message appends a second one, which leaves the answered question's card
	// standing beside a copy of itself carrying the result.
	It("Should write into the assistant message the answering request names", func() {
		ch := servedChannel(cfg, store)
		serveAll(cfg, store, agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("c1", "wipe", json.RawMessage(`{}`)),
			agenttest.TextResponse("everything is gone"),
		), ch)

		status, _ := post(ch, prompt("t9", "wipe it"))
		Expect(status).To(Equal(http.StatusOK))

		status, body := post(ch, `{"id":"t9","messages":[
			{"role":"user","parts":[{"type":"text","text":"wipe it"}]},
			{"id":"msg-7","role":"assistant","parts":[{"type":"dynamic-tool","toolCallId":"c1","state":"approval-responded","approval":{"id":"approval-c1","approved":true}}]}
		]}`)
		Expect(status).To(Equal(http.StatusOK))
		Expect(body).To(ContainSubstring(`data: {"type":"start","messageId":"msg-7"}`))
	})

	// A turn the page opened with a message of its own starts an assistant message,
	// since there is nothing on the client to write into.
	It("Should start a message when the newest message is the page's own", func() {
		ch := servedChannel(cfg, store)
		serveAll(cfg, store, agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("hello there")), ch)

		status, body := post(ch, `{"id":"t10","messages":[
			{"id":"msg-3","role":"assistant","parts":[{"type":"text","text":"earlier"}]},
			{"id":"msg-4","role":"user","parts":[{"type":"text","text":"say hello"}]}
		]}`)
		Expect(status).To(Equal(http.StatusOK))
		Expect(body).ToNot(ContainSubstring(`"messageId":"msg-3"`))
		Expect(body).ToNot(ContainSubstring(`"messageId":"msg-4"`))
	})

	// A page that never sets fiskAnswer answers through the SDK's own approval part,
	// which is what makes a stock frontend work against this.
	It("Should take an approval the client answered on the tool part", func() {
		ch := servedChannel(cfg, store)
		serveAll(cfg, store, agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("c1", "wipe", json.RawMessage(`{}`)),
			agenttest.TextResponse("everything is gone"),
		), ch)

		status, _ := post(ch, prompt("t4", "wipe it"))
		Expect(status).To(Equal(http.StatusOK))

		status, body := post(ch, `{"id":"t4","messages":[
			{"role":"user","parts":[{"type":"text","text":"wipe it"}]},
			{"role":"assistant","parts":[{"type":"dynamic-tool","toolCallId":"c1","state":"approval-responded","approval":{"id":"approval-c1","approved":true}}]}
		]}`)
		Expect(status).To(Equal(http.StatusOK))
		Expect(body).To(ContainSubstring(`data: {"type":"tool-output-available","toolCallId":"c1","output":"`), "the command ran")
		Expect(body).To(ContainSubstring(`data: {"type":"finish","finishReason":"stop"}`))
	})

	// The standing allow is the answer the SDK's own approval cannot carry, and it is
	// why fiskAnswer takes an approval at all: the second gated call runs without the
	// person being asked again.
	It("Should stop asking about a tool the page allowed for the conversation", func() {
		ch := servedChannel(cfg, store)
		serveAll(cfg, store, agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("c1", "wipe", json.RawMessage(`{}`)),
			agenttest.ToolUseResponse("c2", "wipe", json.RawMessage(`{}`)),
			agenttest.TextResponse("both are gone"),
		), ch)

		status, _ := post(ch, prompt("t8", "wipe it twice"))
		Expect(status).To(Equal(http.StatusOK))

		status, body := post(ch, `{"id":"t8","messages":[{"role":"user","parts":[{"type":"text","text":"wipe it twice"}]}],"fiskAnswer":{"toolUseId":"c1","kind":"approve","approval":"always"}}`)
		Expect(status).To(Equal(http.StatusOK))
		Expect(body).To(ContainSubstring(`data: {"type":"tool-input-available","toolCallId":"c2","toolName":"wipe","input":{},"dynamic":true}`), "the second call was dispatched rather than asked about")
		Expect(body).ToNot(ContainSubstring(`"type":"tool-approval-request"`))
		Expect(body).To(ContainSubstring(`data: {"type":"finish","finishReason":"stop"}`))
	})

	It("Should refuse a command the client declined and tell the model why", func() {
		ch := servedChannel(cfg, store)
		serveAll(cfg, store, agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("c1", "wipe", json.RawMessage(`{}`)),
			agenttest.TextResponse("left alone then"),
		), ch)

		status, _ := post(ch, prompt("t5", "wipe it"))
		Expect(status).To(Equal(http.StatusOK))

		status, body := post(ch, `{"id":"t5","messages":[{"role":"user","parts":[{"type":"text","text":"wipe it"}]}],"fiskAnswer":{"toolUseId":"c1","kind":"approve","approval":"no"}}`)
		Expect(status).To(Equal(http.StatusOK))
		Expect(body).ToNot(ContainSubstring(`"type":"tool-output-available"`), "the command did not run")
		Expect(body).To(ContainSubstring(`"delta":"left "`), "the model was told the command was refused and answered")
	})

	DescribeTable("A human-in-the-loop question",
		func(tool string, input string, question string, answer string) {
			ch := servedChannel(cfg, store)
			serveAll(cfg, store, agenttest.NewScriptedProvider(GinkgoTB(),
				agenttest.ToolUseResponse("c1", tool, json.RawMessage(input)),
				agenttest.TextResponse("thanks"),
			), ch)

			status, body := post(ch, prompt("t6", "ask me"))
			Expect(status).To(Equal(http.StatusOK))
			Expect(body).To(ContainSubstring(`data: {"type":"tool-input-available","toolCallId":"c1","toolName":"` + tool + `"`))
			Expect(body).To(ContainSubstring(question))
			Expect(body).To(ContainSubstring(`data: {"type":"finish","finishReason":"tool-calls"}`))

			status, body = post(ch, `{"id":"t6","messages":[{"role":"user","parts":[{"type":"text","text":"ask me"}]}],"fiskAnswer":`+answer+`}`)
			Expect(status).To(Equal(http.StatusOK))
			Expect(body).ToNot(ContainSubstring(`"type":"tool-input-available"`), "the card the asking turn drew is the only one")
			Expect(body).To(ContainSubstring(`"delta":"thanks"`), "the answer reached the tool and the run went on")
			Expect(body).To(ContainSubstring(`data: {"type":"finish","finishReason":"stop"}`))
		},
		Entry("asking for a yes or no",
			"ask_human_confirm", `{"question":"shall I?"}`,
			`data: {"type":"data-question","id":"c1","data":{"toolUseId":"c1","kind":"confirm","question":"shall I?"}}`,
			`{"toolUseId":"c1","kind":"confirm","confirmed":true}`),
		Entry("asking for one of a list",
			"ask_human_select", `{"question":"which one?","options":["ORDERS","EVENTS"]}`,
			`data: {"type":"data-question","id":"c1","data":{"toolUseId":"c1","kind":"select","question":"which one?","options":["ORDERS","EVENTS"]}}`,
			`{"toolUseId":"c1","kind":"select","index":1}`),
		Entry("asking for a value",
			"ask_human_input", `{"question":"which subject?","default":"orders"}`,
			`data: {"type":"data-question","id":"c1","data":{"toolUseId":"c1","kind":"input","question":"which subject?","default":"orders"}}`,
			`{"toolUseId":"c1","kind":"input","value":"orders.new"}`),
	)

	// A prompt sent while a question is outstanding is not delivered: the resume puts
	// the question again and the run suspends without reaching a boundary that takes a
	// user message.
	It("Should tell the page a message the conversation did not take", func() {
		ch := servedChannel(cfg, store)
		serveAll(cfg, store, agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("c1", "wipe", json.RawMessage(`{}`)),
			agenttest.TextResponse("everything is gone"),
		), ch)

		status, _ := post(ch, prompt("t7", "wipe it"))
		Expect(status).To(Equal(http.StatusOK))

		status, body := post(ch, prompt("t7", "did it work?"))
		Expect(status).To(Equal(http.StatusOK))
		Expect(body).To(ContainSubstring(`data: {"type":"tool-approval-request","approvalId":"approval-c1"`), "the question is put again")
		Expect(body).To(ContainSubstring(`data: {"type":"error","errorText":"your message was not delivered`))
	})
})

// These drive the open route: a conversation is held by running turns against the hosted
// channel, and what a page picking it out of the rail reads back is what is asserted on.
var _ = Describe("A conversation opened from the rail", func() {
	var (
		store *agenttest.FakeSessionStore
		cfg   *config.Config
	)

	BeforeEach(func() {
		store = agenttest.NewFakeSessionStore(GinkgoTB())
		cfg = agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), gatedApp()), agenttest.WithHITL())
	})

	It("Should read back the turns the conversation took", func() {
		ch := servedChannel(cfg, store)
		serveAll(cfg, store, agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("c1", "list", json.RawMessage(`{}`)),
			agenttest.TextResponse("there are two"),
		), ch)

		status, _ := post(ch, prompt("t1", "list it"))
		Expect(status).To(Equal(http.StatusOK))

		status, body := reopen(cfg, ch, "t1")
		Expect(status).To(Equal(http.StatusOK))
		Expect(body).To(ContainSubstring(`data: {"type":"data-user-message","id":"1","data":{"text":"list it"}}`))
		Expect(body).To(ContainSubstring(`data: {"type":"tool-input-available","toolCallId":"c1","toolName":"list","input":{},"dynamic":true}`))
		Expect(body).To(ContainSubstring(`data: {"type":"tool-output-available","toolCallId":"c1","output":"`))
		Expect(body).To(ContainSubstring(`"delta":"there are two"`), "the assistant turn is written whole rather than in fragments")
		Expect(body).To(ContainSubstring(`data: {"type":"finish","finishReason":"stop"}`))
	})

	// Nothing journals the question, so what comes back is the channel rebuilding it from
	// the call the run left unanswered and the tool that runs it. The card is one tool
	// part, as the live turn's was, and it carries the command path rather than the name
	// the model called the tool by.
	It("Should come back on the approval card it was left on", func() {
		ch := servedChannel(cfg, store)
		serveAll(cfg, store, agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("c1", "stream_rm", json.RawMessage(`{}`)),
			agenttest.TextResponse("everything is gone"),
		), ch)

		status, _ := post(ch, prompt("t2", "wipe it"))
		Expect(status).To(Equal(http.StatusOK))

		status, body := reopen(cfg, ch, "t2")
		Expect(status).To(Equal(http.StatusOK))
		Expect(body).To(Equal(sse(
			`{"type":"start","messageId":"MID"}`,
			`{"type":"data-user-message","id":"1","data":{"text":"wipe it"}}`,
			`{"type":"tool-input-available","toolCallId":"c1","toolName":"stream rm","input":{"command":"stream rm","tag":"ai:confirm"},"dynamic":true}`,
			`{"type":"tool-approval-request","approvalId":"approval-c1","toolCallId":"c1","reason":"stream rm"}`,
			`{"type":"finish","finishReason":"tool-calls"}`,
			`[DONE]`,
		)))
	})

	// The turn that failed sent the run's error, so a page picking the conversation out
	// of the rail reads what failed rather than a finish reason with nothing behind it.
	It("Should come back carrying the error the conversation failed with", func() {
		provider := agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("never reached"))
		provider.SetCallFault(1, agenttest.Fault{Err: errors.New("the provider refused the request")})

		ch := servedChannel(cfg, store)
		serveAll(cfg, store, provider, ch)

		status, _ := post(ch, prompt("t6", "list it"))
		Expect(status).To(Equal(http.StatusOK))

		status, body := reopen(cfg, ch, "t6")
		Expect(status).To(Equal(http.StatusOK))
		Expect(body).To(ContainSubstring(`data: {"type":"error","errorText":"`))
		Expect(body).To(ContainSubstring("the provider refused the request"))
		Expect(body).To(ContainSubstring(`data: {"type":"finish","finishReason":"error"}`))
	})

	It("Should come back on the human-in-the-loop question it was left on", func() {
		ch := servedChannel(cfg, store)
		serveAll(cfg, store, agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("c1", "ask_human_select", json.RawMessage(`{"question":"which one?","options":["ORDERS","EVENTS"]}`)),
			agenttest.TextResponse("thanks"),
		), ch)

		status, _ := post(ch, prompt("t3", "ask me"))
		Expect(status).To(Equal(http.StatusOK))

		status, body := reopen(cfg, ch, "t3")
		Expect(status).To(Equal(http.StatusOK))
		Expect(body).To(ContainSubstring(`data: {"type":"data-question","id":"c1","data":{"toolUseId":"c1","kind":"select","question":"which one?","options":["ORDERS","EVENTS"]}}`))
		Expect(body).To(ContainSubstring(`data: {"type":"finish","finishReason":"tool-calls"}`))
	})

	// The turn that answers reaches the same conversation the open route read, which is
	// what makes opening one a way back into it rather than a view of it.
	It("Should answer the card it came back on and carry the conversation on", func() {
		ch := servedChannel(cfg, store)
		serveAll(cfg, store, agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("c1", "wipe", json.RawMessage(`{}`)),
			agenttest.TextResponse("everything is gone"),
		), ch)

		status, _ := post(ch, prompt("t4", "wipe it"))
		Expect(status).To(Equal(http.StatusOK))

		status, _ = reopen(cfg, ch, "t4")
		Expect(status).To(Equal(http.StatusOK))

		status, body := post(ch, `{"id":"t4","messages":[{"role":"user","parts":[{"type":"text","text":"wipe it"}]}],"fiskAnswer":{"toolUseId":"c1","kind":"approve","approval":"once"}}`)
		Expect(status).To(Equal(http.StatusOK))
		Expect(body).To(ContainSubstring(`data: {"type":"tool-output-available","toolCallId":"c1","output":"`), "the command ran")
		Expect(body).To(ContainSubstring(`data: {"type":"finish","finishReason":"stop"}`))
	})

	It("Should answer a conversation this channel does not hold with a 404", func() {
		ch := servedChannel(cfg, store)
		serveAll(cfg, store, agenttest.NewScriptedProvider(GinkgoTB()), ch)

		status, _ := reopen(cfg, ch, "never-held")
		Expect(status).To(Equal(http.StatusNotFound))
	})
})
