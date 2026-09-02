//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2atransport_test

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/choria-io/fisk-ai/internal/a2a"
)

// The whole of what a binding author must satisfy. A method added to any of the three
// stops this file building, which makes the example a measurement of the interfaces
// rather than a description of them.
var (
	_ a2a.Transport          = (*memTransport)(nil)
	_ a2a.StreamingTransport = (*memTransport)(nil)
	_ a2a.DescribedTransport = (*memTransport)(nil)
)

func init() {
	a2a.RegisterTransport("memory", newMemTransport)
}

// path is one agent's routing path, the key a served handler is registered under. A
// NATS binding uses a subject here and an HTTP one a route.
type path struct {
	identity string
	op       a2a.RouteHint
}

// bus is the substrate this binding carries messages over: a process-local registry of
// who serves which path and of the tasks currently listening for a cancel or an answer.
// A real binding has a broker or an HTTP server here.
type bus struct {
	mu       sync.Mutex
	handlers map[path]a2a.Handler
	sets     map[path]a2a.ReplySetHandler
	cancels  map[string]a2a.Handler
	answers  map[string]a2a.Handler
}

func newBus() *bus {
	return &bus{
		handlers: map[path]a2a.Handler{},
		sets:     map[path]a2a.ReplySetHandler{},
		cancels:  map[string]a2a.Handler{},
		answers:  map[string]a2a.Handler{},
	}
}

// memTransport carries a2a messages between agents in one process.
type memTransport struct {
	bus      *bus
	identity string
}

// newMemTransport is the registered factory. It reads its substrate out of
// cfg.Resources, which is where the transport interface leaves the question of what a
// binding connects to: this one wants a *bus, the NATS binding wants a *conns.Provider,
// and neither has to link the other's dependencies to say so.
func newMemTransport(cfg a2a.TransportConfig) (a2a.Transport, error) {
	b, ok := cfg.Resources.(*bus)
	if !ok {
		return nil, fmt.Errorf("the memory transport needs a *bus in TransportConfig.Resources, got %T", cfg.Resources)
	}

	return &memTransport{bus: b, identity: cfg.Identity}, nil
}

// RoundTrip delivers body to whoever serves the op path for agent and returns the one
// message they answered with.
func (t *memTransport) RoundTrip(ctx context.Context, agent string, op a2a.RouteHint, body []byte) ([]byte, error) {
	t.bus.mu.Lock()
	h, ok := t.bus.handlers[path{agent, op}]
	t.bus.mu.Unlock()

	if !ok {
		return nil, fmt.Errorf("%w: nobody serves %s", a2a.ErrAgentUnavailable, agent)
	}

	return deliver(ctx, h, body)
}

// Stream delivers body to the reply-set handler for the op path and returns a reader
// over the set it produces. The handler runs on a goroutine of its own, which is this
// binding's serving goroutine, so the reader is handed back before the answer arrives.
func (t *memTransport) Stream(_ context.Context, agent string, op a2a.RouteHint, body []byte) (a2a.Reader, error) {
	t.bus.mu.Lock()
	h, ok := t.bus.sets[path{agent, op}]
	t.bus.mu.Unlock()

	if !ok {
		return nil, fmt.Errorf("%w: nobody serves %s", a2a.ErrAgentUnavailable, agent)
	}

	set := newMemSet()
	go h(context.Background(), a2a.Caller{}, body, &setReplier{set: set})

	return &setReader{set: set}, nil
}

// Serve registers h for the op path of this transport's own identity.
func (t *memTransport) Serve(op a2a.RouteHint, h a2a.Handler) error {
	t.bus.mu.Lock()
	defer t.bus.mu.Unlock()

	t.bus.handlers[path{t.identity, op}] = h

	return nil
}

// ServeReplySet registers h for the op path, on the same terms as Serve. The handler
// is handed a a2a.StreamReplier rather than a a2a.Replier, so a set is what it answers
// with and nothing has to assert for the methods that send one.
func (t *memTransport) ServeReplySet(op a2a.RouteHint, h a2a.ReplySetHandler) error {
	t.bus.mu.Lock()
	defer t.bus.mu.Unlock()

	t.bus.sets[path{t.identity, op}] = h

	return nil
}

// WatchCancel routes cancels for one running task to h until the watch is released.
func (t *memTransport) WatchCancel(request string, h a2a.Handler) (a2a.TaskWatch, error) {
	return t.watch(t.bus.cancels, request, h)
}

// WatchElicitReplies routes the answers to one running task's questions to h, on the
// same terms as WatchCancel.
func (t *memTransport) WatchElicitReplies(request string, h a2a.Handler) (a2a.TaskWatch, error) {
	return t.watch(t.bus.answers, request, h)
}

// SendCancel delivers a cancel to the process running the named task.
func (t *memTransport) SendCancel(ctx context.Context, _, request string, body []byte) ([]byte, error) {
	return t.send(ctx, t.bus.cancels, request, body)
}

// SendElicitReply delivers an answer to a question the named task asked.
func (t *memTransport) SendElicitReply(ctx context.Context, _, request string, body []byte) ([]byte, error) {
	return t.send(ctx, t.bus.answers, request, body)
}

