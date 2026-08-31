//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package anthropic

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	sdk "github.com/anthropics/anthropic-sdk-go"

	"github.com/choria-io/fisk-ai/internal/llm"
)

// statusOverloaded is the status Anthropic answers with when it has no capacity for
// the request. net/http names no constant for it.
const statusOverloaded = 529

// contextLengthMessages are the message texts the API uses for a request longer than
// the model's context window: "prompt is too long: 300000 tokens > 200000 maximum"
// and "input length and `max_tokens` exceed context limit: 190000 + 32000 > 200000".
// It reports both as an invalid_request_error, the same type it uses for a request a
// caller has to correct, so the message is the only thing that separates them and a
// wording change upstream reclassifies the error as llm.ErrInvalidRequest.
var contextLengthMessages = []string{"prompt is too long", "exceed context limit"}

// classify wraps err with the llm sentinel for the class the API reported, so a
// caller reads the class with errors.Is and never has to import the SDK to find out
// what failed. The wrapping is %w, so a caller that does import the SDK still reaches
// the *sdk.Error with errors.As. An error the SDK did not build from an API response,
// and a class llm does not name (a permission error, a billing error, a 404, a 5xx
// other than overloaded), is returned as it came.
//
// The class comes from the error type in the response body, which the SDK parses out
// of the {"error":{"type":...}} envelope. A gateway in front of the API answers some
// requests with a status and a body that carries no such type, so the status code
// decides those.
func classify(err error) error {
	var apiErr *sdk.Error
	if !errors.As(err, &apiErr) {
		return err
	}

	sentinel := sentinelFor(apiErr)
	if sentinel == nil {
		return err
	}

	return fmt.Errorf("%w: %w", sentinel, err)
}

// sentinelFor returns the llm sentinel for an API error, or nil for a class llm does
// not name.
func sentinelFor(apiErr *sdk.Error) error {
	switch apiErr.Type() {
	case sdk.ErrorTypeRateLimitError:
		return llm.ErrRateLimited

	case sdk.ErrorTypeOverloadedError:
		return llm.ErrOverloaded

	case sdk.ErrorTypeAuthenticationError:
		return llm.ErrAuthentication

	case sdk.ErrorTypeInvalidRequestError:
		return invalidRequestSentinel(apiErr)
	}

	switch apiErr.StatusCode {
	case http.StatusTooManyRequests:
		return llm.ErrRateLimited

	case statusOverloaded:
		return llm.ErrOverloaded

	case http.StatusUnauthorized:
		return llm.ErrAuthentication

	case http.StatusBadRequest:
		return invalidRequestSentinel(apiErr)
	}

	return nil
}

// invalidRequestSentinel separates a request the model has no room for from one the
// caller has to correct, by matching the error message in the response envelope
// against the wordings the API uses for a full context window. The two classes need
// different handling: a caller trims the conversation for the first and edits its
// request or configuration for the second, and only the message tells them apart.
//
// It reads the message field rather than the whole body, so a request the API echoes
// back, or any other text that quotes one of these wordings, cannot decide the class.
// A body that carries no envelope leaves the message empty and the error is an
// invalid request.
//
// A context-length error carries llm.ErrContextLengthExceeded and not
// llm.ErrInvalidRequest.
func invalidRequestSentinel(apiErr *sdk.Error) error {
	var resp sdk.ErrorResponse

	err := json.Unmarshal([]byte(apiErr.RawJSON()), &resp)
	if err != nil {
		return llm.ErrInvalidRequest
	}

	message := strings.ToLower(resp.Error.Message)
	for _, wording := range contextLengthMessages {
		if strings.Contains(message, wording) {
			return llm.ErrContextLengthExceeded
		}
	}

	return llm.ErrInvalidRequest
}
