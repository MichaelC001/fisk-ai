//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package nats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"

	"github.com/choria-io/fisk-ai/internal/a2a"
	wire "github.com/choria-io/fisk-ai/internal/a2a/wire/v1"
)

// Transport carries a reply set in both directions and addresses a cancel to the one
// process running a task, so a program asserting for the capability at startup finds
// it here. It names its subjects too, on the optional describing interface.
var (
	_ a2a.StreamingTransport = (*Transport)(nil)
	_ a2a.DescribedTransport = (*Transport)(nil)
)

// DescribeTasks names the subject a task request arrives on and the pattern a cancel
// is addressed under, for the banner of a worker that serves tasks. The cancel line
// is a pattern rather than a subject because the request id is part of the address,
// and it is what an operator writes a publish permission against.
//
// The answers line appears only for a worker that asks its callers questions. On one
// that does not, the subject is a place to publish where nothing is listening.
func (t *Transport) DescribeTasks(identity string, elicits bool) []a2a.DescLine {
	lines := []a2a.DescLine{
		{Label: "Requests", Value: TaskSubject(identity)},
		{Label: "Cancels", Value: CancelSubject(identity, "*")},
	}

	if elicits {
		lines = append(lines, a2a.DescLine{Label: "Answers", Value: ElicitSubject(identity, "*")})
	}

	return lines
}

// Stream publishes body on the subject for op against agent, naming an inbox of its
// own, and returns a Reader over the messages that arrive there.
//
// Core request-reply already gives a caller a unique inbox, so the reply set needs no
// subject of its own. The correlation tag is read out of the body because the reader
// has to drop a message belonging to another set before deciding the set has ended;
// nothing else in the body is looked at, and the meaning of every message is still
// the engine's to dispatch.
func (t *Transport) Stream(ctx context.Context, agent string, op a2a.RouteHint, body []byte) (a2a.Reader, error) {
	subject, err := t.subject(agent, op)
	if err != nil {
		return nil, err
	}

	request, err := correlationOf(body)
	if err != nil {
		return nil, err
	}

	inbox := nats.NewInbox()

	sub, err := t.nc.SubscribeSync(inbox)
	if err != nil {
		return nil, fmt.Errorf("subscribing to the reply inbox: %w", err)
	}

	msg := nats.NewMsg(subject)
	msg.Reply = inbox
	msg.Data = body

	err = t.nc.PublishMsg(msg)
	if err != nil {
		_ = sub.Unsubscribe()
		return nil, fmt.Errorf("publishing to %q: %w", subject, err)
	}

	// Flushed so a connection that cannot carry the request reports it here, rather
	// than as a reader that waits for a reply set nobody was asked to produce.
	//
	// The flush is bounded by this transport's own timeout when the caller's context
	// carries no deadline, as a round trip is: a reply set may legitimately run for
	// hours, so a caller is not obliged to bound the reply set to send the request.
	flushCtx := ctx
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		flushCtx, cancel = context.WithTimeout(ctx, t.timeout)
		defer cancel()
	}

	err = t.nc.FlushWithContext(flushCtx)
	if err != nil {
		_ = sub.Unsubscribe()
		return nil, fmt.Errorf("publishing to %q: %w", subject, err)
	}

	return &reader{sub: sub, request: request, subject: subject}, nil
}

// WatchCancel subscribes to the subject cancels for the named request arrive on and
// routes them to h until the watch is released.
//
// It is a plain subscription rather than a micro endpoint because its lifetime is the
// task's and not the service's: micro can only remove a whole service, so staying
// inside it would mean one service per task, and a worker running fifty tasks would
// answer fifty-one pings and list fifty entries.
//
// The subscribe is what closes the race with the ack. A cancel can only arrive at
// nobody if it is sent before the subscription exists, and a caller has no reason to
// cancel a task it has not been told was accepted, so opening this before the ack
// removes the window rather than bounding it.
func (t *Transport) WatchCancel(request string, h a2a.Handler) (a2a.TaskWatch, error) {
	if !wire.ValidRequestID(request) {
		return nil, fmt.Errorf("%q is not a valid request id, so it cannot address a cancel", request)
	}

	sub, err := t.nc.Subscribe(CancelSubject(t.identity, request), func(m *nats.Msg) {
		h(context.Background(), a2a.Caller{}, m.Data, msgReplier{nc: t.nc, reply: m.Reply})
	})
	if err != nil {
		return nil, fmt.Errorf("subscribing to cancels for %q: %w", request, err)
	}

	return &taskWatch{sub: sub}, nil
}

// WatchElicitReplies subscribes to the subject the answers to this task's questions
// arrive on and routes them to h until the watch is released.
//
// It is a plain subscription for the same reason WatchCancel is: its lifetime is the
// task's rather than the service's. It is opened when the task is accepted, not when
// the first question is asked, so an answer cannot arrive before the subscription
// exists.
func (t *Transport) WatchElicitReplies(request string, h a2a.Handler) (a2a.TaskWatch, error) {
	if !wire.ValidRequestID(request) {
		return nil, fmt.Errorf("%q is not a valid request id, so it cannot address an answer", request)
	}

	sub, err := t.nc.Subscribe(ElicitSubject(t.identity, request), func(m *nats.Msg) {
		h(context.Background(), a2a.Caller{}, m.Data, msgReplier{nc: t.nc, reply: m.Reply})
	})
	if err != nil {
		return nil, fmt.Errorf("subscribing to answers for %q: %w", request, err)
	}

	return &taskWatch{sub: sub}, nil
}

