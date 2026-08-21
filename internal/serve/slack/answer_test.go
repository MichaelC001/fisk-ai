//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package slack

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/serve"
)

// answered reports a turn finished with something the agent said, which is what the answer
// message is built from.
func answered(w *serve.Work, text string) {
	GinkgoHelper()

	Expect(w.Done(context.Background(), serve.Outcome{ID: w.ID, Reason: runstate.ReasonCompleted, Text: text})).To(Succeed())
}

var _ = Describe("The answer message", func() {
	var (
		api     *fakeAPI
		socket  *fakeSocket
		opts    Options
		session string
	)

	BeforeEach(func() {
		api = newFakeAPI()
		socket = newFakeSocket()
		opts = testOptions()
		session = SessionFor(opts.Identity, "T1", "C1", "1700000000.000100")
	})

	// The status message is minted first and the answer second, so the fake's second
	// timestamp is the one a permalink names.
	const answerLink = "https://example.slack.com/archives/C1/p1700000002000100?thread_ts=1700000000.000100&cid=C1"

	// Slack sends no notification for an edit and does not mark a thread unread, so a turn
	// that ended by editing its own status message would ping somebody with "Thinking..."
	// and never tell them the answer had arrived.
	It("Should post the answer as its own message in the thread and point the status message at it", func() {
		ch := roomyChannel(opts, api, socket)

		socket.deliver(aMention().envelope())
		Eventually(textIn(api, "C1")).Should(Equal(hintThinking))

		w := nextWork(ch)
		answered(w, "node3 was full of journal logs")

		var posted []fakeMessage
		Eventually(func() []fakeMessage { posted = api.messages(); return posted }).Should(HaveLen(2))

		Expect(posted[1].Text).To(Equal("node3 was full of journal logs"))
		Expect(posted[1].ChannelID).To(Equal("C1"))
		Expect(posted[1].ThreadTS).To(Equal("1700000000.000100"), "in the thread the person asked in")
		Expect(posted[1].Edits).To(BeEmpty(), "an answer is written once")
		Expect(posted[1].Markdown).To(BeTrue(), "the model writes markdown and Slack renders it")

		Expect(posted[0].Markdown).To(BeFalse(), "the status message is a sentence this channel wrote itself")

		Eventually(editsIn(api, "C1")).Should(Equal([]string{"Done: <" + answerLink + "|see the answer>"}))
	})

	// The link is a function of three strings this channel already holds, so nothing here
	// spends a chat.getPermalink call against the allowance the edits come out of.
	Describe("The permalink", func() {
		var ch *Channel

		BeforeEach(func() {
			ch = newTestChannel(opts, api, socket)
			DeferCleanup(func() { Expect(ch.Close()).To(Succeed()) })
		})

		It("Should address the message in its thread under the workspace Slack reported", func() {
			link, ok := ch.permalink("C1", "1700000000.000100", "1700000002.000100")
			Expect(ok).To(BeTrue())
			Expect(link).To(Equal(answerLink))
		})

		It("Should name no thread for a message that is not in one", func() {
			link, ok := ch.permalink("C1", "", "1700000002.000100")
			Expect(ok).To(BeTrue())
			Expect(link).To(Equal("https://example.slack.com/archives/C1/p1700000002000100"))
		})

		It("Should report nothing it cannot build an address from", func() {
			_, ok := ch.permalink("", "1700000000.000100", "1700000002.000100")
			Expect(ok).To(BeFalse())

			_, ok = ch.permalink("C1", "1700000000.000100", "")
			Expect(ok).To(BeFalse())
		})
	})

	// A link that cannot be built is not worth losing the ending over: the answer is posted
	// either way and the thread is told the turn finished.
	It("Should end on a plain done line where the workspace URL is not known", func() {
		api.ws.URL = ""

		ch := roomyChannel(opts, api, socket)

		socket.deliver(aMention().envelope())
		Eventually(textIn(api, "C1")).Should(Equal(hintThinking))

		answered(nextWork(ch), "node3 was full of journal logs")

		Eventually(api.messages).Should(HaveLen(2))
		Eventually(editsIn(api, "C1")).Should(Equal([]string{donePlain}))
	})

	// no_progress takes the running commentary and nothing else. There is no message to
	// edit, and the answer is the whole reason the bot was mentioned.
	It("Should post the answer where there is no status message and edit nothing", func() {
		opts.Progress = false

		ch := roomyChannel(opts, api, socket)

		socket.deliver(aMention().envelope())
		Eventually(socket.acked).Should(HaveLen(1))

		w := nextWork(ch)
		Expect(statusOf(ch, session)).To(BeNil())

		answered(w, "node3 was full of journal logs")

		var posted []fakeMessage
		Eventually(func() []fakeMessage { posted = api.messages(); return posted }).Should(HaveLen(1))

		Expect(posted[0].Text).To(Equal("node3 was full of journal logs"))
		Expect(posted[0].ThreadTS).To(Equal("1700000000.000100"))
		Expect(posted[0].Edits).To(BeEmpty())
	})

	// Outcome.Text is empty whenever the agent said nothing, and a message with no text is
	// refused outright, so there is nothing to post and nothing to point at.
	It("Should post nothing for a turn that produced no text", func() {
		ch := roomyChannel(opts, api, socket)

		socket.deliver(aMention().envelope())
		Eventually(textIn(api, "C1")).Should(Equal(hintThinking))

		answered(nextWork(ch), "")

		Consistently(api.messages, 100*time.Millisecond).Should(HaveLen(1), "the status message and no answer beside it")
		Expect(editsIn(api, "C1")()).To(BeEmpty(), "and nothing pointing at a message that was never posted")
	})
})
