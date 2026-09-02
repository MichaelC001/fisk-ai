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
//
// The task client reaches it by asserting for the interface, so the assertion is what
// makes a missing method a build failure rather than a run whose questions go nowhere.
var _ StreamingTransport = (*heldTransport)(nil)

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

	// failAfter ends the set with a transport error instead of a terminal message, which
	// is the reply set becoming unreadable under whoever is answering.
	failAfter error

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
func (t *heldTransport) Serve(RouteHint, Handler) error                 { return nil }
func (t *heldTransport) ServeReplySet(RouteHint, ReplySetHandler) error { return nil }
func (t *heldTransport) Describe(string) []DescLine                     { return nil }
func (t *heldTransport) DescribeTasks(string, bool) []DescLine          { return nil }
func (t *heldTransport) Close() error                                   { return nil }

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
		messages:  t.prefix(&hdr),
		terminal:  t.terminal(&hdr),
		release:   t.release,
		endEarly:  t.endEarly,
		failAfter: t.failAfter,
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
	messages  [][]byte
	terminal  []byte
	release   chan struct{}
	endEarly  bool
	failAfter error
	at        int
	done      bool
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

	if r.failAfter != nil {
		return nil, r.failAfter
	}

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

	// ignoreCtx takes the pause without watching the context, which is what a prompter
	// owning a terminal in raw mode does: nothing but a keystroke ends it.
	ignoreCtx bool
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

	if h.pause > 0 && h.ignoreCtx {
		time.Sleep(h.pause)
	}

	if h.pause > 0 && !h.ignoreCtx {
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

	// The reported fault: a question left on screen for longer than the idle bound ended
	// the turn with "remote agent unavailable", having asked somebody a question and then
	// given up on them for taking too long to answer it.
	It("Should not end the turn while a question nobody has answered is on screen", func() {
		client.idle = 200 * time.Millisecond
		handler.pause = 500 * time.Millisecond
		handler.answer = func(ask *ElicitRequest) *ElicitReply {
			return NewApproveReply(ask, "caller1", ChoiceOnce)
		}

		// The waiting acks are answers on the wire too, so the set is held open until the
		// decision itself arrives, which is what a real worker does.
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
		Expect(out.Result.Text).To(Equal("removed"))
	})

	// Somebody who decides inside the window is the ordinary case, and the time they took
	// is not charged against the agent: at the shipped default an answer at a minute and
	// fifty would otherwise leave it ten seconds to say anything.
	It("Should give the agent a whole window from the answer, not what was left of one", func() {
		client.idle = 400 * time.Millisecond
		handler.pause = 300 * time.Millisecond
		handler.answer = func(ask *ElicitRequest) *ElicitReply {
			return NewApproveReply(ask, "caller1", ChoiceOnce)
		}

		// The agent speaks again 250ms after the answer, which is past the window the
		// question opened under and inside the one the answer starts.
		go func() {
			Eventually(func() bool {
				for _, a := range transport.sentAnswers() {
					if a.Answer == AnswerChoice {
						return true
					}
				}

				return false
			}, 2*time.Second).Should(BeTrue())

			time.Sleep(250 * time.Millisecond)
			close(transport.release)
		}()

		out, err := run(context.Background())
		Expect(err).ToNot(HaveOccurred())
		Expect(out.Result.Text).To(Equal("removed"))
	})

	// The other half of the same rule: the bound is lifted for the question rather than
	// for the rest of the set, so an agent that goes quiet after the answer reached it is
	// still given up on.
	It("Should bound the read again once the answer has gone", func() {
		client.idle = 150 * time.Millisecond
		handler.pause = 250 * time.Millisecond
		handler.answer = func(ask *ElicitRequest) *ElicitReply {
			return NewApproveReply(ask, "caller1", ChoiceOnce)
		}

		// The set is never released, so nothing follows the answer.
		_, err := run(context.Background())
		Expect(err).To(MatchError(ErrAgentUnavailable))

		answers := transport.sentAnswers()
		Expect(answers[len(answers)-1].Answer).To(Equal(AnswerChoice))
	})

	It("Should end the turn when the agent goes quiet with no question outstanding", func() {
		client.idle = 150 * time.Millisecond

		transport.prefix = func(req *Header) [][]byte {
			ack := NewAck(true)
			StampReply(&ack.Header, req, "svc")
			ack.Sequence = 1

			return [][]byte{encodeMessage(ack)}
		}

		_, err := run(context.Background())
		Expect(err).To(MatchError(ErrAgentUnavailable))
	})

	// The second half of the report the idle bound came from: the client should stay open
	// and, if the other end went away, still take the answer. A question is not torn off
	// the screen because the set under it ended, so what somebody was typing survives.
	It("Should leave a question on screen when the set ends under it", func() {
		transport.refuse = true
		handler.pause = 200 * time.Millisecond
		handler.answer = func(ask *ElicitRequest) *ElicitReply {
			return NewApproveReply(ask, "caller1", ChoiceOnce)
		}

		// The terminal message is already there, so the set ends while somebody is still
		// deciding rather than waiting for them.
		close(transport.release)

		out, err := run(context.Background())
		Expect(err).ToNot(HaveOccurred())
		Expect(out.Result.Text).To(Equal("removed"))

		Expect(out.Unsent).To(HaveLen(1))
		Expect(out.Unsent[0].ToolUseID).To(Equal("toolu_1"))
		Expect(out.Unsent[0].Choice).To(Equal(ChoiceOnce))
	})

	// The ending this item is for. A set that cannot be read is the error return, and the
	// outcome travels with it: discarding it would spend somebody's whole answer and then
	// throw the answer away, which is worse than never having asked them to finish.
	It("Should carry a held answer out of a set that could not be read", func() {
		transport.refuse = true
		transport.failAfter = fmt.Errorf("the subscription is gone")
		handler.pause = 200 * time.Millisecond
		handler.answer = func(ask *ElicitRequest) *ElicitReply {
			return NewApproveReply(ask, "caller1", ChoiceOnce)
		}

		close(transport.release)

		out, err := run(context.Background())
		Expect(err).To(MatchError("the subscription is gone"))
		Expect(out).ToNot(BeNil(), "the outcome comes back beside the error")
		Expect(out.Unsent).To(HaveLen(1))
		Expect(out.Unsent[0].ToolUseID).To(Equal("toolu_1"))
	})

	// Every waiting ack is refused once the set has ended, and the wait that follows one
	// is where a turn would otherwise become unendable: a prompter owning a terminal in
	// raw mode does not watch a context, so the caller's own ending has to be watched
	// here.
	It("Should end the wait when the context ends after a waiting ack was refused", func() {
		transport.refuse = true
		handler.pause = 3 * time.Second
		handler.ignoreCtx = true

		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			<-handler.asked
			time.Sleep(100 * time.Millisecond)
			cancel()
		}()
		defer cancel()

		started := time.Now()
		_, err := run(ctx)
		Expect(err).To(MatchError(context.Canceled))
		Expect(time.Since(started)).To(BeNumerically("<", time.Second), "it waits for the caller, not for the prompt")
	})

	// A surface with nobody to ask answers at once, so nothing holds the turn open for it
	// and it keeps the ending it has always had.
	It("Should not hold the turn open for a handler with nobody to ask", func() {
		transport.refuse = true
		handler.answer = func(ask *ElicitRequest) *ElicitReply {
			return NewNoOperatorReply(ask, "caller1")
		}

		close(transport.release)

		started := time.Now()
		out, err := run(context.Background())
		Expect(err).ToNot(HaveOccurred())
		Expect(out.Unsent).To(BeEmpty())
		Expect(time.Since(started)).To(BeNumerically("<", time.Second))
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
