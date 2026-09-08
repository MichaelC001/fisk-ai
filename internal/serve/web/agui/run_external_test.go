//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agui_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/types"
	"github.com/choria-io/fisk"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/serve"
	"github.com/choria-io/fisk-ai/internal/serve/web"
	"github.com/choria-io/fisk-ai/internal/serve/web/agui"
)

// gatedApp is an application of three commands, two confirmation-gated so a run that
// calls one reaches the gate before the command executes, and one that runs on its own.
//
// The gated subcommand is the tool stream_rm running the command "stream rm", so a spec
// on the interrupt a conversation stopped at tells the model's name for the tool from
// the command path the operator approves.
func gatedApp() *fisk.Application {
	app := fisk.New("app", "an app")
	app.Command("wipe", "delete everything").Tag("ai:confirm")
	app.Command("list", "list everything")
	app.Command("stream", "manage streams").Command("rm", "delete a stream").Tag("ai:confirm")

	return app
}

// servedChannel is a web channel with this format mounted, sharing the store a spec
// passes. It resolves the agent's tools the way a configured channel does, since a
// conversation opened on an outstanding approval is rendered from the tool that runs
// the command.
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
		Formats:     []web.Mount{agui.Mount()},
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

// post sends one run input to the mounted format and returns the status with the
// response as the page read it.
func post(ch *web.Channel, body string) (int, string) {
	GinkgoHelper()

	resp := send(ch, body)
	defer resp.Body.Close()

	read, err := io.ReadAll(resp.Body)
	Expect(err).ToNot(HaveOccurred())

	return resp.StatusCode, string(read)
}

// send posts a body and returns the response for a spec that reads its headers.
func send(ch *web.Channel, body string) *http.Response {
	GinkgoHelper()

	req, err := http.NewRequest(http.MethodPost, "http://"+ch.Addr()+"/fisk/v1/agui", bytes.NewReader([]byte(body)))
	Expect(err).ToNot(HaveOccurred())
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	Expect(err).ToNot(HaveOccurred())

	return resp
}

// runs is the run input a client posts for a typed message, whose thread this format
// reads the newest user message out of and nothing else.
func runs(thread string, run string, text string) string {
	return `{"threadId":"` + thread + `","runId":"` + run + `","messages":[{"id":"m1","role":"user","content":"` + text + `"}]}`
}

// resumes is the run input a client posts to answer the interrupt the run before it
// ended on. It sends the thread again, as a client does, ending on the run that asked so
// that nothing in it is a message the person typed.
func resumes(thread string, run string, id string, payload string) string {
	return `{"threadId":"` + thread + `","runId":"` + run + `","messages":[` + stoppedThread + `],` +
		`"resume":[{"interruptId":"` + id + `","status":"resolved","payload":` + payload + `}]}`
}

// resumesSaying is that run input with a message the person typed while the card was up,
// appended to the thread as a client appends one.
func resumesSaying(thread string, run string, id string, payload string, text string) string {
	return `{"threadId":"` + thread + `","runId":"` + run + `","messages":[` + stoppedThread +
		`,{"id":"m3","role":"user","content":"` + text + `"}],` +
		`"resume":[{"interruptId":"` + id + `","status":"resolved","payload":` + payload + `}]}`
}

// stoppedThread is the thread a client holds when a run has stopped on an interrupt: the
// turn that asked, and what the assistant said before the question came.
const stoppedThread = `{"id":"m1","role":"user","content":"wipe it"},{"id":"m2","role":"assistant","content":"one moment"}`

// cancels is the run input a client posts when the person dismissed the card. It covers
// the interrupt without settling it, which the spec requires of a resume and which a
// plain prompt is not allowed to stand in for.
func cancels(thread string, run string, id string) string {
	return `{"threadId":"` + thread + `","runId":"` + run + `","messages":[` + stoppedThread + `],` +
		`"resume":[{"interruptId":"` + id + `","status":"` + string(types.ResumeStatusCancelled) + `"}]}`
}

