//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/tui"
	"github.com/choria-io/fisk-ai/internal/util"
)

// lineClient renders a run for the line UI and answers what it asks.
//
// It is the caller's half of what a2aendpoint's sink produces, and it is the same
// whether the run is in this process or on a worker somewhere else. The split is the
// one the line UI has always had: the answer goes to stdout so a pipe gets it clean,
// and everything else goes to stderr.
type lineClient struct {
	prompter       toolkit.Prompter
	noColor        bool
	showToolOutput bool
	showThinking   bool

	// answered records that the answer reached stdout as it was produced, so the
	// terminal message does not print it a second time.
	answered bool
}

// Block renders one event of the run.
func (c *lineClient) Block(block a2a.Block) {
	switch b := block.Content().(type) {
	case a2a.TextBlock:
		c.text(b)

	case a2a.ThinkingBlock:
		if c.showThinking && b.Text != "" {
			fmt.Fprintf(os.Stderr, "\n[thinking]\n%s\n", util.SanitizeForDisplay(b.Text))
		}

	case a2a.PromptBlock:
		// Only a replayed conversation carries these: the caller wrote the prompt it
		// sent itself.
		fmt.Fprintf(os.Stderr, "\n> %s\n", util.SanitizeForDisplay(b.Text))

	case a2a.ToolCallBlock:
		fmt.Fprintf(os.Stderr, "-> %s\n", tui.CallLine(b.Name, b.Input))

	case a2a.AgentCallBlock:
		fmt.Fprintf(os.Stderr, "-> %s (remote %s)\n", util.SanitizeForTerminal(b.Name, 120), util.SanitizeForTerminal(b.Task, 120))

	case a2a.ToolResultBlock:
		if !c.showToolOutput {
			return
		}

		fmt.Fprintf(os.Stderr, "<-\n%s\n", toolResultLine(b.Output, b.IsError).Text)

	case a2a.WarningBlock:
		msg := blockWarningMessage(b)
		if msg != "" {
			fmt.Fprintf(os.Stderr, "warning: %s\n", msg)
		}

	case a2a.StatusBlock:
		c.status(b)
	}
}

// text prints the model's prose: the answer to stdout, and anything it said on the way
// there to stderr, which is the split a piped run depends on.
func (c *lineClient) text(b a2a.TextBlock) {
	if b.Text == "" {
		return
	}

	if !b.Final {
		fmt.Fprintln(os.Stderr, util.RenderMarkdownTo(b.Text, os.Stderr, c.noColor))

		return
	}

	fmt.Fprintln(os.Stdout, util.RenderAnswer(b.Text, c.noColor))
	c.answered = true
}

// status marks a replayed conversation, so what already happened reads as history
// rather than as a turn arriving now. The progress statuses are for a caller pacing
// itself and have nothing to show a person.
func (c *lineClient) status(b a2a.StatusBlock) {
	switch b.Phase {
	case a2a.PhaseReplayStart:
		fmt.Fprintln(os.Stderr, "\n--- resuming ---")

	case a2a.PhaseReplayEnd:
		if b.Truncated {
			fmt.Fprintf(os.Stderr, "(showing the last %d of a longer conversation)\n", b.Count)
		}
		fmt.Fprintln(os.Stderr, "--- continuing ---")
	}
}

// Question puts one of the run's questions to the operator and answers it.
//
// The four kinds are the four methods of toolkit.Prompter, so what a person sees here
// is what they saw when the run was in this process. An aborted prompt, which is an
// interrupt or a closed input, is answered as no operator: it ends the question at once
// and fails a gated command closed, where saying nothing would hold the run for a whole
// window first.
func (c *lineClient) Question(ctx context.Context, ask *a2a.ElicitRequest) (*a2a.ElicitReply, error) {
	return answerQuestion(ctx, c.prompter, ask, func(msg string) {
		fmt.Fprintf(os.Stderr, "warning: %s\n", msg)
	})
}

