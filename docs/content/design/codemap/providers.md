+++
title = "Model providers"
description = "A provider-neutral message model that doubles as the on-disk format, and one package allowed to speak an SDK"
toc = true
weight = 60
+++

`internal/llm` is the domain model for talking to a model, and it imports no vendor SDK anywhere. Exactly one package,
`internal/llm/anthropic`, translates that model to and from the Anthropic SDK. Everything else in the tree, including the
durable journal, speaks the neutral types.

{{% notice style="note" title="Where it lives" %}}
`internal/llm` holds the contracts. Key files: `types.go` for the message model, `provider.go` for the interface,
`request.go`, `response.go`, `middleware.go`, `registry.go`. `internal/llm/anthropic` holds the only provider:
`provider.go`, `codec.go`, `tools.go`. `internal/llm/README.md` is the normative contract document and the best source for
the reasoning.
{{% /notice %}}

<figure class="cm-diagram">
  <svg viewBox="0 0 760 350" role="img" aria-label="The neutral message model and the single package allowed to speak an SDK">
    <defs>
      <marker id="pv-ah" markerWidth="9" markerHeight="9" refX="7" refY="3" orient="auto"><path d="M0,0 L7,3 L0,6 Z" fill="var(--cm-accent)"/></marker>
    </defs>
    <line x1="482" y1="12" x2="482" y2="304" stroke="var(--cm-accent3)" stroke-width="2" stroke-dasharray="6 5"/>
    <text class="cm-svg-sub" x="625" y="330" text-anchor="middle" style="fill:var(--cm-accent3)">an SDK is spoken only right of this line</text>
    <rect class="cm-svg-box" x="20" y="30" width="170" height="56" rx="8"/>
    <text class="cm-svg-label" x="105" y="54" text-anchor="middle">agent loop</text>
    <text class="cm-svg-sub" x="105" y="71" text-anchor="middle">builds one request</text>
    <rect x="250" y="30" width="200" height="56" rx="8" fill="color-mix(in srgb, var(--cm-accent) 12%, transparent)" stroke="var(--cm-accent)"/>
    <text class="cm-svg-label" x="350" y="54" text-anchor="middle" style="fill:var(--cm-accent)">llm.Request</text>
    <text class="cm-svg-sub" x="350" y="71" text-anchor="middle">provider-neutral</text>
    <rect class="cm-svg-box" x="510" y="30" width="230" height="56" rx="8"/>
    <text class="cm-svg-label" x="625" y="54" text-anchor="middle">anthropic codec</text>
    <text class="cm-svg-sub" x="625" y="71" text-anchor="middle">the only SDK importer</text>
    <line x1="190" y1="58" x2="244" y2="58" stroke="var(--cm-accent)" stroke-width="2" marker-end="url(#pv-ah)"/>
    <line x1="450" y1="58" x2="504" y2="58" stroke="var(--cm-accent)" stroke-width="2" marker-end="url(#pv-ah)"/>
    <rect class="cm-svg-box" x="510" y="146" width="230" height="56" rx="8"/>
    <text class="cm-svg-label" x="625" y="170" text-anchor="middle">middlewares</text>
    <text class="cm-svg-sub" x="625" y="187" text-anchor="middle">http debug, then tracer</text>
    <line x1="625" y1="86" x2="625" y2="140" stroke="var(--cm-accent)" stroke-width="2" marker-end="url(#pv-ah)"/>
    <rect class="cm-svg-box" x="510" y="248" width="230" height="56" rx="8"/>
    <text class="cm-svg-label" x="625" y="272" text-anchor="middle">Messages API</text>
    <text class="cm-svg-sub" x="625" y="289" text-anchor="middle">no streaming path</text>
    <line x1="625" y1="202" x2="625" y2="242" stroke="var(--cm-accent)" stroke-width="2" marker-end="url(#pv-ah)"/>
    <rect x="250" y="248" width="200" height="56" rx="8" fill="color-mix(in srgb, var(--cm-accent) 12%, transparent)" stroke="var(--cm-accent)"/>
    <text class="cm-svg-label" x="350" y="272" text-anchor="middle" style="fill:var(--cm-accent)">runstate journal</text>
    <text class="cm-svg-sub" x="350" y="289" text-anchor="middle">stores llm.Message</text>
    <line x1="350" y1="86" x2="350" y2="242" stroke="var(--cm-accent)" stroke-width="2" marker-end="url(#pv-ah)"/>
  </svg>
  <figcaption>The neutral model is not only an abstraction over vendors. It is also the durable format, which is why lossless round-tripping is a hard requirement.</figcaption>
</figure>

## The interface

```go
type Provider interface {
	Call(ctx context.Context, req Request) (*Response, error)
	Capabilities() Caps
}
```

`Call` owns the wire call end to end, including the per-call timeout. `Caps` reports the neutral provider id, whether
tool search is supported, and a declared maximum output.

Capabilities are declared rather than discovered, because neither Anthropic nor OpenAI expose capability flags at
runtime. The set is deliberately minimal and grows when a second provider makes a real difference concrete rather than
predicted.

## The neutral model is also the on-disk format

