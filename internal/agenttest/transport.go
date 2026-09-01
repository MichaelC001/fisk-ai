//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agenttest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/choria-io/fisk-ai/internal/a2a"
)

// FakeTransport is an a2a.Transport for tests: it answers discovery with a fixed
// agent card and every direct tool call with a fixed reply, over no wire, so a run
// can import and invoke remote tools through an injected transport with no broker
// reachable. It records how many round trips it served, so a test can assert the run
// went through the injected transport (and, since the fake never dials, that Run did
// not dial either). It is one of the separate-package fakes proving each injectable
// interface can be implemented from outside its own package, and it is safe for the
// concurrent use runs sharing one transport make of it.
type FakeTransport struct {
	mu         sync.Mutex
	card       a2a.AgentCard
	toolOutput string
	toolIsErr  bool
	roundTrips int
	closeCalls int
	serveCalls int
}

// FakeTransport implements a2a.ReplySetTransport; the assertion is the
// separate-package interface audit, failing to compile if the interface stops being
// implementable from outside its own package. A tool call needs the reply set, since
// that is how a served call says it is still working.
var _ a2a.ReplySetTransport = (*FakeTransport)(nil)

// NewFakeTransport returns a transport that answers discovery with card. Tool calls
// answer with a success reply carrying "ok"; use SetToolReply to change it.
func NewFakeTransport(tb testing.TB, card a2a.AgentCard) *FakeTransport {
	tb.Helper()
	return BuildFakeTransport(card)
}

// BuildFakeTransport is NewFakeTransport without a testing.TB, for a func Example or any
// other caller outside a test. The transport answers from the card it was given and
// dials nothing, so Close is there to satisfy the interface.
func BuildFakeTransport(card a2a.AgentCard) *FakeTransport {
	return &FakeTransport{card: card, toolOutput: "ok"}
}

// SetToolReply sets what every direct tool call answers with.
func (t *FakeTransport) SetToolReply(output string, isError bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.toolOutput = output
	t.toolIsErr = isError
}

// RoundTrips reports how many requests the transport answered, across discovery and
// tool calls.
func (t *FakeTransport) RoundTrips() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.roundTrips
}

// Closed reports whether Close was called. A borrowed transport must not be closed
// by Run, so a test asserts this stays false.
func (t *FakeTransport) Closed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closeCalls > 0
}

// RoundTrip implements a2a.Transport by answering discovery and tool requests from
// its fixed card and reply, echoing the request's correlation tags so the reply
// passes the engine's schema validation.
func (t *FakeTransport) RoundTrip(_ context.Context, agent string, op a2a.RouteHint, body []byte) ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.roundTrips++

	var reqHdr a2a.Header
	err := json.Unmarshal(body, &reqHdr)
	if err != nil {
		return nil, fmt.Errorf("agenttest: FakeTransport could not decode request header: %w", err)
	}

	switch op {
	case a2a.OpDiscovery:
		reply := a2a.NewDiscoveryReply(t.card.Name, t.card.Version)
		reply.AgentCard = t.card
		t.stamp(&reply.Header, &reqHdr, agent)
		return json.Marshal(reply)
	default:
		return nil, fmt.Errorf("agenttest: FakeTransport got unexpected op %v", op)
	}
}

// Stream implements a2a.ReplySetTransport by answering a tool call the way a binding
// does: an ack, then the terminal tool reply. A real peer sends keepalives between the
// two while its tool runs; a fake answers at once and has none to send.
func (t *FakeTransport) Stream(_ context.Context, agent string, op a2a.RouteHint, body []byte) (a2a.Reader, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.roundTrips++

	if op != a2a.OpTool {
		return nil, fmt.Errorf("agenttest: FakeTransport got unexpected streaming op %v", op)
	}

	var reqHdr a2a.Header
	err := json.Unmarshal(body, &reqHdr)
	if err != nil {
		return nil, fmt.Errorf("agenttest: FakeTransport could not decode request header: %w", err)
	}

	ack := a2a.NewAck(true)
	t.stamp(&ack.Header, &reqHdr, agent)
	ack.Sequence = 1

	reply := a2a.NewToolReply(t.toolOutput, t.toolIsErr)
	t.stamp(&reply.Header, &reqHdr, agent)
	reply.Sequence = 2

	set := make([][]byte, 0, 2)
	for _, msg := range []any{ack, reply} {
		data, err := json.Marshal(msg)
		if err != nil {
			return nil, fmt.Errorf("agenttest: FakeTransport could not encode a reply: %w", err)
		}
		set = append(set, data)
	}

	return &fakeReader{msgs: set}, nil
}

// fakeReader yields a prepared reply set in order.
type fakeReader struct {
	msgs [][]byte
	next int
}

func (r *fakeReader) Next(context.Context) ([]byte, error) {
	if r.next >= len(r.msgs) {
		return nil, io.EOF
	}

	msg := r.msgs[r.next]
	r.next++

	return msg, nil
}

func (r *fakeReader) Close() error { return nil }

// Serve implements a2a.Transport. The fake is a client transport only; Run never
// serves through the injected transport, so a call here is counted for ServeCalls
// rather than answered.
func (t *FakeTransport) Serve(a2a.RouteHint, a2a.Handler) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.serveCalls++
	return nil
}

// ServeCalls reports how many times Serve was called. Run never serves through a
// borrowed transport, so a test asserts this stays zero.
func (t *FakeTransport) ServeCalls() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.serveCalls
}

// Describe implements a2a.Transport with no address lines.
func (t *FakeTransport) Describe(string) []a2a.DescLine { return nil }

// Close implements a2a.Transport. Run never closes a borrowed transport, so this is
// recorded for a test to assert it was not reached rather than releasing anything.
func (t *FakeTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closeCalls++
	return nil
}

// stamp fills a reply header so it echoes the request it answers and validates
// against the message schema: a fresh id, the request's correlation and conversation
// tags, this agent as sender, and the original sender as recipient.
func (t *FakeTransport) stamp(h *a2a.Header, req *a2a.Header, sender string) {
	h.ID = a2a.NewID()
	h.Request = req.Request
	h.Conversation = req.Conversation
	h.Time = time.Now().UTC()
	h.Sender = a2a.Identity{Name: sender}
	if req.Sender.Name != "" {
		h.Recipient = &a2a.Identity{Name: req.Sender.Name}
	}
}
