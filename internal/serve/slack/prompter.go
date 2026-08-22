//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// The prompter is the whole of what this channel does with Work.Prompter, so a change to
// that contract is a compile error here rather than a run that asks nobody.
var _ toolkit.Prompter = (*prompter)(nil)

// questionKind is which of the four questions a message asks.
//
// It travels in the button's value because the map of open questions is process memory: a
// worker that restarted between the question and the click has no other way to tell an
// answer to a tool call from an approval for the confirm gate.
type questionKind string

const (
	// kindConfirm is ask_human_confirm, kindSelect is ask_human_select and kindInput is
	// ask_human_input.
	kindConfirm questionKind = "confirm"
	kindSelect  questionKind = "select"
	kindInput   questionKind = "input"
	// kindApprove is the confirm gate's three-way approval, which guards a command that
	// has not run.
	kindApprove questionKind = "approve"
)

// known reports whether this names one of the four questions. A value carrying anything
// else was not minted here, or was minted by a version of this bot that asked something
// this one does not, and the resume path has no result shape for it.
func (k questionKind) known() bool {
	switch k {
	case kindConfirm, kindSelect, kindInput, kindApprove:
		return true
	default:
		return false
	}
}

// The choices a button carries. A selection carries the option's index in decimal instead,
// there being no fixed set of them.
const (
	choiceYes    = "yes"
	choiceNo     = "no"
	choiceOnce   = "once"
	choiceAlways = "always"
	// choiceReply is the button that opens the dialog a free-text answer is typed into. It
	// answers nothing on its own.
	choiceReply = "reply"
)

// buttonValue is what one button carries back when somebody presses it. It is JSON so a
// field can be added later without every button minted before that becoming unreadable.
//
// It holds no session id, and deliberately. The session is derived from the interaction
// envelope's own authenticated team, channel and thread, which is what stops the value
// being a string somebody could present as a capability.
type buttonValue struct {
	// Kind is which question this answers, so a restarted worker knows whether the click
	// builds an answer for a tool call or an approval for the gate.
	Kind questionKind `json:"kind"`

	// ToolUse is the call the answer belongs to. The question is asked again under a fresh
	// question id on every resume, so the call is the only thing the asking end and the
	// answering end can agree the answer is about.
	ToolUse string `json:"tool_use"`

	// Choice is what this button says, in the terms the question's own kind reads.
	Choice string `json:"choice"`

	// Label is what the button was drawn with, which for a selection is the option itself.
	// A restarted worker holds no record of the options a selection offered, and the result
	// ask_human_select returns names the option rather than its position, so the option
	// travels with the choice.
	Label string `json:"label,omitempty"`

	// Asker is who the run put the question to. A restarted worker holds no record of it
	// and the question message names them, so it travels here.
	Asker string `json:"asker"`
}

// encodeValue is what a button carries, and decodeValue reads it back off a click.
func encodeValue(v buttonValue) (string, error) {
	out, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("building a button's value: %w", err)
	}

	return string(out), nil
}

func decodeValue(s string) (buttonValue, error) {
	var v buttonValue

	err := json.Unmarshal([]byte(s), &v)
	if err != nil {
		return buttonValue{}, fmt.Errorf("reading a button's value: %w", err)
	}

	if v.ToolUse == "" {
		return buttonValue{}, fmt.Errorf("a button arrived without the call it answers")
	}

	if !v.Kind.known() {
		return buttonValue{}, fmt.Errorf("a button arrived naming %q, which is not a question this bot asks", v.Kind)
	}

	return v, nil
}

// given is one person's answer to one question.
type given struct {
	// By is who pressed the button or sent the dialog, which the question message records.
	By string

	// Choice is what the button carried and Text what somebody typed into the dialog. A
	// question uses one of them.
	Choice string
	Text   string
}

// The states one question passes through. It moves once, and only under the registry's
// lock.
type questionState int

const (
	// questionOpen is a question on the thread that nobody has answered.
	questionOpen questionState = iota
	// questionAnswered is a question somebody answered, whether or not the run that asked
	// it was still waiting.
	questionAnswered
	// questionAbandoned is a question the run stopped waiting for. A click on one is
	// reported back as an answer that has to reach the conversation another way.
	questionAbandoned
)

