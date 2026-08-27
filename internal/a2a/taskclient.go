//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// TaskHandler is what a caller does with a task while it runs: render what the agent
// produced, and answer what it asks.
//
// Block is called in the order the blocks arrived, on the goroutine reading the set, so
// an implementation that draws slowly slows the reading and nothing else.
//
// Question runs on a goroutine of its own, so the set keeps being read while somebody
// decides, and it may block for as long as a person takes. RunTask says so to the agent
// meanwhile, which is what keeps the question open. A handler that has nobody to ask
// answers with NewNoOperatorReply rather than blocking forever: that ends the question
// at once and fails the gated call closed, where silence costs the run a whole window
// first.
type TaskHandler interface {
	Block(Block)
	Question(context.Context, *ElicitRequest) (*ElicitReply, error)
}

// TaskOutcome is how one task ended and what the caller learned on the way.
type TaskOutcome struct {
	// Ack is the acceptance, which carries the conversation token this turn ran under.
	// A caller keeps that token to add another turn.
	Ack *Ack
	// Result is the answer, set when the run answered.
	Result *Result
	// Error is the ending that was not an answer, set when it was not. Exactly one of
	// Result and Error is set for a task that ended; both are nil for one whose set
	// could not be read, which is the error return.
	Error *ErrorMessage
	// Gaps is how many event messages the sequence numbers say never arrived. The
	// stream is advisory and the answer is in the terminal message, so a gap is
	// reported rather than treated as a failure.
	Gaps uint64
	// Unsent holds what a person answered that could not be delivered, because the run
	// had given the question up by the time they answered. It is the answer they typed,
	// in the shape a later request carries, so a caller can offer to send it on one.
	Unsent []*Answer
}

// Answer delivers an answer to a question a task asked and reports what the run said.
//
// ErrAgentUnavailable means the run is not there to hear it: it ended, or this worker
// is not the one running that task. A refusal in the ack means the question is gone,
// which a caller reads the same way. Either way what the person typed is not lost, it
// is sent on a request of its own instead.
func (c *Client) Answer(ctx context.Context, agent, request string, reply *ElicitReply) (*Ack, error) {
	if c.stream == nil {
		return nil, fmt.Errorf("%w: an answer is addressed to a running task", ErrStreamUnsupported)
	}
	if !ValidRequestID(request) {
		return nil, fmt.Errorf("%w: %q is not a valid request id", ErrInvalidMessage, request)
	}

	stampRequest(ctx, &reply.Header, c.sender, agent)

	// An answer correlates to the task that asked rather than to itself, which is what
	// stamping a standalone request gives it, so the tag is set to the task's after.
	reply.Request = request

	data, err := c.marshalValid(reply)
	if err != nil {
		return nil, err
	}

	c.wire.send(OpElicit, agent, request, data)

	raw, err := c.stream.SendElicitReply(ctx, agent, request, data)
	if err != nil {
		return nil, err
	}

	c.wire.recv(OpElicit, agent, request, raw)

	if len(raw) > MaxMessageSize {
		return nil, fmt.Errorf("%w: reply exceeds %d bytes", ErrMessageTooLarge, MaxMessageSize)
	}

	err = c.validator.Validate(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid answer reply: %w", err)
	}

	decoded, err := ExpectProtocol(raw, AckProtocol)
	if err != nil {
		return nil, err
	}

	return decoded.(*Ack), nil
}

