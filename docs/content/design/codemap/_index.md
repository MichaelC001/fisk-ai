+++
title = "Code Map"
description = "A guided deep-dive into the codebase: architecture, subsystems, and the decisions behind them"
toc = true
weight = 10
+++

Fisk AI turns a Fisk command-line application into an LLM agent by introspecting its command tree and exposing the
allowed commands as tools. This section is a reading guide to how that is implemented, aimed at contributors, reviewers,
and anyone auditing the harness before trusting it with a production tool.

{{% notice style="note" title="Snapshot" %}}
Generated 2026-07-24 against commit `561dade` on branch `main`. The working tree carried one untracked file unrelated to
the source. Commits after this one may make parts of this map stale.
{{% /notice %}}

## The mental model

There is one core and several faces. A Fisk application is introspected once into a set of tools; a YAML file narrows
that set and decides what needs approval; and then the same selection is either driven by a model in an agent loop,
served to an MCP client, or served to other agents over NATS. Three durable stores sit underneath, and none of them is
part of the conversation the model sees until the harness decides to put it there.

<figure class="cm-diagram">
  <svg viewBox="0 0 760 350" role="img" aria-label="One tool selection driving three faces over three durable stores">
    <defs>
      <marker id="ov-ah" markerWidth="9" markerHeight="9" refX="7" refY="3" orient="auto"><path d="M0,0 L7,3 L0,6 Z" fill="var(--cm-accent)"/></marker>
    </defs>
    <rect class="cm-svg-box" x="20" y="45" width="180" height="50" rx="8"/>
    <text class="cm-svg-label" x="110" y="67" text-anchor="middle">fisk CLI app</text>
    <text class="cm-svg-sub" x="110" y="84" text-anchor="middle">introspected once</text>
    <rect class="cm-svg-box" x="20" y="140" width="180" height="50" rx="8"/>
    <text class="cm-svg-label" x="110" y="162" text-anchor="middle">agent.yaml</text>
    <text class="cm-svg-sub" x="110" y="179" text-anchor="middle">selects and gates</text>
    <path d="M200,70 L225,70 L225,95 L244,95" fill="none" stroke="var(--cm-accent)" stroke-width="2" marker-end="url(#ov-ah)"/>
    <path d="M200,165 L225,165 L225,140 L244,140" fill="none" stroke="var(--cm-accent)" stroke-width="2" marker-end="url(#ov-ah)"/>
    <rect x="250" y="60" width="230" height="110" rx="10" fill="color-mix(in srgb, var(--cm-accent) 14%, transparent)" stroke="var(--cm-accent)"/>
    <text class="cm-svg-label" x="365" y="98" text-anchor="middle" style="fill:var(--cm-accent)">tool set and loop</text>
    <text class="cm-svg-sub" x="365" y="120" text-anchor="middle">one flat namespace</text>
    <text class="cm-svg-sub" x="365" y="138" text-anchor="middle">gated, bounded, journaled</text>
    <rect class="cm-svg-box" x="560" y="30" width="180" height="50" rx="8"/>
    <text class="cm-svg-label" x="650" y="52" text-anchor="middle">run</text>
    <text class="cm-svg-sub" x="650" y="69" text-anchor="middle">terminal, human gate</text>
    <rect class="cm-svg-box" x="560" y="100" width="180" height="50" rx="8"/>
    <text class="cm-svg-label" x="650" y="122" text-anchor="middle">mcp</text>
    <text class="cm-svg-sub" x="650" y="139" text-anchor="middle">http clients</text>
    <rect class="cm-svg-box" x="560" y="170" width="180" height="50" rx="8"/>
    <text class="cm-svg-label" x="650" y="192" text-anchor="middle">a2a</text>
    <text class="cm-svg-sub" x="650" y="209" text-anchor="middle">other agents</text>
    <path d="M480,95 L520,95 L520,55 L554,55" fill="none" stroke="var(--cm-accent)" stroke-width="2" marker-end="url(#ov-ah)"/>
    <path d="M480,115 L520,115 L520,125 L554,125" fill="none" stroke="var(--cm-accent)" stroke-width="2" marker-end="url(#ov-ah)"/>
    <path d="M480,135 L520,135 L520,195 L554,195" fill="none" stroke="var(--cm-accent)" stroke-width="2" marker-end="url(#ov-ah)"/>
    <line x1="365" y1="170" x2="365" y2="245" stroke="var(--cm-faint)" stroke-width="2"/>
    <line x1="225" y1="245" x2="605" y2="245" stroke="var(--cm-faint)" stroke-width="2"/>
    <line x1="225" y1="245" x2="225" y2="264" stroke="var(--cm-faint)" stroke-width="2" marker-end="url(#ov-ah)"/>
    <line x1="415" y1="245" x2="415" y2="264" stroke="var(--cm-faint)" stroke-width="2" marker-end="url(#ov-ah)"/>
    <line x1="605" y1="245" x2="605" y2="264" stroke="var(--cm-faint)" stroke-width="2" marker-end="url(#ov-ah)"/>
    <rect class="cm-svg-box" x="140" y="270" width="170" height="50" rx="8"/>
    <text class="cm-svg-label" x="225" y="292" text-anchor="middle">memory</text>
    <text class="cm-svg-sub" x="225" y="309" text-anchor="middle">model-written notes</text>
    <rect class="cm-svg-box" x="330" y="270" width="170" height="50" rx="8"/>
    <text class="cm-svg-label" x="415" y="292" text-anchor="middle">knowledge</text>
    <text class="cm-svg-sub" x="415" y="309" text-anchor="middle">operator-owned docs</text>
    <rect class="cm-svg-box" x="520" y="270" width="170" height="50" rx="8"/>
    <text class="cm-svg-label" x="605" y="292" text-anchor="middle">sessions</text>
    <text class="cm-svg-sub" x="605" y="309" text-anchor="middle">append-only journal</text>
    <text class="cm-svg-sub" x="380" y="342" text-anchor="middle">the same selection drives all three faces; only the confirmation policy differs</text>
  </svg>
  <figcaption>One core, three faces, three stores. What changes between the faces is whether a human can be reached.</figcaption>
</figure>

## What the design optimizes for

The project describes itself by what it does not have. Reading the source, three commitments show up repeatedly and
explain most of the structure.

**Nothing is silently weakened.** A configured confirm tag that matches no tool is warned about, because leaving it
unreported would give a false sense of safety. A tool-name collision aborts the run rather than shadowing, because
shadowing a gated command would strip its gate. A tag-based exclude on a remote host is rejected outright, because
discovery carries no tags and the filter could never be honored.

**Failures land at startup.** An unknown backend, a typo in an options block, a bucket with a TTL, an unreachable remote
agent, a stale knowledge manifest: all of them stop the process before the model is contacted, and the error names the
fix.

**Untrusted text stays data.** Model-written memories and retrieved documents are wrapped, labeled as data rather than
instruction, sanitized at write time, and sanitized again at render time.

## How to read this section

Start with [Architecture]({{% relref "architecture" %}}) for the layering and the patterns that repeat, then
[Configuration]({{% relref "configuration" %}}), since every entry point begins by parsing a file. From there,
[The agent loop]({{% relref "agent-loop" %}}) and [Tools and introspection]({{% relref "tools" %}}) are the core, and the
rest can be read in any order.

Each page names real files and symbols, states the invariants the safety story depends on, and is explicit about what is
reserved, unused, or aspirational rather than presenting the whole tree as finished.

## Explore

{{% children description="true" %}}
