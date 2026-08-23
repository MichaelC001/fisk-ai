//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package slack

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/remotetools"
)

// Everything a thread hears about a run in progress comes through this sink. Declaring both
// contracts makes a change to either a compile error here rather than a thread that stops
// narrating.
var (
	_ agent.Events             = (*events)(nil)
	_ agent.RemoteHostReporter = (*events)(nil)
)

// maxThreadWarnings is how many distinct advisories one turn puts in a thread. Anything
// past it is counted rather than written, so a run that raised the same class of problem
// forty times costs the thread one line and a number instead of forty messages.
const maxThreadWarnings = 5

// The two shapes the advisories of one turn arrive in. A turn that raised one says it in a
// sentence; a turn that raised several lists them, since three sentences run together read
// as one long sentence about nothing.
const (
	warnNoteOne  = "Note: "
	warnNoteMany = "Notes:"
	warnNoteMore = "and %d more I have not listed here."
	warnNoteItem = "\n- "
)

// events is the run's narration for one turn: it moves that turn's status message and
// collects the advisories the ending puts under the answer.
//
// Nothing here talks to Slack. A method records where the run has reached and the turn's
// publisher writes whatever that is by the time the workspace's allowance lets it through,
// so a run that called forty tools spends nothing like forty calls saying so.
//
// Every method is called from the run's own goroutine, Panicked from a deferred recover
// while it unwinds, so none of them blocks or panics. The advisories are the exception to
// the single goroutine: they are written here and read when the turn ends, which is
// somewhere else, so they are guarded.
type events struct {
	t   *turn
	log *slog.Logger

	mu sync.Mutex

	// lines is what the thread will be told, already rendered and deduplicated, and
	// dropped counts the distinct advisories that arrived once lines was full.
	//
	// seen is every line already accounted for, so a tool failing the same way twenty
	// times is one line rather than twenty. It is bounded by the number of warning kinds,
	// each of which renders to a fixed sentence.
	lines   []string
	seen    map[string]struct{}
	dropped int
}

// newEvents builds the sink for one turn. It is built with the turn rather than with the
// work, since the ending reads what it collected whether or not the turn ever ran.
func newEvents(t *turn) *events {
	return &events{t: t, log: t.log, seen: map[string]struct{}{}}
}

// Starting reports the run's resolved parameters, none of which a thread has any use for:
// the tool count, the trace file and the session id are the worker's business. It records
// that the run began.
//
// The message a turn is posted with already reads as thinking, so this spends no call: it
// records the state Slack was given at admission and the publisher finds nothing to write.
func (e *events) Starting(agent.RunInfo) {
	e.t.status.note(hintThinking)
}

// Message moves the status message back to thinking for an assistant turn on the way to the
// answer.
//
// The terminal one is not posted here. It is the run's final answer and the turn's ending
// posts it from Outcome.Text, so a thread would otherwise be given the same answer twice,
// once as a message nobody is notified about and once as the message they are.
func (e *events) Message(_ llm.Response, terminal bool) {
	if terminal {
		return
	}

	e.t.status.note(hintThinking)
}

// ToolCall moves the status message to the family the tool belongs to. Which tool is being
// run against what is not the thread's to publish, so the hint table is what a thread hears
// and the worker's log has the call.
func (e *events) ToolCall(t agent.ToolTrace) {
	e.t.status.note(hintFor(t.Name))
}

// ToolResult is not shown. A result is what the model reads, it is unbounded and it carries
// whatever the tool returned, and the status message is already on the hint of the call
// this answers, so there is nothing here a thread can act on.
func (e *events) ToolResult(agent.ToolResultTrace) {}

// LLMRequest is not shown. It is emitted only when a run is verbose and it describes one
// request to the provider, which is a person debugging this worker's business rather than a
// thread's.
func (e *events) LLMRequest(string) {}

// SessionRotated is logged and never posted. The previous session is a journal id, which is
// exactly the material this channel leaves out of a thread, and the log is the only place an
// operator could pick that conversation back up from.
//
// Nothing here produces one today: a rotation is a context reset, and a reset arrives on
// the interactive continuation this channel does not supply.
func (e *events) SessionRotated(prevID string) {
	e.log.Info("The run started a fresh session for this thread", "previous_session", prevID)
}

// Panicked is logged and never posted. The stack leaks absolute paths, module layout and
// frame arguments, and its own contract requires an implementation that forwards anywhere
// to keep it local; a thread of colleagues is somewhere it forwards to.
//
// What the thread is told is that the turn ended, which the outcome the run returns says.
func (e *events) Panicked(value any, stack []byte) {
	e.log.Error("A run crashed", "panic", value, "stack", string(stack))
}

// Warn collects an advisory for the ending to put under the answer.
//
// It is collected rather than posted. A warning is not running commentary and the run is
// still working, so writing it the moment it arrives would interleave the holes in an
// answer with the progress towards it and spend a call on each; under the answer they are
// read once, by somebody who has just read what the answer claims.
func (e *events) Warn(w agent.Warning) {
	e.record(warningLine(w))
}

