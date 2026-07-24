# The agent loop

The agent loop is where a configuration becomes behavior. It calls the model, executes the tools the model asks for,
feeds the results back, and stops. Nearly all of the interesting design work is in what it refuses to do: run a
truncated tool call, spend past a budget, suspend mid-tool, or let one tool source shadow another.

{{% notice style="note" title="Where it lives" %}}
`internal/agent` owns the run. Key files: `agent.go` for setup, `runner.go` for the loop, `hooks.go` for the extension
points, `events.go` for the display sink, `slog_events.go` for a structured sink. The package doc states the boundary
explicitly: it owns no CLI concerns, so flags, signals, and terminal rendering stay with the caller.
{{% /notice %}}

## Two halves

`agent.go` is setup. It resolves every tool source, dials at most one NATS connection, builds the provider, computes the
resume fingerprint, opens or creates a session, and constructs a private `runner`.

`runner.go` is the loop. The `runner` struct is deliberately split into two groups of fields, and that split is the
reason resume works at all:

<dl class="cm-kv">
  <dt>infrastructure</dt><dd>Rebuilt from configuration on every start or resume: provider, system prompt, tool definitions, budgets, confirm gate, prompter, events sink.</dd>
  <dt>run state</dt><dd>What a snapshot has to carry: <code>messages</code>, journal sequence, iteration counters, pending tool calls, session id, and the deferred-reset flags.</dd>
</dl>

Everything the model sees is rebuilt from the config file. Only the conversation itself is restored. That is what makes a
resumed run verifiable rather than merely restarted.

## Startup order is a safety property

<ol class="cm-steps">
  <li><b>Register the panic barrier first</b> Deferred functions run last-in-first-out, so registering the barrier before the journal, store, and tracer closers means it also catches a panic in cleanup.</li>
  <li><b>Validate caller-supplied directories</b> <code>ToolWorkDir</code> and <code>StoreDir</code> must be absolute and must exist, and the error names the option.</li>
  <li><b>Load application tools</b> <code>fisk.LoadTools</code> introspects the binary, strips <code>ai:deny</code>, then applies include and exclude.</li>
  <li><b>Claim names into one set</b> Every later source must claim into <code>taken</code>. A clash aborts the run.</li>
  <li><b>Add the built-ins</b> Human-in-the-loop tools first, then memory, then knowledge. All agent-mode only.</li>
  <li><b>Dial NATS at most once</b> Decided per subsystem, after resolving whether the run is checkpointed at all.</li>
  <li><b>Import remote tools, strictly</b> An unreachable named agent aborts the run.</li>
  <li><b>Register custom tools last</b> So a collision message can name the clashing kind.</li>
  <li><b>Require at least one callable tool</b> With a different message depending on whether <code>application_path</code> is set.</li>
  <li><b>Build the confirm gate and warn about dead tags</b> Every configured confirm tag matching no loaded tool is reported.</li>
</ol>

{{% notice style="warning" title="Load-bearing decision" %}}
Tool names form one flat namespace across application, built-in, remote, and custom tools, because the model addresses
every tool by a single flat name. A collision aborts the run rather than silently shadowing. The rationale in
`internal/agent/agent.go:644` is security, not tidiness: shadowing a confirm-gated command would strip its gate. Any
change that makes a collision non-fatal reopens that hole.
{{% /notice %}}

Two warnings exist purely because silence would be misleading. `WarnConfirmNoTerminal` fires when gated tools exist but
nothing can prompt. `WarnConfirmTagUnmatched` fires for each configured tag that matches no loaded tool, because, in the
source's own words, left unreported it gives a false sense of safety.

The NATS dial is a single connection shared by JetStream memory, JetStream sessions, and remote tools. The session gate
also checks whether the run is checkpointed at all, so a run that never writes a session does not fail on NATS it would
never use.

## One iteration

