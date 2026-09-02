//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// Package nats is the NATS transport binding for the fisk-ai a2a protocol. It
// carries the request-reply messages the a2a engine uses to import and export
// tools between agents, discovery (an agent describes itself) and direct tool
// invocation, and the task flow, where one request produces a reply set the caller
// reads as it arrives and a cancel reaches the one process running that task.
//
// Subjects are routing only. The engine never infers a message's meaning from the
// subject it arrived on; every message is self-describing through its
// Header.Protocol id and is dispatched on that. Each subject does, however, carry a
// single, fixed message type: discovery, tool invocation and tasks ride separate
// subjects so a NATS permission grant can cover one without covering the others.
// That separation is an artifact of this binding and is not relied on by the
// protocol layer, which is why wrapping the same bodies in the Choria Protocol
// later (with its own subject space) needs no change to the engine.
//
// A reply set is framed by a header this binding sets on the last message, since
// every message of a set lands on the same inbox and only the body says which is
// terminal. Nothing else here reads a body, apart from the request correlation tag a
// reader needs to tell its own set from another's.
//
// The package registers itself as the "nats" a2a transport in init, so a program
// links it in with a blank import.
package nats

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"

	"github.com/choria-io/fisk-ai/internal/a2a"
	wire "github.com/choria-io/fisk-ai/internal/a2a/wire/v1"
	"github.com/choria-io/fisk-ai/internal/conns"
)

func init() {
	a2a.RegisterTransport("nats", newTransport)
}

// SubjectPrefix namespaces every a2a NATS subject. It sits inside the existing
// Choria subject space.
const SubjectPrefix = "choria.fisk-ai"

// defaultRequestTimeout bounds a discovery or tool request when neither the
// caller's context nor the transport configuration carries a deadline. It keeps a
// dead or wedged remote from hanging a run indefinitely.
const defaultRequestTimeout = 30 * time.Second

// streamFinalHeader marks the last message of a reply set. Every message of a set
// lands on the same reply inbox and only Header.Protocol separates a terminal one
// from an event, which this binding must not read, so it marks its own framing and
// the reader stops at the message that says it is last. Without it a reader has no
// way to end and every task runs to the caller's deadline.
const streamFinalHeader = "Fisk-AI-Stream-Final"

// microServiceVersion is the SemVer stamped on the micro service registration.
// This is service metadata only; the agent card carries the agent's real,
// free-form version, which the engine builds. A fixed value keeps the transport
// out of the agent-versioning business.
const microServiceVersion = "0.0.0"

// DiscoverySubject is the subject an agent with the given identity answers
// discovery requests on. It carries only discovery.request messages.
func DiscoverySubject(identity string) string {
	return fmt.Sprintf("%s.discovery.%s", SubjectPrefix, identity)
}

// ToolSubject is the subject an agent with the given identity answers tool
// invocation requests on. It carries only tool.request messages.
func ToolSubject(identity string) string {
	return fmt.Sprintf("%s.tool.%s", SubjectPrefix, identity)
}

// TaskSubject is the subject an agent with the given identity answers task requests
// on. It carries only request messages. A subject of its own is what lets a NATS
// permission grant tool calls without granting tasks.
func TaskSubject(identity string) string {
	return fmt.Sprintf("%s.task.%s", SubjectPrefix, identity)
}

// CancelSubject is the subject the one process running the named task listens on for
// cancels addressed to it. The request id is part of the address, so NATS routes a
// cancel to exactly the worker that can act on it and no sibling hears it at all.
//
// This checks the identity with wire.ValidIdentityName and returns "" for one it
// rejects, which nats.Conn refuses to
// subscribe to or publish on: a name carrying a '.' or a '>' would otherwise shape a
// subject somebody else listens on, and a caller that never checked the return would
// address it. The request tag is passed through, since a caller describing what it
// serves builds the pattern with "*" in that position; the transport calls
// wire.ValidRequestID on a real one before it subscribes or sends.
func CancelSubject(identity, request string) string {
	if !wire.ValidIdentityName(identity) {
		return ""
	}

	return fmt.Sprintf("%s.cancel.%s.%s", SubjectPrefix, identity, request)
}