// reopen is a page picking a conversation out of the rail: the session the thread ran
// in, read back through the format that renders it.
func reopen(cfg *config.Config, ch *web.Channel, thread string) (int, string) {
	GinkgoHelper()

	url := "http://" + ch.Addr() + "/fisk/v1/agui/sessions/" + web.SessionFor(cfg.Identity, thread)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	Expect(err).ToNot(HaveOccurred())

	resp, err := http.DefaultClient.Do(req)
	Expect(err).ToNot(HaveOccurred())
	defer resp.Body.Close()

	read, err := io.ReadAll(resp.Body)
	Expect(err).ToNot(HaveOccurred())

	return resp.StatusCode, string(read)
}

// These drive the whole path: a client posts to the hosted channel, the run executes
// against the scripted model and the fake application, and the bytes the client read
// are what is asserted on.
var _ = Describe("A served run", func() {
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

		resp := send(ch, runs("t1", "r1", "say hello"))
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		Expect(resp.Header.Get("Content-Type")).To(Equal("text/event-stream"))
		Expect(resp.Header.Get("X-Accel-Buffering")).To(Equal("no"))

		read, err := io.ReadAll(resp.Body)
		Expect(err).ToNot(HaveOccurred())
		Expect(resp.Body.Close()).To(Succeed())

		Expect(string(read)).To(Equal(frames("t1", "r1",
			`{"type":"TEXT_MESSAGE_START","messageId":"r1-1","role":"assistant"}`,
			`{"type":"TEXT_MESSAGE_CONTENT","messageId":"r1-1","delta":"hello "}`,
			`{"type":"TEXT_MESSAGE_CONTENT","messageId":"r1-1","delta":"there"}`,
			`{"type":"TEXT_MESSAGE_END","messageId":"r1-1"}`,
			`{"type":"RUN_FINISHED","threadId":"t1","runId":"r1","result":{"reason":"completed"},"outcome":{"type":"success"}}`,
		)))
	})

	// A stream this format built wrong is a client that stops rendering, so the SDK's
	// own decoder reads a whole turn back and its own sequence rules pass judgement on
	// the order.
	It("Should write a stream the protocol's own reader accepts", func() {
		ch := servedChannel(cfg, store)
		serveAll(cfg, store, agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("c1", "list", json.RawMessage(`{}`)),
			agenttest.ToolUseResponse("c2", "wipe", json.RawMessage(`{}`)),
			agenttest.TextResponse("done"),
		), ch)

		status, body := post(ch, runs("t9", "r1", "list it then wipe it"))
		Expect(status).To(Equal(http.StatusOK))
		accepted(body)

		status, body = post(ch, resumes("t9", "r2", "approve:c2", `{"approval":"once"}`))
		Expect(status).To(Equal(http.StatusOK))
		accepted(body)
	})

	It("Should render a call and the result that answered it", func() {
		ch := servedChannel(cfg, store)
		serveAll(cfg, store, agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("c1", "list", json.RawMessage(`{}`)),
			agenttest.TextResponse("done"),
		), ch)

		status, body := post(ch, runs("t1", "r1", "list it"))
		Expect(status).To(Equal(http.StatusOK))
		Expect(body).To(ContainSubstring(`data: {"type":"TOOL_CALL_START","toolCallId":"c1","toolCallName":"list"}`))
		Expect(body).To(ContainSubstring(`data: {"type":"TOOL_CALL_ARGS","toolCallId":"c1","delta":"{}"}`))
		Expect(body).To(ContainSubstring(`data: {"type":"TOOL_CALL_END","toolCallId":"c1"}`))
		Expect(body).To(ContainSubstring(`"type":"TOOL_CALL_RESULT","messageId":"r1-1","toolCallId":"c1","content":"`), "the call and its result share one toolCallId")
		Expect(body).To(ContainSubstring(`"outcome":{"type":"success"}`))
	})

	// The interrupt is what the run finishes with, and nothing about the command was
	// sent before it: the gate runs before the run traces the call, and the interrupt
	// carries the line the operator approves.
	It("Should end the run on the interrupt of a gated command", func() {
		ch := servedChannel(cfg, store)
		serveAll(cfg, store, agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("c1", "wipe", json.RawMessage(`{}`)),
			agenttest.TextResponse("everything is gone"),
		), ch)

		status, body := post(ch, runs("t2", "r1", "wipe it"))
		Expect(status).To(Equal(http.StatusOK))
		Expect(body).ToNot(ContainSubstring(`"type":"TOOL_CALL_START"`), "the call the gate stopped was never dispatched")
		Expect(body).To(ContainSubstring(`"outcome":{"type":"interrupt","interrupts":[{"id":"approve:c1","reason":"tool_call","message":"wipe","toolCallId":"c1"`))
		Expect(body).To(ContainSubstring(`"metadata":{"command":"wipe","kind":"approve","tag":"ai:confirm"}`))
	})

	// The turn that answers dispatches the call and sends it the way it sends any
	// other: AG-UI has a first-class interrupt, so there is nothing to suppress.
	It("Should run the command the answer allowed and send the call it dispatched", func() {
		first := servedChannel(cfg, store)
		second := servedChannel(cfg, store)
		serveAll(cfg, store, agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("c1", "wipe", json.RawMessage(`{}`)),
			agenttest.TextResponse("everything is gone"),
		), first, second)

		status, _ := post(first, runs("t3", "r1", "wipe it"))
		Expect(status).To(Equal(http.StatusOK))

		// The answer goes to a second channel on the same store, which is what a second
		// process behind a load balancer is.
		status, body := post(second, resumes("t3", "r2", "approve:c1", `{"approval":"once"}`))
		Expect(status).To(Equal(http.StatusOK))
		Expect(body).To(ContainSubstring(`data: {"type":"TOOL_CALL_START","toolCallId":"c1","toolCallName":"wipe"}`), "the call the interrupt named was dispatched")
		Expect(body).To(ContainSubstring(`"type":"TOOL_CALL_RESULT","messageId":"r2-1","toolCallId":"c1"`), "the command ran")
		Expect(body).To(ContainSubstring(`"delta":"everything "`))
		Expect(body).To(ContainSubstring(`"outcome":{"type":"success"}`))
	})

	// The standing allow is the answer the AI SDK's own approval cannot carry, and here
	// it is one of the three the schema asks for: the second gated call runs without
	// the person being asked again.
	It("Should stop asking about a tool the client allowed for the conversation", func() {
		ch := servedChannel(cfg, store)
		serveAll(cfg, store, agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("c1", "wipe", json.RawMessage(`{}`)),
			agenttest.ToolUseResponse("c2", "wipe", json.RawMessage(`{}`)),
			agenttest.TextResponse("both are gone"),
		), ch)

		status, _ := post(ch, runs("t8", "r1", "wipe it twice"))
		Expect(status).To(Equal(http.StatusOK))

		status, body := post(ch, resumes("t8", "r2", "approve:c1", `{"approval":"always"}`))
		Expect(status).To(Equal(http.StatusOK))
		Expect(body).To(ContainSubstring(`data: {"type":"TOOL_CALL_START","toolCallId":"c2","toolCallName":"wipe"}`), "the second call was dispatched rather than asked about")
		Expect(body).ToNot(ContainSubstring(`"type":"interrupt"`))
		Expect(body).To(ContainSubstring(`"outcome":{"type":"success"}`))
	})

	It("Should refuse a command the client declined and tell the model why", func() {
		ch := servedChannel(cfg, store)
		serveAll(cfg, store, agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("c1", "wipe", json.RawMessage(`{}`)),
			agenttest.TextResponse("left alone then"),
		), ch)

		status, _ := post(ch, runs("t5", "r1", "wipe it"))
		Expect(status).To(Equal(http.StatusOK))

		status, body := post(ch, resumes("t5", "r2", "approve:c1", `{"approval":"no"}`))
		Expect(status).To(Equal(http.StatusOK))
		Expect(body).ToNot(ContainSubstring(`"type":"TOOL_CALL_RESULT"`), "the command did not run")
		Expect(body).To(ContainSubstring(`"delta":"left "`), "the model was told the command was refused and answered")
	})

	// A client that renders its own approval control sends the boolean AG-UI
	// recommends, which is what makes a frontend nobody here wrote work against this.
	It("Should take an approval a client answered with the recommended boolean", func() {
		ch := servedChannel(cfg, store)
		serveAll(cfg, store, agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("c1", "wipe", json.RawMessage(`{}`)),
			agenttest.TextResponse("everything is gone"),
		), ch)

		status, _ := post(ch, runs("t4", "r1", "wipe it"))
		Expect(status).To(Equal(http.StatusOK))

		status, body := post(ch, resumes("t4", "r2", "approve:c1", `{"approved":true}`))
		Expect(status).To(Equal(http.StatusOK))
		Expect(body).To(ContainSubstring(`"type":"TOOL_CALL_RESULT","messageId":"r2-1","toolCallId":"c1"`), "the command ran")
		Expect(body).To(ContainSubstring(`"outcome":{"type":"success"}`))
	})

	// A person who dismisses the card decided nothing. The conversation resumes, the
	// gate asks again, and the client holds a thread it can still answer; refusing the
	// request would leave the interrupt standing with no move left that the interrupt
	// spec allows.
	It("Should put the question again on a resume the client canceled", func() {
		ch := servedChannel(cfg, store)
		serveAll(cfg, store, agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("c1", "wipe", json.RawMessage(`{}`)),
			agenttest.TextResponse("everything is gone"),
		), ch)

		status, _ := post(ch, runs("t11", "r1", "wipe it"))
		Expect(status).To(Equal(http.StatusOK))

		status, body := post(ch, cancels("t11", "r2", "approve:c1"))
		Expect(status).To(Equal(http.StatusOK))
		Expect(body).To(ContainSubstring(`"interrupts":[{"id":"approve:c1","reason":"tool_call","message":"wipe","toolCallId":"c1"`))
		Expect(body).ToNot(ContainSubstring(`"type":"TOOL_CALL_RESULT"`), "the command did not run")
		accepted(body)

		// The conversation is where it was, so the answer that follows still lands.
		status, body = post(ch, resumes("t11", "r3", "approve:c1", `{"approval":"once"}`))
		Expect(status).To(Equal(http.StatusOK))
		Expect(body).To(ContainSubstring(`"type":"TOOL_CALL_RESULT","messageId":"r3-1","toolCallId":"c1"`), "the command ran")
		Expect(body).To(ContainSubstring(`"outcome":{"type":"success"}`))
	})

	DescribeTable("A human-in-the-loop question",
		func(tool string, input string, id string, interrupt string, payload string) {
			ch := servedChannel(cfg, store)
			serveAll(cfg, store, agenttest.NewScriptedProvider(GinkgoTB(),
				agenttest.ToolUseResponse("c1", tool, json.RawMessage(input)),
				agenttest.TextResponse("thanks"),
			), ch)

			status, body := post(ch, runs("t6", "r1", "ask me"))
			Expect(status).To(Equal(http.StatusOK))
			Expect(body).To(ContainSubstring(`data: {"type":"TOOL_CALL_START","toolCallId":"c1","toolCallName":"`+tool+`"}`), "the tool was dispatched and put the question itself")
			Expect(body).To(ContainSubstring(interrupt))

			// The tool ran on the turn that asked, so the client holds the call and
			// accumulates its argument fragments. Sending the redispatched call again
			// writes the question object into the thread twice.
			status, body = post(ch, resumes("t6", "r2", id, payload))
			Expect(status).To(Equal(http.StatusOK))
			Expect(body).ToNot(ContainSubstring(`"type":"TOOL_CALL_START"`), "the call the client already holds is not sent again")
			Expect(body).ToNot(ContainSubstring(`"type":"TOOL_CALL_ARGS"`), "its arguments are not written into the thread twice")
			Expect(body).To(ContainSubstring(`"type":"TOOL_CALL_RESULT","messageId":"r2-1","toolCallId":"c1"`), "the answer the tool returned is new")
			Expect(body).To(ContainSubstring(`"delta":"thanks"`), "the answer reached the tool and the run went on")
			Expect(body).To(ContainSubstring(`"outcome":{"type":"success"}`))
			accepted(body)
		},
		Entry("asking for a yes or no",
			"ask_human_confirm", `{"question":"shall I?"}`, "confirm:c1",
			`"interrupts":[{"id":"confirm:c1","reason":"confirmation","message":"shall I?","toolCallId":"c1"`,
			`{"confirmed":true}`),
		Entry("asking for one of a list",
			"ask_human_select", `{"question":"which one?","options":["ORDERS","EVENTS"]}`, "select:c1",
			`"interrupts":[{"id":"select:c1","reason":"input_required","message":"which one?","toolCallId":"c1"`,
			`{"index":1}`),
		Entry("asking for a value",
			"ask_human_input", `{"question":"which subject?","default":"orders"}`, "input:c1",
			`"interrupts":[{"id":"input:c1","reason":"input_required","message":"which subject?","toolCallId":"c1"`,
			`{"value":"orders.new"}`),
	)

	// A prompt sent while a question is outstanding is not delivered: the resume puts
	// the question again and the run suspends without reaching a boundary that takes a
	// user message.
	It("Should tell the client a message the conversation did not take", func() {
		ch := servedChannel(cfg, store)
		serveAll(cfg, store, agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("c1", "wipe", json.RawMessage(`{}`)),
			agenttest.TextResponse("everything is gone"),
		), ch)

		status, _ := post(ch, runs("t7", "r1", "wipe it"))
		Expect(status).To(Equal(http.StatusOK))

		status, body := post(ch, runs("t7", "r2", "did it work?"))
		Expect(status).To(Equal(http.StatusOK))
		Expect(body).To(ContainSubstring(`data: {"type":"CUSTOM","name":"fisk.prompt_not_taken"`))
		Expect(body).To(ContainSubstring(`"interrupts":[{"id":"approve:c1"`), "the question is put again")
	})

	// Somebody who answers the interrupt and types in the same breath sends one run input
	// carrying both. The answer runs the command, the turn it was part of finishes, and
	// the message is the turn after it.
	It("Should answer the interrupt and take the message sent with it", func() {
		ch := servedChannel(cfg, store)
		serveAll(cfg, store, agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("c1", "wipe", json.RawMessage(`{}`)),
			agenttest.TextResponse("everything is gone"),
			agenttest.TextResponse("there is nothing left to list"),
		), ch)

		status, _ := post(ch, runs("t12", "r1", "wipe it"))
		Expect(status).To(Equal(http.StatusOK))

		status, body := post(ch, resumesSaying("t12", "r2", "approve:c1", `{"approval":"once"}`, "and then list what is left"))
		Expect(status).To(Equal(http.StatusOK))
		Expect(body).To(ContainSubstring(`"type":"TOOL_CALL_RESULT","messageId":"r2-1","toolCallId":"c1"`), "the command ran")
		Expect(body).To(ContainSubstring(`"delta":"everything "`))
		Expect(body).To(ContainSubstring(`"delta":"there "`), "the message was delivered as the turn after the answered one")
		Expect(body).ToNot(ContainSubstring(`"name":"fisk.prompt_not_taken"`))
		Expect(body).To(ContainSubstring(`"outcome":{"type":"success"}`))
		accepted(body)
	})

	// The answered command is followed by a second gated one, so the run stops on that
	// interrupt without reaching a boundary that takes a user message. The message the
	// same run input carried is neither journaled nor answered, and the client is told so.
	It("Should tell the client a message sent with an answer the conversation did not take", func() {
		ch := servedChannel(cfg, store)
		serveAll(cfg, store, agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("c1", "wipe", json.RawMessage(`{}`)),
			agenttest.ToolUseResponse("c2", "wipe", json.RawMessage(`{}`)),
			agenttest.TextResponse("both are gone"),
		), ch)

		status, _ := post(ch, runs("t13", "r1", "wipe it twice"))
		Expect(status).To(Equal(http.StatusOK))

		status, body := post(ch, resumesSaying("t13", "r2", "approve:c1", `{"approval":"once"}`, "did it work?"))
		Expect(status).To(Equal(http.StatusOK))
		Expect(body).To(ContainSubstring(`"type":"TOOL_CALL_RESULT","messageId":"r2-1","toolCallId":"c1"`), "the answer ran the command it named")
		Expect(body).To(ContainSubstring(`data: {"type":"CUSTOM","name":"fisk.prompt_not_taken"`))
		Expect(body).To(ContainSubstring(`"interrupts":[{"id":"approve:c2"`), "the second command is asked about")
		accepted(body)
	})
})