// answerQuestion puts one question to a prompter and turns what it says into a reply.
//
// It is shared by every surface this program has, because the vocabulary is the
// prompter's four methods and what changes between surfaces is only who is asked. An
// aborted prompt, which is an interrupt or a closed input, is answered as no operator:
// that ends the question at once and fails a gated command closed, where saying nothing
// would hold the run for a whole window first. Anything else is reported through warn
// before the same answer is given.
func answerQuestion(ctx context.Context, prompter toolkit.Prompter, ask *a2a.ElicitRequest, warn func(string)) (*a2a.ElicitReply, error) {
	// One question at a time, whichever surface asks it. TaskHandler.Question is
	// contracted as concurrent and a terminal is not: the full-screen prompter draws
	// every overlay under one page name, so a second question would replace the first
	// and the first's dismissal would remove the second, and two survey prompts would
	// read one stdin between them. The contended thing is the terminal rather than
	// either widget, which is why the lock is here and not in a prompter.
	//
	// A question waiting here keeps its window: the acks that hold it open are sent
	// around this call rather than by it.
	askOneAtATime.Lock()
	defer askOneAtATime.Unlock()

	reply, err := putQuestion(ctx, prompter, ask)
	if err == nil {
		return reply, nil
	}

	if warn != nil && !errors.Is(err, toolkit.ErrPromptAborted) {
		warn(fmt.Sprintf("answering the run's question failed: %v", err))
	}

	return a2a.NewNoOperatorReply(ask, clientSender), nil
}

func putQuestion(ctx context.Context, prompter toolkit.Prompter, ask *a2a.ElicitRequest) (*a2a.ElicitReply, error) {
	switch ask.Kind {
	case a2a.ElicitApprove:
		choice, err := prompter.ApproveCommand(ctx, toolkit.GateRequest{
			ToolUseID: ask.ToolUseID,
			Command:   ask.Command,
			Display:   ask.Display,
			Tag:       ask.Tag,
		})
		if err != nil {
			return nil, err
		}

		return a2a.NewApproveReply(ask, clientSender, approveChoice(choice)), nil

	case a2a.ElicitConfirm:
		confirmed, err := prompter.Confirm(ctx, ask.Question)
		if err != nil {
			return nil, err
		}

		return a2a.NewConfirmReply(ask, clientSender, confirmed), nil

	case a2a.ElicitSelect:
		index, err := prompter.Select(ctx, ask.Question, ask.Options)
		if err != nil {
			return nil, err
		}

		return a2a.NewSelectReply(ask, clientSender, index), nil

	case a2a.ElicitInput:
		value, err := prompter.Input(ctx, ask.Question, ask.Default)
		if err != nil {
			return nil, err
		}

		return a2a.NewInputReply(ask, clientSender, value), nil
	}

	return nil, fmt.Errorf("the run asked a %q question, which this client does not know how to put to anybody", ask.Kind)
}

// clientSender is what this terminal calls itself on the wire. It reaches its own
// worker or one an operator pointed it at, and neither verifies it.
const clientSender = "terminal"

// askOneAtATime serializes the questions put to whoever is at this terminal. There is
// one of them and one terminal, however many runs are asking.
var askOneAtATime sync.Mutex

// lineReplay is how much of a stored conversation a resumed run asks for. The line UI
// prints into a scrollback nobody reads upwards, so it asks for enough to see where the
// conversation was rather than for all of it.
const lineReplay = 40

