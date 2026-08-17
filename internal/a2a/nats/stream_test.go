//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package nats

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/nats-io/nats.go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/conns"
)

// streamingOver builds a nats transport for identity on nc and asserts it carries a
// reply set, which is the assertion a program makes once at startup.
func streamingOver(nc *nats.Conn, identity string) a2a.StreamingTransport {
	GinkgoHelper()

	transport, err := a2a.NewTransport("nats", conns.New(conns.WithNats(nc)), a2a.TransportConfig{Identity: identity, Timeout: time.Second})
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(transport.Close)

	streaming, ok := transport.(a2a.StreamingTransport)
	Expect(ok).To(BeTrue(), "the nats binding carries a reply set")

	return streaming
}

// taskRequest builds a task request addressed from caller to agent.
func taskRequest(prompt string) *a2a.Request {
	GinkgoHelper()

	req := a2a.NewRequest(prompt)
	req.ID = a2a.NewID()
	req.Request = req.ID
	req.Conversation = req.ID
	req.Time = time.Now().UTC()
	req.Sender = a2a.Identity{Name: "caller"}

	return req
}

// encode marshals a message for publishing.
func encode(msg any) []byte {
	GinkgoHelper()

	data, err := json.Marshal(msg)
	Expect(err).NotTo(HaveOccurred())

	return data
}

// drain reads a reply set to its end and returns every body it yielded.
func drain(ctx context.Context, reader a2a.Reader) [][]byte {
	GinkgoHelper()

	var out [][]byte
	for {
		body, err := reader.Next(ctx)
		if err == io.EOF {
			return out
		}
		Expect(err).NotTo(HaveOccurred())
		out = append(out, body)
	}
}