// Describe names where this identity is reached, for a banner. It is optional, and a
// binding with nothing worth showing implements neither method.
func (t *memTransport) Describe(identity string) []a2a.DescLine {
	return []a2a.DescLine{
		{Label: "Discovery", Value: "memory://" + identity + "/discovery"},
		{Label: "Tools", Value: "memory://" + identity + "/tools"},
	}
}

// DescribeTasks names where tasks reach this identity and where their cancels and
// answers are addressed.
func (t *memTransport) DescribeTasks(identity string, elicits bool) []a2a.DescLine {
	lines := []a2a.DescLine{
		{Label: "Requests", Value: "memory://" + identity + "/tasks"},
		{Label: "Cancels", Value: "memory://" + identity + "/cancel/*"},
	}

	if elicits {
		lines = append(lines, a2a.DescLine{Label: "Answers", Value: "memory://" + identity + "/answer/*"})
	}

	return lines
}

// Close takes this identity's paths off the bus. It leaves the bus itself alone, which
// the caller established and closes.
func (t *memTransport) Close() error {
	t.bus.mu.Lock()
	defer t.bus.mu.Unlock()

	for p := range t.bus.handlers {
		if p.identity == t.identity {
			delete(t.bus.handlers, p)
		}
	}
	for p := range t.bus.sets {
		if p.identity == t.identity {
			delete(t.bus.sets, p)
		}
	}

	return nil
}

// watch is the registration both per-task watches share.
func (t *memTransport) watch(into map[string]a2a.Handler, request string, h a2a.Handler) (a2a.TaskWatch, error) {
	t.bus.mu.Lock()
	defer t.bus.mu.Unlock()

	into[request] = h

	return &memWatch{bus: t.bus, from: into, request: request}, nil
}

// send is the delivery both per-task sends share. A request nobody is watching means
// the run has ended or is not here, which is what ErrAgentUnavailable says.
func (t *memTransport) send(ctx context.Context, from map[string]a2a.Handler, request string, body []byte) ([]byte, error) {
	t.bus.mu.Lock()
	h, ok := from[request]
	t.bus.mu.Unlock()

	if !ok {
		return nil, fmt.Errorf("%w: no run is listening for %s", a2a.ErrAgentUnavailable, request)
	}

	return deliver(ctx, h, body)
}

// deliver invokes a single-reply handler and returns what it answered.
func deliver(ctx context.Context, h a2a.Handler, body []byte) ([]byte, error) {
	reply := &directReplier{}
	h(ctx, a2a.Caller{}, body, reply)

	if reply.err != nil {
		return nil, reply.err
	}

	return reply.body, nil
}

// memWatch is one running task's claim on its cancels or its answers.
type memWatch struct {
	bus     *bus
	from    map[string]a2a.Handler
	request string
}

func (w *memWatch) Close() error {
	w.bus.mu.Lock()
	defer w.bus.mu.Unlock()

	delete(w.from, w.request)

	return nil
}

// directReplier is the reply side of a request answered with one message.
type directReplier struct {
	body []byte
	err  error
}

func (r *directReplier) Respond(body []byte) error { r.body = body; return nil }

func (r *directReplier) Error(code, description string) error {
	r.err = fmt.Errorf("%s: %s", code, description)

	return nil
}

// memSet is one reply set in flight, held by the handler sending it and by the caller
// reading it.
//
// Closing the reader tells the handler nothing, which is the contract a2a.Reader states:
// a run keeps publishing until it ends, and a cancel is how a caller says it has stopped
// caring. So a send after the reader has gone is dropped rather than parking the serving
// goroutine on a channel nobody drains.
type memSet struct {
	msgs chan []byte
	gone chan struct{}

	mu       sync.Mutex
	err      error
	endOnce  sync.Once
	oneClose sync.Once
}

func newMemSet() *memSet {
	return &memSet{msgs: make(chan []byte, 8), gone: make(chan struct{})}
}

// end closes the set, either on its final message or on a handler error.
func (s *memSet) end(err error) {
	s.endOnce.Do(func() {
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()

		close(s.msgs)
	})
}

// setReplier is the reply side of a request answered with a set: Respond carries the
// ack and Publish every message after it, with the final one closing the set.
type setReplier struct {
	set *memSet
}

func (r *setReplier) Respond(body []byte) error { return r.publish(body, false) }

func (r *setReplier) Publish(body []byte, final bool) error { return r.publish(body, final) }

// Error ends the set with a transport-level failure, which the reader returns instead of
// io.EOF so a caller learns why the answer never came.
func (r *setReplier) Error(code, description string) error {
	r.set.end(fmt.Errorf("%s: %s", code, description))

	return nil
}

func (r *setReplier) publish(body []byte, final bool) error {
	select {
	case r.set.msgs <- body:
	case <-r.set.gone:
		return nil
	}

	if final {
		r.set.end(nil)
	}

	return nil
}

// setReader yields the messages of one reply set in the order they were sent.
type setReader struct {
	set *memSet
}

func (r *setReader) Next(ctx context.Context) ([]byte, error) {
	select {
	case body, ok := <-r.set.msgs:
		if ok {
			return body, nil
		}

		r.set.mu.Lock()
		defer r.set.mu.Unlock()

		if r.set.err != nil {
			return nil, r.set.err
		}

		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *setReader) Close() error {
	r.set.oneClose.Do(func() { close(r.set.gone) })

	return nil
}