// runAsClient answers one prompt as a client of the prompts channel.
//
// This is the path a terminal and a peer agent now share. What a terminal adds is the
// worker underneath it, which it owns: leaving takes it with you, and that is what makes
// the second interrupt still a hard stop with nothing on the wire for it.
//
// It is one turn rather than a conversation because it has no input row to open a second
// one with. The conversation is still there afterwards, and the handle it prints is what
// continues it.
func runAsClient(ctx context.Context, stop context.CancelFunc, host *hostedAgent, token string) error {
	prompt := strings.TrimSpace(strings.Join(q, " "))
	// The full-screen view opens its input row and waits; there is no row here, so a run
	// with nothing to ask is a mistake worth naming rather than an empty turn to spend.
	if prompt == "" && token == "" {
		return fmt.Errorf("no prompt: give one as an argument, or --resume a conversation to continue")
	}

	client := &lineClient{
		prompter:       toolkit.NewSurveyPrompter(),
		noColor:        noColor,
		showToolOutput: showToolOutput,
		showThinking:   showThinking,
	}

	conversation := token

	// A resumed conversation is read back before the turn, since a read takes no turn
	// and a request that carries a prompt cannot also ask for history without the
	// history arriving underneath the prompt.
	if token != "" {
		err := readConversation(ctx, host, token, client, lineReplay)
		if err != nil {
			return err
		}
	}

	// With nothing to ask, a token on its own continues the run that stopped part way.
	var req *a2a.Request
	if prompt == "" {
		req = a2a.NewResume(token)
	} else {
		req = a2a.NewRequest(prompt)
		req.ConversationToken = token
	}

	req.Force = forceResume

	// The first interrupt asks the run to stop where it can be continued, which is what
	// it has always meant here; the second gives up on it and takes the worker with it.
	restore := onInterrupt(ctx, host, req.Request, stop)
	defer restore()

	out, err := host.client.RunTask(ctx, host.identity, req, client)
	if err != nil {
		return err
	}

	if out.Ack != nil && out.Ack.ConversationToken != "" {
		conversation = out.Ack.ConversationToken
	}

	// An answer somebody gave after the run had given the question up. There is no input
	// row here to send it beside, but there is a live client and a live worker, and a
	// request carrying only an answer needs neither.
	deliverHeldAnswers(ctx, host, conversation, out, client)

	reportErr := client.report(out)

	// Every run leaves a conversation, so the handle prints however this one ended. The
	// exception is a conversation that spent its token budget: it is still readable with
	// fisk session show, but resuming it reaches the same refusal, so printing the one
	// line that looks actionable would send a person straight back into it.
	if !endedOnBudget(out) {
		hint := resumeHint(host.identity, host.natsContext, conversation)
		if hint != "" {
			fmt.Fprintf(os.Stderr, "%s\n", hint)
		}
	}

	return reportErr
}

// ackMaxTokens is the conversation's token bound as the agent reported it, zero where it
// said none or never acked.
func ackMaxTokens(out *a2a.TaskOutcome) int64 {
	if out == nil || out.Ack == nil {
		return 0
	}

	return out.Ack.MaxTokens
}

// endedOnBudget reports whether a conversation spent its whole token allowance, which is
// the one ending a resume cannot get past: the allowance belongs to the conversation, so
// the next turn refuses however it is sent and whoever sends it.
func endedOnBudget(out *a2a.TaskOutcome) bool {
	return out != nil && out.Error != nil && out.Error.Code == a2a.CodeBudgetExhausted
}

// deliverHeldAnswers sends what a person answered too late for the run that asked.
//
// It is the same delivery the full-screen client makes and for the same reason: the
// mechanism exists on the wire, an answer nobody delivers leaves the conversation
// waiting on a call, and the only alternative is telling somebody to go and use a
// command that supplies a tool result, which is a different thing from answering a
// confirm gate and is not runnable from this machine when the journal is elsewhere.
func deliverHeldAnswers(ctx context.Context, host *hostedAgent, token string, out *a2a.TaskOutcome, h a2a.TaskHandler) {
	if out == nil || len(out.Unsent) == 0 || token == "" {
		return
	}

	for _, held := range out.Unsent {
		fmt.Fprintln(os.Stderr, "your answer arrived after the run gave the question up; sending it now")

		req := a2a.NewAnswerRequest(token, held)
		req.Force = forceResume

		sent, err := host.client.RunTask(ctx, host.identity, req, h)
		switch {
		case err != nil:
			fmt.Fprintf(os.Stderr, "warning: delivering your answer failed: %v\n", err)
		case sent.Error != nil:
			fmt.Fprintf(os.Stderr, "warning: delivering your answer failed: %s\n", endingMessage(sent.Error))
		}
	}
}

