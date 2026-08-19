//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// hookRecorder keeps the order the hooks fired in, and what the last of each carried.
//
// It locks because QuestionAnswered fires from the goroutine that puts the question,
// while the reply set is still being read, which is the contract ClientHooks states.
type hookRecorder struct {
	mu    sync.Mutex
	order []string

	prompt   ClientPromptSubmitInfo
	started  ConversationInfo
	resumed  ConversationInfo
	accepted TurnAcceptedInfo
	refused  TurnRefusedInfo
	asked    QuestionAskedInfo
	answered QuestionAnsweredInfo
	ended    ClientTurnEndInfo
	canceled CancelRequestedInfo
}

func (r *hookRecorder) fired() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.order...)
}

// hooks wires every point to the recorder. deny, when set, is what PromptSubmit returns.
func (r *hookRecorder) hooks(submit func(ClientPromptSubmitInfo) (ClientPromptSubmitResult, error)) ClientHooks {
	return ClientHooks{
		PromptSubmit: func(_ context.Context, i ClientPromptSubmitInfo) (ClientPromptSubmitResult, error) {
			r.mu.Lock()
			r.prompt = i
			r.order = append(r.order, "PromptSubmit")
			r.mu.Unlock()

			if submit == nil {
				return ClientPromptSubmitResult{}, nil
			}

			return submit(i)
		},
		ConversationStart: func(_ context.Context, i ConversationInfo) {
			r.mu.Lock()
			r.started = i
			r.order = append(r.order, "ConversationStart")
			r.mu.Unlock()
		},
		ConversationResume: func(_ context.Context, i ConversationInfo) {
			r.mu.Lock()
			r.resumed = i
			r.order = append(r.order, "ConversationResume")
			r.mu.Unlock()
		},
		TurnAccepted: func(_ context.Context, i TurnAcceptedInfo) {
			r.mu.Lock()
			r.accepted = i
			r.order = append(r.order, "TurnAccepted")
			r.mu.Unlock()
		},
		TurnRefused: func(_ context.Context, i TurnRefusedInfo) {
			r.mu.Lock()
			r.refused = i
			r.order = append(r.order, "TurnRefused")
			r.mu.Unlock()
		},
		QuestionAsked: func(_ context.Context, i QuestionAskedInfo) {
			r.mu.Lock()
			r.asked = i
			r.order = append(r.order, "QuestionAsked")
			r.mu.Unlock()
		},
		QuestionAnswered: func(_ context.Context, i QuestionAnsweredInfo) {
			r.mu.Lock()
			r.answered = i
			r.order = append(r.order, "QuestionAnswered")
			r.mu.Unlock()
		},
		TurnEnd: func(_ context.Context, i ClientTurnEndInfo) {
			r.mu.Lock()
			r.ended = i
			r.order = append(r.order, "TurnEnd")
			r.mu.Unlock()
		},
		CancelRequested: func(_ context.Context, i CancelRequestedInfo) {
			r.mu.Lock()
			r.canceled = i
			r.order = append(r.order, "CancelRequested")
			r.mu.Unlock()
		},
	}
}

