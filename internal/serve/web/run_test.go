//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// errBadBody is what a format refuses a request with in the specs that need one.
var errBadBody = errors.New("the body is not a turn")

// gatedApp is an application whose one command is confirmation-gated, so a run that
// calls it reaches the gate before the command executes.
func gatedApp() *fisk.Application {
	app := fisk.New("app", "an app")
	app.Command("wipe", "delete everything").Tag("ai:confirm")

	return app
}

// servedConfig is the configuration the runs below execute under, with the web block
// the channel is described by.
func servedConfig() *config.Config {
	GinkgoHelper()

	app := agenttest.NewFakeApp(GinkgoTB(), gatedApp())

	cfg, err := config.ParseConfigForMode([]byte(fmt.Sprintf(`
identity: agent1
application_path: %s
system_prompt: do the thing
llm:
  model: claude-sonnet-4-6
expose:
  agent:
    web:
      listen: 127.0.0.1:0
      origins:
        - %s
`, app.Path, testOrigin)), config.ModeServe)
	Expect(err).ToNot(HaveOccurred())

	return cfg
}

// servedChannel is a channel with the fake format mounted, sharing the store a spec
// passes so a second channel stands in for a second process.
func servedChannel(store runstate.Store, workers int) (*Channel, *fakeFormat) {
	GinkgoHelper()

	format := &fakeFormat{}

	opts := testOptions()
	opts.Sessions = store
	opts.Workers = workers
	opts.Formats = []Mount{{Path: "fake", Format: format}}

	return newTestChannel(opts), format
}

