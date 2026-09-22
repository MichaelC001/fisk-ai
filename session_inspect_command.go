//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/choria-io/fisk"
	"github.com/choria-io/ui/columns"
	"github.com/choria-io/ui/table"

	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/sanitize"
)

var (
	sessionJSON bool

	queryTools        []string
	queryIteration    int64
	queryIterationSet bool
	queryErrors       bool
	queryCallsOnly    bool
	queryResultsOnly  bool
	queryInputMatch   string
	queryOutputMatch  string

	searchIdentity string
	searchModel    string
	searchCaller   string
	searchStatus   string
	searchSince    string
	searchUntil    string
	searchMatch    string
)

// sessionStatuses are the values sessionStatus prints, which --status accepts.
var sessionStatuses = []string{
	runstate.StatusOpen,
	string(runstate.ReasonCompleted),
	string(runstate.ReasonSuspended),
	string(runstate.ReasonError),
	string(runstate.ReasonBudget),
	string(runstate.ReasonMaxIterations),
}

func registerSessionInspectCommands(session *fisk.CmdClause) {
	query := session.Command("query", "Prints the tool calls of a checkpointed session and their answers").Action(sessionQueryAction)
	query.Arg("id", "Session id").Required().StringVar(&sessionArgID)
	query.Flag("tool", "Selects the calls to this tool; repeat to select several tools").PlaceHolder("NAME").StringsVar(&queryTools)
	query.Flag("iteration", "Selects the calls made at this loop iteration").PlaceHolder("N").IsSetByUser(&queryIterationSet).Int64Var(&queryIteration)
	query.Flag("errors", "Selects the calls whose answer is an error").UnNegatableBoolVar(&queryErrors)
	query.Flag("calls-only", "Prints the calls without their answers").UnNegatableBoolVar(&queryCallsOnly)
	query.Flag("results-only", "Prints only the answers, leaving out calls nothing has answered").UnNegatableBoolVar(&queryResultsOnly)
	query.Flag("input-match", "Selects the calls whose input JSON matches this regular expression").PlaceHolder("PATTERN").StringVar(&queryInputMatch)
	query.Flag("output-match", "Selects the calls whose answer matches this regular expression").PlaceHolder("PATTERN").StringVar(&queryOutputMatch)
	query.Flag("json", "Renders the selected calls as one JSON document").UnNegatableBoolVar(&sessionJSON)

	stats := session.Command("stats", "Reports what a checkpointed session cost: tokens per turn, wall clock and a table of tools").Action(sessionStatsAction)
	stats.Arg("id", "Session id").Required().StringVar(&sessionArgID)
	stats.Flag("json", "Renders the report as one JSON document").UnNegatableBoolVar(&sessionJSON)

	search := session.Command("search", "Lists the checkpointed sessions that match every filter given").Action(sessionSearchAction)
	search.Flag("identity", "Selects the sessions of this agent; a session journaled before the agent was recorded carries none and is left out").PlaceHolder("AGENT").StringVar(&searchIdentity)
	search.Flag("model", "Selects the sessions run against this model").PlaceHolder("MODEL").StringVar(&searchModel)
	search.Flag("caller", "Selects the sessions a channel recorded this caller for").PlaceHolder("CALLER").StringVar(&searchCaller)
	search.Flag("status", "Selects the sessions with this status: open, or the reason the session ended").PlaceHolder("STATUS").EnumVar(&searchStatus, sessionStatuses...)
	search.Flag("since", "Selects the sessions created at or after this time: a duration ago (24h, 7d), a date (2006-01-02, local midnight) or an RFC3339 time").PlaceHolder("WHEN").StringVar(&searchSince)
	search.Flag("until", "Selects the sessions created before this time: a duration ago (24h, 7d), a date (2006-01-02, local midnight) or an RFC3339 time").PlaceHolder("WHEN").StringVar(&searchUntil)
	search.Flag("match", "Selects the sessions whose prompt matches this regular expression").PlaceHolder("PATTERN").StringVar(&searchMatch)
	search.Flag("json", "Renders the matching sessions as one JSON document").UnNegatableBoolVar(&sessionJSON)
}

