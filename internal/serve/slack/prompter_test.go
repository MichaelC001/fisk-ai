//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/serve"
	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// promptingChannel serves a channel whose questions measure their grace window with the
// spec's own clock. The allowance is wide enough never to hold a call up, so the only thing
// parked on that clock is the question, and advancing it is the run giving up.
func promptingChannel(opts Options, a api, s *fakeSocket, cl *testClock) *Channel {
	GinkgoHelper()

	ch := newTestChannel(opts, a, s)
	ch.limit = newLimiter(time.Hour, 1000, cl)
	ch.clock = cl
	ch.start()

	Eventually(s.ran).Should(BeClosed())
	DeferCleanup(func() { Expect(ch.Close()).To(Succeed()) })
	DeferCleanup(cl.release)

	return ch
}

// runningTurn admits a mention and hands its work over, which is the state a run asks a
// question from.
func runningTurn(ch *Channel, s *fakeSocket) *serve.Work {
	GinkgoHelper()

	s.deliver(aMention().envelope())
	Eventually(s.acked).Should(HaveLen(1))

	return nextWork(ch)
}

// callCtx is the context a tool runs under, naming the call any question it asks belongs to.
func callCtx(toolUseID string) context.Context {
	return toolkit.ContextWithToolUseID(context.Background(), toolUseID)
}

// questionIn is the message this bot asked its question on, which is the one carrying
// buttons an answer is pressed on, and nil until it has been posted.
func questionIn(a *fakeAPI) func() *fakeMessage {
	return func() *fakeMessage {
		msgs := a.messages()

		for i := range msgs {
			if asks(msgs[i]) {
				return &msgs[i]
			}
		}

		return nil
	}
}

// asks reports whether one message carries the buttons a question is answered on. A status
// message carries a button of its own, which answers nothing.
func asks(m fakeMessage) bool {
	for _, b := range m.Buttons {
		if b.ActionID != stopActionID {
			return true
		}
	}

	return false
}

// bodyOf is what one message says now, so a spec waits for an edit rather than for a count.
func bodyOf(a *fakeAPI, ts string) func() string {
	return func() string {
		for _, m := range a.messages() {
			if m.TS == ts {
				return m.Text
			}
		}

		return ""
	}
}

// buttonsOf is what a person can still press on one message.
func buttonsOf(a *fakeAPI, ts string) func() []button {
	return func() []button {
		for _, m := range a.messages() {
			if m.TS == ts {
				return m.Buttons
			}
		}

		return nil
	}
}

// pressEvent builds the interaction envelope Slack delivers for a button press and for an
// answer typed into a question's own field, so a spec varies one field of a real payload
// rather than a hand-made struct.
//
// Typed says this is the second of those: Slack reports it as a block_actions like any
// press, with the action id the field was drawn with and the typed text as the action's
// value.
type pressEvent struct {
	EnvelopeID string
	Team       string
	Channel    string
	ThreadTS   string
	MessageTS  string
	User       string
	Value      string
	Typed      bool
	Text       string
}

func (p pressEvent) envelope() envelope {
	GinkgoHelper()

	action := map[string]any{
		"type":      "button",
		"block_id":  actionsBlockID,
		"action_id": "answer",
		"value":     p.Value,
	}

	if p.Typed {
		action = map[string]any{
			"type":      "plain_text_input",
			"block_id":  inputBlockID,
			"action_id": p.Value,
			"value":     p.Text,
		}
	}

	body, err := json.Marshal(map[string]any{
		"type":    "block_actions",
		"team":    map[string]any{"id": p.Team},
		"user":    map[string]any{"id": p.User},
		"channel": map[string]any{"id": p.Channel},
		"container": map[string]any{
			"type":       "message",
			"channel_id": p.Channel,
			"thread_ts":  p.ThreadTS,
			"message_ts": p.MessageTS,
		},
		"actions": []any{action},
	})
	Expect(err).ToNot(HaveOccurred())

	id := p.EnvelopeID
	if id == "" {
		id = "Ei1"
	}

	return envelope{ID: id, Kind: envelopeInteractive, Payload: body}
}

// pressing is the press of the button carrying one choice on a question this bot asked.
func pressing(m *fakeMessage, choice string, user string) pressEvent {
	GinkgoHelper()

	for _, b := range m.Buttons {
		v, err := decodeValue(b.Value)
		Expect(err).ToNot(HaveOccurred())

		if v.Choice != choice {
			continue
		}

		return pressEvent{
			Team:      "T1",
			Channel:   m.ChannelID,
			ThreadTS:  m.ThreadTS,
			MessageTS: m.TS,
			User:      user,
			Value:     b.Value,
		}
	}

	Fail("the question carries no button for " + choice)

	return pressEvent{}
}