<figure class="cm-diagram">
  <svg viewBox="0 0 760 430" role="img" aria-label="One iteration of the agent loop from suspend poll through tool execution and back">
    <defs>
      <marker id="al-ah" markerWidth="9" markerHeight="9" refX="7" refY="3" orient="auto"><path d="M0,0 L7,3 L0,6 Z" fill="var(--cm-accent)"/></marker>
      <marker id="al-ad" markerWidth="9" markerHeight="9" refX="7" refY="3" orient="auto"><path d="M0,0 L7,3 L0,6 Z" fill="var(--cm-accent3)"/></marker>
      <marker id="al-af" markerWidth="9" markerHeight="9" refX="7" refY="3" orient="auto"><path d="M0,0 L7,3 L0,6 Z" fill="var(--cm-faint)"/></marker>
    </defs>
    <rect x="290" y="18" width="180" height="46" rx="8" fill="color-mix(in srgb, var(--cm-accent) 12%, transparent)" stroke="var(--cm-accent)"/>
    <text class="cm-svg-label" x="380" y="38" text-anchor="middle" style="fill:var(--cm-accent)">suspend poll</text>
    <text class="cm-svg-sub" x="380" y="54" text-anchor="middle">the only such point</text>
    <rect class="cm-svg-box" x="290" y="96" width="180" height="46" rx="8"/>
    <text class="cm-svg-label" x="380" y="116" text-anchor="middle">provider.Call</text>
    <text class="cm-svg-sub" x="380" y="132" text-anchor="middle">under call_timeout</text>
    <rect class="cm-svg-box" x="290" y="174" width="180" height="46" rx="8"/>
    <text class="cm-svg-label" x="380" y="194" text-anchor="middle">journal turn</text>
    <text class="cm-svg-sub" x="380" y="210" text-anchor="middle">before any tool runs</text>
    <line x1="380" y1="64" x2="380" y2="90" stroke="var(--cm-accent)" stroke-width="2" marker-end="url(#al-ah)"/>
    <line x1="380" y1="142" x2="380" y2="168" stroke="var(--cm-accent)" stroke-width="2" marker-end="url(#al-ah)"/>
    <!-- decision -->
    <path d="M380,244 L474,290 L380,336 L286,290 Z" fill="color-mix(in srgb, var(--cm-accent) 8%, transparent)" stroke="var(--cm-accent)"/>
    <text class="cm-svg-label" x="380" y="295" text-anchor="middle" style="fill:var(--cm-accent)">tool_use?</text>
    <line x1="380" y1="220" x2="380" y2="238" stroke="var(--cm-accent)" stroke-width="2" marker-end="url(#al-ah)"/>
    <!-- terminal branch -->
    <rect class="cm-svg-box" x="20" y="266" width="200" height="48" rx="8"/>
    <text class="cm-svg-label" x="120" y="288" text-anchor="middle">terminal turn</text>
    <text class="cm-svg-sub" x="120" y="304" text-anchor="middle">completed, or refusal</text>
    <line x1="286" y1="290" x2="226" y2="290" stroke="var(--cm-accent)" stroke-width="2" marker-end="url(#al-ah)"/>
    <text class="cm-svg-sub" x="256" y="282" text-anchor="middle">no</text>
    <!-- budget branch -->
    <rect x="540" y="266" width="200" height="48" rx="8" fill="color-mix(in srgb, var(--cm-accent3) 10%, transparent)" stroke="var(--cm-accent3)"/>
    <text class="cm-svg-label" x="640" y="288" text-anchor="middle" style="fill:var(--cm-accent3)">budget check</text>
    <text class="cm-svg-sub" x="640" y="304" text-anchor="middle">stop before tool spend</text>
    <line x1="474" y1="290" x2="534" y2="290" stroke="var(--cm-accent)" stroke-width="2" marker-end="url(#al-ah)"/>
    <text class="cm-svg-sub" x="504" y="282" text-anchor="middle">yes</text>
    <!-- execute -->
    <rect class="cm-svg-box" x="290" y="366" width="200" height="48" rx="8"/>
    <text class="cm-svg-label" x="390" y="388" text-anchor="middle">execute tool batch</text>
    <text class="cm-svg-sub" x="390" y="404" text-anchor="middle">journal each result</text>
    <path d="M640,314 L640,390 L496,390" fill="none" stroke="var(--cm-accent)" stroke-width="2" marker-end="url(#al-ah)"/>
    <!-- loop back -->
    <path d="M290,390 L250,390 L250,41 L284,41" fill="none" stroke="var(--cm-faint)" stroke-width="2" stroke-dasharray="5 4" marker-end="url(#al-af)"/>
    <text class="cm-svg-sub" x="150" y="360" text-anchor="middle">while iter &lt; max_iterations</text>
  </svg>
  <figcaption>The assistant turn is journaled before any tool runs, and the budget is checked after the terminal branch but before tool execution.</figcaption>