// sessionRecords reads a run's records from the configured session store without
// locking or folding it.
func sessionRecords(ctx context.Context, id string) ([]runstate.Record, error) {
	store, cleanup, err := openSessionStore(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	return store.Records(ctx, id)
}

// compileSessionPattern compiles a regular expression given to flag, naming the flag
// when it does not compile. An empty pattern is no filter and returns nil.
func compileSessionPattern(flag string, pattern string) (*regexp.Regexp, error) {
	if pattern == "" {
		return nil, nil
	}

	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid --%s pattern: %w", flag, err)
	}

	return re, nil
}

// newSessionQueryFilter builds the call filter the query flags describe. --results-only
// selects the answered calls, since it prints nothing for the others.
func newSessionQueryFilter() (runstate.CallFilter, error) {
	if queryCallsOnly && queryResultsOnly {
		return runstate.CallFilter{}, fmt.Errorf("--calls-only and --results-only cannot both be set")
	}

	input, err := compileSessionPattern("input-match", queryInputMatch)
	if err != nil {
		return runstate.CallFilter{}, err
	}

	output, err := compileSessionPattern("output-match", queryOutputMatch)
	if err != nil {
		return runstate.CallFilter{}, err
	}

	filter := runstate.CallFilter{
		Tools:    queryTools,
		Answered: queryResultsOnly,
		Errors:   queryErrors,
		Input:    input,
		Output:   output,
	}
	if queryIterationSet {
		iteration := queryIteration
		filter.Iteration = &iteration
	}

	return filter, nil
}

// sessionQueryJSON is the query command's machine rendering. The calls are the library
// type as it marshals, so a script sees the shape an embedder does. Nothing in it is
// sanitized: the encoder escapes control characters, and a consumer that prints a value
// sanitizes it there.
type sessionQueryJSON struct {
	RunID string          `json:"run_id"`
	Calls []runstate.Call `json:"calls"`
}

func sessionQueryAction(_ *fisk.ParseContext) error {
	ctx, cancel := interruptContext()
	defer cancel()

	filter, err := newSessionQueryFilter()
	if err != nil {
		return err
	}

	records, err := sessionRecords(ctx, sessionArgID)
	if err != nil {
		return err
	}

	selected := filter.Select(runstate.Calls(records))

	if sessionJSON {
		// An empty list rather than null, so a script reads none the same as some.
		if selected == nil {
			selected = []runstate.Call{}
		}

		return writeSessionJSON(os.Stdout, sessionQueryJSON{RunID: sessionArgID, Calls: projectSessionCalls(selected, queryCallsOnly, queryResultsOnly)})
	}

	if len(selected) == 0 {
		fmt.Println("No matching calls")
		return nil
	}

	for i, c := range selected {
		if i > 0 {
			fmt.Println()
		}
		printSessionCall(os.Stdout, c, queryCallsOnly, queryResultsOnly)
	}

	return nil
}

// projectSessionCalls drops the half of each call the operator asked to leave out:
// the answer under callsOnly, the input under resultsOnly.
func projectSessionCalls(calls []runstate.Call, callsOnly bool, resultsOnly bool) []runstate.Call {
	for i := range calls {
		if callsOnly {
			calls[i].Answer = nil
			calls[i].AnswerTime = time.Time{}
		}
		if resultsOnly {
			calls[i].Input = nil
		}
	}

	return calls
}

// printSessionCall renders one call for an operator: the tool, the call id and the
// iteration, then the input, then the answer. Everything printed is read back from a
// journal and sanitized, with newlines kept in the input and the answer.
func printSessionCall(w io.Writer, c runstate.Call, callsOnly bool, resultsOnly bool) {
	fmt.Fprintf(w, "%s %s (iteration %d)\n", sanitize.ForTerminal(c.Tool, 100), sanitize.ForTerminal(c.ToolUseID, 100), c.Iteration)

	if !resultsOnly {
		fmt.Fprintln(w, "Input:")
		fmt.Fprintln(w, sanitize.ForDisplay(prettySessionText(c.Input)))
	}

	if callsOnly {
		return
	}

	if c.Deferred != nil {
		fmt.Fprintf(w, "Deferred: %s\n", deferralSummary(*c.Deferred))
	}

	switch {
	case c.Answer == nil:
		fmt.Fprintln(w, "Answer: none yet")
	case c.Answer.Result.IsError:
		fmt.Fprintln(w, "Answer (error):")
		fmt.Fprintln(w, sanitize.ForDisplay(prettySessionText([]byte(c.Answer.Result.Content))))
	default:
		fmt.Fprintln(w, "Answer:")
		fmt.Fprintln(w, sanitize.ForDisplay(prettySessionText([]byte(c.Answer.Result.Content))))
	}
}

