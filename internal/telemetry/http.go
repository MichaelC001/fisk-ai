//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
)

// chatSpanKey carries the chat span down to the HTTP middleware.
type chatSpanKey struct{}

// contextWithChatSpan puts the chat span in ctx for the middleware to annotate.
//
// The span itself travels, not a boolean marker. A marker plus trace.SpanFromContext
// would resolve to whatever span is innermost, so any span opened later between the chat
// span and the request would silently collect its HTTP events instead. Carrying the span
// means the middleware annotates the model call by identity, whatever nests below it.
func contextWithChatSpan(ctx context.Context, s *ChatSpan) context.Context {
	return context.WithValue(ctx, chatSpanKey{}, s)
}

// chatSpanFromContext returns the chat span ctx carries, or nil.
func chatSpanFromContext(ctx context.Context) *ChatSpan {
	s, _ := ctx.Value(chatSpanKey{}).(*ChatSpan)

	return s
}

// HTTPMiddleware annotates the in-flight model call with one event per HTTP attempt.
//
// It creates no span. The chat span already covers the model call, and the middleware
// runs inside the SDK's retry loop, so one chat span is N requests: a status code on the
// span would be last-attempt-wins and report 200 for a call that spent most of its time
// being rate limited. Per-attempt detail therefore lives in events, and the only thing
// that reaches the span is the resend count, which is monotonic and so means the same
// thing whichever attempt sets it last.
//
// The return type is written out rather than named as llm.Middleware because
// internal/telemetry imports nothing from this repository. That costs nothing here: the
// llm package declares both halves as type ALIASES, so this value satisfies
// llm.Middleware without either package importing the other.
//
// Nothing about the request or the response body is recorded: not the URL, which can
// carry userinfo, not the headers, which carry the credential, and not the response body
// or the error text. An attempt is a status code, a duration and an ordinal.
//
// It must be appended LAST to the middleware slice. The SDK applies the first element
// outermost, so anything appended after this one would run inside it and have its work
// charged to the measured attempt duration.
func HTTPMiddleware() func(*http.Request, func(*http.Request) (*http.Response, error)) (*http.Response, error) {
	return func(req *http.Request, next func(*http.Request) (*http.Response, error)) (*http.Response, error) {
		span := chatSpanFromContext(req.Context())
		if span == nil || span.Span == nil || span.Span.span == nil {
			return next(req)
		}

		attempt := span.attempts.Add(1)

		started := time.Now()
		resp, err := next(req)
		elapsed := time.Since(started)

		attrs := []attribute.KeyValue{
			AttrLLMHTTPAttempt.Int64(attempt),
			AttrLLMHTTPDurationMS.Int64(elapsed.Milliseconds()),
		}

		// A transport failure returns a nil response, which is the common case this has
		// to survive: a DNS failure, a reset connection, a TLS error or a per-attempt
		// deadline all arrive here with nothing to read a status code off. Reaching for
		// resp.StatusCode without this branch is a nil dereference inside the retry loop
		// of a live run.
		if err != nil {
			class, ok := ClassifyContext(err)
			if !ok {
				class = ClassProvider
			}
			attrs = append(attrs, errorType(class))
			span.Span.span.AddEvent(EventLLMHTTPError, trace.WithAttributes(attrs...))

			return resp, err
		}

		attrs = append(attrs, semconv.HTTPResponseStatusCode(resp.StatusCode))
		span.Span.span.AddEvent(EventLLMHTTPResponse, trace.WithAttributes(attrs...))

		return resp, nil
	}
}