// ElicitSubject is the subject the one process running the named task listens on for
// the answers to its questions. The request id is part of the address for the same
// reason a cancel's is, and an invalid identity returns "" on the same terms.
//
// It is a subject of its own rather than a second use of the cancel subject, so an
// operator can grant answering a question and stopping a task separately: an answer
// can approve a confirmation-gated command, where a cancel only ends the run.
func ElicitSubject(identity, request string) string {
	if !wire.ValidIdentityName(identity) {
		return ""
	}

	return fmt.Sprintf("%s.elicit.%s.%s", SubjectPrefix, identity, request)
}

// options is the nats-specific transport options block. It has no fields yet; it
// exists so the factory can reject any unknown option strictly, surfacing an
// operator's mistake at construction.
type options struct{}

// transport implements a2a.Transport over core NATS request-reply. It borrows the
// NATS connection from the shared Provider (it never closes it) and, on the serving
// side, registers a micro service whose endpoints map the discovery and tool
// subjects onto a2a handlers.
//
// It is reached through the registry alone, as a2a.Transport: a caller holding a
// *nats.Conn builds one by handing conns.New(conns.WithNats(nc)) to
// a2a.NewTransport, and every method here answers one of a2a's own interfaces, so
// naming the concrete type buys nothing.
type transport struct {
	nc       *nats.Conn
	identity string
	timeout  time.Duration
	svc      micro.Service
	log      *slog.Logger
	onFault  func(error)
	// stopping records that this process asked the service to stop, so the done
	// handler micro pushes from Stop is not reported as a fault. Every drain calls
	// Close, so without it a clean shutdown would look like the failure this reports.
	stopping atomic.Bool
}

// newTransport is the registered factory. It reads a *conns.Provider out of
// cfg.Resources and borrows its NATS connection, returning an error when the
// resources are of another type or carry no connection, so a misconfigured wiring
// fails loudly rather than dereferencing a nil connection.
func newTransport(cfg a2a.TransportConfig) (a2a.Transport, error) {
	p, ok := cfg.Resources.(*conns.Provider)
	if !ok {
		return nil, fmt.Errorf("a2a NATS transport requires a *conns.Provider in TransportConfig.Resources, got %T", cfg.Resources)
	}

	nc := p.Nats()
	if nc == nil {
		return nil, fmt.Errorf("a2a NATS transport requires a NATS connection but none was provisioned")
	}

	if len(cfg.Options) > 0 {
		dec := json.NewDecoder(bytes.NewReader(cfg.Options))
		dec.DisallowUnknownFields()
		var o options
		err := dec.Decode(&o)
		if err != nil {
			return nil, fmt.Errorf("decoding nats transport options: %w", err)
		}
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}

	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	return &transport{nc: nc, identity: cfg.Identity, timeout: timeout, log: log, onFault: cfg.OnFault}, nil
}

// fault reports that this transport has stopped serving for a reason nobody asked
// for. It logs whatever happens and calls back only when the stop was not this
// process's own doing, since micro pushes its done handler from Stop and every drain
// calls Stop.
func (t *transport) fault(err error) {
	if t.stopping.Load() {
		t.log.Debug("The a2a micro service stopped", "identity", t.identity)
		return
	}

	t.log.Error("The a2a micro service stopped answering", "identity", t.identity, "error", err)

	if t.onFault != nil {
		t.onFault(err)
	}
}

// RoundTrip publishes body on the subject for op against agent and returns the raw
// reply. A missing responder or an elapsed deadline is reported as
// a2a.ErrAgentUnavailable. The reply is returned undecoded; the engine size-caps
// and validates it.
//
// The two failures read differently. Nobody subscribed says the agent is not there;
// an elapsed wait says how long this agent waited and which bound it was, since an
// operator seeing only "unavailable" looks for a dead worker when the answer is that
// the peer needed longer than the caller allows.
func (t *transport) RoundTrip(ctx context.Context, agent string, op a2a.RouteHint, body []byte) ([]byte, error) {
	subject, err := t.subject(agent, op)
	if err != nil {
		return nil, err
	}

	waited := t.timeout
	if deadline, ok := ctx.Deadline(); ok {
		waited = time.Until(deadline)
	} else {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, t.timeout)
		defer cancel()
	}

	msg, err := t.nc.RequestWithContext(ctx, subject, body)
	if err != nil {
		switch {
		case errors.Is(err, nats.ErrNoResponders):
			return nil, fmt.Errorf("%w on %q", a2a.ErrNoResponders, subject)

		case errors.Is(err, context.DeadlineExceeded):
			return nil, fmt.Errorf("%w: no reply on %q within %s", a2a.ErrAgentUnavailable, subject, waited.Round(time.Second))
		}

		return nil, fmt.Errorf("requesting %q: %w", subject, err)
	}

	return msg.Data, nil
}

