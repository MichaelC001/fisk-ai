//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agenttest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/choria-io/fisk-ai/internal/llm"
)

var _ llm.StreamingProvider = (*ScriptedProvider)(nil)

// ScriptedProvider is an llm.Provider and an llm.StreamingProvider that returns a fixed
// queue of responses in order, one per call, so a test drives the agent loop
// deterministically without a live backend. It is handed to a run through
// agent.Options.Provider, so nothing is registered in the global llm registry and each
// run owns its own provider with no shared state to isolate. A call past the end of the
// script returns an error, which surfaces as a failed run the test's completion
// assertion catches.
//
// CallStream reports the same script position as fragments and then returns the same
// response Call returns for it, which is the equivalence llm.StreamingProvider requires
// of a backend. The fragments are derived from the response unless SetCallDeltas gives
// the position its own.
//
// SetCallFault and SetEveryCallFault make a call fail with an llm sentinel, take a stated
// time, or fail part way through a stream, so a spec drives what an agent does on a 429
// or a deadline without writing a provider of its own.
type ScriptedProvider struct {
	caps llm.Caps

	mu         sync.Mutex
	responses  []*llm.Response
	idx        int
	requests   []llm.Request
	waiter     Waiter
	everyFault Fault
	faults     map[int]Fault
	deltas     map[int][]llm.Delta
}

// Fault is what a ScriptedProvider does to one model call instead of answering it from
// the script, or before it does.
type Fault struct {
	// Err is what the call returns. Name one of the llm sentinels (llm.ErrRateLimited,
	// llm.ErrOverloaded, llm.ErrAuthentication and the rest) and the agent under test
	// classifies the failure the way it classifies a real backend's; the provider returns
	// the error unchanged, so errors.Is on the caller's side reaches whatever class it
	// names. A fault with a nil Err answers from the script.
	//
	// A call the fault fails needs no scripted response, so a provider built with an empty
	// script and an every-call fault fails every call.
	Err error

	// Delay is how long the call takes before it answers or fails. The provider passes it
	// to the Waiter, which defaults to a timer that ctx cancels; a spec asserting on a
	// delay rather than serving it installs its own with SetWaiter.
	Delay time.Duration

	// AfterFragments is how many fragments CallStream sends to fn before Err fails the
	// call. Zero fails it before any fragment. A number at or over the count the position
	// produces sends them all and then fails, as a backend does when its connection is cut
	// before the end-of-message event. Call ignores it.
	AfterFragments int
}

// Waiter is how a delay a fake was told to take passes. It returns nil once d has gone by
// and the call carries on to its answer, or an error the call returns instead.
//
// The default waits on a timer and returns ctx.Err() when ctx ends first. A spec that
// asserts on a delay rather than serving it installs one that records d and returns nil,
// which keeps the suite fast, or one that returns context.DeadlineExceeded to drive what
// an agent does when a deadline lands during a model call or a call to a peer. A
// ScriptedProvider and a FakeTransport each call it only for a fault that names a delay,
// and on the goroutine that made the call.
type Waiter func(ctx context.Context, d time.Duration) error

// NewScriptedProvider builds a provider that answers successive calls with the given
// responses. Its declared capabilities default to an anthropic-shaped provider with
// tool search; override them with SetCapabilities before the run when a spec needs
// different behavior.
func NewScriptedProvider(tb testing.TB, responses ...*llm.Response) *ScriptedProvider {
	tb.Helper()

	p, err := BuildScriptedProvider(responses...)
	if err != nil {
		tb.Fatalf("%v", err)
	}

	return p
}

// BuildScriptedProvider is NewScriptedProvider without a testing.TB, for a func Example
// or any other caller outside a test. It returns an error naming the position of the
// first nil response.
func BuildScriptedProvider(responses ...*llm.Response) (*ScriptedProvider, error) {
	for i, r := range responses {
		if r == nil {
			return nil, fmt.Errorf("agenttest: scripted response %d is nil", i)
		}
	}

	return &ScriptedProvider{
		caps:      llm.Caps{Provider: "anthropic", SupportsToolSearch: true},
		responses: responses,
		waiter:    wallWait,
		faults:    map[int]Fault{},
		deltas:    map[int][]llm.Delta{},
	}, nil
}

// wallWait serves a delay on a timer and returns ctx.Err() when ctx ends first. A
// provider or a transport waits this way until SetWaiter installs another Waiter.
func wallWait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// SetCapabilities overrides the capabilities the provider declares, for a spec that
// exercises capability-dependent behavior (tool-search degradation, a provider name
// a checkpoint fingerprint pins).
func (p *ScriptedProvider) SetCapabilities(caps llm.Caps) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.caps = caps
}

// SetCallFault makes the nth call fail, take time, or send part of a stream and then
// fail. n counts from 1, so SetCallFault(2, Fault{Err: llm.ErrRateLimited}) leaves the
// first call answering from the script and rate limits the second.
//
// A call number takes precedence over SetEveryCallFault.
func (p *ScriptedProvider) SetCallFault(n int, f Fault) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.faults[n] = f
}

// SetEveryCallFault applies a fault to every call the provider has not been given one for
// by number, for a spec driving a backend that is down for the whole run.
func (p *ScriptedProvider) SetEveryCallFault(f Fault) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.everyFault = f
}

// SetWaiter takes over how a scripted delay passes. A nil w restores the timer the
// provider waits on by default.
func (p *ScriptedProvider) SetWaiter(w Waiter) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if w == nil {
		w = wallWait
	}

	p.waiter = w
}

