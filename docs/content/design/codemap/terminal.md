+++
title = "Terminal and events"
description = "One typed event contract, two terminal surfaces, and a screen owned by exactly one goroutine"
toc = true
weight = 110
+++

The agent package formats no prose. It emits typed events and lets the caller decide how they look. Two terminal surfaces
consume that contract: a line-oriented renderer and a full-screen tview application. A third, structured sink exists for a
server that does not yet live in this repository.

{{% notice style="note" title="Where it lives" %}}
`internal/agent/events.go` defines the contract, `slog_events.go` the structured sink. `run_events.go` is the line UI and
`run_tui_events.go` the full-screen one. `internal/tui` holds the screen: `live.go`, `viewer.go`, `prompter.go`,
`splash.go`. `internal/util` supplies sanitization, markdown rendering, terminal detection, the trace, and the run
summary.
{{% /notice %}}

## The event contract

`Events` is a typed sink with a three-clause contract: it is called from the single run goroutine, it may be called
during teardown, and a per-run sink therefore needs no locking.

The payloads carry display decisions rather than rendered text. `ToolTrace` is the clearest example: it carries both a
full display form and a middle-elided short form, plus the presentation and the provider kind. Renderers key suppression
off presentation and never off kind.

All warning wording lives in one function shared by both surfaces, so the line UI and the full-screen UI cannot drift
apart on phrasing. The same is true for tool-result lines and for the tool-call elision function, which the line UI calls
with a width measured from stderr so both surfaces cut at the same point.

## Who owns the screen

<figure class="cm-diagram">
  <svg viewBox="0 0 760 330" role="img" aria-label="Events travelling from the run goroutine to a drawn frame">
    <defs>
      <marker id="tm-ah" markerWidth="9" markerHeight="9" refX="7" refY="3" orient="auto"><path d="M0,0 L7,3 L0,6 Z" fill="var(--cm-accent)"/></marker>
    </defs>
    <rect class="cm-svg-box" x="20" y="40" width="200" height="54" rx="8"/>
    <text class="cm-svg-label" x="120" y="63" text-anchor="middle">run goroutine</text>
    <text class="cm-svg-sub" x="120" y="80" text-anchor="middle">single, needs no locks</text>
    <rect x="280" y="40" width="200" height="54" rx="8" fill="color-mix(in srgb, var(--cm-accent) 12%, transparent)" stroke="var(--cm-accent)"/>
    <text class="cm-svg-label" x="380" y="63" text-anchor="middle" style="fill:var(--cm-accent)">Events sink</text>
    <text class="cm-svg-sub" x="380" y="80" text-anchor="middle">cli, tcell, or slog</text>
    <line x1="220" y1="67" x2="274" y2="67" stroke="var(--cm-accent)" stroke-width="2" marker-end="url(#tm-ah)"/>
    <rect class="cm-svg-box" x="540" y="40" width="200" height="54" rx="8"/>
    <text class="cm-svg-label" x="640" y="63" text-anchor="middle">QueueUpdateDraw</text>
    <text class="cm-svg-sub" x="640" y="80" text-anchor="middle">blocks until drawn</text>
    <line x1="480" y1="67" x2="534" y2="67" stroke="var(--cm-accent)" stroke-width="2" marker-end="url(#tm-ah)"/>
    <rect x="540" y="146" width="200" height="54" rx="8" fill="color-mix(in srgb, var(--cm-accent) 12%, transparent)" stroke="var(--cm-accent)"/>
    <text class="cm-svg-label" x="640" y="169" text-anchor="middle" style="fill:var(--cm-accent)">tview loop</text>
    <text class="cm-svg-sub" x="640" y="186" text-anchor="middle">sole screen owner</text>
    <line x1="640" y1="94" x2="640" y2="140" stroke="var(--cm-accent)" stroke-width="2" marker-end="url(#tm-ah)"/>
    <rect class="cm-svg-box" x="540" y="250" width="200" height="54" rx="8"/>
    <text class="cm-svg-label" x="640" y="273" text-anchor="middle">drawn frame</text>
    <text class="cm-svg-sub" x="640" y="290" text-anchor="middle">alt-screen, dev tty</text>
    <line x1="640" y1="200" x2="640" y2="244" stroke="var(--cm-accent)" stroke-width="2" marker-end="url(#tm-ah)"/>
    <rect class="cm-svg-box" x="280" y="250" width="200" height="54" rx="8"/>
    <text class="cm-svg-label" x="380" y="273" text-anchor="middle">stdout and stderr</text>
    <text class="cm-svg-sub" x="380" y="290" text-anchor="middle">answers to stdout</text>
    <line x1="380" y1="94" x2="380" y2="244" stroke="var(--cm-accent)" stroke-width="2" marker-end="url(#tm-ah)"/>
    <text class="cm-svg-sub" x="150" y="170" text-anchor="middle">every append is synchronous:</text>
    <text class="cm-svg-sub" x="150" y="185" text-anchor="middle">the run goroutine waits for the</text>
    <text class="cm-svg-sub" x="150" y="200" text-anchor="middle">frame, so an event storm</text>
    <text class="cm-svg-sub" x="150" y="215" text-anchor="middle">throttles the agent, not memory</text>
  </svg>
  <figcaption>The line UI writes directly. The full-screen UI marshals every mutation onto the loop, which owns the screen.</figcaption>
