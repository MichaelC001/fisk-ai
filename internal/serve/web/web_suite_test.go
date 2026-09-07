//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/toolkit"
)

func TestWeb(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Serve/Web")
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testOrigin is the page every spec's listener allows.
const testOrigin = "http://localhost:5173"

// testOptions are valid options a spec varies one field of. The port is zero so specs
// never collide on one.
func testOptions() Options {
	return Options{
		Listen:   "127.0.0.1:0",
		BasePath: "/fisk/v1",
		Origins:  []string{testOrigin},
		Identity: "agent1",
		Workers:  2,
		Sessions: agenttest.NewFakeSessionStore(GinkgoTB()),
		Logger:   quietLogger(),
	}
}

// newTestChannel builds a Channel over the options, failing the spec if it refuses, and
// closes it when the spec ends.
func newTestChannel(opts Options) *Channel {
	GinkgoHelper()

	ch, err := New(opts)
	Expect(err).ToNot(HaveOccurred())
	DeferCleanup(func() { Expect(ch.Close()).To(Succeed()) })

	return ch
}

// turnBody is the JSON the fake format reads a turn out of.
type turnBody struct {
	Thread string      `json:"thread"`
	Prompt string      `json:"prompt,omitempty"`
	Caller string      `json:"caller,omitempty"`
	Answer *answerBody `json:"answer,omitempty"`
}

type answerBody struct {
	ToolUse   string `json:"tool_use"`
	Kind      string `json:"kind"`
	Approval  int    `json:"approval,omitempty"`
	Confirmed bool   `json:"confirmed,omitempty"`
	Index     int    `json:"index,omitempty"`
	Value     string `json:"value,omitempty"`
}

// fakeFormat is a Format that reads turnBody and writes one line per event, so a spec
// asserts on what a page would have read without a real protocol in the way.
type fakeFormat struct {
	mu sync.Mutex

	// decodes counts the requests that reached Decode, which is how a spec proves a
	// refusal happened before the body was read.
	decodes int

	// decodeErr is what Decode fails with instead of reading the body.
	decodeErr error

	// streams says the writers this format hands out ask for fragments.
	streams bool
}

func (f *fakeFormat) Decode(w http.ResponseWriter, r *http.Request) (Turn, TurnWriter, error) {
	f.mu.Lock()
	f.decodes++
	err := f.decodeErr
	streams := f.streams
	f.mu.Unlock()

	if err != nil {
		return Turn{}, nil, err
	}

	var body turnBody

	derr := json.NewDecoder(r.Body).Decode(&body)
	if derr != nil {
		return Turn{}, nil, fmt.Errorf("reading the turn: %w", derr)
	}

	turn := Turn{ThreadID: body.Thread, Prompt: body.Prompt, Caller: body.Caller}
	if body.Answer != nil {
		turn.Answer = &Answer{
			ToolUseID: body.Answer.ToolUse,
			Kind:      QuestionKind(body.Answer.Kind),
			Approval:  toolkit.ConfirmChoice(body.Answer.Approval),
			Confirmed: body.Answer.Confirmed,
			Index:     body.Answer.Index,
			Value:     body.Answer.Value,
		}
	}

	return turn, &fakeWriter{w: w, streams: streams}, nil
}

func (f *fakeFormat) decoded() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.decodes
}

// fakeWriter writes one line per event, and the head on Open.
type fakeWriter struct {
	w       http.ResponseWriter
	opened  bool
	streams bool
}

func (w *fakeWriter) line(format string, args ...any) {
	fmt.Fprintf(w.w, format+"\n", args...)

	flusher, ok := w.w.(http.Flusher)
	if ok {
		flusher.Flush()
	}
}

func (w *fakeWriter) Open() {
	if w.opened {
		return
	}
	w.opened = true

	w.w.Header().Set("Content-Type", "text/plain")
	w.w.Header().Set("X-Fake-Format", "v1")
	w.w.WriteHeader(http.StatusOK)
	w.line("open")
}

func (w *fakeWriter) Starting(info agent.RunInfo) {
	w.line("starting resumed=%t", info.Resumed)
}

func (w *fakeWriter) Warn(warning agent.Warning)   { w.line("warn %s", warning.Kind) }
func (w *fakeWriter) LLMRequest(summary string)    { w.line("request") }
func (w *fakeWriter) ToolCall(t agent.ToolTrace)   { w.line("tool %s %s", t.ID, t.Name) }
func (w *fakeWriter) SessionRotated(prevID string) { w.line("rotated %s", prevID) }
func (w *fakeWriter) Panicked(any, []byte)         { w.line("panicked") }
func (w *fakeWriter) StreamDeltas() bool           { return w.streams }
func (w *fakeWriter) MessageDelta(d llm.Delta)     { w.line("delta %d %q", d.Index, d.Text) }
func (w *fakeWriter) Replay(*runstate.RunState)    { w.line("replay") }

func (w *fakeWriter) ToolResult(t agent.ToolResultTrace) {
	w.line("result %s error=%t", t.CallID, t.IsError)
}

func (w *fakeWriter) Message(resp llm.Response, terminal bool) {
	text := ""
	for _, block := range resp.Content {
		if block.Text != nil {
			text += block.Text.Text
		}
	}

	w.line("message terminal=%t %q", terminal, text)
}

func (w *fakeWriter) Ask(q Question) {
	w.line("ask %s %s", q.Kind, q.ToolUseID)
}

func (w *fakeWriter) Close(e Ending) {
	w.Open()
	w.line("close reason=%s taken=%t err=%v", e.Outcome.Reason, !e.PromptNotTaken, e.Outcome.Err)
}

// post sends one turn to a channel's fake route and returns the status and the body's
// lines, which is what a page reads.
func post(ch *Channel, body turnBody) (int, []string) {
	GinkgoHelper()

	resp := send(ch, body, nil)
	defer resp.Body.Close()

	read, err := io.ReadAll(resp.Body)
	Expect(err).ToNot(HaveOccurred())

	return resp.StatusCode, strings.Split(strings.TrimSpace(string(read)), "\n")
}

// send posts a turn with the headers a spec adds, returning the response for the
// caller to read.
func send(ch *Channel, body turnBody, headers map[string]string) *http.Response {
	GinkgoHelper()

	encoded, err := json.Marshal(body)
	Expect(err).ToNot(HaveOccurred())

	req, err := http.NewRequest(http.MethodPost, "http://"+ch.Addr()+"/fisk/v1/fake", bytes.NewReader(encoded))
	Expect(err).ToNot(HaveOccurred())

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	Expect(err).ToNot(HaveOccurred())

	return resp
}