`runstate` records store `llm.Message` and `llm.ToolResultBlock` directly. An earlier version stored the Anthropic wire
format and did not round-trip, which is why the record version refuses both older and newer snapshots.

That makes lossless preservation a hard requirement rather than a nicety:

- `ThinkingBlock.Signature` is `[]byte`. The neutral model never inspects or renders it; it only preserves it. The model
  rejects a turn whose thinking signature was dropped or altered.
- `ToolUseBlock.Input` is `json.RawMessage`, so arguments survive byte-for-byte with no schema-shaped intermediate.
- `ProviderBlock{Kind, Raw}` is the escape hatch for a server-side block the neutral model does not name. `Kind` is the
  provider's own discriminator and `Raw` its faithful JSON.

A golden test asserts a byte-identical round-trip across thinking, redacted thinking, text, server tool use, web search
results, tool use, and tool results. That test is the tripwire for the whole scheme.

### One codec subtlety worth knowing

`providerBlockToNeutral` reads the type discriminator out of the marshaled JSON rather than calling `GetType()`, because
the SDK leaves each block's `Type` field at its zero value and fills the default only on marshal. A block with no
discriminator is an error rather than a silently untyped passthrough.

Coming back the other way, `repairToolSearchResult` rebuilds the entire content union from `Raw`. The SDK decoder drops
`tool_references` on a successful tool-search result and mis-selects the error variant, so a plain unmarshal would lose
data. The error variant already round-trips and is left alone.

A response is routed through the same block codec as a stored message, so a reply and a journaled turn share one
representation.

## The registry, and credentials as a boundary

Registration is by import side effect. The agent package blank-imports the provider, and a second provider is a second
blank import there.

```go
func Register(name string, factory Factory, credentialEnvNames []string)
```

The credential list is a required positional argument, not an option. A provider cannot be registered without declaring
its secrets.

{{% notice style="warning" title="Load-bearing decision" %}}
`llm.CredentialEnvNames()` is a security boundary, not documentation. Its union is stripped from the environment of every
model-chosen tool subprocess, and it covers every linked provider rather than only the active one. Selector variables
such as `ANTHROPIC_PROFILE` and `XDG_CONFIG_HOME` are deliberately excluded, since they hold no secret and stripping them
buys nothing a tool could not rediscover. An injected `agent.Options.Provider` bypasses the registry, so its credentials
are not in this union and are therefore not scrubbed.
{{% /notice %}}

`Register` panics on an empty name, a nil factory, or a duplicate, mirroring `database/sql.Register`. Each is a
programming error resolvable at compile time.

## The request

`Request` carries no client, no credential, and no timeout, so it is a plain value a test can build and assert on.

Two fields deserve a note. `SystemBlocks` is a slice rather than a string because Anthropic sends separate system blocks
and places the cache breakpoint on the last one; a string-system provider would join them. `Interactive` exists only to
select a longer cache TTL.

The SDK system slice is rebuilt fresh on every call, specifically so marking its last element for caching cannot write
through to any value the caller hashed into the run fingerprint. That has its own test.

## Tool declaration

`defer_loading` is emitted unconditionally, including a present `false`, so the rendered tool is a pure function of the
neutral value rather than varying with its zero state.

Schema rendering maps `properties` and `required` to dedicated SDK fields and forwards every other schema key verbatim
through `ExtraFields`, notably `additionalProperties`. The `type` key is dropped because the SDK fixes it to object.

Strict mode is deliberately not used: its grammar compilation caps the total optional parameters across all tools, which a
broad command tree exceeds.

Deferral itself is decided in `util.BuildToolParams` over the combined local, remote, custom, and built-in count against a
threshold of 10. It reports back whether anything actually deferred, so the tool-search tool is only requested when there
is something to find.

## Middleware

`Middleware` and `MiddlewareNext` are type aliases rather than defined types. That is what lets an `llm.Middleware` pass
straight into the SDK's `option.WithMiddleware` unchanged.

Middlewares are assembled in the agent, where their lifecycle lives, not in the provider. The SDK wraps from last to
first, so the first appended is outermost. With the current order the HTTP debug dump is outermost and the tracer sits
closest to the wire.

<dl class="cm-kv">
  <dt>HttpDebugMiddleware</dt><dd>Dumps the request via <code>GetBody</code> and the response by buffering and replacing it, so the SDK still parses normally. The sink is injectable so it cannot corrupt the full-screen UI.</dd>
  <dt>Tracer.Middleware</dt><dd>JSON-lines trace of every request, response, and error. Nil-safe, and tracing never changes the call's outcome.</dd>
</dl>

Middleware is HTTP-level and therefore invisible to an injected provider. The `PreModelCall` and `PostModelCall` hooks are
the provider-agnostic complement: they sit above the provider and fire either way.

## No streaming, and what compensates

There is no streaming path. `Call` uses the non-streaming Messages API only.

The accommodation is on the output cap: the thinking-mode default of 16384 stays within the non-streaming ceiling that
keeps responses clear of SDK HTTP timeouts. Streaming-shaped feedback is achieved at a coarser grain through per-turn
events and the verbose request summary.

