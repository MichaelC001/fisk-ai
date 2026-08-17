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

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/a2a"
	natstransport "github.com/choria-io/fisk-ai/internal/a2a/nats"
	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/serve"
	"github.com/choria-io/fisk-ai/internal/util"
)

// The specs here drive the channel the way a server does: take the work it produced,
// report an outcome against it, and read what the caller received off the wire. The run
// itself is never started, so what is under test is the endpoint rather than the loop.
var _ = Describe("The prompt channel", func() {
	var (
		ch     *Channel
		client *a2a.Client
	)

	// A channel per spec, since the identity is a queue group and a leftover
	// registration would answer the next spec's request.
	newChannel := func(workers int) {
		GinkgoHelper()

		built, err := NewFromConfig(promptsConfig(fmt.Sprintf("        workers: %d\n", workers)), ConfigOptions{Conns: provider, Logger: quietLogger()})
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(closeAll, built)

		ch = channelOf(built)
	}

	BeforeEach(func() {
		transport, err := a2a.NewTransport(config.A2ATransportName, provider, a2a.TransportConfig{Identity: "caller1", Timeout: 2 * time.Second})
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(transport.Close)

		client, err = a2a.NewClient(transport, "caller1")
		Expect(err).ToNot(HaveOccurred())
	})

	// bounded is a caller's context. The streaming binding requires a deadline on the
	// context a prompt is sent under, since the reply set has no other end.
	bounded := func() context.Context {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		DeferCleanup(cancel)

		return ctx
	}

	// send sends a prompt and returns the stream the caller reads its run on.
	send := func(req *a2a.Request) *a2a.TaskStream {
		GinkgoHelper()

		stream, err := client.Task(bounded(), "agent1", req)
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(stream.Close)

		return stream
	}

	// rawRequest assembles a request by hand, so a spec chooses the correlation id
	// rather than taking the fresh one the client stamps.
	rawRequest := func(id, prompt string) []byte {
		GinkgoHelper()

		req := a2a.NewRequest(prompt)
		req.ID = id
		req.Request = id
		req.Conversation = id
		req.Time = time.Now().UTC()
		req.Sender = a2a.Identity{Name: "caller1"}

		body, err := json.Marshal(req)
		Expect(err).ToNot(HaveOccurred())

		return body
	}

	// ackOf sends a body on the task subject and decodes the ack it answers with, which
	// is the one message a plain request-reply sees of the set.
	ackOf := func(body []byte) *a2a.Ack {
		GinkgoHelper()

		reply, err := nc.Request(natstransport.TaskSubject("agent1"), body, 5*time.Second)
		Expect(err).ToNot(HaveOccurred())

		msg, err := a2a.Decode(reply.Data)
		Expect(err).ToNot(HaveOccurred())

		ack, ok := msg.(*a2a.Ack)
		Expect(ok).To(BeTrue())

		return ack
	}

	// next reads one message of a reply set, failing rather than hanging when nothing
	// arrives.
	next := func(stream *a2a.TaskStream) any {
		GinkgoHelper()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		DeferCleanup(cancel)

		msg, err := stream.Next(ctx)
		Expect(err).ToNot(HaveOccurred())

		return msg
	}

	// takeWork waits for the work the channel produced from an admitted request.
	takeWork := func() *serve.Work {
		GinkgoHelper()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		DeferCleanup(cancel)

		work, err := ch.Next(ctx)
		Expect(err).ToNot(HaveOccurred())

		return work
	}

	report := func(work *serve.Work, out serve.Outcome) {
		GinkgoHelper()

		Expect(work.Done(context.Background(), out)).To(Succeed())
	}

	Describe("Admission", func() {
		It("Should ack a request, run it, and close the set with a result", func() {
			newChannel(1)

			stream := send(a2a.NewRequest("do the thing"))

			ack, ok := next(stream).(*a2a.Ack)
			Expect(ok).To(BeTrue())
			Expect(ack.Accepted).To(BeTrue())
			Expect(ack.Sequence).To(Equal(uint64(1)))

			work := takeWork()
			Expect(work.Prompt).To(Equal("do the thing"))
			Expect(work.Prompter).To(BeNil(), "this channel has nobody to ask, so gated tools are refused")
			Expect(work.Continue).To(BeNil(), "a prompt gets one turn")
			Expect(work.Checkpoint.ResumeID).To(Equal(work.ID))
			Expect(work.Checkpoint.CreateIfMissing).To(BeTrue())
			Expect(work.Caller.Name).To(Equal("caller1"))
			Expect(work.Caller.Verified).To(BeFalse(), "NATS vouches for nobody")

			report(work, serve.Outcome{
				Reason: runstate.ReasonCompleted,
				Text:   "did the thing",
				Stats:  &util.RunStats{InTokens: 3, OutTokens: 4, LlmCalls: 1},
			})

			res, ok := next(stream).(*a2a.Result)
			Expect(ok).To(BeTrue())
			Expect(res.Text).To(Equal("did the thing"))
			Expect(res.StopReason).To(Equal(a2a.StopEndTurn))
			Expect(res.Usage.OutputTokens).To(Equal(int64(4)))
		})

		It("Should carry a caller's budget for the server to clamp", func() {
			newChannel(1)

			req := a2a.NewRequest("do the thing")
			req.Budget = &a2a.Budget{MaxTokens: 10, MaxIterations: 2, CallTimeout: "1m"}
			send(req)

			work := takeWork()
			Expect(work.Budget).To(Equal(serve.Budget{MaxTokens: 10, MaxIterations: 2}), "call_timeout has nowhere to go")
		})

		It("Should refuse a body that is not a request without acking it", func() {
			newChannel(1)

			reply, err := nc.Request(natstransport.TaskSubject("agent1"), []byte(`{"protocol":"io.choria.fisk-ai.v1.cancel"}`), 5*time.Second)
			Expect(err).ToNot(HaveOccurred())
			Expect(reply.Header.Get("Nats-Service-Error-Code")).To(Equal("400"))
			Expect(reply.Data).To(BeEmpty(), "nothing was accepted, so there is no reply set")
		})

		// A refusal is an ack that says no and then a terminal message, since the ack does
		// not close the set: a caller holding only a refusing ack would wait for a terminal
		// message to its own deadline.
		It("Should refuse a prompt over its worker count with a code the caller can act on", func() {
			newChannel(1)

			first := send(a2a.NewRequest("first"))
			Expect(next(first).(*a2a.Ack).Accepted).To(BeTrue())
			work := takeWork()

			second := send(a2a.NewRequest("second"))

			ack, ok := next(second).(*a2a.Ack)
			Expect(ok).To(BeTrue())
			Expect(ack.Accepted).To(BeFalse())
			Expect(ack.Reason).To(ContainSubstring("maximum of 1 prompts"))

			failed, ok := next(second).(*a2a.ErrorMessage)
			Expect(ok).To(BeTrue())
			Expect(failed.Code).To(Equal(codeCapacity))

			// The slot comes back when the outcome is reported, so the next caller is taken.
			report(work, serve.Outcome{Reason: runstate.ReasonCompleted})
			Expect(next(first)).To(BeAssignableToTypeOf(&a2a.Result{}))

			third := send(a2a.NewRequest("third"))
			Expect(next(third).(*a2a.Ack).Accepted).To(BeTrue())
		})

		// The request id addresses the cancel subscription, so two runs sharing one would
		// both hear a cancel meant for one of them, and the ack a caller read back would
		// come from whichever answered first. The id is sent raw because Client.Task
		// stamps a fresh one on every call, which is what a well-behaved caller does.
		It("Should refuse a request id already running here", func() {
			newChannel(2)

			body := rawRequest("dup1", "first")

			Expect(ackOf(body).Accepted).To(BeTrue())
			takeWork()

			second := ackOf(body)
			Expect(second.Accepted).To(BeFalse())
			Expect(second.Reason).To(ContainSubstring("already in flight here"))
		})

		// Every ending releases, including the ending of a request that was never
		// admitted, and a duplicate carries the running task's id. Releasing on the id
		// alone would hand back a slot this task never held.
		It("Should keep the running task's slot when a duplicate is refused", func() {
			newChannel(1)

			body := rawRequest("dup1", "first")
			Expect(ackOf(body).Accepted).To(BeTrue())
			takeWork()

			Expect(ackOf(body).Accepted).To(BeFalse())

			// The only slot is still held, so the next caller is refused at capacity
			// rather than admitted over the running run.
			third := ackOf(rawRequest("other1", "second"))
			Expect(third.Accepted).To(BeFalse())
			Expect(third.Reason).To(ContainSubstring("maximum of 1 prompts"))
		})
	})

	Describe("Follow-up turns", func() {
		It("Should hand back a token on a first turn and resume its journal on the next", func() {
			newChannel(1)

			first := send(a2a.NewRequest("how many streams are there"))

			ack, ok := next(first).(*a2a.Ack)
			Expect(ok).To(BeTrue())
			Expect(ack.Accepted).To(BeTrue())
			Expect(ack.ConversationToken).ToNot(BeEmpty())

			opening := takeWork()
			Expect(opening.Checkpoint.CreateIfMissing).To(BeTrue(), "a first turn creates the journal")
			Expect(opening.Checkpoint.FollowUp).To(BeFalse())
			// The journal is the hash of the token, so a caller's bytes never become a
			// store key and no journal is reachable by knowing one.
			Expect(opening.Checkpoint.ResumeID).To(HavePrefix("t-"))
			Expect(opening.Checkpoint.ResumeID).ToNot(ContainSubstring(ack.ConversationToken))

			report(opening, serve.Outcome{Reason: runstate.ReasonCompleted, Text: "there are three"})
			Expect(next(first)).To(BeAssignableToTypeOf(&a2a.Result{}))

			// The next turn carries the token back and lands in the same journal.
			second := send(a2a.NewFollowUp(ack, "what is the first one called"))

			echoed, ok := next(second).(*a2a.Ack)
			Expect(ok).To(BeTrue())
			Expect(echoed.Accepted).To(BeTrue())
			Expect(echoed.ConversationToken).To(Equal(ack.ConversationToken), "a caller reads back which conversation it is on")

			turn := takeWork()
			Expect(turn.Prompt).To(Equal("what is the first one called"))
			Expect(turn.Checkpoint.ResumeID).To(Equal(opening.Checkpoint.ResumeID))
			Expect(turn.Checkpoint.FollowUp).To(BeTrue())
			Expect(turn.Checkpoint.CreateIfMissing).To(BeFalse(), "a token naming no journal is refused rather than creating one")

			report(turn, serve.Outcome{Reason: runstate.ReasonCompleted, Text: "the first is ORDERS", FollowUpTaken: true})

			res, ok := next(second).(*a2a.Result)
			Expect(ok).To(BeTrue())
			Expect(res.Text).To(Equal("the first is ORDERS"))
		})

		// Both turns would resume one journal, and the second to claim it takes it while
		// the first fails at its next append. A sibling worker would do the same, so the
		// caller is told to wait rather than to try elsewhere.
		It("Should refuse a turn of a conversation it is already running", func() {
			newChannel(2)

			first := send(a2a.NewRequest("how many streams are there"))
			ack, ok := next(first).(*a2a.Ack)
			Expect(ok).To(BeTrue())
			takeWork()

			second := send(a2a.NewFollowUp(ack, "and the other one"))

			refused, ok := next(second).(*a2a.Ack)
			Expect(ok).To(BeTrue())
			Expect(refused.Accepted).To(BeFalse())

			failed, ok := next(second).(*a2a.ErrorMessage)
			Expect(ok).To(BeTrue())
			Expect(failed.Code).To(Equal(codeConversationBusy))
			Expect(failed.Err).To(ContainSubstring("wait for its terminal message"))
		})

		DescribeTable("Should tell a caller what became of its turn",
			func(followUp bool, out serve.Outcome, code string) {
				newChannel(1)

				req := a2a.NewRequest("and the other one")
				if followUp {
					req.ConversationToken = "2Ab3Cd4Ef5Gh"
				}
				stream := send(req)
				Expect(next(stream).(*a2a.Ack).Accepted).To(BeTrue())

				report(takeWork(), out)

				failed, ok := next(stream).(*a2a.ErrorMessage)
				Expect(ok).To(BeTrue())
				Expect(failed.Code).To(Equal(code))
			},
			Entry("a token naming no journal", true,
				serve.Outcome{Err: fmt.Errorf("resuming: %w", agent.ErrConversationNotFound)}, codeUnknownConversation),
			Entry("a conversation that could not take the turn", true,
				serve.Outcome{Reason: runstate.ReasonSuspended, Deferred: []agent.DeferredCall{{ToolUseID: "c1", ToolName: "change_request"}}}, codeTurnNotTaken),
			Entry("a first turn that deferred, which is not a turn refused", false,
				serve.Outcome{Reason: runstate.ReasonSuspended, Deferred: []agent.DeferredCall{{ToolUseID: "c1", ToolName: "change_request"}}}, codeDeferred),
		)
	})

	Describe("Endings", func() {
		DescribeTable("Should tell a caller how the run ended",
			func(out serve.Outcome, code string, reason a2a.StopReason) {
				newChannel(1)

				stream := send(a2a.NewRequest("do the thing"))
				Expect(next(stream).(*a2a.Ack).Accepted).To(BeTrue())

				report(takeWork(), out)

				failed, ok := next(stream).(*a2a.ErrorMessage)
				Expect(ok).To(BeTrue())
				Expect(failed.Code).To(Equal(code))
				Expect(failed.StopReason).To(Equal(reason))
			},
			Entry("a failure", serve.Outcome{Reason: runstate.ReasonError, Err: fmt.Errorf("the model refused")}, codeFailed, a2a.StopError),
			Entry("a suspend", serve.Outcome{Reason: runstate.ReasonSuspended}, codeSuspended, a2a.StopSuspended),
			Entry("work never started", serve.Outcome{Abandoned: true}, codeNotStarted, a2a.StopError),
			Entry("a run that reached no outcome", serve.Outcome{}, codeFailed, a2a.StopError),
		)

		It("Should tell a caller a run crashed and nothing of the stack", func() {
			newChannel(1)

			stream := send(a2a.NewRequest("do the thing"))
			Expect(next(stream).(*a2a.Ack).Accepted).To(BeTrue())

			report(takeWork(), serve.Outcome{Crashed: true, Err: fmt.Errorf("internal error at /home/rip/go/src/agent.go:41")})

			failed, ok := next(stream).(*a2a.ErrorMessage)
			Expect(ok).To(BeTrue())
			Expect(failed.Code).To(Equal(codeCrashed))
			Expect(failed.Err).To(Equal("the run crashed"))
			Expect(failed.Err).ToNot(ContainSubstring("/home/rip"))
		})

		// Nothing wakes a deferred task, so the ids an answer is supplied against travel to
		// the caller with the session that holds them.
		It("Should name the session and the calls a deferred run is waiting on", func() {
			newChannel(1)

			stream := send(a2a.NewRequest("do the thing"))
			Expect(next(stream).(*a2a.Ack).Accepted).To(BeTrue())

			work := takeWork()
			report(work, serve.Outcome{
				Reason:   runstate.ReasonSuspended,
				Deferred: []agent.DeferredCall{{ToolUseID: "toolu_1", ToolName: "ask", Note: "waiting on a human"}},
			})

			failed, ok := next(stream).(*a2a.ErrorMessage)
			Expect(ok).To(BeTrue())
			Expect(failed.Code).To(Equal(codeDeferred))
			Expect(failed.Err).To(ContainSubstring(work.ID))
			Expect(failed.Err).To(ContainSubstring("toolu_1"))
			Expect(failed.Err).ToNot(ContainSubstring("waiting on a human"), "the tool's own text does not travel")
		})

		// An accepted task always reaches an ending, including one the server never had a
		// chance to run.
		It("Should end a prompt accepted while the worker was stopping", func() {
			newChannel(1)

			stream := send(a2a.NewRequest("do the thing"))
			Expect(next(stream).(*a2a.Ack).Accepted).To(BeTrue())

			Expect(ch.Close()).To(Succeed())

			failed, ok := next(stream).(*a2a.ErrorMessage)
			Expect(ok).To(BeTrue())
			Expect(failed.Code).To(Equal(codeDraining))
		})
	})

	Describe("Canceling", func() {
		It("Should cancel the run and answer the caller", func() {
			newChannel(1)

			req := a2a.NewRequest("do the thing")
			stream := send(req)
			Expect(next(stream).(*a2a.Ack).Accepted).To(BeTrue())

			work := takeWork()
			runCtx, cancel := work.RunContext(context.Background())
			DeferCleanup(cancel)
			Expect(runCtx.Err()).To(BeNil())

			ack, err := client.Cancel(context.Background(), "agent1", req.Request, "changed my mind")
			Expect(err).ToNot(HaveOccurred())
			Expect(ack.Accepted).To(BeTrue())

			Eventually(runCtx.Done()).Should(BeClosed())

			report(work, serve.Outcome{Err: context.Canceled})

			failed, ok := next(stream).(*a2a.ErrorMessage)
			Expect(ok).To(BeTrue())
			Expect(failed.Code).To(Equal(codeCanceled))
			Expect(failed.StopReason).To(Equal(a2a.StopCanceled))
		})

		// Between the ack and the run's start there is no context to cancel, so the task
		// records the cancel and the run is entered on a context that has already ended.
		It("Should honor a cancel that arrives before the run starts", func() {
			newChannel(1)

			req := a2a.NewRequest("do the thing")
			stream := send(req)
			Expect(next(stream).(*a2a.Ack).Accepted).To(BeTrue())

			work := takeWork()

			_, err := client.Cancel(context.Background(), "agent1", req.Request, "too slow")
			Expect(err).ToNot(HaveOccurred())

			runCtx, cancel := work.RunContext(context.Background())
			DeferCleanup(cancel)
			Expect(runCtx.Err()).To(MatchError(context.Canceled))
		})

		// The subscription is the run's own, so a cancel for a run that is not going
		// here reaches nobody, which separates a delivered cancel from one that found
		// nothing.
		It("Should leave a cancel for an unknown task unanswered", func() {
			newChannel(1)

			_, err := client.Cancel(context.Background(), "agent1", a2a.NewID(), "whatever")
			Expect(err).To(MatchError(a2a.ErrAgentUnavailable))
		})

		It("Should release the cancel address when the task ends", func() {
			newChannel(1)

			req := a2a.NewRequest("do the thing")
			stream := send(req)
			Expect(next(stream).(*a2a.Ack).Accepted).To(BeTrue())

			report(takeWork(), serve.Outcome{Reason: runstate.ReasonCompleted})
			Expect(next(stream)).To(BeAssignableToTypeOf(&a2a.Result{}))

			_, err := client.Cancel(context.Background(), "agent1", req.Request, "too late")
			Expect(err).To(MatchError(a2a.ErrAgentUnavailable))
		})
	})

	Describe("The event stream", func() {
		It("Should stream what the run narrates and end with the answer", func() {
			newChannel(1)

			stream := send(a2a.NewRequest("do the thing"))
			Expect(next(stream).(*a2a.Ack).Accepted).To(BeTrue())

			work := takeWork()
			Expect(work.Events).ToNot(BeNil())

			work.Events.ToolCall(agent.ToolTrace{ID: "toolu_1", Name: "backup", Input: []byte(`{"target":"orders"}`)})
			work.Events.ToolResult(agent.ToolResultTrace{CallID: "toolu_1", Output: "backed up"})

			call, ok := next(stream).(*a2a.Event)
			Expect(ok).To(BeTrue())
			Expect(call.Block.Type()).To(Equal(a2a.BlockToolCall))
			Expect(call.Block.Content().(a2a.ToolCallBlock).ID).To(Equal("toolu_1"))

			result, ok := next(stream).(*a2a.Event)
			Expect(ok).To(BeTrue())
			Expect(result.Block.Content().(a2a.ToolResultBlock).CallID).To(Equal("toolu_1"), "a result answers the call it names")

			report(work, serve.Outcome{Reason: runstate.ReasonCompleted, Text: "done"})
			Expect(next(stream)).To(BeAssignableToTypeOf(&a2a.Result{}))
			Expect(stream.Gaps()).To(Equal(uint64(0)))
		})

		// A caller that asked for no stream still gets the ack and the answer, which is
		// the whole of what it asked for.
		It("Should send no events to a caller that asked for none", func() {
			newChannel(1)

			req := a2a.NewRequest("do the thing")
			no := false
			req.Stream = &no

			stream := send(req)
			Expect(next(stream).(*a2a.Ack).Accepted).To(BeTrue())

			work := takeWork()
			Expect(work.Events).To(BeNil())

			report(work, serve.Outcome{Reason: runstate.ReasonCompleted, Text: "done"})
			Expect(next(stream)).To(BeAssignableToTypeOf(&a2a.Result{}))
		})
	})
})