// RunTask sends a request and drives the reply set to its end, rendering what arrives
// through h and answering what the run asks.
//
// It is the loop every client of this channel would otherwise write, and the rules it
// obeys are the ones a client gets wrong: say the question is still on screen every
// AckInterval so the agent keeps holding it, stop saying so before answering, and treat
// a question the agent has given up on as a question to answer on a later request
// rather than as an answer to throw away. It also stops expecting the agent to say
// anything while a question is on screen, since the silence there is this caller's.
//
// The error return is for a set that could not be read. A run that failed is not an
// error here: it ended, and how it ended is in TaskOutcome.Error.
func (c *Client) RunTask(ctx context.Context, agent string, req *Request, h TaskHandler) (*TaskOutcome, error) {
	// The one hook that can stop a task, fired before anything is sent so a denial costs
	// nothing and a rewrite is what the agent receives, journals and answers. A request
	// carrying no prompt submits nothing: a resume, a read and an answer all skip it.
	if req.Prompt != "" {
		dec, herr := c.hooks.firePromptSubmit(ctx, ClientPromptSubmitInfo{
			Agent:        agent,
			Request:      req.Request,
			Conversation: req.ConversationToken,
			Prompt:       req.Prompt,
		})
		if herr != nil {
			return nil, fmt.Errorf("PromptSubmit hook: %w", herr)
		}
		if dec.Deny {
			return nil, fmt.Errorf("%w: the prompt was rejected before it was sent: %s", ErrPromptDenied, dec.DenyReason)
		}
		if dec.Prompt != "" {
			req.Prompt = dec.Prompt
		}
	}

	stream, err := c.Task(ctx, agent, req)
	if err != nil {
		return nil, err
	}
	defer stream.Close()

	out := &TaskOutcome{}

	// Questions outlive the messages that carried them: a person is still deciding
	// while the rest of the set arrives. They are given a context of their own so the
	// end of the set takes them down with it, and waited for so nothing writes to the
	// outcome after it is returned.
	asking, stopAsking := context.WithCancel(ctx)
	defer stopAsking()

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		unsent []*Answer
	)

	// endErr is why the set stopped when no terminal message arrived, read by the
	// deferred report below. A set that ended with one leaves it nil.
	var endErr error

	// Every path out of the loop passes through here, which is what makes TurnEnd a
	// point a caller can rely on: a turn that was accepted closes exactly once,
	// whether it answered, failed, was canceled or lost its transport. The questions
	// this task opened are ended and waited for first, so nothing about a question
	// lands after the turn that asked it.
	//
	// A task whose ack never arrived opened no turn, so it closes none.
	defer func() {
		stopAsking()
		wg.Wait()

		mu.Lock()
		out.Unsent = unsent
		mu.Unlock()

		if out.Ack == nil {
			return
		}

		// The cancellation is dropped and the values kept: how a turn ended is a fact
		// rather than work, and the path that most needs reporting is the one where ctx
		// is already dead.
		c.reportTurnEnd(context.WithoutCancel(ctx), agent, out, stream.Request(), endErr)
	}()

	for {
		msg, err := stream.Next(ctx)
		if errors.Is(err, io.EOF) {
			endErr = ErrIncompleteStream

			return out, nil
		}
		if err != nil {
			endErr = err

			return nil, err
		}

		switch m := msg.(type) {
		case *Ack:
			// A refusing ack is followed by the terminal message that says why, so it
			// is recorded and the set is read to its end either way.
			out.Ack = m
			c.reportAck(ctx, agent, req, m, stream.Request())

		case *Event:
			h.Block(m.Block)

		case *ElicitRequest:
			c.hooks.fireQuestionAsked(ctx, QuestionAskedInfo{
				Agent:      agent,
				Request:    stream.Request(),
				QuestionID: m.QuestionID,
				ToolUseID:  m.ToolUseID,
				Kind:       m.Kind,
				Question:   m.Question,
				Display:    m.Display,
			})

			// The set goes quiet from here until somebody decides, and this caller is the
			// reason for it, so the idle bound is lifted until the answer has gone. Every
			// other read stays bounded, where silence does mean the agent has stopped
			// saying anything.
			release := stream.suspend()

			wg.Add(1)
			go func() {
				defer wg.Done()

				outcome := c.answerQuestion(asking, agent, stream.Request(), m, h)
				release()

				// Fired from this goroutine rather than the reader, so it lands when the
				// question is actually done with rather than when the set moves on. The
				// turn waits for it: TurnEnd is reported once these goroutines are done.
				//
				// The cancellation is dropped for the same reason it is on the turn's own
				// ending: a run canceled under a person is exactly when a caller needs to
				// hear that nobody answered.
				c.hooks.fireQuestionAnswered(context.WithoutCancel(ctx), QuestionAnsweredInfo{
					Agent:      agent,
					Request:    stream.Request(),
					QuestionID: m.QuestionID,
					ToolUseID:  m.ToolUseID,
					Answered:   outcome.answered,
					Held:       outcome.held != nil,
				})

				if outcome.held == nil {
					return
				}

				mu.Lock()
				unsent = append(unsent, outcome.held)
				mu.Unlock()
			}()

		case *Result:
			out.Result = m
			out.Gaps = stream.Gaps()

			return out, nil

		case *ErrorMessage:
			out.Error = m
			out.Gaps = stream.Gaps()

			return out, nil
		}
	}
}