// typing is somebody pressing enter in the field a free-text question carries.
func typing(m *fakeMessage, text string, user string) pressEvent {
	GinkgoHelper()

	Expect(m.Input).ToNot(BeNil(), "the question carries no field to type into")

	return pressEvent{
		Team:      "T1",
		Channel:   m.ChannelID,
		ThreadTS:  m.ThreadTS,
		MessageTS: m.TS,
		User:      user,
		Value:     m.Input.ActionID,
		Typed:     true,
		Text:      text,
	}
}

// heldQuestion is the question this worker is holding for one call.
func heldQuestion(ch *Channel, toolUseID string) *question {
	ch.asked.mu.Lock()
	defer ch.asked.mu.Unlock()

	return ch.asked.open[toolUseID]
}

var _ = Describe("The prompter", func() {
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

	It("Should report that a thread can be asked", func() {
		ch := promptingChannel(opts, api, socket, clock)
		w := runningTurn(ch, socket)

		Expect(w.Prompter.CanPrompt()).To(BeTrue())
		Expect(w.PromptsMayBlock).To(BeTrue(), "answer_grace is the only limit on a question, not the server's")
		Expect(w.PromptWait).To(BeZero())
	})

	Describe("A question answered inside the grace window", func() {
		It("Should put a yes/no question to the thread and answer the run with what was pressed", func() {
			ch := promptingChannel(opts, api, socket, clock)
			w := runningTurn(ch, socket)

			answers := make(chan bool, 1)
			failures := make(chan error, 1)

			go func() {
				ok, err := w.Prompter.Confirm(callCtx("tu1"), "restart node3?")
				failures <- err
				answers <- ok
			}()

			var q *fakeMessage
			Eventually(func() *fakeMessage { q = questionIn(api)(); return q }).ShouldNot(BeNil())

			Expect(q.Text).To(ContainSubstring("restart node3?"))
			Expect(q.Text).To(ContainSubstring(typedRepliesNote), "a bare reply in the thread reaches this worker not at all")
			Expect(q.ThreadTS).To(Equal("1700000000.000100"))
			Expect(q.Buttons).To(HaveLen(3), "yes, no and the Dismiss every question carries")

			socket.deliver(pressing(q, choiceYes, "U2").envelope())

			Eventually(failures).Should(Receive(BeNil()))
			Eventually(answers).Should(Receive(BeTrue()))
		})

		// The words are on the message and the buttons carry the number of the line they
		// answer, so an option longer than a button label reads in full.
		It("Should put the options in the message and answer with the index of the number pressed", func() {
			ch := promptingChannel(opts, api, socket, clock)
			w := runningTurn(ch, socket)

			long := []string{
				"node3, the one in Frankfurt that has been paging since Tuesday about its disk",
				"node4, the one in Dublin that nobody has touched since the upgrade last month",
			}

			answers := make(chan int, 1)
			failures := make(chan error, 1)

			go func() {
				idx, err := w.Prompter.Select(callCtx("tu1"), "which node?", long)
				failures <- err
				answers <- idx
			}()

			var q *fakeMessage
			Eventually(func() *fakeMessage { q = questionIn(api)(); return q }).ShouldNot(BeNil())

			Expect(q.Text).To(ContainSubstring("*1.* " + long[0]))
			Expect(q.Text).To(ContainSubstring("*2.* " + long[1]))
			Expect(len(long[1])).To(BeNumerically(">", maxButtonLabel), "an option no button label would hold")

			Expect(q.Buttons).To(HaveLen(3), "one numbered button per option, and Dismiss after them")
			Expect(q.Buttons[0].Label).To(Equal("1"))
			Expect(q.Buttons[1].Label).To(Equal("2"))
			Expect(q.Buttons[2].Label).To(Equal(labelDismiss))

			// The words travel in the value too, which is what a worker holding no question
			// answers the model with.
			v, err := decodeValue(q.Buttons[1].Value)
			Expect(err).ToNot(HaveOccurred())
			Expect(v.Choice).To(Equal("1"))
			Expect(v.Label).To(Equal(long[1]))

			socket.deliver(pressing(q, "1", "U2").envelope())

			Eventually(failures).Should(Receive(BeNil()))
			Eventually(answers).Should(Receive(Equal(1)))
		})

		// A button is minted before anybody has typed, so it cannot carry what they will
		// type. The field on the question's own message reports what they wrote when they
		// press enter in it.
		It("Should answer a free-text question with what was typed into the field on it", func() {
			ch := promptingChannel(opts, api, socket, clock)
			w := runningTurn(ch, socket)

			answers := make(chan string, 1)
			failures := make(chan error, 1)

			go func() {
				text, err := w.Prompter.Input(callCtx("tu1"), "which node should I drain?", "node3")
				failures <- err
				answers <- text
			}()

			var q *fakeMessage
			Eventually(func() *fakeMessage { q = questionIn(api)(); return q }).ShouldNot(BeNil())

			Expect(q.Buttons).To(HaveLen(1), "Dismiss, the answer being typed rather than pressed")
			Expect(q.Buttons[0].Label).To(Equal(labelDismiss))

			Expect(q.Input).ToNot(BeNil())
			Expect(q.Input.Initial).To(Equal("node3"), "the default the run supplied")

			// The element takes no value this channel mints, so what a button carries in its
			// value travels in the action id.
			v, err := decodeValue(q.Input.ActionID)
			Expect(err).ToNot(HaveOccurred())
			Expect(v.ToolUse).To(Equal("tu1"))
			Expect(v.Kind).To(Equal(kindInput))
			Expect(len(q.Input.ActionID)).To(BeNumerically("<=", maxActionID))

			socket.deliver(typing(q, "node4", "U2").envelope())

			Eventually(failures).Should(Receive(BeNil()))
			Eventually(answers).Should(Receive(Equal("node4")))
		})

		// An empty answer is one somebody gave, so pressing enter in an empty field is what
		// says a value arrived rather than the value itself.
		It("Should read an empty field as the answer it is", func() {
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

			socket.deliver(typing(q, "", "U2").envelope())

			Eventually(failures).Should(Receive(BeNil()))
			Eventually(answers).Should(Receive(Equal("")))
			Eventually(bodyOf(api, q.TS)).Should(ContainSubstring("Answered by <@U2>: _nothing_"))
		})

		DescribeTable("Should put the gate's three-way choice to the thread",
			func(choice string, expected toolkit.ConfirmChoice) {
				ch := promptingChannel(opts, api, socket, clock)
				w := runningTurn(ch, socket)

				answers := make(chan toolkit.ConfirmChoice, 1)
				failures := make(chan error, 1)

				go func() {
					got, err := w.Prompter.ApproveCommand(context.Background(), toolkit.GateRequest{
						ToolUseID: "tu1",
						Command:   "stream rm",
						Display:   "stream rm ORDERS",
						Tag:       "ai:confirm",
					})
					failures <- err
					answers <- got
				}()

				var q *fakeMessage
				Eventually(func() *fakeMessage { q = questionIn(api)(); return q }).ShouldNot(BeNil())

				Expect(q.Text).To(ContainSubstring("stream rm ORDERS"))
				Expect(q.Text).To(ContainSubstring("ai:confirm"))
				Expect(q.Buttons).To(HaveLen(3))

				socket.deliver(pressing(q, choice, "U2").envelope())

				Eventually(failures).Should(Receive(BeNil()))
				Eventually(answers).Should(Receive(Equal(expected)))
			},
			Entry("allowed once", choiceOnce, toolkit.ConfirmOnce),
			Entry("allowed for the rest of the conversation", choiceAlways, toolkit.ConfirmAlways),
			Entry("declined", choiceNo, toolkit.ConfirmNo),
		)

		// Anybody in the thread may answer, so who did is the one thing the message has to
		// say that nobody could work out from it.
		It("Should record the answer and who gave it on the question, and take the buttons off", func() {
			ch := promptingChannel(opts, api, socket, clock)
			w := runningTurn(ch, socket)

			go func() {
				_, _ = w.Prompter.Select(callCtx("tu1"), "which node?", []string{"node3", "node4"})
			}()

			var q *fakeMessage
			Eventually(func() *fakeMessage { q = questionIn(api)(); return q }).ShouldNot(BeNil())

			socket.deliver(pressing(q, "1", "U7").envelope())

			Eventually(bodyOf(api, q.TS)).Should(ContainSubstring("Answered by <@U7>: node4"))
			Eventually(buttonsOf(api, q.TS)).Should(BeEmpty(), "a settled question is not answered twice")
			Expect(bodyOf(api, q.TS)()).To(ContainSubstring("which node?"), "what was asked stays on the message")
		})

		// Escaping grows the string, so an answer of nothing but the three characters Slack
		// reads as markup is five times its own length once it is written back. A question
		// already at its own cap plus an answer cut before it was escaped is a message Slack
		// refuses, which would leave the buttons on a call that has been answered.
		It("Should record an answer of escaping characters inside the section Slack takes", func() {
			ch := promptingChannel(opts, api, socket, clock)
			w := runningTurn(ch, socket)

			go func() {
				_, _ = w.Prompter.Input(callCtx("tu1"), strings.Repeat("n", maxSectionText), "")
			}()

			var q *fakeMessage
			Eventually(func() *fakeMessage { q = questionIn(api)(); return q }).ShouldNot(BeNil())

			socket.deliver(typing(q, strings.Repeat("&", 500), "U7").envelope())

			// The cut lands two bytes into the fortieth ampersand, so it takes the whole of
			// that one rather than leaving &am on the message.
			Eventually(bodyOf(api, q.TS)).Should(ContainSubstring("Answered by <@U7>: " + strings.Repeat("&amp;", 39) + clipMarker))
			Expect(len(bodyOf(api, q.TS)())).To(BeNumerically("<=", maxSectionText))
		})

		// The choice comes off the click payload rather than out of the button this worker
		// minted, so a choice naming no option is whatever the press carried. Recording it
		// uncut takes the update past the section Slack accepts, which leaves the buttons on
		// a call that has been answered.
		It("Should record a choice naming no option inside the section Slack takes", func() {
			ch := promptingChannel(opts, api, socket, clock)
			w := runningTurn(ch, socket)

			options := make([]string, 25)
			for i := range options {
				options[i] = strings.Repeat("n", 300)
			}

			go func() {
				_, _ = w.Prompter.Select(callCtx("tu1"), strings.Repeat("n", maxSectionText), options)
			}()

			var q *fakeMessage
			Eventually(func() *fakeMessage { q = questionIn(api)(); return q }).ShouldNot(BeNil())

			press := pressing(q, "0", "U7")

			v, err := decodeValue(press.Value)
			Expect(err).ToNot(HaveOccurred())
			v.Choice = strings.Repeat("<", maxSectionText)

			press.Value, err = encodeValue(v)
			Expect(err).ToNot(HaveOccurred())

			socket.deliver(press.envelope())

			Eventually(bodyOf(api, q.TS)).Should(ContainSubstring("Answered by <@U7>: " + strings.Repeat("&lt;", 49) + clipMarker))
			Expect(len(bodyOf(api, q.TS)())).To(BeNumerically("<=", maxSectionText))
		})

		// The run blocks in the prompter, so a thread watching a status message that still
		// says Thinking cannot tell a turn waiting on a person from one that has hung.
		It("Should say the turn is waiting while a question is open", func() {
			ch := promptingChannel(opts, api, socket, clock)
			w := runningTurn(ch, socket)

			Eventually(textIn(api, "C1")).Should(Equal(statusText(emojiThinking, hintThinking)))

			go func() {
				_, _ = w.Prompter.Confirm(callCtx("tu1"), "restart node3?")
			}()

			Eventually(textIn(api, "C1")).Should(Equal(statusText(emojiAsking, hintWaiting)))

			var q *fakeMessage
			Eventually(func() *fakeMessage { q = questionIn(api)(); return q }).ShouldNot(BeNil())

			socket.deliver(pressing(q, choiceYes, "U2").envelope())

			Eventually(textIn(api, "C1")).Should(Equal(statusText(emojiThinking, hintThinking)), "back to where the run was when it asked")
		})
	})

	Describe("Dismissing a question", func() {
		// A question nobody wants to answer would otherwise hold the conversation until
		// somebody remembered it. Dismiss answers the call with the null result its own tool
		// produces for an operator who was reached and gave none, which is a result the model
		// reasons about rather than a tool failure.
		DescribeTable("Should report a dismissal to a run still waiting rather than an answer",
			func(ask func(toolkit.Prompter) error) {
				ch := promptingChannel(opts, api, socket, clock)
				w := runningTurn(ch, socket)

				failures := make(chan error, 1)
				go func() { failures <- ask(w.Prompter) }()

				var q *fakeMessage
				Eventually(func() *fakeMessage { q = questionIn(api)(); return q }).ShouldNot(BeNil())

				socket.deliver(pressing(q, choiceDismiss, "U7").envelope())

				var failed error
				Eventually(failures).Should(Receive(&failed))
				Expect(failed).To(MatchError(errDismissed))
				Expect(failed).ToNot(MatchError(toolkit.ErrPromptAborted), "the call is answered, not left open")
				Expect(failed).ToNot(MatchError(toolkit.ErrDeferredResult))

				Eventually(bodyOf(api, q.TS)).Should(ContainSubstring("Answered by <@U7>: " + answerDismissed))
				Eventually(buttonsOf(api, q.TS)).Should(BeEmpty())
			},
			Entry("a yes/no question", func(p toolkit.Prompter) error {
				_, err := p.Confirm(callCtx("tu1"), "restart node3?")

				return err
			}),
			Entry("a selection", func(p toolkit.Prompter) error {
				_, err := p.Select(callCtx("tu1"), "which node?", []string{"node3", "node4"})

				return err
			}),
			Entry("a free-text question", func(p toolkit.Prompter) error {
				_, err := p.Input(callCtx("tu1"), "which node?", "")

				return err
			}),
		)

		// The gated command does not run, which is the whole of what declining to answer a
		// gate can mean, so its Decline is its dismissal and there is no second button beside
		// it saying the same thing.
		It("Should end the confirm gate on the Decline it already carries", func() {
			ch := promptingChannel(opts, api, socket, clock)
			w := runningTurn(ch, socket)

			failures := make(chan error, 1)
			answers := make(chan toolkit.ConfirmChoice, 1)

			go func() {
				got, err := w.Prompter.ApproveCommand(callCtx("tu1"), gateFor("tu1"))
				failures <- err
				answers <- got
			}()

			var q *fakeMessage
			Eventually(func() *fakeMessage { q = questionIn(api)(); return q }).ShouldNot(BeNil())

			labels := make([]string, 0, len(q.Buttons))
			for _, b := range q.Buttons {
				labels = append(labels, b.Label)
			}
			Expect(labels).To(Equal([]string{labelOnce, labelAlways, labelDecline}))

			socket.deliver(pressing(q, choiceNo, "U7").envelope())

			Eventually(failures).Should(Receive(BeNil()))
			Eventually(answers).Should(Receive(Equal(toolkit.ConfirmNo)))
		})
	})

	Describe("A question nobody answers in time", func() {
		// The run ends at a resumable boundary and the worker is freed. A person answers in
		// a minute or on Thursday, and no worker can be held for the second case.
		DescribeTable("Should defer the call the answer would have gone to",
			func(ask func(toolkit.Prompter) error) {
				ch := promptingChannel(opts, api, socket, clock)
				w := runningTurn(ch, socket)

				failures := make(chan error, 1)
				go func() { failures <- ask(w.Prompter) }()

				Eventually(questionIn(api)).ShouldNot(BeNil())
				Eventually(clock.waiting).Should(Equal(1), "the question is the only thing parked on this clock")

				clock.advance(opts.AnswerGrace)

				var failed error
				Eventually(failures).Should(Receive(&failed))
				Expect(failed).To(MatchError(toolkit.ErrDeferredResult))

				deferred, ok := toolkit.IsDeferred(failed)
				Expect(ok).To(BeTrue())
				Expect(deferred.Handle).To(Equal("tu1"), "the call the later answer is delivered to")
			},
			Entry("a yes/no question", func(p toolkit.Prompter) error {
				_, err := p.Confirm(callCtx("tu1"), "restart node3?")

				return err
			}),
			Entry("a selection", func(p toolkit.Prompter) error {
				_, err := p.Select(callCtx("tu1"), "which node?", []string{"node3", "node4"})

				return err
			}),
			Entry("a free-text question", func(p toolkit.Prompter) error {
				_, err := p.Input(callCtx("tu1"), "which node?", "")

				return err
			}),
		)

		// The gate guards a command that has not run, and a deferred call is never
		// dispatched again: a deferring gate would mark the guarded command deferred and the
		// approval would arrive as that command's own result, so the command never runs.
		It("Should abort the gate rather than defer it, leaving the call to be dispatched again", func() {
			ch := promptingChannel(opts, api, socket, clock)
			w := runningTurn(ch, socket)

			failures := make(chan error, 1)
			answers := make(chan toolkit.ConfirmChoice, 1)

			go func() {
				got, err := w.Prompter.ApproveCommand(context.Background(), toolkit.GateRequest{
					ToolUseID: "tu1", Command: "stream rm", Display: "stream rm ORDERS", Tag: "ai:confirm",
				})
				failures <- err
				answers <- got
			}()

			Eventually(questionIn(api)).ShouldNot(BeNil())
			Eventually(clock.waiting).Should(Equal(1))

			clock.advance(opts.AnswerGrace)

			var failed error
			Eventually(failures).Should(Receive(&failed))
			Expect(failed).To(MatchError(toolkit.ErrPromptAborted))
			Expect(failed).ToNot(MatchError(toolkit.ErrDeferredResult))
			Eventually(answers).Should(Receive(Equal(toolkit.ConfirmNo)))
		})

		// The buttons stay on the message. Nothing has been answered, and the press that
		// arrives on Thursday is what the conversation is resumed from.
		It("Should leave the question standing so it can still be answered", func() {
			ch := promptingChannel(opts, api, socket, clock)
			w := runningTurn(ch, socket)

			failures := make(chan error, 1)
			go func() {
				_, err := w.Prompter.Confirm(callCtx("tu1"), "restart node3?")
				failures <- err
			}()

			var q *fakeMessage
			Eventually(func() *fakeMessage { q = questionIn(api)(); return q }).ShouldNot(BeNil())
			Eventually(clock.waiting).Should(Equal(1))

			clock.advance(opts.AnswerGrace)
			Eventually(failures).Should(Receive(MatchError(toolkit.ErrDeferredResult)))

			Consistently(buttonsOf(api, q.TS), 100*time.Millisecond).Should(HaveLen(3))
		})
	})

	Describe("The boundary between giving up and delivering", func() {
		// The two are one transition under one lock. Reporting a click as taken when the run
		// has stopped waiting would put the answer in a buffer nobody reads, and a thread has
		// no way to send it again: the buttons have been replaced by the answer the message
		// records.
		It("Should report a click that lands after the run gave up as one that has to become a resume", func() {
			ch := promptingChannel(opts, api, socket, clock)
			w := runningTurn(ch, socket)

			failures := make(chan error, 1)
			go func() {
				_, err := w.Prompter.Confirm(callCtx("tu1"), "restart node3?")
				failures <- err
			}()

			var q *fakeMessage
			Eventually(func() *fakeMessage { q = questionIn(api)(); return q }).ShouldNot(BeNil())
			Eventually(clock.waiting).Should(Equal(1))

			clock.advance(opts.AnswerGrace)
			Eventually(failures).Should(Receive(MatchError(toolkit.ErrDeferredResult)))

			held := heldQuestion(ch, "tu1")
			Expect(held).ToNot(BeNil(), "the question stays until the turn that asked it ends")

			in := pressing(q, choiceYes, "U2")
			_, _, out := ch.asked.deliver(&click{
				Interaction: interactionPress,
				TeamID:      in.Team,
				ChannelID:   in.Channel,
				ThreadTS:    in.ThreadTS,
				UserID:      in.User,
				Value:       buttonValue{Kind: kindConfirm, ToolUse: "tu1", Choice: choiceYes},
			})

			Expect(out).To(Equal(deliveryResume))
		})

		// The other order: the click won the race, so the run takes the answer rather than
		// deferring on a question that has already been answered.
		It("Should answer the run with a click that beat its own giving up", func() {
			ch := promptingChannel(opts, api, socket, clock)
			w := runningTurn(ch, socket)

			go func() {
				_, _ = w.Prompter.Confirm(callCtx("tu1"), "restart node3?")
			}()

			Eventually(questionIn(api)).ShouldNot(BeNil())

			var held *question
			Eventually(func() *question { held = heldQuestion(ch, "tu1"); return held }).ShouldNot(BeNil())

			_, _, out := ch.asked.deliver(&click{
				Interaction: interactionPress,
				TeamID:      "T1",
				ChannelID:   "C1",
				ThreadTS:    "1700000000.000100",
				UserID:      "U2",
				Value:       buttonValue{Kind: kindConfirm, ToolUse: "tu1", Choice: choiceYes},
			})
			Expect(out).To(Equal(deliveryTaken))

			given, beat := ch.asked.giveUp(held)
			Expect(beat).To(BeTrue(), "the click won, so the run takes the answer rather than deferring")
			Expect(given.Choice).To(Equal(choiceYes))
		})

		// End to end: the press lands between the run deferring and the turn reporting its
		// outcome, and the message says what happened rather than leaving somebody looking at
		// a button they pressed.
		It("Should record a late answer on its question and say the run had stopped waiting", func() {
			ch := promptingChannel(opts, api, socket, clock)
			w := runningTurn(ch, socket)

			failures := make(chan error, 1)
			go func() {
				_, err := w.Prompter.Confirm(callCtx("tu1"), "restart node3?")
				failures <- err
			}()

			var q *fakeMessage
			Eventually(func() *fakeMessage { q = questionIn(api)(); return q }).ShouldNot(BeNil())
			Eventually(clock.waiting).Should(Equal(1))

			clock.advance(opts.AnswerGrace)
			Eventually(failures).Should(Receive(MatchError(toolkit.ErrDeferredResult)))

			socket.deliver(pressing(q, choiceYes, "U7").envelope())

			Eventually(bodyOf(api, q.TS)).Should(ContainSubstring("Answered by <@U7>: Yes"))
			Expect(bodyOf(api, q.TS)()).To(ContainSubstring("I had already stopped waiting"))
			Eventually(buttonsOf(api, q.TS)).Should(BeEmpty())
		})

		// The turn that asked has ended, so nothing is waiting on the question. The thread
		// still is: a deferred call is answered there or by nothing at all, which is what the
		// next mention in that thread is decided against.
		It("Should leave a question standing past the turn that asked it", func() {
			ch := promptingChannel(opts, api, socket, clock)
			w := runningTurn(ch, socket)

			failures := make(chan error, 1)
			go func() {
				_, err := w.Prompter.Confirm(callCtx("tu1"), "restart node3?")
				failures <- err
			}()

			Eventually(questionIn(api)).ShouldNot(BeNil())
			Eventually(clock.waiting).Should(Equal(1))

			clock.advance(opts.AnswerGrace)
			Eventually(failures).Should(Receive(HaveOccurred()))

			ended(w)
			abandoned(ch, "tu1")

			Expect(ch.asked.openIn("C1", "1700000000.000100")).To(HaveLen(1))
		})

		// The bound is what stops a worker accumulating one entry for every question nobody
		// ever answered. Evicting one costs nothing a person sees: the press is built from
		// the value alone, which is the path a press after a restart takes.
		It("Should evict the oldest question once it is holding as many as it may", func() {
			ch := promptingChannel(opts, api, socket, clock)
			ch.asked = newQuestions(2)

			for _, id := range []string{"tu1", "tu2", "tu3"} {
				ch.asked.start(&question{
					kind: kindConfirm, toolUseID: id, turn: "t1",
					channelID: "C1", threadTS: "1700000000.000100",
					woken: make(chan struct{}, 1),
				})
			}

			Expect(heldQuestion(ch, "tu1")).To(BeNil(), "the oldest of three, where two is the bound")
			Expect(heldQuestion(ch, "tu3")).ToNot(BeNil())
			Expect(ch.asked.openIn("C1", "1700000000.000100")).To(HaveLen(2))
		})
	})

	Describe("A question this worker is not holding", func() {
		It("Should acknowledge the press and leave the question it does not know alone", func() {
			ch := promptingChannel(opts, api, socket, clock)
			w := runningTurn(ch, socket)

			answers := make(chan bool, 1)
			go func() {
				ok, _ := w.Prompter.Confirm(callCtx("tu1"), "restart node3?")
				answers <- ok
			}()

			var q *fakeMessage
			Eventually(func() *fakeMessage { q = questionIn(api)(); return q }).ShouldNot(BeNil())

			press := pressing(q, choiceYes, "U2")

			v, err := decodeValue(press.Value)
			Expect(err).ToNot(HaveOccurred())
			v.ToolUse = "tu-nobody-asked"

			press.Value, err = encodeValue(v)
			Expect(err).ToNot(HaveOccurred())

			socket.deliver(press.envelope())

			Eventually(socket.acked).Should(HaveLen(2), "an envelope Slack is waiting on is answered whether or not it reaches a question")
			Consistently(answers, 100*time.Millisecond).ShouldNot(Receive())
			Expect(buttonsOf(api, q.TS)()).To(HaveLen(3), "the question that is still open keeps its buttons")
		})

		It("Should refuse a press that names a question in another conversation", func() {
			ch := promptingChannel(opts, api, socket, clock)
			w := runningTurn(ch, socket)

			go func() {
				_, _ = w.Prompter.Confirm(callCtx("tu1"), "restart node3?")
			}()

			Eventually(questionIn(api)).ShouldNot(BeNil())
			Eventually(func() *question { return heldQuestion(ch, "tu1") }).ShouldNot(BeNil())

			_, _, out := ch.asked.deliver(&click{
				Interaction: interactionPress,
				TeamID:      "T1",
				ChannelID:   "C9",
				ThreadTS:    "1700000000.000100",
				UserID:      "U2",
				Value:       buttonValue{Kind: kindConfirm, ToolUse: "tu1", Choice: choiceYes},
			})

			Expect(out).To(Equal(deliveryElsewhere), "a value presented against a thread it was not minted in reaches nothing")
		})
	})

	// The button carries what the interaction envelope cannot, and nothing else. The session
	// is derived from the envelope's own authenticated team, channel and thread, so the value
	// is not a string anybody could present as a capability.
	It("Should mint a button value naming the call, the choice, the asker and the kind", func() {
		ch := promptingChannel(opts, api, socket, clock)
		w := runningTurn(ch, socket)

		go func() {
			_, _ = w.Prompter.Confirm(callCtx("tu1"), "restart node3?")
		}()

		var q *fakeMessage
		Eventually(func() *fakeMessage { q = questionIn(api)(); return q }).ShouldNot(BeNil())

		v, err := decodeValue(q.Buttons[0].Value)
		Expect(err).ToNot(HaveOccurred())

		Expect(v.Kind).To(Equal(kindConfirm))
		Expect(v.ToolUse).To(Equal("tu1"))
		Expect(v.Choice).To(Equal(choiceYes))
		Expect(v.Asker).To(Equal("U1"), "who the run put the question to, which a restarted worker holds no record of")

		Expect(q.Buttons[0].Value).ToNot(ContainSubstring(SessionFor(opts.Identity, "T1", "C1", "1700000000.000100")))
	})

	// A raw user id names nobody a reader recognizes. Slack's own mention markup renders as
	// that person's name, links them and notifies them, and costs no lookup.
	It("Should name the person it is asking with a mention", func() {
		ch := promptingChannel(opts, api, socket, clock)
		w := runningTurn(ch, socket)

		go func() {
			_, _ = w.Prompter.Confirm(callCtx("tu1"), "restart node3?")
		}()

		var q *fakeMessage
		Eventually(func() *fakeMessage { q = questionIn(api)(); return q }).ShouldNot(BeNil())

		Expect(q.Text).To(HavePrefix("<@U1> restart node3?"), "the asker, then what they are being asked")
		Expect(q.Text).ToNot(MatchRegexp(`(^|[^@<])U1\b`), "nowhere does it name them by the id itself")

		socket.deliver(pressing(q, choiceYes, "U7").envelope())

		Eventually(bodyOf(api, q.TS)).Should(ContainSubstring("Answered by <@U7>: Yes"), "and neither does the line recording who answered")
	})

	// The call is what the click is routed by and what the resume answers, so a question that
	// cannot name one would put buttons in a thread nothing could ever be delivered from.
	It("Should refuse to ask a question outside a tool call", func() {
		ch := promptingChannel(opts, api, socket, clock)
		w := runningTurn(ch, socket)

		_, err := w.Prompter.Confirm(context.Background(), "restart node3?")
		Expect(err).To(MatchError(ContainSubstring("outside a tool call")))
		Expect(err).ToNot(MatchError(toolkit.ErrPromptAborted), "nobody was asked, so nobody walked away from it")
	})
})

