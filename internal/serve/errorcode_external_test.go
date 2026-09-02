//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package serve_test

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	wire "github.com/choria-io/fisk-ai/internal/a2a/wire/v1"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/serve"
)

// The errors here are shaped the way a run delivers them: a provider wraps its
// backend's error with the sentinel, the runner wraps that in "llm call: %w", and the
// server assigns the result to Outcome.Err without rebuilding it. A spec passing a bare
// sentinel would pass whether or not the wrapping survives.
var _ = Describe("ErrorCode", func() {
	DescribeTable("Should name the class a caller acts on",
		func(err error, code string) {
			Expect(serve.ErrorCode(serve.Outcome{Err: err})).To(Equal(code))
		},
		Entry("a rate limit", fmt.Errorf("llm call: %w: 429 rate_limit_error", llm.ErrRateLimited), wire.CodeProviderBusy),
		Entry("a provider with no capacity", fmt.Errorf("llm call: %w: 529 overloaded_error", llm.ErrOverloaded), wire.CodeProviderBusy),
		Entry("rejected credentials", fmt.Errorf("llm call: %w: 401 authentication_error", llm.ErrAuthentication), wire.CodeProviderRefused),
		Entry("a model that does not exist", fmt.Errorf("llm call: %w: 404 not_found_error", llm.ErrModelNotFound), wire.CodeProviderRefused),
		Entry("a request past the context window", fmt.Errorf("llm call: %w: 400 invalid_request_error", llm.ErrContextLengthExceeded), wire.CodeContextExceeded),
		Entry("a journal another writer holds", fmt.Errorf("cannot resume %q: %w", "s-abcdef", runstate.ErrLocked), wire.CodeConversationBusy),
	)

	// A caller that reads nothing here keeps whatever ending it worked out itself, so an
	// error this does not place has to answer with the empty string rather than with a
	// code that means something else.
	DescribeTable("Should name nothing for a failure it does not place",
		func(err error) {
			Expect(serve.ErrorCode(serve.Outcome{Err: err})).To(BeEmpty())
		},
		Entry("no error at all", nil),
		Entry("a backend that failed rather than refused", fmt.Errorf("llm call: %w: 500 api_error", llm.ErrBackendFailure)),
		Entry("a request the model refused", fmt.Errorf("llm call: %w: 400 invalid_request_error", llm.ErrInvalidRequest)),
		Entry("a body over the endpoint's size", fmt.Errorf("llm call: %w: 413 request_too_large", llm.ErrRequestTooLarge)),
		Entry("a tool that failed", fmt.Errorf("running %q: exit status 1", "restart_node")),
		Entry("a canceled run", context.Canceled),
	)

	// A lock the run met after it started has already executed part of the turn. Telling
	// a caller its work never started sends an interactive client's held approve reply
	// again, which runs a gated command twice.
	It("Should place a held journal only where the run reached no outcome", func() {
		held := fmt.Errorf("journaling run: %w", runstate.ErrLocked)

		Expect(serve.ErrorCode(serve.Outcome{Err: held})).To(Equal(wire.CodeConversationBusy))
		Expect(serve.ErrorCode(serve.Outcome{Err: held, Reason: runstate.ReasonError})).To(BeEmpty())
	})
})
