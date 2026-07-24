# Sessions and replay

A checkpointed run writes an append-only journal. Nothing about the run's configuration is stored: the fold reconstructs
the conversation, and everything else is rebuilt from the config file. That is what makes a resumed run verifiable rather
than merely restarted.

{{% notice style="note" title="Where it lives" %}}
`internal/runstate` holds the format and the fold. Key files: `record.go`, `state.go`, `fingerprint.go`, `store.go`,
`validate.go`, `schemas.go`. Backends are `internal/runstate/file` and `internal/runstate/jetstream`. The CLI surfaces
are `session_command.go` and `resume_replay.go`.
{{% /notice %}}

## Records

A record is a sequence number, a protocol id, and exactly one payload. Sequence starts at 1 on the meta record and is the
sole ordering authority.

The protocol ids sit under `io.choria.fisk-ai.v1.session.*`. The namespace constant is spelled out by hand rather than
imported from the A2A package, so storage does not depend on the protocol package, with a comment noting it must track
the other. The intent is stated plainly: the record stream maps one to one onto the durability journal today and onto the
A2A event stream later, so one model backs both local resume and remote streaming.

<figure class="cm-diagram">
  <svg viewBox="0 0 760 280" role="img" aria-label="Journal write order across one run, folded into a run state">
    <defs>
      <marker id="st-ah" markerWidth="9" markerHeight="9" refX="7" refY="3" orient="auto"><path d="M0,0 L7,3 L0,6 Z" fill="var(--cm-accent)"/></marker>
    </defs>
    <rect x="20" y="30" width="130" height="54" rx="8" fill="color-mix(in srgb, var(--cm-accent) 12%, transparent)" stroke="var(--cm-accent)"/>
    <text class="cm-svg-label" x="85" y="53" text-anchor="middle" style="fill:var(--cm-accent)">Meta</text>
    <text class="cm-svg-sub" x="85" y="70" text-anchor="middle">seq 1, fingerprint</text>
    <rect class="cm-svg-box" x="168" y="30" width="130" height="54" rx="8"/>
    <text class="cm-svg-label" x="233" y="53" text-anchor="middle">Assistant</text>
    <text class="cm-svg-sub" x="233" y="70" text-anchor="middle">before any tool</text>
    <rect class="cm-svg-box" x="315" y="30" width="130" height="54" rx="8"/>
    <text class="cm-svg-label" x="380" y="53" text-anchor="middle">ToolResult</text>
    <text class="cm-svg-sub" x="380" y="70" text-anchor="middle">one per tool</text>
    <rect class="cm-svg-box" x="462" y="30" width="130" height="54" rx="8"/>
    <text class="cm-svg-label" x="527" y="53" text-anchor="middle">User</text>
    <text class="cm-svg-sub" x="527" y="70" text-anchor="middle">chat follow-up</text>
    <rect class="cm-svg-box" x="610" y="30" width="130" height="54" rx="8"/>
    <text class="cm-svg-label" x="675" y="53" text-anchor="middle">Terminal</text>
    <text class="cm-svg-sub" x="675" y="70" text-anchor="middle">once, at the end</text>
    <line x1="150" y1="57" x2="162" y2="57" stroke="var(--cm-accent)" stroke-width="2" marker-end="url(#st-ah)"/>
    <line x1="298" y1="57" x2="309" y2="57" stroke="var(--cm-accent)" stroke-width="2" marker-end="url(#st-ah)"/>
    <line x1="445" y1="57" x2="456" y2="57" stroke="var(--cm-accent)" stroke-width="2" marker-end="url(#st-ah)"/>
    <line x1="592" y1="57" x2="604" y2="57" stroke="var(--cm-accent)" stroke-width="2" marker-end="url(#st-ah)"/>
    <path d="M20,104 L20,120 L740,120 L740,104" fill="none" stroke="var(--cm-faint)" stroke-width="2"/>
    <text class="cm-svg-sub" x="380" y="140" text-anchor="middle">strictly increasing seq, fsynced or write-once per subject</text>
    <line x1="380" y1="146" x2="380" y2="186" stroke="var(--cm-accent)" stroke-width="2" marker-end="url(#st-ah)"/>
    <rect x="250" y="192" width="260" height="54" rx="8" fill="color-mix(in srgb, var(--cm-accent) 12%, transparent)" stroke="var(--cm-accent)"/>
    <text class="cm-svg-label" x="380" y="215" text-anchor="middle" style="fill:var(--cm-accent)">Fold(records)</text>
    <text class="cm-svg-sub" x="380" y="232" text-anchor="middle">pure, no IO, derives everything</text>
    <text class="cm-svg-sub" x="130" y="219" text-anchor="middle">a crash loses at</text>
    <text class="cm-svg-sub" x="130" y="234" text-anchor="middle">most one tool result</text>
  </svg>
  <figcaption>The assistant turn becomes durable before any tool runs, and each tool result as it completes.</figcaption>