</figure>

Three placements in that flow carry most of the weight.

**The suspend poll is at the top, and nowhere else.** It runs at a boundary where the conversation is coherent and
nothing is in flight, and it runs before the iteration index is consumed, so a suspend does not burn an iteration number.
A run that is not checkpointed has no suspend function wired at all, so it cannot suspend.

**The assistant turn is journaled before any tool executes.** A crash partway through a batch therefore resumes without
re-paying for the model call that produced it.

**The budget is checked after the terminal branch and before tool execution.** A terminal answer is delivered regardless
of remaining budget, since no further spend follows it. An over-budget turn that wanted tools stops without incurring
their side effects.

### Why a truncated reply is an error

A reply that stopped at the output token cap may carry a partial `tool_use` block whose input is incomplete. Executing it
would mean running malformed arguments. The loop treats `StopMaxTokens` as `ReasonError` with an explicit message rather
than running the call or silently reporting completion.

The `terminal` flag is computed with a `StopMaxTokens` term that is a no-op on every path past the truncation branch. It
exists only so that `PostModelCall`, which observes every reply including a truncated one, sees the correct value.

### Terminal reasons

| Reason | Cause | Continues a chat? |
|--------|-------|-------------------|
| `ReasonCompleted` | A terminal turn with no tool calls | yes |
| `ReasonMaxIterations` | The per-run iteration cap was reached | yes |
| `ReasonError` | Provider failure, hook error, refusal, or truncation | yes |
| `ReasonBudget` | The token budget was exhausted | no |
| `ReasonSuspended` | A graceful suspend was requested | no |

A transient model failure must not stall an interactive session, which is why `ReasonError` is continuable. An aborted
context is filtered out separately by the loop condition rather than by the reason.

## Tool dispatch

Dispatch is an ordered pipeline, and the order is the safety argument.