var _ = Describe("Integration: a2a NATS reply set", func() {
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
		DeferCleanup(cancel)
	})

	It("Should carry many events and a terminal result across a real inbox, in order", func() {
		nc := runNATS()
		server := streamingOver(nc, "svc")
		caller := streamingOver(nc, "caller")

		// The served side is what phase 3 builds; here it is the handler itself, driving a
		// ReplyStream the way an admitted task will.
		err := server.Serve(a2a.OpTask, func(_ context.Context, _ a2a.Caller, body []byte, reply a2a.Replier) {
			defer GinkgoRecover()

			var hdr a2a.Header
			Expect(json.Unmarshal(body, &hdr)).To(Succeed())

			sink, ok := reply.(a2a.StreamReplier)
			Expect(ok).To(BeTrue(), "a streaming transport supplies a stream replier")

			stream := a2a.NewReplyStream(sink, &hdr, "svc")
			Expect(stream.Ack(true, "")).To(Succeed())

			go func() {
				defer GinkgoRecover()

				for _, text := range []string{"one", "two", "three"} {
					Expect(stream.Event(a2a.NewTextBlock(text))).To(Succeed())
				}

				res := a2a.NewResult(a2a.StopEndTurn)
				res.Text = "done"
				Expect(stream.Result(res)).To(Succeed())
			}()
		})
		Expect(err).NotTo(HaveOccurred())

		reader, err := caller.Stream(ctx, "svc", a2a.OpTask, encode(taskRequest("go")))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(reader.Close)

		bodies := drain(ctx, reader)
		Expect(bodies).To(HaveLen(5))

		var protocols []string
		var sequences []uint64
		for _, body := range bodies {
			var hdr a2a.Header
			Expect(json.Unmarshal(body, &hdr)).To(Succeed())
			protocols = append(protocols, hdr.Protocol)
			sequences = append(sequences, hdr.Sequence)
		}

		Expect(protocols).To(Equal([]string{
			a2a.AckProtocol,
			a2a.EventProtocol, a2a.EventProtocol, a2a.EventProtocol,
			a2a.ResultProtocol,
		}))
		Expect(sequences).To(Equal([]uint64{1, 2, 3, 4, 5}))
	})

	It("Should refuse the first event of a request that carried no reply inbox", func() {
		nc := runNATS()
		server := streamingOver(nc, "svc")

		refusal := make(chan error, 1)
		err := server.Serve(a2a.OpTask, func(_ context.Context, _ a2a.Caller, body []byte, reply a2a.Replier) {
			defer GinkgoRecover()

			var hdr a2a.Header
			Expect(json.Unmarshal(body, &hdr)).To(Succeed())

			sink, ok := reply.(a2a.StreamReplier)
			Expect(ok).To(BeTrue())

			refusal <- a2a.NewReplyStream(sink, &hdr, "svc").Event(a2a.NewTextBlock("nobody is listening"))
		})
		Expect(err).NotTo(HaveOccurred())

		// Published rather than requested, so micro hands the handler a request with no
		// reply subject on it.
		Expect(nc.Publish(TaskSubject("svc"), encode(taskRequest("go")))).To(Succeed())

		var got error
		Eventually(refusal).Should(Receive(&got))
		Expect(got).To(MatchError(ContainSubstring("no reply subject")))
	})

	It("Should drop a message belonging to another reply set", func() {
		nc := runNATS()
		server := streamingOver(nc, "svc")
		caller := streamingOver(nc, "caller")

		err := server.Serve(a2a.OpTask, func(_ context.Context, _ a2a.Caller, body []byte, reply a2a.Replier) {
			defer GinkgoRecover()

			var hdr a2a.Header
			Expect(json.Unmarshal(body, &hdr)).To(Succeed())
			sink := reply.(a2a.StreamReplier)

			// A terminal message carrying another request's id, then this set's own. A
			// reader that took the first would end the read before the answer arrived.
			foreign := a2a.NewResult(a2a.StopEndTurn)
			foreign.ID = a2a.NewID()
			foreign.Request = a2a.NewID()
			foreign.Conversation = foreign.Request
			foreign.Sequence = 1
			foreign.Time = time.Now().UTC()
			foreign.Sender = a2a.Identity{Name: "svc"}
			foreign.Text = "another task's answer"
			Expect(sink.Publish(encode(foreign), true)).To(Succeed())

			stream := a2a.NewReplyStream(sink, &hdr, "svc")
			Expect(stream.Ack(true, "")).To(Succeed())

			res := a2a.NewResult(a2a.StopEndTurn)
			res.Text = "mine"
			Expect(stream.Result(res)).To(Succeed())
		})
		Expect(err).NotTo(HaveOccurred())

		reader, err := caller.Stream(ctx, "svc", a2a.OpTask, encode(taskRequest("go")))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(reader.Close)

		bodies := drain(ctx, reader)
		Expect(bodies).To(HaveLen(2))

		var res a2a.Result
		Expect(json.Unmarshal(bodies[1], &res)).To(Succeed())
		Expect(res.Text).To(Equal("mine"))
	})

	It("Should surface a micro service error rather than yielding its empty body", func() {
		nc := runNATS()
		server := streamingOver(nc, "svc")
		caller := streamingOver(nc, "caller")

		err := server.Serve(a2a.OpTask, func(_ context.Context, _ a2a.Caller, _ []byte, reply a2a.Replier) {
			defer GinkgoRecover()
			Expect(reply.Error("400", "request exceeds the size limit")).To(Succeed())
		})
		Expect(err).NotTo(HaveOccurred())

		reader, err := caller.Stream(ctx, "svc", a2a.OpTask, encode(taskRequest("go")))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(reader.Close)

		_, err = reader.Next(ctx)
		Expect(err).To(MatchError(ContainSubstring("400")))
		Expect(err).To(MatchError(ContainSubstring("size limit")))
	})

	It("Should report an identity nobody serves as unavailable", func() {
		nc := runNATS()
		caller := streamingOver(nc, "caller")

		reader, err := caller.Stream(ctx, "nobody", a2a.OpTask, encode(taskRequest("go")))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(reader.Close)

		_, err = reader.Next(ctx)
		Expect(err).To(MatchError(a2a.ErrAgentUnavailable))
	})
})

