//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/choria-io/fisk"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	fisk2 "github.com/choria-io/fisk-ai/internal/toolkit/fisk"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// fakeTransport is an a2a.Transport that records the handlers the Server registers
// so a test can drive them directly, without a wire. RoundTrip is unused here.
type fakeTransport struct {
	handlers map[RouteHint]Handler
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{handlers: map[RouteHint]Handler{}}
}

func (f *fakeTransport) RoundTrip(context.Context, string, RouteHint, []byte) ([]byte, error) {
	return nil, nil
}

// Stream is what makes this a ReplySetTransport, which serving tools requires. The
// tests drive the registered handlers directly, so nothing calls it.
func (f *fakeTransport) Stream(context.Context, string, RouteHint, []byte) (Reader, error) {
	return nil, nil
}

func (f *fakeTransport) Serve(op RouteHint, h Handler) error {
	f.handlers[op] = h
	return nil
}

func (f *fakeTransport) Describe(string) []DescLine { return nil }

func (f *fakeTransport) Close() error { return nil }

// fakeReplier captures the reply set a handler produces: the ack through Respond and
// everything after it through Publish.
type fakeReplier struct {
	mu        sync.Mutex
	body      []byte
	published [][]byte
	final     []byte
	alive     int
	code      string
	responded atomic.Bool
	errored   atomic.Bool
}

func (r *fakeReplier) Respond(body []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.body = body
	r.responded.Store(true)
	return nil
}

func (r *fakeReplier) Publish(body []byte, final bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.published = append(r.published, body)
	if final {
		r.final = body
	} else {
		r.alive++
	}
	return nil
}

func (r *fakeReplier) Error(code, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.code = code
	r.errored.Store(true)
	return nil
}

// terminal decodes the last message of the set, which is the tool's own answer.
func (r *fakeReplier) terminal() *ToolReply {
	GinkgoHelper()

	r.mu.Lock()
	defer r.mu.Unlock()

	Expect(r.final).ToNot(BeEmpty(), "the reply set carries no terminal message")

	var reply ToolReply
	Expect(json.Unmarshal(r.final, &reply)).To(Succeed())

	return &reply
}

// finished reports whether the terminal message has been sent, which is what a caller
// waits for rather than the ack.
func (r *fakeReplier) finished() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.final != nil
}

// keepalives counts the non-terminal messages published between the ack and the reply.
func (r *fakeReplier) keepalives() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.alive
}

// toolRequestBody builds a schema-valid tool request body for name.
func toolRequestBody(name string) []byte {
	GinkgoHelper()

	req := NewToolRequest(name, nil)
	stampRequest(context.Background(), &req.Header, "caller", "svc")
	data, err := json.Marshal(req)
	Expect(err).NotTo(HaveOccurred())

	return data
}

var _ = Describe("handleTool dispatch", func() {
	It("Should hard-deny a confirm-gated tool: absent from the card and not invokable", func() {
		app := fisk.New("app", "an app")
		app.Command("keep", "kept")
		app.Command("danger", "gated").Tag("ai:confirm")

		ft := newFakeTransport()
		srv, err := NewServer(ft, toolsFor(app), ServerOptions{Identity: "svc", LogOutput: io.Discard})
		Expect(err).NotTo(HaveOccurred())

		// The gated tool is not advertised in the card.
		Expect(srv.ExposedTools()).To(ConsistOf("keep"))

		// A direct request for it is refused in-band, never run: the ack says no and
		// the terminal reply says why.
		rep := &fakeReplier{}
		ft.handlers[OpTool](context.Background(), Caller{}, toolRequestBody("danger"), rep)
		Expect(rep.responded.Load()).To(BeTrue())

		var ack Ack
		Expect(json.Unmarshal(rep.body, &ack)).To(Succeed())
		Expect(ack.Accepted).To(BeFalse())

		reply := rep.terminal()
		Expect(reply.IsError).To(BeTrue())
		Expect(reply.Output).To(ContainSubstring("not available"))
	})
})

// servingApp builds a single-command tool whose command runs a stand-in script,
// so a served tool call actually executes.
func servingApp(name, body string) []toolkit.Tool {
	GinkgoHelper()

	app := fisk.New("app", "an app")
	app.Command(name, "a command")

	tools, err := fisk2.ApplicationTools(introspect(app))
	Expect(err).NotTo(HaveOccurred())

	path := filepath.Join(GinkgoT().TempDir(), "app")
	Expect(os.WriteFile(path, []byte(body), 0o700)).To(Succeed())
	for _, t := range tools {
		t.AppPath = path
	}

	return toolkit.Tools(tools)
}