// Serve registers h as a micro endpoint on the subject for op under this
// transport's own identity. micro invokes the endpoint synchronously on its
// per-subscription goroutine, so h must answer and return rather than wait: a
// handler that blocks stops this subject being read, messages accumulate in the
// client's pending buffer, and past its limits micro stops the whole service, taking
// every path of this identity off the air. The engine refuses a call it has no slot
// for rather than holding the goroutine. The micro service is created on first use.
func (t *transport) Serve(op a2a.RouteHint, h a2a.Handler) error {
	return t.serve(op, func(ctx context.Context, caller a2a.Caller, body []byte, r replier) {
		h(ctx, caller, body, r)
	})
}

// ServeReplySet registers h the way Serve does and hands it the same replier, which
// carries the reply set methods as well. Every path of this transport can answer with
// a set, so the two differ only in what the handler is given.
func (t *transport) ServeReplySet(op a2a.RouteHint, h a2a.ReplySetHandler) error {
	return t.serve(op, func(ctx context.Context, caller a2a.Caller, body []byte, r replier) {
		h(ctx, caller, body, r)
	})
}

// serve is the registration both serve methods share, differing only in which
// interface they hand the replier over as.
func (t *transport) serve(op a2a.RouteHint, h func(context.Context, a2a.Caller, []byte, replier)) error {
	subject, err := t.subject(t.identity, op)
	if err != nil {
		return err
	}

	name, err := endpointName(op)
	if err != nil {
		return err
	}

	svc, err := t.service()
	if err != nil {
		return err
	}

	// The caller is zero because a micro request carries no identity this transport can
	// vouch for: NATS authenticates the connection to the server, not the publisher to
	// the subscriber, so subject permissions are the whole of the control here. The
	// request's own Header.Sender is a claim in the body and stays there.
	handler := micro.HandlerFunc(func(req micro.Request) {
		h(context.Background(), a2a.Caller{}, req.Data(), replier{nc: t.nc, req: req, reply: req.Reply()})
	})

	err = svc.AddEndpoint(name, handler, micro.WithEndpointSubject(subject))
	if err != nil {
		return fmt.Errorf("registering %s endpoint: %w", name, err)
	}

	return nil
}

// Describe returns the discovery and tool subjects the identity is reached on, for
// CLI display.
func (t *transport) Describe(identity string) []a2a.DescLine {
	return []a2a.DescLine{
		{Label: "Discovery", Value: DiscoverySubject(identity)},
		{Label: "Tools", Value: ToolSubject(identity)},
	}
}

// Close stops the micro service and its subscriptions. It leaves the borrowed NATS
// connection open; the Provider that established it owns its lifecycle.
//
// The stop is recorded before it is asked for, since micro pushes its done handler
// from Stop and this is the one stop that is not a fault.
func (t *transport) Close() error {
	t.stopping.Store(true)

	if t.svc != nil {
		return t.svc.Stop()
	}

	return nil
}

// service lazily registers the micro service that backs the serving endpoints. It
// is created only when the transport is used to serve, so a client-only transport
// registers nothing.
func (t *transport) service() (micro.Service, error) {
	if t.svc != nil {
		return t.svc, nil
	}

	// micro stops the whole service on any async error on one of its endpoint
	// subscriptions, a subscription that overflowed included, and it stops it on a
	// closed connection. Both handlers are wired because neither is recoverable here:
	// the service is gone by the time they run, and re-registering into the same
	// condition would loop.
	svc, err := micro.AddService(t.nc, micro.Config{
		Name:        t.identity,
		Version:     microServiceVersion,
		Description: "a2a protocol endpoint for this agent",
		QueueGroup:  t.identity,
		ErrorHandler: func(_ micro.Service, e *micro.NATSError) {
			t.fault(fmt.Errorf("a2a service error on %q: %s", e.Subject, e.Description))
		},
		DoneHandler: func(micro.Service) {
			t.fault(fmt.Errorf("the a2a service stopped"))
		},
	})
	if err != nil {
		return nil, fmt.Errorf("registering a2a service: %w", err)
	}

	t.svc = svc

	return svc, nil
}