// prettySessionText indents text that parses as JSON and returns anything else as it
// is, so a tool that returned a command's output reads as that output.
func prettySessionText(text []byte) string {
	trimmed := bytes.TrimSpace(text)
	if len(trimmed) == 0 || !json.Valid(trimmed) {
		return strings.TrimRight(string(text), "\n")
	}

	var out bytes.Buffer
	err := json.Indent(&out, trimmed, "", "  ")
	if err != nil {
		return strings.TrimRight(string(text), "\n")
	}

	return out.String()
}

func sessionStatsAction(_ *fisk.ParseContext) error {
	ctx, cancel := interruptContext()
	defer cancel()

	records, err := sessionRecords(ctx, sessionArgID)
	if err != nil {
		return err
	}

	stats := runstate.Stats(records)

	// The library type as it marshals, unsanitized, for the reason sessionQueryJSON
	// gives.
	if sessionJSON {
		return writeSessionJSON(os.Stdout, stats)
	}

	printSessionStats(os.Stdout, stats)

	return nil
}

// printSessionStats renders a run's statistics for an operator: the frame, a table of
// assistant turns, a table of tools and a note saying what the tool time measured.
// Strings read back from the journal are sanitized to one line.
func printSessionStats(w io.Writer, s runstate.Statistics) {
	c := columns.New()
	c.Headingf("Session {bold}%s{/bold}", sanitize.ForTerminal(s.RunID, 100))

	if s.Agent != "" {
		c.Item("Agent", sanitize.ForTerminal(s.Agent, 100))
	}
	if s.Caller != "" {
		c.Item("Caller", sanitize.ForTerminal(s.Caller, 100))
	}
	c.Item("Model", sanitize.ForTerminal(s.Model, 100))
	c.Item("Status", sanitize.ForTerminal(sessionStatus(s.Terminal), 50))
	if !s.Created.IsZero() {
		c.Item("Created", columns.DateTime(s.Created))
	}
	if !s.Ended.IsZero() {
		c.Item("Ended", columns.DateTime(s.Ended))
	}

	// The cap applies to each turn on its own, so a conversation of several turns can
	// hold more responses than it and the two are not shown as a fraction.
	if s.MaxIterations > 0 {
		c.Item("Iterations", fmt.Sprintf("%d (at most %d per turn)", s.Iterations, s.MaxIterations))
	} else {
		c.Item("Iterations", s.Iterations)
	}

	wall, ok := s.WallClock()
	if ok {
		c.Item("Wall clock", columns.Annotated(columns.HumanizeDuration(wall), "creation to the last journal record"))
	} else {
		c.Item("Wall clock", "not recorded in this journal")
	}

	c.Item("Tokens", statsTokens(s))

	fmt.Fprintln(w, c.String())

	if len(s.Responses) > 0 {
		fmt.Fprintln(w)

		responses := table.NewTableWriter("Model responses")
		responses.AddHeaders("Iteration", "In", "Out", "Cache read", "Cache write", "Thinking", "Total")
		for _, r := range s.Responses {
			responses.AddRow(r.Iteration, r.Tokens.In, r.Tokens.Out, r.Tokens.CacheRead, r.Tokens.CacheCreate, r.Tokens.Thinking, r.Tokens.Total())
		}
		responses.WriteTo(w)
	}

	if len(s.Tools) == 0 {
		return
	}

	fmt.Fprintln(w)

	var calls, timed, untimed int64
	tools := table.NewTableWriter("Tools")
	tools.AddHeaders("Tool", "Kind", "Calls", "Dispatched", "Not run", "Unanswered", "Errors", "Time")
	for _, t := range s.Tools {
		calls += t.Calls
		timed += t.Timed
		untimed += t.Untimed
		tools.AddRow(sanitize.ForTerminal(t.Tool, 60), strings.Join(t.Kinds, ", "), t.Calls, t.Dispatched, t.Undispatched, t.Unanswered, t.Errors, statsToolTime(t))
	}
	tools.WriteTo(w)

	fmt.Fprintln(w)
	fmt.Fprint(w, statsTimingNote(calls, timed, untimed))
}

