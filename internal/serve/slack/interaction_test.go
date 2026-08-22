//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/serve"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/toolkit/builtin"
)

// deferredQuestion asks one question, lets the grace window run out, and answers with the
// message it was asked on. It is where every click that has to become a resume starts.
func deferredQuestion(w *serve.Work, api *fakeAPI, clock *testClock, grace time.Duration, ask func(toolkit.Prompter) error) *fakeMessage {
	GinkgoHelper()

	failures := make(chan error, 1)
	go func() { failures <- ask(w.Prompter) }()

	var q *fakeMessage
	Eventually(func() *fakeMessage { q = questionIn(api)(); return q }).ShouldNot(BeNil())
	Eventually(clock.waiting).Should(Equal(1), "the question is the only thing parked on this clock")

	clock.advance(grace)
	Eventually(failures).Should(Receive(HaveOccurred()))

	return q
}

// abandoned waits until no run is waiting on one question, which is what the turn that asked
// it reporting its outcome does. The question itself stands: the thread is still waiting on
// it, so a press from here on becomes a resume.
func abandoned(ch *Channel, toolUseID string) {
	GinkgoHelper()

	Eventually(func() questionState {
		q := heldQuestion(ch, toolUseID)
		if q == nil {
			return questionAnswered
		}

		ch.asked.mu.Lock()
		defer ch.asked.mu.Unlock()

		return q.state
	}).Should(Equal(questionAbandoned))
}

// pressFor is the press of one button on a question this worker no longer holds, built the
// way a restarted worker receives one: from the value alone.
func pressFor(v buttonValue, user string) pressEvent {
	GinkgoHelper()

	value, err := encodeValue(v)
	Expect(err).ToNot(HaveOccurred())

	return pressEvent{
		Team:      "T1",
		Channel:   "C1",
		ThreadTS:  "1700000000.000100",
		MessageTS: "1700000000.000200",
		User:      user,
		TriggerID: "Tr1",
		Value:     value,
	}
}

// clickFrom decodes one press the way the intake goroutine does, for the specs that drive
// the click path directly rather than through the socket.
func clickFrom(p pressEvent) *click {
	GinkgoHelper()

	in, wanted, err := clickOf(p.envelope())
	Expect(err).ToNot(HaveOccurred())
	Expect(wanted).To(BeTrue())

	return in
}

// gateFor is the approval request the gate puts for one call, the command being the same
// one across a run that gave up and the resume that dispatches the call again.
func gateFor(toolUseID string) toolkit.GateRequest {
	return toolkit.GateRequest{ToolUseID: toolUseID, Command: "stream rm", Display: "stream rm ORDERS", Tag: "ai:confirm"}
}

// questionsIn is how many messages this bot is still carrying buttons on, which is how a
// spec says a resumed run asked the thread again rather than answering from what it held.
func questionsIn(a *fakeAPI) func() int {
	return func() int {
		n := 0

		for _, m := range a.messages() {
			if asks(m) {
				n++
			}
		}

		return n
	}
}

// questionFor is the message this bot asked one call's question on, for the specs that have
// more than one open at a time.
func questionFor(a *fakeAPI, toolUseID string) func() *fakeMessage {
	return func() *fakeMessage {
		msgs := a.messages()

		for i := range msgs {
			if !asks(msgs[i]) {
				continue
			}

			v, err := decodeValue(msgs[i].Buttons[0].Value)
			if err != nil || v.ToolUse != toolUseID {
				continue
			}

			return &msgs[i]
		}

		return nil
	}
}

// inThread is a mention somebody makes in the thread this bot has been asking questions in.
// ts names the message, which is what a redelivery is recognized by, so two of them in one
// spec are two mentions.
func inThread(ts, text string) mentionEvent {
	m := aMention()
	m.EnvelopeID = "Ev-" + ts
	m.ThreadTS = "1700000000.000100"
	m.TS = ts
	m.User = "U7"
	m.Text = "<@U0BOT> " + text

	return m
}

// deferring reports a turn that ended waiting on answers, which is what leaves a question
// standing in a thread.
func deferring(w *serve.Work, ids ...string) {
	GinkgoHelper()

	calls := make([]agent.DeferredCall, 0, len(ids))
	for _, id := range ids {
		calls = append(calls, agent.DeferredCall{ToolUseID: id})
	}

	Expect(w.Done(context.Background(), serve.Outcome{
		ID: w.ID, Reason: runstate.ReasonSuspended, Deferred: calls,
	})).To(Succeed())
}

// postedLine reports whether this bot said one thing in a thread, whichever message it said
// it on.
func postedLine(a *fakeAPI, text string) func() bool {
	return func() bool {
		for _, m := range a.messages() {
			if m.Text == text {
				return true
			}
		}

		return false
	}
}