var _ = Describe("The words a question is asked in", func() {
	// ask_human_select allows twenty-five options and Slack refuses a section over 3000
	// characters rather than trimming it, so the budget is shared out and every option is
	// on the list a person chooses from.
	It("Should list every option of a long selection and stay inside one section", func() {
		options := make([]string, 25)
		for i := range options {
			options[i] = strings.Repeat("n", 300)
		}

		text := questionText("U1", "which node?", options)

		Expect(len(text)).To(BeNumerically("<=", maxSectionText))

		for i := range options {
			Expect(text).To(ContainSubstring(fmt.Sprintf("*%d.* ", i+1)))
		}
	})

	It("Should leave a question with no options as the words and the note", func() {
		text := questionText("U1", "restart node3?", nil)

		Expect(text).To(Equal("<@U1> restart node3?\n\n" + typedRepliesNote))
	})
})

var _ = Describe("Escaping what somebody else wrote", func() {
	It("Should take the markup characters out of a question", func() {
		Expect(escapeMrkdwn("drop <@U1> & everything > 5?")).To(Equal("drop &lt;@U1&gt; &amp; everything &gt; 5?"))
	})

	It("Should clip a string that is too long and mark where it stopped", func() {
		Expect(clipped("node3", 20)).To(Equal("node3"))
		Expect(clipped("restart every node", 10)).To(Equal("restart..."))
	})

	It("Should never clip a character in half", func() {
		out := clipped("世界世界世界", 8)
		Expect(out).To(Equal("世..."))
	})

	// Slack renders what it is given, so a cut between the ampersand and the semicolon puts
	// &am in the message where the answer had an ampersand.
	It("Should never clip escaped text inside an entity", func() {
		Expect(clippedMrkdwn("a&amp;b", 6)).To(Equal("a" + clipMarker))
		Expect(clippedMrkdwn("&amp;&amp;", 8)).To(Equal("&amp;" + clipMarker))
		Expect(clippedMrkdwn("&lt;&gt;", 7)).To(Equal("&lt;" + clipMarker))
	})

	// Escaping grows the string, so an answer cut before it is escaped comes out longer than
	// the room the question's own cap left for it.
	It("Should escape a typed answer before it is cut to fit", func() {
		q := &question{kind: kindInput}
		answer := q.reads(&given{Choice: choiceTyped, Text: strings.Repeat("&", maxAnswerText)})

		Expect(len(answer)).To(BeNumerically("<=", maxAnswerText))
		Expect(answer).To(Equal(strings.Repeat("&amp;", 39) + clipMarker))
	})

	It("Should escape a chosen option before it is cut to fit", func() {
		q := &question{kind: kindSelect, options: []string{strings.Repeat("<", maxAnswerText)}}
		answer := q.reads(&given{Choice: "0"})

		Expect(len(answer)).To(BeNumerically("<=", maxAnswerText))
		Expect(answer).To(Equal(strings.Repeat("&lt;", 49) + clipMarker))
	})

	// A choice this worker minted names one of the options, but what comes back is whatever
	// the payload carried, so the fallback is held to the same cap the option is.
	It("Should escape and cut a choice that names no option", func() {
		q := &question{kind: kindSelect, options: []string{"node3"}}
		answer := q.reads(&given{Choice: strings.Repeat("<", maxSectionText)})

		Expect(len(answer)).To(BeNumerically("<=", maxAnswerText))
		Expect(answer).To(Equal(strings.Repeat("&lt;", 49) + clipMarker))
	})
})
