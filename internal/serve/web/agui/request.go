//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agui

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/types"
	"github.com/segmentio/ksuid"

	"github.com/choria-io/fisk-ai/internal/serve/web"
	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// request is the AG-UI run input a client posts. The SDK's own type reads it, snake
// case and camel case alike, and four of its fields are used: the thread, the run, the
// resume entries and the messages.
//
// State, Tools, Context and ForwardedProps are dropped. This agent's tools are its own
// and a browser does not supply them, and nothing here reads a state a client keeps.
type request struct {
	types.RunAgentInput
}

// resumePayload is what a client sends to answer one interrupt, in the shape the
// interrupt's responseSchema asked for.
//
// Every field is a pointer because an absent answer and a zero one are different
// things: false is an answer to a confirm question and "" is an answer to an input one.
type resumePayload struct {
	// Approval is the three-way answer the approval schema asks for: no, once or
	// always.
	Approval *string `json:"approval"`
	// Approved is the two-way approval AG-UI recommends, read from a client that
	// renders its own approval control rather than the schema. A page that sends it
	// can allow a command once or decline it, and cannot ask this agent to stop
	// asking.
	Approved *bool `json:"approved"`
	// Confirmed answers a confirm question, Index a select question by the position of
	// the option chosen, and Value an input question.
	Confirmed *bool   `json:"confirmed"`
	Index     *int    `json:"index"`
	Value     *string `json:"value"`
}

// interrupted is the resume entry that settles the question the conversation is waiting
// on, with the question read back out of the interrupt it names.
type interrupted struct {
	entry types.ResumeEntry
	kind  web.QuestionKind
	call  string
}

// turn is what this request asks of the conversation, and the call the run that raised
// the interrupt already sent to the client.
//
// That call is empty for everything but a resume of one of the three question kinds.
// Those are put by a tool that ran, so the run that asked traced the call and the client
// holds it, and a client accumulates the argument fragments of a call rather than
// replacing them: sending the call again would write its arguments into the thread
// twice. The gate runs before the run traces a call, so an approval's call has not been
// sent and the run that answers sends it for the first time.
func (r *request) turn() (web.Turn, string, error) {
	out := web.Turn{ThreadID: r.ThreadID}

	if len(r.Resume) == 0 {
		out.Prompt = r.prompt()

		return out, "", nil
	}

	resumed, found := r.resumed()
	if !found {
		return web.Turn{}, "", fmt.Errorf("%w: no entry of the resume array names an interrupt this agent raised, so there is nothing to resume", ErrBadRequest)
	}

	sent := ""
	if resumed.kind != web.KindApprove {
		sent = resumed.call
	}

	if resumed.entry.Status == types.ResumeStatusCancelled {
		out.Answer = withdrawn(resumed)

		return out, sent, nil
	}

	answer, err := answerFor(resumed.kind, resumed.call, resumed.entry.Payload)
	if err != nil {
		return web.Turn{}, "", err
	}

	out.Answer = answer

	return out, sent, nil
}

// withdrawn is what a canceled entry resumes the conversation with.
//
// Canceling covers an interrupt without settling it, so the conversation resumes, the
// call is dispatched again and the question is put again. The channel takes a turn
// carrying a prompt or an answer and this one carries no prompt, so it carries an
// answer the run cannot spend: it names the interrupt rather than the call the question
// was about, and the prompter spends an answer only on the call it names.
func withdrawn(resumed interrupted) *web.Answer {
	return &web.Answer{ToolUseID: resumed.entry.InterruptID, Kind: resumed.kind}
}

// runID is the run this response is written under, minted when the client named none.
// An AG-UI client mints one per run, and it is what makes the ids of the messages this
// turn writes distinct from the ids of the turn before it.
func (r *request) runID() string {
	if r.RunID != "" {
		return r.RunID
	}

	return ksuid.New().String()
}

// resumed is the entry of this request's resume array that settles the question the
// conversation is waiting on.
//
// A resume covers every interrupt the run left open, and a Fisk run raises one at a
// time: its prompter runs on the loop's own goroutine, so the first question ends the
// turn. So the first entry naming an interrupt this agent raised is the one the
// conversation is waiting on, and an entry naming anything else belongs to a run this
// agent did not serve.
func (r *request) resumed() (interrupted, bool) {
	for _, entry := range r.Resume {
		kind, toolUseID, ok := splitInterruptID(entry.InterruptID)
		if !ok {
			continue
		}

		return interrupted{entry: entry, kind: kind, call: toolUseID}, true
	}

	return interrupted{}, false
}

// prompt is the newest user message as one string.
//
// An AG-UI client sends the whole thread on every run and its own docs treat that
// thread as the conversation. This worker has an authoritative journal, so the turn
// being asked for is what is read out of the body and nothing else in it is trusted.
func (r *request) prompt() string {
	for i := len(r.Messages) - 1; i >= 0; i-- {
		if r.Messages[i].Role != types.RoleUser {
			continue
		}

		return strings.TrimSpace(messageText(r.Messages[i]))
	}

	return ""
}