// readConversation asks the worker for a conversation and renders it, which is how a
// resumed run opens on what it is continuing.
//
// The request carries a replay count and nothing else, which is what makes it a read:
// no turn is taken and no model is called, so it is answered whatever state the
// conversation is in.
func readConversation(ctx context.Context, host *hostedAgent, token string, h a2a.TaskHandler, replay int) error {
	req, err := a2a.NewRead(token, replay)
	if err != nil {
		return err
	}

	out, err := host.client.RunTask(ctx, host.identity, req, h)
	if err != nil {
		return err
	}

	// A conversation that cannot be read cannot be continued either, so this ends the
	// run rather than being a note on the way to a turn that would fail the same way.
	if out.Error != nil {
		return fmt.Errorf("%s", endingMessage(out.Error))
	}

	return nil
}

// onInterrupt wires the two-stage interrupt for a client: the first asks the run to
// stop at a boundary, the second ends this process, which is where the run is.
//
// A cancel is a message rather than a signal now, so it is sent from here. Nothing is
// held if the worker has already finished: canceling a task that has ended is answered
// as received and changes nothing.
func onInterrupt(ctx context.Context, host *hostedAgent, request string, stop context.CancelFunc) func() {
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)

	go func() {
		_, ok := <-sigs
		if !ok {
			return
		}

		fmt.Fprintln(os.Stderr, "\nasked the run to stop at its next boundary; press ^C again to leave it")

		_, err := host.client.Cancel(ctx, host.identity, request, "the operator interrupted the run")
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: asking the run to stop failed: %v\n", err)
		}

		_, ok = <-sigs
		if !ok {
			return
		}

		stop()
	}()

	return func() { signal.Stop(sigs); close(sigs) }
}

