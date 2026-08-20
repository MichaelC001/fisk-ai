//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package asyncjobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/choria-io/asyncjobs"
	"github.com/nats-io/nats.go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/serve"
	"github.com/choria-io/fisk-ai/internal/util"
)

func TestAsyncJobs(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Serve/AsyncJobs")
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// stampHeader fills the framing a caller assembles by hand. The message constructors
// set only the protocol id, and nothing of ours is in a caller's submission path, so a
// spec builds a message the same way a caller has to.
func stampHeader(h *a2a.Header) {
	id := a2a.NewID()
	h.ID = id
	h.Request = id
	h.Conversation = id
	h.Time = time.Now().UTC()
	h.Sender = a2a.Identity{Name: "caller"}
}

func newRequest(prompt string) *a2a.Request {
	req := a2a.NewRequest(prompt)
	stampHeader(&req.Header)

	return req
}

func encode(v any) []byte {
	GinkgoHelper()

	raw, err := json.Marshal(v)
	Expect(err).ToNot(HaveOccurred())

	return raw
}

// testChannel is a Channel assembled without binding to a queue, for the parts that
// decide things rather than talk to one.
func testChannel() *Channel {
	GinkgoHelper()

	validator, err := a2a.NewValidator()
	Expect(err).ToNot(HaveOccurred())

	return &Channel{
		name:       "asyncjobs/TEST",
		identity:   "worker",
		maxPayload: defaultMaxPayload,
		validator:  validator,
		log:        quietLogger(),
	}
}

var _ = Describe("Options", func() {
	It("Should require everything it cannot invent", func() {
		conn := &nats.Conn{}

		_, err := New(Options{})
		Expect(err).To(MatchError(ContainSubstring("NATS connection is required")))

		// The engine's own requirement, checked here because the caller establishes the
		// connection and the engine's message does not say where to set it.
		_, err = New(Options{Conn: conn})
		Expect(err).To(MatchError(ContainSubstring("nats.UseOldRequestStyle()")))

		conn.Opts.UseOldRequestStyle = true

		_, err = New(Options{Conn: conn})
		Expect(err).To(MatchError(ContainSubstring("queue name is required")))

		_, err = New(Options{Conn: conn, Queue: "Q"})
		Expect(err).To(MatchError(ContainSubstring("task type is required")))

		_, err = New(Options{Conn: conn, Queue: "Q", TaskType: "t"})
		Expect(err).To(MatchError(ContainSubstring("identity is required")))

		_, err = New(Options{Conn: conn, Queue: "Q", TaskType: "t", Identity: "me"})
		Expect(err).To(MatchError(ContainSubstring("concurrency must be greater than zero")))
	})
})

var _ = Describe("Intake", func() {
	var ch *Channel

	BeforeEach(func() {
		ch = testChannel()
	})

	It("Should admit a valid request", func() {
		req := newRequest("do a thing")
		req.Context = "some context"

		got, err := ch.intake(&asyncjobs.Task{ID: "job1", Payload: encode(req)}, ch.log)
		Expect(err).ToNot(HaveOccurred())
		Expect(got.Prompt).To(Equal("do a thing"))
		Expect(got.Context).To(Equal("some context"))
		Expect(got.Sender.Name).To(Equal("caller"))
	})

	It("Should refuse a payload over the size cap before decoding it", func() {
		ch.maxPayload = 16

		_, err := ch.intake(&asyncjobs.Task{ID: "job1", Payload: encode(newRequest("go"))}, ch.log)
		Expect(err).To(MatchError(ContainSubstring("over the 16 byte limit")))
	})

	// The engine's own name rule allows a leading dash, a colon and any length, so a
	// task id it accepted can still be one no session store will take. Finding that out
	// here rather than at the create is the whole point of checking it.
	It("Should refuse a task id no session store would accept", func() {
		for _, id := range []string{"-leading-dash", "has:colon", string(make([]byte, 200))} {
			_, err := ch.intake(&asyncjobs.Task{ID: id, Payload: encode(newRequest("go"))}, ch.log)
			Expect(err).To(MatchError(ContainSubstring("cannot name a session")), "id %q", id)
		}
	})

	It("Should refuse a payload that is not a valid message", func() {
		_, err := ch.intake(&asyncjobs.Task{ID: "job1", Payload: []byte(`{"protocol":"io.choria.fisk-ai.v1.request"}`)}, ch.log)
		Expect(err).To(MatchError(ContainSubstring("not a valid v1 message")))

		_, err = ch.intake(&asyncjobs.Task{ID: "job1", Payload: []byte(`not json`)}, ch.log)
		Expect(err).To(HaveOccurred())
	})

	It("Should refuse a message that is not a prompt", func() {
		res := a2a.NewResult(a2a.StopEndTurn)
		stampHeader(&res.Header)

		_, err := ch.intake(&asyncjobs.Task{ID: "job1", Payload: encode(res)}, ch.log)
		Expect(err).To(MatchError(ContainSubstring("is not a io.choria.fisk-ai.v1.request.prompt message")))
	})

	// A queue has nobody waiting on it, so the three kinds of request that act on a
	// conversation somebody is watching reach it as a mistake.
	It("Should refuse a request that is not a prompt", func() {
		read, err := a2a.NewRead("2Ab3Cd4Ef5Gh", 10)
		Expect(err).ToNot(HaveOccurred())
		stampHeader(&read.Header)

		_, err = ch.intake(&asyncjobs.Task{ID: "job1", Payload: encode(read)}, ch.log)
		Expect(err).To(MatchError(ContainSubstring("is not a io.choria.fisk-ai.v1.request.prompt message")))

		resume := a2a.NewResume("2Ab3Cd4Ef5Gh")
		stampHeader(&resume.Header)

		_, err = ch.intake(&asyncjobs.Task{ID: "job1", Payload: encode(resume)}, ch.log)
		Expect(err).To(MatchError(ContainSubstring("is not a io.choria.fisk-ai.v1.request.prompt message")))
	})

	It("Should refuse a prompt with nothing in it", func() {
		_, err := ch.intake(&asyncjobs.Task{ID: "job1", Payload: encode(newRequest(""))}, ch.log)
		Expect(err).To(MatchError(ContainSubstring("not a valid v1 message")))
	})
})

var _ = Describe("Dispositions", func() {
	var (
		ch        *Channel
		req       *a2a.Request
		validator *a2a.Validator
	)

	BeforeEach(func() {
		ch = testChannel()
		req = newRequest("go")

		var err error
		validator, err = a2a.NewValidator()
		Expect(err).ToNot(HaveOccurred())
	})

	// A run that finished is a completed job whatever it decided, so the answer is
	// stored and the job is acknowledged.
	It("Should acknowledge a completed run with a result message", func() {
		payload, err := ch.disposition(req, serve.Outcome{
			ID:        "job1",
			SessionID: "job1",
			Text:      "the answer",
			Reason:    runstate.ReasonCompleted,
			Stats:     &util.RunStats{InTokens: 11, OutTokens: 22},
		}, ch.log)

		Expect(err).ToNot(HaveOccurred())

		res, ok := payload.(*a2a.Result)
		Expect(ok).To(BeTrue())
		Expect(res.StopReason).To(Equal(a2a.StopEndTurn))
		Expect(res.Text).To(Equal("the answer"))
		Expect(res.Usage).To(Equal(&a2a.Usage{InputTokens: 11, OutputTokens: 22}))

		Expect(res.Request).To(Equal(req.Request), "the answer correlates to the request")
		Expect(res.Conversation).To(Equal(req.Conversation))
		Expect(res.Sender.Name).To(Equal("worker"))
		Expect(res.Recipient).To(Equal(&a2a.Identity{Name: "caller"}))
		Expect(res.ID).ToNot(BeEmpty())

		Expect(validator.ValidateMessage(res)).To(Succeed(), "the answer is a published contract")
	})

	It("Should acknowledge a run that failed with an error message", func() {
		payload, err := ch.disposition(req, serve.Outcome{
			SessionID: "job1",
			Reason:    runstate.ReasonMaxIterations,
			Stats:     &util.RunStats{},
			Err:       fmt.Errorf("ran out of iterations"),
		}, ch.log)

		Expect(err).ToNot(HaveOccurred())

		msg, ok := payload.(*a2a.ErrorMessage)
		Expect(ok).To(BeTrue())
		Expect(msg.StopReason).To(Equal(a2a.StopMaxIterations))
		Expect(msg.Err).To(Equal("ran out of iterations"))
		Expect(msg.Request).To(Equal(req.Request))

		Expect(validator.ValidateMessage(msg)).To(Succeed())
	})

	It("Should terminate work that was refused or crashed", func() {
		_, err := ch.disposition(req, serve.Outcome{Rejected: true}, ch.log)
		Expect(errors.Is(err, asyncjobs.ErrTerminateTask)).To(BeTrue())
		Expect(err).To(MatchError(ContainSubstring("refused")))

		_, err = ch.disposition(req, serve.Outcome{Crashed: true, Err: fmt.Errorf("boom")}, ch.log)
		Expect(errors.Is(err, asyncjobs.ErrTerminateTask)).To(BeTrue())
		Expect(err).To(MatchError(ContainSubstring("crashed")))
	})

	// Redelivering achieves nothing while a tool is waiting on an answer that may be
	// days away: every attempt would resume, find it still absent, suspend again and
	// spend a delivery. Terminating gives the lease back; RetryTaskByID is what brings
	// the job back once the answer is supplied.
	It("Should terminate a run waiting on a deferred tool result", func() {
		_, err := ch.disposition(req, serve.Outcome{
			SessionID: "job1",
			Reason:    runstate.ReasonSuspended,
			Deferred: []agent.DeferredCall{
				{ToolUseID: "tu_1", ToolName: "change_request", Note: "waiting on approval"},
				{ToolUseID: "tu_2", ToolName: "change_request"},
			},
		}, ch.log)

		Expect(errors.Is(err, asyncjobs.ErrTerminateTask)).To(BeTrue())

		// The ids reach Task.LastErr, which is all a queue operator has to find what
		// session answer needs.
		Expect(err).To(MatchError(ContainSubstring("job1")))
		Expect(err).To(MatchError(ContainSubstring("tu_1,tu_2")))

		// The tool's own words are not stored: they are tool-supplied text and only the
		// ids are needed to answer the call.
		Expect(err).ToNot(MatchError(ContainSubstring("waiting on approval")))
	})

	// Nothing was answered in any of these, so the job goes back to the queue rather
	// than recording a non-answer as the result.
	It("Should return work that produced no answer", func() {
		for _, tc := range []struct {
			name string
			out  serve.Outcome
			want string
		}{
			{"abandoned", serve.Outcome{Abandoned: true}, "never started"},
			{"suspended", serve.Outcome{Reason: runstate.ReasonSuspended, SessionID: "job1"}, "suspended"},
			{"setup failed", serve.Outcome{Err: fmt.Errorf("no store")}, "no store"},
			{"held elsewhere", serve.Outcome{Err: runstate.ErrLocked}, "already locked"},
			{"nothing at all", serve.Outcome{}, "neither an outcome nor an error"},
		} {
			payload, err := ch.disposition(req, tc.out, ch.log)
			Expect(payload).To(BeNil(), tc.name)
			Expect(err).To(MatchError(ContainSubstring(tc.want)), tc.name)
			Expect(errors.Is(err, asyncjobs.ErrTerminateTask)).To(BeFalse(), "%s must be retried", tc.name)
		}
	})
})

var _ = Describe("Translation", func() {
	It("Should carry only the budget limits Work has a home for", func() {
		Expect(budgetOf(newRequest("go"))).To(Equal(serve.Budget{}))

		req := newRequest("go")
		req.Budget = &a2a.Budget{MaxTokens: 10, MaxIterations: 3, CallTimeout: "1m"}
		Expect(budgetOf(req)).To(Equal(serve.Budget{MaxTokens: 10, MaxIterations: 3}))
	})
})
