//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	slackgo "github.com/slack-go/slack"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/toolkit/builtin"
)

// interactionKind is which interaction one envelope carries.
type interactionKind int

const (
	// interactionPress is a button on a message this bot posted.
	interactionPress interactionKind = iota
	// interactionSubmit is the dialog a free-text answer was typed into, being sent.
	interactionSubmit
)

// click is one interaction reduced to what this channel acts on.
//
// The conversation comes from the envelope's own authenticated fields rather than from
// anything the button carried. A workspace member knows the team, channel and thread of
// every thread they can see, so those are not a secret that would make a hash of them a
// capability; reading them from the envelope is what keeps the property both sibling
// channels have.
type click struct {
	// Interaction says which of the two shapes this is.
	Interaction interactionKind

	// TeamID, ChannelID and ThreadTS identify the conversation.
	TeamID    string
	ChannelID string
	ThreadTS  string

	// MessageTS is the question message the button was on, empty for a dialog submission.
	MessageTS string

	// UserID is who pressed it, which the question message records. Anybody in the thread
	// may answer, so this is not required to be the person the question was put to.
	UserID string

	// TriggerID opens a dialog. It expires three seconds after the click and may be used
	// once, so nothing waits on it.
	TriggerID string

	// Value is what the button carried, or what the dialog was stamped with when it was
	// opened.
	Value buttonValue

	// Text is what somebody typed into the dialog.
	Text string
}

// The dialog a free-text answer is typed into.
//
// The callback id is checked on submission, so a dialog some other app opened in this
// workspace is not taken for an answer. The block and action ids name the one field, which
// is where the typed value is read from.
const (
	modalCallbackID = "fisk_ai_answer"
	modalBlockID    = "answer"
	modalActionID   = "text"
)

// The words the dialog is drawn with.
const (
	modalTitle       = "Your answer"
	modalSubmit      = "Send"
	modalClose       = "Cancel"
	modalLabel       = "Answer"
	modalPlaceholder = "Type your answer"
)

// triggerWindow bounds opening a dialog. The trigger a click carries expires three seconds
// after the press, so a call still waiting on the workspace's allowance past this window is
// worth abandoning rather than spending: it would reach Slack with a trigger Slack has
// already retired.
const triggerWindow = 2 * time.Second

// modalMeta is what a dialog carries so its submission can be placed.
//
// A view_submission payload carries neither the value of the button that opened the dialog
// nor a channel and a thread, so everything the answer has to be routed by is stamped in
// when the dialog is opened. Slack returns it unchanged, and this bot is the only writer of
// it.
type modalMeta struct {
	// Question is the value of the button that opened the dialog.
	Question buttonValue `json:"question"`

	// ChannelID and ThreadTS are the conversation the button was pressed in, taken from
	// that press's own authenticated envelope.
	ChannelID string `json:"channel"`
	ThreadTS  string `json:"thread"`
}

// clickOf decodes one envelope into the interaction this channel acts on, reporting false
// for an envelope that is not one.
//
// It reads the payload and nothing else: the caller acknowledges before acting, Slack
// redelivering an interaction it has not been answered about within three seconds exactly as
// it does a mention.
func clickOf(env envelope) (*click, bool, error) {
	if env.Kind != envelopeInteractive {
		return nil, false, nil
	}

	// The token is not verified for the reason a mention's is not: socket mode already
	// authenticated the connection, this process having opened it with its own app-level
	// token, so there is no shared secret left to check.
	var cb slackgo.InteractionCallback

	err := json.Unmarshal(env.Payload, &cb)
	if err != nil {
		return nil, false, fmt.Errorf("decoding an interaction envelope: %w", err)
	}

	switch cb.Type {
	case slackgo.InteractionTypeBlockActions:
		return pressOf(&cb)
	case slackgo.InteractionTypeViewSubmission:
		return submitOf(&cb)
	default:
		return nil, false, nil
	}
}

