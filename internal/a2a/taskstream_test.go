//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// scriptedTransport is a StreamingTransport whose reply set is written by the test.
// Stream hands back a reader over script, stamped to answer whatever request it was
// given, so the engine's half of a task can be driven with no wire.
type scriptedTransport struct {
	// script builds the reply set for a request header. Nil yields an empty set.
	script func(req *Header) [][]byte
	// block, when set, is returned by the reader instead of a message, so a read
	// waits until the caller's context ends.
	block bool
	// cancelReply is what SendCancel answers with; a nil value answers an accepted ack.
	cancelReply func(req *Header) []byte

	sent    *Header
	readers []*scriptedReader
}

func (t *scriptedTransport) RoundTrip(context.Context, string, RouteHint, []byte) ([]byte, error) {
	return nil, nil
}
func (t *scriptedTransport) Serve(RouteHint, Handler) error        { return nil }
func (t *scriptedTransport) Describe(string) []DescLine            { return nil }
func (t *scriptedTransport) DescribeTasks(string, bool) []DescLine { return nil }
func (t *scriptedTransport) Close() error                          { return nil }

func (t *scriptedTransport) Stream(_ context.Context, _ string, _ RouteHint, body []byte) (Reader, error) {
	var hdr Header
	err := json.Unmarshal(body, &hdr)
	if err != nil {
		return nil, err
	}
	t.sent = &hdr

	var messages [][]byte
	if t.script != nil {
		messages = t.script(&hdr)
	}

	r := &scriptedReader{messages: messages, block: t.block}
	t.readers = append(t.readers, r)

	return r, nil
}

func (t *scriptedTransport) WatchCancel(string, Handler) (TaskWatch, error) {
	return nil, fmt.Errorf("the scripted transport does not serve")
}

func (t *scriptedTransport) WatchElicitReplies(string, Handler) (TaskWatch, error) {
	return nil, fmt.Errorf("the scripted transport does not serve")
}

func (t *scriptedTransport) SendElicitReply(context.Context, string, string, []byte) ([]byte, error) {
	return nil, fmt.Errorf("the scripted transport answers no questions")
}

func (t *scriptedTransport) SendCancel(_ context.Context, _, _ string, body []byte) ([]byte, error) {
	var hdr Header
	err := json.Unmarshal(body, &hdr)
	if err != nil {
		return nil, err
	}
	t.sent = &hdr

	if t.cancelReply != nil {
		return t.cancelReply(&hdr), nil
	}

	ack := NewAck(true)
	StampReply(&ack.Header, &hdr, "svc")
	ack.Sequence = 1

	return encodeMessage(ack), nil
}

// scriptedReader yields a written reply set in order.
type scriptedReader struct {
	messages [][]byte
	block    bool
	at       int
	closed   bool
}

func (r *scriptedReader) Next(ctx context.Context) ([]byte, error) {
	if r.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}

	if r.at >= len(r.messages) {
		return nil, io.EOF
	}

	msg := r.messages[r.at]
	r.at++

	return msg, nil
}

func (r *scriptedReader) Close() error {
	r.closed = true

	return nil
}

// encodeMessage marshals a message for a script.
func encodeMessage(msg any) []byte {
	GinkgoHelper()

	data, err := json.Marshal(msg)
	Expect(err).ToNot(HaveOccurred())

	return data
}

// replySet builds an ack, one event per text, and a terminal result, numbered
// gap-free from 1, all answering req.
func replySet(req *Header, texts ...string) [][]byte {
	GinkgoHelper()

	var out [][]byte
	seq := uint64(0)

	stamp := func(hdr *Header) {
		StampReply(hdr, req, "svc")
		seq++
		hdr.Sequence = seq
	}

	ack := NewAck(true)
	stamp(&ack.Header)
	out = append(out, encodeMessage(ack))

	for _, text := range texts {
		ev := NewEvent(NewTextBlock(text))
		stamp(&ev.Header)
		out = append(out, encodeMessage(ev))
	}

	res := NewResult(StopEndTurn)
	res.Text = "done"
	stamp(&res.Header)
	out = append(out, encodeMessage(res))

	return out
}

// taskClient builds a client over transport and starts a task on it.
func taskClient(transport Transport) *Client {
	GinkgoHelper()

	client, err := NewClient(transport, "caller")
	Expect(err).ToNot(HaveOccurred())

	return client
}

