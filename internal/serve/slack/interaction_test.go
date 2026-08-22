//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package slack

import (
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
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

// forgotten waits until this worker holds no question for one call, which is what the turn
// that asked it reporting its outcome does.
func forgotten(ch *Channel, toolUseID string) {
	GinkgoHelper()

	Eventually(func() *question { return heldQuestion(ch, toolUseID) }).Should(BeNil())
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
			forgotten(ch, "tu1")

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
			Expect(resumed.Caller).To(Equal(serve.Caller{Name: "T1/U7", Verified: true}), "whoever pressed it")
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
			forgotten(ch, "tu1")

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
			forgotten(ch, "tu1")

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
			forgotten(ch, "tu1")

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
			Expect(buttonsOf(api, q.TS)()).To(HaveLen(2), "the press somebody was asked to make again has a button to be made on")

			_, _, out := ch.asked.deliver(clickFrom(pressing(q, choiceYes, "U7")))
			Expect(out).To(Equal(deliveryResume), "the question is where the click found it")
		})
	})
})
