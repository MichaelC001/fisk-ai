//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agui_test

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	aguievents "github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/events"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/serve/web"
	"github.com/choria-io/fisk-ai/internal/serve/web/agui"
)

func TestAGUI(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Serve/Web/AGUI")
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// sse is the body a run of events writes: one data line each, the way the protocol
// SDK's own frame writer lays them out.
func sse(events ...string) string {
	var out strings.Builder

	for _, e := range events {
		fmt.Fprintf(&out, "data: %s\n\n", e)
	}

	return out.String()
}

// started is the head of every response: the run frame the request named and the
// declaration of what this agent does with an interrupt.
func started(thread string, run string) []string {
	return []string{
		`{"type":"RUN_STARTED","threadId":"` + thread + `","runId":"` + run + `"}`,
		`{"type":"CUSTOM","name":"fisk.capabilities","value":{"humanInTheLoop":{"interrupts":true,"approveWithEdits":false}}}`,
	}
}

// frames is the events of one response with the head prepended, for a spec pinning a
// whole body.
func frames(thread string, run string, rest ...string) string {
	return sse(append(started(thread, run), rest...)...)
}

// decoded is one request through the format: the turn it asked for, the writer the
// response is written with, and the recorder that writer writes to.
type decoded struct {
	turn   web.Turn
	writer web.TurnWriter
	rec    *httptest.ResponseRecorder
}

// body is what the recorder holds.
func (d decoded) body() string { return d.rec.Body.String() }

// decode reads body through the format, failing the spec if the format refuses it.
func decode(body string) decoded {
	GinkgoHelper()

	out, err := decodeErr(body)
	Expect(err).ToNot(HaveOccurred())

	return out
}

// decodeErr is decode for the specs that assert on the refusal.
func decodeErr(body string) (decoded, error) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/fisk/v1/agui", strings.NewReader(body))

	turn, writer, err := agui.New().Decode(rec, req)

	return decoded{turn: turn, writer: writer, rec: rec}, err
}

// opened is one page opening a stored conversation: the writer the format replays into
// and the recorder it writes to.
type opened struct {
	writer web.TurnWriter
	rec    *httptest.ResponseRecorder
}

// body is what the recorder holds.
func (o opened) body() string { return o.rec.Body.String() }

// open is the writer the channel replays a stored conversation into, which is what the
// session open route asks the format for.
func open() opened {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/fisk/v1/agui/sessions/w-abc", nil)
	req.SetPathValue("id", "w-abc")

	return opened{writer: agui.New().Replayer(rec, req), rec: rec}
}

// accepted reads a response back through the protocol SDK's own decoder and checks the
// sequence it makes, which is what a client does with it. An event this format built
// wrong, or a message left open, fails here rather than in somebody's browser.
func accepted(body string) {
	GinkgoHelper()

	var read []aguievents.Event

	for _, frame := range strings.Split(body, "\n\n") {
		line, found := strings.CutPrefix(frame, "data: ")
		if !found {
			continue
		}

		event, err := aguievents.EventFromJSON([]byte(line))
		Expect(err).ToNot(HaveOccurred(), line)

		read = append(read, event)
	}

	Expect(read).ToNot(BeEmpty())
	Expect(aguievents.ValidateSequence(read)).To(Succeed())
}

// streamer is the half of the writer the runner asserts at runtime to stream fragments.
func streamer(w web.TurnWriter) agent.MessageStreamer {
	GinkgoHelper()

	s, ok := w.(agent.MessageStreamer)
	Expect(ok).To(BeTrue(), "a format that streams implements agent.MessageStreamer")

	return s
}