// statsTimingNote says what the tool time measured and which calls it leaves out. A
// call that ended but has no time is one whose records carry none. A call that never
// ended, by a result or a deferral, has no answer yet, and the Unanswered column counts
// it with the deferred calls still waiting.
func statsTimingNote(calls int64, timed int64, untimed int64) string {
	var b strings.Builder

	if timed > 0 {
		b.WriteString("Tool time is the gap between the record that ended each call, its result or its deferral, and the journal\n")
		b.WriteString("record before it. Tools run one at a time, so this is the call's own time plus any approval prompt answered\n")
		b.WriteString("while the run waited. An approval given while the run was suspended is not counted.\n")
	}

	switch {
	case untimed == calls:
		b.WriteString("The journal records no times for these calls, so no tool time is shown.\n")
	case untimed > 0:
		fmt.Fprintf(&b, "%d of %d calls ended with a record that carries no time and are not timed.\n", untimed, calls)
	}

	open := calls - timed - untimed
	if open > 0 {
		fmt.Fprintf(&b, "%d of %d calls have no answer yet, so they are not timed.\n", open, calls)
	}

	return b.String()
}

// statsTokens is the run's tokens against its budget, with the meaning sessionTokens
// gives them on session show: the total counts every tier, cache reads and writes
// included, and the split follows.
func statsTokens(s runstate.Statistics) string {
	split := fmt.Sprintf("%d in / %d out", s.Tokens.In, s.Tokens.Out)
	if s.Tokens.CacheRead > 0 || s.Tokens.CacheCreate > 0 {
		split = fmt.Sprintf("%s / %d cache read / %d cache write", split, s.Tokens.CacheRead, s.Tokens.CacheCreate)
	}

	if s.MaxTokens > 0 {
		return fmt.Sprintf("%d of %d budget (%s)", s.Tokens.Total(), s.MaxTokens, split)
	}

	return fmt.Sprintf("%d (%s)", s.Tokens.Total(), split)
}

// statsToolTime renders a tool row's time, saying how many of its calls it covers when
// that is not all of them, and a dash when it covers none.
func statsToolTime(t runstate.ToolStats) string {
	if t.Timed == 0 {
		return "-"
	}

	d := columns.HumanizeDuration(t.Duration)
	if t.Timed < t.Calls {
		return fmt.Sprintf("%s (%d of %d calls)", d, t.Timed, t.Calls)
	}

	return d
}

// parseSessionTime reads a --since or --until value: an RFC3339 time, a date taken as
// local midnight, or a duration counted back from now. An empty value is no bound and
// returns the zero time.
func parseSessionTime(flag string, value string, now time.Time) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}

	t, err := time.Parse(time.RFC3339, value)
	if err == nil {
		return t, nil
	}

	t, err = time.ParseInLocation(time.DateOnly, value, time.Local)
	if err == nil {
		return t, nil
	}

	d, err := fisk.ParseDuration(value)
	if err != nil || d < 0 {
		return time.Time{}, fmt.Errorf("invalid --%s %q: give a duration ago such as 24h or 7d, a date such as 2006-01-02, or an RFC3339 time", flag, value)
	}

	return now.Add(-d), nil
}

// newSessionSearchFilter builds the search filter the flags describe, reading a
// duration in --since or --until back from now.
func newSessionSearchFilter(now time.Time) (runstate.SearchFilter, error) {
	since, err := parseSessionTime("since", searchSince, now)
	if err != nil {
		return runstate.SearchFilter{}, err
	}

	until, err := parseSessionTime("until", searchUntil, now)
	if err != nil {
		return runstate.SearchFilter{}, err
	}

	match, err := compileSessionPattern("match", searchMatch)
	if err != nil {
		return runstate.SearchFilter{}, err
	}

	return runstate.SearchFilter{
		Agent:  searchIdentity,
		Model:  searchModel,
		Caller: searchCaller,
		Status: searchStatus,
		Since:  since,
		Until:  until,
		Prompt: match,
	}, nil
}

// sessionSearchRunJSON is one run in the search command's machine rendering. It is
// unsanitized, for the reason sessionQueryJSON gives.
type sessionSearchRunJSON struct {
	RunID   string    `json:"run_id"`
	Agent   string    `json:"agent,omitempty"`
	Caller  string    `json:"caller,omitempty"`
	Model   string    `json:"model,omitempty"`
	Status  string    `json:"status"`
	Created time.Time `json:"created,omitzero"`
	Updated time.Time `json:"updated,omitzero"`
	Ended   time.Time `json:"ended,omitzero"`
	Prompt  string    `json:"prompt"`
}

