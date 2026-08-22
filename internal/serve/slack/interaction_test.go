//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package slack

import (
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

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
	})
})