// question is one thing a run asked its thread, from the moment it was registered until the
// turn that asked it ended.
//
// Everything above state is written once, before the question is registered, and read from
// both goroutines afterwards. state, messageTS and answer move, and every read and write of
// those is under the registry's lock.
type question struct {
	kind      questionKind
	toolUseID string
	asker     string
	turn      string

	channelID string
	threadTS  string

	// text is the message body without the buttons, so recording an answer rewrites the
	// message rather than losing what was asked.
	text string

	// buttons are what the message is posted with, and options the choices a selection was
	// offered, which is what an index answers with.
	buttons []button
	options []string

	// def pre-fills the dialog a free-text answer is typed into, for the question kind that
	// opens one.
	def string

	// woken tells the run an answer arrived. It is buffered by one and written once, under
	// the lock and only on the move out of questionOpen, so the goroutine reading envelopes
	// never blocks on it.
	woken chan struct{}

	state     questionState
	messageTS string
	answer    *given
}

// delivery says what became of a click.
//
// deliveryResume exists because a click can land in the moment a run gives up. Reporting it
// as taken would put the answer in a buffer nobody will read, and a Slack thread has no way
// to send it again: the buttons have been replaced by the answer the message records.
type delivery int

const (
	// deliveryUnknown is a click naming a call this worker holds no question for: it
	// restarted since the question was asked, or the turn that asked it has ended. The
	// answer reaches the conversation as a resume, built from the click alone.
	deliveryUnknown delivery = iota
	// deliveryTaken is a click the run that asked took, still loaded and still waiting.
	deliveryTaken
	// deliveryResume is a click that landed after the run stopped waiting. The answer is
	// not lost: it reaches the conversation as a resume rather than through the live run.
	deliveryResume
	// deliveryAnswered is a press on a question that already has an answer. The first
	// press is the answer, and a second resume would answer one call twice.
	deliveryAnswered
	// deliveryElsewhere is a click whose value names a call this worker holds a question
	// for in another conversation, or under another kind. The conversation comes from the
	// interaction envelope, so this is a value presented against a thread it was not
	// minted in.
	deliveryElsewhere
)

// questions is every question this worker is holding, keyed by the call a click names. It
// belongs to the channel rather than to a run, because the goroutine reading envelopes has
// to find a question without knowing which turn asked it.
//
// One mutex covers both directions, which is the point of it. A run giving up on a question
// and a click being delivered to that question are one transition under this lock, so a
// click landing exactly as the window closes is reported as one or the other and never as
// both.
type questions struct {
	mu   sync.Mutex
	open map[string]*question
}

func newQuestions() *questions {
	return &questions{open: map[string]*question{}}
}

// start registers a question before it is posted, so a click landing while the post is
// still returning has somewhere to be delivered.
//
// A second question about the same call replaces the first. That call's earlier question
// belongs to a turn that has ended, since a turn's questions are dropped when it reports its
// outcome, and the live one is the one a person is looking at.
func (qs *questions) start(q *question) {
	qs.mu.Lock()
	defer qs.mu.Unlock()

	qs.open[q.toolUseID] = q
}

// posted records the message a question was asked on, which is what recording the answer
// rewrites.
func (qs *questions) posted(q *question, ts string) {
	qs.mu.Lock()
	defer qs.mu.Unlock()

	q.messageTS = ts
}

// message is where a question was asked and what it said, so the answer can be written onto
// it. An empty timestamp is a question whose message never reached Slack.
func (qs *questions) message(q *question) (channelID string, ts string, text string) {
	qs.mu.Lock()
	defer qs.mu.Unlock()

	return q.channelID, q.messageTS, q.text
}

// defaultFor is what the dialog for one call arrives pre-filled with, empty where this
// worker is holding no question for it. A restarted worker opens the dialog empty rather
// than refusing to open one, since a person pressing Reply has an answer to give either way.
func (qs *questions) defaultFor(toolUseID string) string {
	qs.mu.Lock()
	defer qs.mu.Unlock()

	q, held := qs.open[toolUseID]
	if !held {
		return ""
	}

	return q.def
}