var _ = Describe("Integration: a2a NATS cancel", func() {
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
		DeferCleanup(cancel)
	})

	It("Should reach the one instance holding the task while a sibling hears nothing", func() {
		nc := runNATS()
		holder := streamingOver(nc, "svc")
		sibling := streamingOver(nc, "svc")
		caller := streamingOver(nc, "caller")

		task := a2a.NewID()

		held := make(chan []byte, 1)
		watch, err := holder.WatchCancel(task, func(_ context.Context, _ a2a.Caller, body []byte, reply a2a.Replier) {
			defer GinkgoRecover()
			held <- body
			Expect(reply.Respond(encode(cancelAck(body)))).To(Succeed())
		})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(watch.Close)

		// The sibling is running a different task, so it must not hear this cancel.
		heardBySibling := make(chan []byte, 1)
		other, err := sibling.WatchCancel(a2a.NewID(), func(_ context.Context, _ a2a.Caller, body []byte, _ a2a.Replier) {
			heardBySibling <- body
		})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(other.Close)

		reply, err := caller.SendCancel(ctx, "svc", task, encode(cancelFor(task)))
		Expect(err).NotTo(HaveOccurred())

		var ack a2a.Ack
		Expect(json.Unmarshal(reply, &ack)).To(Succeed())
		Expect(ack.Accepted).To(BeTrue())

		Eventually(held).Should(Receive())
		Consistently(heardBySibling, 200*time.Millisecond, 20*time.Millisecond).ShouldNot(Receive())
	})

	It("Should report no responders once the task has ended", func() {
		nc := runNATS()
		holder := streamingOver(nc, "svc")
		caller := streamingOver(nc, "caller")

		task := a2a.NewID()

		watch, err := holder.WatchCancel(task, func(context.Context, a2a.Caller, []byte, a2a.Replier) {})
		Expect(err).NotTo(HaveOccurred())

		// The task ends, which is what releases its subscription.
		Expect(watch.Close()).To(Succeed())
		Expect(nc.Flush()).To(Succeed())

		_, err = caller.SendCancel(ctx, "svc", task, encode(cancelFor(task)))
		Expect(err).To(MatchError(a2a.ErrAgentUnavailable))
	})

	It("Should release the subscription only once however often a task ends", func() {
		nc := runNATS()
		holder := streamingOver(nc, "svc")

		watch, err := holder.WatchCancel(a2a.NewID(), func(context.Context, a2a.Caller, []byte, a2a.Replier) {})
		Expect(err).NotTo(HaveOccurred())

		Expect(watch.Close()).To(Succeed())
		Expect(watch.Close()).To(Succeed())
	})

	It("Should refuse a request id that would shape the subject, before any subscription is made", func() {
		nc := runNATS()
		transport := streamingOver(nc, "svc")

		for _, bad := range []string{"task.other", "task>", "task*", "", "task with space"} {
			watch, err := transport.WatchCancel(bad, func(context.Context, a2a.Caller, []byte, a2a.Replier) {})
			Expect(err).To(MatchError(ContainSubstring("not a valid request id")), bad)
			Expect(watch).To(BeNil(), bad)

			_, err = transport.SendCancel(ctx, "svc", bad, []byte(`{}`))
			Expect(err).To(MatchError(ContainSubstring("not a valid request id")), bad)
		}
	})
})

// cancelFor builds a cancel addressed to the named task.
func cancelFor(task string) *a2a.Cancel {
	GinkgoHelper()

	msg := a2a.NewCancel()
	msg.ID = a2a.NewID()
	msg.Request = task
	msg.Conversation = task
	msg.Time = time.Now().UTC()
	msg.Sender = a2a.Identity{Name: "caller"}
	msg.Reason = "the caller went away"

	return msg
}

// cancelAck builds the ack answering a cancel body.
func cancelAck(body []byte) *a2a.Ack {
	GinkgoHelper()

	var hdr a2a.Header
	Expect(json.Unmarshal(body, &hdr)).To(Succeed())

	ack := a2a.NewAck(true)
	ack.ID = a2a.NewID()
	ack.Request = hdr.Request
	ack.Conversation = hdr.Conversation
	ack.Sequence = 1
	ack.Time = time.Now().UTC()
	ack.Sender = a2a.Identity{Name: "svc"}

	return ack
}

