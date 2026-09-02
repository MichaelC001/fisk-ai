//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/choria-io/fisk-ai/internal/a2a"
	wire "github.com/choria-io/fisk-ai/internal/a2a/wire/v1"
	"github.com/choria-io/fisk-ai/internal/tui"
)

// tuiReplay is how much of a stored conversation the full-screen view opens with. It is
// a number at or above the worker's own cap, which is the most a caller can ask for:
// the count is floored at zero on the wire, so there is no way to spell "all of it".
const tuiReplay = 500

// chatSession is a conversation held from the full-screen view: one request per turn,
// each carrying the token of the conversation before it.
//
// Nothing is held between turns on either side. The worker takes a turn and returns,
// and what makes the next turn part of the same conversation is the token, which is the
// same thing that makes a peer agent's follow-up part of one.
type chatSession struct {
	host   *hostedAgent
	live   *tui.Live
	client *tuiClient

	// conversation is the token every turn after the first carries. It is empty before
	// the first turn of a new conversation and after a reset, which is what makes the
	// next turn open a conversation rather than continue one.
	conversation string

	// outcome is how the last turn ended, read after the run to classify the view's
	// terminal state and to print the summary.
	outcome *a2a.TaskOutcome

	// left records the handles of conversations a reset walked away from, reprinted
	// after the alt-screen is gone so they stay findable.
	left []string

	// mu guards request, which is the id of the turn in flight. It is written by the
	// run goroutine and read by the interrupt hook, which runs on the tview loop.
	mu      sync.Mutex
	request string
}

// run drives the conversation until the operator leaves it.
//
// It is the function the live view runs, so it holds the run goroutine for the whole
// session: a turn, then the input row, then the next turn.
func (s *chatSession) run(ctx context.Context) error {
	prompt := strings.TrimSpace(strings.Join(q, " "))

	// A resumed conversation opens on what it holds. The read takes no turn and calls
	// no model, so the history is on screen before the operator has decided what to say,
	// which is what a resume that opened on an empty view could not do.
	if s.conversation != "" {
		err := s.read(ctx)
		if err != nil {
			return err
		}
	}

	for {
		if prompt == "" {
			next, reset, cont := s.live.NextPromptFunc()(ctx)
			if !cont {
				return nil
			}
			if reset {
				s.reset()
			}
			// A bare reset reopens the row without running anything.
			if next == "" {
				continue
			}

			prompt = next
		}

		done, err := s.turn(ctx, prompt)

		// Delivered whether or not the set could be read. A set that died under a question
		// is the ending most likely to leave one on screen, and what the person answered
		// after it went is the whole reason the question stayed.
		s.deliverHeld(ctx, s.outcome)

		if err != nil {
			return err
		}

		if done {
			return nil
		}

		prompt = ""
	}
}

// read asks the worker for the conversation so far and draws it.
func (s *chatSession) read(ctx context.Context) error {
	req, err := wire.NewRead(s.conversation, tuiReplay)
	if err != nil {
		return err
	}

	out, err := s.host.client.RunTask(ctx, s.host.identity, req, s.client)
	if err != nil {
		return err
	}

	// A conversation that cannot be read cannot be continued either, so this is the
	// ending rather than a note on the way to one.
	if out.Error != nil {
		s.outcome = out

		return fmt.Errorf("%s", endingMessage(out.Error))
	}

	if out.Result != nil {
		s.client.setUsage(out.Result.Usage)
	}

	return nil
}