// forget drops a question this worker is no longer holding.
func (qs *questions) forget(q *question) {
	qs.mu.Lock()
	defer qs.mu.Unlock()

	held, ok := qs.open[q.toolUseID]
	if ok && held == q {
		delete(qs.open, q.toolUseID)
	}
}

// dropTurn drops every question one turn asked. It is called when that turn reports its
// outcome: the run that would have taken an answer has ended, so a click arriving afterwards
// reaches the conversation as a resume rather than a question nobody is waiting on.
func (qs *questions) dropTurn(id string) {
	qs.mu.Lock()
	defer qs.mu.Unlock()

	for key, q := range qs.open {
		if q.turn == id {
			delete(qs.open, key)
		}
	}
}

// deliver hands one click to the question it names and reports what became of it.
//
// The lookup and the state change are one operation under the lock, as giving up is, which
// is what makes the two atomic against each other.
//
// A click whose value does not describe the question this worker holds under that call, or
// that arrived from another conversation, answers nothing: the channel and the thread come
// from the interaction envelope's own authenticated fields, so a value naming a call in
// somebody else's thread is refused rather than delivered.
func (qs *questions) deliver(in *click) (*question, *given, delivery) {
	qs.mu.Lock()
	defer qs.mu.Unlock()

	q, held := qs.open[in.Value.ToolUse]
	if !held {
		return nil, nil, deliveryUnknown
	}

	if q.kind != in.Value.Kind || q.channelID != in.ChannelID || q.threadTS != in.ThreadTS {
		return nil, nil, deliveryElsewhere
	}

	g := &given{By: in.UserID, Choice: in.Value.Choice, Text: in.Text}

	switch q.state {
	case questionOpen:
		q.state = questionAnswered
		q.answer = g
		q.woken <- struct{}{}

		return q, g, deliveryTaken

	case questionAbandoned:
		q.state = questionAnswered
		q.answer = g

		return q, g, deliveryResume

	default:
		// A second press on a question already settled. The answer it holds is what the
		// message records, and this press starts nothing.
		return q, q.answer, deliveryAnswered
	}
}

// reopen puts a question back where a click found it, for an answer this channel took off
// it and then could not act on.
//
// The buttons are still on the message in that case, since recording an answer is what
// takes them off and a refused press records none, so whoever was told to press again
// reaches a question that can take it.
func (qs *questions) reopen(q *question) {
	qs.mu.Lock()
	defer qs.mu.Unlock()

	if q.state != questionAnswered {
		return
	}

	q.state = questionAbandoned
	q.answer = nil
}

// giveUp ends the run's wait on one question and answers with the click that beat it, where
// one did.
//
// The question stays registered. A click arriving between here and the turn's ending is what
// deliveryResume is for, and dropTurn is what takes it out.
func (qs *questions) giveUp(q *question) (*given, bool) {
	qs.mu.Lock()
	defer qs.mu.Unlock()

	if q.state == questionAnswered {
		return q.answer, true
	}

	q.state = questionAbandoned

	return nil, false
}

// taken is the answer a question was given, read after woken said there is one.
func (qs *questions) taken(q *question) *given {
	qs.mu.Lock()
	defer qs.mu.Unlock()

	return q.answer
}

// prompter puts one run's questions to the thread it is running in and delivers the answers
// back while that run is still loaded.
//
// The run goroutine calls one method at a time and blocks in it. An answer arrives on the
// goroutine reading envelopes, which must never block, so it is handed over through the
// buffered channel the question holds and everything that talks to Slack happens somewhere
// else.
//
// Each question is held for the channel's answer_grace. Past it Confirm, Select and Input
// report toolkit.DeferResult, which ends the run at a resumable boundary and gives the
// worker back; ApproveCommand reports toolkit.ErrPromptAborted instead, since the gate
// guards a command that has not run and a deferred call is never dispatched again.
//
// serve bounds none of this: the work sets PromptsMayBlock, so answer_grace and the run's
// own context are the only limits there are.
type prompter struct {
	t  *turn
	ch *Channel
}