// These drive the open route: a conversation is held by running turns against the
// hosted channel, and what a page picking it out of the rail reads back is what is
// asserted on.
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

		status, _ := post(ch, runs("t1", "r1", "list it"))
		Expect(status).To(Equal(http.StatusOK))

		status, body := reopen(cfg, ch, "t1")
		Expect(status).To(Equal(http.StatusOK))
		Expect(body).To(ContainSubstring(`"type":"MESSAGES_SNAPSHOT"`))
		Expect(body).To(ContainSubstring(`"role":"user","content":"list it"`))
		Expect(body).To(ContainSubstring(`"toolCalls":[{"id":"c1","type":"function","function":{"name":"list","arguments":"{}"}}]`))
		Expect(body).To(ContainSubstring(`"role":"tool","content":"`))
		Expect(body).To(ContainSubstring(`"role":"assistant","content":"there are two"`))
		Expect(body).To(ContainSubstring(`"outcome":{"type":"success"}`))
	})

	// Nothing journals the question, so what comes back is the channel rebuilding it
	// from the call the run left unanswered and the tool that runs it. It carries the
	// command path rather than the name the model called the tool by.
	It("Should come back on the interrupt it was left on", func() {
		ch := servedChannel(cfg, store)
		serveAll(cfg, store, agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("c1", "stream_rm", json.RawMessage(`{}`)),
			agenttest.TextResponse("everything is gone"),
		), ch)

		status, _ := post(ch, runs("t2", "r1", "wipe it"))
		Expect(status).To(Equal(http.StatusOK))

		status, body := reopen(cfg, ch, "t2")
		Expect(status).To(Equal(http.StatusOK))
		Expect(body).To(ContainSubstring(`"toolCalls":[{"id":"c1","type":"function","function":{"name":"stream_rm","arguments":"{}"}}]`))
		Expect(body).To(ContainSubstring(`"interrupts":[{"id":"approve:c1","reason":"tool_call","message":"stream rm","toolCallId":"c1"`))
		Expect(body).To(ContainSubstring(`"metadata":{"command":"stream rm","kind":"approve","tag":"ai:confirm"}`))
	})

	// The turn that failed sent the run's error, so a page picking the conversation out
	// of the rail reads what failed rather than an outcome with nothing behind it.
	It("Should come back carrying the error the conversation failed with", func() {
		provider := agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("never reached"))
		provider.SetCallFault(1, agenttest.Fault{Err: errors.New("the provider refused the request")})

		ch := servedChannel(cfg, store)
		serveAll(cfg, store, provider, ch)

		status, _ := post(ch, runs("t6", "r1", "list it"))
		Expect(status).To(Equal(http.StatusOK))

		status, body := reopen(cfg, ch, "t6")
		Expect(status).To(Equal(http.StatusOK))
		Expect(body).To(ContainSubstring(`"type":"RUN_ERROR"`))
		Expect(body).To(ContainSubstring("the provider refused the request"))
	})

	// The turn that answers reaches the same conversation the open route read, which is
	// what makes opening one a way back into it rather than a view of it.
	It("Should answer the interrupt it came back on and carry the conversation on", func() {
		ch := servedChannel(cfg, store)
		serveAll(cfg, store, agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("c1", "wipe", json.RawMessage(`{}`)),
			agenttest.TextResponse("everything is gone"),
		), ch)

		status, _ := post(ch, runs("t4", "r1", "wipe it"))
		Expect(status).To(Equal(http.StatusOK))

		status, _ = reopen(cfg, ch, "t4")
		Expect(status).To(Equal(http.StatusOK))

		status, body := post(ch, resumes("t4", "r2", "approve:c1", `{"approval":"once"}`))
		Expect(status).To(Equal(http.StatusOK))
		Expect(body).To(ContainSubstring(`"type":"TOOL_CALL_RESULT","messageId":"r2-1","toolCallId":"c1"`), "the command ran")
		Expect(body).To(ContainSubstring(`"outcome":{"type":"success"}`))
	})

	It("Should answer a conversation this channel does not hold with a 404", func() {
		ch := servedChannel(cfg, store)
		serveAll(cfg, store, agenttest.NewScriptedProvider(GinkgoTB()), ch)

		status, _ := reopen(cfg, ch, "never-held")
		Expect(status).To(Equal(http.StatusNotFound))
	})
})
