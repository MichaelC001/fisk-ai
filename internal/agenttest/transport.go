//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agenttest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/choria-io/fisk-ai/internal/a2a"
	wire "github.com/choria-io/fisk-ai/internal/a2a/wire/v1"
)

// FakeTransport is an a2a.Transport for tests: it answers discovery with a fixed
// agent card and every direct tool call with a fixed reply, over no wire, so a run
// can import and invoke remote tools through an injected transport with no broker
// reachable. It records how many round trips it served, so a test can assert the run
// went through the injected transport (and, since the fake never dials, that Run did
// not dial either). It is one of the separate-package fakes proving each injectable
// interface can be implemented from outside its own package, and it is safe for the
// concurrent use runs sharing one transport make of it.
//
// SetFaults makes a request fail with an a2a sentinel or take a stated time, by peer
// and by operation, so a spec drives what an agent does when a peer is missing, silent
// or slow without writing a transport of its own.
type FakeTransport struct {
	mu         sync.Mutex
	card       wire.AgentCard
	toolOutput string
	toolIsErr  bool
	faults     []TransportFault
	waiter     Waiter
	roundTrips int
	closeCalls int
	serveCalls int
}

// TransportFault is what a FakeTransport does to a request instead of answering it, or
// before it does.
type TransportFault struct {
	// Agent is the peer whose requests the fault applies to. An empty Agent applies it to every peer,
	// which is a run whose broker reaches nothing; naming one peer leaves the others
	// answering, which is a run that imported two agents and lost one of them.
	Agent string

	// Ops are the operations it applies to. An empty Ops applies it to all of them, and
	// naming a2a.OpTool alone leaves discovery answering, so a run imports the peer's
	// tools and fails when the model calls one.
	Ops []a2a.RouteHint

	// Err is what the request returns. Name a2a.ErrNoResponders for a peer nothing is
	// listening for, a2a.ErrAgentUnavailable for a peer that accepted the request and
	// never answered, or a2a.ErrToolImport for a reply the caller cannot use; the
	// transport returns the error unchanged, so errors.Is on the caller's side reaches
	// the sentinel a real transport reports. A fault with a nil Err answers from the
	// card and the tool reply, after Delay.
	Err error

	// Delay is how long the request takes before it answers or fails. The transport
	// passes it to the Waiter, which defaults to a timer that ctx cancels; a spec
	// asserting on a delay rather than serving it installs its own with SetWaiter.
	Delay time.Duration
}

// FakeTransport implements a2a.ReplySetTransport; the assertion is the
// separate-package interface audit, failing to compile if the interface stops being
// implementable from outside its own package. A tool call needs the reply set, since
// that is how a served call says it is still working.
var (
	_ a2a.ReplySetTransport  = (*FakeTransport)(nil)
	_ a2a.DescribedTransport = (*FakeTransport)(nil)
)

// NewFakeTransport returns a transport that answers discovery with card. Tool calls
// answer with a success reply carrying "ok"; use SetToolReply to change it.
func NewFakeTransport(tb testing.TB, card wire.AgentCard) *FakeTransport {
	tb.Helper()
	return BuildFakeTransport(card)
}

// BuildFakeTransport is NewFakeTransport without a testing.TB, for a func Example or any
// other caller outside a test. The transport answers from the card it was given and
// dials nothing, so Close is there to satisfy the interface.
func BuildFakeTransport(card wire.AgentCard) *FakeTransport {
	return &FakeTransport{card: card, toolOutput: "ok", waiter: wallWait}
}

// SetToolReply sets what every direct tool call answers with.
func (t *FakeTransport) SetToolReply(output string, isError bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.toolOutput = output
	t.toolIsErr = isError
}

// SetFaults replaces the faults the transport applies. A request takes the first fault
// whose Agent and Ops match it, so a spec orders the narrow ones ahead of the wide ones.
// Calling it with no faults restores a transport that answers everything.
func (t *FakeTransport) SetFaults(faults ...TransportFault) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.faults = slices.Clone(faults)
}

// SetWaiter takes over how a fault's delay passes. A nil w restores the timer the
// transport waits on by default.
func (t *FakeTransport) SetWaiter(w Waiter) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if w == nil {
		w = wallWait
	}

	t.waiter = w
}

// RoundTrips reports how many requests the transport was given, across discovery and
// tool calls. A request a fault fails is counted, and counted as it arrives rather than
// once its delay has passed, so this counter still tells a spec that made every peer
// unreachable that the run reached the injected transport.
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

// fakeRequest is one request as the transport found it: what it answers from, and the
// fault it matched.
type fakeRequest struct {
	card       wire.AgentCard
	toolOutput string
	toolIsErr  bool
	fault      TransportFault
	waiter     Waiter
}