func newPrompter(t *turn) *prompter {
	return &prompter{t: t, ch: t.ch}
}

// CanPrompt reports true: everybody in the thread can be asked, and any one of them may
// answer. Whether somebody is looking at it is what answer_grace measures.
func (p *prompter) CanPrompt() bool { return true }

// ApproveCommand puts a confirm-gated command to the thread as its three-way choice:
// decline, allow once, or allow for the rest of the conversation.
//
// An unanswered approval reports toolkit.ErrPromptAborted rather than a deferral. The gate
// guards a call that has not run, so what is being waited on is permission: a deferred call
// is never dispatched again, and somebody approving on Thursday would find nothing left to
// approve. The abort leaves the call unanswered, so the resume dispatches it and the gate
// asks again.
func (p *prompter) ApproveCommand(ctx context.Context, req toolkit.GateRequest) (toolkit.ConfirmChoice, error) {
	q, err := p.newQuestion(kindApprove, req.ToolUseID, gateText(req), nil)
	if err != nil {
		return toolkit.ConfirmNo, err
	}

	g, err := p.ask(ctx, q, unansweredAborts)
	if err != nil {
		return toolkit.ConfirmNo, err
	}

	switch g.Choice {
	case choiceAlways:
		return toolkit.ConfirmAlways, nil
	case choiceOnce:
		return toolkit.ConfirmOnce, nil
	default:
		return toolkit.ConfirmNo, nil
	}
}

// Confirm puts a yes/no question to the thread as a pair of buttons.
func (p *prompter) Confirm(ctx context.Context, question string) (bool, error) {
	q, err := p.newQuestion(kindConfirm, toolkit.ToolUseIDFromContext(ctx), escapeMrkdwn(question), nil)
	if err != nil {
		return false, err
	}

	g, err := p.ask(ctx, q, unansweredDefers)
	if err != nil {
		return false, err
	}

	return g.Choice == choiceYes, nil
}

// Select puts the options to the thread as one button each and answers with the index of
// the one pressed. A choice outside the options is one nobody was offered, reported rather
// than clamped.
func (p *prompter) Select(ctx context.Context, question string, options []string) (int, error) {
	q, err := p.newQuestion(kindSelect, toolkit.ToolUseIDFromContext(ctx), escapeMrkdwn(question), options)
	if err != nil {
		return -1, err
	}

	g, err := p.ask(ctx, q, unansweredDefers)
	if err != nil {
		return -1, err
	}

	idx, err := strconv.Atoi(g.Choice)
	if err != nil {
		return -1, fmt.Errorf("a selection was answered with %q, which names no option", g.Choice)
	}
	if idx < 0 || idx >= len(options) {
		return -1, fmt.Errorf("a selection chose option %d of %d", idx, len(options))
	}

	return idx, nil
}

// Input asks the thread for a free-text value. The message carries one button, which opens
// the dialog the value is typed into: a button is minted before anybody has typed, so it
// cannot carry what they will type.
//
// An empty string is a valid answer, which is why the dialog's own submission is what says
// one was given.
func (p *prompter) Input(ctx context.Context, question, def string) (string, error) {
	q, err := p.newQuestion(kindInput, toolkit.ToolUseIDFromContext(ctx), escapeMrkdwn(question), nil)
	if err != nil {
		return "", err
	}

	q.def = def

	g, err := p.ask(ctx, q, unansweredDefers)
	if err != nil {
		return "", err
	}

	return g.Text, nil
}

// unanswered says what a question nobody answered in time reports, which differs by who
// asked it. The two are named here rather than passed as errors so each call site reads as
// the decision it is making.
type unanswered int

const (
	// unansweredDefers marks the call deferred, so the answer somebody gives later is
	// supplied to it. It is what a tool whose result is the answer needs.
	unansweredDefers unanswered = iota
	// unansweredAborts leaves the call unanswered and ends the run, so the resume
	// dispatches it and the gate asks again. It is what the confirm gate needs.
	unansweredAborts
)