// pressOf reads a button press.
//
// One press reports one action, and a message of this channel's carries only buttons, so the
// first is the one that was pressed. A press carrying a value this channel did not mint is
// refused rather than routed.
func pressOf(cb *slackgo.InteractionCallback) (*click, bool, error) {
	if len(cb.ActionCallback.BlockActions) == 0 {
		return nil, false, nil
	}

	value, err := decodeValue(cb.ActionCallback.BlockActions[0].Value)
	if err != nil {
		return nil, false, err
	}

	in := &click{
		Interaction: interactionPress,
		TeamID:      cb.Team.ID,
		ChannelID:   cb.Container.ChannelID,
		ThreadTS:    cb.Container.ThreadTs,
		MessageTS:   cb.Container.MessageTs,
		UserID:      cb.User.ID,
		TriggerID:   cb.TriggerID,
		Value:       value,
	}

	// The container is where a press on a message reports its conversation; the channel is
	// where an interaction with no container reports one.
	if in.ChannelID == "" {
		in.ChannelID = cb.Channel.ID
	}

	err = in.complete()
	if err != nil {
		return nil, false, err
	}

	return in, true, nil
}

// submitOf reads a dialog being sent.
//
// The value of the button that opened it comes back out of private_metadata, the payload
// carrying no action of its own, and the typed text off the one field the dialog holds. An
// empty field is a valid answer, so nothing here refuses one.
func submitOf(cb *slackgo.InteractionCallback) (*click, bool, error) {
	if cb.View.CallbackID != modalCallbackID {
		return nil, false, nil
	}

	var meta modalMeta

	err := json.Unmarshal([]byte(cb.View.PrivateMetadata), &meta)
	if err != nil {
		return nil, false, fmt.Errorf("reading what a dialog was opened for: %w", err)
	}

	if meta.Question.ToolUse == "" || !meta.Question.Kind.known() {
		return nil, false, fmt.Errorf("a dialog arrived without the question it answers")
	}

	in := &click{
		Interaction: interactionSubmit,
		TeamID:      cb.Team.ID,
		ChannelID:   meta.ChannelID,
		ThreadTS:    meta.ThreadTS,
		UserID:      cb.User.ID,
		Value:       meta.Question,
	}

	if cb.View.State != nil {
		in.Text = cb.View.State.Values[modalBlockID][modalActionID].Value
	}

	err = in.complete()
	if err != nil {
		return nil, false, err
	}

	return in, true, nil
}

// complete refuses an interaction missing what an answer is placed by. The click path
// derives the conversation from these, so one arriving without them could only be delivered
// by trusting the button's own bytes.
func (in *click) complete() error {
	if in.UserID == "" || in.ChannelID == "" || in.ThreadTS == "" {
		return fmt.Errorf("an interaction arrived without the identifiers an answer is placed by")
	}

	return nil
}

// clicked acts on one interaction and acknowledges it.
//
// The decode is in memory and the acknowledgement follows it, which is the mention path's
// order and the same three-second rule: Slack redelivers an interaction it has not been
// answered about, and a dialog acknowledged late stays on the screen of whoever sent it.
//
// A press on the Reply button of a free-text question opens the dialog and answers nothing.
// Everything else carries the answer itself.
func (c *Channel) clicked(env envelope) {
	in, wanted, err := clickOf(env)
	if err != nil {
		// An envelope this channel cannot read is still one Slack expects an answer to, so
		// it is acknowledged rather than left to be redelivered forever.
		c.log.Warn("Dropping an interaction that could not be read", "error", err)
	}

	c.acknowledge(env)

	if !wanted {
		return
	}

	if in.Interaction == interactionPress && in.Value.Kind == kindInput && in.Value.Choice == choiceReply {
		c.openReply(in)

		return
	}

	c.answerQuestion(in)
}

