//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"runtime"
	"time"

	"github.com/choria-io/fisk-ai/internal/telemetry"
	"github.com/choria-io/fisk-ai/internal/toolkit"
)

const (
	// DefaultCallTimeout bounds a single served tool call against a command that
	// never returns. It is exported because a program reporting what its server will
	// do has to be able to name the value nobody set, and a startup line saying the
	// bound is zero when it is thirty seconds is worse than saying nothing.
	DefaultCallTimeout = 30 * time.Second

	// KeepaliveInterval is how often a running tool call tells its caller it is still
	// working. It is protocol timing rather than policy, so it is a constant: three
	// keepalives fit inside the default served-call bound, and a caller waiting on
	// silence has a signal long before the shortest bound worth configuring.
	KeepaliveInterval = 10 * time.Second

	// PhaseRunningTool is the phase a served tool call's keepalive carries. A tool
	// call has one phase, since nothing between the request and the reply is visible
	// to the server beyond the tool still running.
	PhaseRunningTool = "running-tool"

	// maxDefaultConcurrency caps what DefaultConcurrency derives from the machine. A
	// served call is a command this agent runs for a peer, so the bound is how much
	// unauthenticated work the host accepts rather than how much of it a CPU can do,
	// and a large build host should not become a wide command runner for peers.
	maxDefaultConcurrency = 8
	// minDefaultConcurrency keeps a single-core machine able to answer more than one
	// call at a time, since a served call spends most of its life waiting on a child
	// process rather than on a CPU.
	minDefaultConcurrency = 2
)

// DefaultConcurrency bounds how many tool calls run at once when a configuration sets
// none. A caller has no iteration budget, so without a cap an open path could spawn
// unbounded concurrent commands.
//
// It scales with the machine and, on Go 1.25, with the cgroup, so a worker sized for
// the box it runs on needs no configuration. It is exported for the reason
// DefaultCallTimeout is: a program has to be able to report the value nobody set.
func DefaultConcurrency() int {
	return max(minDefaultConcurrency, min(runtime.GOMAXPROCS(0), maxDefaultConcurrency))
}

// toolNamePattern is the character set a tool name must match to be exposed. It
// mirrors the MCP server's rule: a name outside this set cannot be imported by a
// caller as a usable tool, so such tools are skipped rather than silently broken.
var toolNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// ServerOptions configures the a2a tool server.
type ServerOptions struct {
	// Identity is the served agent's identity; it is the Sender on replies and is
	// used by the transport to key the serving paths.
	Identity string
	// Version is reported in the agent card.
	Version string
	// Model is reported in the agent card as what this agent answers a prompt with.
	// This server runs no agent loop and calls no model, so it is told rather than
	// derived. Empty publishes no model, which is what an agent taking no prompts says.
	Model string
	// ConfirmTags are the operator-configured tags that, with the always-on
	// ai:confirm, gate a command behind approval. A served agent has no operator,
	// so commands carrying any of these are never exposed (hard-deny).
	ConfirmTags []string
	// Concurrency is the maximum number of tool calls run at once; <= 0 uses the
	// default.
	Concurrency int
	// CallTimeout bounds a single tool call; <= 0 uses the default.
	CallTimeout time.Duration
	// KeepaliveInterval is how often a running tool call tells its caller it is still
	// working; <= 0 uses KeepaliveInterval. It is here rather than in a configuration
	// because it is protocol timing an operator has nothing to decide with, and a test
	// that would otherwise wait a real interval needs it short.
	KeepaliveInterval time.Duration
	// LogOutput is the sink for the default Logger; nil means os.Stderr. It is
	// ignored when Logger is supplied.
	LogOutput io.Writer
	// Logger receives structured progress; nil builds a text logger over
	// LogOutput.
	Logger *slog.Logger
	// Telemetry, when non-nil, receives a span per served call and is put on the
	// context a tool runs under, so a served tool that opens spans of its own is not
	// silent.
	//
	// It is a field here where the client reads its Provider off the caller's context,
	// and the asymmetry is forced rather than chosen: a handler's context comes from
	// the transport, which supplies a background context carrying nothing. Removing it
	// would mean widening Handler so a constructor-supplied context reaches a call.
	//
	// It is also what the agent card reports about export, read off the resolved
	// Provider rather than off a configuration so that a veto or an endpoint that was
	// refused does not leave the card claiming an export that will not happen.
	Telemetry *telemetry.Provider

	// DiscoveryOnly registers the discovery route and not the tool route, for an
	// identity that answers what it is without serving tools to anybody.
	//
	// Discovery is one route on one micro service and an identity has one card, so
	// whoever answers discovery answers it for every endpoint of that identity. An
	// agent that only takes prompts still has to be discoverable, and it is this that
	// lets one be built for it without also putting an empty tool set on the network.
	DiscoveryOnly bool
}