var _ = Describe("Integration: a2a NATS elicit replies", func() {
	var ctx context.Context

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
		DeferCleanup(cancel)
	})

	It("Should reach the one instance that asked the question while a sibling hears nothing", func() {
		nc := runNATS()
		holder := streamingOver(nc, "svc")
		sibling := streamingOver(nc, "svc")
		caller := streamingOver(nc, "caller")

		task := a2a.NewID()

		answered := make(chan []byte, 1)
		watch, err := holder.WatchElicitReplies(task, func(_ context.Context, _ a2a.Caller, body []byte, reply a2a.Replier) {
			defer GinkgoRecover()
			answered <- body
			Expect(reply.Respond(encode(cancelAck(body)))).To(Succeed())
		})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(watch.Close)

		// The sibling is running a different task, so an answer to this one must not
		// reach it: an answer approves what a run does, and it belongs to one run.
		heardBySibling := make(chan []byte, 1)
		other, err := sibling.WatchElicitReplies(a2a.NewID(), func(_ context.Context, _ a2a.Caller, body []byte, _ a2a.Replier) {
			heardBySibling <- body
		})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(other.Close)

		reply, err := caller.SendElicitReply(ctx, "svc", task, encode(answerFor(task, "q1")))
		Expect(err).NotTo(HaveOccurred())

		var ack a2a.Ack
		Expect(json.Unmarshal(reply, &ack)).To(Succeed())
		Expect(ack.Accepted).To(BeTrue())

		var body []byte
		Eventually(answered).Should(Receive(&body))
		Consistently(heardBySibling, 200*time.Millisecond, 20*time.Millisecond).ShouldNot(Receive())

		var got a2a.ElicitReply
		Expect(json.Unmarshal(body, &got)).To(Succeed())
		Expect(got.QuestionID).To(Equal("q1"))
		Expect(got.Choice).To(Equal(a2a.ChoiceOnce))
	})

	It("Should not route an answer to a task watching only cancels", func() {
		nc := runNATS()
		holder := streamingOver(nc, "svc")
		caller := streamingOver(nc, "caller")

		task := a2a.NewID()

		heard := make(chan []byte, 1)
		watch, err := holder.WatchCancel(task, func(_ context.Context, _ a2a.Caller, body []byte, _ a2a.Replier) {
			heard <- body
		})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(watch.Close)

		// The two travel on subjects of their own, so an operator can grant answering a
		// question and canceling a task separately.
		_, err = caller.SendElicitReply(ctx, "svc", task, encode(answerFor(task, "q1")))
		Expect(err).To(MatchError(a2a.ErrAgentUnavailable))
		Consistently(heard, 200*time.Millisecond, 20*time.Millisecond).ShouldNot(Receive())
	})

	It("Should report no responders once the run that asked has ended", func() {
		nc := runNATS()
		holder := streamingOver(nc, "svc")
		caller := streamingOver(nc, "caller")

		task := a2a.NewID()

		watch, err := holder.WatchElicitReplies(task, func(context.Context, a2a.Caller, []byte, a2a.Replier) {})
		Expect(err).NotTo(HaveOccurred())

		Expect(watch.Close()).To(Succeed())
		Expect(nc.Flush()).To(Succeed())

		_, err = caller.SendElicitReply(ctx, "svc", task, encode(answerFor(task, "q1")))
		Expect(err).To(MatchError(a2a.ErrAgentUnavailable))
	})

	It("Should release the subscription only once however often a task ends", func() {
		nc := runNATS()
		holder := streamingOver(nc, "svc")

		watch, err := holder.WatchElicitReplies(a2a.NewID(), func(context.Context, a2a.Caller, []byte, a2a.Replier) {})
		Expect(err).NotTo(HaveOccurred())

		Expect(watch.Close()).To(Succeed())
		Expect(watch.Close()).To(Succeed())
	})

	It("Should refuse a request id that would shape the subject, before any subscription is made", func() {
		nc := runNATS()
		transport := streamingOver(nc, "svc")

		for _, bad := range []string{"task.other", "task>", "task*", "", "task with space"} {
			watch, err := transport.WatchElicitReplies(bad, func(context.Context, a2a.Caller, []byte, a2a.Replier) {})
			Expect(err).To(MatchError(ContainSubstring("not a valid request id")), bad)
			Expect(watch).To(BeNil(), bad)

			_, err = transport.SendElicitReply(ctx, "svc", bad, []byte(`{}`))
			Expect(err).To(MatchError(ContainSubstring("not a valid request id")), bad)
		}
	})
})

// answerFor builds an elicit reply answering the named question of the named task.
func answerFor(task, question string) *a2a.ElicitReply {
	GinkgoHelper()

	msg := a2a.NewElicitReply(question, a2a.AnswerChoice)
	msg.Choice = a2a.ChoiceOnce
	msg.ID = a2a.NewID()
	msg.Request = task
	msg.Conversation = task
	msg.Time = time.Now().UTC()
	msg.Sender = a2a.Identity{Name: "caller"}

	return msg
}
