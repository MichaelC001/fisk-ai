//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package slack

import (
	"encoding/json"
	"fmt"

	slackgo "github.com/slack-go/slack"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/toolkit/builtin"
)

// interactionKind is which interaction one envelope carries.
type interactionKind int

const (
	// interactionPress is a button on a message this bot posted, or the field a free-text
	// answer was typed into on one. Slack reports both as block_actions.
	interactionPress interactionKind = iota
	// interactionMention is a mention that answered the free-text question open in its
	// thread. Nothing decodes one off the socket: the intake mints it so the answer takes
	// the path a press takes, there being one way an answer reaches a run.
	interactionMention
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

	// MessageTS is the question message the answer was given on.
	MessageTS string

	// UserID is who pressed it, which the question message records. Anybody in the thread
	// may answer, so this is not required to be the person the question was put to.
	UserID string

	// Value is what the control carried: the button's own value, or the action id the
	// field a free-text answer is typed into was drawn with.
	Value buttonValue

	// Text is what somebody typed.
	Text string
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

	if cb.Type != slackgo.InteractionTypeBlockActions {
		return nil, false, nil
	}

	return pressOf(&cb)
}

// pressOf reads a button press, or an answer typed into the field a free-text question
// carries.
//
// One interaction reports one action, and a message of this channel's carries one control a
// person acts on at a time, so the first action is the one they used. A button carries what
// this channel minted in its own value. A plain_text_input element takes no such value, so
// the same bytes travel in its action id and the typed text arrives as the action's value.
//
// An interaction carrying a value this channel did not mint is refused rather than routed.
func pressOf(cb *slackgo.InteractionCallback) (*click, bool, error) {
	if len(cb.ActionCallback.BlockActions) == 0 {
		return nil, false, nil
	}

	act := cb.ActionCallback.BlockActions[0]

	minted, typed := act.Value, ""
	if act.Type == slackgo.ActionType(slackgo.METPlainTextInput) {
		minted, typed = act.ActionID, act.Value
	}

	value, err := decodeValue(minted)
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
		Value:       value,
		Text:        typed,
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
// answered about within three seconds.
//
// A press on a Stop button answers no question: it names a turn rather than a call, so it is
// routed by that name and never looked for among the open questions. Everything else carries
// the answer itself.
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

	if in.Interaction == interactionPress && in.Value.Stop != "" {
		c.stopPressed(in)

		return
	}

	c.answerQuestion(in)
}

// stopPressed asks the turn a Stop button names to park at its next boundary.
//
// The run's context is left alone. Somebody pressing Stop is asking for a conversation they
// can carry on with rather than a turn that died half done, so the run finishes the step in
// hand, parks where a later mention resumes it, and reports a suspend.
//
// Pressing twice is no worse than pressing once: the same live turn is asked for the same
// thing, and the button stays on the message until the turn's ending takes it off.
//
// A turn this worker is not running is one that ended, or one that belonged to a process
// that has restarted since. Whoever pressed is told so rather than left looking at a button
// that did nothing.
func (c *Channel) stopPressed(in *click) {
	t := c.stopping(in)
	if t == nil {
		c.log.Info("A Stop press named a turn this worker is not running",
			"turn", in.Value.Stop, "channel", in.ChannelID, "thread", in.ThreadTS, "by", in.UserID)
		c.reply(clickMention(in), stalePressRefusal)

		return
	}

	t.askStop()
	t.log.Info("Somebody asked a run to stop", "by", in.UserID)
}

