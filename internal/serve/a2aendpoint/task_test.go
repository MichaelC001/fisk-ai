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
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/serve"
	"github.com/choria-io/fisk-ai/internal/toolkit"
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
		transport, err := a2a.NewTransport(config.A2ATransportName, a2a.TransportConfig{Resources: provider, Identity: "caller1", Timeout: 2 * time.Second})
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

	// read builds a request that asks for a conversation back, whose replay count the
	// constructor bounds.
	read := func(token string, replay int) *a2a.Request {
		GinkgoHelper()

		req, err := a2a.NewRead(token, replay)
		Expect(err).ToNot(HaveOccurred())

		return req
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

	// refuse sends a request the worker is expected to turn down at intake and returns
	// what it said, which reaches the caller as a transport error since nothing was
	// accepted and there is no reply set to end.
	refuse := func(req *a2a.Request) string {
		GinkgoHelper()

		req.ID = a2a.NewID()
		req.Request = req.ID
		req.Conversation = req.ID
		req.Time = time.Now().UTC()
		req.Sender = a2a.Identity{Name: "caller1"}

		body, err := json.Marshal(req)
		Expect(err).ToNot(HaveOccurred())

		reply, err := nc.Request(natstransport.TaskSubject("agent1"), body, 5*time.Second)
		Expect(err).ToNot(HaveOccurred())
		Expect(reply.Header.Get("Nats-Service-Error-Code")).To(Equal("400"))

		return reply.Header.Get("Nats-Service-Error")
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
			Expect(work.HumanPaced).To(BeTrue(), "the next turn is the token holder's to send, at their pace")
			Expect(work.Checkpoint.ResumeID).To(Equal(work.ID))
			Expect(work.Checkpoint.CreateIfMissing).To(BeTrue())
			Expect(work.Caller.Name).To(Equal("caller1"))
			Expect(work.Caller.Verified).To(BeFalse(), "NATS vouches for nobody")

			report(work, serve.Outcome{
				Reason: runstate.ReasonCompleted,
				Text:   "did the thing",
				Stats:  &agent.RunStats{InTokens: 3, OutTokens: 4, LlmCalls: 1},
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
			// The turn that creates the journal is the one that records the token in it,
			// so a caller that loses the ack can be handed it back from the store.
			Expect(opening.Checkpoint.ConversationToken).To(Equal(ack.ConversationToken))

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
			Expect(turn.Checkpoint.ConversationToken).To(BeEmpty(), "the journal recorded it when it was created")

			report(turn, serve.Outcome{Reason: runstate.ReasonCompleted, Text: "the first is ORDERS", FollowUpTaken: true})

			res, ok := next(second).(*a2a.Result)
			Expect(ok).To(BeTrue())
			Expect(res.Text).To(Equal("the first is ORDERS"))
		})

		// A person who was asked something and could not answer in time answers on a
		// request of its own, which resumes the conversation and adds no turn to it.
		It("Should resume the conversation an answer names without adding a turn", func() {
			newChannel(1)

			first := send(a2a.NewRequest("delete the stream"))
			ack := next(first).(*a2a.Ack)
			opening := takeWork()
			report(opening, serve.Outcome{Reason: runstate.ReasonSuspended})
			Expect(next(first)).To(BeAssignableToTypeOf(&a2a.ErrorMessage{}))

			asked := a2a.NewElicitRequest(a2a.ElicitInput, "q1")
			asked.ToolUseID = "toolu_1"

			answering, err := a2a.NewAnsweringRequest(ack.ConversationToken, asked, a2a.NewInputReply(asked, "caller1", "ORDERS"))
			Expect(err).ToNot(HaveOccurred())

			second := send(answering)
			Expect(next(second).(*a2a.Ack).Accepted).To(BeTrue())

			resumed := takeWork()
			Expect(resumed.Prompt).To(BeEmpty(), "an answer is not a prompt")
			Expect(resumed.Checkpoint.ResumeID).To(Equal(opening.Checkpoint.ResumeID))
			Expect(resumed.Checkpoint.FollowUp).To(BeFalse(), "it adds no turn")
			Expect(resumed.Checkpoint.CreateIfMissing).To(BeFalse())
			Expect(resumed.Checkpoint.ConversationToken).To(BeEmpty(), "the journal recorded it when it was created")
			Expect(resumed.Checkpoint.Answer).To(Equal(&agent.DeferredAnswer{ToolUseID: "toolu_1", Content: `{"value":"ORDERS"}`}))

			// It is not reported as a turn that was not taken, which is what a follow-up
			// that reached no boundary would be.
			report(resumed, serve.Outcome{Reason: runstate.ReasonCompleted, Text: "deleted"})

			res, ok := next(second).(*a2a.Result)
			Expect(ok).To(BeTrue())
			Expect(res.Text).To(Equal("deleted"))
		})

		// The gate asks about the same call again on the resume, so nothing is written
		// and nothing is stored: the answer waits in the prompter for that question.
		It("Should answer the approval the resumed run asks about again", func() {
			newChannel(1)
			// An approval is answered through the prompter, which a channel that asks
			// its callers nothing does not have.
			ch.elicits = true

			first := send(a2a.NewRequest("delete the stream"))
			ack := next(first).(*a2a.Ack)
			report(takeWork(), serve.Outcome{Reason: runstate.ReasonSuspended})
			Expect(next(first)).To(BeAssignableToTypeOf(&a2a.ErrorMessage{}))

			asked := a2a.NewElicitRequest(a2a.ElicitApprove, "q1")
			asked.ToolUseID = "toolu_1"
			asked.Command = "stream rm"

			answering, err := a2a.NewAnsweringRequest(ack.ConversationToken, asked, a2a.NewApproveReply(asked, "caller1", a2a.ChoiceOnce))
			Expect(err).ToNot(HaveOccurred())

			second := send(answering)
			Expect(next(second).(*a2a.Ack).Accepted).To(BeTrue())

			resumed := takeWork()
			Expect(resumed.Checkpoint.Answer).To(BeNil(), "an approval writes nothing to the journal")

			choice, err := resumed.Prompter.ApproveCommand(context.Background(), toolkit.GateRequest{ToolUseID: "toolu_1", Command: "stream rm"})
			Expect(err).ToNot(HaveOccurred())
			Expect(choice).To(Equal(toolkit.ConfirmOnce), "the question is answered from what the caller sent")

			report(resumed, serve.Outcome{Reason: runstate.ReasonCompleted, Text: "deleted"})
			Expect(next(second)).To(BeAssignableToTypeOf(&a2a.Result{}))
		})

		It("Should spend a held approval on the call it was given for and no other", func() {
			newChannel(1)
			ch.elicits = true

			first := send(a2a.NewRequest("delete the streams"))
			ack := next(first).(*a2a.Ack)
			report(takeWork(), serve.Outcome{Reason: runstate.ReasonSuspended})
			Expect(next(first)).To(BeAssignableToTypeOf(&a2a.ErrorMessage{}))

			asked := a2a.NewElicitRequest(a2a.ElicitApprove, "q1")
			asked.ToolUseID = "toolu_1"

			answering, err := a2a.NewAnsweringRequest(ack.ConversationToken, asked, a2a.NewApproveReply(asked, "caller1", a2a.ChoiceOnce))
			Expect(err).ToNot(HaveOccurred())

			second := send(answering)
			Expect(next(second).(*a2a.Ack).Accepted).To(BeTrue())

			resumed := takeWork()
			prompter := resumed.Prompter.(*elicitPrompter)

			_, held := prompter.heldFor("toolu_2")
			Expect(held).To(BeFalse(), "another call is not what was answered")

			_, held = prompter.heldFor("toolu_1")
			Expect(held).To(BeTrue())

			_, held = prompter.heldFor("toolu_1")
			Expect(held).To(BeFalse(), "and it authorizes one dispatch")

			report(resumed, serve.Outcome{Reason: runstate.ReasonCompleted})
			Expect(next(second)).To(BeAssignableToTypeOf(&a2a.Result{}))
		})

		// Each id states its own shape and refuses the fields belonging to its siblings,
		// so a body that disagrees with the id it arrived under never reaches the run.
		It("Should refuse a body that does not fit the id it arrived under", func() {
			newChannel(1)

			answer := &a2a.Answer{ToolUseID: "toolu_1", Kind: a2a.ElicitInput, Answer: a2a.AnswerValue, Value: "ORDERS"}

			untokened := a2a.NewAnswerRequest("", answer)
			Expect(refuse(untokened)).To(ContainSubstring("conversation_token"))

			both := a2a.NewRequest("do the thing")
			both.ConversationToken = "t1"
			both.Answer = answer
			Expect(refuse(both)).To(ContainSubstring("answer"))

			empty := a2a.NewRequest("")
			Expect(refuse(empty)).To(ContainSubstring("prompt"))

			// A resume carrying a replay count is a read, and the two are separate
			// operations.
			replaying := a2a.NewResume("t1")
			replaying.Replay = 10
			Expect(refuse(replaying)).To(ContainSubstring("replay"))
		})

		// A caller that was suspended part way through a turn continues it rather than
		// adding to it, so the request carries a conversation and nothing else.
		It("Should take a request that continues a run and adds nothing", func() {
			newChannel(1)

			first := send(a2a.NewRequest("how many streams are there"))
			ack, ok := next(first).(*a2a.Ack)
			Expect(ok).To(BeTrue())

			opening := takeWork()
			report(opening, serve.Outcome{Reason: runstate.ReasonSuspended})
			Expect(next(first)).To(BeAssignableToTypeOf(&a2a.ErrorMessage{}))

			second := send(a2a.NewResume(ack.ConversationToken))
			Expect(next(second).(*a2a.Ack).Accepted).To(BeTrue())

			resumed := takeWork()
			Expect(resumed.Prompt).To(BeEmpty())
			Expect(resumed.Checkpoint.ResumeID).To(Equal(opening.Checkpoint.ResumeID))
			Expect(resumed.Checkpoint.FollowUp).To(BeFalse(), "it adds no turn")
			Expect(resumed.Checkpoint.CreateIfMissing).To(BeFalse())
			Expect(resumed.Checkpoint.Answer).To(BeNil())
		})

		// A caller resuming across a configuration its conversation no longer matches
		// asks for it, and the run drops what it cannot vouch for.
		It("Should carry force onto a resume", func() {
			newChannel(1)

			first := send(a2a.NewRequest("how many streams are there"))
			ack := next(first).(*a2a.Ack)
			report(takeWork(), serve.Outcome{Reason: runstate.ReasonSuspended})
			Expect(next(first)).To(BeAssignableToTypeOf(&a2a.ErrorMessage{}))

			forced := a2a.NewFollowUp(ack, "and the second one")
			forced.Force = true
			Expect(next(send(forced)).(*a2a.Ack).Accepted).To(BeTrue())

			Expect(takeWork().Checkpoint.Force).To(BeTrue())
		})

		It("Should refuse an answer whose value does not fit the question it names", func() {
			newChannel(1)

			mismatched := a2a.NewAnswerRequest("t1", &a2a.Answer{
				ToolUseID: "toolu_1", Kind: a2a.ElicitApprove, Answer: a2a.AnswerValue, Value: "yes",
			})

			Expect(refuse(mismatched)).To(ContainSubstring("an approval is answered with a choice"))
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
			// A budget refusal also leaves FollowUpTaken false, so without a case of its
			// own it would take the deferred-tool branch above and tell the caller to send
			// the prompt again once something that does not exist has been answered.
			Entry("a conversation that has spent its token budget", true,
				serve.Outcome{Reason: runstate.ReasonBudget, Err: fmt.Errorf("this conversation has processed 210 of its 200 token budget (llm.budget.max_tokens)")}, codeBudgetExhausted),
		)

		It("Should tell a caller a spent budget is permanent, and say what it spent", func() {
			newChannel(1)

			req := a2a.NewRequest("and the other one")
			req.ConversationToken = "2Ab3Cd4Ef5Gh"
			stream := send(req)
			Expect(next(stream).(*a2a.Ack).Accepted).To(BeTrue())

			report(takeWork(), serve.Outcome{
				Reason: runstate.ReasonBudget,
				Err:    fmt.Errorf("this conversation has processed 210 of its 200 token budget (llm.budget.max_tokens)"),
			})

			failed, ok := next(stream).(*a2a.ErrorMessage)
			Expect(ok).To(BeTrue())
			Expect(failed.Code).To(Equal(codeBudgetExhausted))

			// Both numbers and the key reach the caller, and the advice is the only one
			// that works: this conversation is finished, whoever sends the next turn.
			Expect(failed.Err).To(ContainSubstring("processed 210 of its 200 token budget"))
			Expect(failed.Err).To(ContainSubstring("llm.budget.max_tokens"))
			Expect(failed.Err).To(ContainSubstring("no further turn"))
			Expect(failed.Err).ToNot(ContainSubstring("deferred"))
			Expect(failed.StopReason).To(Equal(a2a.StopBudgetExhausted))
		})
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
			Expect(failed.Err).To(ContainSubstring("toolu_1"), "the call an answer names")
			Expect(failed.Err).ToNot(ContainSubstring(work.ID), "the caller answers on its token, so the session stays out of it")
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
		// A cancel asks for a boundary rather than a stop: the run is left to finish the
		// step in hand and park somewhere a resume can continue from, so the caller is
		// answered suspended and keeps a conversation rather than half a turn.
		It("Should ask the run to stop at its next boundary", func() {
			newChannel(1)

			req := a2a.NewRequest("do the thing")
			stream := send(req)
			Expect(next(stream).(*a2a.Ack).Accepted).To(BeTrue())

			work := takeWork()
			runCtx, cancel := work.RunContext(context.Background())
			DeferCleanup(cancel)
			Expect(work.SuspendRequested()).To(BeFalse())

			ack, err := client.Cancel(context.Background(), "agent1", req.Request, "changed my mind")
			Expect(err).ToNot(HaveOccurred())
			Expect(ack.Accepted).To(BeTrue())

			Eventually(work.SuspendRequested).Should(BeTrue())
			Expect(runCtx.Err()).To(BeNil(), "the run is asked to stop, not stopped where it stands")

			report(work, serve.Outcome{Reason: runstate.ReasonSuspended})

			failed, ok := next(stream).(*a2a.ErrorMessage)
			Expect(ok).To(BeTrue())
			Expect(failed.Code).To(Equal(codeSuspended))
		})

		// Between the ack and the run's start there is nothing to poll, so the flag is
		// what carries the cancel: the loop reads it at the boundary before its first
		// model call and parks without spending anything.
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
			Expect(runCtx.Err()).To(BeNil())
			Expect(work.SuspendRequested()).To(BeTrue())
		})

		// The worker stopping a run under its caller is the ending that is still a
		// cancel: nothing was asked for, the run ended where it stood, and what it left
		// is whatever the journal holds.
		It("Should report a worker that stopped the run as canceled", func() {
			newChannel(1)

			stream := send(a2a.NewRequest("do the thing"))
			Expect(next(stream).(*a2a.Ack).Accepted).To(BeTrue())

			report(takeWork(), serve.Outcome{Err: context.Canceled})

			failed, ok := next(stream).(*a2a.ErrorMessage)
			Expect(ok).To(BeTrue())
			Expect(failed.Code).To(Equal(codeCanceled))
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

	// A read is a request that names a conversation and asks for some of it back,
	// carrying neither a prompt nor an answer. It is what a client opens a resumed
	// conversation with: a completed one refuses a plain resume, and a request that
	// carried a prompt would put the history underneath it.
	Describe("Reading a conversation back", func() {
		// withStore builds a channel holding a store, and seeds one stored conversation
		// under the session id the given token names.
		withStore := func(token string) *agenttest.FakeSessionStore {
			GinkgoHelper()

			store := agenttest.NewFakeSessionStore(GinkgoTB())

			built, err := NewFromConfig(promptsConfig("        workers: 1\n"), ConfigOptions{Conns: provider, Logger: quietLogger(), Sessions: store})
			Expect(err).ToNot(HaveOccurred())
			DeferCleanup(closeAll, built)

			ch = channelOf(built)

			id := SessionFor("agent1", token)
			j, err := store.Create(context.Background(), id, runstate.MetaRecord{Version: runstate.Version, RunID: id, Prompt: "how many streams"})
			Expect(err).ToNot(HaveOccurred())

			Expect(j.Append(context.Background(), 2, runstate.Record{Protocol: runstate.AssistantProtocol, Seq: 2, Assistant: &runstate.AssistantRecord{
				Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
					{Text: &llm.TextBlock{Text: "there are three"}},
				}},
			}})).To(Succeed())
			Expect(j.Append(context.Background(), 3, runstate.Record{Protocol: runstate.TerminalProtocol, Seq: 3, Terminal: &runstate.TerminalRecord{Reason: runstate.ReasonCompleted}})).To(Succeed())
			Expect(j.Close()).To(Succeed())

			return store
		}

		It("Should replay the conversation and answer without running a turn", func() {
			withStore("tok1")

			stream := send(read("tok1", 50))

			ack, ok := next(stream).(*a2a.Ack)
			Expect(ok).To(BeTrue())
			Expect(ack.Accepted).To(BeTrue())
			Expect(ack.ConversationToken).To(Equal("tok1"))

			start, ok := next(stream).(*a2a.Event)
			Expect(ok).To(BeTrue())
			Expect(start.Block.Content().(a2a.StatusBlock).Phase).To(Equal(a2a.PhaseReplayStart))

			prompt, ok := next(stream).(*a2a.Event)
			Expect(ok).To(BeTrue())
			Expect(prompt.Block.Content().(a2a.PromptBlock).Text).To(Equal("how many streams"))

			answer, ok := next(stream).(*a2a.Event)
			Expect(ok).To(BeTrue())
			Expect(answer.Block.Content().(a2a.TextBlock).Text).To(Equal("there are three"))

			end, ok := next(stream).(*a2a.Event)
			Expect(ok).To(BeTrue())
			Expect(end.Block.Content().(a2a.StatusBlock).Phase).To(Equal(a2a.PhaseReplayEnd))
			Expect(end.Block.Content().(a2a.StatusBlock).Usage).ToNot(BeNil(), "a client seeds its running total from here")

			res, ok := next(stream).(*a2a.Result)
			Expect(ok).To(BeTrue())
			Expect(res.StopReason).To(Equal(a2a.StopEndTurn))
			Expect(res.Text).To(BeEmpty(), "a read answers nothing, it only says what is there")
		})

		// The whole point of the shape: the conversation above has completed, which is
		// what every finished turn leaves behind, and no other request could open on it.
		It("Should read a conversation no plain resume could continue", func() {
			withStore("tok2")

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			DeferCleanup(cancel)

			// No work is ever produced, so the channel's worker is free throughout.
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, err := ch.Next(ctx)
				Expect(err).To(HaveOccurred())
			}()

			stream := send(read("tok2", 50))

			Expect(next(stream).(*a2a.Ack).Accepted).To(BeTrue())

			for {
				msg := next(stream)
				if _, ok := msg.(*a2a.Result); ok {
					break
				}
			}

			cancel()
			Eventually(done, 5*time.Second).Should(BeClosed())
		})

		It("Should say so when the token names no conversation", func() {
			withStore("tok3")

			stream := send(read("nosuchtoken", 50))

			Expect(next(stream).(*a2a.Ack).Accepted).To(BeTrue())

			msg, ok := next(stream).(*a2a.ErrorMessage)
			Expect(ok).To(BeTrue())
			Expect(msg.Code).To(Equal(a2a.CodeUnknownConversation))
		})

		// A resume that asks for no history is the other operation: continue a run that
		// stopped part way. It still reaches the worker.
		It("Should leave a plain resume as work for the run", func() {
			withStore("tok4")

			stream := send(a2a.NewResume("tok4"))

			Expect(next(stream).(*a2a.Ack).Accepted).To(BeTrue())

			work := takeWork()
			Expect(work.Prompt).To(BeEmpty())
			Expect(work.Checkpoint.ResumeID).To(Equal(SessionFor("agent1", "tok4")))

			report(work, serve.Outcome{Reason: runstate.ReasonCompleted, Text: "carried on"})
			Expect(next(stream)).To(BeAssignableToTypeOf(&a2a.Result{}))
		})

		It("Should refuse a read when the worker holds no store", func() {
			newChannel(1)

			stream := send(read("tok5", 50))

			ack, ok := next(stream).(*a2a.Ack)
			Expect(ok).To(BeTrue())
			Expect(ack.Accepted).To(BeFalse())

			msg, ok := next(stream).(*a2a.ErrorMessage)
			Expect(ok).To(BeTrue())
			Expect(msg.Code).To(Equal(a2a.CodeRejected))
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