</figure>

## Write order is the durability argument

Every append goes through one helper that is a no-op when the journal is nil, so an unchecked run costs nothing.

- The **assistant record** is written before any tool executes, so a crash midway through a batch resumes without re-paying
  for that model call.
- **One tool-result record** is written as each tool completes, so a crash loses at most one tool.
- A **user record** is written only for a free-standing interactive follow-up. The initial prompt lives in the meta
  record and tool results in their own records. A journal failure here ends the session before the turn, so the journal
  stops at the last coherent boundary rather than recording assistant turns with no preceding user message.
- The **terminal record** is written once, at the true end, carrying the reason and any error text. A failed terminal
  write is a warning, not a failure.

`CheckAppend` decides whether an append is a duplicate, a gap, or valid, but the backend advances its own last-sequence
counter. The doc comment explicitly forbids folding the advance into the helper: the writer must advance only after the
record is durably stored, which is what makes a torn or failed write re-append the same sequence instead of losing it.

## The fold

`Fold` is pure and does no IO. Counters and the resume position are derived from the records rather than stored, so they
cannot drift from the recorded events.

Two invariants define the output:

<dl class="cm-kv">
  <dt>Messages</dt><dd>Always ends on a boundary the model API would accept. An in-flight tool batch never appears here.</dd>
  <dt>Pending</dt><dd>Holds the partly-answered assistant turn: the message, its iteration, its stop reason, the results so far, and the set of answered tool-use ids.</dd>
</dl>

Role alternation is reconstructed rather than stored. A user record holds only the newly typed blocks, never a merged
view, and the fold re-derives the same merge the live path performs. Recording the merged message instead would double
the tool-result blocks the fold already appended. A test asserts both paths reconstruct an identical conversation.

On resume, the loop first completes the pending turn: it reuses journaled results for answered tool-use ids, runs only the
unanswered ones, journals each, and then commits the assistant turn together with the full result set.

## The fingerprint

The fingerprint records the configuration the journal was written against: provider, model, a hash of the system prompt,
a hash of the tool set, thinking mode, max tokens, and max iterations.

Continuing a conversation against a changed configuration can be genuinely incoherent. A stored tool call may reference a
tool that no longer exists, or a thinking signature may be rejected outright. So the gates run in a fixed order.

| Gate | Behavior |
|------|----------|
| Already completed | Refused. Only `ReasonCompleted` blocks a resume |
| Interactivity mismatch | Refused in both directions, as defense in depth behind the CLI's own reconciliation |
| Provider changed | Refused unconditionally. `--force` does not apply |
| Anything else changed | Refused unless `--force`, with a per-field diff |

Provider is checked before drift so the message is unambiguous, and it is deliberately excluded from `Equal` and `Diff`
so those govern only forceable drift.

{{% notice style="warning" title="Load-bearing decision" %}}
The system prompt is only ever hashed, never stored verbatim, and there is a test named for exactly that: it must not leak
a sensitive prompt into the fingerprint on disk. Three things are deliberately outside the fingerprint so that data drift
never blocks a resume: the memory index, the resume reminder, and the prompt-cache setting. All three are appended after
the fingerprint is computed and none is persisted.
{{% /notice %}}

A resume seeds the sequence from the journal's last, the iteration from the recorded next, plus the pending turn, the
messages, and the six counters.

An interactive resume gets a fresh per-turn iteration budget from where it left off, because a chat's grown cap is not
stored, only the position. A one-shot resume keeps the absolute cumulative cap.

`resumeAtInputBoundary` skips the first loop entirely when the session is interactive, has no pending turn, ends on an
assistant message, and did not stop on a paused turn. The operator lands at the input bar rather than watching a
redundant model call.

A run resting on a paused-turn boundary raises `WarnResumePausedTurn`, since the server-side state it depends on may have
expired.

The resume reminder is advisory: it tells the model that tool results may be stale and to re-verify before taking
state-changing actions. It is not enforcement.

## Two backends

Both store byte-identical record bodies, so a run migrates between them unchanged. There is a test asserting the two fold
identically.