var _ = Describe("TaskStream", func() {
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		DeferCleanup(cancel)
	})

	It("Should yield the ack, then the events, then the terminal result", func() {
		transport := &scriptedTransport{script: func(req *Header) [][]byte {
			return replySet(req, "thinking", "still thinking")
		}}

		stream, err := taskClient(transport).Task(ctx, "svc", NewRequest("go"))
		Expect(err).ToNot(HaveOccurred())

		msg, err := stream.Next(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(msg).To(BeAssignableToTypeOf(&Ack{}))
		Expect(msg.(*Ack).Accepted).To(BeTrue())

		for _, want := range []string{"thinking", "still thinking"} {
			msg, err = stream.Next(ctx)
			Expect(err).ToNot(HaveOccurred())
			Expect(msg).To(BeAssignableToTypeOf(&Event{}))
			Expect(msg.(*Event).Block.Content()).To(Equal(TextBlock{Text: want}))
		}

		msg, err = stream.Next(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(msg).To(BeAssignableToTypeOf(&Result{}))
		Expect(msg.(*Result).Text).To(Equal("done"))

		Expect(stream.Gaps()).To(BeZero())
	})

	It("Should return io.EOF once the terminal message has been returned", func() {
		transport := &scriptedTransport{script: func(req *Header) [][]byte {
			return replySet(req)
		}}

		stream, err := taskClient(transport).Task(ctx, "svc", NewRequest("go"))
		Expect(err).ToNot(HaveOccurred())

		for range 2 {
			_, err = stream.Next(ctx)
			Expect(err).ToNot(HaveOccurred())
		}

		_, err = stream.Next(ctx)
		Expect(err).To(MatchError(io.EOF))
	})

	It("Should report a gap beside a successful result rather than failing the task", func() {
		transport := &scriptedTransport{script: func(req *Header) [][]byte {
			set := replySet(req, "one", "two")

			// The two events are dropped, so the terminal message arrives numbered 4 where
			// the reader last saw 1.
			return [][]byte{set[0], set[3]}
		}}

		stream, err := taskClient(transport).Task(ctx, "svc", NewRequest("go"))
		Expect(err).ToNot(HaveOccurred())

		_, err = stream.Next(ctx)
		Expect(err).ToNot(HaveOccurred())

		msg, err := stream.Next(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(msg).To(BeAssignableToTypeOf(&Result{}))
		Expect(msg.(*Result).Text).To(Equal("done"))

		Expect(stream.Gaps()).To(Equal(uint64(2)))
	})

	// The cost this item removed: the reason traveled on the same message as the
	// answer, so refusing one lost the other.
	It("Should deliver a terminal result whose stop reason it does not name", func() {
		transport := &scriptedTransport{script: func(req *Header) [][]byte {
			ack := NewAck(true)
			StampReply(&ack.Header, req, "svc")
			ack.Sequence = 1

			res := NewResult(StopReason("throttled"))
			res.Text = "as far as I got"
			res.Usage = &Usage{InputTokens: 10, OutputTokens: 20}
			StampReply(&res.Header, req, "svc")
			res.Sequence = 2

			return [][]byte{encodeMessage(ack), encodeMessage(res)}
		}}

		stream, err := taskClient(transport).Task(ctx, "svc", NewRequest("go"))
		Expect(err).ToNot(HaveOccurred())

		_, err = stream.Next(ctx)
		Expect(err).ToNot(HaveOccurred())

		msg, err := stream.Next(ctx)
		Expect(err).ToNot(HaveOccurred())

		res := msg.(*Result)
		Expect(res.StopReason).To(Equal(StopReason("throttled")))
		Expect(res.StopReason.Valid()).To(BeFalse())
		Expect(res.Text).To(Equal("as far as I got"))
		Expect(res.Usage.OutputTokens).To(Equal(int64(20)))
	})

	It("Should return a failed task as an ErrorMessage value, not as the error", func() {
		transport := &scriptedTransport{script: func(req *Header) [][]byte {
			ack := NewAck(true)
			StampReply(&ack.Header, req, "svc")
			ack.Sequence = 1

			failed := NewError("the tool could not run")
			failed.StopReason = StopError
			StampReply(&failed.Header, req, "svc")
			failed.Sequence = 2

			return [][]byte{encodeMessage(ack), encodeMessage(failed)}
		}}

		stream, err := taskClient(transport).Task(ctx, "svc", NewRequest("go"))
		Expect(err).ToNot(HaveOccurred())

		_, err = stream.Next(ctx)
		Expect(err).ToNot(HaveOccurred())

		msg, err := stream.Next(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(msg).To(BeAssignableToTypeOf(&ErrorMessage{}))
		Expect(msg.(*ErrorMessage).Err).To(Equal("the tool could not run"))
	})

	It("Should stop the read when the context ends mid-stream", func() {
		transport := &scriptedTransport{block: true}

		stream, err := taskClient(transport).Task(ctx, "svc", NewRequest("go"))
		Expect(err).ToNot(HaveOccurred())

		short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()

		_, err = stream.Next(short)
		Expect(err).To(MatchError(context.DeadlineExceeded))
	})

	// The reason the block item rides beside this one: a reply set is where an event
	// first reaches an independently versioned peer, and the whole message was the
	// unit being lost.
	It("Should deliver an event of a kind it does not name", func() {
		transport := &scriptedTransport{script: func(req *Header) [][]byte {
			set := replySet(req, "before")

			// A third message hand-built under an event id no build here names, numbered
			// after the event beside it.
			unknown := []byte(`{"protocol":"` + EventProtocol + `.citation","id":"` + NewID() +
				`","request":"` + req.Request + `","conversation":"` + req.Conversation +
				`","sequence":3,"time":"2026-01-01T00:00:00Z","sender":{"name":"svc"},` +
				`"block":{"source":"rfc1","page":12}}`)

			return [][]byte{set[0], set[1], unknown}
		}}

		stream, err := taskClient(transport).Task(ctx, "svc", NewRequest("go"))
		Expect(err).ToNot(HaveOccurred())

		for range 2 {
			_, err = stream.Next(ctx)
			Expect(err).ToNot(HaveOccurred())
		}

		msg, err := stream.Next(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(msg).To(BeAssignableToTypeOf(&Event{}))

		block := msg.(*Event).Block
		Expect(block.Type()).To(Equal(BlockType("citation")))
		Expect(block.Content()).To(BeAssignableToTypeOf(UnknownBlock{}))
		Expect(stream.Gaps()).To(BeZero())
	})

	It("Should refuse a message that does not belong in a reply set", func() {
		transport := &scriptedTransport{script: func(req *Header) [][]byte {
			reply := NewToolReply("ok", false)
			StampReply(&reply.Header, req, "svc")
			reply.Sequence = 1

			return [][]byte{encodeMessage(reply)}
		}}

		stream, err := taskClient(transport).Task(ctx, "svc", NewRequest("go"))
		Expect(err).ToNot(HaveOccurred())

		_, err = stream.Next(ctx)
		Expect(err).To(MatchError(ErrProtocolMismatch))
	})

	It("Should close the reader it was given", func() {
		transport := &scriptedTransport{script: func(req *Header) [][]byte { return replySet(req) }}

		stream, err := taskClient(transport).Task(ctx, "svc", NewRequest("go"))
		Expect(err).ToNot(HaveOccurred())
		Expect(stream.Close()).To(Succeed())

		Expect(transport.readers).To(HaveLen(1))
		Expect(transport.readers[0].closed).To(BeTrue())
	})

	It("Should refuse a task on a transport that carries a single reply", func() {
		client := taskClient(plainTransport{})
		Expect(client.CanStream()).To(BeFalse())

		_, err := client.Task(ctx, "svc", NewRequest("go"))
		Expect(err).To(MatchError(ErrStreamUnsupported))

		_, err = client.Cancel(ctx, "svc", NewID(), "changed my mind")
		Expect(err).To(MatchError(ErrStreamUnsupported))
	})
})

var _ = Describe("Client.Cancel", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("Should address the cancel to the task it stops rather than to itself", func() {
		transport := &scriptedTransport{}
		task := NewID()

		ack, err := taskClient(transport).Cancel(ctx, "svc", task, "the caller went away")
		Expect(err).ToNot(HaveOccurred())
		Expect(ack.Accepted).To(BeTrue())

		Expect(transport.sent.Request).To(Equal(task))
		Expect(transport.sent.ID).ToNot(Equal(task))
		Expect(transport.sent.Protocol).To(Equal(CancelProtocol))
	})

	It("Should refuse a request id that could not address one", func() {
		_, err := taskClient(&scriptedTransport{}).Cancel(ctx, "svc", "task.>", "stop")
		Expect(err).To(MatchError(ErrInvalidMessage))
	})

	It("Should refuse a reply that is not an ack", func() {
		transport := &scriptedTransport{cancelReply: func(req *Header) []byte {
			res := NewResult(StopCanceled)
			StampReply(&res.Header, req, "svc")
			res.Sequence = 1

			return encodeMessage(res)
		}}

		_, err := taskClient(transport).Cancel(ctx, "svc", NewID(), "stop")
		Expect(err).To(MatchError(ErrProtocolMismatch))
	})
})
