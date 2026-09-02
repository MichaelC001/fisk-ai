//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	wire "github.com/choria-io/fisk-ai/internal/a2a/wire/v1"
	"github.com/choria-io/fisk-ai/internal/runstate"
)

// StopReasonFor maps a run's terminal reason onto the protocol's neutral
// vocabulary. A reason with no counterpart is reported as an error, which is what a
// caller that cannot interpret it should treat it as.
//
// It lives here rather than in each channel that answers over this protocol, since
// two copies would let one endpoint call a suspended run something the other does not.
func StopReasonFor(reason runstate.TerminalReason) wire.StopReason {
	switch reason {
	case runstate.ReasonCompleted:
		return wire.StopEndTurn
	case runstate.ReasonBudget:
		return wire.StopBudgetExhausted
	case runstate.ReasonMaxIterations:
		return wire.StopMaxIterations
	case runstate.ReasonSuspended:
		return wire.StopSuspended
	default:
		return wire.StopError
	}
}

// UsageFromCounters reports what a stored conversation has consumed so far, which is
// the same shape read from a journal rather than from a run in progress.
//
// It carries no call counts on purpose: LlmCalls and ToolCalls are restored beside
// these and describe the conversation, while this is sent to seed a caller's running
// total, where a call count from before the caller arrived would read as this turn's.
func UsageFromCounters(c runstate.Counters) *wire.Usage {
	return &wire.Usage{
		InputTokens:       c.InTokens + c.CacheReadTokens + c.CacheCreateTokens,
		OutputTokens:      c.OutTokens,
		CacheReadTokens:   c.CacheReadTokens,
		CacheCreateTokens: c.CacheCreateTokens,
		ThinkingTokens:    c.ThinkingTokens,
	}
}