// subject maps a route hint to the NATS subject for the given identity.
func (t *transport) subject(identity string, op a2a.RouteHint) (string, error) {
	switch op {
	case a2a.OpDiscovery:
		return DiscoverySubject(identity), nil
	case a2a.OpTool:
		return ToolSubject(identity), nil
	case a2a.OpTask:
		return TaskSubject(identity), nil
	default:
		return "", fmt.Errorf("unknown a2a route hint %d", op)
	}
}

// endpointName is the micro endpoint name for a route hint. It errors on a hint it
// does not know rather than falling back, because micro does not refuse a duplicate
// endpoint name: a forgotten case would register a second endpoint under an existing
// name and corrupt what INFO and STATS report, silently.
func endpointName(op a2a.RouteHint) (string, error) {
	switch op {
	case a2a.OpDiscovery:
		return "discovery", nil
	case a2a.OpTool:
		return "tool", nil
	case a2a.OpTask:
		return "task", nil
	default:
		return "", fmt.Errorf("unknown a2a route hint %d", op)
	}
}

// Subject implements a2a.SubjectNamer: the address a message travels on, for a log
// somebody reads when they want to go and watch the same traffic themselves.
//
// It names the per-task paths as well as the served ones, which subject() does not,
// because those are the ones an operator most wants to subscribe to and neither is
// registered as an endpoint.
func (t *transport) Subject(op a2a.RouteHint, agent, request string) string {
	switch op {
	case a2a.OpDiscovery:
		return DiscoverySubject(agent)
	case a2a.OpTool:
		return ToolSubject(agent)
	case a2a.OpTask:
		return TaskSubject(agent)
	case a2a.OpCancel:
		return CancelSubject(agent, request)
	case a2a.OpElicit:
		return ElicitSubject(agent, request)
	default:
		return ""
	}
}

// replier adapts a micro.Request's reply side to a2a.Replier and a2a.StreamReplier.
// It targets only the reply inbox micro supplied for the request and stays valid
// after the handler returns, so the engine's worker goroutine can answer.
//
// Respond stays micro's, so a failure to send the one message it is contracted for
// still reaches the service's NumErrors and LastError. Publish goes to the captured
// subject with the borrowed connection, which is what keeps a reply produced after
// the handler returned off the state micro's own dispatch is reading.
type replier struct {
	nc    *nats.Conn
	req   micro.Request
	reply string
}

var _ a2a.StreamReplier = replier{}

func (r replier) Respond(body []byte) error {
	return r.req.Respond(body)
}

func (r replier) Error(code, description string) error {
	return r.req.Error(code, description, nil)
}

func (r replier) Publish(body []byte, final bool) error {
	return publishReply(r.nc, r.reply, body, final)
}

// msgReplier is the reply side of a plain subscription, which the cancel watch is:
// there is no micro request to answer through, so every reply is a publish to the
// subject the message named.
type msgReplier struct {
	nc    *nats.Conn
	reply string
}

var _ a2a.StreamReplier = msgReplier{}

func (r msgReplier) Respond(body []byte) error {
	return publishReply(r.nc, r.reply, body, false)
}

// Error answers with micro's own error headers and an empty body, so a caller reads
// a refusal from a plain subscription the same way it reads one from an endpoint.
func (r msgReplier) Error(code, description string) error {
	if r.reply == "" {
		return fmt.Errorf("the request carried no reply subject")
	}

	msg := nats.NewMsg(r.reply)
	msg.Header.Set(micro.ErrorHeader, description)
	msg.Header.Set(micro.ErrorCodeHeader, code)

	return r.nc.PublishMsg(msg)
}

func (r msgReplier) Publish(body []byte, final bool) error {
	return publishReply(r.nc, r.reply, body, final)
}

// publishReply sends one message of a reply set to the inbox the request supplied,
// marking the last one so the reader knows where the set ends.
//
// A request that carried no reply subject cannot be streamed to, so this refuses
// rather than publishing into an empty subject: a caller that published without an
// inbox is told at the first message instead of receiving silence.
func publishReply(nc *nats.Conn, reply string, body []byte, final bool) error {
	if reply == "" {
		return fmt.Errorf("the request carried no reply subject to stream to")
	}

	msg := nats.NewMsg(reply)
	msg.Data = body
	if final {
		msg.Header.Set(streamFinalHeader, "true")
	}

	return nc.PublishMsg(msg)
}