// openReply puts the dialog a free-text answer is typed into in front of whoever pressed
// Reply.
//
// It is started here rather than after any other work, the trigger expiring three seconds
// after the press, and it runs under triggerWindow so a call held behind the workspace's
// allowance past that is abandoned rather than spent on a trigger Slack has retired.
//
// The dialog is opened whether or not this worker still holds the question. A person who
// pressed Reply has an answer to give either way, and what becomes of it is decided when
// they send it.
func (c *Channel) openReply(in *click) {
	meta, err := json.Marshal(modalMeta{Question: in.Value, ChannelID: in.ChannelID, ThreadTS: in.ThreadTS})
	if err != nil {
		c.log.Warn("Building what a dialog is opened for failed", "tool_use", in.Value.ToolUse, "error", err)

		return
	}

	def := c.asked.defaultFor(in.Value.ToolUse)

	c.speak(func() {
		ctx, cancel := context.WithTimeout(context.Background(), triggerWindow)
		defer cancel()

		err := c.limit.take(ctx)
		if err != nil {
			c.log.Warn("Waiting for the allowance to open a dialog failed", "tool_use", in.Value.ToolUse, "error", err)

			return
		}

		err = c.api.openView(ctx, in.TriggerID, modalView{
			Title:       modalTitle,
			Submit:      modalSubmit,
			Close:       modalClose,
			CallbackID:  modalCallbackID,
			Metadata:    string(meta),
			BlockID:     modalBlockID,
			ActionID:    modalActionID,
			Label:       modalLabel,
			Placeholder: modalPlaceholder,
			Initial:     def,
		})
		if err != nil {
			c.log.Warn("Opening the dialog a free-text answer is typed into failed", "tool_use", in.Value.ToolUse, "error", err)
		}
	})
}

// answerQuestion routes one answer to the question it names.
//
// A live run takes it. A run that stopped waiting before the click landed, and a call this
// worker holds no question for at all, both reach the conversation as a resume. The second
// is what a press after a restart is: the envelope names the thread, and the value names
// the call and the kind, which is everything the answer has to be built from.
//
// A call that already has an answer starts nothing, one call taking one answer. A press
// naming a call this worker holds a question for in another conversation starts nothing
// either: the conversation a resume runs in comes from the envelope, so a value presented
// against a thread it was not minted in reaches a journal that never made the call.
func (c *Channel) answerQuestion(in *click) {
	q, g, out := c.asked.deliver(in)

	switch out {
	case deliveryTaken:
		c.log.Info("An answer reached the run that asked for it",
			"tool_use", in.Value.ToolUse, "kind", in.Value.Kind, "by", in.UserID)

	case deliveryResume:
		c.log.Info("An answer arrived after the run had stopped waiting for it",
			"tool_use", in.Value.ToolUse, "kind", in.Value.Kind, "by", in.UserID, "channel", in.ChannelID, "thread", in.ThreadTS)

		c.resume(in, q, g)

	case deliveryUnknown:
		c.log.Info("An answer arrived for a question this worker is not holding, so it resumes the thread it was given in",
			"tool_use", in.Value.ToolUse, "kind", in.Value.Kind, "by", in.UserID, "channel", in.ChannelID, "thread", in.ThreadTS)

		c.resume(in, nil, nil)

	case deliveryAnswered:
		c.log.Info("An answer reached a call that already has one",
			"tool_use", in.Value.ToolUse, "kind", in.Value.Kind, "by", in.UserID)

		if g != nil {
			c.speak(func() { c.recordAnswer(q, g, secondPressLine) })
		}

	case deliveryElsewhere:
		c.log.Warn("An answer named a call this worker is holding a question for in another conversation",
			"tool_use", in.Value.ToolUse, "kind", in.Value.Kind, "channel", in.ChannelID, "thread", in.ThreadTS)
	}
}