</figure>

The tview loop runs on the caller's goroutine. The agent runs on a spawned goroutine with its own recover, and reaches the
screen only through the queue.

Because the queue waits on a per-call done channel, every append is synchronous: the run goroutine blocks until the loop
has applied the closure and drawn. That is the back-pressure mechanism. An event storm throttles the agent to the draw
rate instead of growing an unbounded queue.

A live run has seven goroutines: the tview loop, the agent, a spinner ticker, a signal watcher, a teardown coordinator, a
stderr copier, and any notice-expiry timer.

Four invariants keep a stopped loop from turning into a deadlock or a panic:

- The loop is never stopped while the run goroutine can still queue, because teardown waits for the run to return first.
- The ticker is stopped and joined before the run is marked ended, closing the race window before the loop stops.
- Notice timers are always cancelled before a stop.
- Flags that cannot go through the loop are atomics: whether the spinner should animate, and whether an explicit leave was
  requested.

The screen wrapper makes finalization idempotent, because tview finalizes on stop and again from its own panic recovery,
while the viewer defers one of its own.

The one deliberate exception to the queue rule is the initial status refresh, called directly before the loop starts,
since queueing onto a loop that has not started would block forever.

## The widget layout

```text
pages: main | help | search | prompt | splash
  main: Flex(row)
    header  TextView   1 row, only when non-empty
    view    TextView   flex, dynamic colors, regions, scrollable, wrap
    promptTop / promptInput / promptBottom   inserted only in chat mode
    status  TextView   1 row, filled bar
```

Overlays are centered pages built from nested flexes. The splash page is added last so it sits on top of everything else.

Three parallel slices stay index-aligned one to one: the raw lines, their sanitized plain text, and their rendered
markup. A folded line still renders inside its search region, so a match still lands on it and the slices do not shift.
Search walks the plain text, which is also what the copy key sends, so search stays authoritative over folding and
revealing a match unfolds rather than skips.

Nothing renders until the first draw knows the width. A resize re-renders everything, which also re-picks tool-call
elision and re-wraps markdown.

Folding is a global mode rather than per-block state, because a flat text view has no cursor. Tool output folds at any
size, since the raw result is rarely worth reading inline. Thinking folds only past a six-row threshold, because a short
block saves nothing folded. Row counting hard-wraps at the current width, so a single very long JSON or base64 line counts
as the many rows it actually paints.

## One-shot and chat

The TUI gate requires that the TUI is not disabled by flag or config and that both stdin and stdout are terminals. Config
`no_tui` is an absolute veto; the flag can only turn it off.

Chat mode is TUI-only and fails loudly otherwise, with a distinct message for the resume case. A resumed session restores
its own chat-ness, peeked from the stored metadata before the run, so `--chat` need not be re-passed.

Chat mode also starts with thinking folded, on the theory that a chat leans on the answers with reasoning a keystroke
away.

The input row grows to five rows, lowered further so the transcript keeps at least three, with an overflow marker
embedded in its bottom rule. Bracketed paste stays enabled so a multi-line paste reaches the text area verbatim instead
of arriving as raw Enter keys that the key capture would read as a submit at the first newline.

Growing the input row re-pins the tail, because it steals height with no key pressed.

## Confirmation prompts

The full-screen prompter shows a three-button modal with the safe option as button zero, so the escape key's no-selection
result maps to the safe default. The default-deny policy itself lives in the gate, not in the prompter.

While a prompt page is front, every key goes to the widget except the abort key, so an abort is always possible while a
prompt is up.

Blocking recolors the status bar amber, sets a word describing the state, drops any lingering notice so it cannot mask
the bar, and rings the bell when enabled. Clearing the blocked state only reverts from blocked, so a terminal state set
by teardown after a cancelled prompt is not overwritten.

## The two-step markup defense

{{% notice style="warning" title="Load-bearing decision" %}}
Every piece of model text follows the same order: sanitize for display, then escape for tview, then apply trusted tags.
Escaping before translating ANSI is what stops model output from injecting markup. The same order is used for the
prompter body, border titles, splash values, the status bar, and the header. Bracketed literals in trusted text are
escaped too, because the bars have dynamic colors enabled.
{{% /notice %}}

Sanitization comes in two strengths. The display form strips escape and control sequences while preserving newlines and
tabs, and does not truncate. The terminal form collapses to one line and caps runes, and is what command lines and
model-supplied keys go through.

## Two markdown renderers

The split is deliberate.

