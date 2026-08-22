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

	if meta.Question.Kind == "" || meta.Question.ToolUse == "" {
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
// A live run takes it. A question this worker is not holding reaches nothing and is logged.
// A run that stopped waiting before the click landed leaves an answer that has to reach the
// conversation as a resume, which is the next thing built here; the message is rewritten
// meanwhile, so nobody is left looking at a button they pressed with no sign it registered.
func (c *Channel) answerQuestion(in *click) {
	q, g, out := c.asked.deliver(in)

	switch out {
	case deliveryTaken:
		c.log.Info("An answer reached the run that asked for it",
			"tool_use", in.Value.ToolUse, "kind", in.Value.Kind, "by", in.UserID)

	case deliveryResume:
		c.log.Info("An answer arrived after the run had stopped waiting for it",
			"tool_use", in.Value.ToolUse, "kind", in.Value.Kind, "by", in.UserID, "channel", in.ChannelID, "thread", in.ThreadTS)

		c.speak(func() { c.recordAnswer(q, g, true) })

	default:
		c.log.Warn("An answer reached no question this worker is holding",
			"tool_use", in.Value.ToolUse, "kind", in.Value.Kind, "channel", in.ChannelID, "thread", in.ThreadTS)
	}
}
