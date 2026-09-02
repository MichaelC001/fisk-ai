//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package slack

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/serve"
	"github.com/choria-io/fisk-ai/internal/toolkit"
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
		Eventually(textIn(api, "C1")).Should(Equal(statusText(emojiThinking, hintThinking)))

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

		Eventually(editsIn(api, "C1")).Should(Equal([]string{emojiAnswered + " Done: <" + answerLink + "|see the answer>"}),
			"the emoji goes in front of the line, so Slack's link markup is still a link")
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
		Eventually(textIn(api, "C1")).Should(Equal(statusText(emojiThinking, hintThinking)))

		answered(nextWork(ch), "node3 was full of journal logs")

		Eventually(api.messages).Should(HaveLen(2))
		Eventually(editsIn(api, "C1")).Should(Equal([]string{statusText(emojiAnswered, donePlain)}))
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
		Eventually(textIn(api, "C1")).Should(Equal(statusText(emojiThinking, hintThinking)))

		status := api.messages()[0].TS

		answered(nextWork(ch), "")

		// The turn ended, so the edit that says so also takes the Stop button off a run
		// nothing can park any more.
		Eventually(buttonsOf(api, status)).Should(BeEmpty())

		Expect(api.messages()).To(HaveLen(1), "the status message and no answer beside it")
		Expect(textIn(api, "C1")()).To(Equal(statusText(emojiAnswered, silentNote)), "a run can end on tool calls alone, and the thread is told so")
	})

	// A stopped run produces no text either, and leaving the message on its last hint made
	// the press read as having done nothing.
	It("Should say a turn was stopped, and that the thread carries on", func() {
		ch := roomyChannel(opts, api, socket)

		socket.deliver(aMention().envelope())
		Eventually(textIn(api, "C1")).Should(Equal(statusText(emojiThinking, hintThinking)))

		w := nextWork(ch)

		ch.stopPressed(&click{
			ChannelID: "C1",
			ThreadTS:  "1700000000.000100",
			TeamID:    "T1",
			UserID:    "U2",
			Value:     buttonValue{Stop: w.ID},
		})

		Expect(w.SuspendRequested()).To(BeTrue())

		Expect(w.Done(context.Background(), serve.Outcome{ID: w.ID, Reason: runstate.ReasonSuspended})).To(Succeed())

		Eventually(textIn(api, "C1")).Should(Equal(statusText(emojiStopped, stoppedNote)))
		Expect(api.messages()).To(HaveLen(1), "nothing is posted beside it")
	})

	// Slack refuses the whole message rather than trimming it, and answer returns on a
	// refusal, so an uncut answer leaves the thread on a status message saying the run is
	// still working.
	It("Should cut an answer longer than the markdown block holds and say that it did", func() {
		ch := roomyChannel(opts, api, socket)

		socket.deliver(aMention().envelope())
		Eventually(textIn(api, "C1")).Should(Equal(statusText(emojiThinking, hintThinking)))

		answered(nextWork(ch), strings.Repeat("every node in the fleet was checked\n", 1000))

		var posted []fakeMessage
		Eventually(func() []fakeMessage { posted = api.messages(); return posted }).Should(HaveLen(2))

		Expect(len(posted[1].Text)).To(BeNumerically("<=", markdownCap))
		Expect(posted[1].Text).To(HaveSuffix(answerCutNote))
		Expect(posted[1].Text).To(HavePrefix("every node in the fleet was checked\n"))
		Expect(posted[1].Markdown).To(BeTrue())

		Eventually(editsIn(api, "C1")).Should(Equal([]string{emojiAnswered + " Done: <" + answerLink + "|see the answer>"}),
			"the emoji goes in front of the line, so Slack's link markup is still a link")
	})
})