<dl class="cm-kv">
  <dt>Line UI</dt><dd>Sanitizes first, so a raw escape in the model's prose cannot set a style that outlives the message. Returns raw markdown when color is off or when the target is not a terminal, so redirected output is ANSI-free. Otherwise renders with automatic style matching the terminal background.</dd>
  <dt>Full-screen UI</dt><dd>Never inspects a terminal. It forces an explicit style and color profile so nothing queries the tty, which the UI owns while its screen is held. A minimum width floor exists because below it the word wrap produces mangled output.</dd>
</dl>

## What no-color does and does not do

`--no-color` affects markdown only. The per-kind line tags, the blue, green, and amber status bar, the splash accent, and
the gray fold placeholders are not suppressed by it.

The design compensates by keeping information in words rather than color. The status bar carries a state word so the
information survives on a monochrome terminal, fold placeholders say how many lines are hidden and which key expands
them, and an errored tool result is prefixed with a word so the failure reads without the color.

The full-screen UI binds its screen to the controlling terminal rather than stdout, so stdout stays free for the piped
answer.

## Suspend and abort

For a checkpointed run, the first leave key requests a graceful suspend and appends a notice. A second press aborts, as
does a leave on a non-checkpointed run, an already-ended run, or a run blocked on a prompt.

At the input bar the abort key aborts directly, because the loop-boundary suspend flag is useless there: the run is parked
gathering a prompt, not in the loop, so it never polls the flag. The graceful leave at the input bar is the end-of-input
key instead.

Outcome classification uses the run's real result rather than operator intent. A run counts as suspended only if it ended
without error and the suspend flag is set, because the run may have completed at the very boundary the operator asked to
suspend at.

An explicit leave skips the "press q to quit" park entirely. The operator already asked to go, so the answer, statistics,
and any resume hint are reprinted to the restored terminal immediately.

## Surviving the alternate screen

Standard error is redirected into a buffer for the run's duration, so an SDK, a library, or a deferred write cannot draw
on the alternate screen, and the buffer is flushed to the restored terminal afterwards. Nothing is lost, only deferred.
That is also why HTTP debug output goes to a file rather than stderr.

The full-screen event sink hoards the answer, the warnings, the rotated session ids, and any panic value and stack, and
the command reprints them after teardown: warnings and hints to stderr, the answer through the markdown renderer to
stdout, so the run stays pipe-compatible.

The panic event deliberately writes nothing into the view, because it fires during unwind and races the teardown.

The terminal is always restored. The run defers the restore and then the screen finalization, so the pair unwinds in the
right order even through a panic.

Live counters must agree with the end-of-run summary, so the live view sums per-message usage exactly as the runner sums
its statistics, and a resume seeds the bar from the restored counters.

Elapsed time is deliberately absent from the bar: it kept climbing through idle input waits, which read as odd. The
summary still reports real latency.

## The trace file

`--trace` installs a middleware that writes JSON Lines. The file is created exclusively at mode 0600, so an existing path
is an error rather than a clobber, and the permissions match the fact that it holds full prompts and responses.

| Event | Contents |
|-------|----------|
| `session` | Written once: model, config path, version |
| `request` | Id, iteration, attempt, method, URL, body |
| `response` | Same id, status, duration, body |
| `error` | Same id, duration, error text; the error still propagates |
| `summary` | Session, counters, all four token tiers, duration |

A request and its response share an id. The iteration comes from the agent loop through the context, and the attempt is
parsed from the SDK's retry-count header, so a retry reuses the iteration but gets a new id and an incremented attempt.

Bodies are read non-destructively, with the response buffered and restored even on a read error. A valid JSON body is
embedded raw and anything else is wrapped as a JSON string, so a non-JSON body can never corrupt the one-object-per-line
format.

Writes never abort the run. The first failure warns once through an injected sink, which exists because `util` is a
dependency of the agent rather than the other way around. Every method is nil-safe, so the run path wires the tracer
unconditionally.

The per-kind tool counts are omitted on a resume, so a partial map can never look like it should sum.

## Reserved and unused

- Cache-creation tokens are tracked but never shown on the live bar, so a resumed run's counters stay whole without
  crowding the compact bar. They do reach the trace summary and the verbose statistics line.
- The full-screen sink deliberately drops the no-application notice, with a comment saying the TUI has no place for
  incidental notes yet and to restore it once a logs pane exists. A logs pane is the named future work.
- The line UI's session-rotated handler is a defensive fallback, since chat runs in the full-screen UI.
- Two slash-command handlers ignore their argument parameter; the signature exists only to fit the command table.
- The tool-error line kind has no dedicated fold toggle. The tool-output toggle governs it, though the fold notice only
  ever names thinking or output.
- The splash card is a single opaque text view rather than nested flexes, because a flex leaves its background unfilled
  and the transcript bleeds through the gaps. Its width is fixed because it cannot be resized from the before-draw hook,
  which runs under the application lock where hiding a page would deadlock.
- Clipboard copy is fire-and-forget over the terminal escape, and the notice wording says what was sent rather than that
  it landed, because many terminals disable or cap it.

{{% notice style="tip" title="Next" %}}
[Reference and map]({{% relref "reference" %}}) lists the command surface, the packages, and the vocabulary in one place.
{{% /notice %}}