// reportAck tells the hooks what the agent said about taking the work, and which
// conversation this turn belongs to.
//
// Whether a conversation is opening or continuing is the request's question rather than
// the ack's: a token the caller sent names a conversation it already had, and a token
// only the ack carries is one the agent has just issued.
func (c *Client) reportAck(ctx context.Context, agent string, req *Request, ack *Ack, request string) {
	if !ack.Accepted {
		c.hooks.fireTurnRefused(ctx, TurnRefusedInfo{Agent: agent, Request: request, Reason: ack.Reason})

		return
	}

	info := ConversationInfo{Agent: agent, Request: request, Conversation: ack.ConversationToken}
	switch {
	case req.ConversationToken != "":
		info.Conversation = req.ConversationToken
		c.hooks.fireConversationResume(ctx, info)

	case ack.ConversationToken != "":
		c.hooks.fireConversationStart(ctx, info)
	}

	c.hooks.fireTurnAccepted(ctx, TurnAcceptedInfo{Agent: agent, Request: request, Conversation: info.Conversation})
}

// reportTurnEnd tells the hooks how the turn ended, from whichever of the two terminal
// messages arrived, or from endErr when neither did.
func (c *Client) reportTurnEnd(ctx context.Context, agent string, out *TaskOutcome, request string, endErr error) {
	info := ClientTurnEndInfo{Agent: agent, Request: request, Err: endErr}
	if out.Ack != nil {
		info.Conversation = out.Ack.ConversationToken
	}

	switch {
	case out.Result != nil:
		info.Answered = true
		info.StopReason = out.Result.StopReason
		info.Usage = out.Result.Usage

	case out.Error != nil:
		info.Code = out.Error.Code
		info.StopReason = out.Error.StopReason
		info.Usage = out.Error.Usage
	}

	c.hooks.fireTurnEnd(ctx, info)
}

// questionOutcome is what became of one question: whether an answer reached the run,
// and what a person decided that could not be delivered to it.
//
// The two are separate because most of what is not delivered is not held either: a
// question nobody answered, one the operator dismissed, and one answered by a handler
// with nobody to ask all end with neither.
type questionOutcome struct {
	answered bool
	held     *Answer
}

// answerQuestion puts one question to the handler and delivers the answer, saying that
// somebody is still there while it waits.
//
// What it reports is what happened to the question, so a caller can tell an answered
// one from a question given up on, and it returns what could not be delivered so a
// person's decision survives the run having moved on under them.
func (c *Client) answerQuestion(ctx context.Context, agent, request string, ask *ElicitRequest, h TaskHandler) questionOutcome {
	reply, err := c.waitForAnswer(ctx, agent, request, ask, func() (*ElicitReply, error) {
		return h.Question(ctx, ask)
	})
	// Nobody answered: the set ended under the question, the run was canceled, or the
	// operator dismissed it. Nothing reached the run and there is nothing to keep.
	if err != nil || reply == nil {
		return questionOutcome{}
	}

	_, err = c.Answer(ctx, agent, request, reply)
	if err == nil {
		return questionOutcome{answered: true}
	}

	// Only a decision is worth keeping. A no-operator reply is what a handler produces
	// when it has nobody to ask, which includes the case where the set ended and the
	// question was taken off the screen under somebody: delivering that later would
	// decline a gated command on their behalf, having never shown them the question, and
	// delivering it to a question that is already gone changes nothing either way.
	if reply.Answer == AnswerNoOperator {
		return questionOutcome{}
	}

	// The run is not there to hear it. What the person typed is worth keeping: it goes
	// on a request of its own, where the answer names the call rather than the question,
	// since a resumed run mints a new question id.
	held, buildErr := NewAnswer(ask, reply)
	if buildErr != nil {
		return questionOutcome{}
	}

	return questionOutcome{held: held}
}

// waitForAnswer runs get and, while it is running, tells the agent every AckInterval
// that the question is still in front of somebody, which restarts the window.
//
// A question whose agent takes no such replies (no window, or one that refuses the
// reply) is simply waited on: the answer is either inside the window or too late, and
// too late is what Unsent is for.
func (c *Client) waitForAnswer(ctx context.Context, agent, request string, ask *ElicitRequest, get func() (*ElicitReply, error)) (*ElicitReply, error) {
	type answered struct {
		reply *ElicitReply
		err   error
	}

	done := make(chan answered, 1)

	go func() {
		reply, err := get()
		done <- answered{reply: reply, err: err}
	}()

	interval := ask.AckInterval()
	if interval <= 0 {
		got := <-done

		return got.reply, got.err
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case got := <-done:
			return got.reply, got.err

		case <-ctx.Done():
			return nil, ctx.Err()

		case <-ticker.C:
			// A refusal here means the question is gone. Nothing is sent about it: the
			// person is left with what they were typing, and the answer travels on a
			// request of its own if they finish it.
			ack, err := c.Answer(ctx, agent, request, NewWaitingAck(ask, c.sender))
			if err != nil || (ack != nil && !ack.Accepted) {
				got := <-done

				return got.reply, got.err
			}
		}
	}
}
