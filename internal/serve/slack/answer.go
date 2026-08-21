//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package slack

import (
	"context"
	"fmt"
	"strings"
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
	ts, err := c.api.postMarkdown(ctx, t.m.ChannelID, t.m.ThreadTS, text)
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