// sessionSearchJSON is the search command's machine rendering. NoAgentExcluded counts
// the runs --identity left out only because they carry no agent.
type sessionSearchJSON struct {
	Runs            []sessionSearchRunJSON `json:"runs"`
	NoAgentExcluded int                    `json:"no_agent_excluded"`
}

func sessionSearchAction(_ *fisk.ParseContext) error {
	ctx, cancel := interruptContext()
	defer cancel()

	filter, err := newSessionSearchFilter(time.Now())
	if err != nil {
		return err
	}

	store, cleanup, err := openSessionStore(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	// A zero list filter, so a run carrying no agent reaches the search filter, which
	// excludes it, rather than being taken in by ListFilter.MatchesAgent.
	infos, err := store.List(ctx, runstate.ListFilter{})
	if err != nil {
		return err
	}

	found, excluded := filter.Select(infos)
	sort.Slice(found, func(i, j int) bool {
		return found[i].Updated.After(found[j].Updated)
	})

	if sessionJSON {
		doc := sessionSearchJSON{Runs: []sessionSearchRunJSON{}, NoAgentExcluded: excluded}
		for _, info := range found {
			doc.Runs = append(doc.Runs, sessionSearchRunJSON{
				RunID:   info.RunID,
				Agent:   info.Agent,
				Caller:  info.Caller,
				Model:   info.Model,
				Status:  sessionStatus(info.Terminal),
				Created: info.Created,
				Updated: info.Updated,
				Ended:   info.Ended,
				Prompt:  info.Prompt,
			})
		}

		return writeSessionJSON(os.Stdout, doc)
	}

	printSessionSearch(os.Stdout, filter, found, excluded)

	return nil
}

// printSessionSearch renders the matching runs as the table session ls prints, with an
// agent, caller or created column added where a filter made it relevant, then the
// count of runs --identity left out for carrying no agent. Values read back from a
// journal are sanitized to one line, as session ls sanitizes them.
func printSessionSearch(w io.Writer, f runstate.SearchFilter, found []runstate.RunInfo, excluded int) {
	if len(found) == 0 {
		fmt.Fprintln(w, "No sessions matched")
	} else {
		showAgent := f.Agent != ""
		showCaller := f.Caller != ""
		showCreated := !f.Since.IsZero() || !f.Until.IsZero()

		headers := []any{"ID"}
		if showAgent {
			headers = append(headers, "Agent")
		}
		if showCaller {
			headers = append(headers, "Caller")
		}
		headers = append(headers, "Model", "Status")
		if showCreated {
			headers = append(headers, "Created")
		}
		headers = append(headers, "Updated", "Prompt")

		tbl := table.NewTableWriter("")
		tbl.AddHeaders(headers...)
		for _, info := range found {
			row := []any{sanitize.ForTerminal(info.RunID, 100)}
			if showAgent {
				row = append(row, sanitize.ForTerminal(info.Agent, 50))
			}
			if showCaller {
				row = append(row, sanitize.ForTerminal(info.Caller, 50))
			}
			row = append(row, sanitize.ForTerminal(info.Model, 100), sanitize.ForTerminal(sessionStatus(info.Terminal), 50))
			if showCreated {
				row = append(row, info.Created)
			}
			row = append(row, info.Updated, sanitize.ForTerminal(info.Prompt, 50))
			tbl.AddRow(row...)
		}
		tbl.WriteTo(w)
	}

	switch {
	case excluded == 1:
		fmt.Fprintln(w)
		fmt.Fprintln(w, "1 more session matched every other filter but carries no agent, so --identity left it out. It was journaled before the agent was recorded.")
	case excluded > 1:
		fmt.Fprintln(w)
		fmt.Fprintf(w, "%d more sessions matched every other filter but carry no agent, so --identity left them out. They were journaled before the agent was recorded.\n", excluded)
	}
}

// writeSessionJSON writes one indented JSON document. HTML escaping is off because the
// output is read by a person or a parser, never placed in a page.
func writeSessionJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)

	return enc.Encode(v)
}