// SetCallDeltas gives the nth call the fragments CallStream sends for it, in place of the
// ones ScriptedDeltas derives from its response. n counts from 1.
//
// A spec needs this to drive two blocks whose fragments interleave. llm.StreamingProvider
// allows that, and the derived fragments finish one block before they start the next. It
// also drives a consumer whose fragments and whole block disagree. The response the call
// returns is the scripted one either way, so fragments that contradict it are the spec's
// own doing.
func (p *ScriptedProvider) SetCallDeltas(n int, deltas ...llm.Delta) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deltas[n] = deltas
}

// Call records the request and returns the next scripted response, after any delay the
// call's fault names and unless that fault fails it.
func (p *ScriptedProvider) Call(ctx context.Context, req llm.Request) (*llm.Response, error) {
	call := p.next(req)

	err := call.wait(ctx)
	if err != nil {
		return nil, err
	}

	if call.fault.Err != nil {
		return nil, call.fault.Err
	}

	return call.resp, call.err
}

// CallStream records the request, sends the call's fragments to fn and returns the same
// scripted response Call returns for that position. A fault fails the call after
// Fault.AfterFragments of them have reached fn, and it withdraws none of the fragments
// already sent.
//
// fn runs on this goroutine, in order, and never after CallStream returns. A nil fn is
// refused without spending a script position, since a caller wanting no fragments calls
// Call.
func (p *ScriptedProvider) CallStream(ctx context.Context, req llm.Request, fn func(llm.Delta)) (*llm.Response, error) {
	if fn == nil {
		return nil, fmt.Errorf("agenttest: ScriptedProvider.CallStream requires a delta function")
	}

	call := p.next(req)

	err := call.wait(ctx)
	if err != nil {
		return nil, err
	}

	if call.fault.Err != nil {
		for i, d := range call.deltas {
			if i >= call.fault.AfterFragments {
				break
			}

			fn(d)
		}

		return nil, call.fault.Err
	}

	if call.err != nil {
		return nil, call.err
	}

	for _, d := range call.deltas {
		fn(d)
	}

	return call.resp, nil
}

// scriptedCall is one position of the script as a call found it.
type scriptedCall struct {
	resp   *llm.Response
	deltas []llm.Delta
	fault  Fault
	waiter Waiter
	err    error
}

// wait serves the call's delay, if it has one.
func (c scriptedCall) wait(ctx context.Context) error {
	if c.fault.Delay <= 0 {
		return nil
	}

	return c.waiter(ctx, c.fault.Delay)
}

// next records the request and takes the script position the call answers from, together
// with the fault attached to it. Both call paths do their waiting and their calls to fn
// outside the lock, so a fn that reads the provider back does not deadlock and a slow
// call does not hold up the calls a spec is making beside it.
func (p *ScriptedProvider) next(req llm.Request) scriptedCall {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.requests = append(p.requests, req)
	p.idx++

	call := scriptedCall{waiter: p.waiter, fault: p.everyFault}

	fault, held := p.faults[p.idx]
	if held {
		call.fault = fault
	}

	if p.idx > len(p.responses) {
		call.err = fmt.Errorf("agenttest: ScriptedProvider exhausted: call %d exceeds %d scripted responses", p.idx, len(p.responses))
		return call
	}

	call.resp = p.responses[p.idx-1]

	deltas, held := p.deltas[p.idx]
	if held {
		call.deltas = deltas
		return call
	}

	call.deltas = ScriptedDeltas(call.resp)

	return call
}

// ScriptedDeltas is the fragments CallStream sends for a response: every text and thinking
// block split on spaces in index order, each index closed by a Final fragment with no
// text, as the Anthropic backend closes a block it streamed. Joining the fragments of an
// index gives that block's text, so the stream and the turn it returns agree.
//
// A tool call streams no fragments, which llm.Delta requires of a backend, so its index is
// absent. So is the index of a block whose text is empty.
func ScriptedDeltas(resp *llm.Response) []llm.Delta {
	if resp == nil {
		return nil
	}

	var out []llm.Delta

	for i, block := range resp.Content {
		var kind llm.DeltaKind
		var text string

		switch {
		case block.Text != nil:
			kind, text = llm.DeltaText, block.Text.Text
		case block.Thinking != nil:
			kind, text = llm.DeltaThinking, block.Thinking.Text
		default:
			continue
		}

		fragments := 0

		for _, piece := range strings.SplitAfter(text, " ") {
			if piece == "" {
				continue
			}

			out = append(out, llm.Delta{Kind: kind, Index: i, Text: piece})
			fragments++
		}

		if fragments > 0 {
			out = append(out, llm.Delta{Kind: kind, Index: i, Final: true})
		}
	}

	return out
}

// Capabilities reports the declared capability set.
func (p *ScriptedProvider) Capabilities() llm.Caps {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.caps
}

// Requests returns a copy of every request the provider was called with, in order,
// so a spec can assert what the loop sent (the tools offered, the messages built
// from tool results).
func (p *ScriptedProvider) Requests() []llm.Request {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := make([]llm.Request, len(p.requests))
	copy(out, p.requests)

	return out
}

// TextResponse is a terminal assistant turn carrying a single text block: the
// simplest completing reply, ending the run with ReasonCompleted.
func TextResponse(text string) *llm.Response {
	return &llm.Response{
		StopReason: llm.StopEndTurn,
		Content:    []llm.ContentBlock{{Text: &llm.TextBlock{Text: text}}},
	}
}

// ToolUseResponse is an assistant turn asking to run one tool: the loop executes it,
// feeds the result back as a user turn, and calls the provider again for the next
// scripted response.
func ToolUseResponse(id, name string, input json.RawMessage) *llm.Response {
	return &llm.Response{
		StopReason: llm.StopToolUse,
		Content:    []llm.ContentBlock{{ToolUse: &llm.ToolUseBlock{ID: id, Name: name, Input: input}}},
	}
}
