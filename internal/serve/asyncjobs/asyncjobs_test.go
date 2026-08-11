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

	It("Should refuse a message that is not a request", func() {
		res := a2a.NewResult(a2a.StopEndTurn)
		stampHeader(&res.Header)

		_, err := ch.intake(&asyncjobs.Task{ID: "job1", Payload: encode(res)}, ch.log)
		Expect(err).To(MatchError(ContainSubstring("is not a io.choria.fisk-ai.v1.request message")))
	})

	It("Should refuse a request with no prompt", func() {
		req := newRequest("")

		_, err := ch.intake(&asyncjobs.Task{ID: "job1", Payload: encode(req)}, ch.log)
		Expect(err).To(MatchError(ContainSubstring("carries no prompt")))
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

var _ = Describe("Usage", func() {
	It("Should report nothing for a run that never started", func() {
		Expect(usageOf(nil)).To(BeNil())
	})

	// The input total is assembled rather than copied: RunStats keeps the uncached
	// remainder in InTokens and the cached input beside it, so a caller handed InTokens
	// alone would be told a fraction of what the task was billed for.
	It("Should report total input, with the cache split kept alongside it", func() {
		usage := usageOf(&util.RunStats{
			InTokens:          10,
			OutTokens:         5,
			CacheReadTokens:   900,
			CacheCreateTokens: 90,
		})

		Expect(usage.InputTokens).To(Equal(int64(1000)), "everything the task consumed, cached or not")
		Expect(usage.OutputTokens).To(Equal(int64(5)))
		Expect(usage.CacheReadTokens).To(Equal(int64(900)))
		Expect(usage.CacheCreateTokens).To(Equal(int64(90)))
	})

	It("Should report what the run did as well as what it cost", func() {
		usage := usageOf(&util.RunStats{LlmCalls: 27, ToolCalls: 27})

		Expect(usage.LLMCalls).To(Equal(int64(27)))
		Expect(usage.ToolCalls).To(Equal(int64(27)))
	})

	It("Should produce a usage the v1 schema accepts", func() {
		validator, err := a2a.NewValidator()
		Expect(err).ToNot(HaveOccurred())

		res := a2a.NewResult(a2a.StopEndTurn)
		stampHeader(&res.Header)
		res.Usage = usageOf(&util.RunStats{InTokens: 1, OutTokens: 2, CacheReadTokens: 3, LlmCalls: 4, ToolCalls: 5})

		Expect(validator.ValidateMessage(res)).To(Succeed())
	})
})

var _ = Describe("Translation", func() {
	It("Should map every terminal reason onto the protocol vocabulary", func() {
		Expect(stopReason(runstate.ReasonCompleted)).To(Equal(a2a.StopEndTurn))
		Expect(stopReason(runstate.ReasonBudget)).To(Equal(a2a.StopBudgetExhausted))
		Expect(stopReason(runstate.ReasonMaxIterations)).To(Equal(a2a.StopMaxIterations))
		Expect(stopReason(runstate.ReasonSuspended)).To(Equal(a2a.StopSuspended))
		Expect(stopReason(runstate.ReasonError)).To(Equal(a2a.StopError))
		Expect(stopReason("something later")).To(Equal(a2a.StopError))
	})

	It("Should carry only the budget limits Work has a home for", func() {
		Expect(budgetOf(newRequest("go"))).To(Equal(serve.Budget{}))

		req := newRequest("go")
		req.Budget = &a2a.Budget{MaxTokens: 10, MaxIterations: 3, CallTimeout: "1m"}
		Expect(budgetOf(req)).To(Equal(serve.Budget{MaxTokens: 10, MaxIterations: 3}))
	})
})
