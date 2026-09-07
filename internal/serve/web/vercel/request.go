//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package vercel

import (
	"fmt"
	"strings"

	"github.com/choria-io/fisk-ai/internal/serve/web"
	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// request is the body useChat posts. Three fields are read and the rest is dropped.
//
// id, messages, trigger and messageId are applied after the body spread on the client
// and cannot be overridden, so the answer this format needs goes in a field of its
// own.
type request struct {
	// ID is the chat the page is on, which the channel hashes into the session the
	// conversation is journaled under.
	ID string `json:"id"`

	// Messages is the whole history the client holds. Only the newest message is read.
	Messages []message `json:"messages"`

	// FiskAnswer carries the answer to a question the AI SDK has no approval flow for,
	// which is the three human-in-the-loop kinds, and the standing allow of a confirm
	// gate, which has no boolean in the SDK's own approval to travel in.
	FiskAnswer *fiskAnswer `json:"fiskAnswer"`
}

// message is one message of the history.
type message struct {
	Role  string `json:"role"`
	Parts []part `json:"parts"`
}

// part is one part of a message. A text part and a tool part are the two this reads,
// and they share a struct because one shape is ever populated at a time.
type part struct {
	Type       string        `json:"type"`
	Text       string        `json:"text"`
	ToolCallID string        `json:"toolCallId"`
	State      string        `json:"state"`
	Approval   *partApproval `json:"approval"`
}

// partApproval is what the person said about a tool-approval-request, as the client
// writes it back into the part it mutated.
type partApproval struct {
	ID       string `json:"id"`
	Approved bool   `json:"approved"`
}

// fiskAnswer is the answer to a question this format sent as a data part, and the
// three-way answer to an approval. It names the call because the answer is keyed by
// the call rather than by a question id: the question is asked again on every resume.
type fiskAnswer struct {
	ToolUseID string `json:"toolUseId"`
	Kind      string `json:"kind"`
	// Approval is "no", "once" or "always" for an approve answer.
	Approval string `json:"approval"`
	// Confirmed answers a confirm question, Index a select question by the position of
	// the option chosen, and Value an input question.
	Confirmed bool   `json:"confirmed"`
	Index     int    `json:"index"`
	Value     string `json:"value"`
}

// approvedState is the state the AI SDK writes on a tool part whose approval the
// person answered.
const approvedState = "approval-responded"

// turn is what this request asks of the conversation.
func (r *request) turn() (web.Turn, error) {
	answer, err := r.answer()
	if err != nil {
		return web.Turn{}, err
	}

	out := web.Turn{ThreadID: r.ID}
	if answer != nil {
		out.Answer = answer

		return out, nil
	}

	out.Prompt = r.prompt()

	return out, nil
}

// answer is the answer this request carries, nil for one that only prompts.
func (r *request) answer() (*web.Answer, error) {
	if r.FiskAnswer != nil {
		return r.FiskAnswer.answer()
	}

	return r.approval(), nil
}

// approval reads an answered tool-approval-request out of the history.
//
// Only the newest message is read, and only when it is the assistant's: answering an
// approval submits with no new user message, so the assistant message is last. A body
// whose newest message is the user's is a new turn, and the approvals in its history
// were answered on the request that made each of them newest.
//
// The last answered part is the one this request is about. A declined call never gets
// an output part, so its part stays in the state the person left it in for the life of
// the assistant message, and useChat continues that same message across every
// answer-only POST. Reading forwards would answer the declined call again on every
// later approval, and the question the person just answered would be put again forever.
func (r *request) approval() *web.Answer {
	if len(r.Messages) == 0 {
		return nil
	}

	last := r.Messages[len(r.Messages)-1]
	if last.Role != "assistant" {
		return nil
	}

	for i := len(last.Parts) - 1; i >= 0; i-- {
		p := last.Parts[i]
		if p.State != approvedState || p.Approval == nil || p.ToolCallID == "" {
			continue
		}

		// The SDK's approval is a boolean, so it reaches the gate as the answer for
		// this call alone. A page wanting the standing allow sends fiskAnswer.
		choice := toolkit.ConfirmNo
		if p.Approval.Approved {
			choice = toolkit.ConfirmOnce
		}

		return &web.Answer{ToolUseID: p.ToolCallID, Kind: web.KindApprove, Approval: choice}
	}

	return nil
}

// prompt is the newest user message as one string.
//
// useChat resends the whole history on every POST and the AI SDK's own documentation
// treats that history as the conversation. This worker has an authoritative journal,
// so the turn being asked for is what is read out of the body and nothing else in it
// is trusted.
func (r *request) prompt() string {
	for i := len(r.Messages) - 1; i >= 0; i-- {
		if r.Messages[i].Role != "user" {
			continue
		}

		var text []string

		for _, p := range r.Messages[i].Parts {
			if p.Type == "text" && p.Text != "" {
				text = append(text, p.Text)
			}
		}

		return strings.TrimSpace(strings.Join(text, "\n"))
	}

	return ""
}

// answer is the answer in the shape the channel holds for the prompter. Each kind
// carries the one field it is answered with, so an answer of the wrong shape is
// refused here rather than reaching the run as a value nobody chose.
func (a *fiskAnswer) answer() (*web.Answer, error) {
	out := &web.Answer{ToolUseID: a.ToolUseID, Kind: web.QuestionKind(a.Kind)}

	switch out.Kind {
	case web.KindApprove:
		choice, err := approvalChoice(a.Approval)
		if err != nil {
			return nil, err
		}

		out.Approval = choice

	case web.KindConfirm:
		out.Confirmed = a.Confirmed

	case web.KindSelect:
		if a.Index < 0 {
			return nil, fmt.Errorf("%w: the answer chooses option %d, and an option is named by its position in the list", ErrBadRequest, a.Index)
		}

		out.Index = a.Index

	case web.KindInput:
		out.Value = a.Value

	default:
		return nil, fmt.Errorf("%w: %q is not a question this agent asks", ErrBadRequest, a.Kind)
	}

	return out, nil
}

// approvalChoice is the operator's three-way answer to a confirm gate. The standing
// allow is the value the SDK's own approval cannot carry, which is why this field
// exists.
func approvalChoice(answer string) (toolkit.ConfirmChoice, error) {
	switch answer {
	case "no":
		return toolkit.ConfirmNo, nil
	case "once":
		return toolkit.ConfirmOnce, nil
	case "always":
		return toolkit.ConfirmAlways, nil
	default:
		return toolkit.ConfirmNo, fmt.Errorf("%w: an approval is answered with no, once or always, and this one says %q", ErrBadRequest, answer)
	}
}
