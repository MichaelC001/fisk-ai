//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// sentMessage is one message a fake sink received, with how it was sent.
type sentMessage struct {
	body       []byte
	final      bool
	viaRespond bool
}

// fakeSink is an a2a.StreamReplier that records the reply set a ReplyStream
// produces, so a test can assert both the numbering and which call carried each
// message. failFrom makes every send from that message on fail, which is how a
// failed publish is driven.
type fakeSink struct {
	sent     []sentMessage
	failFrom int
	calls    int
}

func (s *fakeSink) Respond(body []byte) error {
	return s.record(sentMessage{body: body, viaRespond: true})
}

func (s *fakeSink) Error(string, string) error { return nil }

func (s *fakeSink) Publish(body []byte, final bool) error {
	return s.record(sentMessage{body: body, final: final})
}

func (s *fakeSink) record(msg sentMessage) error {
	s.calls++
	if s.failFrom > 0 && s.calls >= s.failFrom {
		return fmt.Errorf("the sink refused message %d", s.calls)
	}

	s.sent = append(s.sent, msg)

	return nil
}

// sequences returns the sequence number of every message the sink received.
func (s *fakeSink) sequences() []uint64 {
	var out []uint64
	for _, msg := range s.sent {
		var hdr Header
		Expect(json.Unmarshal(msg.body, &hdr)).To(Succeed())
		out = append(out, hdr.Sequence)
	}

	return out
}

// taskRequest is the inbound request header a reply set answers.
func taskRequest() *Header {
	req := NewRequest("do the thing")
	StampRequest(context.Background(), &req.Header, "caller", "svc")

	return &req.Header
}

// plainTransport is a Transport that carries a single reply, for the admission
// question a request asking to stream puts to one.
type plainTransport struct{}

func (plainTransport) RoundTrip(context.Context, string, RouteHint, []byte) ([]byte, error) {
	return nil, nil
}
func (plainTransport) Serve(RouteHint, Handler) error { return nil }
func (plainTransport) Describe(string) []DescLine     { return nil }
func (plainTransport) Close() error                   { return nil }

var _ = Describe("ReplyStream", func() {
	var sink *fakeSink
	var stream *ReplyStream

	BeforeEach(func() {
		sink = &fakeSink{}
		stream = NewReplyStream(sink, taskRequest(), "svc")
	})

	It("Should number from 1 with no gaps across the ack, the events and the terminal", func() {
		Expect(stream.Ack(NewAck(true))).To(Succeed())
		Expect(stream.Event(NewTextBlock("first"))).To(Succeed())
		Expect(stream.Event(NewTextBlock("second"))).To(Succeed())

		res := NewResult(StopEndTurn)
		res.Text = "done"
		Expect(stream.Result(res)).To(Succeed())

		Expect(sink.sequences()).To(Equal([]uint64{1, 2, 3, 4}))
		Expect(stream.Sequence()).To(Equal(uint64(4)))
	})

	It("Should carry the ack through Respond and everything after it through Publish, marking only the last", func() {
		Expect(stream.Ack(NewAck(true))).To(Succeed())
		Expect(stream.Event(NewTextBlock("working"))).To(Succeed())
		Expect(stream.Error(NewError("it broke"))).To(Succeed())

		Expect(sink.sent).To(HaveLen(3))
		Expect(sink.sent[0].viaRespond).To(BeTrue())
		Expect(sink.sent[1].viaRespond).To(BeFalse())
		Expect(sink.sent[2].viaRespond).To(BeFalse())

		Expect(sink.sent[0].final).To(BeFalse())
		Expect(sink.sent[1].final).To(BeFalse())
		Expect(sink.sent[2].final).To(BeTrue())
	})

	It("Should echo the request it answers on every message", func() {
		req := taskRequest()
		stream = NewReplyStream(sink, req, "svc")

		Expect(stream.Ack(NewAck(true))).To(Succeed())
		Expect(stream.Event(NewTextBlock("hi"))).To(Succeed())

		for _, msg := range sink.sent {
			var hdr Header
			Expect(json.Unmarshal(msg.body, &hdr)).To(Succeed())
			Expect(hdr.Request).To(Equal(req.Request))
			Expect(hdr.Conversation).To(Equal(req.Conversation))
			Expect(hdr.Sender.Name).To(Equal("svc"))
			Expect(hdr.Recipient.Name).To(Equal("caller"))
		}
	})

	It("Should refuse an oversized event without advancing the sequence", func() {
		Expect(stream.Ack(NewAck(true))).To(Succeed())

		err := stream.Event(NewTextBlock(strings.Repeat("x", MaxMessageSize)))
		Expect(err).To(MatchError(ErrMessageTooLarge))
		Expect(sink.sent).To(HaveLen(1))

		// The number the refused event would have taken is still free, so the event that
		// does go out leaves no gap describing a message the sender chose not to send.
		Expect(stream.Event(NewTextBlock("small"))).To(Succeed())
		Expect(sink.sequences()).To(Equal([]uint64{1, 2}))
	})

	It("Should report a failed terminal publish rather than dropping it", func() {
		Expect(stream.Ack(NewAck(true))).To(Succeed())

		sink.failFrom = 2
		err := stream.Result(NewResult(StopEndTurn))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("the sink refused"))

		// Nothing went out, so the number is still free.
		Expect(stream.Sequence()).To(Equal(uint64(1)))
	})

	It("Should refuse an ack that is not the first message of the set", func() {
		Expect(stream.Ack(NewAck(true))).To(Succeed())
		Expect(stream.Event(NewTextBlock("hi"))).To(Succeed())

		Expect(stream.Ack(NewAck(true))).To(MatchError(ErrInvalidMessage))
	})

	It("Should produce messages that pass the schema", func() {
		validator, err := NewValidator()
		Expect(err).ToNot(HaveOccurred())

		refusal := NewAck(false)
		refusal.Reason = "at capacity"
		refusal.ConversationToken = "2Ab3Cd4Ef5Gh"

		Expect(stream.Ack(refusal)).To(Succeed())
		Expect(stream.Event(NewTextBlock("hi"))).To(Succeed())
		Expect(stream.Result(NewResult(StopEndTurn))).To(Succeed())

		for _, msg := range sink.sent {
			Expect(validator.Validate(msg.body)).To(Succeed())
		}
	})
})

var _ = Describe("AcceptStream", func() {
	yes, no := true, false

	It("Should refuse only a request that explicitly asks a transport that cannot carry one", func() {
		req := NewRequest("go")
		req.Stream = &yes

		_, err := AcceptStream(plainTransport{}, req)
		Expect(err).To(MatchError(ErrStreamUnsupported))
	})

	It("Should answer a request that says nothing terminal-only rather than refusing it", func() {
		stream, err := AcceptStream(plainTransport{}, NewRequest("go"))
		Expect(err).ToNot(HaveOccurred())
		Expect(stream).To(BeFalse())

		req := NewRequest("go")
		req.Stream = &no
		stream, err = AcceptStream(plainTransport{}, req)
		Expect(err).ToNot(HaveOccurred())
		Expect(stream).To(BeFalse())
	})

	It("Should stream on a transport that can, unless the request declined it", func() {
		stream, err := AcceptStream(&scriptedTransport{}, NewRequest("go"))
		Expect(err).ToNot(HaveOccurred())
		Expect(stream).To(BeTrue())

		req := NewRequest("go")
		req.Stream = &no
		stream, err = AcceptStream(&scriptedTransport{}, req)
		Expect(err).ToNot(HaveOccurred())
		Expect(stream).To(BeFalse())
	})
})