// resume turns one click into a turn of its own, which is the second source Next produces
// work from.
//
// q is the question this worker is holding, and nil where it holds none. Everything the
// resume needs is in the click either way, which is what the call and the kind travel in
// the value for.
//
// The answer is written onto the question only once the turn has been admitted. A press
// this channel could not take leaves the question where it found it, buttons and all, so
// whoever is told to press again has a button to press.
func (c *Channel) resume(in *click, q *question, g *given) {
	m := clickMention(in)

	answer, err := answerFor(in)
	if err != nil {
		c.log.Error("Building the answer a press carried failed",
			"tool_use", in.Value.ToolUse, "kind", in.Value.Kind, "error", err)
		c.declinePress(m, q, unreadablePressRefusal)

		return
	}

	// A resume waits behind the turn that asked and is refused by any other, since what
	// that turn is ending on is this very question.
	var askedBy string
	if q != nil {
		askedBy = q.turn
	}

	refusal, narration := c.admitResume(m, in, askedBy, answer)
	if refusal != "" {
		c.log.Warn("Refusing an answer", "channel", in.ChannelID, "thread", in.ThreadTS, "reason", refusal)
		c.declinePress(m, q, refusal)

		return
	}

	if q != nil && g != nil {
		c.speak(func() { c.recordAnswer(q, g, lateAnswerLine) })
	}

	c.startStatus(narration)
}

// declinePress tells whoever pressed that their answer is not being acted on and puts the
// question back where they found it.
//
// The line goes on the question message where this worker holds one, which is where the
// person is looking and where the button they pressed still is. A worker that restarted
// since the question was asked holds none, so it goes into the thread instead.
func (c *Channel) declinePress(m *mention, q *question, line string) {
	if q == nil {
		c.reply(m, line)

		return
	}

	c.asked.reopen(q)
	c.speak(func() { c.pressNote(q, line) })
}

// clickMention places one click in the thread it was made in, in the shape the rest of this
// channel decides a turn on.
//
// Every identifier is the interaction envelope's own, so a resume reaches the journal of
// the thread the button was pressed in and no other. The text is empty: a resume adds no
// turn to the conversation, it supplies the result of a call the conversation is already
// waiting on.
func clickMention(in *click) *mention {
	return &mention{
		TeamID:    in.TeamID,
		ChannelID: in.ChannelID,
		ThreadTS:  in.ThreadTS,
		TS:        in.MessageTS,
		UserID:    in.UserID,
	}
}

// answerFor is the result the tool that asked would have returned, which the resume
// supplies to the call that deferred.
//
// This channel renders it rather than reading a shape off the button, because the shape is
// the one that tool's own results take and the model was told to expect it. It is built
// from the click alone, so a worker that restarted between the question and the press
// answers as completely as the one that asked.
//
// The confirm gate has none. Its call was never dispatched, so the resume dispatches it
// and the gate asks again, answered by approvalFrom rather than by anything here.
func answerFor(in *click) (*agent.DeferredAnswer, error) {
	if in.Value.Kind == kindApprove {
		return nil, nil
	}

	content, err := renderAnswer(in)
	if err != nil {
		return nil, err
	}

	return &agent.DeferredAnswer{ToolUseID: in.Value.ToolUse, Content: content}, nil
}

// approvalFrom reads the gate's approval off a press, reporting false for a press that
// answers one of the three tools instead.
//
// Anything that is not an explicit allow is a refusal, which is the direction the gate
// defaults in, so a value carrying a choice this bot never minted declines the command.
func approvalFrom(in *click) (toolkit.ConfirmChoice, bool) {
	if in.Value.Kind != kindApprove {
		return toolkit.ConfirmNo, false
	}

	switch in.Value.Choice {
	case choiceOnce:
		return toolkit.ConfirmOnce, true
	case choiceAlways:
		return toolkit.ConfirmAlways, true
	default:
		return toolkit.ConfirmNo, true
	}
}

// renderAnswer produces the result of the built-in that asked the question.
//
// A selection answers with the option rather than with its position, which is what the
// model was told ask_human_select returns, so the option is read off the button that was
// pressed rather than out of a list this worker may no longer hold.
func renderAnswer(in *click) (string, error) {
	switch in.Value.Kind {
	case kindConfirm:
		return builtin.ConfirmResult(in.Value.Choice == choiceYes, "")
	case kindSelect:
		return builtin.SelectResult(in.Value.Label, "")
	case kindInput:
		return builtin.InputResult(in.Text, "")
	default:
		return "", fmt.Errorf("%q has no result shape", in.Value.Kind)
	}
}
