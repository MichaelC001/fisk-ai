//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/toolkit/builtin"
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
	// choiceTyped is what an answer somebody wrote carries, whether they typed it into the
	// field on the question message or mentioned the bot with it. The words are the click's
	// own text; this says which of the two shapes of answer they are.
	choiceTyped = "typed"
	// choiceDismiss ends a question nobody wants to answer. The call is answered with the
	// null result its own tool produces for an operator who gave no answer, so the
	// conversation takes a turn again instead of waiting on the thread.
	choiceDismiss = "dismiss"
)

// dismissedReason is what the model is told about a call somebody dismissed. It reaches the
// model inside the tool's own null result and nowhere else.
const dismissedReason = "the question was dismissed in the thread"

// errDismissed is what a dismissed question reports to the tool that asked it, for a run
// still waiting when the button was pressed. The three question tools turn an error that is
// neither an abort nor a deferral into their own null result, which is a result the model
// reasons about rather than a tool failure.
var errDismissed = errors.New(dismissedReason)

// toolFor is the built-in one question kind belongs to, for rendering the result a
// dismissal supplies to the call. The confirm gate is not one of them: its call was never
// dispatched, so there is nothing to supply a result to.
func toolFor(kind questionKind) (string, bool) {
	switch kind {
	case kindConfirm:
		return builtin.AskHumanConfirmName, true
	case kindSelect:
		return builtin.AskHumanSelectName, true
	case kindInput:
		return builtin.AskHumanInputName, true
	default:
		return "", false
	}
}

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

	// Label is what the choice says in words, which for a selection is the option itself
	// rather than the number its button is drawn with. A restarted worker holds no record
	// of the options a selection offered, and the result ask_human_select returns names the
	// option rather than its position, so the option travels with the choice.
	Label string `json:"label,omitempty"`

	// Asker is who the run put the question to. A restarted worker holds no record of it
	// and the question message names them, so it travels here.
	Asker string `json:"asker"`

	// Stop names the turn a Stop button asks to park, and is empty on every button that
	// answers a question. A press carrying it names no call and no question kind, so the
	// click path routes it by this field alone and looks for nothing in the registry of
	// open questions.
	//
	// The turn id is not a capability. It is checked against the team, channel and thread
	// the interaction envelope authenticated before it reaches a run, the way an answer's
	// conversation is.
	Stop string `json:"stop,omitempty"`
}

// encodeValue is what a button carries, and decodeValue reads it back off a click.
//
// A value naming a turn to stop is complete on that name: it answers no call, so neither
// the kind nor the tool_use a question's value has to carry is required of it.
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

	if v.Stop != "" {
		return v, nil
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
	// By is who pressed the button or typed the answer, which the question message records.
	By string

	// Choice is what the control carried and Text what somebody typed. A question uses one
	// of them.
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

	// text is the message body without the controls, so recording an answer rewrites the
	// message rather than losing what was asked. A selection's options are part of it: the
	// buttons carry the index and the words are read off the message.
	text string

	// buttons are what the message is posted with, and options the choices a selection was
	// offered, which is what an index answers with.
	buttons []button
	options []string

	// def pre-fills the field a free-text answer is typed into, for the question kind that
	// carries one.
	def string

	// input is the field a free-text answer is typed into, nil for the three kinds that
	// take a button. It is held here as well as posted so a refused press can put the
	// message back the way whoever pressed found it.
	input *textInput

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
	// restarted since the question was asked, or the question was evicted by the bound on
	// how many are held. The answer reaches the conversation as a resume, built from the
	// click alone.
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

	// order is the arrival sequence the oldest question is evicted from, and limit how
	// many are held. A question outlives the turn that asked it, so without a bound a
	// worker would accumulate one entry for every question nobody ever answered.
	//
	// Evicting one costs nothing a person sees: the buttons are still on its message, and
	// a press on a question this worker no longer holds is built from the value alone,
	// which is the same path a press after a restart takes.
	order []string
	limit int
}

// defaultQuestionLimit is how many questions one worker holds. A bot asked a question an
// hour would hold ten days of them before the oldest was evicted.
const defaultQuestionLimit = 256

func newQuestions(limit int) *questions {
	if limit <= 0 {
		limit = defaultQuestionLimit
	}

	return &questions{open: map[string]*question{}, limit: limit}
}