func (o *ServerOptions) applyDefaults() {
	if o.Concurrency <= 0 {
		o.Concurrency = DefaultConcurrency()
	}
	if o.CallTimeout <= 0 {
		o.CallTimeout = DefaultCallTimeout
	}
	if o.KeepaliveInterval <= 0 {
		o.KeepaliveInterval = KeepaliveInterval
	}
	if o.LogOutput == nil {
		o.LogOutput = os.Stderr
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewTextHandler(o.LogOutput, nil))
	}
}

// Server exposes a set of local tools to remote agents over a Transport: it
// answers discovery with an agent card and runs tools on request. It is the
// producer side of the protocol. It owns all message validation and the
// concurrency bound, refusing a call it has no slot for; the Transport only carries
// bytes and keeps the discovery and tool paths separate.
type Server struct {
	opts      ServerOptions
	identity  string
	byName    map[string]toolkit.Tool
	card      AgentCard
	validator *Validator
	sem       chan struct{}
	transport Transport
}

// NewServer builds a Server over transport and registers its discovery and tool
// handlers. It accepts any toolkit.Tool, so a wrapped application's commands and
// the harness's own in-process tools are served through one path; what may be
// carried is each tool's own a2a exposure declaration rather than its Go type. The
// exposed set is tools minus those that do not declare a2a exposure, minus those
// gated behind operator confirmation (which a served agent cannot satisfy), and
// minus any tool whose name a caller could not use; skipped tools are logged. Use
// ai:deny to keep a command out of the served set entirely. The Transport is
// borrowed: Stop releases only the Transport, and the caller closes the Provider
// behind it.
func NewServer(transport Transport, tools []toolkit.Tool, opts ServerOptions) (*Server, error) {
	opts.applyDefaults()

	// A tool call is answered as a reply set, so a binding that cannot carry one cannot
	// serve tools at all. It is refused here rather than per call, so a program learns
	// it at startup rather than from the first peer.
	replySets, streams := transport.(ReplySetTransport)
	if !streams {
		return nil, fmt.Errorf("%w: serving tools needs a transport that carries a reply set", ErrStreamUnsupported)
	}

	validator, err := NewValidator()
	if err != nil {
		return nil, fmt.Errorf("building message validator: %w", err)
	}

	s := &Server{
		opts:      opts,
		identity:  opts.Identity,
		byName:    make(map[string]toolkit.Tool, len(tools)),
		validator: validator,
		sem:       make(chan struct{}, opts.Concurrency),
		transport: transport,
	}

	exposed := s.selectExposed(tools)
	s.card = buildCard(opts, exposed)

	err = transport.Serve(OpDiscovery, s.handleDiscovery)
	if err != nil {
		return nil, fmt.Errorf("registering discovery handler: %w", err)
	}

	if opts.DiscoveryOnly {
		return s, nil
	}

	err = replySets.ServeReplySet(OpTool, s.handleTool)
	if err != nil {
		return nil, fmt.Errorf("registering tool handler: %w", err)
	}

	return s, nil
}

// ExposedTools returns the names of the tools the server exposes, in card order.
func (s *Server) ExposedTools() []string {
	names := make([]string, len(s.card.Tools))
	for i, t := range s.card.Tools {
		names[i] = t.Name
	}

	return names
}

// Describe returns the transport-neutral lines describing how this server is
// reached, for display. A transport that does not implement DescribedTransport has
// no addresses to name, and the answer is empty.
func (s *Server) Describe() []DescLine {
	described, ok := s.transport.(DescribedTransport)
	if !ok {
		return nil
	}

	return described.Describe(s.identity)
}