<figure class="cm-diagram">
  <svg viewBox="0 0 760 480" role="img" aria-label="The tool dispatch pipeline with its four exit points">
    <defs>
      <marker id="td-ah" markerWidth="9" markerHeight="9" refX="7" refY="3" orient="auto"><path d="M0,0 L7,3 L0,6 Z" fill="var(--cm-accent)"/></marker>
      <marker id="td-ad" markerWidth="9" markerHeight="9" refX="7" refY="3" orient="auto"><path d="M0,0 L7,3 L0,6 Z" fill="var(--cm-accent3)"/></marker>
    </defs>
    <rect class="cm-svg-box" x="260" y="16" width="220" height="42" rx="8"/>
    <text class="cm-svg-label" x="370" y="42" text-anchor="middle">registry lookup</text>
    <rect class="cm-svg-box" x="260" y="72" width="220" height="42" rx="8"/>
    <text class="cm-svg-label" x="370" y="98" text-anchor="middle">PreToolUse hook</text>
    <rect class="cm-svg-box" x="260" y="128" width="220" height="42" rx="8"/>
    <text class="cm-svg-label" x="370" y="154" text-anchor="middle">resolve rewrite</text>
    <rect class="cm-svg-box" x="260" y="184" width="220" height="42" rx="8"/>
    <text class="cm-svg-label" x="370" y="210" text-anchor="middle">count by kind</text>
    <rect class="cm-svg-box" x="260" y="240" width="220" height="42" rx="8"/>
    <text class="cm-svg-label" x="370" y="266" text-anchor="middle">validate arguments</text>
    <rect x="260" y="296" width="220" height="42" rx="8" fill="color-mix(in srgb, var(--cm-accent) 14%, transparent)" stroke="var(--cm-accent)"/>
    <text class="cm-svg-label" x="370" y="322" text-anchor="middle" style="fill:var(--cm-accent)">confirm gate</text>
    <rect class="cm-svg-box" x="260" y="352" width="220" height="42" rx="8"/>
    <text class="cm-svg-label" x="370" y="378" text-anchor="middle">ExecuteUse</text>
    <rect class="cm-svg-box" x="260" y="408" width="220" height="42" rx="8"/>
    <text class="cm-svg-label" x="370" y="434" text-anchor="middle">PostToolUse hook</text>
    <line x1="370" y1="58" x2="370" y2="66" stroke="var(--cm-accent)" stroke-width="2" marker-end="url(#td-ah)"/>
    <line x1="370" y1="114" x2="370" y2="122" stroke="var(--cm-accent)" stroke-width="2" marker-end="url(#td-ah)"/>
    <line x1="370" y1="170" x2="370" y2="178" stroke="var(--cm-accent)" stroke-width="2" marker-end="url(#td-ah)"/>
    <line x1="370" y1="226" x2="370" y2="234" stroke="var(--cm-accent)" stroke-width="2" marker-end="url(#td-ah)"/>
    <line x1="370" y1="282" x2="370" y2="290" stroke="var(--cm-accent)" stroke-width="2" marker-end="url(#td-ah)"/>
    <line x1="370" y1="338" x2="370" y2="346" stroke="var(--cm-accent)" stroke-width="2" marker-end="url(#td-ah)"/>
    <line x1="370" y1="394" x2="370" y2="402" stroke="var(--cm-accent)" stroke-width="2" marker-end="url(#td-ah)"/>
    <!-- exits -->
    <rect x="530" y="16" width="210" height="42" rx="8" fill="color-mix(in srgb, var(--cm-accent3) 10%, transparent)" stroke="var(--cm-accent3)"/>
    <text class="cm-svg-label" x="635" y="42" text-anchor="middle" style="fill:var(--cm-accent3)">WarnUnknownTool</text>
    <line x1="480" y1="37" x2="524" y2="37" stroke="var(--cm-accent3)" stroke-width="2" stroke-dasharray="4 3" marker-end="url(#td-ad)"/>
    <rect x="530" y="72" width="210" height="42" rx="8" fill="color-mix(in srgb, var(--cm-accent3) 10%, transparent)" stroke="var(--cm-accent3)"/>
    <text class="cm-svg-label" x="635" y="98" text-anchor="middle" style="fill:var(--cm-accent3)">deny result</text>
    <line x1="480" y1="93" x2="524" y2="93" stroke="var(--cm-accent3)" stroke-width="2" stroke-dasharray="4 3" marker-end="url(#td-ad)"/>
    <rect x="530" y="240" width="210" height="42" rx="8" fill="color-mix(in srgb, var(--cm-accent3) 10%, transparent)" stroke="var(--cm-accent3)"/>
    <text class="cm-svg-label" x="635" y="266" text-anchor="middle" style="fill:var(--cm-accent3)">WarnMissingRequired</text>
    <line x1="480" y1="261" x2="524" y2="261" stroke="var(--cm-accent3)" stroke-width="2" stroke-dasharray="4 3" marker-end="url(#td-ad)"/>
    <rect x="530" y="296" width="210" height="42" rx="8" fill="color-mix(in srgb, var(--cm-accent3) 10%, transparent)" stroke="var(--cm-accent3)"/>
    <text class="cm-svg-label" x="635" y="322" text-anchor="middle" style="fill:var(--cm-accent3)">ConfirmDeniedResult</text>
    <line x1="480" y1="317" x2="524" y2="317" stroke="var(--cm-accent3)" stroke-width="2" stroke-dasharray="4 3" marker-end="url(#td-ad)"/>
    <text class="cm-svg-sub" x="130" y="205" text-anchor="middle">counted once, after</text>
    <text class="cm-svg-sub" x="130" y="220" text-anchor="middle">rewrite is resolved</text>
    <text class="cm-svg-sub" x="130" y="315" text-anchor="middle">gated on the union of</text>
    <text class="cm-svg-sub" x="130" y="330" text-anchor="middle">original and effective</text>
  </svg>
  <figcaption>Four exits return a result to the model without running the tool. None of them is silent.</figcaption>
</figure>

The details that matter:

- **The original call is described before any mutation.** `PreToolUse` sees exactly what the model asked for, and a
  rewrite can still be gated against the original.