var _ = Describe("Interactions", func() {
	var (
		api    *fakeAPI
		socket *fakeSocket
		opts   Options
		clock  *testClock
	)

	BeforeEach(func() {
		api = newFakeAPI()
		api.names = map[string]person{"U7": {Full: "Cara Duarte", Username: "cara"}}
		socket = newFakeSocket()
		opts = testOptions()
		clock = newTestClock()
	})

	// Slack redelivers an envelope it has not been answered about within three seconds,
	// exactly as it does a mention, so the decode is in memory and everything that reaches
	// Slack happens after the acknowledgement and somewhere else.
	It("Should acknowledge a press before anything it starts talks to Slack", func() {
		ch := promptingChannel(opts, api, socket, clock)
		w := runningTurn(ch, socket)

		go func() {
			_, _ = w.Prompter.Input(callCtx("tu1"), "which node should I drain?", "")
		}()

		var q *fakeMessage
		Eventually(func() *fakeMessage { q = questionIn(api)(); return q }).ShouldNot(BeNil())

		release, arrivals := api.hold()

		socket.deliver(pressing(q, choiceReply, "U2").envelope())

		Eventually(socket.acked).Should(HaveLen(2))
		Eventually(arrivals).Should(Receive())
		Expect(api.opened()).To(BeEmpty(), "the dialog is still in flight, and the envelope was answered before it")

		release()

		Eventually(api.opened).Should(HaveLen(1))
	})

	It("Should acknowledge an interaction it cannot read", func() {
		promptingChannel(opts, api, socket, clock)

		socket.deliver(envelope{ID: "Ei9", Kind: envelopeInteractive, Payload: []byte("not json")})

		Eventually(socket.acked).Should(Equal([]string{"Ei9"}))
		Consistently(api.messages, 100*time.Millisecond).Should(BeEmpty())
	})

	It("Should acknowledge a press whose button this bot did not mint", func() {
		promptingChannel(opts, api, socket, clock)

		press := pressEvent{
			EnvelopeID: "Ei9",
			Team:       "T1",
			Channel:    "C1",
			ThreadTS:   "1700000000.000100",
			MessageTS:  "1700000000.000100",
			User:       "U2",
			Value:      "not a value this bot wrote",
		}

		socket.deliver(press.envelope())

		Eventually(socket.acked).Should(Equal([]string{"Ei9"}))
	})

	// A dialog some other app opened in this workspace is not an answer to anything here.
	It("Should ignore a dialog opened under another callback id", func() {
		ch := promptingChannel(opts, api, socket, clock)
		w := runningTurn(ch, socket)

		answers := make(chan string, 1)
		go func() {
			text, _ := w.Prompter.Input(callCtx("tu1"), "which node?", "")
			answers <- text
		}()

		Eventually(questionIn(api)).ShouldNot(BeNil())

		meta, err := json.Marshal(modalMeta{
			Question:  buttonValue{Kind: kindInput, ToolUse: "tu1", Choice: choiceReply},
			ChannelID: "C1",
			ThreadTS:  "1700000000.000100",
		})
		Expect(err).ToNot(HaveOccurred())

		socket.deliver(submitEvent{
			EnvelopeID: "Ei9",
			Team:       "T1",
			User:       "U2",
			CallbackID: "somebody_elses_dialog",
			Metadata:   string(meta),
			Text:       "node4",
		}.envelope())

		Eventually(socket.acked).Should(HaveLen(2))
		Consistently(answers, 100*time.Millisecond).ShouldNot(Receive())
	})

	Describe("Reading one interaction", func() {
		It("Should take the conversation from the envelope rather than from the button", func() {
			press := pressEvent{
				Team:      "T1",
				Channel:   "C1",
				ThreadTS:  "1700000000.000100",
				MessageTS: "1700000005.000100",
				User:      "U2",
				TriggerID: "Tr1",
			}

			value, err := encodeValue(buttonValue{Kind: kindConfirm, ToolUse: "tu1", Choice: choiceYes, Asker: "U1"})
			Expect(err).ToNot(HaveOccurred())
			press.Value = value

			in, wanted, err := clickOf(press.envelope())
			Expect(err).ToNot(HaveOccurred())
			Expect(wanted).To(BeTrue())

			Expect(in.Interaction).To(Equal(interactionPress))
			Expect(in.TeamID).To(Equal("T1"))
			Expect(in.ChannelID).To(Equal("C1"))
			Expect(in.ThreadTS).To(Equal("1700000000.000100"))
			Expect(in.MessageTS).To(Equal("1700000005.000100"))
			Expect(in.UserID).To(Equal("U2"))
			Expect(in.TriggerID).To(Equal("Tr1"))
			Expect(in.Value.ToolUse).To(Equal("tu1"))
			Expect(in.Value.Asker).To(Equal("U1"))
		})

		It("Should take a dialog's conversation from what it was stamped with", func() {
			meta, err := json.Marshal(modalMeta{
				Question:  buttonValue{Kind: kindInput, ToolUse: "tu1", Choice: choiceReply, Asker: "U1"},
				ChannelID: "C1",
				ThreadTS:  "1700000000.000100",
			})
			Expect(err).ToNot(HaveOccurred())

			in, wanted, err := clickOf(submitEvent{Team: "T1", User: "U2", Metadata: string(meta), Text: "node4"}.envelope())
			Expect(err).ToNot(HaveOccurred())
			Expect(wanted).To(BeTrue())

			Expect(in.Interaction).To(Equal(interactionSubmit))
			Expect(in.ChannelID).To(Equal("C1"))
			Expect(in.ThreadTS).To(Equal("1700000000.000100"))
			Expect(in.UserID).To(Equal("U2"))
			Expect(in.Text).To(Equal("node4"))
			Expect(in.Value.Kind).To(Equal(kindInput))
		})

		// An empty answer is one somebody gave, which is why the dialog's own submission is
		// what says a value arrived rather than the value itself.
		It("Should read an empty dialog as the answer it is", func() {
			meta, err := json.Marshal(modalMeta{
				Question:  buttonValue{Kind: kindInput, ToolUse: "tu1", Choice: choiceReply},
				ChannelID: "C1",
				ThreadTS:  "1700000000.000100",
			})
			Expect(err).ToNot(HaveOccurred())

			in, wanted, err := clickOf(submitEvent{Team: "T1", User: "U2", Metadata: string(meta)}.envelope())
			Expect(err).ToNot(HaveOccurred())
			Expect(wanted).To(BeTrue())
			Expect(in.Text).To(BeEmpty())
		})

		It("Should refuse an interaction missing what an answer is placed by", func() {
			press := pressEvent{Team: "T1", Channel: "C1", User: "U2"}

			value, err := encodeValue(buttonValue{Kind: kindConfirm, ToolUse: "tu1", Choice: choiceYes})
			Expect(err).ToNot(HaveOccurred())
			press.Value = value

			_, _, err = clickOf(press.envelope())
			Expect(err).To(MatchError(ContainSubstring("identifiers an answer is placed by")))
		})

		It("Should ignore an envelope that carries no interaction", func() {
			_, wanted, err := clickOf(envelope{Kind: envelopeMention, Payload: []byte(`{}`)})
			Expect(err).ToNot(HaveOccurred())
			Expect(wanted).To(BeFalse())
		})

		It("Should refuse a press naming a question this bot does not ask", func() {
			_, err := decodeValue(`{"kind":"something_else","tool_use":"tu1"}`)
			Expect(err).To(MatchError(ContainSubstring("not a question this bot asks")))
		})
	})

	Describe("A click as a second source of work", func() {
		var session string

		BeforeEach(func() {
			session = SessionFor(opts.Identity, "T1", "C1", "1700000000.000100")
		})

		// The turn that asked has reported its outcome, so nothing is waiting on the
		// question and the answer reaches the conversation the only way left to it.
		It("Should hand over a resume carrying the answer to the call that deferred", func() {
			ch := promptingChannel(opts, api, socket, clock)
			w := runningTurn(ch, socket)

			q := deferredQuestion(w, api, clock, opts.AnswerGrace, func(p toolkit.Prompter) error {
				_, err := p.Confirm(callCtx("tu1"), "restart node3?")

				return err
			})

			ended(w)
			abandoned(ch, "tu1")

			socket.deliver(pressing(q, choiceYes, "U7").envelope())

			content, err := builtin.ConfirmResult(true, "")
			Expect(err).ToNot(HaveOccurred())

			resumed := nextWork(ch)

			Expect(resumed.Checkpoint).To(Equal(agent.Checkpoint{
				ResumeID: session,
				Answer:   &agent.DeferredAnswer{ToolUseID: "tu1", Content: content},
				Force:    true,
			}))
			Expect(resumed.ID).To(Equal("C1/tu1"), "the call rather than a message, a dialog submission carrying none")
			Expect(resumed.ClaimedBy).To(Equal(resumed.ID))
			Expect(resumed.Prompt).To(BeEmpty(), "a resume adds no turn; it answers a call the conversation is waiting on")
			Expect(resumed.Caller).To(Equal(serve.Caller{Name: "cara/U7", Verified: true}), "whoever pressed it")
		})

		// The pending-question map is process memory, so a worker that restarted between
		// the question and the press holds nothing. The envelope names the thread and the
		// value names the call and the kind, which is the whole of what the answer needs.
		It("Should build the resume from the click alone where it holds no question", func() {
			ch := promptingChannel(opts, api, socket, clock)

			Expect(heldQuestion(ch, "tu-restart")).To(BeNil())

			socket.deliver(pressFor(buttonValue{
				Kind: kindConfirm, ToolUse: "tu-restart", Choice: choiceNo, Label: labelNo, Asker: "U1",
			}, "U2").envelope())

			content, err := builtin.ConfirmResult(false, "")
			Expect(err).ToNot(HaveOccurred())

			resumed := nextWork(ch)

			Expect(resumed.Checkpoint).To(Equal(agent.Checkpoint{
				ResumeID: session,
				Answer:   &agent.DeferredAnswer{ToolUseID: "tu-restart", Content: content},
				Force:    true,
			}))
			Expect(resumed.Checkpoint.ResumeID).To(Equal(SessionFor(opts.Identity, "T1", "C1", "1700000000.000100")),
				"the team, channel and thread the interaction envelope authenticated")
		})

		// A selection answers with the option rather than with its position, which is what
		// the model was told ask_human_select returns. The options were dropped with the
		// turn that asked, so the option comes off the button that was pressed.
		It("Should answer a selection with the option that was chosen", func() {
			ch := promptingChannel(opts, api, socket, clock)
			w := runningTurn(ch, socket)

			q := deferredQuestion(w, api, clock, opts.AnswerGrace, func(p toolkit.Prompter) error {
				_, err := p.Select(callCtx("tu1"), "which node?", []string{"node3", "node4", "node5"})

				return err
			})

			ended(w)
			abandoned(ch, "tu1")

			socket.deliver(pressing(q, "1", "U7").envelope())

			content, err := builtin.SelectResult("node4", "")
			Expect(err).ToNot(HaveOccurred())

			resumed := nextWork(ch)
			Expect(resumed.Checkpoint.Answer).To(Equal(&agent.DeferredAnswer{ToolUseID: "tu1", Content: content}))
		})

		// The value a free-text question is answered with is typed after the button was
		// minted, so it arrives on the dialog's own submission.
		It("Should answer a free-text question with what was typed into the dialog", func() {
			ch := promptingChannel(opts, api, socket, clock)
			w := runningTurn(ch, socket)

			q := deferredQuestion(w, api, clock, opts.AnswerGrace, func(p toolkit.Prompter) error {
				_, err := p.Input(callCtx("tu1"), "which node should I drain?", "node3")

				return err
			})

			ended(w)
			abandoned(ch, "tu1")

			socket.deliver(pressing(q, choiceReply, "U7").envelope())
			Eventually(api.opened).Should(HaveLen(1))

			socket.deliver(submitEvent{
				Team: "T1", User: "U7", Metadata: api.opened()[0].View.Metadata, Text: "node4",
			}.envelope())

			content, err := builtin.InputResult("node4", "")
			Expect(err).ToNot(HaveOccurred())

			resumed := nextWork(ch)
			Expect(resumed.Checkpoint.Answer).To(Equal(&agent.DeferredAnswer{ToolUseID: "tu1", Content: content}))
		})

		// The gate guards a command that has not run, so there is no result to supply. The
		// resume dispatches the call and the gate asks again.
		It("Should resume a gate press with no answer for the call", func() {
			ch := promptingChannel(opts, api, socket, clock)
			w := runningTurn(ch, socket)

			q := deferredQuestion(w, api, clock, opts.AnswerGrace, func(p toolkit.Prompter) error {
				_, err := p.ApproveCommand(callCtx("tu1"), toolkit.GateRequest{
					ToolUseID: "tu1", Command: "stream rm", Display: "stream rm ORDERS", Tag: "ai:confirm",
				})

				return err
			})

			ended(w)
			abandoned(ch, "tu1")

			socket.deliver(pressing(q, choiceOnce, "U7").envelope())

			resumed := nextWork(ch)
			Expect(resumed.Checkpoint).To(Equal(agent.Checkpoint{ResumeID: session, Force: true}))
		})

		// The press landed between the run giving up and the turn reporting its outcome, so
		// the thread is still held by the turn this very answer was asked for.
		It("Should queue a click that landed as the run gave up behind the turn that asked", func() {
			ch := promptingChannel(opts, api, socket, clock)
			w := runningTurn(ch, socket)

			q := deferredQuestion(w, api, clock, opts.AnswerGrace, func(p toolkit.Prompter) error {
				_, err := p.Confirm(callCtx("tu1"), "restart node3?")

				return err
			})

			socket.deliver(pressing(q, choiceYes, "U7").envelope())

			Eventually(bodyOf(api, q.TS)).Should(ContainSubstring("Answered by <@U7>: Yes"))
			Expect(bodyOf(api, q.TS)()).To(ContainSubstring(lateAnswerLine))
			noWork(ch)

			ended(w)

			resumed := nextWork(ch)
			Expect(resumed.Checkpoint.Answer).ToNot(BeNil())
			Expect(resumed.Checkpoint.Answer.ToolUseID).To(Equal("tu1"))
		})

		// Two concurrent resumes of one conversation is what the in-flight entry prevents,
		// and the press stays pressable, so whoever clicked is told to make it again.
		It("Should refuse a press on a thread it is running another turn in", func() {
			ch := promptingChannel(opts, api, socket, clock)
			runningTurn(ch, socket)

			socket.deliver(pressFor(buttonValue{
				Kind: kindConfirm, ToolUse: "tu-old", Choice: choiceYes, Label: labelYes, Asker: "U1",
			}, "U2").envelope())

			Eventually(postedLine(api, busyPressRefusal)).Should(BeTrue())
			noWork(ch)
		})

		// One call takes one answer, and the first press is the one the conversation runs
		// on.
		It("Should start nothing for a press on a call that already has an answer", func() {
			ch := promptingChannel(opts, api, socket, clock)
			w := runningTurn(ch, socket)

			q := deferredQuestion(w, api, clock, opts.AnswerGrace, func(p toolkit.Prompter) error {
				_, err := p.Confirm(callCtx("tu1"), "restart node3?")

				return err
			})

			_, _, out := ch.asked.deliver(clickFrom(pressing(q, choiceYes, "U7")))
			Expect(out).To(Equal(deliveryResume), "the first press is the answer")

			socket.deliver(pressing(q, choiceNo, "U8").envelope())

			Eventually(bodyOf(api, q.TS)).Should(ContainSubstring(secondPressLine))
			Expect(bodyOf(api, q.TS)()).To(ContainSubstring("Answered by <@U7>: Yes"))
			noWork(ch)
		})

		// The refusal is not the end of the question: nothing was recorded as its answer,
		// the buttons stay on the message, and the press can be made again.
		It("Should leave a question pressable where the backlog refused its answer", func() {
			opts.MaxWaiting = 1

			ch := promptingChannel(opts, api, socket, clock)
			w := runningTurn(ch, socket)

			q := deferredQuestion(w, api, clock, opts.AnswerGrace, func(p toolkit.Prompter) error {
				_, err := p.Confirm(callCtx("tu1"), "restart node3?")

				return err
			})

			// One turn admitted and waiting for a worker is the whole backlog this channel
			// was built to hold.
			filling := aMention()
			filling.EnvelopeID = "Ev2"
			filling.Channel = "C2"
			filling.TS = "1700000009.000100"

			socket.deliver(filling.envelope())
			Eventually(socket.acked).Should(HaveLen(2))

			socket.deliver(pressing(q, choiceYes, "U7").envelope())

			Eventually(bodyOf(api, q.TS)).Should(ContainSubstring(backlogPressRefusal))
			Expect(bodyOf(api, q.TS)()).ToNot(ContainSubstring("Answered by"), "no answer was recorded, none having reached a run")
			Expect(buttonsOf(api, q.TS)()).To(HaveLen(3), "the press somebody was asked to make again has a button to be made on")

			_, _, out := ch.asked.deliver(clickFrom(pressing(q, choiceYes, "U7")))
			Expect(out).To(Equal(deliveryResume), "the question is where the click found it")
		})
	})

	Describe("The approval a late press carried", func() {
		var session string

		BeforeEach(func() {
			session = SessionFor(opts.Identity, "T1", "C1", "1700000000.000100")
		})

		// The resume dispatches the call the gate guards and the gate asks about it again.
		// Somebody who pressed Allow gets the command they approved rather than the same
		// question a second time.
		DescribeTable("Should answer the resumed run's gate from the press",
			func(choice string, expected toolkit.ConfirmChoice) {
				ch := promptingChannel(opts, api, socket, clock)
				w := runningTurn(ch, socket)

				q := deferredQuestion(w, api, clock, opts.AnswerGrace, func(p toolkit.Prompter) error {
					_, err := p.ApproveCommand(callCtx("tu1"), gateFor("tu1"))

					return err
				})

				ended(w)
				abandoned(ch, "tu1")

				socket.deliver(pressing(q, choice, "U7").envelope())

				resumed := nextWork(ch)
				Expect(resumed.Checkpoint).To(Equal(agent.Checkpoint{ResumeID: session, Force: true}),
					"the gate's call has no result to supply; the resume dispatches it")

				answers := make(chan toolkit.ConfirmChoice, 1)
				failures := make(chan error, 1)

				go func() {
					got, err := resumed.Prompter.ApproveCommand(callCtx("tu1"), gateFor("tu1"))
					failures <- err
					answers <- got
				}()

				Eventually(failures).Should(Receive(BeNil()))
				Eventually(answers).Should(Receive(Equal(expected)))
				Expect(questionsIn(api)()).To(BeZero(), "the thread was asked once, and the answer took that question's buttons off")
			},
			Entry("allowed once", choiceOnce, toolkit.ConfirmOnce),
			Entry("allowed for the rest of the conversation", choiceAlways, toolkit.ConfirmAlways),
			Entry("declined", choiceNo, toolkit.ConfirmNo),
		)

		// The press approved one command. A later call of the same tool carries its own
		// arguments, so the thread decides on that one too.
		It("Should ask about a later call of the same tool, the approval being spent on the first", func() {
			ch := promptingChannel(opts, api, socket, clock)
			w := runningTurn(ch, socket)

			q := deferredQuestion(w, api, clock, opts.AnswerGrace, func(p toolkit.Prompter) error {
				_, err := p.ApproveCommand(callCtx("tu1"), gateFor("tu1"))

				return err
			})

			ended(w)
			abandoned(ch, "tu1")

			socket.deliver(pressing(q, choiceOnce, "U7").envelope())

			resumed := nextWork(ch)

			got, err := resumed.Prompter.ApproveCommand(callCtx("tu1"), gateFor("tu1"))
			Expect(err).ToNot(HaveOccurred())
			Expect(got).To(Equal(toolkit.ConfirmOnce))

			failures := make(chan error, 1)
			go func() {
				_, err := resumed.Prompter.ApproveCommand(callCtx("tu2"), gateFor("tu2"))
				failures <- err
			}()

			Eventually(questionsIn(api)).Should(Equal(1), "a second command is a second question, the first having been answered")
			Eventually(clock.waiting).Should(Equal(1))

			clock.advance(opts.AnswerGrace)
			Eventually(failures).Should(Receive(MatchError(toolkit.ErrPromptAborted)))
		})

		// The approval is on the turn the press produced and reaches no journal, so a run
		// that ended before the gate asked leaves the next one to ask the thread.
		It("Should hold the approval no longer than the resume it arrived on", func() {
			ch := promptingChannel(opts, api, socket, clock)
			w := runningTurn(ch, socket)

			q := deferredQuestion(w, api, clock, opts.AnswerGrace, func(p toolkit.Prompter) error {
				_, err := p.ApproveCommand(callCtx("tu1"), gateFor("tu1"))

				return err
			})

			ended(w)
			abandoned(ch, "tu1")

			socket.deliver(pressing(q, choiceOnce, "U7").envelope())

			resumed := nextWork(ch)
			ended(resumed)

			again := aMention()
			again.EnvelopeID = "Ev2"
			again.ThreadTS = "1700000000.000100"
			again.TS = "1700000020.000100"
			again.Text = "<@U0BOT> try that again"

			socket.deliver(again.envelope())
			Eventually(socket.acked).Should(HaveLen(3))

			next := nextWork(ch)

			failures := make(chan error, 1)
			go func() {
				_, err := next.Prompter.ApproveCommand(callCtx("tu1"), gateFor("tu1"))
				failures <- err
			}()

			Eventually(questionsIn(api)).Should(Equal(1), "nothing outlived the resume, so the thread is asked again")
			Eventually(clock.waiting).Should(Equal(1))

			clock.advance(opts.AnswerGrace)
			Eventually(failures).Should(Receive(MatchError(toolkit.ErrPromptAborted)))
		})
	})

	Describe("A mention while a question is open", func() {
		var session string

		BeforeEach(func() {
			session = SessionFor(opts.Identity, "T1", "C1", "1700000000.000100")
		})

		// A conversation waiting on a deferred tool result reaches no boundary a user message
		// can join, so an ordinary turn there would be journaled as nothing. What the person
		// wrote is the answer instead, from anybody in the thread, which is who may press the
		// buttons.
		It("Should take a mention as the answer to a free-text question the run is waiting on", func() {
			ch := promptingChannel(opts, api, socket, clock)
			w := runningTurn(ch, socket)

			answers := make(chan string, 1)
			failures := make(chan error, 1)

			go func() {
				text, err := w.Prompter.Input(callCtx("tu1"), "which node should I drain?", "")
				failures <- err
				answers <- text
			}()

			var q *fakeMessage
			Eventually(func() *fakeMessage { q = questionIn(api)(); return q }).ShouldNot(BeNil())

			socket.deliver(inThread("1700000020.000100", "node4").envelope())

			Eventually(failures).Should(Receive(BeNil()))
			Eventually(answers).Should(Receive(Equal("node4")))
			Eventually(bodyOf(api, q.TS)).Should(ContainSubstring("Answered by <@U7>: node4"))
			noWork(ch)
		})

		// The run gave up hours ago, so the answer reaches the conversation the way a press
		// does: as a resume carrying the result of the call that deferred.
		It("Should resume a thread from a mention answering a free-text question that deferred", func() {
			ch := promptingChannel(opts, api, socket, clock)
			w := runningTurn(ch, socket)

			q := deferredQuestion(w, api, clock, opts.AnswerGrace, func(p toolkit.Prompter) error {
				_, err := p.Input(callCtx("tu1"), "which node should I drain?", "")

				return err
			})

			deferring(w, "tu1")
			abandoned(ch, "tu1")

			socket.deliver(inThread("1700000020.000100", "node4").envelope())

			content, err := builtin.InputResult("node4", "")
			Expect(err).ToNot(HaveOccurred())

			resumed := nextWork(ch)

			Expect(resumed.Checkpoint).To(Equal(agent.Checkpoint{
				ResumeID: session,
				Answer:   &agent.DeferredAnswer{ToolUseID: "tu1", Content: content},
				Force:    true,
			}))
			Expect(resumed.Caller).To(Equal(serve.Caller{Name: "cara/U7", Verified: true}), "whoever wrote it")
			Eventually(bodyOf(api, q.TS)).Should(ContainSubstring("Answered by <@U7>: node4"))
		})

		// A confirm, a selection and a gate each have a fixed set of answers, and prose cannot
		// be matched to one of them.
		DescribeTable("Should refuse a mention while a question it cannot answer is open",
			func(ask func(toolkit.Prompter) error) {
				ch := promptingChannel(opts, api, socket, clock)
				w := runningTurn(ch, socket)

				q := deferredQuestion(w, api, clock, opts.AnswerGrace, ask)

				deferring(w, "tu1")
				abandoned(ch, "tu1")

				socket.deliver(inThread("1700000020.000100", "just do it").envelope())

				link, ok := ch.permalink("C1", "1700000000.000100", q.TS)
				Expect(ok).To(BeTrue())

				pointer := fmt.Sprintf(openQuestionRefusal, fmt.Sprintf(openQuestionLinked, link))

				Eventually(postedLine(api, pointer)).Should(BeTrue(), "the refusal says where the question is")
				noWork(ch)
				Expect(buttonsOf(api, q.TS)()).ToNot(BeEmpty(), "the question the person was pointed at is still pressable")
			},
			Entry("a yes/no question", func(p toolkit.Prompter) error {
				_, err := p.Confirm(callCtx("tu1"), "restart node3?")

				return err
			}),
			Entry("a selection", func(p toolkit.Prompter) error {
				_, err := p.Select(callCtx("tu1"), "which node?", []string{"node3", "node4"})

				return err
			}),
			Entry("the confirm gate", func(p toolkit.Prompter) error {
				_, err := p.ApproveCommand(callCtx("tu1"), gateFor("tu1"))

				return err
			}),
		)

		// Two questions cannot be told apart by prose either, whichever kinds they are, so the
		// refusal names them together.
		It("Should refuse a mention while more than one question is open", func() {
			ch := promptingChannel(opts, api, socket, clock)
			w := runningTurn(ch, socket)

			failures := make(chan error, 2)
			for _, id := range []string{"tu1", "tu2"} {
				go func() {
					_, err := w.Prompter.Input(callCtx(id), "which node for "+id+"?", "")
					failures <- err
				}()
			}

			Eventually(questionsIn(api)).Should(Equal(2))
			Eventually(clock.waiting).Should(Equal(2))

			clock.advance(opts.AnswerGrace)
			Eventually(failures).Should(Receive(MatchError(toolkit.ErrDeferredResult)))
			Eventually(failures).Should(Receive(MatchError(toolkit.ErrDeferredResult)))

			deferring(w, "tu1", "tu2")
			abandoned(ch, "tu1")
			abandoned(ch, "tu2")

			socket.deliver(inThread("1700000020.000100", "node4").envelope())

			Eventually(postedLine(api, openQuestionsRefusal)).Should(BeTrue())
			noWork(ch)
		})

		// The thread is answered and free, so the mention is the ordinary turn it would have
		// been had nothing ever been asked.
		It("Should take a mention in a thread with nothing open as an ordinary turn", func() {
			ch := promptingChannel(opts, api, socket, clock)
			w := runningTurn(ch, socket)

			q := deferredQuestion(w, api, clock, opts.AnswerGrace, func(p toolkit.Prompter) error {
				_, err := p.Confirm(callCtx("tu1"), "restart node3?")

				return err
			})

			deferring(w, "tu1")
			abandoned(ch, "tu1")

			socket.deliver(pressing(q, choiceYes, "U7").envelope())
			ended(nextWork(ch))

			socket.deliver(inThread("1700000030.000100", "and now node4").envelope())

			next := nextWork(ch)
			Expect(next.Prompt).To(ContainSubstring("and now node4"))
			Expect(next.Checkpoint.Answer).To(BeNil())
		})
	})

	Describe("Dismissing a question the run gave up on", func() {
		var session string

		BeforeEach(func() {
			session = SessionFor(opts.Identity, "T1", "C1", "1700000000.000100")
		})

		// The conversation is waiting on the call, so what unblocks it is a result for that
		// call. Each tool has one for an operator who was reached and gave no answer, and the
		// dismissal supplies it with the reason.
		DescribeTable("Should answer the deferred call with its tool's own null result",
			func(tool string, ask func(toolkit.Prompter) error) {
				ch := promptingChannel(opts, api, socket, clock)
				w := runningTurn(ch, socket)

				q := deferredQuestion(w, api, clock, opts.AnswerGrace, ask)

				deferring(w, "tu1")
				abandoned(ch, "tu1")

				socket.deliver(pressing(q, choiceDismiss, "U7").envelope())

				content, err := builtin.NoAnswerResult(tool, dismissedReason)
				Expect(err).ToNot(HaveOccurred())

				resumed := nextWork(ch)

				Expect(resumed.Checkpoint).To(Equal(agent.Checkpoint{
					ResumeID: session,
					Answer:   &agent.DeferredAnswer{ToolUseID: "tu1", Content: content},
					Force:    true,
				}))
				Eventually(bodyOf(api, q.TS)).Should(ContainSubstring("Answered by <@U7>: " + answerDismissed))

				// The thread is answered and free, which is the whole point of dismissing.
				ended(resumed)

				socket.deliver(inThread("1700000030.000100", "carry on then").envelope())
				Expect(nextWork(ch).Prompt).To(ContainSubstring("carry on then"))
			},
			Entry("a yes/no question", builtin.AskHumanConfirmName, func(p toolkit.Prompter) error {
				_, err := p.Confirm(callCtx("tu1"), "restart node3?")

				return err
			}),
			Entry("a selection", builtin.AskHumanSelectName, func(p toolkit.Prompter) error {
				_, err := p.Select(callCtx("tu1"), "which node?", []string{"node3", "node4"})

				return err
			}),
			Entry("a free-text question", builtin.AskHumanInputName, func(p toolkit.Prompter) error {
				_, err := p.Input(callCtx("tu1"), "which node?", "")

				return err
			}),
		)

		// The gate's call was never dispatched, so there is nothing to supply a result to.
		// Declining it is what ends it: the resume dispatches the call, the gate is answered
		// from the press, and the guarded command does not run.
		It("Should end a gate question on Decline and leave the thread able to take a turn", func() {
			ch := promptingChannel(opts, api, socket, clock)
			w := runningTurn(ch, socket)

			q := deferredQuestion(w, api, clock, opts.AnswerGrace, func(p toolkit.Prompter) error {
				_, err := p.ApproveCommand(callCtx("tu1"), gateFor("tu1"))

				return err
			})

			deferring(w, "tu1")
			abandoned(ch, "tu1")

			socket.deliver(pressing(q, choiceNo, "U7").envelope())

			resumed := nextWork(ch)
			Expect(resumed.Checkpoint).To(Equal(agent.Checkpoint{ResumeID: session, Force: true}))

			got, err := resumed.Prompter.ApproveCommand(callCtx("tu1"), gateFor("tu1"))
			Expect(err).ToNot(HaveOccurred())
			Expect(got).To(Equal(toolkit.ConfirmNo), "answered from the press rather than asked again")

			ended(resumed)

			socket.deliver(inThread("1700000030.000100", "carry on then").envelope())
			Expect(nextWork(ch).Prompt).To(ContainSubstring("carry on then"))
		})
	})

	// The first post is held back and no edit after it is, so a press that resumes a run
	// with work left in it narrates that work as any other turn does.
	It("Should narrate a resume that gets somewhere", func() {
		ch := promptingChannel(opts, api, socket, clock)
		w := runningTurn(ch, socket)

		q := deferredQuestion(w, api, clock, opts.AnswerGrace, func(p toolkit.Prompter) error {
			_, err := p.Confirm(callCtx("tu1"), "restart node3?")

			return err
		})

		deferring(w, "tu1")
		abandoned(ch, "tu1")

		socket.deliver(pressing(q, choiceYes, "U7").envelope())

		resumed := nextWork(ch)
		resumed.Events.ToolCall(agent.ToolTrace{Name: "shell"})

		Eventually(postedLine(api, statusText(emojiTools, hintTools))).Should(BeTrue())
	})

	Describe("Two questions open at once", func() {
		// Outcome.Deferred is a list and Checkpoint.Answer is one answer, so answering the
		// first resumes, finds the second outstanding and defers again without asking
		// anything. A thread that collected a "Thinking..." for every answer somebody gave
		// would be unreadable, and both questions have to survive until both are answered.
		It("Should bank one answer without a new status message and leave the other pressable", func() {
			ch := promptingChannel(opts, api, socket, clock)
			w := runningTurn(ch, socket)

			failures := make(chan error, 2)
			for _, id := range []string{"tu1", "tu2"} {
				go func() {
					_, err := w.Prompter.Confirm(callCtx(id), "restart the node for "+id+"?")
					failures <- err
				}()
			}

			Eventually(questionsIn(api)).Should(Equal(2))
			Eventually(clock.waiting).Should(Equal(2))

			clock.advance(opts.AnswerGrace)
			Eventually(failures).Should(Receive(MatchError(toolkit.ErrDeferredResult)))
			Eventually(failures).Should(Receive(MatchError(toolkit.ErrDeferredResult)))

			deferring(w, "tu1", "tu2")
			abandoned(ch, "tu1")
			abandoned(ch, "tu2")

			first := questionFor(api, "tu1")()
			second := questionFor(api, "tu2")()
			Expect(first).ToNot(BeNil())
			Expect(second).ToNot(BeNil())

			said := len(api.messages())

			socket.deliver(pressing(first, choiceYes, "U7").envelope())

			resumed := nextWork(ch)
			Expect(resumed.Checkpoint.Answer.ToolUseID).To(Equal("tu1"))

			deferring(resumed, "tu2")

			Consistently(func() int { return len(api.messages()) }, 100*time.Millisecond).Should(Equal(said),
				"a resume that only banks an answer says nothing new in the thread")
			Expect(buttonsOf(api, second.TS)()).To(HaveLen(3), "the second question is still pressable")

			socket.deliver(pressing(second, choiceNo, "U7").envelope())

			again := nextWork(ch)
			Expect(again.Checkpoint.Answer.ToolUseID).To(Equal("tu2"))
		})
	})
})
