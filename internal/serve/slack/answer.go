//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package slack

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"
)

// What a status message becomes once the turn has posted its answer.
//
// The linked form is Slack's own link markup, so the thread shows the words rather than
// the address. The plain one is what a workspace whose URL this bot never learned falls
// back to: a link that cannot be built is not worth losing the ending over.
const (
	doneLinked = "Done: <%s|see the answer>"
	donePlain  = "Done"
)

// stoppedNote is what a turn somebody pressed Stop on ends its status message with.
//
// A stopped run produces no text, so without this the message is left on whichever hint it
// had reached and the press looks like it did nothing. The conversation is journaled at the
// boundary the run parked on, which is what the second sentence is telling the person.
const stoppedNote = "Stopped. Mention me in this thread to carry on."

// markdownCap is the characters every markdown block in one payload shares, and cutWindow
// how far back from the cut a line or a word boundary is looked for.
//
// The cap is counted in bytes. Slack states it in characters and does not say which count
// it means, and a UTF-8 byte count is at or above every reading of that: one rune is one
// to four bytes and one to two UTF-16 units, so a message inside the cap in bytes is
// inside it however Slack counts.
const (
	markdownCap = 12000
	cutWindow   = 200
)

// answerCutNote ends an answer that did not fit, and spends characters of the same cap the
// answer is cut to.
const answerCutNote = "\n\n_The answer was too long for one Slack message and stops here._"

// answer posts what a turn produced and turns its status message into a pointer at it.
//
// The answer is its own message rather than the status message's last edit, which is the
// whole reason the two are separate. Slack sends no notification for an edit and does not
// mark a thread unread, so a turn that ended by editing "Thinking..." into its answer
// would have pinged somebody with "Thinking..." and then told them nothing at all.
//
// An empty answer posts nothing and leaves the status message on the state the turn ended
// in. chat.postMessage refuses a message with no text, and there is nothing to point at
// where nothing was posted.
func (c *Channel) answer(t *turn, text string) {
	if text == "" {
		return
	}

	// Slack refuses a markdown block past the cap rather than trimming it, and answer
	// returns on a refusal, so an answer that went out whole would leave the thread with a
	// status message saying the run is still working. It is cut before the allowance is
	// waited on, so the log line says what was dropped whether or not the message got out.
	body, cut := fitMarkdown(text, markdownCap)
	if cut {
		t.log.Warn("The answer was longer than one Slack message holds and was cut",
			"channel", t.m.ChannelID, "thread", t.m.ThreadTS, "bytes", len(text), "cap", markdownCap)
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultReplyDeadline)
	defer cancel()

	// The same allowance the running commentary and the refusals answer to: an answer is a
	// Tier 3 call like every other, counted for the app across the workspace.
	err := c.limit.take(ctx)
	if err != nil {
		t.log.Warn("Waiting for the allowance to post an answer failed", "error", err)

		return
	}

	// The answer is the model's own markdown, so Slack renders it rather than this channel
	// translating it. Everything else this channel posts is a sentence it wrote itself and
	// goes as text.
	ts, err := c.api.postMarkdown(ctx, t.m.ChannelID, t.m.ThreadTS, body)
	if err != nil {
		t.log.Warn("Posting an answer failed", "channel", t.m.ChannelID, "thread", t.m.ThreadTS, "error", err)

		return
	}

	link, ok := c.permalink(t.m.ChannelID, t.m.ThreadTS, ts)
	if !ok {
		t.status.ends(donePlain)

		return
	}

	t.status.ends(fmt.Sprintf(doneLinked, link))
}

