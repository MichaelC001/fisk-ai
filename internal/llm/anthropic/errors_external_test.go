//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// These tests drive the provider from outside its package and never import the
// Anthropic SDK, which is the position a caller is in: it reads a failure class with
// errors.Is against the llm sentinels, and a per-call timeout it never set.
package anthropic_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/llm/anthropic"
)

// apiServer answers every request with status and body. It sets x-should-retry so
// the SDK returns the first answer instead of retrying a 429 or a 5xx through its
// backoff.
func apiServer(status int, body string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-should-retry", "false")
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	DeferCleanup(srv.Close)

	return srv
}

// streamServer opens a 200 event stream and writes events, the shape the API uses
// once it has accepted a streaming request.
func streamServer(events string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(events))
	}))
	DeferCleanup(srv.Close)

	return srv
}

// messageServer answers with one assistant turn after delay.
func messageServer(delay time.Duration) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_test","type":"message","role":"assistant","model":"test-model","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`))
	}))
	DeferCleanup(srv.Close)

	return srv
}

// errorBody is the error envelope the API sends: a type a caller acts on and a
// message it shows.
func errorBody(errType string, message string) string {
	return fmt.Sprintf(`{"type":"error","request_id":"req_test","error":{"type":%q,"message":%q}}`, errType, message)
}

func probeRequest() llm.Request {
	return llm.Request{
		Model:           "test-model",
		MaxOutputTokens: 64,
		Messages:        []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Text: &llm.TextBlock{Text: "go"}}}}},
	}
}

// callAgainst resolves the provider the way a caller does, through the registry and
// the neutral Config, and returns the error one call produced.
func callAgainst(srv *httptest.Server) error {
	provider, err := llm.NewProvider("anthropic", llm.Config{APIKey: "test-key", BaseURL: srv.URL})
	Expect(err).NotTo(HaveOccurred())

	_, err = provider.Call(context.Background(), probeRequest())
	Expect(err).To(HaveOccurred())

	return err
}

// streamAgainst runs one streaming call against srv, reached through the type
// assertion a caller makes.
func streamAgainst(srv *httptest.Server) (*llm.Response, error) {
	provider, err := llm.NewProvider("anthropic", llm.Config{APIKey: "test-key", BaseURL: srv.URL})
	Expect(err).NotTo(HaveOccurred())

	streamer, ok := provider.(llm.StreamingProvider)
	Expect(ok).To(BeTrue())

	return streamer.CallStream(context.Background(), probeRequest(), func(llm.Delta) {})
}

var _ = Describe("Error classification", func() {
	It("Should report a rate limit a caller backs off on", func() {
		err := callAgainst(apiServer(http.StatusTooManyRequests, errorBody("rate_limit_error", "number of requests has exceeded your rate limit")))
		Expect(errors.Is(err, llm.ErrRateLimited)).To(BeTrue())
	})

	It("Should report an overloaded backend", func() {
		err := callAgainst(apiServer(529, errorBody("overloaded_error", "Overloaded")))
		Expect(errors.Is(err, llm.ErrOverloaded)).To(BeTrue())
		Expect(errors.Is(err, llm.ErrRateLimited)).To(BeFalse())
	})

	// 529 sits inside the server-error range, so a body with no error type leaves the
	// order of the status cases as the only thing keeping the two classes apart.
	It("Should keep an untyped 529 out of the server-error range", func() {
		err := callAgainst(apiServer(529, "<html>overloaded</html>"))
		Expect(errors.Is(err, llm.ErrOverloaded)).To(BeTrue())
		Expect(errors.Is(err, llm.ErrBackendFailure)).To(BeFalse())
	})

	It("Should report a rejected credential", func() {
		err := callAgainst(apiServer(http.StatusUnauthorized, errorBody("authentication_error", "invalid x-api-key")))
		Expect(errors.Is(err, llm.ErrAuthentication)).To(BeTrue())
	})

	// The API reports both of these as an invalid_request_error, so the message is
	// what places them in the class a caller trims its history on.
	It("Should report a request longer than the context window as a context length error", func() {
		err := callAgainst(apiServer(http.StatusBadRequest, errorBody("invalid_request_error", "prompt is too long: 300000 tokens > 200000 maximum")))
		Expect(errors.Is(err, llm.ErrContextLengthExceeded)).To(BeTrue())
		Expect(errors.Is(err, llm.ErrInvalidRequest)).To(BeFalse())
	})

	It("Should report the input plus max_tokens wording as a context length error", func() {
		err := callAgainst(apiServer(http.StatusBadRequest, errorBody("invalid_request_error", "input length and `max_tokens` exceed context limit: 190000 + 32000 > 200000")))
		Expect(errors.Is(err, llm.ErrContextLengthExceeded)).To(BeTrue())
	})

	It("Should report a request the caller has to correct", func() {
		err := callAgainst(apiServer(http.StatusBadRequest, errorBody("invalid_request_error", "max_tokens: Field required")))
		Expect(errors.Is(err, llm.ErrInvalidRequest)).To(BeTrue())
		Expect(errors.Is(err, llm.ErrContextLengthExceeded)).To(BeFalse())
	})

	It("Should report a model the backend does not have", func() {
		err := callAgainst(apiServer(http.StatusNotFound, errorBody("not_found_error", "model: claude-not-a-model")))
		Expect(errors.Is(err, llm.ErrModelNotFound)).To(BeTrue())
	})

	// A body over the size limit is a different remedy from a context window over the
	// token limit, so the two classes stay apart.
	It("Should report a body over the size limit", func() {
		err := callAgainst(apiServer(http.StatusRequestEntityTooLarge, errorBody("request_too_large", "Request body exceeds the maximum allowed size")))
		Expect(errors.Is(err, llm.ErrRequestTooLarge)).To(BeTrue())
		Expect(errors.Is(err, llm.ErrContextLengthExceeded)).To(BeFalse())
	})

	It("Should report a backend that failed rather than refused", func() {
		err := callAgainst(apiServer(http.StatusInternalServerError, "<html>internal server error</html>"))
		Expect(errors.Is(err, llm.ErrBackendFailure)).To(BeTrue())
	})

	It("Should report a gateway that gave up waiting as the same failure", func() {
		err := callAgainst(apiServer(http.StatusGatewayTimeout, errorBody("timeout_error", "Request timed out")))
		Expect(errors.Is(err, llm.ErrBackendFailure)).To(BeTrue())
	})

	// A proxy in front of the API answers with a code the API never sends and a body
	// that carries no error type, so the server-error range is what places it.
	It("Should report a proxy's own server error as a backend failure", func() {
		err := callAgainst(apiServer(http.StatusServiceUnavailable, "<html>503 Service Unavailable</html>"))
		Expect(errors.Is(err, llm.ErrBackendFailure)).To(BeTrue())
	})

	// A gateway in front of the API answers with a status and no error envelope, so
	// the status carries the class on its own.
	It("Should classify on the status when the body carries no error type", func() {
		err := callAgainst(apiServer(http.StatusTooManyRequests, "<html>too many requests</html>"))
		Expect(errors.Is(err, llm.ErrRateLimited)).To(BeTrue())
	})

	// A class llm does not name reaches the caller as the API sent it.
	It("Should leave a permission error unclassified", func() {
		err := callAgainst(apiServer(http.StatusForbidden, errorBody("permission_error", "not allowed to use this model")))
		Expect(errors.Is(err, llm.ErrAuthentication)).To(BeFalse())
		Expect(errors.Is(err, llm.ErrInvalidRequest)).To(BeFalse())
		Expect(err.Error()).To(ContainSubstring("not allowed to use this model"))
	})

	It("Should classify the streaming call the same way", func() {
		srv := apiServer(http.StatusTooManyRequests, errorBody("rate_limit_error", "number of requests has exceeded your rate limit"))

		_, err := streamAgainst(srv)
		Expect(errors.Is(err, llm.ErrRateLimited)).To(BeTrue())
	})

	// The API opens the stream with a 200 and reports the failure as an error event
	// inside it, so the response status says nothing and the error type in the event is
	// the only thing that places the class.
	It("Should classify an error event that arrives on an opened stream", func() {
		srv := streamServer("event: error\ndata: " + errorBody("overloaded_error", "Overloaded") + "\n\n")

		_, err := streamAgainst(srv)
		Expect(errors.Is(err, llm.ErrOverloaded)).To(BeTrue())
	})

	// The other type an error event carries mid-stream, where the 500 that would place
	// it on the non-streaming path never arrives.
	It("Should classify an api_error event on an opened stream as a backend failure", func() {
		srv := streamServer("event: error\ndata: " + errorBody("api_error", "Internal server error") + "\n\n")

		_, err := streamAgainst(srv)
		Expect(errors.Is(err, llm.ErrBackendFailure)).To(BeTrue())
	})
})

var _ = Describe("Per-call timeout", func() {
	// A middleware sees the request the provider issued, so a spec reads the deadline
	// the provider set instead of inferring one from how long a call took. The
	// remaining time is read at the moment the request goes out, a few hundred
	// microseconds after the deadline was computed.
	deadlineFor := func(timeout time.Duration, stream bool) (time.Duration, bool) {
		var (
			remaining time.Duration
			set       bool
		)

		observe := func(req *http.Request, next llm.MiddlewareNext) (*http.Response, error) {
			deadline, ok := req.Context().Deadline()
			set = ok
			if ok {
				remaining = time.Until(deadline)
			}

			return next(req)
		}

		opts := anthropic.Options{APIKey: "test-key", Timeout: timeout, Middlewares: []llm.Middleware{observe}}

		if stream {
			opts.BaseURL = streamServer("event: error\ndata: " + errorBody("overloaded_error", "Overloaded") + "\n\n").URL

			provider := anthropic.NewProvider(opts)
			_, err := provider.CallStream(context.Background(), probeRequest(), func(llm.Delta) {})
			Expect(err).To(HaveOccurred())

			return remaining, set
		}

		opts.BaseURL = messageServer(0).URL

		provider := anthropic.NewProvider(opts)
		resp, err := provider.Call(context.Background(), probeRequest())
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StopReason).To(Equal(llm.StopEndTurn))

		return remaining, set
	}

	// The SDK puts its own 10 minute request timeout on a non-streaming call, so a
	// deadline alone proves nothing: only its value says whether the provider applied
	// llm.DefaultTimeout or added no limit at all.
	It("Should call under llm.DefaultTimeout when Options names no timeout", func() {
		remaining, set := deadlineFor(0, false)
		Expect(set).To(BeTrue())
		Expect(remaining).To(BeNumerically("~", llm.DefaultTimeout, 5*time.Second))
	})

	It("Should call under the timeout the caller set", func() {
		remaining, set := deadlineFor(37*time.Second, false)
		Expect(set).To(BeTrue())
		Expect(remaining).To(BeNumerically("~", 37*time.Second, 5*time.Second))
	})

	// What is left on a negative timeout is the SDK's own request timeout, which is
	// what the caller's documentation promises rather than an unlimited call.
	It("Should add no deadline of its own when the timeout is negative", func() {
		remaining, set := deadlineFor(-1, false)
		Expect(set).To(BeTrue())
		Expect(remaining).To(BeNumerically(">", llm.DefaultTimeout))
		Expect(remaining).To(BeNumerically("~", 10*time.Minute, time.Minute))
	})

	// A streaming call gets no request timeout from the SDK, so a negative timeout
	// leaves it running until the caller's context ends it.
	It("Should leave a streaming call with no deadline when the timeout is negative", func() {
		_, set := deadlineFor(-1, true)
		Expect(set).To(BeFalse())
	})

	It("Should end a call that outlasts the timeout the caller set", func() {
		srv := messageServer(500 * time.Millisecond)

		provider := anthropic.NewProvider(anthropic.Options{APIKey: "test-key", BaseURL: srv.URL, Timeout: 20 * time.Millisecond})

		_, err := provider.Call(context.Background(), probeRequest())
		Expect(errors.Is(err, context.DeadlineExceeded)).To(BeTrue())
	})
})