// RemoteHostNotes collects what importing another agent's tools had to say about it.
//
// This sink implements the optional half because nothing else on this path reports it: the
// notes reach an implementation or nowhere, so a mistyped include filter would otherwise
// leave every thread short of tools and say nothing.
func (e *events) RemoteHostNotes(imports []remotetools.HostImport) {
	for _, imp := range imports {
		for _, w := range agent.HostImportWarnings(imp) {
			e.Warn(w)
		}
	}
}

// record keeps one rendered advisory, once.
func (e *events) record(line string) {
	if line == "" {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	_, dup := e.seen[line]
	if dup {
		return
	}

	e.seen[line] = struct{}{}

	if len(e.lines) >= maxThreadWarnings {
		e.dropped++

		return
	}

	e.lines = append(e.lines, line)
}

// note is what the thread is told about the advisories this run raised, empty where it
// raised none.
func (e *events) note() string {
	e.mu.Lock()
	defer e.mu.Unlock()

	switch {
	case len(e.lines) == 0:
		return ""

	case len(e.lines) == 1 && e.dropped == 0:
		return warnNoteOne + e.lines[0]
	}

	out := warnNoteMany + warnNoteItem + strings.Join(e.lines, warnNoteItem)
	if e.dropped > 0 {
		out += warnNoteItem + fmt.Sprintf(warnNoteMore, e.dropped)
	}

	return out
}

// The advisories a thread hears, one fixed sentence for each kind.
//
// None of them carries the warning's own fields. A Warning names tools, paths, parameters
// and errors, some of it supplied by a tool, which is the material the hint table already
// leaves out of a channel everybody reads. A person in a thread can act on the answer
// having a hole in it and roughly which one; the worker's log has the rest, the server
// recording every advisory whatever a channel does with it.
const (
	warnJournal = "I could not fully record this turn, so what I remember of this thread may be incomplete."
	warnTrace   = "I could not fully write this turn's trace, which costs the record of it rather than the answer."
	warnRemote  = "I did not take every tool another agent offered me, so something I usually reach may be missing."
)

var warningLines = map[agent.WarningKind]string{
	agent.WarnHITLNoTerminal:         "I had nobody to ask a question of during this turn, so the tools that need an answer from a person declined instead.",
	agent.WarnConfirmNoTerminal:      "I had nobody to ask for approval during this turn, so the tools that need it were not run.",
	agent.WarnConfirmTagUnmatched:    "One of my confirmation rules names a tool I do not have.",
	agent.WarnUnknownTool:            "I tried to use a tool that is not available to me.",
	agent.WarnMissingRequired:        "I called a tool without everything it needs, so that call did not run.",
	agent.WarnJournalTerminal:        warnJournal,
	agent.WarnJournalUser:            warnJournal,
	agent.WarnJournalMemoryRevisions: warnJournal,
	agent.WarnJournalClose:           warnJournal,
	agent.WarnResumePausedTurn:       "I picked this thread up at a point whose earlier state may have expired.",
	agent.WarnMemoryIndex:            "I could not read my memory at the start of this turn.",
	agent.WarnSessionRotate:          "I could not start a fresh conversation for this thread, so it continues in the current one.",
	agent.WarnToolSearchUnsupported:  "I have more tools than this model can search, so every request carries all of them and leaves less room for the conversation.",
	agent.WarnKnowledgeIndexAbsent:   "My knowledge index is not where I expect it, so anything I looked for in it found nothing.",
	agent.WarnTraceClose:             warnTrace,
	agent.WarnTraceWrite:             warnTrace,
	agent.WarnRunEndHook:             "One of the hooks that runs when I finish failed.",
	agent.WarnUnknownReservedTag:     "One of my tools carries a tag I do not know, so whatever it was meant to do did not happen.",
	agent.WarnBehaviorTagConflict:    "One of my tools carries tags that contradict each other, so I treated it as the more dangerous of the two.",
	agent.WarnToolTimeout:            "A tool ran past its time limit and was stopped, so I answered without its result.",
	agent.WarnToolDeferred:           "A tool is waiting on something before it can answer, so this turn stopped before it was finished.",
	agent.WarnApprovalsDropped:       "The approvals this thread had already given were not restored, so I will ask about them again.",
	agent.WarnToolSetDrift:           "My tools have changed since this thread last ran, and I continued with the ones I have now.",
	agent.WarnBudgetDrift:            "The limits on this thread have changed since it last ran, and I continued under the current ones.",
	agent.WarnRemoteTagFilterIgnored: warnRemote,
	agent.WarnRemoteToolsSkipped:     warnRemote,
	agent.WarnRemoteNoTools:          warnRemote,
}

// warningLine is what one advisory reads as in a thread.
//
// A kind with no sentence of its own is named rather than dropped. The name is this
// program's own stable identifier for the kind, so it carries nothing a tool supplied, and
// an advisory nobody has written a sentence for is still a hole in the answer somebody has
// to be told about.
func warningLine(w agent.Warning) string {
	line, ok := warningLines[w.Kind]
	if ok {
		return line
	}

	return fmt.Sprintf("Something did not go to plan on the way to this answer (%s).", w.Kind)
}