// Stop releases the transport's resources. It does not close the shared
// connection Provider, which the caller owns and closes.
func (s *Server) Stop() error {
	return s.transport.Close()
}

// selectExposed filters tools to those safe to serve and records them by name for
// invocation. A tool that does not declare a2a exposure is dropped, which is the
// ceiling every other rule sits under; confirm-gated tools have no operator to
// approve them on a served agent and are dropped; a tool with a name a caller could
// not use is dropped; a tool that advertises no description is dropped, since a
// remote agent importing it would reject it as giving the model nothing to decide
// on. Each drop is logged with its reason.
//
// Confirmability is an optional capability, so a tool that does not implement it
// cannot be asked whether it needs approval. That is treated as a refusal rather
// than as "no gate needed": an exposed tool must be able to answer, or the absence
// of a gate would be indistinguishable from not having one.
func (s *Server) selectExposed(tools []toolkit.Tool) []toolkit.Tool {
	var exposed []toolkit.Tool
	for _, t := range tools {
		confirmable, canAnswerConfirm := t.(toolkit.Confirmable)

		switch {
		case !t.A2AExposable():
			s.opts.Logger.Warn("Skipping tool: it does not declare a2a exposure", "tool", t.Name())
		case !canAnswerConfirm:
			s.opts.Logger.Warn("Skipping tool: it cannot report whether it is confirmation-gated, so it cannot be served", "tool", t.Name())
		case confirmable.NeedsConfirm(s.opts.ConfirmTags):
			s.opts.Logger.Warn("Skipping tool: confirmation-gated commands are not served over a2a (no operator to approve); use ai:deny to suppress this", "tool", t.Name())
		case !toolNamePattern.MatchString(t.Name()):
			s.opts.Logger.Warn("Skipping tool: not a valid a2a tool name", "tool", t.Name())
		case t.ModelDescription() == "":
			s.opts.Logger.Warn("Skipping tool: served tools must advertise a description; remote agents will not import a description-less tool", "tool", t.Name())
		default:
			// The tool is served whatever its tags say; a reserved tag that does nothing
			// looks exactly like one that works, and the behavior this agent advertises to
			// its peers is built from the same tags.
			unknown, conflicting := toolkit.TagIssues(t)
			if len(unknown) > 0 {
				s.opts.Logger.Warn("Tool carries unknown reserved tags: the ai: prefix is reserved and these do nothing", "tool", t.Name(), "tags", unknown)
			}
			if len(conflicting) > 0 {
				s.opts.Logger.Warn("Tool carries contradictory behavior tags: the more dangerous reading is advertised", "tool", t.Name(), "tags", conflicting)
			}

			exposed = append(exposed, t)
			s.byName[t.Name()] = t
		}
	}

	return exposed
}

// handleDiscovery answers a discovery request with the agent card. The discovery
// path carries only discovery requests, so any other message is rejected.
func (s *Server) handleDiscovery(_ context.Context, _ Caller, body []byte, reply Replier) {
	msg, err := s.inbound(body, DiscoveryRequestProtocol)
	if err != nil {
		_ = reply.Error("400", err.Error())
		return
	}
	dr := msg.(*DiscoveryRequest)

	out := &DiscoveryReply{AgentCard: s.card}
	out.Protocol = DiscoveryReplyProtocol
	StampReply(&out.Header, &dr.Header, s.identity)

	s.respond(reply, out)
}