// stopping is the turn one Stop press names, nil where this worker is running none.
//
// The conversation is checked against the interaction envelope's own authenticated fields,
// as an answer's is. The turn id on the button is not a capability, so a value presented
// against a thread it was not minted in reaches no run.
func (c *Channel) stopping(in *click) *turn {
	c.mu.Lock()
	defer c.mu.Unlock()

	t, held := c.stoppable[in.Value.Stop]
	if !held {
		return nil
	}

	if t.m.ChannelID != in.ChannelID || t.m.ThreadTS != in.ThreadTS {
		c.log.Warn("A Stop press named a turn this worker is running in another conversation",
			"turn", in.Value.Stop, "channel", in.ChannelID, "thread", in.ThreadTS)

		return nil
	}

	return t
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

// The lines a mention gets while a question it cannot answer is open in its thread.
//
// A confirm, a selection and a gate each have a fixed set of answers, and prose cannot be
// matched to one of them; two questions open at once cannot be told apart by prose either,
// whichever kinds they are. Each line says where the question is and what ends it, since a
// person who mentioned the bot and was refused has to know what to do instead.
const (
	openQuestionRefusal  = "I am waiting on an answer to %s in this thread. Answer or dismiss it there, and mention me again after that."
	openQuestionsRefusal = "I am waiting on answers to the questions still open in this thread. Answer or dismiss them there, and mention me again after that."
	// openQuestionLinked addresses the question message, and openQuestionPlain stands in
	// where no permalink could be built.
	openQuestionLinked = "<%s|a question I asked>"
	openQuestionPlain  = "a question I asked"
)

// answersOpenQuestion decides what a mention arriving in a thread with a question open in it
// becomes: the answer to a free-text question, or the refusal to reply with. It reports
// neither for a thread with nothing open, which is the ordinary mention.
//
// A conversation waiting on a deferred tool result reaches no boundary a user message can
// join, so an ordinary turn there would be journaled as nothing. What a mention is worth
// instead is decided by what is open: ask_human_input takes the mention's text as its
// answer, from anybody in the thread, which is who may press its buttons.
//
// It reads memory and does no I/O, so it runs where every other admission decision runs,
// before the envelope is acknowledged.
func (c *Channel) answersOpenQuestion(m *mention) (*click, string) {
	open := c.asked.openIn(m.ChannelID, m.ThreadTS)
	if len(open) == 0 {
		return nil, ""
	}

	if len(open) > 1 {
		return nil, openQuestionsRefusal
	}

	if open[0].kind == kindInput {
		return mentionAnswer(m, open[0]), ""
	}

	link, ok := c.permalink(m.ChannelID, m.ThreadTS, open[0].messageTS)
	if !ok {
		return nil, fmt.Sprintf(openQuestionRefusal, openQuestionPlain)
	}

	return nil, fmt.Sprintf(openQuestionRefusal, fmt.Sprintf(openQuestionLinked, link))
}

// mentionAnswer is the answer one mention gives to the free-text question open in its
// thread, in the shape a click takes.
//
// It is a click rather than a shape of its own because everything after this point is the
// same: the same registry decides whether a run is still waiting, the same resume carries it
// to a conversation that is not, and the same message records who answered.
//
// The kind and the call come from the question this worker is holding, never from the
// mention, and the conversation is the mention's own authenticated fields.
func mentionAnswer(m *mention, q openQuestion) *click {
	return &click{
		Interaction: interactionMention,
		TeamID:      m.TeamID,
		ChannelID:   m.ChannelID,
		ThreadTS:    m.ThreadTS,
		MessageTS:   m.TS,
		UserID:      m.UserID,
		Value: buttonValue{
			Kind:    kindInput,
			ToolUse: q.toolUseID,
			Choice:  choiceTyped,
			Asker:   q.asker,
		},
		Text: m.Text,
	}
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
//
// A dismissal answers with the null result each of those tools produces for an operator who
// was reached and gave none, carrying the reason, so the conversation takes a turn again.
// The model reasons about that result rather than routing around a tool failure.
func renderAnswer(in *click) (string, error) {
	if in.Value.Choice == choiceDismiss {
		tool, ok := toolFor(in.Value.Kind)
		if !ok {
			return "", fmt.Errorf("%q has no result shape", in.Value.Kind)
		}

		return builtin.NoAnswerResult(tool, dismissedReason)
	}

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
