//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package slack

import (
	"context"
	"fmt"
	"sync"
)

// fakeAPI answers the Web API calls this channel makes, recording what was posted so a
// spec asserts on what a person would have seen.
type fakeAPI struct {
	mu sync.Mutex

	// ws is what authTest answers, and authErr what it fails with instead.
	ws      workspace
	authErr error
	auths   int

	// posted holds every message, keyed by the timestamp it was given, and order records
	// the timestamps in the order they were posted.
	posted map[string]*fakeMessage
	order  []string
	nextTS int

	// history and replies are what the two read calls answer, keyed by channel and by
	// channel plus thread.
	history map[string][]message
	replies map[string][]message

	// names is what userDisplayName answers, and lookups counts the calls that reached
	// it rather than a cache above it.
	names   map[string]string
	lookups int

	// postErr and updateErr fail the next call of each, so a spec covers a Slack that
	// refuses.
	postErr   error
	updateErr error
}

// fakeMessage is one message this bot posted, with every edit it received.
type fakeMessage struct {
	ChannelID string
	ThreadTS  string
	Text      string
	Edits     []string
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{
		ws: workspace{
			URL:    "https://example.slack.com",
			Team:   "Example",
			TeamID: "T000",
			UserID: "U0BOT",
		},
		posted:  map[string]*fakeMessage{},
		history: map[string][]message{},
		replies: map[string][]message{},
		names:   map[string]string{},
	}
}

func (f *fakeAPI) authTest(context.Context) (workspace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.auths++

	if f.authErr != nil {
		return workspace{}, f.authErr
	}

	return f.ws, nil
}

func (f *fakeAPI) postMessage(_ context.Context, channelID, threadTS, text string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.postErr != nil {
		err := f.postErr
		f.postErr = nil

		return "", err
	}

	f.nextTS++
	ts := fmt.Sprintf("%d.000100", 1700000000+f.nextTS)

	f.posted[ts] = &fakeMessage{ChannelID: channelID, ThreadTS: threadTS, Text: text}
	f.order = append(f.order, ts)

	return ts, nil
}

func (f *fakeAPI) updateMessage(_ context.Context, channelID, ts, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.updateErr != nil {
		err := f.updateErr
		f.updateErr = nil

		return err
	}

	m, ok := f.posted[ts]
	if !ok {
		return fmt.Errorf("no message %s in %s", ts, channelID)
	}

	m.Edits = append(m.Edits, text)
	m.Text = text

	return nil
}

func (f *fakeAPI) threadReplies(_ context.Context, channelID, threadTS string, limit int) ([]message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return capped(f.replies[channelID+"/"+threadTS], limit), nil
}

func (f *fakeAPI) channelHistory(_ context.Context, channelID string, limit int) ([]message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return capped(f.history[channelID], limit), nil
}

func (f *fakeAPI) userDisplayName(_ context.Context, userID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.lookups++

	name, ok := f.names[userID]
	if !ok {
		return userID, nil
	}

	return name, nil
}

// capped keeps the last n of a slice, which is what both read calls do with a limit.
func capped(msgs []message, n int) []message {
	if n <= 0 || len(msgs) <= n {
		return msgs
	}

	return msgs[len(msgs)-n:]
}

// fakeSocket delivers envelopes a spec writes and records what was acknowledged.
type fakeSocket struct {
	mu sync.Mutex

	out  chan envelope
	acks []string

	// runErr is what run answers with instead of the context's own ending, which is how a
	// spec produces a refused credential.
	runErr error

	// ran is closed when run starts, so a spec can wait for the socket rather than sleep.
	ran     chan struct{}
	runOnce sync.Once
}

func newFakeSocket() *fakeSocket {
	return &fakeSocket{out: make(chan envelope), ran: make(chan struct{})}
}

func (f *fakeSocket) envelopes() <-chan envelope { return f.out }

func (f *fakeSocket) run(ctx context.Context) error {
	f.runOnce.Do(func() { close(f.ran) })

	f.mu.Lock()
	err := f.runErr
	f.mu.Unlock()

	if err != nil {
		return err
	}

	<-ctx.Done()

	return nil
}

func (f *fakeSocket) ack(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if id != "" {
		f.acks = append(f.acks, id)
	}

	return nil
}

// fail makes run answer with err rather than waiting for its context.
func (f *fakeSocket) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.runErr = err
}