// serve passes the fault's delay and returns the error it fails the request with. A
// waiter that ends the wait early returns its own error to the caller, so a caller whose
// deadline elapses during the delay gets the context error.
func (r fakeRequest) serve(ctx context.Context) error {
	if r.fault.Delay > 0 {
		err := r.waiter(ctx, r.fault.Delay)
		if err != nil {
			return err
		}
	}

	return r.fault.Err
}

// request counts a request and takes what answering it needs: the card and tool reply as
// they stand, and the fault it matches. The delay and the encoding happen outside the
// lock, so a delayed call does not hold up the calls a spec makes beside it.
func (t *FakeTransport) request(agent string, op a2a.RouteHint) fakeRequest {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.roundTrips++

	req := fakeRequest{
		card:       t.card,
		toolOutput: t.toolOutput,
		toolIsErr:  t.toolIsErr,
		waiter:     t.waiter,
	}

	for _, f := range t.faults {
		if f.Agent != "" && f.Agent != agent {
			continue
		}

		if len(f.Ops) > 0 && !slices.Contains(f.Ops, op) {
			continue
		}

		req.fault = f
		break
	}

	return req
}

// RoundTrip implements a2a.Transport by answering discovery and tool requests from
// its fixed card and reply, echoing the request's correlation tags so the reply
// passes the engine's schema validation. A fault matching the request fails it, or
// delays the answer, before the body is read.
func (t *FakeTransport) RoundTrip(ctx context.Context, agent string, op a2a.RouteHint, body []byte) ([]byte, error) {
	req := t.request(agent, op)

	err := req.serve(ctx)
	if err != nil {
		return nil, err
	}

	var reqHdr wire.Header
	err = json.Unmarshal(body, &reqHdr)
	if err != nil {
		return nil, fmt.Errorf("agenttest: FakeTransport could not decode request header: %w", err)
	}

	switch op {
	case a2a.OpDiscovery:
		reply := wire.NewDiscoveryReply(req.card.Name, req.card.Version)
		reply.AgentCard = req.card
		t.stamp(&reply.Header, &reqHdr, agent)
		return json.Marshal(reply)
	default:
		return nil, fmt.Errorf("agenttest: FakeTransport got unexpected op %v", op)
	}
}

// Stream implements a2a.ReplySetTransport by answering a tool call the way a binding
// does: an ack, then the terminal tool reply. A real peer sends keepalives between the
// two while its tool runs; a fake answers at once and has none to send. A fault
// matching the call fails it, or delays the whole set, before the reader is returned.
func (t *FakeTransport) Stream(ctx context.Context, agent string, op a2a.RouteHint, body []byte) (a2a.Reader, error) {
	req := t.request(agent, op)

	err := req.serve(ctx)
	if err != nil {
		return nil, err
	}

	if op != a2a.OpTool {
		return nil, fmt.Errorf("agenttest: FakeTransport got unexpected streaming op %v", op)
	}

	var reqHdr wire.Header
	err = json.Unmarshal(body, &reqHdr)
	if err != nil {
		return nil, fmt.Errorf("agenttest: FakeTransport could not decode request header: %w", err)
	}

	ack := wire.NewAck(true)
	t.stamp(&ack.Header, &reqHdr, agent)
	ack.Sequence = 1

	reply := wire.NewToolReply(req.toolOutput, req.toolIsErr)
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

// ServeReplySet implements a2a.ReplySetTransport on the same terms as Serve, and is
// counted with it: the fake answers no inbound request through either.
func (t *FakeTransport) ServeReplySet(a2a.RouteHint, a2a.ReplySetHandler) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.serveCalls++
	return nil
}

// ServeCalls reports how many times Serve or ServeReplySet was called. Run never
// serves through a borrowed transport, so a test asserts this stays zero.
func (t *FakeTransport) ServeCalls() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.serveCalls
}

// Describe implements a2a.DescribedTransport with no address lines, so a spec reading
// a banner built over this fake gets the endpoint's own rows and nothing else.
func (t *FakeTransport) Describe(string) []a2a.DescLine { return nil }

// DescribeTasks implements a2a.DescribedTransport with no address lines. The fake is
// a client transport and carries no task path.
func (t *FakeTransport) DescribeTasks(string, bool) []a2a.DescLine { return nil }

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
func (t *FakeTransport) stamp(h *wire.Header, req *wire.Header, sender string) {
	h.ID = wire.NewID()
	h.Request = req.Request
	h.Conversation = req.Conversation
	h.Time = time.Now().UTC()
	h.Sender = wire.Identity{Name: sender}
	if req.Sender.Name != "" {
		h.Recipient = &wire.Identity{Name: req.Sender.Name}
	}
}