var _ = Describe("How a turn ends", func() {
	var (
		api    *fakeAPI
		socket *fakeSocket
		opts   Options
	)

	BeforeEach(func() {
		api = newFakeAPI()
		socket = newFakeSocket()
		opts = testOptions()
	})

	// endedOn admits a mention, hands the turn over, reports the outcome it is given, and
	// answers with what that turn's status message says from then on.
	endedOn := func(ch *Channel, out serve.Outcome) func() string {
		GinkgoHelper()

		socket.deliver(aMention().envelope())
		Eventually(textIn(api, "C1")).Should(Equal(statusText(emojiThinking, hintThinking)))

		w := nextWork(ch)
		out.ID = w.ID

		Expect(w.Done(context.Background(), out)).To(Succeed())

		return textIn(api, "C1")
	}

	// The emoji is what a thread scrolled past is read by, so each row says which of the
	// four an ending takes as well as what it says.
	DescribeTable("Should say what became of a turn that produced no answer",
		func(out serve.Outcome, expected string, icon string) {
			ch := roomyChannel(opts, api, socket)

			Eventually(endedOn(ch, out)).Should(Equal(statusText(icon, expected)))
			Expect(api.messages()).To(HaveLen(1), "the status message, and nothing posted beside it")
		},
		// The stack is in the worker's log. A thread is told the turn ended and nothing
		// about where.
		Entry("a crash", serve.Outcome{Crashed: true, Err: fmt.Errorf("runtime error: index out of range [3]")}, crashedNote, emojiFailed),
		// Work the server took and never started, which is a drain that reached it first.
		Entry("work that never started", serve.Outcome{Abandoned: true}, abandonedNote, emojiStopped),
		// A budget stop reports a reason and an error, and the reason is the one that
		// decides: the allowance belongs to the conversation, so what it says is to start
		// another thread rather than to mention this one again.
		Entry("a conversation that used its allowance",
			serve.Outcome{Reason: runstate.ReasonBudget, Err: fmt.Errorf("this conversation has processed 210 of its 200 token budget (llm.budget.max_tokens)")},
			budgetNote, emojiStopped),
		// The question message is in the thread with its buttons on it, so this says why
		// the turn stopped moving.
		Entry("a run waiting on an answer",
			serve.Outcome{Reason: runstate.ReasonSuspended, Deferred: []agent.DeferredCall{{ToolUseID: "toolu_01deferred"}}},
			deferredNote, emojiAsking),
		// An aborted gate reports a suspend and an error wrapping ErrPromptAborted, which
		// is why the reasons are tested before the error is.
		Entry("a gate nobody approved in time",
			serve.Outcome{Reason: runstate.ReasonSuspended, Err: fmt.Errorf("%w: nobody in the thread answered inside the grace window", toolkit.ErrPromptAborted)},
			abortedNote, emojiStopped),
		// A cap reached is not a failure: the conversation is journaled where the next
		// mention carries on from.
		Entry("a run out of steps", serve.Outcome{Reason: runstate.ReasonMaxIterations, Err: fmt.Errorf("reached the iteration cap of 25")}, stepsNote, emojiStopped),
		Entry("a failure", serve.Outcome{Reason: runstate.ReasonError, Err: fmt.Errorf("the provider refused the request")}, failedNote, emojiFailed),
		// A run can end on tool calls alone, and chat.update refuses a message with no
		// text.
		Entry("a run that finished with nothing to say", serve.Outcome{Reason: runstate.ReasonCompleted}, silentNote, emojiAnswered),
	)

	// The two raced clicks a person can produce: pressing a button on a question the
	// conversation is no longer waiting on, and pressing one somebody else already answered.
	DescribeTable("Should say what a person can do about a failure they caused",
		func(err error, expected string) {
			ch := roomyChannel(opts, api, socket)

			Eventually(endedOn(ch, serve.Outcome{Reason: runstate.ReasonError, Err: err})).Should(Equal(statusText(emojiFailed, expected)))
		},
		Entry("an answer for a call nothing deferred",
			fmt.Errorf("cannot answer call %q of %q: %w", "toolu_01xyz", "s-abcdef", runstate.ErrNotDeferred), notDeferredNote),
		Entry("an answer for a call that has one",
			fmt.Errorf("cannot answer call %q of %q: %w", "toolu_01xyz", "s-abcdef", runstate.ErrAlreadyAnswered), alreadyAnsweredNote),
		Entry("a thread whose journal is gone",
			fmt.Errorf("cannot resume %q: %w", "s-abcdef", agent.ErrConversationNotFound), lostThreadNote),
	)

	// The provider maps its backend's refusal onto an llm sentinel and the runner wraps
	// that in "llm call: %w", so the sentinel is what the thread is read off rather than
	// the backend's own wording, which names a model, a status code and an account.
	DescribeTable("Should say what the model provider did in words a thread can use",
		func(err error, expected string) {
			ch := roomyChannel(opts, api, socket)

			Eventually(endedOn(ch, serve.Outcome{Reason: runstate.ReasonError, Err: err})).Should(Equal(statusText(emojiFailed, expected)))
		},
		Entry("a rate limit", fmt.Errorf("llm call: %w: 429 rate_limit_error", llm.ErrRateLimited), modelBusyNote),
		Entry("a provider with no capacity", fmt.Errorf("llm call: %w: 529 overloaded_error", llm.ErrOverloaded), modelBusyNote),
		Entry("rejected credentials", fmt.Errorf("llm call: %w: 401 authentication_error", llm.ErrAuthentication), modelUnusableNote),
		Entry("a model that does not exist", fmt.Errorf("llm call: %w: 404 not_found_error", llm.ErrModelNotFound), modelUnusableNote),
		Entry("a conversation past the context window", fmt.Errorf("llm call: %w: 400 invalid_request_error", llm.ErrContextLengthExceeded), threadTooLongNote),
		Entry("a journal another writer holds", fmt.Errorf("cannot resume %q: %w", "s-abcdef", runstate.ErrLocked), threadWorkingNote),
	)

	// The four above say what to do next; everything else says the turn failed and leaves
	// the detail in the worker's log.
	It("Should still say a failure it does not place failed", func() {
		ch := roomyChannel(opts, api, socket)

		Eventually(endedOn(ch, serve.Outcome{Reason: runstate.ReasonError, Err: fmt.Errorf("llm call: %w: 500 api_error", llm.ErrBackendFailure)})).
			Should(Equal(statusText(emojiFailed, failedNote)))
	})

	// The resume path builds its errors out of the session and the call they name, so
	// rendering Outcome.Err would publish exactly what every other decision here keeps out
	// of a thread.
	It("Should put no session, call or error text in the thread", func() {
		ch := roomyChannel(opts, api, socket)

		session := SessionFor(opts.Identity, "T1", "C1", "1700000000.000100")
		err := fmt.Errorf("cannot answer call %q of %q: %w", "toolu_01secret", session, runstate.ErrAlreadyAnswered)

		Eventually(endedOn(ch, serve.Outcome{SessionID: session, Reason: runstate.ReasonError, Err: err})).Should(Equal(statusText(emojiFailed, alreadyAnsweredNote)))

		for _, m := range api.messages() {
			Expect(m.Text).ToNot(ContainSubstring(session))
			Expect(m.Text).ToNot(ContainSubstring("toolu_01secret"))
			Expect(m.Text).ToNot(ContainSubstring("cannot answer call"))
		}
	})

	// A run that said something and then stopped has both to report. The answer is posted
	// as its own message either way, and the status message says how the turn ended rather
	// than pointing at an answer the thread already has.
	It("Should post what a stopped run said and still say it stopped", func() {
		ch := roomyChannel(opts, api, socket)

		text := endedOn(ch, serve.Outcome{Reason: runstate.ReasonMaxIterations, Text: "I got as far as node3"})

		Eventually(api.messages).Should(HaveLen(2))
		Expect(api.messages()[1].Text).To(Equal("I got as far as node3"))
		Eventually(text).Should(Equal(statusText(emojiStopped, stepsNote)))
	})

	// Three endings arrive as one reason, so what tells them apart is who asked for the
	// suspend.
	Describe("A suspend", func() {
		It("Should say the worker is going down where nothing in the thread asked for it", func() {
			opts.SuspendRequested = func() bool { return true }

			ch := roomyChannel(opts, api, socket)

			Eventually(endedOn(ch, serve.Outcome{Reason: runstate.ReasonSuspended})).Should(Equal(statusText(emojiStopped, drainedNote)))
		})

		// Somebody who pressed Stop while the worker happened to be draining is told what
		// they asked for, which is why the press is tested before the drain.
		It("Should say a press stopped it even while the worker is going down", func() {
			opts.SuspendRequested = func() bool { return true }

			ch := roomyChannel(opts, api, socket)

			socket.deliver(aMention().envelope())
			Eventually(textIn(api, "C1")).Should(Equal(statusText(emojiThinking, hintThinking)))

			w := nextWork(ch)

			// Pressed here rather than delivered over the socket, the worker's own drain
			// signal making SuspendRequested true whether or not the press has landed.
			ch.stopPressed(&click{
				ChannelID: "C1",
				ThreadTS:  "1700000000.000100",
				TeamID:    "T1",
				UserID:    "U2",
				Value:     buttonValue{Stop: w.ID},
			})

			Expect(w.Done(context.Background(), serve.Outcome{ID: w.ID, Reason: runstate.ReasonSuspended})).To(Succeed())

			Eventually(textIn(api, "C1")).Should(Equal(statusText(emojiStopped, stoppedNote)))
		})
	})
})