// start registers a question before it is posted, so a click landing while the post is
// still returning has somewhere to be delivered.
//
// A second question about the same call replaces the first. The live one is the one a
// person is looking at, and the earlier one belongs to a turn that has ended.
func (qs *questions) start(q *question) {
	qs.mu.Lock()
	defer qs.mu.Unlock()

	_, replacing := qs.open[q.toolUseID]
	qs.open[q.toolUseID] = q

	if replacing {
		return
	}

	qs.order = append(qs.order, q.toolUseID)

	if len(qs.order) > qs.limit {
		delete(qs.open, qs.order[0])
		qs.order = qs.order[1:]
	}
}

// openQuestion is one question still waiting in a thread, read out of the registry so a
// caller decides on it without holding the lock or reading fields that move under it.
type openQuestion struct {
	kind      questionKind
	toolUseID string
	asker     string

	// messageTS names the message the question was asked on, which is what a refusal
	// points at. It is empty for a question whose message never reached Slack.
	messageTS string
}

// openIn is every question still waiting in one conversation, oldest message first.
//
// A question is waiting from the moment it is registered until somebody answers it,
// whether or not the run that asked it is still loaded: a deferred call is answered by the
// thread or by nothing, so the question stands until it is.
func (qs *questions) openIn(channelID, threadTS string) []openQuestion {
	qs.mu.Lock()
	defer qs.mu.Unlock()

	var out []openQuestion

	for _, q := range qs.open {
		if q.channelID != channelID || q.threadTS != threadTS {
			continue
		}
		if q.state == questionAnswered {
			continue
		}

		out = append(out, openQuestion{
			kind:      q.kind,
			toolUseID: q.toolUseID,
			asker:     q.asker,
			messageTS: q.messageTS,
		})
	}

	// The map is walked in whatever order Go gives, so the order a thread is asked in is
	// restored here: the message timestamps are Slack's own and sort chronologically, and
	// the call breaks a tie between two whose messages never landed.
	slices.SortFunc(out, func(a, b openQuestion) int {
		if a.messageTS != b.messageTS {
			return strings.Compare(a.messageTS, b.messageTS)
		}

		return strings.Compare(a.toolUseID, b.toolUseID)
	})

	return out
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

// forget drops a question this worker is no longer holding.
func (qs *questions) forget(q *question) {
	qs.mu.Lock()
	defer qs.mu.Unlock()

	held, ok := qs.open[q.toolUseID]
	if ok && held == q {
		delete(qs.open, q.toolUseID)
		qs.order = slices.DeleteFunc(qs.order, func(key string) bool { return key == q.toolUseID })
	}
}

// abandonTurn ends one turn's wait on every question it asked and leaves those questions
// standing. It is called when that turn reports its outcome.
//
// The entry is what the thread's next mention is decided against: a call the conversation
// deferred on is still waiting on an answer, whether or not a run is loaded, so a question
// outlives the turn that asked it and is dropped when somebody answers it or when the bound
// evicts it. The run that would have taken an answer has ended, so a click arriving now
// reaches the conversation as a resume rather than a run nobody is waiting on.
func (qs *questions) abandonTurn(id string) {
	qs.mu.Lock()
	defer qs.mu.Unlock()

	for _, q := range qs.open {
		if q.turn == id && q.state == questionOpen {
			q.state = questionAbandoned
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
// The question stays registered, and stays registered past the turn's ending: a click
// arriving at either point is what deliveryResume is for.
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

	// mu guards the held approval. It is written when the turn is built, on the goroutine
	// reading envelopes, and read on the run's own goroutine.
	mu sync.Mutex

	// heldCall and heldChoice are the approval a press carried, for a thread that was asked
	// before its run gave up and answered afterwards. The resume dispatches the call the
	// gate guards and the gate asks about it again, and that question is answered from here
	// rather than put back to a thread that has already answered it.
	//
	// It is spent on the first question about that call. A later call of the same tool
	// carries its own arguments, so the thread decides on that one too.
	//
	// It lives on this turn and nowhere else. Nothing writes it to the journal, so a resume
	// that ends before the gate asks loses it and the next press is asked about again.
	heldCall   string
	heldChoice toolkit.ConfirmChoice
}

func newPrompter(t *turn) *prompter {
	return &prompter{t: t, ch: t.ch}
}

// hold takes the approval a press carried, to answer the question the resumed run puts
// about that call.
func (p *prompter) hold(toolUseID string, choice toolkit.ConfirmChoice) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.heldCall = toolUseID
	p.heldChoice = choice
}

// heldFor reports the approval this turn is holding for the named call, and spends it.
func (p *prompter) heldFor(toolUseID string) (toolkit.ConfirmChoice, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if toolUseID == "" || p.heldCall != toolUseID {
		return toolkit.ConfirmNo, false
	}

	p.heldCall = ""

	return p.heldChoice, true
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
//
// That second question is answered from the press that produced the resume, where this turn
// is holding one for the call. Somebody who pressed Allow gets the command they approved,
// not the same question in the thread again.
func (p *prompter) ApproveCommand(ctx context.Context, req toolkit.GateRequest) (toolkit.ConfirmChoice, error) {
	choice, held := p.heldFor(req.ToolUseID)
	if held {
		p.t.log.Info("Answering an approval from the press that resumed this thread", "tool_use", req.ToolUseID, "command", req.Command)

		return choice, nil
	}

	q, err := p.newQuestion(kindApprove, req.ToolUseID, gateText(req), nil, "")
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

// Confirm puts a yes/no question to the thread as a pair of buttons, with Dismiss beside
// them.
//
// A dismissal is not a no. It reports errDismissed, which ask_human_confirm turns into its
// own null result carrying the reason, so the model reads that nobody decided rather than
// that somebody decided against.
func (p *prompter) Confirm(ctx context.Context, question string) (bool, error) {
	q, err := p.newQuestion(kindConfirm, toolkit.ToolUseIDFromContext(ctx), escapeMrkdwn(question), nil, "")
	if err != nil {
		return false, err
	}

	g, err := p.ask(ctx, q, unansweredDefers)
	if err != nil {
		return false, err
	}

	if g.Choice == choiceDismiss {
		return false, errDismissed
	}

	return g.Choice == choiceYes, nil
}

// Select puts the options to the thread as a numbered list in the message, with one button
// per number under it and Dismiss after them, and answers with the index of the one pressed.
//
// The words are in the message rather than on the buttons because a button label is cut at
// 75 characters and a row of long ones is unreadable, where a section block holds 3000. A
// choice outside the options is one nobody was offered, reported rather than clamped.
func (p *prompter) Select(ctx context.Context, question string, options []string) (int, error) {
	q, err := p.newQuestion(kindSelect, toolkit.ToolUseIDFromContext(ctx), escapeMrkdwn(question), options, "")
	if err != nil {
		return -1, err
	}

	g, err := p.ask(ctx, q, unansweredDefers)
	if err != nil {
		return -1, err
	}

	if g.Choice == choiceDismiss {
		return -1, errDismissed
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

// Input asks the thread for a free-text value. The message carries the field it is typed
// into and Dismiss beside it: a button is minted before anybody has typed, so it cannot
// carry what they will type, and the field reports what somebody wrote when they press
// enter in it.
//
// A mention in the thread answers this question too, its text being the value, which is
// what a person's first instinct produces.
//
// An empty string is a valid answer, which is why pressing enter is what says one was given
// rather than the value itself.
func (p *prompter) Input(ctx context.Context, question, def string) (string, error) {
	q, err := p.newQuestion(kindInput, toolkit.ToolUseIDFromContext(ctx), escapeMrkdwn(question), nil, def)
	if err != nil {
		return "", err
	}

	g, err := p.ask(ctx, q, unansweredDefers)
	if err != nil {
		return "", err
	}

	if g.Choice == choiceDismiss {
		return "", errDismissed
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

	ts, err := p.ch.api.postBlocks(ctx, q.channelID, q.threadTS, blockMessage{Text: q.text, Buttons: q.buttons, Input: q.input})
	if err != nil {
		return fmt.Errorf("asking a question in %s: %w", q.channelID, err)
	}

	p.ch.asked.posted(q, ts)

	return nil
}

// newQuestion builds one question, the words it is asked in and the controls it is answered
// on. def is what a free-text question's field arrives pre-filled with and is empty for the
// other three.
//
// A call this question cannot name is refused rather than asked. The control carries the
// call, the click is routed by it, and the resume answers it, so a question with none would
// put buttons in a thread that nothing could ever be delivered from.
func (p *prompter) newQuestion(kind questionKind, toolUseID, body string, options []string, def string) (*question, error) {
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
		def:       def,
		woken:     make(chan struct{}, 1),
	}

	q.text = questionText(q.asker, body, options)

	buttons, field, err := q.mint()
	if err != nil {
		return nil, err
	}

	q.buttons = buttons
	q.input = field

	return q, nil
}

// questionText is what one question message says: who is being asked, what is being asked,
// the options where there are any, and the note about typed replies.
//
// The asker is named with Slack's own mention markup, so the thread shows their name, links
// them and notifies them. Their id comes from the event Slack delivered and carries nothing
// that needs escaping.
//
// The words are cut to fit under Slack's section cap. A selection's options take room of
// their own beside them, so the question is held to what is left rather than to the whole.
// body arrives escaped, so the cut it takes is the one that leaves no entity half written.
func questionText(asker, body string, options []string) string {
	room := maxQuestionText
	if len(options) > 0 {
		room -= maxOptionsText
	}

	text := clippedMrkdwn(body, room)
	if asker != "" {
		text = fmt.Sprintf(askedOf, asker) + " " + text
	}

	if len(options) > 0 {
		text += "\n\n" + optionList(options, maxOptionsText)
	}

	return text + "\n\n" + typedRepliesNote
}

// askedOf names the person a question is put to, and Slack renders it as their name.
const askedOf = "<@%s>"

// optionList renders a selection's options as the numbered list its buttons answer, one per
// line, within budget bytes.
//
// Every option is on the list, since a button carries the number and the words are the only
// place the person reads what they are choosing between. The budget is shared out evenly, so
// twenty-five options each get a share rather than the first few taking all of it, and an
// option longer than its share is cut where it is.
//
// The escape comes before the cut, as it does for the question's own words: an option of
// ampersands is five times its own length once escaped, and a list cut afterwards would take
// the message past what Slack accepts.
func optionList(options []string, budget int) string {
	per := max(budget/len(options)-optionOverhead, minOptionText)
	per = min(per, maxOptionText)

	lines := make([]string, 0, len(options))
	for i, opt := range options {
		lines = append(lines, fmt.Sprintf("*%d.* %s", i+1, clippedMrkdwn(escapeMrkdwn(opt), per)))
	}

	return strings.Join(lines, "\n")
}

// typedRepliesNote is on every question, because a person's first instinct is to type the
// answer under it. Only app_mention is subscribed, so a bare reply in the thread reaches
// this worker not at all.
//
// A mention answers a free-text question and is refused while any other kind is open, so
// what a mention is worth differs by kind. The note says the same thing on all four rather
// than four things: mentioning the bot is what gets a person heard either way, and the
// refusal that comes back says what to do instead.
const typedRepliesNote = "_Use the buttons, or mention me with your answer. I do not see plain replies in this thread._"

// The labels the buttons carry. They are plain text rather than mrkdwn, which is what Slack
// draws a button with, so nothing here is escaped.
const (
	labelYes     = "Yes"
	labelNo      = "No"
	labelOnce    = "Allow once"
	labelAlways  = "Allow for this conversation"
	labelDecline = "Decline"
	labelDismiss = "Dismiss"
)

// What the field a free-text answer is typed into is called, and what an empty one shows.
const (
	labelAnswer       = "Your answer"
	placeholderAnswer = "Type your answer and press enter"
)

// What a question says once it has been answered. Anybody in the thread may answer, so who
// did is the one thing the message has to say that nobody could work out from it.
const (
	answeredLine = "Answered by <@%s>: %s"
	// answerDismissed is what a dismissal reads as, whichever question was dismissed.
	answerDismissed = "Dismissed"
	// lateAnswerLine ends a message whose answer arrived after the run had stopped waiting,
	// so nobody is left looking at a button they pressed with no sign it registered.
	lateAnswerLine = "_I had already stopped waiting for this one, so I am carrying on from your answer._"
	// secondPressLine ends a message somebody pressed again once it already had an answer.
	// One call takes one answer, and the first is the one the conversation ran on.
	secondPressLine = "_This one already has an answer, so I have left it where it is._"
)

// mint builds the controls one question is answered on: the buttons, and for a free-text
// question the field the answer is typed into. Each carries the same value shape, so the
// click path reads one thing however the question was put.
//
// A selection's buttons are numbered rather than labeled with the options. A label is cut at
// 75 characters and a row of long ones reads badly, so the options are on the message and
// each button carries the number of the line it answers. The option itself travels in the
// value, which is what a worker holding no question answers with.
//
// Every question can be ended from its own message. Three of them carry Dismiss, which
// answers the call with the null result its own tool produces for an operator who gave no
// answer. The confirm gate's Decline is its dismissal: the gated command does not run, which
// is the whole of what declining to answer a gate can mean, and a second button beside it
// saying the same thing in vaguer words would be a choice nobody has to make.
func (q *question) mint() ([]button, *textInput, error) {
	switch q.kind {
	case kindConfirm:
		buttons, err := q.buttonsFor([]buttonSpec{
			{choice: choiceYes, label: labelYes, style: buttonPrimary},
			{choice: choiceNo, label: labelNo},
			{choice: choiceDismiss, label: labelDismiss},
		})

		return buttons, nil, err

	case kindApprove:
		buttons, err := q.buttonsFor([]buttonSpec{
			{choice: choiceOnce, label: labelOnce, style: buttonPrimary},
			{choice: choiceAlways, label: labelAlways},
			{choice: choiceNo, label: labelDecline, style: buttonDanger},
		})

		return buttons, nil, err

	case kindInput:
		buttons, err := q.buttonsFor([]buttonSpec{{choice: choiceDismiss, label: labelDismiss}})
		if err != nil {
			return nil, nil, err
		}

		field, err := q.field()
		if err != nil {
			return nil, nil, err
		}

		return buttons, field, nil

	case kindSelect:
		specs := make([]buttonSpec, 0, len(q.options)+1)
		for i, opt := range q.options {
			specs = append(specs, buttonSpec{choice: strconv.Itoa(i), label: strconv.Itoa(i + 1), answer: opt})
		}

		buttons, err := q.buttonsFor(append(specs, buttonSpec{choice: choiceDismiss, label: labelDismiss}))

		return buttons, nil, err

	default:
		return nil, nil, fmt.Errorf("no controls are defined for a %q question", q.kind)
	}
}

// field is the input a free-text answer is typed into.
//
// The action id carries what a button carries in its value, a plain_text_input element
// taking none of its own, and Slack returns it on the payload the enter key produces. A
// value too long for an action id is refused rather than posted: Slack would refuse the
// message itself, leaving a run waiting on a question nobody was ever shown.
func (q *question) field() (*textInput, error) {
	value, err := encodeValue(buttonValue{
		Kind:    q.kind,
		ToolUse: q.toolUseID,
		Choice:  choiceTyped,
		Asker:   q.asker,
	})
	if err != nil {
		return nil, err
	}

	if len(value) > maxActionID {
		return nil, fmt.Errorf("a free-text question about call %q cannot be asked: what the field has to carry is %d characters, and Slack takes %d", q.toolUseID, len(value), maxActionID)
	}

	return &textInput{
		ActionID:    value,
		Label:       labelAnswer,
		Placeholder: placeholderAnswer,
		Initial:     q.def,
	}, nil
}

// buttonSpec is one button before its value has been built. answer is what the value records
// the choice as, for the button whose label is not the answer in words; it is empty where
// the label says it.
type buttonSpec struct {
	choice string
	label  string
	answer string
	style  string
}

func (q *question) buttonsFor(specs []buttonSpec) ([]button, error) {
	out := make([]button, 0, len(specs))

	for _, spec := range specs {
		label := clipped(spec.label, maxButtonLabel)

		// The answer travels in the value as well as on the message, so a selection
		// resumed after a restart names the option rather than the number of its button.
		answer := spec.answer
		if answer == "" {
			answer = label
		}

		value, err := encodeValue(buttonValue{
			Kind:    q.kind,
			ToolUse: q.toolUseID,
			Choice:  spec.choice,
			Label:   clipped(answer, maxOptionText),
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
// controls off it: the question is settled and a second press would change nothing.
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
// The controls go back on with it. Nothing was recorded as the answer, so the press whoever
// clicked was asked to make again has a button to be made on, and the field a free-text
// answer is typed into is there to type it into again.
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

	err = c.api.updateBlocks(ctx, channelID, ts, blockMessage{Text: text + "\n\n" + line, Buttons: q.buttons, Input: q.input})
	if err != nil {
		c.log.Warn("Answering a press on its question failed", "channel", channelID, "message", ts, "error", err)
	}
}

// reads is what an answer says when it is written back onto the question, in the terms the
// question was put in rather than the terms the button carried.
//
// A selection's option, a choice naming none of them and a typed answer are escaped before
// they are cut, the way the question's own words are: escaping grows the string, so an answer
// cut first would put the rewritten message past the room maxQuestionText left for it and the
// update recording it would be refused.
//
// The choice and the text come off the click rather than out of anything this worker minted,
// so both are held to maxAnswerText however long they arrive.
func (q *question) reads(g *given) string {
	if g.Choice == choiceDismiss {
		return answerDismissed
	}

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
			return clippedMrkdwn(escapeMrkdwn(g.Choice), maxAnswerText)
		}

		return clippedMrkdwn(escapeMrkdwn(q.options[idx]), maxAnswerText)

	default:
		if g.Text == "" {
			return "_nothing_"
		}

		return clippedMrkdwn(escapeMrkdwn(g.Text), maxAnswerText)
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
//
// maxOptionsText is what a selection's list takes of maxQuestionText, and the other two
// limit one option: maxOptionText where there are few enough for the whole share to be more
// than an option needs, minOptionText where there are enough that an even share would be
// shorter than a word.
const (
	maxQuestionText = maxSectionText - 400
	maxAnswerText   = 200
	maxOptionsText  = 1200
	maxOptionText   = 200
	minOptionText   = 24
	// optionOverhead is the number, its markup and the newline each option's line spends
	// beyond the option itself.
	optionOverhead = 8
)

// clipMarker ends a string that had to be cut.
const clipMarker = "..."

// clipped cuts s to at most n bytes, on a rune boundary, marking it where it had to cut. It
// is bytes rather than characters for the reason the answer's own cap is: Slack states these
// limits in characters without saying which count, and a byte length is at or above every
// reading of that.
//
// Text that has been through escapeMrkdwn is cut by clippedMrkdwn instead.
func clipped(s string, n int) string {
	if len(s) <= n {
		return s
	}

	return wholeRunes(s[:max(0, n-len(clipMarker))]) + clipMarker
}

// clippedMrkdwn cuts escaped text to at most n bytes, on a rune boundary and never inside
// one of the entities escapeMrkdwn writes.
//
// Escaping comes before the cut wherever a size limit is counted, because escaping grows the
// string: an answer of ampersands is five times its length once escaped, and a string cut
// first takes the message past the room its caller reserved. The cut then has the entities
// to land in, and a message carrying &am where the answer had an ampersand shows it that
// way.
func clippedMrkdwn(s string, n int) string {
	if len(s) <= n {
		return s
	}

	return wholeEntities(wholeRunes(s[:max(0, n-len(clipMarker))])) + clipMarker
}

// wholeRunes drops the bytes of a character a cut landed in the middle of.
func wholeRunes(s string) string {
	for len(s) > 0 {
		r, size := utf8.DecodeLastRuneInString(s)
		if r != utf8.RuneError || size > 1 {
			break
		}

		s = s[:len(s)-1]
	}

	return s
}

// mrkdwnEntities is what escapeMrkdwn writes, which is where the ampersands in escaped text
// come from.
var mrkdwnEntities = []string{"&amp;", "&lt;", "&gt;"}

// wholeEntities drops the start of an entity a cut landed inside. The ampersand it looks
// back to is the last one in escaped text, so an entity ending before it is complete.
func wholeEntities(s string) string {
	i := strings.LastIndexByte(s, '&')
	if i < 0 {
		return s
	}

	tail := s[i:]

	for _, entity := range mrkdwnEntities {
		if tail == entity {
			return s
		}

		if strings.HasPrefix(entity, tail) {
			return s[:i]
		}
	}

	return s
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