// ask posts one question, holds it for the grace window, and answers with what somebody
// pressed.
//
// The question is registered before it is posted, so a click landing while the post is
// still returning has a question to be delivered to rather than arriving at nothing.
//
// Giving up and delivering are one transition, made under the registry's own lock. A click
// that lands as the window closes either reaches this run or is reported to the click path
// as an answer that has to become a resume, and never lands in a buffer nobody reads.
func (p *prompter) ask(ctx context.Context, q *question, onSilence unanswered) (*given, error) {
	p.ch.asked.start(q)

	// The turn narrates that it is waiting rather than what it was doing when it asked, and
	// goes back to that hint however the question ends.
	p.t.status.asking(true)
	defer p.t.status.asking(false)

	err := p.post(ctx, q)
	if err != nil {
		p.ch.asked.forget(q)

		return nil, err
	}

	p.t.log.Info("Asked the thread a question", "kind", q.kind, "tool_use", q.toolUseID, "grace", p.ch.grace)

	select {
	case <-q.woken:
		return p.answered(q), nil

	case <-p.ch.clock.after(p.ch.grace):
	case <-ctx.Done():
	}

	_, beat := p.ch.asked.giveUp(q)
	if beat {
		// The click and the window arrived together and the click won, which is the whole
		// reason those two are one transition.
		return p.answered(q), nil
	}

	p.t.log.Info("Nobody answered a question inside the grace window", "kind", q.kind, "tool_use", q.toolUseID, "aborts", onSilence == unansweredAborts, "error", ctx.Err())

	if onSilence == unansweredAborts {
		return nil, fmt.Errorf("%w: nobody in the thread answered inside the grace window", toolkit.ErrPromptAborted)
	}

	return nil, toolkit.DeferResult("waiting on somebody in the thread to answer", q.toolUseID)
}

// answered records the answer on the question message and hands it to the run.
//
// The message is rewritten on a goroutine Close waits for rather than on the run's, so a
// person waiting on the answer is not held behind the workspace's allowance and the run is
// not held behind Slack.
func (p *prompter) answered(q *question) *given {
	g := p.ch.asked.taken(q)

	p.t.log.Info("A question was answered", "kind", q.kind, "tool_use", q.toolUseID, "by", g.By)

	p.ch.asked.forget(q)
	p.ch.speak(func() { p.ch.recordAnswer(q, g, "") })

	return g
}

// post puts the question in the thread and records the message it landed on.
//
// It spends a token from the channel's one bucket, which every call this channel makes to
// Slack answers to: a question is a Tier 3 call like a status edit and an answer.
func (p *prompter) post(ctx context.Context, q *question) error {
	err := p.ch.limit.take(ctx)
	if err != nil {
		return fmt.Errorf("waiting for the allowance to ask a question: %w", err)
	}

	ts, err := p.ch.api.postBlocks(ctx, q.channelID, q.threadTS, blockMessage{Text: q.text, Buttons: q.buttons})
	if err != nil {
		return fmt.Errorf("asking a question in %s: %w", q.channelID, err)
	}

	p.ch.asked.posted(q, ts)

	return nil
}

// newQuestion builds one question and the buttons it is asked with.
//
// A call this question cannot name is refused rather than asked. The button carries the
// call, the click is routed by it, and the resume answers it, so a question with none would
// put buttons in a thread that nothing could ever be delivered from.
func (p *prompter) newQuestion(kind questionKind, toolUseID, body string, options []string) (*question, error) {
	if toolUseID == "" {
		return nil, fmt.Errorf("a question cannot be put to a thread outside a tool call: nothing anybody pressed could be delivered back to it")
	}

	q := &question{
		kind:      kind,
		toolUseID: toolUseID,
		asker:     p.t.m.UserID,
		turn:      p.t.id,
		channelID: p.t.m.ChannelID,
		threadTS:  p.t.m.ThreadTS,
		options:   options,
		woken:     make(chan struct{}, 1),
	}

	q.text = clipped(body, maxQuestionText) + "\n\n" + typedRepliesNote

	buttons, err := q.mint()
	if err != nil {
		return nil, err
	}
	q.buttons = buttons

	return q, nil
}

