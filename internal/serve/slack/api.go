//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package slack

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	slackgo "github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
)

// api is the Slack Web API surface this channel calls, and socket is the socket mode
// envelope stream it reads. Everything else in this package is written against these two
// rather than against the client library, so a test drives the channel's own decisions
// without a network and without Slack's wire format.
//
// They are unexported because this package substitutes them for its own tests and
// nothing outside implements them: a caller wanting a different chat system writes a
// serve.Channel, which is the interface designed to be implemented.
type api interface {
	// authTest identifies the credential and the workspace it belongs to. It is called
	// once at construction, so a bad token fails at startup rather than on the first
	// mention.
	authTest(ctx context.Context) (workspace, error)

	// postMessage posts to a thread and returns the new message's timestamp. threadTS is
	// the thread to reply in and is never empty here: this bot only ever speaks in
	// threads.
	//
	// The text is sent as mrkdwn, which is what this channel writes for itself: plain
	// sentences with the occasional emphasis. An answer the model wrote goes through
	// postMarkdown instead.
	postMessage(ctx context.Context, channelID, threadTS, text string) (string, error)

	// postMarkdown posts standard markdown and lets Slack render it, returning the new
	// message's timestamp.
	//
	// The model writes markdown and mrkdwn is a different dialect: it has no headings and
	// no table syntax, and it spells bold and links differently, so an answer sent as text
	// arrives with its asterisks and brackets showing. The markdown block takes the
	// dialect the model already writes.
	//
	// All markdown blocks in one payload share 12,000 characters, against the 40,000 text
	// gets, and Slack refuses a longer one rather than trimming it. The caller cuts an
	// answer to fit; nothing here checks the length.
	postMarkdown(ctx context.Context, channelID, threadTS, markdown string) (string, error)

	// updateMessage replaces the text of a message this bot posted.
	updateMessage(ctx context.Context, channelID, ts, text string) error

	// threadReplies returns the last limit messages of a thread, oldest first, the parent
	// among them.
	//
	// The tail rather than the head, which costs the implementation something: Slack pages
	// a thread chronologically from its start, so reading the most recent messages of a
	// long one means paging to the end. The alternative is asking for a window with
	// latest, whose interaction with limit is not something this repository can verify
	// without a workspace to try it against, and a preload that silently read the wrong
	// end of an incident thread would be invisible until somebody noticed the bot
	// answering about an hour ago.
	threadReplies(ctx context.Context, channelID, threadTS string, limit int) ([]message, error)

	// channelHistory returns up to limit of a channel's most recent messages, oldest
	// first. Threaded replies are not among them, which is why an opening turn cannot
	// find a message of its own to stop at.
	channelHistory(ctx context.Context, channelID string, limit int) ([]message, error)

	// userDisplayName resolves a user id to what a person reading the thread sees.
	userDisplayName(ctx context.Context, userID string) (string, error)
}

// socket is the socket mode connection: it runs until its context ends, yields one
// envelope at a time, and takes an acknowledgement for each.
type socket interface {
	// run drives the connection and returns when ctx ends or when the connection cannot
	// be re-established.
	run(ctx context.Context) error

	// envelopes yields what arrives. It is closed when run returns.
	envelopes() <-chan envelope

	// ack acknowledges one envelope by its id. Slack redelivers an envelope that is not
	// acknowledged within three seconds, so nothing between reading and acking may block.
	ack(id string) error
}

// workspace is what authTest reports: enough to name this bot in a log line and to build
// a permalink without spending another call.
type workspace struct {
	// URL is the workspace's own address, which permalinks are built under.
	URL string
	// Team is the workspace's display name, and TeamID its identifier. The id is hashed
	// into every session, so a bot serving two workspaces keeps their conversations
	// apart.
	Team   string
	TeamID string
	// UserID is this bot's own user id, which is how its messages are told from a
	// person's when reading a thread back.
	UserID string
}

// message is one line of conversation read back from Slack, reduced to what a prompt
// needs.
type message struct {
	// UserID is who said it, empty for a message no user posted.
	UserID string
	// BotID is set when a bot posted it, so this bot's own messages and every other
	// bot's are recognizable without resolving the user.
	BotID string
	// Text is what was said, with Slack's markup left as it arrived.
	Text string
	// TS identifies the message within its channel and orders it against its siblings.
	TS string
	// ThreadTS names the thread it belongs to, empty for a message that is not in one.
	ThreadTS string
	// Subtype is Slack's own classification: empty for an ordinary message, and set for
	// the joins, the leaves and the broadcasts.
	Subtype string
}

// envelopeKind is what one socket mode envelope carries.
type envelopeKind int

const (
	// envelopeOther is an envelope this channel does not act on. It is acknowledged and
	// dropped.
	envelopeOther envelopeKind = iota
	// envelopeMention is an app_mention: somebody addressed this bot.
	envelopeMention
	// envelopeInteractive is a click on something this bot posted.
	envelopeInteractive
)