// turn sends one prompt and renders the reply set. It reports whether the session ends
// here.
func (s *chatSession) turn(ctx context.Context, prompt string) (bool, error) {
	req := wire.NewRequest(prompt)
	req.ConversationToken = s.conversation
	req.Force = forceResume

	s.holdRequest(req.Request)
	defer s.holdRequest("")

	out, err := s.host.client.RunTask(ctx, s.host.identity, req, s.client)

	// The outcome and the token the worker minted for a conversation this turn opened are
	// recorded before the error rather than after it: a set that could not be read still
	// carries what somebody answered under it, and an answer needs the token to travel on.
	// Nothing reads an outcome that carries neither ending.
	s.outcome = out

	if out != nil && out.Ack != nil && out.Ack.ConversationToken != "" {
		s.conversation = out.Ack.ConversationToken
	}

	if err != nil {
		return false, err
	}

	// What this conversation may process in total, which only the agent knows: it is the
	// agent's configuration, clamped by anything this caller asked for, and the agent may
	// not be on this machine. Told every turn rather than once, since a caller can lower
	// it per request.
	if out.Ack != nil {
		s.live.SetTokenBudget(out.Ack.MaxTokens)
	}

	if out.Result != nil {
		s.client.setUsage(out.Result.Usage)
		s.reportStop(out.Result.StopReason)

		return false, nil
	}

	if out.Error == nil {
		return true, fmt.Errorf("the run ended without saying how")
	}

	s.client.setUsage(out.Error.Usage)
	s.live.Append(tui.Line{Kind: tui.LineWarning, Text: endingMessage(out.Error)})

	return endsSession(out.Error.Code), nil
}

// deliverHeld sends an answer the run had given up on by the time somebody finished
// giving it.
//
// It happens here, between the turn that collected it and the input row, rather than
// riding with the next prompt: a request carrying an answer resumes the run and produces
// a whole turn's work, so attaching it to a follow-up would show a person the previous
// turn's tool calls and answer after they pressed Enter on a new prompt, with nothing on
// screen explaining why.
func (s *chatSession) deliverHeld(ctx context.Context, out *a2a.TaskOutcome) {
	if out == nil || len(out.Unsent) == 0 || s.conversation == "" {
		return
	}

	for _, held := range out.Unsent {
		s.live.Append(tui.Line{Kind: tui.LineMeta, Text: "--- your answer arrived after the run gave the question up; sending it now ---"})

		err := s.sendAnswer(ctx, held)
		if err != nil {
			s.live.Append(tui.Line{Kind: tui.LineWarning, Text: "delivering your answer failed: " + err.Error()})
		}
	}
}

// sendAnswer delivers one answer, retrying the endings that mean "not yet".
//
// The worker releases a task's slot after it publishes the terminal message, so a client
// that sends the next request the moment it reads one can be refused for work that has
// already ended. That is the worker's to fix; until it is, this waits and asks again.
func (s *chatSession) sendAnswer(ctx context.Context, held *wire.Answer) error {
	for attempt := range 3 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(answerRetryWait):
			}
		}

		req := wire.NewAnswerRequest(s.conversation, held)
		req.Force = forceResume

		out, err := s.host.client.RunTask(ctx, s.host.identity, req, s.client)
		if err != nil {
			return err
		}

		if out.Error == nil {
			return nil
		}
		if !answerNotTaken(out.Error.Code) {
			return fmt.Errorf("%s", endingMessage(out.Error))
		}
	}

	return fmt.Errorf("the conversation stayed busy")
}

// answerRetryWait is how long to wait before asking again, which is long enough for a
// worker to have released the slot it holds past its own terminal message.
const answerRetryWait = 250 * time.Millisecond

// reportStop says when a turn stopped before it was finished. The answer above it reads
// as a whole one otherwise, which is the difference between a model that answered and a
// model that ran out of room.
func (s *chatSession) reportStop(reason wire.StopReason) {
	var msg string

	switch reason {
	case wire.StopMaxIterations:
		msg = "the turn reached its iteration cap before finishing; send a follow-up to steer it"
	case wire.StopMaxTokens:
		msg = "the model stopped at its output limit, so the answer above is incomplete"
	case wire.StopBudgetExhausted:
		msg = "the conversation reached its token budget before this turn finished"
	case wire.StopRefusal:
		msg = "the model declined to answer"
	default:
		return
	}

	s.live.Append(tui.Line{Kind: tui.LineWarning, Text: msg})
}