// typedRepliesNote is on every question, because a person's first instinct is to type the
// answer under it. Only app_mention is subscribed, so a bare reply in the thread reaches
// this worker not at all.
const typedRepliesNote = "_Use the buttons, or mention me with your answer. I do not see plain replies in this thread._"

// The labels the buttons carry. They are plain text rather than mrkdwn, which is what Slack
// draws a button with, so nothing here is escaped.
const (
	labelYes     = "Yes"
	labelNo      = "No"
	labelOnce    = "Allow once"
	labelAlways  = "Allow for this conversation"
	labelDecline = "Decline"
	labelReply   = "Reply"
)

// What a question says once it has been answered. Anybody in the thread may answer, so who
// did is the one thing the message has to say that nobody could work out from it.
const (
	answeredLine = "Answered by <@%s>: %s"
	// lateAnswerLine ends a message whose answer arrived after the run had stopped waiting,
	// so nobody is left looking at a button they pressed with no sign it registered.
	lateAnswerLine = "_I had already stopped waiting for this one, so I am carrying on from your answer._"
	// secondPressLine ends a message somebody pressed again once it already had an answer.
	// One call takes one answer, and the first is the one the conversation ran on.
	secondPressLine = "_This one already has an answer, so I have left it where it is._"
)

// mint builds the buttons one question is asked with. Each carries the same value shape, so
// the click path reads one thing however the question was put.
func (q *question) mint() ([]button, error) {
	switch q.kind {
	case kindConfirm:
		return q.buttonsFor([]buttonSpec{
			{choice: choiceYes, label: labelYes, style: buttonPrimary},
			{choice: choiceNo, label: labelNo},
		})

	case kindApprove:
		return q.buttonsFor([]buttonSpec{
			{choice: choiceOnce, label: labelOnce, style: buttonPrimary},
			{choice: choiceAlways, label: labelAlways},
			{choice: choiceNo, label: labelDecline, style: buttonDanger},
		})

	case kindInput:
		return q.buttonsFor([]buttonSpec{
			{choice: choiceReply, label: labelReply, style: buttonPrimary},
		})

	case kindSelect:
		specs := make([]buttonSpec, 0, len(q.options))
		for i, opt := range q.options {
			specs = append(specs, buttonSpec{choice: strconv.Itoa(i), label: opt})
		}

		return q.buttonsFor(specs)

	default:
		return nil, fmt.Errorf("no buttons are defined for a %q question", q.kind)
	}
}

// buttonSpec is one button before its value has been built.
type buttonSpec struct {
	choice string
	label  string
	style  string
}

func (q *question) buttonsFor(specs []buttonSpec) ([]button, error) {
	out := make([]button, 0, len(specs))

	for _, spec := range specs {
		// The label travels in the value as well as on the button, so the answer a
		// selection resumes with names the option rather than its position even where this
		// worker restarted between the question and the press.
		label := clipped(spec.label, maxButtonLabel)

		value, err := encodeValue(buttonValue{
			Kind:    q.kind,
			ToolUse: q.toolUseID,
			Choice:  spec.choice,
			Label:   label,
			Asker:   q.asker,
		})
		if err != nil {
			return nil, err
		}

		out = append(out, button{
			// The choice is what makes each unique within the message, which is what Slack
			// requires of an action id.
			ActionID: "answer_" + spec.choice,
			Label:    label,
			Value:    value,
			Style:    spec.style,
		})
	}

	return out, nil
}

// recordAnswer rewrites a question message with the answer it was given, which takes the
// buttons off it: the question is settled and a second press would change nothing.
//
// note is one line about what became of that answer, so nobody is left looking at a button
// they pressed with no sign it registered. It is empty for an answer the run that asked
// took while it was still waiting.
func (c *Channel) recordAnswer(q *question, g *given, note string) {
	channelID, ts, text := c.asked.message(q)
	if ts == "" {
		// A question whose message never reached Slack. There is nothing to rewrite and the
		// post itself was already reported.
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultReplyDeadline)
	defer cancel()

	err := c.limit.take(ctx)
	if err != nil {
		c.log.Warn("Waiting for the allowance to record an answer failed", "channel", channelID, "error", err)

		return
	}

	body := text + "\n\n" + fmt.Sprintf(answeredLine, g.By, q.reads(g))
	if note != "" {
		body += "\n" + note
	}

	err = c.api.updateBlocks(ctx, channelID, ts, blockMessage{Text: body})
	if err != nil {
		c.log.Warn("Recording an answer on its question failed", "channel", channelID, "message", ts, "error", err)
	}
}