// SendCancel delivers body to the process running the named task on agent and returns
// what it answered.
//
// It is sent as a request rather than published, because only the running task
// subscribes: no responders means the task is not running there, which separates a
// never-accepted, not-yet-started or already-finished task from one that received the
// cancel. A broadcast cancel cannot say that, since every instance receives it and
// almost all of them correctly do nothing.
func (t *Transport) SendCancel(ctx context.Context, agent, request string, body []byte) ([]byte, error) {
	if !wire.ValidRequestID(request) {
		return nil, fmt.Errorf("%q is not a valid request id, so it cannot address a cancel", request)
	}

	subject := CancelSubject(agent, request)

	msg, err := t.requestTask(ctx, subject, body, "cancel")
	if err != nil {
		return nil, err
	}

	return msg, nil
}

// SendElicitReply delivers an answer to the process running the named task and
// returns what it answered.
//
// It is a request rather than a publish for the reason a cancel is: only the running
// task subscribes, so no responders says the run ended without the answer, which the
// answering party needs to know. A published answer would be lost silently.
func (t *Transport) SendElicitReply(ctx context.Context, agent, request string, body []byte) ([]byte, error) {
	if !wire.ValidRequestID(request) {
		return nil, fmt.Errorf("%q is not a valid request id, so it cannot address an answer", request)
	}

	subject := ElicitSubject(agent, request)

	msg, err := t.requestTask(ctx, subject, body, "answer")
	if err != nil {
		return nil, err
	}

	return msg, nil
}

// requestTask sends body to one running task's own subject and returns what it
// answered. what names the message for the errors, since an operator reading one
// needs to know whether their cancel or their answer went nowhere.
func (t *Transport) requestTask(ctx context.Context, subject string, body []byte, what string) ([]byte, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, t.timeout)
		defer cancel()
	}

	msg, err := t.nc.RequestWithContext(ctx, subject, body)
	if err != nil {
		switch {
		case errors.Is(err, nats.ErrNoResponders):
			return nil, fmt.Errorf("%w on %q, so that task is not running there", a2a.ErrNoResponders, subject)

		case errors.Is(err, context.DeadlineExceeded):
			return nil, fmt.Errorf("%w: the %s on %q was not answered", a2a.ErrAgentUnavailable, what, subject)
		}

		return nil, fmt.Errorf("requesting %q: %w", subject, err)
	}

	return msg.Data, nil
}

// taskWatch holds one subscription a running task owns, for its cancels or for the
// answers to its questions. Close is idempotent because the task releases its watches
// on every ending and a task has several.
type taskWatch struct {
	sub  *nats.Subscription
	once sync.Once
	err  error
}

func (w *taskWatch) Close() error {
	w.once.Do(func() { w.err = w.sub.Unsubscribe() })

	return w.err
}

// reader yields the messages of one reply set off an inbox subscription. It is not
// safe for concurrent use, which matches a2a.Reader: a set is read in arrival order
// by whoever asked for it.
type reader struct {
	sub     *nats.Subscription
	request string
	subject string
	done    bool
	once    sync.Once
	closErr error
}

// Next returns the next message of the set, and io.EOF once the one marked final has
// been returned.
//
// It reports three things a single reply does not have to deal with: no responders,
// which the server answers with a 503 the subscription surfaces as an error and this
// states as ErrAgentUnavailable, the error the interface promises; a micro service
// error, which is how a size-capped or schema-invalid request is refused and which
// would otherwise reach the engine as an empty body that fails validation; and a
// message belonging to another reply set, which is dropped, since a terminal message
// from elsewhere would end this read early.
func (r *reader) Next(ctx context.Context) ([]byte, error) {
	if r.done {
		return nil, io.EOF
	}

	for {
		msg, err := r.sub.NextMsgWithContext(ctx)
		if errors.Is(err, nats.ErrNoResponders) {
			r.done = true
			return nil, fmt.Errorf("%w: nothing answers %q", a2a.ErrAgentUnavailable, r.subject)
		}
		if err != nil {
			return nil, err
		}

		code := msg.Header.Get(micro.ErrorCodeHeader)
		if code != "" {
			r.done = true
			return nil, fmt.Errorf("%q refused the request: %s: %s", r.subject, code, msg.Header.Get(micro.ErrorHeader))
		}

		correlation, err := correlationOf(msg.Data)
		if err != nil || correlation != r.request {
			continue
		}

		if msg.Header.Get(streamFinalHeader) != "" {
			r.done = true
		}

		return msg.Data, nil
	}
}

// Close unsubscribes the inbox. The producer is told nothing by it and keeps
// publishing until its task ends, which is what a cancel is for.
func (r *reader) Close() error {
	r.once.Do(func() { r.closErr = r.sub.Unsubscribe() })

	return r.closErr
}

// correlationOf reads the request tag a message carries. It is the one field this
// binding reads out of a body, and it reads it as an opaque correlation token: a
// message of a reply set is keyed on it, so one carrying a different value is
// misdelivered whatever put it there.
func correlationOf(body []byte) (string, error) {
	var probe struct {
		Request string `json:"request"`
	}

	err := json.Unmarshal(body, &probe)
	if err != nil {
		return "", fmt.Errorf("reading the request correlation tag: %w", err)
	}

	if probe.Request == "" {
		return "", fmt.Errorf("the message carries no request correlation tag")
	}

	return probe.Request, nil
}