// clientWorkerLogger is where the hosted agent's own logging goes: nowhere by default,
// since the run's narration reaches the terminal as events and the rest is machinery.
// Under --verbose it goes to stderr, where somebody debugging their own worker wants it.
//
// The full-screen view takes stderr for itself and flushes what it captured once the
// terminal is restored, so a worker logging into it would say nothing during the run and
// everything at once afterwards. That surface stays quiet whatever --verbose says.
func clientWorkerLogger(fullScreen bool) *slog.Logger {
	if verbose && !fullScreen {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}

	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// report renders how a run ended and reports whether the caller should treat it as a
// failure.
//
// An ending is not always a failure and not always an answer. A suspended run did what
// it could and left a conversation to continue, a deferred one is waiting on somebody,
// and both are ordinary. What makes an ending worth an exit code is the run not having
// done the work: it failed, it crashed, or it was never taken.
func (c *lineClient) report(out *a2a.TaskOutcome) error {
	if out.Result != nil {
		// The answer reached stdout as it was produced. It is printed here only when it
		// did not, which is a worker that sent no final text block.
		if !c.answered && out.Result.Text != "" {
			fmt.Fprintln(os.Stdout, util.RenderAnswer(out.Result.Text, c.noColor))
		}

		printUsage(out.Result.Usage, ackMaxTokens(out))

		return nil
	}

	if out.Error == nil {
		return fmt.Errorf("the run ended without saying how")
	}

	fmt.Fprintf(os.Stderr, "\n%s\n", endingMessage(out.Error))
	printUsage(out.Error.Usage, ackMaxTokens(out))

	switch out.Error.Code {
	case a2a.CodeSuspended, a2a.CodeDeferred:
		return nil
	}

	return fmt.Errorf("%s", util.SanitizeForTerminal(out.Error.Err, 400))
}

// endingMessage says what happened and, where there is one, what to do about it. The
// worker's own message is included because it carries the detail: which tool is
// waiting, which call was refused, what the model provider said.
func endingMessage(msg *a2a.ErrorMessage) string {
	detail := util.SanitizeForTerminal(msg.Err, 400)

	switch msg.Code {
	case a2a.CodeSuspended:
		return "the run stopped where it can be resumed: " + detail

	case a2a.CodeDeferred:
		return "the run is waiting on an answer before it can go on: " + detail

	case a2a.CodeCapacity:
		return "the agent is already running as much as it will at once; try again shortly"

	case a2a.CodeDraining:
		return "the agent is going out of service and took no work"

	case a2a.CodeConversationBusy:
		return "a turn of this conversation is already running; wait for it to finish"

	case a2a.CodeUnknownConversation:
		// Never "start a new one": the usual cause is a handle that names a conversation
		// which does exist, given to an agent that cannot resolve it. A session id is one
		// way to say that, since only the store it came from can turn it into the token
		// the agent takes.
		return "this agent has no conversation by that name; check that the handle is the conversation token, which 'fisk session show' prints, and that it belongs to this agent"

	case a2a.CodeBudgetExhausted:
		// The detail carries both numbers and names the key, so what is added here is
		// only what a person does next. It is a property of the conversation rather than
		// of this turn, so resuming reaches the same refusal.
		return "this conversation has used up its token budget and will take no further turn: " + detail

	case a2a.CodeTurnNotTaken:
		return "the conversation could not take this turn, so nothing ran: " + detail

	case a2a.CodeCrashed:
		return "the agent crashed, which is a bug rather than a model or tool failure; the detail is in its log"

	case a2a.CodeNotStarted:
		return "the work was taken and never started; send it again"

	case a2a.CodeCanceled:
		return "the agent stopped the run"

	case a2a.CodeRejected:
		return "the agent refused the work: " + detail
	}

	return detail
}

// printUsage reports what the turn cost, for the endings that ran. A run that was
// refused spent nothing and says nothing.
func printUsage(usage *a2a.Usage, maxTokens int64) {
	if usage == nil {
		return
	}

	total := usage.InputTokens + usage.OutputTokens
	if total == 0 {
		return
	}

	// The total goes first and is what the budget counts, InputTokens being
	// cache-inclusive, so the number beside the bound is the one the bound is compared
	// against rather than a share of it.
	if maxTokens > 0 {
		fmt.Fprintf(os.Stderr, "\n%d of %d tokens (%d in / %d out)\n", total, maxTokens, usage.InputTokens, usage.OutputTokens)

		return
	}

	fmt.Fprintf(os.Stderr, "\n%d tokens in / %d out\n", usage.InputTokens, usage.OutputTokens)
}

// approveChoice maps the operator's three-way answer onto the wire.
func approveChoice(choice toolkit.ConfirmChoice) a2a.ElicitChoice {
	switch choice {
	case toolkit.ConfirmOnce:
		return a2a.ChoiceOnce
	case toolkit.ConfirmAlways:
		return a2a.ChoiceAlways
	default:
		return a2a.ChoiceNo
	}
}

// blockWarningMessage is the operator-facing text for a warning that arrived over the
// wire, which is warningMessage given a kind that crossed as a name.
//
// A kind this build does not know still says something: the run raised it, and its
// fields are what the wire carried. Dropping it would hide a run reporting that
// something went wrong.
func blockWarningMessage(b a2a.WarningBlock) string {
	kind, known := agent.ParseWarningKind(b.Kind)
	if !known {
		return unknownWarningMessage(b)
	}

	warning := agent.Warning{Kind: kind, Name: b.Name, Count: b.Count, Params: b.Params}
	if b.Error != "" {
		warning.Err = errors.New(b.Error)
	}

	msg := warningMessage(warning)
	if msg == "" {
		return unknownWarningMessage(b)
	}

	return msg
}

// unknownWarningMessage renders a warning from its fields alone.
func unknownWarningMessage(b a2a.WarningBlock) string {
	parts := []string{util.SanitizeForTerminal(b.Kind, 120)}

	if b.Name != "" {
		parts = append(parts, util.SanitizeForTerminal(b.Name, 120))
	}
	if len(b.Params) > 0 {
		parts = append(parts, util.SanitizeForTerminal(strings.Join(b.Params, ", "), 200))
	}
	if b.Error != "" {
		parts = append(parts, util.SanitizeForTerminal(b.Error, 200))
	}

	return strings.Join(parts, ": ")
}
