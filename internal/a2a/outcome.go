//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/util"
)

// StopReasonFor maps a run's terminal reason onto the protocol's neutral
// vocabulary. A reason with no counterpart is reported as an error, which is what a
// caller that cannot interpret it should treat it as.
//
// It lives here rather than in each channel that answers over this protocol, since
// two copies would let one endpoint call a suspended run something the other does not.
func StopReasonFor(reason runstate.TerminalReason) StopReason {
	switch reason {
	case runstate.ReasonCompleted:
		return StopEndTurn
	case runstate.ReasonBudget:
		return StopBudgetExhausted
	case runstate.ReasonMaxIterations:
		return StopMaxIterations
	case runstate.ReasonSuspended:
		return StopSuspended
	default:
		return StopError
	}
}

// UsageFrom reports what a run consumed, or nothing when it never got far enough to
// consume anything.
//
// The input total is assembled rather than copied. RunStats.InTokens is the uncached
// remainder, with the cached input counted separately, so reporting it alone would
// hand a caller a fraction of what it was billed for and no way to tell.
func UsageFrom(stats *util.RunStats) *Usage {
	if stats == nil {
		return nil
	}

	return &Usage{
		InputTokens:       stats.InTokens + stats.CacheReadTokens + stats.CacheCreateTokens,
		OutputTokens:      stats.OutTokens,
		CacheReadTokens:   stats.CacheReadTokens,
		CacheCreateTokens: stats.CacheCreateTokens,
		ThinkingTokens:    stats.ThinkingTokens,
		LLMCalls:          stats.LlmCalls,
		ToolCalls:         stats.ToolCalls,
	}
}

// UsageFromCounters reports what a stored conversation has consumed so far, which is
// the same shape read from a journal rather than from a run in progress.
//
// It carries no call counts on purpose: LlmCalls and ToolCalls are restored beside
// these and describe the conversation, while this is sent to seed a caller's running
// total, where a call count from before the caller arrived would read as this turn's.
func UsageFromCounters(c runstate.Counters) *Usage {
	return &Usage{
		InputTokens:       c.InTokens + c.CacheReadTokens + c.CacheCreateTokens,
		OutputTokens:      c.OutTokens,
		CacheReadTokens:   c.CacheReadTokens,
		CacheCreateTokens: c.CacheCreateTokens,
		ThinkingTokens:    c.ThinkingTokens,
	}
}
