//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// heldTransport is a StreamingTransport whose reply set stops before its terminal
// message until the test releases it, which is how a real worker behaves while it holds
// a question open: nothing else arrives until somebody answers.
type heldTransport struct {
	prefix   func(req *Header) [][]byte
	terminal func(req *Header) []byte
	release  chan struct{}

	// refuse makes every answer fail as though the run had ended under the person
	// typing it.
	refuse bool

	// endEarly stops the set after its prefix instead of carrying a terminal message,
	// which is what a worker that died mid-turn leaves behind.
	endEarly bool

	mu      sync.Mutex
	answers []*ElicitReply
	// requests is every request the client actually sent, so a spec can tell a prompt
	// that was rewritten from one that was not, and a task that was never sent at all.
	requests []*Request
}

// sentRequests returns what the client sent, which a spec reads while the loop may still
// be running.
func (t *heldTransport) sentRequests() []*Request {
	t.mu.Lock()
	defer t.mu.Unlock()

	return append([]*Request(nil), t.requests...)
}

func (t *heldTransport) RoundTrip(context.Context, string, RouteHint, []byte) ([]byte, error) {
	return nil, nil
}
func (t *heldTransport) Serve(RouteHint, Handler) error        { return nil }
func (t *heldTransport) Describe(string) []DescLine            { return nil }
func (t *heldTransport) DescribeTasks(string, bool) []DescLine { return nil }
func (t *heldTransport) Close() error                          { return nil }

func (t *heldTransport) WatchCancel(string, Handler) (TaskWatch, error) {
	return nil, fmt.Errorf("the held transport does not serve")
}

func (t *heldTransport) WatchElicitReplies(string, Handler) (TaskWatch, error) {
	return nil, fmt.Errorf("the held transport does not serve")
}

func (t *heldTransport) SendCancel(context.Context, string, string, []byte) ([]byte, error) {
	return nil, fmt.Errorf("the held transport takes no cancels")
}

func (t *heldTransport) Stream(_ context.Context, _ string, _ RouteHint, body []byte) (Reader, error) {
	var hdr Header
	err := json.Unmarshal(body, &hdr)
	if err != nil {
		return nil, err
	}

	var req Request
	err = json.Unmarshal(body, &req)
	if err != nil {
		return nil, err
	}

	t.mu.Lock()
	t.requests = append(t.requests, &req)
	t.mu.Unlock()

	return &heldReader{
		messages: t.prefix(&hdr),
		terminal: t.terminal(&hdr),
		release:  t.release,
		endEarly: t.endEarly,
	}, nil
}

// SendElicitReply records what the client answered, and answers the way a worker does:
// an accepted ack, or a refusal for a question it is no longer holding.
func (t *heldTransport) SendElicitReply(_ context.Context, _, _ string, body []byte) ([]byte, error) {
	var reply ElicitReply
	err := json.Unmarshal(body, &reply)
	if err != nil {
		return nil, err
	}

	t.mu.Lock()
	t.answers = append(t.answers, &reply)
	t.mu.Unlock()

	if t.refuse {
		return nil, ErrAgentUnavailable
	}

	ack := NewAck(true)
	StampReply(&ack.Header, &reply.Header, "svc")
	ack.Sequence = 1

	return encodeMessage(ack), nil
}

// sentAnswers returns what the client sent, which a spec reads while the loop may still
// be running.
func (t *heldTransport) sentAnswers() []*ElicitReply {
	t.mu.Lock()
	defer t.mu.Unlock()

	return append([]*ElicitReply(nil), t.answers...)
}

// heldReader yields its prefix, then waits for the release before the terminal message,
// so the set stays open exactly as long as the test wants it to.
type heldReader struct {
	messages [][]byte
	terminal []byte
	release  chan struct{}
	endEarly bool
	at       int
	done     bool
}