// handleTool runs the requested tool and answers with a tool reply. A failed, denied
// or refused call is reported in-band on the reply (IsError set), never as a transport
// error. The tool path carries only tool requests, so any other message is rejected.
//
// A call arriving with every slot in use is refused rather than queued. The acquire
// runs on the transport's serving goroutine, so waiting for a slot would stop this
// path reading its subject: requests would pile up in the transport's own buffers
// where nothing measures them, and past those buffers' limits a binding may take the
// whole service off the air. A caller is waiting on the other end, so being told at
// once that this agent is at capacity is worth more than a place in a queue it cannot
// see.
func (s *Server) handleTool(ctx context.Context, caller Caller, body []byte, reply StreamReplier) {
	msg, err := s.inbound(body, ToolRequestProtocol)
	if err != nil {
		_ = reply.Error("400", err.Error())
		return
	}
	tr := msg.(*ToolRequest)

	// Two names for who is calling, kept apart. The sender is the body's own claim and
	// is worth logging because it is what a caller meant to say; the caller is what the
	// transport will vouch for. Merging them would let an unverified claim be read as an
	// established identity.
	sender := senderName(&tr.Header)
	log := s.opts.Logger.With("sender", sender, "caller", caller.Name, "caller_verified", caller.Verified)

	tool, ok := s.byName[tr.Name]

	// The caller's trace is joined before the span opens, so a served call sits under
	// the invocation that made it. The name is supplied only when it resolved: a name
	// that did not is peer-controlled input and goes no further than an attribute.
	info := telemetry.ServedToolInfo{
		Identity:       s.identity,
		Request:        tr.Header.Request,
		Caller:         caller.Name,
		CallerVerified: caller.Verified,
		Sender:         tr.Header.Sender.Name,
	}
	if ok {
		info.Name = tool.Name()
	} else {
		info.RequestedName = tr.Name
	}

	ctx = telemetry.ContextWithRemoteTrace(ctx, telemetry.TraceContext{TraceParent: tr.Header.TraceParent})
	ctx, span := s.opts.Telemetry.StartServedTool(ctx, info)

	// Every answer travels as a reply set, refusals included, so a caller reads one
	// shape whatever happened. The stream is built before the first decision because
	// each of them answers through it.
	stream := NewReplyStream(reply, &tr.Header, s.identity)

	if !ok {
		log.Warn("Rejecting unknown tool call", "tool", tr.Name)
		s.refuse(stream, log, fmt.Sprintf("tool %q is not available", tr.Name), "")
		span.Finish(telemetry.ServedToolOutcome{Outcome: telemetry.ToolOutcomeUnknownTool, Failed: true})
		return
	}

	// Refused after the tool is resolved, so an unknown tool is still named as one
	// rather than reported as capacity, which would tell a caller to retry a call that
	// can never succeed.
	select {
	case s.sem <- struct{}{}:
	default:
		log.Warn("Refusing tool call: every slot is in use", "tool", tool.Name(), "concurrency", s.opts.Concurrency)
		s.refuse(stream, log, capacityMessage(tool.Name(), s.identity, s.opts.Concurrency), CodeCapacity)
		span.Finish(telemetry.ServedToolOutcome{Outcome: telemetry.ToolOutcomeCapacity, Failed: true})
		return
	}

	// Accepted on the serving goroutine, so the caller knows it has a worker before
	// anything long starts and can tell this from a peer that never received it.
	err = stream.Ack(NewAck(true))
	if err != nil {
		<-s.sem
		log.Error("Acknowledging the tool call failed", "tool", tool.Name(), "error", err)
		span.Finish(telemetry.ServedToolOutcome{Outcome: telemetry.ToolOutcomeError, Failed: true})
		return
	}

	go func() {
		defer func() { <-s.sem }()

		// The Provider travels on the context the tool runs under, so a served tool that
		// opens spans of its own nests them under this call rather than dropping them.
		runCtx, cancel := context.WithTimeout(telemetry.ContextWithProvider(ctx, s.opts.Telemetry), s.opts.CallTimeout)
		defer cancel()

		log := log.With("tool", tool.Name(), "command", commandOf(tool))
		log.Info("Running tool call")

		start := time.Now()
		// The tool runs on a goroutine of its own so this one owns the stream: every
		// message of the set is sent from here, which is what keeps the sequence
		// gap-free without a lock.
		//
		// The served tool runs in the process working directory; a per-call scratch
		// directory for served tools is future server work, not this run path. There
		// is no operator behind a served call, so the deny prompter refuses any
		// question a tool asks rather than blocking the call forever.
		done := make(chan toolOutcome, 1)
		go func() {
			result, err := tool.Execute(runCtx, tr.Input, toolkit.ExecDeps{Prompter: toolkit.DefaultDenyPrompter()})
			done <- toolOutcome{result: result, err: err}
		}()

		res := s.awaitTool(done, stream, log)
		duration := time.Since(start)

		switch {
		case res.err != nil:
			log.Error("Tool call failed", "duration", duration, "error", res.err)
		case res.result != nil && res.result.Exec != nil:
			log.Info("Tool call completed", "duration", duration, "exit_code", res.result.Exec.ExitCode, "truncated", res.result.Exec.Truncated)
		default:
			log.Info("Tool call completed", "duration", duration)
		}

		out := resultToToolResult(res.result, res.err)
		err := stream.ToolReply(&ToolReply{ToolResult: *out})
		if err != nil {
			log.Error("Sending the tool reply failed", "error", err)
		}

		// Ended after the reply rather than after the tool: a reply that failed to send
		// is a peer that got nothing, and a green span here under a red one on the
		// caller would describe the wrong thing.
		span.Finish(servedOutcome(out))
	}()
}