// messageText is the text of one message.
//
// AG-UI carries a message's content as a string or as a list of typed fragments, and a
// browser sending an image alongside its words sends the list. The text fragments are
// joined and the rest is passed over: this agent takes a prompt in words.
func messageText(m types.Message) string {
	switch content := m.Content.(type) {
	case string:
		return content

	case []any:
		var text []string

		for _, item := range content {
			fragment, ok := item.(map[string]any)
			if !ok {
				continue
			}

			if fragment["type"] != types.InputContentTypeText {
				continue
			}

			line, ok := fragment["text"].(string)
			if ok && line != "" {
				text = append(text, line)
			}
		}

		return strings.Join(text, "\n")

	default:
		return ""
	}
}

// answerFor reads one interrupt's payload as the answer to the question it was raised
// for. Each kind takes the one field it is answered with, so a payload of the wrong
// shape is refused here rather than reaching the run as a value nobody chose.
func answerFor(kind web.QuestionKind, toolUseID string, raw any) (*web.Answer, error) {
	payload, err := decodePayload(raw)
	if err != nil {
		return nil, err
	}

	out := &web.Answer{ToolUseID: toolUseID, Kind: kind}

	switch kind {
	case web.KindApprove:
		choice, err := approvalChoice(payload)
		if err != nil {
			return nil, err
		}

		out.Approval = choice

	case web.KindConfirm:
		if payload.Confirmed == nil {
			return nil, fmt.Errorf("%w: a yes or no question is answered with a confirmed field, and this payload has none", ErrBadRequest)
		}

		out.Confirmed = *payload.Confirmed

	case web.KindSelect:
		if payload.Index == nil {
			return nil, fmt.Errorf("%w: a choice is answered with an index field naming the position of the option chosen, and this payload has none", ErrBadRequest)
		}
		if *payload.Index < 0 {
			return nil, fmt.Errorf("%w: the answer chooses option %d, and an option is named by its position in the list", ErrBadRequest, *payload.Index)
		}

		out.Index = *payload.Index

	case web.KindInput:
		if payload.Value == nil {
			return nil, fmt.Errorf("%w: a question asking for a value is answered with a value field, and this payload has none", ErrBadRequest)
		}

		out.Value = *payload.Value
	}

	return out, nil
}

// decodePayload reads an interrupt's payload, which reaches here as whatever JSON the
// client sent, into the fields the four schemas ask for.
func decodePayload(raw any) (*resumePayload, error) {
	if raw == nil {
		return &resumePayload{}, nil
	}

	body, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadRequest, err)
	}

	var out resumePayload

	err = json.Unmarshal(body, &out)
	if err != nil {
		return nil, fmt.Errorf("%w: the payload answering an interrupt is an object of the fields its schema asks for: %w", ErrBadRequest, err)
	}

	return &out, nil
}

// approvalChoice is the operator's three-way answer to a confirm gate.
//
// The schema asks for approval, which carries the standing allow the AI SDK's own
// boolean cannot. A client that renders its own approval control sends the boolean
// AG-UI recommends instead, and gets an allow for this call or a decline.
func approvalChoice(payload *resumePayload) (toolkit.ConfirmChoice, error) {
	if payload.Approval != nil {
		switch *payload.Approval {
		case "no":
			return toolkit.ConfirmNo, nil
		case "once":
			return toolkit.ConfirmOnce, nil
		case "always":
			return toolkit.ConfirmAlways, nil
		default:
			return toolkit.ConfirmNo, fmt.Errorf("%w: an approval is answered with no, once or always, and this one says %q", ErrBadRequest, *payload.Approval)
		}
	}

	if payload.Approved != nil {
		if *payload.Approved {
			return toolkit.ConfirmOnce, nil
		}

		return toolkit.ConfirmNo, nil
	}

	return toolkit.ConfirmNo, fmt.Errorf("%w: an approval is answered with an approval field of no, once or always, or an approved boolean, and this payload has neither", ErrBadRequest)
}

// interruptID names one interrupt from the question it carries: the kind, so a client
// reads what it is being asked without a schema, and the call, so the answer reaches
// the call it belongs to.
//
// Nothing is remembered on either side of it. The question is asked again under a new
// question id on every resume, so the call is the one thing both ends agree an answer
// is about, and any process may serve the run that answers.
func interruptID(kind web.QuestionKind, toolUseID string) string {
	return string(kind) + ":" + toolUseID
}

// splitInterruptID reads an interrupt id back into the question it named, and reports
// whether it is one this agent raised.
func splitInterruptID(id string) (web.QuestionKind, string, bool) {
	prefix, toolUseID, found := strings.Cut(id, ":")
	if !found || toolUseID == "" {
		return "", "", false
	}

	kind := web.QuestionKind(prefix)

	switch kind {
	case web.KindApprove, web.KindConfirm, web.KindSelect, web.KindInput:
		return kind, toolUseID, true
	default:
		return "", "", false
	}
}