- **A denied call is counted under the original kind, once.** The accounting invariant is that every call counted in
  `ToolCalls` is also counted in `ToolCallsByKind`, including rejected ones, so on a fresh run the buckets partition the
  total.
- **A rewrite that names an unregistered tool, or supplies invalid JSON, aborts the run** rather than dispatching a
  malformed call. `RewriteInput` replaces the whole argument object, so `{}` clears every argument. The hook does not
  re-fire on the rewritten call.
- **Argument validation runs before the gate**, so an operator is never asked to approve a structurally invalid call. The
  outcome is a warning, not a call-and-result pair.
- **The gate fires on `origGated || effGated`.** This is the point of the whole ordering.

{{% notice style="warning" title="Load-bearing decision" %}}
The confirm gate is evaluated on the union of the original and the effective tool, so a hook cannot strip a gate by
redirecting a gated call to an ungated tool. The operator is shown the effective command, because that is what actually
runs, but the trigger tag falls back to the original tool's when the effective tool is not `Confirmable`. See
[Tools and introspection]({{% relref "tools" %}}) for the gate itself.
{{% /notice %}}

A denial returns `util.ConfirmDeniedResult`, which is deliberately not an error result. The model treats it as
authoritative rather than as something to retry.

Tools describe themselves rather than having the runner switch on concrete types. A tool that does not implement
`Describer` degrades safely: unknown kind, name-only trace, no dependencies, not remote. Dependencies follow
least privilege, so a kind receives only what it asked for through `NeedsPrompter` and `NeedsWorkDir`.

## The panic barrier

The barrier is registered first so it unwinds last, which means it also covers a panic during cleanup. Its scope is
documented honestly: it catches only the run goroutine, and it cannot catch a fatal runtime error such as a concurrent
map write, an out-of-memory kill, or `runtime.Goexit`. It is not a substitute for process isolation.

There are three nested recovers, each for a different reason.

1. The barrier itself.
2. An inner recover around `Events.Panicked`, so a misbehaving sink cannot crash the process the barrier just protected.
3. `fireSessionEnd` has its own recover, because by the time it runs the barrier's recover has already completed.

The stack trace never crosses a trust boundary. `PanicError` carries a fixed generic message and exposes only the
recovered value; the stack goes to `Events.Panicked` alone, since it leaks absolute paths and frame arguments. The stack
is captured before any caller code runs, while the panicking frames are still live, because `SessionEnd` may itself
panic.

## Hooks

Eight optional callbacks, all on the single run goroutine, in loop order. A nil hook is a no-op. There is one callback
per point; composing several behaviors means wrapping them in one function.

| Hook | Fires | Can change | On failure |
|------|-------|-----------|------------|
| `SessionStart` | Once, before any session is created or opened | nothing | aborts the run, leaving no orphan session |
| `UserPromptSubmit` | Initial prompt, and each interactive follow-up | `Deny` with a reason | initial deny ends the run; a follow-up deny reopens the input |
| `PreModelCall` | Before each `provider.Call` | nothing, counts only | `ReasonError` |
| `PostModelCall` | After every reply, including a truncated one | nothing, gets a deep copy | `ReasonError`, but not durable across resume |
| `PreToolUse` | Ahead of validation, gate, trace, and execution | deny, or rewrite the tool or its input | aborts the run |
| `PostToolUse` | After execution, before trace and journal | replace the output and error flag | aborts the run |
| `TurnEnd` | At each interactive continuation boundary | nothing | `ReasonError` |
| `SessionEnd` | Inside the panic barrier, after everything is closed | nothing | downgraded to a warning |

The contract bounds hook power deliberately: a hook may observe, terminate, or adjust tool data. It never injects
prompts, continues or extends a turn, or changes token or tool accounting, budgets, or iteration caps.

Payloads are snapshot-isolated so a mutating hook cannot corrupt the live conversation. Tool input arrives as a
`bytes.Clone`, the model reply as a JSON round-trip deep copy built only when a hook is actually set, and the per-kind
tool counts as a `maps.Clone`.

Lifecycle facts worth knowing:

