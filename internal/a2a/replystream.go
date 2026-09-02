//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	"encoding/json"
	"fmt"

	wire "github.com/choria-io/fisk-ai/internal/a2a/wire/v1"
)

// ReplyStream numbers and sends the messages of one task's reply set.
//
// It stamps Header.Sequence gap-free and monotonic from 1 across everything it
// sends, the ack included. The ack is sequence 1 and goes through Respond, and every
// message after it is published, so Replier's single-shot contract describes the one
// message it still sends. The numbering is per direction and per reply set: a cancel
// traveling the other way carries the same Header.Request and is stamped by the
// request path, which sets 0.
//
// The ack is sent at admission, on the intake goroutine, and the stream is then
// handed to the run, so it is owned by one goroutine at a time and is not safe for
// concurrent use. It is not used after a terminal message.
type ReplyStream struct {
	reply  StreamReplier
	req    wire.Header
	sender string
	seq    uint64
}

// NewReplyStream numbers a reply set answering req, sending through reply and
// stamping sender as the message sender. It is a value the caller owns rather than
// registered anywhere, so a run that ends takes its stream with it.
func NewReplyStream(reply StreamReplier, req *wire.Header, sender string) *ReplyStream {
	return &ReplyStream{reply: reply, req: *req, sender: sender}
}

// Ack accepts or refuses the request, carrying whatever the caller put on it: the
// reason a refusal gives, and the conversation token a follow-up turn is sent with. It
// is sequence 1 and is sent synchronously, while the handler is still on the serving
// goroutine, so the accept is what the transport measures and no reply is written from
// a worker the transport may be reading. It is refused after anything else has been
// sent, since it is the single message Respond is contracted for.
//
// It takes the message rather than its fields, as Result and Error do, so a surface
// that has something to say on an ack says it here rather than through a parameter
// every other caller passes empty.
func (s *ReplyStream) Ack(ack *wire.Ack) error {
	if s.seq != 0 {
		return fmt.Errorf("%w: the ack is the first message of a reply set", wire.ErrInvalidMessage)
	}

	ack.Protocol = wire.AckProtocol

	data, err := s.encode(&ack.Header, ack)
	if err != nil {
		return err
	}

	err = s.reply.Respond(data)
	if err != nil {
		return fmt.Errorf("sending the ack: %w", err)
	}

	s.seq++

	return nil
}

// Event publishes one content block of the run as it is produced.
func (s *ReplyStream) Event(block wire.Block) error {
	ev := wire.NewEvent(block)

	return s.send(&ev.Header, ev, false)
}

// Result publishes the terminal success message and ends the set. A failure to send
// it is returned rather than logged, since a caller left without a terminal message
// holds a stream that never ends.
func (s *ReplyStream) Result(res *wire.Result) error {
	res.Protocol = wire.ResultProtocol

	return s.send(&res.Header, res, true)
}

// Error publishes the terminal failure message and ends the set. It carries the same
// obligation as Result: the caller is waiting for one of the two.
func (s *ReplyStream) Error(msg *wire.ErrorMessage) error {
	msg.Protocol = wire.ErrorProtocol

	return s.send(&msg.Header, msg, true)
}

// ToolReply publishes the terminal answer to a tool call and ends the set. A tool
// call's reply set ends with the tool's own outcome rather than with a Result, which
// belongs to a run: the two sets share an ack and their events and differ in what
// closes them.
func (s *ReplyStream) ToolReply(reply *wire.ToolReply) error {
	reply.Protocol = wire.ToolReplyProtocol

	return s.send(&reply.Header, reply, true)
}

// Elicit publishes a question the run is putting to the caller. It is not terminal:
// the run continues, and the answer arrives on the task's own inbound path rather
// than on this set.
//
// The question's kind stamps its own id, so a question this build does not name is refused
// here rather than published under a family prefix that names nothing.
func (s *ReplyStream) Elicit(ask *wire.ElicitRequest) error {
	protocol, ok := wire.ElicitRequestProtocolFor(ask.Kind)
	if !ok {
		return fmt.Errorf("%w: %q is not a question this agent asks", wire.ErrInvalidMessage, ask.Kind)
	}

	ask.Protocol = protocol

	return s.send(&ask.Header, ask, false)
}

// Sequence reports the number stamped on the last message sent, which is how many
// messages of the set have gone out. It is 0 before the ack.
func (s *ReplyStream) Sequence() uint64 { return s.seq }

// send stamps, encodes and publishes one message, advancing the counter only once it
// has gone out. A message the sink refused was never sent, so reusing its number
// keeps the set gap-free and stops a gap describing a message the sender chose not
// to send.
func (s *ReplyStream) send(hdr *wire.Header, msg any, final bool) error {
	data, err := s.encode(hdr, msg)
	if err != nil {
		return err
	}

	err = s.reply.Publish(data, final)
	if err != nil {
		return fmt.Errorf("publishing %s: %w", hdr.Protocol, err)
	}

	s.seq++

	return nil
}

// encode stamps a message header so it echoes the request, numbers it with the
// sequence it would occupy, and marshals it. The encoded body is checked against the
// size cap before it reaches the sink, since an event carrying a large tool result
// can exceed both it and the transport's own payload limit.
func (s *ReplyStream) encode(hdr *wire.Header, msg any) ([]byte, error) {
	wire.StampReply(hdr, &s.req, s.sender)
	hdr.Sequence = s.seq + 1

	data, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("marshaling %s: %w", hdr.Protocol, err)
	}

	if len(data) > wire.MaxMessageSize {
		return nil, fmt.Errorf("%w: %s is %d bytes, over the %d byte limit", ErrMessageTooLarge, hdr.Protocol, len(data), wire.MaxMessageSize)
	}

	return data, nil
}

// AcceptStream reports whether a task's events may be streamed to its caller over
// transport, and refuses a request asking for what the transport cannot carry.
//
// The refusal is narrow because Request.Stream is a *bool: a request that explicitly
// asks to stream is refused, and one that says nothing is answered terminal-only,
// which is what a caller who did not ask for a stream gets anyway.
func AcceptStream(transport Transport, req *wire.Request) (bool, error) {
	_, streams := transport.(StreamingTransport)

	switch {
	case streams:
		return req.WantsStream(), nil
	case req.Stream != nil && *req.Stream:
		return false, fmt.Errorf("%w: this transport carries a single reply, not a reply set", ErrStreamUnsupported)
	default:
		return false, nil
	}
}
