//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/choria-io/fisk-ai/internal/telemetry"
)

// Client performs a2a request-reply interactions over a Transport: discovering a
// remote agent and invoking its tools directly. It is the consumer side of the
// protocol, used to import remote tools. All message validation (outgoing request,
// incoming reply size cap and schema) lives here in the engine; the Transport only
// moves bytes.
type Client struct {
	transport Transport
	stream    StreamingTransport
	sender    string
	validator *Validator
}

// NewClient wraps a Transport as a Client. sender is this agent's identity, set as
// the Header.Sender on outgoing requests. The Transport is borrowed: the caller
// established it (and the Provider behind it) and closes them.
//
// Whether the binding can carry a reply set is decided here, once, so a client built
// on a transport that cannot knows it before a task is sent rather than one request
// at a time.
func NewClient(transport Transport, sender string) (*Client, error) {
	validator, err := NewValidator()
	if err != nil {
		return nil, fmt.Errorf("building message validator: %w", err)
	}

	stream, _ := transport.(StreamingTransport)

	return &Client{transport: transport, stream: stream, sender: sender, validator: validator}, nil
}

// CanStream reports whether the transport behind this client carries a reply set, so
// a caller can tell that a task is not available here before building one.
func (c *Client) CanStream() bool { return c.stream != nil }

// Discover asks the named agent to describe itself and returns its agent card.
// ErrAgentUnavailable is returned when no agent answers.
func (c *Client) Discover(ctx context.Context, agent string) (*AgentCard, error) {
	req := NewDiscoveryRequest()
	stampRequest(ctx, &req.Header, c.sender, agent)

	reply, err := c.roundTrip(ctx, agent, OpDiscovery, req, DiscoveryReplyProtocol)
	if err != nil {
		return nil, err
	}

	dr, ok := reply.(*DiscoveryReply)
	if !ok {
		return nil, fmt.Errorf("%w: discovery reply had unexpected type %T", ErrProtocolMismatch, reply)
	}

	return &dr.AgentCard, nil
}

// InvokeTool calls a single tool on the named agent and returns its reply. A
// failed or denied call is reported in-band on the ToolReply (IsError set), not
// as a Go error; a Go error means the call could not be made or answered.
//
// The hop is traced when the caller's context carries a telemetry provider, nesting
// under whatever span opened it. The request carries that span's trace context, so the
// peer's own span for the call joins this trace rather than starting one.
func (c *Client) InvokeTool(ctx context.Context, agent, tool string, input json.RawMessage) (reply *ToolReply, err error) {
	ctx, span := telemetry.ProviderFromContext(ctx).StartRemoteAgent(ctx, telemetry.RemoteAgentInfo{
		Agent: agent,
		Tool:  tool,
	})
	// Ended from one defer over named returns because the failure this reports is not
	// only the error: a tool that failed on the peer answers with IsError and a nil
	// error, and a span that ignored that would show a green remote call under the red
	// local one that wraps it.
	defer func() { span.Finish(remoteOutcome(reply, err)) }()

	req := NewToolRequest(tool, normalizeInput(input))
	stampRequest(ctx, &req.Header, c.sender, agent)

	raw, err := c.roundTrip(ctx, agent, OpTool, req, ToolReplyProtocol)
	if err != nil {
		return nil, err
	}

	tr, ok := raw.(*ToolReply)
	if !ok {
		return nil, fmt.Errorf("%w: tool reply had unexpected type %T", ErrProtocolMismatch, raw)
	}

	return tr, nil
}

// remoteOutcome classifies how a remote invocation ended, for the span.
//
// The class is named here rather than derived by the telemetry package, which imports
// nothing from this tree and so cannot recognize this package's sentinels. Nothing
// derived from the error text travels: these errors name hosts, subjects and reply
// fragments.
func remoteOutcome(reply *ToolReply, err error) telemetry.RemoteAgentOutcome {
	switch {
	case err == nil && reply != nil && reply.IsError:
		// The call was made and answered; the tool itself failed on the far side.
		return telemetry.RemoteAgentOutcome{Failed: true, Class: telemetry.ClassToolError}

	case err == nil:
		return telemetry.RemoteAgentOutcome{}

	case errors.Is(err, ErrAgentUnavailable):
		return telemetry.RemoteAgentOutcome{Failed: true, Class: telemetry.ClassRemoteUnavailable}
	}

	class, ok := telemetry.ClassifyContext(err)
	if ok {
		return telemetry.RemoteAgentOutcome{Failed: true, Class: class}
	}

	return telemetry.RemoteAgentOutcome{Failed: true, Class: telemetry.ClassOther}
}

// roundTrip validates and sends a request over the transport, then returns the
// reply decoded once it passes the size cap, the schema, and the expected protocol
// id. A missing responder or an elapsed deadline is surfaced by the transport as
// ErrAgentUnavailable; an unusable reply is ErrToolImport.
func (c *Client) roundTrip(ctx context.Context, agent string, op RouteHint, req any, wantReply string) (any, error) {
	data, err := c.marshalValid(req)
	if err != nil {
		return nil, err
	}

	reply, err := c.transport.RoundTrip(ctx, agent, op, data)
	if err != nil {
		return nil, err
	}

	if len(reply) > MaxMessageSize {
		return nil, fmt.Errorf("%w: reply exceeds %d bytes", ErrToolImport, MaxMessageSize)
	}

	err = c.validator.Validate(reply)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid reply: %w", ErrToolImport, err)
	}

	return ExpectProtocol(reply, wantReply)
}

// marshalValid encodes an outgoing message and holds it to the same schema a
// receiver will, so a message this agent could not have answered is refused here
// rather than arriving as a peer's validation failure.
func (c *Client) marshalValid(msg any) ([]byte, error) {
	data, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	err = c.validator.Validate(data)
	if err != nil {
		return nil, fmt.Errorf("invalid outgoing request: %w", err)
	}

	return data, nil
}

// normalizeInput drops an empty or explicit-null tool input. The tool.request
// schema requires input to be a JSON object when present, while the model may
// emit null or nothing for a no-argument tool; omitting it keeps such a request
// valid, and the server treats an absent input as an empty object.
func normalizeInput(input json.RawMessage) json.RawMessage {
	if len(input) == 0 || string(input) == "null" {
		return nil
	}

	return input
}