func (r *heldReader) Next(ctx context.Context) ([]byte, error) {
	if r.at < len(r.messages) {
		msg := r.messages[r.at]
		r.at++

		return msg, nil
	}

	if r.done {
		return nil, io.EOF
	}

	select {
	case <-r.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	r.done = true

	if r.endEarly {
		return nil, io.EOF
	}

	return r.terminal, nil
}

func (r *heldReader) Close() error { return nil }

// scriptedHandler renders into a slice and answers questions with what the spec set.
type scriptedHandler struct {
	mu     sync.Mutex
	blocks []Block

	// answer is what a question is answered with, after taking pause to decide.
	answer func(*ElicitRequest) *ElicitReply
	pause  time.Duration
	asked  chan struct{}
}

func (h *scriptedHandler) Block(b Block) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.blocks = append(h.blocks, b)
}

func (h *scriptedHandler) Question(ctx context.Context, ask *ElicitRequest) (*ElicitReply, error) {
	if h.asked != nil {
		close(h.asked)
	}

	if h.pause > 0 {
		select {
		case <-time.After(h.pause):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	if h.answer == nil {
		return NewNoOperatorReply(ask, "caller1"), nil
	}

	return h.answer(ask), nil
}

func (h *scriptedHandler) rendered() []Block {
	h.mu.Lock()
	defer h.mu.Unlock()

	return append([]Block(nil), h.blocks...)
}

var _ = Describe("RunTask", func() {
	var (
		transport *heldTransport
		client    *Client
		handler   *scriptedHandler
	)

	// A set of an ack, one event, a question, and a result once the question is
	// answered, which is the shape of every turn that asks something.
	BeforeEach(func() {
		transport = &heldTransport{release: make(chan struct{})}
		handler = &scriptedHandler{asked: make(chan struct{})}

		transport.prefix = func(req *Header) [][]byte {
			seq := uint64(0)
			stamp := func(hdr *Header) {
				StampReply(hdr, req, "svc")
				seq++
				hdr.Sequence = seq
			}

			ack := NewAck(true)
			ack.ConversationToken = "tok-1"
			stamp(&ack.Header)

			ev := NewEvent(NewTextBlock("working on it"))
			stamp(&ev.Header)

			ask := NewElicitRequest(ElicitApprove, "q-1")
			ask.ToolUseID = "toolu_1"
			ask.Command = "stream rm"
			ask.Display = "stream rm ORDERS"
			ask.WaitMS = 90
			stamp(&ask.Header)

			return [][]byte{encodeMessage(ack), encodeMessage(ev), encodeMessage(ask)}
		}

		transport.terminal = func(req *Header) []byte {
			res := NewResult(StopEndTurn)
			res.Text = "removed"
			StampReply(&res.Header, req, "svc")
			res.Sequence = 9

			return encodeMessage(res)
		}

		var err error
		client, err = NewClient(transport, "caller1")
		Expect(err).ToNot(HaveOccurred())
	})

	run := func(ctx context.Context) (*TaskOutcome, error) {
		return client.RunTask(ctx, "svc", NewRequest("remove the stream"), handler)
	}

	It("Should render the blocks, answer the question and return the result", func() {
		handler.answer = func(ask *ElicitRequest) *ElicitReply {
			return NewApproveReply(ask, "caller1", ChoiceOnce)
		}

		// The worker holds the set open until the answer reaches it, as a real one does.
		go func() {
			Eventually(transport.sentAnswers, time.Second).ShouldNot(BeEmpty())
			close(transport.release)
		}()

		out, err := run(context.Background())
		Expect(err).ToNot(HaveOccurred())

		Expect(out.Ack.ConversationToken).To(Equal("tok-1"), "the token a caller keeps to add a turn")
		Expect(out.Result.Text).To(Equal("removed"))
		Expect(out.Error).To(BeNil())
		Expect(out.Unsent).To(BeEmpty())

		Expect(out.Ack.Accepted).To(BeTrue())
		Expect(handler.rendered()).To(HaveLen(1))
		Expect(handler.rendered()[0].Content().(TextBlock).Text).To(Equal("working on it"))

		answers := transport.sentAnswers()
		Expect(answers[len(answers)-1].Answer).To(Equal(AnswerChoice))
		Expect(answers[len(answers)-1].Choice).To(Equal(ChoiceOnce))
	})

	// The window is what the worker holds a question for, and it restarts when the
	// caller says somebody is still reading. A client that says nothing loses the
	// question after one window, however fast the person is after that.
	It("Should say the question is still on screen while somebody decides", func() {
		handler.pause = 250 * time.Millisecond
		handler.answer = func(ask *ElicitRequest) *ElicitReply {
			return NewApproveReply(ask, "caller1", ChoiceOnce)
		}

		go func() {
			Eventually(func() bool {
				for _, a := range transport.sentAnswers() {
					if a.Answer == AnswerChoice {
						return true
					}
				}

				return false
			}, 2*time.Second).Should(BeTrue())
			close(transport.release)
		}()

		out, err := run(context.Background())
		Expect(err).ToNot(HaveOccurred())
		Expect(out.Result).ToNot(BeNil())

		// A 90ms window acks every 30ms, so a 250ms decision is held open by several.
		var waiting int
		for _, a := range transport.sentAnswers() {
			if a.Answer == AnswerWaiting {
				waiting++
			}
		}
		Expect(waiting).To(BeNumerically(">=", 2))

		// And they stop: the answer is the last thing sent, since one arriving after it
		// reaches a question the worker has finished with.
		last := transport.sentAnswers()
		Expect(last[len(last)-1].Answer).To(Equal(AnswerChoice))
	})

	// A person who was away answers a question the run gave up on. What they typed is
	// kept, in the shape a later request carries, rather than thrown away.
	It("Should hold an answer the run would not take", func() {
		transport.refuse = true
		handler.answer = func(ask *ElicitRequest) *ElicitReply {
			return NewApproveReply(ask, "caller1", ChoiceOnce)
		}

		go func() {
			Eventually(transport.sentAnswers, time.Second).ShouldNot(BeEmpty())
			close(transport.release)
		}()

		out, err := run(context.Background())
		Expect(err).ToNot(HaveOccurred())

		Expect(out.Unsent).To(HaveLen(1))
		Expect(out.Unsent[0].ToolUseID).To(Equal("toolu_1"), "an answer sent later names the call, not the question")
		Expect(out.Unsent[0].Kind).To(Equal(ElicitApprove))
		Expect(out.Unsent[0].Choice).To(Equal(ChoiceOnce))
	})

	// A no-operator reply is what a handler produces when it has nobody to ask, which
	// includes a prompt taken off the screen when the set ended. Delivering that later
	// would decline a gated command for somebody who was never shown the question, and
	// delivering it to a question already gone changes nothing either way.
	It("Should not hold an answer nobody gave", func() {
		transport.refuse = true
		handler.answer = func(ask *ElicitRequest) *ElicitReply {
			return NewNoOperatorReply(ask, "caller1")
		}

		go func() {
			Eventually(transport.sentAnswers, time.Second).ShouldNot(BeEmpty())
			close(transport.release)
		}()

		out, err := run(context.Background())
		Expect(err).ToNot(HaveOccurred())
		Expect(out.Unsent).To(BeEmpty())
	})

	// A terminal error is how a run ended, not a failure to read the set.
	It("Should return an ending that was not an answer", func() {
		transport.terminal = func(req *Header) []byte {
			msg := NewError("the run suspended and left a resumable session")
			msg.Code = "suspended"
			StampReply(&msg.Header, req, "svc")
			msg.Sequence = 9

			return encodeMessage(msg)
		}

		go func() {
			Eventually(transport.sentAnswers, time.Second).ShouldNot(BeEmpty())
			close(transport.release)
		}()

		out, err := run(context.Background())
		Expect(err).ToNot(HaveOccurred())
		Expect(out.Result).To(BeNil())
		Expect(out.Error.Code).To(Equal("suspended"))
	})
})