var _ = Describe("ClientHooks", func() {
	var (
		transport *heldTransport
		handler   *scriptedHandler
		rec       *hookRecorder
	)

	// The same set every RunTask spec uses: an ack, an event, a question, and a result
	// once the question is answered.
	BeforeEach(func() {
		transport = &heldTransport{release: make(chan struct{})}
		handler = &scriptedHandler{asked: make(chan struct{})}
		rec = &hookRecorder{}

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
			res.Usage = &Usage{InputTokens: 12, OutputTokens: 3}
			StampReply(&res.Header, req, "svc")
			res.Sequence = 9

			return encodeMessage(res)
		}

		handler.answer = func(ask *ElicitRequest) *ElicitReply {
			return NewApproveReply(ask, "caller1", ChoiceOnce)
		}
	})

	newClient := func(h ClientHooks) *Client {
		GinkgoHelper()

		c, err := NewClient(transport, "caller1", WithClientHooks(h))
		Expect(err).ToNot(HaveOccurred())

		return c
	}

	// The worker holds the set open until the answer reaches it, as a real one does.
	releaseOnAnswer := func() {
		go func() {
			defer GinkgoRecover()
			Eventually(transport.sentAnswers, time.Second).ShouldNot(BeEmpty())
			close(transport.release)
		}()
	}

	It("Should fire the whole lifecycle of a turn, in order", func() {
		releaseOnAnswer()

		client := newClient(rec.hooks(nil))
		out, err := client.RunTask(context.Background(), "svc", NewRequest("remove the stream"), handler)
		Expect(err).ToNot(HaveOccurred())
		Expect(out.Result).ToNot(BeNil())

		// QuestionAnswered fires from the question's own goroutine, so it may land
		// either side of TurnEnd; everything else is ordered by the reply set.
		Expect(rec.fired()).To(ContainElements("PromptSubmit", "ConversationStart", "TurnAccepted", "QuestionAsked", "QuestionAnswered", "TurnEnd"))
		Expect(rec.fired()).ToNot(ContainElement("ConversationResume"))
		Expect(rec.fired()).ToNot(ContainElement("TurnRefused"))

		order := rec.fired()
		Expect(order[0]).To(Equal("PromptSubmit"), "nothing fires before the prompt is cleared to go")
		Expect(order).To(HaveExactElements(
			ContainSubstring("PromptSubmit"),
			ContainSubstring("ConversationStart"),
			ContainSubstring("TurnAccepted"),
			ContainSubstring("QuestionAsked"),
			Or(ContainSubstring("QuestionAnswered"), ContainSubstring("TurnEnd")),
			Or(ContainSubstring("QuestionAnswered"), ContainSubstring("TurnEnd")),
		))
	})

	It("Should carry what each point knows", func() {
		releaseOnAnswer()

		client := newClient(rec.hooks(nil))
		req := NewRequest("remove the stream")
		_, err := client.RunTask(context.Background(), "svc", req, handler)
		Expect(err).ToNot(HaveOccurred())

		Expect(rec.prompt.Agent).To(Equal("svc"))
		Expect(rec.prompt.Prompt).To(Equal("remove the stream"))
		Expect(rec.prompt.Conversation).To(BeEmpty(), "a first turn names no conversation")

		Expect(rec.started.Conversation).To(Equal("tok-1"), "the token the agent just issued")
		Expect(rec.accepted.Conversation).To(Equal("tok-1"))

		Expect(rec.asked.QuestionID).To(Equal("q-1"))
		Expect(rec.asked.ToolUseID).To(Equal("toolu_1"), "so a caller can pair it with the call it drew")
		Expect(rec.asked.Kind).To(Equal(ElicitApprove))
		Expect(rec.asked.Display).To(Equal("stream rm ORDERS"))

		Expect(rec.answered.QuestionID).To(Equal("q-1"))
		Expect(rec.answered.Answered).To(BeTrue())
		Expect(rec.answered.Held).To(BeFalse())

		Expect(rec.ended.Answered).To(BeTrue())
		Expect(rec.ended.Code).To(BeEmpty(), "an answer has no terminal code")
		Expect(rec.ended.StopReason).To(Equal(StopEndTurn))
		Expect(rec.ended.Usage).ToNot(BeNil())
		Expect(rec.ended.Usage.InputTokens).To(Equal(int64(12)))
	})

	// A request naming a conversation is continuing one the caller already had, which is
	// the request's own claim rather than something the ack reveals.
	It("Should report a resume rather than a start when the request names a conversation", func() {
		releaseOnAnswer()

		client := newClient(rec.hooks(nil))
		req := NewRequest("and the other one")
		req.ConversationToken = "tok-existing"
		_, err := client.RunTask(context.Background(), "svc", req, handler)
		Expect(err).ToNot(HaveOccurred())

		Expect(rec.fired()).To(ContainElement("ConversationResume"))
		Expect(rec.fired()).ToNot(ContainElement("ConversationStart"))
		Expect(rec.resumed.Conversation).To(Equal("tok-existing"))
		Expect(rec.prompt.Conversation).To(Equal("tok-existing"))
	})

	Describe("A refused turn", func() {
		BeforeEach(func() {
			transport.prefix = func(req *Header) [][]byte {
				ack := NewAck(false)
				ack.Reason = "the agent is already running as much as it will at once"
				StampReply(&ack.Header, req, "svc")
				ack.Sequence = 1

				return [][]byte{encodeMessage(ack)}
			}

			transport.terminal = func(req *Header) []byte {
				msg := NewError("no free slot")
				msg.Code = CodeCapacity
				StampReply(&msg.Header, req, "svc")
				msg.Sequence = 2

				return encodeMessage(msg)
			}

			close(transport.release)
		})

		It("Should report the refusal and not an acceptance", func() {
			client := newClient(rec.hooks(nil))
			out, err := client.RunTask(context.Background(), "svc", NewRequest("remove the stream"), handler)
			Expect(err).ToNot(HaveOccurred())
			Expect(out.Error).ToNot(BeNil())

			Expect(rec.fired()).To(ContainElement("TurnRefused"))
			Expect(rec.fired()).ToNot(ContainElement("TurnAccepted"))
			Expect(rec.fired()).ToNot(ContainElement("ConversationStart"), "a refused turn opened nothing")
			Expect(rec.refused.Reason).To(ContainSubstring("already running"))

			// The ending still reports, carrying the code that says whether another
			// attempt is worth making.
			Expect(rec.ended.Answered).To(BeFalse())
			Expect(rec.ended.Code).To(Equal(CodeCapacity))
		})
	})

	Describe("PromptSubmit", func() {
		It("Should stop the task before anything is sent when it denies", func() {
			client := newClient(rec.hooks(func(ClientPromptSubmitInfo) (ClientPromptSubmitResult, error) {
				return ClientPromptSubmitResult{Deny: true, DenyReason: "it carried a secret"}, nil
			}))

			out, err := client.RunTask(context.Background(), "svc", NewRequest("the password is hunter2"), handler)
			Expect(out).To(BeNil())
			Expect(err).To(MatchError(ErrPromptDenied))
			Expect(err).To(MatchError(ContainSubstring("it carried a secret")))

			Expect(transport.sentRequests()).To(BeEmpty(), "nothing reached the agent")
			Expect(rec.fired()).To(HaveExactElements("PromptSubmit"))
		})

		It("Should send what it rewrote rather than what the caller passed", func() {
			releaseOnAnswer()

			client := newClient(rec.hooks(func(ClientPromptSubmitInfo) (ClientPromptSubmitResult, error) {
				return ClientPromptSubmitResult{Prompt: "the password is [redacted]"}, nil
			}))

			_, err := client.RunTask(context.Background(), "svc", NewRequest("the password is hunter2"), handler)
			Expect(err).ToNot(HaveOccurred())

			sent := transport.sentRequests()
			Expect(sent).To(HaveLen(1))
			Expect(sent[0].Prompt).To(Equal("the password is [redacted]"))
			Expect(sent[0].Prompt).ToNot(ContainSubstring("hunter2"), "the secret never left the machine")
		})

		It("Should return a hook error to the caller without sending", func() {
			client := newClient(rec.hooks(func(ClientPromptSubmitInfo) (ClientPromptSubmitResult, error) {
				return ClientPromptSubmitResult{}, fmt.Errorf("the policy service is down")
			}))

			_, err := client.RunTask(context.Background(), "svc", NewRequest("remove the stream"), handler)
			Expect(err).To(MatchError(ContainSubstring("PromptSubmit hook")))
			Expect(err).To(MatchError(ContainSubstring("policy service is down")))
			Expect(transport.sentRequests()).To(BeEmpty())
		})

		// A resume, a read and an answer submit nothing, so there is nothing to vet and
		// firing would report a prompt that does not exist.
		It("Should not fire for a request carrying no prompt", func() {
			releaseOnAnswer()

			client := newClient(rec.hooks(nil))
			req := NewRequest("")
			req.ConversationToken = "tok-existing"
			_, err := client.RunTask(context.Background(), "svc", req, handler)
			Expect(err).ToNot(HaveOccurred())

			Expect(rec.fired()).ToNot(ContainElement("PromptSubmit"))
			Expect(rec.fired()).To(ContainElement("ConversationResume"))
		})
	})

	// The zero value is what every caller that set no hooks has, so it must be inert
	// rather than a nil call.
	It("Should do nothing and panic on nothing with no hooks set", func() {
		releaseOnAnswer()

		client, err := NewClient(transport, "caller1")
		Expect(err).ToNot(HaveOccurred())

		out, err := client.RunTask(context.Background(), "svc", NewRequest("remove the stream"), handler)
		Expect(err).ToNot(HaveOccurred())
		Expect(out.Result).ToNot(BeNil())
	})
})

var _ = Describe("ErrNoResponders", func() {
	// The nesting is the whole point: it lets a caller that cares which failure it was
	// branch on the narrower one, while every caller that does not keeps matching on
	// ErrAgentUnavailable and needs no change.
	It("Should also be an unavailable agent", func() {
		Expect(errors.Is(ErrNoResponders, ErrAgentUnavailable)).To(BeTrue())
	})

	It("Should not be reported for a responder that answered too slowly", func() {
		slow := fmt.Errorf("%w: no reply on %q within 2s", ErrAgentUnavailable, "choria.fisk-ai.discovery.svc")

		Expect(errors.Is(slow, ErrAgentUnavailable)).To(BeTrue())
		Expect(errors.Is(slow, ErrNoResponders)).To(BeFalse(), "somebody is listening, they were just slow")
	})

	It("Should keep the subject in the message a binding builds", func() {
		err := fmt.Errorf("%w on %q", ErrNoResponders, "choria.fisk-ai.discovery.svc")

		Expect(err).To(MatchError(ContainSubstring("remote agent unavailable")))
		Expect(err).To(MatchError(ContainSubstring("no subscription interest")))
		Expect(err).To(MatchError(ContainSubstring("choria.fisk-ai.discovery.svc")))
	})
})