// envelope is one socket mode message, reduced to what this channel decides on.
//
// The raw payload travels with it rather than being decoded here, so the intake and the
// click path each read the shape they expect and this file stays a transport.
type envelope struct {
	// ID is what an acknowledgement names. It is empty for an envelope that takes none.
	ID string
	// Kind says which of the payloads below is worth decoding.
	Kind envelopeKind
	// Payload is the envelope's own body, undecoded.
	Payload []byte
	// RetryAttempt is greater than zero when Slack is redelivering. Delivery is
	// at-least-once, so a follow-up turn built from a redelivery would pay for the same
	// turn twice.
	RetryAttempt int
}

// clientAPI is api over the Slack client library.
type clientAPI struct {
	client *slackgo.Client
}

func (c *clientAPI) authTest(ctx context.Context) (workspace, error) {
	res, err := c.client.AuthTestContext(ctx)
	if err != nil {
		return workspace{}, fmt.Errorf("authenticating to Slack: %w", err)
	}

	return workspace{
		URL:    strings.TrimSuffix(res.URL, "/"),
		Team:   res.Team,
		TeamID: res.TeamID,
		UserID: res.UserID,
	}, nil
}

func (c *clientAPI) postMessage(ctx context.Context, channelID, threadTS, text string) (string, error) {
	_, ts, err := c.client.PostMessageContext(ctx, channelID,
		slackgo.MsgOptionText(text, false),
		slackgo.MsgOptionTS(threadTS),
		// Slack would otherwise turn a hostname or a path in an answer into a preview
		// card, which pushes the answer itself off the screen.
		slackgo.MsgOptionDisableLinkUnfurl(),
		slackgo.MsgOptionDisableMediaUnfurl(),
	)
	if err != nil {
		return "", fmt.Errorf("posting to %s: %w", channelID, err)
	}

	return ts, nil
}

func (c *clientAPI) postMarkdown(ctx context.Context, channelID, threadTS, markdown string) (string, error) {
	// The block id is discarded by Slack and nothing reads it back, so it is empty rather
	// than minted.
	_, ts, err := c.client.PostMessageContext(ctx, channelID,
		slackgo.MsgOptionBlocks(slackgo.NewMarkdownBlock("", markdown)),
		slackgo.MsgOptionTS(threadTS),
		// A message carrying blocks still takes a text argument, which is what a
		// notification and a client that cannot render blocks show. Slack warns when it is
		// missing, and the fallback a person reads on a phone should be the answer rather
		// than a placeholder.
		slackgo.MsgOptionText(markdown, false),
		slackgo.MsgOptionDisableLinkUnfurl(),
		slackgo.MsgOptionDisableMediaUnfurl(),
	)
	if err != nil {
		return "", fmt.Errorf("posting markdown to %s: %w", channelID, err)
	}

	return ts, nil
}

func (c *clientAPI) updateMessage(ctx context.Context, channelID, ts, text string) error {
	_, _, _, err := c.client.UpdateMessageContext(ctx, channelID, ts,
		slackgo.MsgOptionText(text, false),
		slackgo.MsgOptionDisableLinkUnfurl(),
		slackgo.MsgOptionDisableMediaUnfurl(),
	)
	if err != nil {
		return fmt.Errorf("updating %s in %s: %w", ts, channelID, err)
	}

	return nil
}

// threadPageSize is how many messages one page of a thread asks for, and threadMaxPages
// how many pages are read before the walk gives up and answers with what it has.
//
// The page bound exists so a thread nobody will stop growing cannot hold a turn open. A
// thread past it answers from the messages the last pages held, which is older than the
// true tail and still the right end of the conversation.
const (
	threadPageSize = 200
	threadMaxPages = 10
)

func (c *clientAPI) threadReplies(ctx context.Context, channelID, threadTS string, limit int) ([]message, error) {
	if limit <= 0 {
		return nil, nil
	}

	var (
		tail   []message
		cursor string
	)

	for range threadMaxPages {
		msgs, _, next, err := c.client.GetConversationRepliesContext(ctx, &slackgo.GetConversationRepliesParameters{
			ChannelID: channelID,
			Timestamp: threadTS,
			Limit:     threadPageSize,
			Cursor:    cursor,
		})
		if err != nil {
			return nil, fmt.Errorf("reading thread %s in %s: %w", threadTS, channelID, err)
		}

		for _, m := range msgs {
			tail = append(tail, messageOf(m.Msg))
		}

		// Only the last limit are wanted, so the head is dropped as the walk goes rather
		// than held to the end. A thread of ten thousand messages costs the pages and
		// nothing else.
		if len(tail) > limit {
			tail = append(tail[:0], tail[len(tail)-limit:]...)
		}

		if next == "" {
			break
		}
		cursor = next
	}

	return tail, nil
}

func (c *clientAPI) channelHistory(ctx context.Context, channelID string, limit int) ([]message, error) {
	res, err := c.client.GetConversationHistoryContext(ctx, &slackgo.GetConversationHistoryParameters{
		ChannelID: channelID,
		Limit:     limit,
	})
	if err != nil {
		return nil, fmt.Errorf("reading the history of %s: %w", channelID, err)
	}

	// Slack answers newest first and a prompt reads oldest first.
	out := make([]message, 0, len(res.Messages))
	for i := len(res.Messages) - 1; i >= 0; i-- {
		out = append(out, messageOf(res.Messages[i].Msg))
	}

	return out, nil
}

