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

	// names is what userNames answers, and lookups counts the calls that reached it
	// rather than a cache above it.
	names   map[string]person
	lookups int

	// postErr and updateErr fail the next call of each, so a spec covers a Slack that
	// refuses.
	postErr   error
	updateErr error

	// historyErr, repliesErr and nameErr fail every call of each, for the reads a turn
	// makes before it runs.
	historyErr error
	repliesErr error
	nameErr    error

	// gate holds every call that talks to Slack until a spec releases it and arrivals
	// reports each one reaching that hold, which is how a spec keeps a message in flight
	// while it asserts on what waits for it.
	gate     chan struct{}
	arrivals chan struct{}
}

// fakeMessage is one message this bot posted, with every edit it received.
type fakeMessage struct {
	ChannelID string
	ThreadTS  string
	TS        string
	Text      string
	Edits     []string

	// Markdown records that this went as a markdown block for Slack to render, rather than
	// as text this channel wrote itself.
	Markdown bool

	// Buttons is what a person can press on it, empty for the messages that carry none and
	// for a question whose answer has been recorded.
	Buttons []button

	// Input is the field a free-text answer is typed into, nil for every message that
	// carries none and for a question that has been answered.
	Input *textInput
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{
		ws: workspace{
			URL:    "https://example.slack.com",
			Team:   "Example",
			TeamID: "T000",
			UserID: "U0BOT",
			User:   "NATS Docs",
		},
		posted:  map[string]*fakeMessage{},
		history: map[string][]message{},
		replies: map[string][]message{},
		names:   map[string]person{},
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

// hold makes every post and every edit wait, returning the release and the arrivals to wait
// on.
func (f *fakeAPI) hold() (release func(), arrivals chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.gate = make(chan struct{})
	f.arrivals = make(chan struct{}, 8)

	gate := f.gate

	return func() { close(gate) }, f.arrivals
}

// wait is where a call held by hold stops, reporting that it reached the hold before it
// blocks. A spec that set no hold passes straight through.
func (f *fakeAPI) wait() {
	f.mu.Lock()
	gate, arrivals := f.gate, f.arrivals
	f.mu.Unlock()

	if gate == nil {
		return
	}

	select {
	case arrivals <- struct{}{}:
	default:
	}

	<-gate
}

func (f *fakeAPI) postMessage(_ context.Context, channelID, threadTS, text string) (string, error) {
	return f.record(channelID, threadTS, text, false, nil, nil)
}

func (f *fakeAPI) postMarkdown(_ context.Context, channelID, threadTS, markdown string) (string, error) {
	return f.record(channelID, threadTS, markdown, true, nil, nil)
}

func (f *fakeAPI) postBlocks(_ context.Context, channelID, threadTS string, msg blockMessage) (string, error) {
	return f.record(channelID, threadTS, msg.Text, false, msg.Buttons, msg.Input)
}

// record is what every posting path does, differing only in whether Slack is being asked to
// render the text and in what a person can answer on the result.
func (f *fakeAPI) record(channelID, threadTS, text string, markdown bool, buttons []button, field *textInput) (string, error) {
	f.wait()

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.postErr != nil {
		err := f.postErr
		f.postErr = nil

		return "", err
	}

	f.nextTS++
	ts := fmt.Sprintf("%d.000100", 1700000000+f.nextTS)

	f.posted[ts] = &fakeMessage{ChannelID: channelID, ThreadTS: threadTS, TS: ts, Text: text, Markdown: markdown, Buttons: buttons, Input: field}
	f.order = append(f.order, ts)

	return ts, nil
}

func (f *fakeAPI) updateMessage(_ context.Context, channelID, ts, text string) error {
	return f.edit(channelID, ts, text, nil, nil)
}

func (f *fakeAPI) updateBlocks(_ context.Context, channelID, ts string, msg blockMessage) error {
	return f.edit(channelID, ts, msg.Text, msg.Buttons, msg.Input)
}

// edit is what both update paths do. The controls are replaced rather than added to, so a
// question that has been answered records what it lost.
func (f *fakeAPI) edit(channelID, ts, text string, buttons []button, field *textInput) error {
	f.wait()

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
	m.Buttons = buttons
	m.Input = field

	return nil
}

func (f *fakeAPI) threadReplies(_ context.Context, channelID, threadTS string, limit int) ([]message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.repliesErr != nil {
		return nil, f.repliesErr
	}

	return capped(f.replies[channelID+"/"+threadTS], limit), nil
}

func (f *fakeAPI) channelHistory(_ context.Context, channelID string, limit int) ([]message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.historyErr != nil {
		return nil, f.historyErr
	}

	return capped(f.history[channelID], limit), nil
}

// userNames answers from what a spec set, falling back to the id under both names the way
// a workspace that reports neither does.
func (f *fakeAPI) userNames(_ context.Context, userID string) (person, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.lookups++

	if f.nameErr != nil {
		return person{}, f.nameErr
	}

	p, ok := f.names[userID]
	if !ok {
		return person{Full: userID, Username: userID}, nil
	}

	if p.Full == "" {
		p.Full = userID
	}
	if p.Username == "" {
		p.Username = userID
	}

	return p, nil
}

// messages is every message this bot posted, in the order it posted them.
func (f *fakeAPI) messages() []fakeMessage {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]fakeMessage, 0, len(f.order))
	for _, ts := range f.order {
		out = append(out, *f.posted[ts])
	}

	return out
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

// deliver hands one envelope to whoever is reading, returning once it has been taken.
func (f *fakeSocket) deliver(env envelope) {
	f.out <- env
}

// acked is the envelopes that were acknowledged, in order.
func (f *fakeSocket) acked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.acks...)
}

// fail makes run answer with err rather than waiting for its context.
func (f *fakeSocket) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.runErr = err
}