// refuse answers a call that never reaches a tool: the ack says no and the terminal
// reply carries what a caller acts on. Both are needed because the ack ends nothing,
// and the code is on the reply because an ack has no room for one.
func (s *Server) refuse(stream *ReplyStream, log *slog.Logger, reason, code string) {
	refusal := NewAck(false)
	refusal.Reason = reason

	err := stream.Ack(refusal)
	if err != nil {
		log.Error("Refusing the tool call failed", "error", err)
		return
	}

	err = stream.ToolReply(&ToolReply{ToolResult: ToolResult{IsError: true, Output: reason}, Code: code})
	if err != nil {
		log.Error("Refusing the tool call failed", "error", err)
	}
}

// toolOutcome is what a served tool returned, carried from the goroutine that ran it
// to the one that owns the reply set.
type toolOutcome struct {
	result *toolkit.Outcome
	err    error
}

// awaitTool waits for the tool to finish, publishing a keepalive on the reply set
// every KeepaliveInterval so a caller can tell a call that is running from a peer that
// is gone. It runs on the goroutine that owns the stream, which is what keeps the
// set's numbering gap-free without a lock.
//
// A keepalive that fails to send stops the keepalives rather than repeating the
// failure every interval. Nothing else changes: the tool runs to completion and the
// terminal reply is still attempted, since a caller that is gone costs a publish into
// an inbox nobody reads while a command stopped halfway costs whatever it was doing.
func (s *Server) awaitTool(done <-chan toolOutcome, stream *ReplyStream, log *slog.Logger) toolOutcome {
	ticker := time.NewTicker(s.opts.KeepaliveInterval)
	defer ticker.Stop()

	alive := true

	for {
		select {
		case res := <-done:
			return res

		case <-ticker.C:
			if !alive {
				continue
			}

			err := stream.Event(NewBlock(StatusBlock{Phase: PhaseRunningTool}))
			if err != nil {
				log.Warn("Sending a keepalive failed; the call continues", "error", err)
				alive = false
			}
		}
	}
}

// capacityMessage is what a refused caller's model is told. It states that nothing ran
// and names the limit, and it offers no advice about retrying: nothing here counts
// repeats, there is no backoff anywhere on this path, and a peer that is saturated is
// the last thing a run should call again immediately.
func capacityMessage(tool, identity string, concurrency int) string {
	return fmt.Sprintf("tool %q on agent %q did not run: the agent is already running its maximum of %d concurrent tool calls", tool, identity, concurrency)
}

// servedOutcome maps a served call's reply onto the span outcome, using the same
// vocabulary a local tool call reports so the two are comparable on one key.
func servedOutcome(out *ToolResult) telemetry.ServedToolOutcome {
	o := telemetry.ServedToolOutcome{Outcome: telemetry.ToolOutcomeExecuted}

	if out.IsError {
		o.Outcome = telemetry.ToolOutcomeError
		o.Failed = true
	}
	// Set whenever a command ran, including a zero exit, for the reason the local tool
	// span sets it: reporting only failures makes a successful command look like a
	// built-in that never ran one.
	if out.Exec != nil {
		code := out.Exec.ExitCode
		o.ExitCode = &code
	}

	return o
}

// inbound size-caps, validates and decodes a request body and confirms it is the
// protocol the receiving path is contracted to carry. The size cap runs first,
// before any decode or allocation.
func (s *Server) inbound(body []byte, want string) (any, error) {
	if len(body) > MaxMessageSize {
		return nil, fmt.Errorf("request exceeds %d bytes", MaxMessageSize)
	}

	err := s.validator.Validate(body)
	if err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	return ExpectProtocol(body, want)
}

