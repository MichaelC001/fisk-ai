//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package llm

import "errors"

// The failure classes a caller handles differently. A Provider maps its backend's
// error onto the matching sentinel and wraps it, so errors.Is reports the class
// while the wrapped error keeps the backend's own message, and the caller branches
// on a failed call without importing a provider's SDK.
//
// A failure a provider cannot place in one of these classes is returned as it came,
// so errors.Is reports none of them.
var (
	// ErrRateLimited is the backend refusing a call because the account is over its
	// request or token rate limit. The same request succeeds once the window rolls
	// over, so a caller waits and sends it again unchanged.
	ErrRateLimited = errors.New("rate limited")

	// ErrOverloaded is the backend refusing a call because it has no capacity. The
	// request is fine and a caller sends it again unchanged, but it cannot compute when
	// capacity returns the way it can compute when a rate limit window rolls over, so
	// it waits longer than it would for a rate limit.
	ErrOverloaded = errors.New("provider overloaded")

	// ErrAuthentication is the backend rejecting the credentials. A retry sends the
	// same key, so a caller stops and reports the failure to whoever configured it.
	ErrAuthentication = errors.New("authentication failed")

	// ErrContextLengthExceeded is the request holding more tokens than the model's
	// context window takes. A caller drops or summarizes conversation history, or
	// lowers Request.MaxOutputTokens, before it calls again.
	ErrContextLengthExceeded = errors.New("context length exceeded")

	// ErrInvalidRequest is the backend refusing the request itself: a parameter the
	// model does not take, a malformed message, a field it requires and the request
	// omits. A caller fixes the request or the configuration, since sending it again
	// fails the same way.
	ErrInvalidRequest = errors.New("invalid request")

	// ErrModelNotFound is the backend having no model with the id the request named,
	// which a mistyped or retired model in the configuration produces. A caller stops
	// instead of retrying and an operator corrects the id. A base URL that addresses
	// something other than the API answers this way too, so the remedy is the model
	// name or the endpoint.
	ErrModelNotFound = errors.New("model not found")

	// ErrRequestTooLarge is the request body being over the size the endpoint accepts,
	// counted in bytes rather than in tokens. A caller sends smaller tool results,
	// images and attachments, where ErrContextLengthExceeded asks it for fewer turns of
	// history.
	ErrRequestTooLarge = errors.New("request too large")

	// ErrBackendFailure is the backend failing to produce an answer rather than
	// refusing the request. A caller retries on a backoff it chooses itself, since
	// nothing in the answer says when to come back, and gives up after a few attempts
	// rather than waiting out a queue as it does for ErrOverloaded.
	//
	// A provider maps its whole server-error range here, so this covers what a load
	// balancer, a corporate proxy or a gateway in front of the backend answers as well
	// as what the API itself sends.
	ErrBackendFailure = errors.New("backend failure")

	// ErrUnknownProvider is NewProvider given a name no provider registered under, most
	// often a provider whose package this build never imported. A caller adds the
	// import or selects a name from Providers().
	ErrUnknownProvider = errors.New("unknown llm provider")
)