func (c *clientAPI) userDisplayName(ctx context.Context, userID string) (string, error) {
	u, err := c.client.GetUserInfoContext(ctx, userID)
	if err != nil {
		return "", fmt.Errorf("resolving user %s: %w", userID, err)
	}

	// The display name is what a person chose to be called and is often empty, in which
	// case the real name is what the client itself falls back to.
	if u.Profile.DisplayName != "" {
		return u.Profile.DisplayName, nil
	}
	if u.RealName != "" {
		return u.RealName, nil
	}

	return userID, nil
}

// messageOf reduces the library's message to the fields a prompt needs.
func messageOf(m slackgo.Msg) message {
	return message{
		UserID:   m.User,
		BotID:    m.BotID,
		Text:     m.Text,
		TS:       m.Timestamp,
		ThreadTS: m.ThreadTimestamp,
		Subtype:  m.SubType,
	}
}

// clientSocket is socket over the library's socket mode client.
//
// The library reconnects on its own and reports each attempt as an event, so a dropped
// connection travels no further than the log here. What ends the connection is a refused
// authentication, which run reports and the channel turns into a fault.
type clientSocket struct {
	client *socketmode.Client
	out    chan envelope
	fail   chan error
	log    *slog.Logger
}

func (s *clientSocket) envelopes() <-chan envelope { return s.out }

func (s *clientSocket) ack(id string) error {
	if id == "" {
		return nil
	}

	return s.client.Ack(socketmode.Request{EnvelopeID: id})
}

// run reads the library's event channel, translates what this channel acts on, and
// returns when the context ends or the credential is refused.
//
// A recorded refusal wins over whatever the client reports, since the client answers a
// closed connection the same way whether it was closed for that reason or another. A
// context that has ended is a shutdown somebody asked for and reports nothing.
func (s *clientSocket) run(ctx context.Context) error {
	go s.translate(ctx)

	err := s.client.RunContext(ctx)

	select {
	case failed := <-s.fail:
		return failed
	default:
	}

	if ctx.Err() != nil {
		return nil
	}

	return err
}

// translate turns the library's events into envelopes, reporting an invalid credential
// through fail so run answers with it rather than with the context's own error.
//
// It logs what the connection does as it happens, because a worker whose Slack app is
// misconfigured and one nobody has mentioned yet both sit there answering nothing.
func (s *clientSocket) translate(ctx context.Context) {
	defer close(s.out)

	for {
		select {
		case <-ctx.Done():
			return

		case ev, ok := <-s.client.Events:
			if !ok {
				return
			}

			if ev.Type == socketmode.EventTypeInvalidAuth {
				select {
				case s.fail <- fmt.Errorf("the Slack credentials were refused"):
				default:
				}

				return
			}

			s.report(ev)

			env, wanted := envelopeOf(ev)
			if !wanted {
				continue
			}

			select {
			case s.out <- env:
			case <-ctx.Done():
				return
			}
		}
	}
}

// report says what the connection is doing, so an operator can tell a worker that never
// reached Slack from one nobody has mentioned.
//
// The client reconnects on its own, so connecting and disconnecting happen often enough
// to log at Debug. Connected is Info: a worker that never logs it never reached the
// workspace, and a bot that answers nothing looks the same from a thread either way.
func (s *clientSocket) report(ev socketmode.Event) {
	if s.log == nil {
		return
	}

	switch ev.Type {
	case socketmode.EventTypeConnected:
		s.log.Info("Connected to Slack")
	case socketmode.EventTypeConnecting:
		s.log.Debug("Connecting to Slack")
	case socketmode.EventTypeDisconnect:
		s.log.Debug("The Slack connection dropped, reconnecting")
	case socketmode.EventTypeConnectionError, socketmode.EventTypeIncomingError, socketmode.EventTypeErrorWriteFailed, socketmode.EventTypeErrorBadMessage:
		s.log.Warn("The Slack connection reported an error", "event", string(ev.Type), "data", fmt.Sprint(ev.Data))
	}
}

// envelopeOf reduces one library event to an envelope, reporting whether it is one this
// channel reads. An envelope carrying no request is a connection notice rather than
// something Slack expects an answer to.
func envelopeOf(ev socketmode.Event) (envelope, bool) {
	if ev.Request == nil {
		return envelope{}, false
	}

	env := envelope{
		ID:           ev.Request.EnvelopeID,
		Payload:      ev.Request.Payload,
		RetryAttempt: ev.Request.RetryAttempt,
	}

	switch ev.Type {
	case socketmode.EventTypeEventsAPI:
		env.Kind = envelopeMention
	case socketmode.EventTypeInteractive:
		env.Kind = envelopeInteractive
	default:
		env.Kind = envelopeOther
	}

	return env, true
}