// respond marshals a reply and sends it through the request's Replier. The reply
// target is always the inbox the transport supplied, never a subject taken from
// the message body.
func (s *Server) respond(reply Replier, msg any) {
	data, err := json.Marshal(msg)
	if err != nil {
		s.opts.Logger.Warn("Marshaling reply failed", "error", err)
		_ = reply.Error("500", "marshaling reply")
		return
	}

	err = reply.Respond(data)
	if err != nil {
		s.opts.Logger.Warn("Sending reply failed", "error", err)
	}
}

// buildCard assembles an agent card from the exposed tools.
func buildCard(opts ServerOptions, tools []toolkit.Tool) AgentCard {
	card := AgentCard{
		Name:      opts.Identity,
		Version:   versionOrDev(opts.Version),
		Model:     opts.Model,
		Protocols: []string{ProtocolNamespace},
		// Read off the resolved provider rather than a configuration, so a veto or an
		// endpoint that was refused does not leave the card promising an export nobody
		// will make. Both are nil-safe.
		Telemetry:        opts.Telemetry.Enabled(),
		TelemetryContent: opts.Telemetry.CaptureEnabled(),
	}

	for _, t := range tools {
		card.Tools = append(card.Tools, ToolDescriptor{
			Name:        t.Name(),
			Description: t.ModelDescription(),
			InputSchema: marshalSchema(t.InputSchema()),
			Behavior:    toolBehavior(toolkit.BehaviorOf(t)),
		})
	}

	return card
}

// commandOf renders the bare command a call runs, for the server log. It is an
// optional capability: a tool that runs no command reports its name instead.
func commandOf(t toolkit.Tool) string {
	if c, ok := t.(toolkit.Confirmable); ok {
		return c.Command()
	}

	return t.Name()
}

// resultToToolResult maps a tool's outcome to the shared ToolResult. A harness
// failure (the tool could not run) sets IsError; a tool that ran, including a
// command that exited non-zero, is a successful result. A command's outcome carries
// its exec metadata so the caller can reconstruct the same CommandResult a local
// tool would have produced; an in-process tool has none, and its output travels
// verbatim so the importing agent hands the model exactly what the tool returned
// rather than a command envelope wrapped around it.
//
// A nil outcome with no error is a tool kind misbehaving rather than a reachable
// state, but this runs on a goroutine serving a remote caller, where a nil
// dereference would take the process down; it is reported as an error instead.
func resultToToolResult(result *toolkit.Outcome, err error) *ToolResult {
	switch {
	// A tool that answers later cannot be served: the answer would arrive against a
	// session this path does not have, long after the peer stopped waiting. The
	// caller is told the surface cannot carry the call rather than being handed the
	// tool's own account of what it is waiting for, which would read as a promise.
	case err != nil && errors.Is(err, toolkit.ErrDeferredResult):
		return &ToolResult{IsError: true, Output: toolkit.ServedDeferralRefusal}
	case err != nil:
		return &ToolResult{IsError: true, Output: err.Error()}
	case result == nil:
		return &ToolResult{IsError: true, Output: "tool returned no result"}
	case result.Exec == nil:
		return &ToolResult{Output: result.Output}
	}

	return &ToolResult{
		Output: result.Output,
		Exec: &ExecResult{
			Command:   result.Exec.Command,
			ExitCode:  result.Exec.ExitCode,
			Truncated: result.Exec.Truncated,
		},
	}
}

// marshalSchema renders a tool's input schema as raw JSON for a tool descriptor,
// falling back to an empty object schema when it is absent or cannot be marshaled.
func marshalSchema(schema map[string]any) json.RawMessage {
	if schema == nil {
		return json.RawMessage(`{"type":"object"}`)
	}

	data, err := json.Marshal(schema)
	if err != nil {
		return json.RawMessage(`{"type":"object"}`)
	}

	return data
}

// senderName returns the sender identity of a header for logging, or "unknown".
func senderName(h *Header) string {
	if h.Sender.Name == "" {
		return "unknown"
	}

	return h.Sender.Name
}

// versionOrDev returns the version, or "dev" when it is empty, so the agent card
// always carries a non-empty version. The card version is a free-form string.
func versionOrDev(version string) string {
	if version == "" {
		return "dev"
	}

	return version
}