// pressNote says on a question message what became of a press this channel did not act on.
//
// The buttons go back on with it. Nothing was recorded as the answer, so the press whoever
// clicked was asked to make again has a button to be made on.
func (c *Channel) pressNote(q *question, line string) {
	channelID, ts, text := c.asked.message(q)
	if ts == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultReplyDeadline)
	defer cancel()

	err := c.limit.take(ctx)
	if err != nil {
		c.log.Warn("Waiting for the allowance to answer a press failed", "channel", channelID, "error", err)

		return
	}

	err = c.api.updateBlocks(ctx, channelID, ts, blockMessage{Text: text + "\n\n" + line, Buttons: q.buttons})
	if err != nil {
		c.log.Warn("Answering a press on its question failed", "channel", channelID, "message", ts, "error", err)
	}
}

// reads is what an answer says when it is written back onto the question, in the terms the
// question was put in rather than the terms the button carried.
func (q *question) reads(g *given) string {
	switch q.kind {
	case kindConfirm:
		if g.Choice == choiceYes {
			return labelYes
		}

		return labelNo

	case kindApprove:
		switch g.Choice {
		case choiceOnce:
			return labelOnce
		case choiceAlways:
			return labelAlways
		default:
			return labelDecline
		}

	case kindSelect:
		idx, err := strconv.Atoi(g.Choice)
		if err != nil || idx < 0 || idx >= len(q.options) {
			return escapeMrkdwn(g.Choice)
		}

		return escapeMrkdwn(clipped(q.options[idx], maxAnswerText))

	default:
		if g.Text == "" {
			return "_nothing_"
		}

		return escapeMrkdwn(clipped(g.Text, maxAnswerText))
	}
}

// gateText is what a confirm-gated command reads as in a thread: what is being approved,
// what gated it, and the command line itself.
//
// Every part of it comes from the model, so every part is escaped. GateRequest.Display is
// sanitized by the caller for terminal escapes and says so; markup is this prompter's to
// deal with, its rendering layer being Slack's.
func gateText(req toolkit.GateRequest) string {
	return fmt.Sprintf("*Approval needed for `%s`*, gated by `%s`.\n```%s```",
		escapeMrkdwn(req.Command), escapeMrkdwn(req.Tag), escapeMrkdwn(req.Display))
}

// The lengths the text of one question is held to. maxQuestionText leaves room under
// Slack's own section cap for the line the answer adds and for the note about typed
// replies, and maxAnswerText caps what somebody typed where it is written back.
const (
	maxQuestionText = maxSectionText - 400
	maxAnswerText   = 200
)

// clipMarker ends a string that had to be cut.
const clipMarker = "..."

// clipped cuts s to at most n bytes, on a rune boundary, marking it where it had to cut. It
// is bytes rather than characters for the reason the answer's own cap is: Slack states these
// limits in characters without saying which count, and a byte length is at or above every
// reading of that.
func clipped(s string, n int) string {
	if len(s) <= n {
		return s
	}

	kept := s[:max(0, n-len(clipMarker))]
	for len(kept) > 0 {
		r, size := utf8.DecodeLastRuneInString(kept)
		if r != utf8.RuneError || size > 1 {
			break
		}

		kept = kept[:len(kept)-1]
	}

	return kept + clipMarker
}

// escapeMrkdwn takes the three characters Slack reads as markup out of text somebody else
// wrote, so a question carrying a tag or a comparison arrives as the question rather than as
// a link, a mention or a broadcast.
//
// The three are all Slack asks for, and the order matters: the ampersand goes first or the
// escapes of the other two are escaped again.
func escapeMrkdwn(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")

	return strings.ReplaceAll(s, ">", "&gt;")
}