| | `file` | `jetstream` |
|---|--------|-------------|
| Unit | One line in one JSON-lines file | One message on one subject |
| Write-once | Append-only plus fsync | `MaxMsgsPerSubject=1` with discard-new-per-subject |
| Mutual exclusion | Advisory `flock` on a per-run lock file | Per-run tail fence on the expected last sequence |
| Retry safety | Last sequence advances only after fsync | A nonce message id lets a lost ack be adopted |
| Torn tail | Dropped as the final line only | Impossible; a message is atomic |
| Crash recovery | The kernel releases the flock | Nothing to release |
| Requires NATS | No | Yes |

The file backend syncs the line, and on the first write of a new journal also syncs the directory so the entry survives a
crash, and only then advances its counter. Reads drop an unparsable final line as a torn tail but treat an interior parse
failure as corruption, which is valid only because the file is append-only and fsynced.

The JetStream backend binds a stream and never creates one; the operator owns durability. The subject prefix is derived
from the stream's single wildcard subject rather than configured, and zero or multiple wildcards are construction
failures. `checkStreamConfig` rejects, at construction, a stream whose per-subject message limit is not 1, whose discard
policy is not discard-new per subject, that has any max age, or whose max message size is below the record floor. Each
error names the exact `nats stream` fix, and a missing stream produces a ready-to-run `nats stream add` line.

Its exclusion is not a lock. Appends are fenced against the expected last sequence for the run's subject, so a stale
writer is rejected rather than interleaving. A wrong-last-sequence error is disambiguated by reading the target subject
back: a matching message id means this writer's own ack was lost and the record can be adopted, and anything else is a
genuine conflict.

Non-Unix platforms get a stub file lock that does not exclude. The comment is explicit that those platforms rely on the
operator not resuming a run twice.

## Where a run is stored

Runs are never stored in the working directory, so state does not leak into repositories, and `DefaultDir` lives in the
core rather than the file backend precisely so that contract stays visible to every backend.

Sessions are also not namespaced by identity, unlike memory. A resume must find its run whatever identity is active, so
the registry passes no identity to a session backend at all.

`ValidateID` is both a format rule and a path-traversal defense: a single safe path component that is also a valid NATS
subject token, bounded to 128 characters so the same ids work in both backends without producing an oversized filename.
Both stores validate before an id becomes a key or a path, and listings skip ids that fail it.

`Load` reads and folds without locking; `Open` takes the lock for appending. Resume reads and gates with `Load`, then
takes the lock with `Open`, so inspection never blocks a running session. The `ErrLocked` message names neither a process
nor a run, because a flock is per open file description and a caller genuinely cannot tell which holder it hit.

## Version 3 is refused in both directions

A newer snapshot may carry record shapes this build cannot fold. An older one predates the provider-neutral record format
and does not round-trip. Refusing beats silently mis-folding, so neither is accepted.

## Replay

Three renderings share one folded state: live narration to stderr on resume, a plain-text dump for inspection, and
structured lines for the full-screen transcript viewer.

`transcriptLines` pairs each tool call with its result by tool-use id across the whole conversation, so calls and results
interleave as they did live. An unanswered call in a suspended turn simply has no result. All three walks handle the
pending turn explicitly after the committed messages, since the in-flight turn lives outside the conversation.

A read-only view renders tool calls without loading the tool registry, while the live resume path renders the resolved
command line when the tool is still known.

## Reserved and unused

- **The JSON schema validator is not wired into any write or read path.** Five schemas exist, are embedded, and are
  compiled, and nothing in production calls them; only tests do. It exists as the published contract for external and
  future consumers, and for parity with A2A, where the equivalent validator is on the live path.
- The message and result objects in those schemas are intentionally opaque, so provider-specific blocks round-trip
  verbatim without the schema constraining them.
- **`Journal.Records()` has no production caller.** The resume path uses the unlocked load followed by a locked open.
- **`Backends()` has no CLI surface**, despite its doc comment; it only feeds the unknown-backend error message.
- **`RunInfo.Created` is populated and never displayed.** `session ls` shows only the updated time.
- `SessionInteractive` double-dials NATS on a JetStream resume, once for the pre-flight meta read and once for the resume
  itself. That is accepted for simplicity.
- `RemoteToolCalls` and the per-record remote flag are counted and displayed, but resume logic does not act on them.
- `internal/agenttest.FakeSessionStore` is an in-memory implementation written in a separate package to prove the
  interface is implementable from outside. It reuses the shared id and append validation.

{{% notice style="tip" title="Next" %}}
[Memory]({{% relref "memory" %}}) covers the other durable store, and why it is deliberately kept out of the fingerprint.
{{% /notice %}}