## Retries

Retries are delegated entirely to the SDK default of two retries, three attempts. Two consequences follow.

The retry loop lives inside the middleware-wrapped handler's caller, so middlewares are re-entered per attempt. The
tracer relies on exactly that: it reads the SDK's retry-count header to set `attempt`, giving each retry a new trace id
while reusing the iteration number.

All attempts share the one per-call deadline, because `Call` wraps the context before dispatching. So
`llm.budget.call_timeout` bounds the attempt series, not each attempt.

`Call` adds one error-shaping rule: a 400 while thinking is enabled is annotated with the suggestion to set
`llm.thinking.enabled` to false, because older and compatibility models reject adaptive thinking.

## Token accounting

`Usage` has exactly four tiers: input, output, cache read, and cache create. The run sums all four, and the same four are
journaled on each assistant record so a resume can reseed the totals.

`InTokens` is the uncached remainder, which makes the summary line diagnostic. A healthy multi-iteration run shows a small
input count and a climbing cache-read count. A silent cache miss shows cache reads stuck at zero against a large input
count.

The budget check sums all four tiers, so the cap measures total throughput and keeps its magnitude comparable to the
pre-cache world.

## Local and compatible endpoints

Running against ollama, llama.cpp, or LM Studio is a first-class deployment, and four knobs exist mostly for it.

`--base-url` or `ANTHROPIC_BASE_URL` is applied only when non-empty, so the SDK default endpoint is used otherwise.
`util.ValidateBaseURL` requires http or https, rejects embedded userinfo, and requires https for any non-loopback host.
Plain http is allowed only for `127.0.0.1`, `::1`, and `localhost`, so a local server keeps working. It does not resolve
names, so a hostname that happens to resolve to loopback is not treated as loopback.

Validation runs twice on purpose: once at the CLI boundary, so a bad base URL fails on a normal terminal before the
HTTP debug file is created or the full-screen UI starts, and again inside `agent.Run`. That is what lets the provider
factory declare construction infallible.

<dl class="cm-kv">
  <dt>llm.no_tool_search</dt><dd>The manual complement to the capability flag. The flag says tool search is possible; this switch turns it off for an endpoint where it is possible but unwanted, such as a proxy that does not implement the tool-search tool.</dd>
  <dt>llm.no_prompt_cache</dt><dd>For a proxy that rejects or ignores <code>cache_control</code>. Disabling only raises cost; it never changes output.</dd>
  <dt>llm.budget.max_output_tokens</dt><dd>Set it to fit an endpoint whose per-response limit is below the default.</dd>
  <dt>llm.thinking.enabled</dt><dd>Off by default, because older and compatibility models reject adaptive thinking.</dd>
</dl>

A future `anthropic-compat` selector is anticipated, and the contract document is explicit that it must still report the
same provider id when the backend semantics are identical, so it does not break resumes.

## Provider identity gates a resume

`Capabilities().Provider` is stamped into the run fingerprint, and it is the resolved provider's own id rather than the
config selector. A provider change is a hard resume refusal that `--force` cannot cross, which is why `Provider` is
excluded from the fingerprint's `Equal` and `Diff`: those govern only forceable drift.

Prompt caching is deliberately outside the fingerprint, so toggling it never refuses a resume.

## Reserved and unused

- **Three exported codec functions have no non-test callers**: `ToolUseToNeutral`, `ToolResultToAnthropic`, and
  `ToolResultFromAnthropic`. Their comments reference boundaries that now use neutral types end to end. They are
  migration residue.
- **`Caps.MaxOutputTokens` is declared but never read.** Nothing clamps or validates the per-call cap against a
  provider's stated ceiling, and the Anthropic provider leaves it zero.
- **`Providers()` has no caller** beyond the unknown-provider error message.
- **The factory's error return is always nil today.** It exists for a future provider that can fail to construct.
- **`StopStopSequence` is never branched on.** A stop-sequence reply with no tool call reads as a completed answer.
- **Only one provider exists.** The declared next target is OpenAI, explicitly with no new SDK dependency, following the
  hand-rolled client in the rag embedder rather than taking on an SDK whose types would leak back through this layer. The
  known hard spots are enumerated in the README: per-tool-result messages versus Anthropic's batched synthetic user turn,
  a string system prompt which voids the cache-breakpoint mechanism, and a thinking round-trip that may need more than a
  single opaque field.
- **Credential selection is not yet provider-conditional.** `--api-key` is hardwired to `ANTHROPIC_API_KEY` and
  unconditionally required.
- **`internal/util/anthropic.go` is named for the provider but contains no SDK reference** and is fully neutral. The name
  is misleading rather than meaningful.
- **A latent trap, not a bug today**: `Call` unconditionally applies its timeout, so a provider built with a zero timeout
  would fail instantly. The run path always supplies a resolved value, and the one zero-timeout construction only reads
  capabilities offline.

{{% notice style="tip" title="Next" %}}
[Sessions and replay]({{% relref "state" %}}) covers what the journal does with these neutral messages, and why a
provider change is the one refusal `--force` cannot override.
{{% /notice %}}