// reset walks away from the conversation so the next turn opens a fresh one, naming the
// one it left so an operator can still reach it.
//
// The worker holds nothing between turns, so this is entirely the client's: what made
// the turns one conversation was the token, and dropping it is what ends it.
func (s *chatSession) reset() {
	if s.conversation == "" {
		return
	}

	hint := resumeHint(s.host.identity, s.host.natsContext, s.conversation)
	if hint != "" {
		s.left = append(s.left, hint)
		s.live.Append(tui.Line{Kind: tui.LineMeta, Text: "previous conversation saved; " + hint})
	}

	s.conversation = ""
}

// holdRequest records the turn in flight, or clears it between turns.
func (s *chatSession) holdRequest(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.request = id
}

// requestStop asks the turn in flight to stop where the conversation can be continued.
//
// It runs on the tview loop, where the leave key is handled, so the cancel is sent from
// a goroutine: it is a request with an ack to wait for, and waiting for one here would
// freeze the view and swallow the second press that gives up on the run. Between turns
// there is nothing in flight and nothing to ask.
func (s *chatSession) requestStop() {
	s.mu.Lock()
	id := s.request
	s.mu.Unlock()

	if id == "" {
		return
	}

	go func() {
		_, err := s.host.client.Cancel(context.Background(), s.host.identity, id, "the operator interrupted the run")
		if err != nil {
			s.live.Append(tui.Line{Kind: tui.LineWarning, Text: "asking the run to stop failed: " + err.Error()})
		}
	}()
}

// suspended reports whether the session ended at a boundary it can be continued from,
// which the view reads to classify its terminal state.
func (s *chatSession) suspended() bool {
	return s.outcome != nil && s.outcome.Error != nil && s.outcome.Error.Code == wire.CodeSuspended
}

// usage is what the conversation has cost, which the terminal message of its last turn
// reports as a running total rather than as that turn's share.
func (s *chatSession) usage() *wire.Usage {
	switch {
	case s.outcome == nil:
		return nil
	case s.outcome.Result != nil:
		return s.outcome.Result.Usage
	case s.outcome.Error != nil:
		return s.outcome.Error.Usage
	}

	return nil
}

// traceID is the trace the last turn recorded, or nothing when the worker exports no
// telemetry.
func (s *chatSession) traceID() string {
	switch {
	case s.outcome == nil:
		return ""
	case s.outcome.Result != nil:
		return s.outcome.Result.TraceID
	case s.outcome.Error != nil:
		return s.outcome.Error.TraceID
	}

	return ""
}

// traceLine is what a person keeps: the trace to go and read, and whether the words
// themselves went with it.
//
// The two are printed together because they answer one question. A trace id says where
// to look and the marker says what is there, and the marker is the half that matters
// after the fact: the startup card said what the agent was configured to do, and this
// says what this conversation actually did.
func (s *chatSession) traceLine() string {
	id := s.traceID()
	if id == "" {
		return ""
	}

	if s.contentExported() {
		return "trace: " + id + " content=exported"
	}

	return "trace: " + id
}

// contentExported reports whether the last turn's conversation reached a collector.
func (s *chatSession) contentExported() bool {
	switch {
	case s.outcome == nil:
		return false
	case s.outcome.Result != nil:
		return s.outcome.Result.ContentExported
	case s.outcome.Error != nil:
		return s.outcome.Error.ContentExported
	}

	return false
}

// answerNotTaken reports whether an ending means the answer never reached a turn, which
// is the only kind worth sending again.
//
// A turn that ran and failed has spent it: an approve reply is consumed where it lands
// and the gated command runs, so sending it again on a fresh resume would run that
// command a second time. The three here are refusals taken before any of that: no slot,
// a conversation already working, and a turn the worker did not accept.
func answerNotTaken(code string) bool {
	switch code {
	case wire.CodeCapacity, wire.CodeConversationBusy, wire.CodeTurnNotTaken:
		return true
	}

	return false
}

// endsSession reports whether an ending takes the whole conversation with it.
//
// A turn that failed, was refused for want of capacity or was not taken leaves the
// conversation where it was, so the input row reopens and the operator decides. The
// rest are endings the conversation does not continue past here.
func endsSession(code string) bool {
	switch code {
	case wire.CodeFailed, wire.CodeCapacity, wire.CodeConversationBusy, wire.CodeTurnNotTaken:
		return false
	}

	return true
}