- `SessionStart` fires before the initial `UserPromptSubmit`.
- A resume fires `SessionStart` with `Resumed: true` and does not re-fire `UserPromptSubmit` for reconstructed history.
- A context reset does not re-fire `SessionStart`. That rotation is reported through `Events.SessionRotated`.
- `SessionEnd` fires from exactly one place, exactly once, for every run that reached the runner, whether it completed,
  ran out of budget, suspended, errored, or crashed. A setup failure before the runner exists never fires it.
- On a crash, `Err` is nil and `Reason` is unset, so a hook must key off `Crashed`.
- `PreToolUse` is the only reliable way to block a tool durably, because `PostModelCall` runs after the turn is already
  journaled.
- `PreToolUse` and `PreModelCall` sit above the provider, so they fire even for an injected provider, unlike an
  `llm.Middleware`.

`PostToolUseResult.Replace` is an explicit boolean rather than an empty-value sentinel, because an empty output is a
valid replacement.

## Interactive continuation

A nil `NextPrompt` makes the run one-shot. Otherwise the outer loop gathers a `Continuation` at each boundary.

`Continuation` has three fields. `Continue` false ends the session. `Reset` clears the conversation but keeps the system
prompt, the tools, and the confirm gate's approvals. `Reset` with empty text reopens the input without running a turn.

The follow-up `UserPromptSubmit` hook fires before any reset, rotation, append, or journal write, so a denial reopens the
input without clearing context or journaling a rejected turn. The stale terminal reason is reset so the previous
advisory does not fire twice.

A reset on a checkpointed run is deferred rather than immediate. Clearing the conversation would leave an empty prompt in
the session metadata, which would fail to resume, so the reset waits for the next real prompt and then rotates to a new
session. If rotation fails the reset is abandoned and the turn continues in the current session, which stays consistent
because the messages were never cleared.

Each accepted follow-up adds the configured iteration budget to the cap, so one long turn cannot starve the next.

A journal write failure at a follow-up boundary ends the session before the turn runs, so the journal stops at the last
coherent boundary rather than recording a turn whose result was never written.

## What the loop does not promise

Being explicit about this is part of the design.

- `ToolWorkDir` is collision avoidance, not confinement. It sets a per-run working directory; it sandboxes nothing.
- `CustomTools` run in process with the agent's own privileges and an unscrubbed environment.
- An injected `Provider` bypasses the registry, so its credentials are not in `llm.CredentialEnvNames` and are therefore
  not scrubbed from tool subprocesses. Middlewares, including HTTP debug and tracing, are also not applied to it.
- `RunStats.ToolCallsByKind` is live-only. It stays nil-seeded across a resume, so the buckets partition the total only
  on a fresh run.
- A confirm-denied or missing-argument result is journaled with `remote=false` regardless of the tool's actual kind.
- The token budget is soft in two ways. It over-counts by design, summing uncached input, cache reads, cache writes, and
  output, and it is checked after each call, so a single call can overshoot.

## Reserved and unused

- `llm.Caps.MaxOutputTokens` is declared but read nowhere. `resolveMaxOutputTokens` uses two package constants and the
  config override only, so a provider's declared ceiling does not currently clamp the per-call cap.
- `SlogEvents` has no in-tree consumer outside its own test. It exists for a server or job runner that does not yet live
  in this repository, and its doc comment prescribes `NewSlogEvents(base.With("run_id", id), verbose)` as the attribution
  pattern.
- The shared-`Events` contract for concurrent runs is explicitly deferred to whenever a job system arrives.
- Most of `Options` is embedder-only. The CLI sets fewer than half its fields; `Provider`, `ToolWorkDir`, `StoreDir`,
  `Conns`, `RAGStore`, `MemoryStore`, `SessionStore`, `A2ATransport`, `CustomTools`, and `Hooks` are exercised only by
  tests today.
- `resumeHazards` returns a slice but has one hazard kind, and `Warning.Params` is used by one warning.
- Only `StopMaxTokens`, `StopPauseTurn`, and `StopRefusal` are branched on. A `stop_sequence` reply with no tool call
  therefore reads as a completed answer.

{{% notice style="tip" title="Next" %}}
[Tools and introspection]({{% relref "tools" %}}) covers where the tools the loop dispatches come from, and what the tag
system does to them.
{{% /notice %}}