var _ = Describe("Fitting an answer to the markdown block", func() {
	It("Should answer an answer that fits with itself", func() {
		text := strings.Repeat("a", markdownCap)

		out, cut := fitMarkdown(text, markdownCap)
		Expect(cut).To(BeFalse())
		Expect(out).To(Equal(text))
	})

	// The note is inside the cap rather than beside it, so the message Slack is asked to
	// take is under the cap with the note counted.
	It("Should cut an answer past the cap and pay for the note out of the same budget", func() {
		text := strings.Repeat("word ", 4000)

		out, cut := fitMarkdown(text, markdownCap)
		Expect(cut).To(BeTrue())
		Expect(len(out)).To(BeNumerically("<=", markdownCap))
		Expect(len(out)).To(BeNumerically(">", markdownCap-cutWindow-len(answerCutNote)), "the cut spends the budget it has")
		Expect(out).To(HaveSuffix(answerCutNote))
	})

	// A cut mid-word reads as corruption, and a line boundary is what a reader sees as a
	// place the answer could have stopped.
	It("Should move the cut back to a line boundary where one is close by", func() {
		line := "the same line of the answer, over and over\n"

		out, cut := fitMarkdown(strings.Repeat(line, 1000), markdownCap)
		Expect(cut).To(BeTrue())

		body := strings.TrimSuffix(out, answerCutNote)
		for _, l := range strings.Split(body, "\n") {
			Expect(l + "\n").To(Equal(line))
		}
	})

	// A fence left open renders everything after it, the note included, as prose.
	It("Should close a fenced block the cut landed inside", func() {
		text := "here is what the disk was full of\n\n```\n" + strings.Repeat("/var/log/journal 8.1G\n", 1000)

		out, cut := fitMarkdown(text, markdownCap)
		Expect(cut).To(BeTrue())
		Expect(len(out)).To(BeNumerically("<=", markdownCap))
		Expect(out).To(HaveSuffix("\n```" + answerCutNote))
		Expect(strings.Count(out, "```")).To(Equal(2))
	})

	It("Should close a tilde fence with tildes, of the length that opened it", func() {
		text := "~~~~\n" + strings.Repeat("still inside the block\n", 1000)

		out, cut := fitMarkdown(text, markdownCap)
		Expect(cut).To(BeTrue())
		Expect(out).To(HaveSuffix("\n~~~~" + answerCutNote))
	})

	// A fence that closed before the cut needs no closer, and a fence of the other
	// character inside a block is content rather than its end.
	It("Should close nothing where every block the answer opened was closed", func() {
		text := "```\nfree -m\n~~~\n```\n" + strings.Repeat("prose after the block, at length. ", 1000)

		out, cut := fitMarkdown(text, markdownCap)
		Expect(cut).To(BeTrue())
		Expect(strings.Count(out, "```")).To(Equal(2))
		Expect(out).To(HaveSuffix(answerCutNote))
	})

	// The cap is counted in bytes, so a three byte rune straddles the budget rather than
	// ending on it, and half of one is not a character at all.
	It("Should never cut a character in half", func() {
		text := strings.Repeat("世", 6000)

		out, cut := fitMarkdown(text, markdownCap)
		Expect(cut).To(BeTrue())
		Expect(len(out)).To(BeNumerically("<=", markdownCap))
		Expect(utf8.ValidString(out)).To(BeTrue())
		Expect(strings.TrimSuffix(out, answerCutNote)).To(Equal(strings.Repeat("世", (markdownCap-len(answerCutNote))/3)))
	})
})
