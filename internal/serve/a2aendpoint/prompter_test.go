//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2aendpoint

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/a2a"
	natstransport "github.com/choria-io/fisk-ai/internal/a2a/nats"
	"github.com/choria-io/fisk-ai/internal/serve"
	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// These drive the prompter the way a run does: take the work the channel produced, put
// a question through its Prompter, and answer it off the wire as a caller would.
var _ = Describe("Elicitation", func() {
	var (
		ch     *Channel
		client *a2a.Client
	)

	newChannel := func(extra string) {
		GinkgoHelper()

		built, err := NewFromConfig(promptsConfig(extra), ConfigOptions{Conns: provider, Logger: quietLogger()})
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(closeAll, built)

		ch = channelOf(built)
	}

	BeforeEach(func() {
		transport, err := a2a.NewTransport("nats", provider, a2a.TransportConfig{Identity: "caller1", Timeout: 2 * time.Second})
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(transport.Close)

		client, err = a2a.NewClient(transport, "caller1")
		Expect(err).ToNot(HaveOccurred())
	})

	// start sends a prompt and returns the caller's stream with the work the channel
	// produced from it, which is what carries the Prompter under test.
	start := func() (*a2a.TaskStream, *serve.Work) {
		GinkgoHelper()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		DeferCleanup(cancel)

		stream, err := client.Task(ctx, "agent1", a2a.NewRequest("do the thing"))
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(stream.Close)

		workCtx, workCancel := context.WithTimeout(context.Background(), 5*time.Second)
		DeferCleanup(workCancel)

		work, err := ch.Next(workCtx)
		Expect(err).ToNot(HaveOccurred())

		return stream, work
	}

	// question reads messages off the set until the question arrives, since the ack
	// comes first.
	question := func(stream *a2a.TaskStream) *a2a.ElicitRequest {
		GinkgoHelper()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		DeferCleanup(cancel)

		for {
			msg, err := stream.Next(ctx)
			Expect(err).ToNot(HaveOccurred())

			ask, ok := msg.(*a2a.ElicitRequest)
			if ok {
				return ask
			}
		}
	}

	// answer replies to a question on the task's own subject, as a caller does, and
	// returns what the worker answered.
	answer := func(task string, reply *a2a.ElicitReply) *a2a.Ack {
		GinkgoHelper()

		reply.ID = a2a.NewID()
		reply.Request = task
		reply.Conversation = task
		reply.Time = time.Now().UTC()
		reply.Sender = a2a.Identity{Name: "caller1"}

		body, err := json.Marshal(reply)
		Expect(err).ToNot(HaveOccurred())

		msg, err := nc.Request(natstransport.ElicitSubject("agent1", task), body, 5*time.Second)
		Expect(err).ToNot(HaveOccurred())

		decoded, err := a2a.Decode(msg.Data)
		Expect(err).ToNot(HaveOccurred())

		ack, ok := decoded.(*a2a.Ack)
		Expect(ok).To(BeTrue())

		return ack
	}

	// refusalFor sends a reply the worker is expected to turn down and returns the code
	// it refused with, where answer above decodes an acceptance.
	refusalFor := func(task string, reply *a2a.ElicitReply) string {
		GinkgoHelper()

		reply.ID = a2a.NewID()
		reply.Request = task
		reply.Conversation = task
		reply.Time = time.Now().UTC()
		reply.Sender = a2a.Identity{Name: "caller1"}

		body, err := json.Marshal(reply)
		Expect(err).ToNot(HaveOccurred())

		msg, err := nc.Request(natstransport.ElicitSubject("agent1", task), body, 5*time.Second)
		Expect(err).ToNot(HaveOccurred())

		return msg.Header.Get("Nats-Service-Error-Code")
	}

	// ask puts one question through the work's prompter on a goroutine, since the
	// answer has to be sent while it is waiting.
	ask := func(put func() (string, error)) chan struct {
		answer string
		err    error
	} {
		done := make(chan struct {
			answer string
			err    error
		}, 1)

		go func() {
			defer GinkgoRecover()

			value, err := put()
			done <- struct {
				answer string
				err    error
			}{value, err}
		}()

		return done
	}

	Describe("With the key off", func() {
		It("Should supply no prompter, so every gated tool is refused", func() {
			newChannel("        workers: 1\n")

			_, work := start()
			Expect(work.Prompter).To(BeNil())
		})
	})

	Describe("With the key on", func() {
		BeforeEach(func() {
			newChannel("        workers: 1\n        elicit: true\n")
		})

		It("Should carry an approval to the caller and its choice back", func() {
			stream, work := start()
			Expect(work.Prompter).ToNot(BeNil())
			Expect(work.Prompter.CanPrompt()).To(BeTrue())

			done := ask(func() (string, error) {
				choice, err := work.Prompter.ApproveCommand(context.Background(), toolkit.GateRequest{
					Command: "stream rm",
					Display: "stream rm ORDERS",
					Tag:     "ai:confirm",
				})

				return fmt.Sprintf("%d", choice), err
			})

			asked := question(stream)
			Expect(asked.Kind).To(Equal(a2a.ElicitApprove))
			Expect(asked.Command).To(Equal("stream rm"))
			Expect(asked.Display).To(Equal("stream rm ORDERS"))
			Expect(asked.Tag).To(Equal("ai:confirm"))

			reply := a2a.NewElicitReply(asked.QuestionID, a2a.AnswerChoice)
			reply.Choice = a2a.ChoiceAlways
			Expect(answer(asked.Request, reply).Accepted).To(BeTrue())

			var got struct {
				answer string
				err    error
			}
			Eventually(done, 5*time.Second).Should(Receive(&got))
			Expect(got.err).ToNot(HaveOccurred())
			Expect(got.answer).To(Equal(fmt.Sprintf("%d", toolkit.ConfirmAlways)))
		})

		It("Should carry each of the other three questions and their answers", func() {
			stream, work := start()

			confirmed := ask(func() (string, error) {
				yes, err := work.Prompter.Confirm(context.Background(), "Proceed?")

				return fmt.Sprintf("%t", yes), err
			})

			asked := question(stream)
			Expect(asked.Kind).To(Equal(a2a.ElicitConfirm))
			Expect(asked.Question).To(Equal("Proceed?"))

			reply := a2a.NewElicitReply(asked.QuestionID, a2a.AnswerConfirmed)
			reply.Confirmed = true
			answer(asked.Request, reply)

			var got struct {
				answer string
				err    error
			}
			Eventually(confirmed, 5*time.Second).Should(Receive(&got))
			Expect(got.err).ToNot(HaveOccurred())
			Expect(got.answer).To(Equal("true"))

			chosen := ask(func() (string, error) {
				idx, err := work.Prompter.Select(context.Background(), "Which one?", []string{"east", "west"})

				return fmt.Sprintf("%d", idx), err
			})

			asked = question(stream)
			Expect(asked.Kind).To(Equal(a2a.ElicitSelect))
			Expect(asked.Options).To(Equal([]string{"east", "west"}))

			reply = a2a.NewElicitReply(asked.QuestionID, a2a.AnswerIndex)
			reply.Index = 1
			answer(asked.Request, reply)

			Eventually(chosen, 5*time.Second).Should(Receive(&got))
			Expect(got.err).ToNot(HaveOccurred())
			Expect(got.answer).To(Equal("1"))

			typed := ask(func() (string, error) {
				return work.Prompter.Input(context.Background(), "Which subject?", "orders.>")
			})

			asked = question(stream)
			Expect(asked.Kind).To(Equal(a2a.ElicitInput))
			Expect(asked.Default).To(Equal("orders.>"))

			reply = a2a.NewElicitReply(asked.QuestionID, a2a.AnswerValue)
			reply.Value = "orders.new"
			answer(asked.Request, reply)

			Eventually(typed, 5*time.Second).Should(Receive(&got))
			Expect(got.err).ToNot(HaveOccurred())
			Expect(got.answer).To(Equal("orders.new"))
		})

		// A caller that cannot reach a person says so. It is an answer rather than a
		// failure, and it fails closed: the gate reads the error as a denial.
		It("Should report no operator as an error rather than a decline", func() {
			stream, work := start()

			done := ask(func() (string, error) {
				_, err := work.Prompter.ApproveCommand(context.Background(), toolkit.GateRequest{Command: "stream rm", Display: "stream rm ORDERS"})

				return "", err
			})

			asked := question(stream)
			answer(asked.Request, a2a.NewElicitReply(asked.QuestionID, a2a.AnswerNoOperator))

			var got struct {
				answer string
				err    error
			}
			Eventually(done, 5*time.Second).Should(Receive(&got))
			Expect(got.err).To(MatchError(ContainSubstring("no operator")))
			Expect(got.err).ToNot(MatchError(toolkit.ErrPromptAborted))
			Expect(got.err).ToNot(MatchError(toolkit.ErrDeferredResult))
		})

		It("Should refuse an answer to a question it is not waiting on", func() {
			stream, _ := start()

			// A request id the caller knows, taken from the ack of the set.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			DeferCleanup(cancel)

			msg, err := stream.Next(ctx)
			Expect(err).ToNot(HaveOccurred())
			ack, ok := msg.(*a2a.Ack)
			Expect(ok).To(BeTrue())

			reply := a2a.NewElicitReply("q-nobody-asked", a2a.AnswerConfirmed)
			reply.ID = a2a.NewID()
			reply.Request = ack.Request
			reply.Conversation = ack.Conversation
			reply.Time = time.Now().UTC()
			reply.Sender = a2a.Identity{Name: "caller1"}

			body, merr := json.Marshal(reply)
			Expect(merr).ToNot(HaveOccurred())

			answered, rerr := nc.Request(natstransport.ElicitSubject("agent1", ack.Request), body, 5*time.Second)
			Expect(rerr).ToNot(HaveOccurred())
			Expect(answered.Header.Get("Nats-Service-Error-Code")).To(Equal("404"))
		})
	})

	// The channel bounds the question itself, one window of request_timeout at a time,
	// which is what lets a caller restart it. A question outliving the window ends
	// differently for the gate than for a tool: the gate's call is left unanswered so the
	// resume asks again, and a tool's call is deferred so the answer can be supplied to it.
	Describe("A question nobody answers", func() {
		BeforeEach(func() {
			newChannel("        workers: 1\n        elicit: true\n")

			// The real window is request_timeout, shortened here so the spec does not sit
			// it out.
			ch.promptWait = 20 * time.Millisecond
		})

		It("Should abort an approval and defer the rest", func() {
			_, work := start()

			Expect(work.PromptWait).To(BeZero(), "the server bounds none of this channel's questions")
			Expect(work.PromptsMayBlock).To(BeTrue(), "the channel bounds them itself")

			prompter, ok := work.Prompter.(*elicitPrompter)
			Expect(ok).To(BeTrue())

			_, err := prompter.ApproveCommand(context.Background(), toolkit.GateRequest{Command: "stream rm", Display: "stream rm ORDERS"})
			Expect(err).To(MatchError(toolkit.ErrPromptAborted))

			_, err = prompter.Confirm(context.Background(), "Proceed?")
			Expect(err).To(MatchError(toolkit.ErrDeferredResult))

			_, err = prompter.Select(context.Background(), "Which one?", []string{"east"})
			Expect(err).To(MatchError(toolkit.ErrDeferredResult))

			_, err = prompter.Input(context.Background(), "Which subject?", "")
			Expect(err).To(MatchError(toolkit.ErrDeferredResult))
		})

		It("Should refuse an answer that arrives after the window closed", func() {
			stream, work := start()

			done := ask(func() (string, error) {
				_, err := work.Prompter.Confirm(context.Background(), "Proceed?")

				return "", err
			})

			asked := question(stream)

			var got struct {
				answer string
				err    error
			}
			Eventually(done, 5*time.Second).Should(Receive(&got))
			Expect(got.err).To(MatchError(toolkit.ErrDeferredResult))

			refused := refusalFor(asked.Request, a2a.NewWaitingAck(asked, "caller1"))
			Expect(refused).To(Equal("404"), "the question is gone")

			refused = refusalFor(asked.Request, a2a.NewConfirmReply(asked, "caller1", true))
			Expect(refused).To(Equal("404"), "and so is the answer behind it")
		})
	})

	// A caller with somebody in front of the question says so, which restarts the window
	// rather than answering it. It is evidence rather than a claim: a caller that stops
	// saying it loses the question one window later, exactly as one that never said it.
	Describe("A caller that says it is still waiting", func() {
		BeforeEach(func() {
			newChannel("        workers: 1\n        elicit: true\n")

			// Long enough that a slow machine does not close a window between the acks
			// below, short enough that three of them are not a wait anybody notices.
			ch.promptWait = 500 * time.Millisecond
		})

		It("Should tell the caller how long it has", func() {
			stream, work := start()

			done := ask(func() (string, error) {
				_, err := work.Prompter.Confirm(context.Background(), "Proceed?")

				return "", err
			})

			asked := question(stream)
			Expect(asked.WaitMS).To(Equal(int64(500)), "the window actually enforced")
			Expect(asked.AckInterval()).To(Equal(500 * time.Millisecond / 3))

			Eventually(done, 5*time.Second).Should(Receive())
		})

		It("Should hold the question open past the window while acks arrive", func() {
			stream, work := start()

			answered := ask(func() (string, error) {
				ok, err := work.Prompter.Confirm(context.Background(), "Proceed?")

				return fmt.Sprintf("%v", ok), err
			})

			asked := question(stream)

			// Three windows of elapsed time, each crossed on an ack. Without them the
			// question is given up on before the first sleep ends.
			for range 3 {
				time.Sleep(300 * time.Millisecond)

				ack := answer(asked.Request, a2a.NewWaitingAck(asked, "caller1"))
				Expect(ack.Accepted).To(BeTrue())
			}

			answer(asked.Request, a2a.NewConfirmReply(asked, "caller1", true))

			var got struct {
				answer string
				err    error
			}
			Eventually(answered, 5*time.Second).Should(Receive(&got))
			Expect(got.err).ToNot(HaveOccurred())
			Expect(got.answer).To(Equal("true"), "the answer rather than the acks before it")
		})

		It("Should deliver the answer that follows an ack rather than refusing it", func() {
			stream, work := start()

			answered := ask(func() (string, error) {
				ok, err := work.Prompter.Confirm(context.Background(), "Proceed?")

				return fmt.Sprintf("%v", ok), err
			})

			asked := question(stream)

			// Two acks back to back leave one queued, since the signal is one deep. The
			// second is still accepted, and neither takes the slot the answer needs.
			Expect(answer(asked.Request, a2a.NewWaitingAck(asked, "caller1")).Accepted).To(BeTrue())
			Expect(answer(asked.Request, a2a.NewWaitingAck(asked, "caller1")).Accepted).To(BeTrue(), "a duplicate reaches a question that is wide open")
			Expect(answer(asked.Request, a2a.NewConfirmReply(asked, "caller1", true)).Accepted).To(BeTrue())

			var got struct {
				answer string
				err    error
			}
			Eventually(answered, 5*time.Second).Should(Receive(&got))
			Expect(got.err).ToNot(HaveOccurred())
			Expect(got.answer).To(Equal("true"))
		})

		It("Should refuse an ack for a question it is not holding", func() {
			stream, _ := start()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			DeferCleanup(cancel)

			msg, err := stream.Next(ctx)
			Expect(err).ToNot(HaveOccurred())
			ack, ok := msg.(*a2a.Ack)
			Expect(ok).To(BeTrue())

			refused := refusalFor(ack.Request, a2a.NewElicitReply("q-nobody-asked", a2a.AnswerWaiting))
			Expect(refused).To(Equal("404"))
		})

		It("Should end a question the run was canceled under, acks notwithstanding", func() {
			stream, work := start()

			runCtx, endRun := context.WithCancel(context.Background())
			DeferCleanup(endRun)

			answered := ask(func() (string, error) {
				_, err := work.Prompter.Confirm(runCtx, "Proceed?")

				return "", err
			})

			asked := question(stream)
			Expect(answer(asked.Request, a2a.NewWaitingAck(asked, "caller1")).Accepted).To(BeTrue())

			endRun()

			var got struct {
				answer string
				err    error
			}
			Eventually(answered, 5*time.Second).Should(Receive(&got))
			Expect(got.err).To(MatchError(toolkit.ErrDeferredResult))
		})

		// A drain closes the endpoints and waits for the runs already under way without
		// canceling them, so a question that could be restarted forever would hold the
		// shutdown open with it.
		It("Should stop restarting the window once the channel is draining", func() {
			stream, work := start()

			answered := ask(func() (string, error) {
				_, err := work.Prompter.Confirm(context.Background(), "Proceed?")

				return "", err
			})

			asked := question(stream)
			Expect(ch.Close()).To(Succeed())

			// The ack is still delivered, since the task's own subscriptions outlive the
			// service registration. What it no longer does is buy another window.
			Expect(answer(asked.Request, a2a.NewWaitingAck(asked, "caller1")).Accepted).To(BeTrue())

			var got struct {
				answer string
				err    error
			}
			Eventually(answered, 5*time.Second).Should(Receive(&got))
			Expect(got.err).To(MatchError(toolkit.ErrDeferredResult))
		})
	})

	// A channel whose operator is attached holds a question open, and the run context is
	// what ends it. It ends the way an elapsed wait does, so an answer that arrives after
	// the operator went away still has somewhere to land.
	Describe("A run that ends while a question is outstanding", func() {
		BeforeEach(func() {
			newChannel("        workers: 1\n        elicit: true\n")
		})

		It("Should defer a tool's question and leave a gated call unanswered", func() {
			stream, work := start()

			prompter, ok := work.Prompter.(*elicitPrompter)
			Expect(ok).To(BeTrue())

			runCtx, endRun := context.WithCancel(context.Background())
			DeferCleanup(endRun)

			confirmed := ask(func() (string, error) {
				_, err := prompter.Confirm(runCtx, "Proceed?")

				return "", err
			})

			question(stream)
			endRun()

			var got struct {
				answer string
				err    error
			}
			Eventually(confirmed, 5*time.Second).Should(Receive(&got))
			Expect(got.err).To(MatchError(toolkit.ErrDeferredResult))

			gateCtx, endGateRun := context.WithCancel(context.Background())
			DeferCleanup(endGateRun)

			approved := ask(func() (string, error) {
				_, err := prompter.ApproveCommand(gateCtx, toolkit.GateRequest{Command: "stream rm", Display: "stream rm ORDERS"})

				return "", err
			})

			question(stream)
			endGateRun()

			Eventually(approved, 5*time.Second).Should(Receive(&got))
			Expect(got.err).To(MatchError(toolkit.ErrPromptAborted))
			Expect(got.err).ToNot(MatchError(toolkit.ErrDeferredResult))
		})
	})
})