// fitMarkdown answers with text and false where it is inside limit, and otherwise with a
// cut answer ending in answerCutNote and true.
//
// The answer is cut rather than split across several messages. Twelve thousand characters
// is already more than a thread usefully holds, and four messages are worse to read than
// one that stops and says it stopped. Where the rest of a long answer should go is not
// decided.
//
// A cut answer costs the text that survived the cut, the fence line that closes it and the
// note, all three inside limit. Closing the fence can push the message over, so the budget
// comes down by the overflow and the cut is taken again.
func fitMarkdown(text string, limit int) (string, bool) {
	if len(text) <= limit {
		return text, false
	}

	budget := limit - len(answerCutNote)

	for budget > 0 {
		kept := cutMarkdown(text, budget)
		closer := fenceCloser(kept)

		out := kept + closer + answerCutNote

		over := len(out) - limit
		if over <= 0 {
			return out, true
		}

		budget -= over
	}

	// A limit no answer can be said to have been cut within. Nothing calls it with one,
	// and a thread told the answer was too long reads better than a message Slack refuses.
	return strings.TrimLeft(answerCutNote, "\n"), true
}

// cutMarkdown is the first budget bytes of text, moved back to the nearest line or word
// boundary within cutWindow of it.
//
// A cut mid-word or mid-line reads as corruption, so the boundary is preferred wherever
// one is close by. A rune is never cut in half: the byte offset lands wherever the budget
// puts it, and the bytes of a partial rune at the end come off before anything else does.
func cutMarkdown(text string, budget int) string {
	if budget <= 0 {
		return ""
	}
	if len(text) <= budget {
		return text
	}

	kept := text[:budget]

	for len(kept) > 0 {
		r, size := utf8.DecodeLastRuneInString(kept)
		if r != utf8.RuneError || size > 1 {
			break
		}

		kept = kept[:len(kept)-1]
	}

	line := strings.LastIndexByte(kept, '\n')
	if line >= 0 && len(kept)-line <= cutWindow {
		return kept[:line]
	}

	word := strings.LastIndexByte(kept, ' ')
	if word >= 0 && len(kept)-word <= cutWindow {
		return kept[:word]
	}

	return kept
}

// fenceCloser is the line an unclosed fenced block in md needs, empty where every fence md
// opened was closed again.
//
// The block is closed rather than the cut being moved back to where the block started. A
// fence left open renders everything after it as prose, and an answer that ends in a long
// listing would lose the listing entirely if the cut moved to its opening line.
//
// A fence opens on three or more backticks or tildes at the start of a line, and closes on
// a line of the same character, at least as long, carrying nothing else. Anything else
// inside the block is content, including a fence of the other character.
func fenceCloser(md string) string {
	var open string

	for _, line := range strings.Split(md, "\n") {
		trimmed := strings.TrimLeft(line, " ")

		var marker string
		switch {
		case strings.HasPrefix(trimmed, "```"):
			marker = "`"
		case strings.HasPrefix(trimmed, "~~~"):
			marker = "~"
		default:
			continue
		}

		run := len(trimmed) - len(strings.TrimLeft(trimmed, marker))

		if open == "" {
			open = strings.Repeat(marker, run)
			continue
		}

		if !strings.HasPrefix(open, marker) || run < len(open) {
			continue
		}
		if strings.TrimSpace(trimmed[run:]) != "" {
			continue
		}

		open = ""
	}

	if open == "" {
		return ""
	}

	return "\n" + open
}

// permalink addresses one message this bot posted, reporting false where the workspace
// URL is not known and a caller has to say the ending without a link.
//
// It is built here rather than asked for. chat.getPermalink answers to the same allowance
// every status edit is spent from, and the address is a function of three strings this
// channel already holds: the workspace URL auth.test reported at construction, the
// channel, and the message's own timestamp.
//
// The archives form takes the timestamp with its dot removed, under a p. The thread and
// the channel travel as query parameters, which is what makes a client open the thread the
// answer is in rather than the channel around it: a permalink to a reply with neither
// lands a reader in the channel with the answer nowhere on screen. Slack mints all three
// values out of digits, a dot and its own id alphabet, so none of them needs escaping.
func (c *Channel) permalink(channelID string, threadTS string, ts string) (string, bool) {
	if c.workspace.URL == "" || channelID == "" || ts == "" {
		return "", false
	}

	link := fmt.Sprintf("%s/archives/%s/p%s", c.workspace.URL, channelID, strings.ReplaceAll(ts, ".", ""))
	if threadTS != "" {
		link += fmt.Sprintf("?thread_ts=%s&cid=%s", threadTS, channelID)
	}

	return link, true
}
