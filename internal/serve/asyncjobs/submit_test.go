//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package asyncjobs

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/choria-io/asyncjobs"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/a2a"
)

// requestOf decodes the payload NewJob built, which is the only thing a worker ever
// sees of a job.
func requestOf(task *asyncjobs.Task) *a2a.Request {
	GinkgoHelper()

	msg, err := a2a.Decode(task.Payload)
	Expect(err).ToNot(HaveOccurred())

	req, ok := msg.(*a2a.Request)
	Expect(ok).To(BeTrue())

	return req
}

var _ = Describe("NewJob", func() {
	It("Should build a payload the worker's own intake accepts", func() {
		task, err := NewJob(Job{Prompt: "go", Caller: "caller"})
		Expect(err).ToNot(HaveOccurred())

		Expect(task.Type).To(Equal(config.DefaultJobsTaskType))

		// The worker validates against the schema and then decodes, so running its own
		// intake is what proves the two agree rather than a field-by-field assertion
		// against the shape this test also wrote.
		req, err := testChannel().intake(task, quietLogger())
		Expect(err).ToNot(HaveOccurred())
		Expect(req.Prompt).To(Equal("go"))
		Expect(req.Sender.Name).To(Equal("caller"))
	})

	It("Should carry the optional fields a caller sets", func() {
		budget := &a2a.Budget{MaxTokens: 100, MaxIterations: 3}

		task, err := NewJob(Job{
			Prompt:       "go",
			Context:      "supporting material",
			Caller:       "caller",
			Conversation: "conv-1",
			Budget:       budget,
			TaskType:     "fisk-ai:slow",
		})
		Expect(err).ToNot(HaveOccurred())
		Expect(task.Type).To(Equal("fisk-ai:slow"))

		req := requestOf(task)
		Expect(req.Context).To(Equal("supporting material"))
		Expect(req.Conversation).To(Equal("conv-1"))
		Expect(req.Budget).To(Equal(budget))
	})

	// The framing a one-shot job does not care about is still required of it, which is
	// most of why this helper exists.
	It("Should fill in the correlation fields a one-shot job has no answer for", func() {
		task, err := NewJob(Job{Prompt: "go", Caller: "caller"})
		Expect(err).ToNot(HaveOccurred())

		req := requestOf(task)
		Expect(req.ID).ToNot(BeEmpty())
		Expect(req.Request).To(Equal(req.ID), "on a request the correlation tag is its own id")
		Expect(req.Conversation).To(Equal(req.ID), "nothing to group, so it groups with itself")
		Expect(req.Time).To(BeTemporally("~", time.Now(), time.Minute))
		Expect(req.WantsStream()).To(BeFalse(), "this binding has no event stream to ask for")
	})

	It("Should refuse what would fail at the worker", func() {
		_, err := NewJob(Job{Caller: "caller"})
		Expect(err).To(MatchError(ContainSubstring("needs a prompt")))

		_, err = NewJob(Job{Prompt: "go"})
		Expect(err).To(MatchError(ContainSubstring("needs a caller name")))

		// The schema's identity pattern. A worker given this answers only that the
		// payload is not a valid v1 message.
		_, err = NewJob(Job{Prompt: "go", Caller: "svc.example"})
		Expect(err).To(MatchError(ContainSubstring(`caller name "svc.example" is not valid`)))
	})

	// The task id names the session, and the engine's name rule is looser than the
	// store's, so an id the queue accepts can still be one no journal can be created
	// under. Caught here, it would otherwise terminate on first delivery having run
	// nothing.
	It("Should refuse an id that cannot name a session", func() {
		_, err := NewJob(Job{Prompt: "go", Caller: "caller", ID: "-leading-dash"})
		Expect(err).To(MatchError(ContainSubstring("cannot name a session")))

		_, err = NewJob(Job{Prompt: "go", Caller: "caller", ID: "has:colon"})
		Expect(err).To(MatchError(ContainSubstring("cannot name a session")))

		task, err := NewJob(Job{Prompt: "go", Caller: "caller", ID: "job-1"})
		Expect(err).ToNot(HaveOccurred())
		Expect(task.ID).To(Equal("job-1"))
	})

	It("Should mint an id when none is given", func() {
		task, err := NewJob(Job{Prompt: "go", Caller: "caller"})
		Expect(err).ToNot(HaveOccurred())
		Expect(task.ID).ToNot(BeEmpty())
	})

	// Options are the engine's, so one setting an id has to be checked like our own.
	It("Should pass the engine's options through and still check the id they set", func() {
		task, err := NewJob(Job{Prompt: "go", Caller: "caller"}, asyncjobs.TaskMaxTries(9))
		Expect(err).ToNot(HaveOccurred())
		Expect(task.MaxTries).To(Equal(9))

		_, err = NewJob(Job{Prompt: "go", Caller: "caller"}, func(t *asyncjobs.Task) error {
			t.ID = "bad:id"
			return nil
		})
		Expect(err).To(MatchError(ContainSubstring("cannot name a session")))
	})
})

var _ = Describe("ParseAnswer", func() {
	// The stored payload is an any that arrived as JSON, so a spec has to store it the
	// way the engine does rather than handing over the Go value.
	stored := func(msg any) *asyncjobs.Task {
		GinkgoHelper()

		raw, err := json.Marshal(msg)
		Expect(err).ToNot(HaveOccurred())

		var payload any
		Expect(json.Unmarshal(raw, &payload)).To(Succeed())

		return &asyncjobs.Task{
			State:  asyncjobs.TaskStateCompleted,
			Result: &asyncjobs.TaskResult{Payload: payload},
		}
	}

	It("Should return the result of a run that answered", func() {
		msg := a2a.NewResult(a2a.StopEndTurn)
		msg.Text = "all done"
		msg.Usage = &a2a.Usage{InputTokens: 10, OutputTokens: 5}

		res, err := ParseAnswer(stored(msg))
		Expect(err).ToNot(HaveOccurred())
		Expect(res.Text).To(Equal("all done"))
		Expect(res.StopReason).To(Equal(a2a.StopEndTurn))
		Expect(res.Usage.InputTokens).To(BeNumerically("==", 10))
	})

	// The stored message implements error, so a caller reaches its stop reason through
	// errors.As rather than through a second return value.
	It("Should return a recorded failure as the error it already is", func() {
		msg := a2a.NewError("the run failed")
		msg.StopReason = a2a.StopBudgetExhausted

		res, err := ParseAnswer(stored(msg))
		Expect(res).To(BeNil())
		Expect(err).To(MatchError("the run failed"))

		var stored *a2a.ErrorMessage
		Expect(errors.As(err, &stored)).To(BeTrue())
		Expect(stored.StopReason).To(Equal(a2a.StopBudgetExhausted))
	})

	It("Should say a task carries no answer rather than panic on it", func() {
		_, err := ParseAnswer(nil)
		Expect(err).To(MatchError(ContainSubstring("no task")))

		_, err = ParseAnswer(&asyncjobs.Task{State: asyncjobs.TaskStateActive})
		Expect(err).To(MatchError(ContainSubstring("carries no answer")))
		Expect(err).To(MatchError(ContainSubstring("active")), "the state is what tells a caller to wait or give up")
	})

	// What the message means is a2a.DecodeTerminal's to decide, so this only proves the
	// unwrapped payload reaches it.
	It("Should refuse a payload that is not a terminal message", func() {
		_, err := ParseAnswer(stored(a2a.NewRequest("go")))
		Expect(err).To(MatchError(ContainSubstring("is not a terminal message")))
	})
})