var _ = Describe("Integration: a2a tool keepalives", func() {
	It("Should say it is still working while a tool runs, and close the set with the reply", func() {
		dir := GinkgoT().TempDir()
		gate := filepath.Join(dir, "gate")

		body := fmt.Sprintf("#!/bin/sh\nwhile [ ! -f %q ]; do sleep 0.02; done\necho done\n", gate)

		ft := newFakeTransport()
		_, err := NewServer(ft, servingApp("slow", body), ServerOptions{
			Identity:          "svc",
			LogOutput:         io.Discard,
			KeepaliveInterval: 20 * time.Millisecond,
		})
		Expect(err).NotTo(HaveOccurred())

		rep := &fakeReplier{}
		ft.handlers[OpTool](context.Background(), Caller{}, toolRequestBody("slow"), rep)

		// Accepted on the serving goroutine, before the tool has finished.
		Expect(rep.responded.Load()).To(BeTrue())
		Expect(rep.finished()).To(BeFalse())

		var ack Ack
		Expect(json.Unmarshal(rep.body, &ack)).To(Succeed())
		Expect(ack.Accepted).To(BeTrue())

		// Sent by the goroutine that owns the stream, so the keepalive is numbered
		// before the reply rather than racing it.
		Eventually(rep.keepalives).Should(BeNumerically(">=", 2))
		Expect(rep.finished()).To(BeFalse())

		Expect(os.WriteFile(gate, []byte("go"), 0o600)).To(Succeed())
		Eventually(rep.finished).Should(BeTrue())

		reply := rep.terminal()
		Expect(reply.IsError).To(BeFalse())
		Expect(reply.Output).To(ContainSubstring("done"))
	})
})

var _ = Describe("Integration: a2a capacity refusal", func() {
	It("Should refuse a second tool request while the first holds the only slot, and serve again once it frees", func() {
		dir := GinkgoT().TempDir()
		runs := filepath.Join(dir, "runs")
		gate := filepath.Join(dir, "gate")

		// The script records that it entered (append to runs) then blocks until the
		// gate file appears. A refused request never enters, so it never appends.
		body := fmt.Sprintf("#!/bin/sh\necho run >> %q\nwhile [ ! -f %q ]; do sleep 0.02; done\necho done\n", runs, gate)

		ft := newFakeTransport()
		// NewServer's side effect is what the test needs: it registers the tool
		// handler on ft and records the tool under byName.
		_, err := NewServer(ft, servingApp("block", body), ServerOptions{Identity: "svc", Concurrency: 1, LogOutput: io.Discard})
		Expect(err).NotTo(HaveOccurred())

		runLines := func() int {
			data, err := os.ReadFile(runs)
			if err != nil {
				return 0
			}
			n := 0
			for _, b := range data {
				if b == '\n' {
					n++
				}
			}
			return n
		}

		rep1 := &fakeReplier{}
		// The first request acquires the only slot and its worker starts; the call
		// itself returns once the worker is spawned.
		ft.handlers[OpTool](context.Background(), Caller{}, toolRequestBody("block"), rep1)
		Eventually(runLines).Should(Equal(1))

		// The second is refused whole on the serving goroutine, ack and terminal both,
		// so the call returns answered without the first having finished.
		rep2 := &fakeReplier{}
		ft.handlers[OpTool](context.Background(), Caller{}, toolRequestBody("block"), rep2)
		Expect(rep2.finished()).To(BeTrue())

		var refusedAck Ack
		Expect(json.Unmarshal(rep2.body, &refusedAck)).To(Succeed())
		Expect(refusedAck.Accepted).To(BeFalse())

		refused := rep2.terminal()
		Expect(refused.Code).To(Equal(CodeCapacity))
		Expect(refused.IsError).To(BeTrue())
		Expect(refused.Output).To(ContainSubstring("did not run"))
		Expect(refused.Output).To(ContainSubstring("maximum of 1"))

		// Nothing ran for it: the refusal is decided before the tool is reached, so a
		// caller that gave up leaves no command behind.
		Consistently(runLines, 200*time.Millisecond, 20*time.Millisecond).Should(Equal(1))

		// Releasing the first frees the slot, and the next request is served.
		Expect(os.WriteFile(gate, []byte("go"), 0o600)).To(Succeed())
		Eventually(rep1.finished).Should(BeTrue())

		// Retried until accepted, because the worker publishes its reply just before it
		// gives the slot back. An accepted call is acked inline and answered later, so
		// the terminal message is what says which happened.
		rep3 := &fakeReplier{}
		Eventually(func() bool {
			rep3 = &fakeReplier{}
			ft.handlers[OpTool](context.Background(), Caller{}, toolRequestBody("block"), rep3)

			return !rep3.finished()
		}).Should(BeTrue())

		Eventually(rep3.finished).Should(BeTrue())
		Expect(rep3.terminal().Code).To(BeEmpty())
		Eventually(runLines).Should(Equal(2))
	})
})