// serveAll hosts the channels on a server driven by the scripted provider, and stops
// it when the spec ends.
func serveAll(cfg *config.Config, store runstate.Store, provider llm.Provider, channels ...*Channel) {
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

// These are the whole path: a page posts to a hosted channel, the run executes against
// the scripted model and the fake application, and the lines the page reads are what is
// asserted on.
var _ = Describe("A served turn", func() {
	var store *agenttest.FakeSessionStore

	BeforeEach(func() {
		store = agenttest.NewFakeSessionStore(GinkgoTB())
	})

	It("Should open a conversation on a new thread and continue it on the next request", func() {
		cfg := servedConfig()
		ch, _ := servedChannel(store, 2)
		serveAll(cfg, store, agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.TextResponse("hello"),
			agenttest.TextResponse("again"),
		), ch)

		resp := send(ch, turnBody{Thread: "t1", Prompt: "hi there", Caller: "alice"}, map[string]string{"Origin": testOrigin})
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		Expect(resp.Header.Get("X-Fake-Format")).To(Equal("v1"), "the format wrote the head")
		Expect(resp.Header.Get("Access-Control-Allow-Origin")).To(Equal(testOrigin))
		Expect(resp.Body.Close()).To(Succeed())

		status, lines := post(ch, turnBody{Thread: "t1", Prompt: "and again"})
		Expect(status).To(Equal(http.StatusOK))
		Expect(lines[0]).To(Equal("open"))
		Expect(lines).To(ContainElement("starting resumed=true"), "the second request continues the conversation")
		Expect(lines).To(ContainElement(`message terminal=true "again"`))
		Expect(lines[len(lines)-1]).To(Equal("close reason=completed taken=true err=<nil>"))

		rs, err := store.Load(context.Background(), SessionFor("agent1", "t1"))
		Expect(err).ToNot(HaveOccurred())
		Expect(rs.Messages).To(HaveLen(4), "two user turns and two answers")
		Expect(rs.Caller).To(Equal("alice"), "the request's claim reached the journal")
	})

	It("Should stream fragments to a writer that asks for them", func() {
		cfg := servedConfig()
		ch, format := servedChannel(store, 2)
		format.streams = true
		serveAll(cfg, store, agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("hello")), ch)

		status, lines := post(ch, turnBody{Thread: "t1", Prompt: "hi"})
		Expect(status).To(Equal(http.StatusOK))
		Expect(lines).To(ContainElement(HavePrefix("delta 0 ")))
		Expect(lines).To(ContainElement(`message terminal=true "hello"`))
	})

	// The proof of the design: a gated command stops the run, the page is asked on the
	// response that ends it, and the answer on the next request resumes the run. The
	// next request goes to a second channel on the same store, which is what a second
	// process behind a load balancer is.
	It("Should end the turn on a question and answer it from a second process", func() {
		cfg := servedConfig()
		first, _ := servedChannel(store, 2)
		second, _ := servedChannel(store, 2)
		serveAll(cfg, store, agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("c1", "wipe", json.RawMessage(`{}`)),
			agenttest.TextResponse("everything is gone"),
		), first, second)

		status, lines := post(first, turnBody{Thread: "t2", Prompt: "wipe it"})
		Expect(status).To(Equal(http.StatusOK))
		Expect(lines).To(ContainElement("starting resumed=false"))
		Expect(lines).ToNot(ContainElement(HavePrefix("tool ")), "the gated command did not run")
		Expect(lines[len(lines)-2]).To(Equal("ask approve c1"))
		// A suspended run reports the abort beside its reason, so the line carries both.
		Expect(lines[len(lines)-1]).To(HavePrefix("close reason=suspended taken=true err=the operator did not answer"))

		status, lines = post(second, turnBody{Thread: "t2", Answer: &answerBody{ToolUse: "c1", Kind: "approve", Approval: int(toolkit.ConfirmOnce)}})
		Expect(status).To(Equal(http.StatusOK))
		Expect(lines).To(ContainElement("starting resumed=true"))
		Expect(lines).To(ContainElement("tool c1 wipe"), "the resume dispatched the call and the gate answered from the request")
		Expect(lines).To(ContainElement("result c1 error=false"))
		Expect(lines).To(ContainElement(`message terminal=true "everything is gone"`))
		Expect(lines).ToNot(ContainElement(HavePrefix("ask ")))
		Expect(lines[len(lines)-1]).To(Equal("close reason=completed taken=true err=<nil>"))
	})

	// A prompt sent while a question is outstanding is not delivered: the resume puts
	// the question again and the run suspends without reaching a boundary that takes a
	// user message. The page is told rather than the prompt being dropped in silence.
	It("Should report a prompt the conversation did not take", func() {
		cfg := servedConfig()
		ch, _ := servedChannel(store, 2)
		serveAll(cfg, store, agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("c1", "wipe", json.RawMessage(`{}`)),
			agenttest.TextResponse("everything is gone"),
		), ch)

		status, lines := post(ch, turnBody{Thread: "t3", Prompt: "wipe it"})
		Expect(status).To(Equal(http.StatusOK))
		Expect(lines).To(ContainElement("ask approve c1"))

		status, lines = post(ch, turnBody{Thread: "t3", Prompt: "did it work?"})
		Expect(status).To(Equal(http.StatusOK))
		Expect(lines).To(ContainElement("ask approve c1"), "the question is put again")
		Expect(lines[len(lines)-1]).To(HavePrefix("close reason=suspended taken=false err=the operator did not answer"))

		status, lines = post(ch, turnBody{Thread: "t3", Answer: &answerBody{ToolUse: "c1", Kind: "approve", Approval: int(toolkit.ConfirmOnce)}})
		Expect(status).To(Equal(http.StatusOK))
		Expect(lines[len(lines)-1]).To(Equal("close reason=completed taken=true err=<nil>"))
	})

	// Two turns on one thread would resume one journal at once. The claim record is the
	// first append of a resume and is written before any model call, so the loser fails
	// with nothing streamed and is answered with a status code rather than a 200 with an
	// empty body.
	It("Should refuse a second turn on a thread already running", func() {
		cfg := servedConfig()
		ch, _ := servedChannel(store, 2)

		provider := agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.TextResponse("hello"),
			agenttest.TextResponse("later"),
		)
		provider.SetCallFault(2, agenttest.Fault{Delay: 1500 * time.Millisecond})
		serveAll(cfg, store, provider, ch)

		status, _ := post(ch, turnBody{Thread: "t4", Prompt: "hi"})
		Expect(status).To(Equal(http.StatusOK))

		type answered struct {
			status int
			lines  []string
		}

		results := make(chan answered, 2)
		for range 2 {
			go func() {
				defer GinkgoRecover()

				status, lines := post(ch, turnBody{Thread: "t4", Prompt: "more"})
				results <- answered{status, lines}
			}()
		}

		var got []answered
		for range 2 {
			var a answered
			Eventually(results, 10*time.Second).Should(Receive(&a))
			got = append(got, a)
		}

		statuses := []int{got[0].status, got[1].status}
		Expect(statuses).To(ConsistOf(http.StatusOK, http.StatusConflict))

		for _, a := range got {
			if a.status == http.StatusConflict {
				Expect(a.lines).To(ConsistOf(busyRefusal))
			} else {
				Expect(a.lines[len(a.lines)-1]).To(Equal("close reason=completed taken=true err=<nil>"))
			}
		}
	})

	// A request above the worker count is refused rather than taken: the puller takes
	// work before it acquires a slot, so a taken request would wait with no response
	// head at all.
	It("Should refuse a request above the worker count", func() {
		cfg := servedConfig()
		ch, _ := servedChannel(store, 1)

		provider := agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("hello"))
		provider.SetCallFault(1, agenttest.Fault{Delay: 1500 * time.Millisecond})
		serveAll(cfg, store, provider, ch)

		first := make(chan int, 1)
		go func() {
			defer GinkgoRecover()

			status, _ := post(ch, turnBody{Thread: "t5", Prompt: "hi"})
			first <- status
		}()

		Eventually(func() int {
			ch.mu.Lock()
			defer ch.mu.Unlock()

			return ch.inFlight
		}).Should(Equal(1))

		resp := send(ch, turnBody{Thread: "t6", Prompt: "hi"}, nil)
		Expect(resp.StatusCode).To(Equal(http.StatusServiceUnavailable))
		Expect(resp.Header.Get("Retry-After")).To(Equal(retryAfter))
		Expect(resp.Body.Close()).To(Succeed())

		Eventually(first, 10*time.Second).Should(Receive(Equal(http.StatusOK)))
	})
})
