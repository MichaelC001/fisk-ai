# Knowledge

Knowledge gives an agent one tool, `knowledge_search`, over a locally built index of its own markdown. The design
constraint that shapes everything else is that the whole feature ships inside the single Fisk AI binary: no C toolchain
at build time, no shared library at runtime, and no external database.

{{% notice style="note" title="Where it lives" %}}
`internal/rag` holds the store. Key files: `store.go` for the handle and schema, `index.go` for the write path,
`search.go` for the read path, `chunk.go` for chunking, `embed.go` for the embeddings client, `watch.go`, `doctor.go`.
The tool is in `internal/toolkit/builtin/builtin_rag.go`, and the CLI in `rag_command.go`.

The YAML key and user-facing noun are `knowledge`; the Go identifiers keep the `rag` prefix. Knowledge is the feature,
RAG is the technique.
{{% /notice %}}

## Why pure-Go SQLite is load-bearing

`store.go` blank-imports two packages: the `modernc.org/sqlite` driver and its `vec` subpackage, which registers the
`vec0` virtual-table module. Both come from one module that is a transpilation of upstream SQLite and sqlite-vec into Go.

{{% notice style="warning" title="Load-bearing decision" %}}
Because there is no cgo, `CGO_ENABLED=0` static builds and cross-compilation keep working, and sqlite-vec is linked in
rather than loaded as an extension. That removes an entire class of problems: no extension path to configure, no
`enable_load_extension`, and no version skew between binary and extension. Both tiers link in regardless of
configuration, so there is no build-tag matrix and no separate "vector build". A lexical-only deployment still links
`vec0`; it simply never creates the table.
{{% /notice %}}

FTS5 is compiled in for the same reason, which makes the `verifyFTS5` check a defensive guard against a surprising build
rather than an expected failure. It is nonetheless the one fatal check the doctor reports.

## Indexing

<ol class="cm-steps">
  <li><b>Open a writer</b> Build the embedder from config with no network call, create the directory at 0700, take the advisory lock, create the database at 0600 with no-follow, and enforce permissions on the database and both WAL sidecars.</li>
  <li><b>Preview the cost</b> On a first build or a reindex with the vector tier on, a dry pass reports how many chunks across how many files are about to be embedded. Nothing is embedded yet.</li>
  <li><b>Prepare the vector tier</b> Probe the embedding dimension once, reconcile against the stored manifest, and only then create the vector table and its delete trigger. This runs before any embedding spend.</li>
  <li><b>Walk each root</b> Checking the context between files so an interrupt is prompt across CPU-bound chunking.</li>
  <li><b>Classify by content hash</b> No row means add, an equal hash means skip, a different hash means update.</li>
  <li><b>Chunk, embed, then commit</b> The network call happens outside the transaction; only the upsert, purge, and insert are transactional.</li>
  <li><b>Reconcile orphans</b> Only on a full-corpus walk, and only when the walk saw at least one file.</li>
</ol>

The walk skips the store's own directory, any dotdir, and a sibling `memory` directory, through a predicate shared with
the watcher so the two agree on what the corpus is. Symlinks are skipped entirely, files over 512 KiB are skipped with a
note, and non-UTF-8 files are skipped.

An interrupted index is not an error. Already-embedded files are committed and skipped on the next run.

An equal-hash skip still adds the existing chunk count to the statistics, so the totals stay truthful. A reindex
deliberately bypasses the equal-hash short-circuit so a dry-run reindex reports the real work.

{{% notice style="warning" title="Load-bearing decision" %}}
Reconcile is double-guarded: deletion happens only on a full-corpus walk and only when that walk saw at least one file. A
walk that errored early can therefore never wipe the index.
{{% /notice %}}

## Chunking

The chunker is heading-aware and size-packed, and it is pure: no database, no IO.

- A **fenced code block** is collected whole and marked indivisible. Code is never split, even past the maximum chunk
  size, because splitting code hurts retrieval more than an oversized chunk does.
- A **heading** flushes the current section, updates the breadcrumb stack, and starts a fresh packer. The heading line
  itself is not added to the body; the breadcrumb carries it.
- A **paragraph** is the contiguous run of non-blank lines up to the next blank line, fence, or heading, which is what
  keeps a markdown table's rows together.

Blocks accumulate to a 1200-byte target and a 1500-byte maximum, and a divisible block over the maximum is hard-split.
Each finished chunk folds its breadcrumb into the body, so the section title travels into both the embedding and the
lexical index.

The chunk ordinal is its index within the document. That is exactly why citations shift when a file is edited.

## The schema

```sql
documents(id, path UNIQUE, title, mtime, hash)
chunks(id, document_id REFERENCES documents ON DELETE CASCADE, heading_path, ordinal, content)
chunks_fts  VIRTUAL USING fts5(content, heading_path,
                               content='chunks', content_rowid='id',
                               tokenize='porter unicode61')
chunks_vec  VIRTUAL USING vec0(chunk_id INTEGER PRIMARY KEY, embedding FLOAT[<dim>])
rag_meta(key PRIMARY KEY, value)
```

`chunks_fts` is an external-content table, so chunk text is stored exactly once and FTS5 holds only the index.
`heading_path` gets its own FTS column because a section title is often the most search-relevant phrase in a chunk.

Foreign keys are on for every connection, so deleting a document cascades to chunks and, through triggers, to both the
full-text and vector indexes. The vector table is created only when the vector tier is on and only once the dimension is
known, so a lexical-only index has no vector table at all.

## The pinned vector identity

`rag_meta` holds a manifest: format version, model, dimension, whether vectors are normalized, and both prefixes.
Together those are the vector identity, and mismatches are hard failures rather than degradations.

The invariant is enforced in three places for three reasons. On the write path before any embedding spend, so a changed
model does not cost money before it is refused. At open time for everything except dimension, so opening never contacts
the embeddings server. At query time for dimension only, against a live probe.

Adding the vector tier to a non-empty lexical-only index is also refused, rather than silently searching lexically
forever after an operator asked for hybrid.

Every mismatch message ends by naming the fix: reindex. A stale manifest yields garbage rankings, which is worse than a
refusal.

Normalization lives in exactly one place. The package L2-normalizes every vector regardless of which embedder produced
it, which is what lets the manifest pin normalization to true and lets the default L2 distance stand in for cosine
similarity.

## Search

<figure class="cm-diagram">
  <svg viewBox="0 0 760 330" role="img" aria-label="Hybrid search fusing BM25 and vector results by rank">
    <defs>
      <marker id="kn-ah" markerWidth="9" markerHeight="9" refX="7" refY="3" orient="auto"><path d="M0,0 L7,3 L0,6 Z" fill="var(--cm-accent)"/></marker>
    </defs>
    <rect class="cm-svg-box" x="20" y="143" width="130" height="54" rx="8"/>
    <text class="cm-svg-label" x="85" y="166" text-anchor="middle">query</text>
    <text class="cm-svg-sub" x="85" y="183" text-anchor="middle">sanitized</text>
    <rect x="230" y="50" width="210" height="54" rx="8" fill="color-mix(in srgb, var(--cm-accent) 12%, transparent)" stroke="var(--cm-accent)"/>
    <text class="cm-svg-label" x="335" y="73" text-anchor="middle" style="fill:var(--cm-accent)">FTS5 BM25</text>
    <text class="cm-svg-sub" x="335" y="90" text-anchor="middle">always on, fanout 50</text>
    <rect x="230" y="236" width="210" height="54" rx="8" fill="color-mix(in srgb, var(--cm-accent2) 14%, transparent)" stroke="var(--cm-accent2)"/>
    <text class="cm-svg-label" x="335" y="259" text-anchor="middle" style="fill:var(--cm-accent2)">vector KNN</text>
    <text class="cm-svg-sub" x="335" y="276" text-anchor="middle">optional, fanout 50</text>
    <path d="M150,170 L190,170 L190,77 L224,77" fill="none" stroke="var(--cm-accent)" stroke-width="2" marker-end="url(#kn-ah)"/>
    <path d="M150,170 L190,170 L190,263 L224,263" fill="none" stroke="var(--cm-accent)" stroke-width="2" marker-end="url(#kn-ah)"/>
    <rect x="520" y="143" width="200" height="54" rx="8" fill="color-mix(in srgb, var(--cm-accent) 12%, transparent)" stroke="var(--cm-accent)"/>
    <text class="cm-svg-label" x="620" y="166" text-anchor="middle" style="fill:var(--cm-accent)">RRF fusion</text>
    <text class="cm-svg-sub" x="620" y="183" text-anchor="middle">by rank, not by score</text>
    <path d="M440,77 L480,77 L480,170 L514,170" fill="none" stroke="var(--cm-accent)" stroke-width="2" marker-end="url(#kn-ah)"/>
    <path d="M440,263 L480,263 L480,170 L514,170" fill="none" stroke="var(--cm-accent)" stroke-width="2" marker-end="url(#kn-ah)"/>
    <text class="cm-svg-sub" x="620" y="222" text-anchor="middle">truncate to top_k, max 20</text>
    <text class="cm-svg-sub" x="620" y="237" text-anchor="middle">then hydrate into citations</text>
    <text class="cm-svg-sub" x="335" y="316" text-anchor="middle">an unreachable embeddings server degrades to lexical, and always says so</text>
  </svg>
  <figcaption>Fusion is on rank, so BM25 scores and L2 distances never have to be normalized against each other.</figcaption>
</figure>

The query is split on non-alphanumeric runs, terms under two runes are dropped, each term is double-quoted with embedded
quotes doubled, the list is clamped to 40 terms, and they are joined with `OR`. The quoting is both a syntax fix and an
injection guard. The `OR` is deliberate: the lexical tier supplies recall and the vector tier supplies precision.

Both retrievers over-fetch 50 results before truncation to at most 20, so a chunk that is strong in one list and deep in
the other still gets its rank boost before the cut.

Hydration is a single join re-ordered to the fused order. A row that vanished mid-flight, because a reindex was running,
is skipped rather than failing the search.

When the vector tier is off or degraded, no fusion runs at all and the result is straight BM25 order.

The active tier is never ambiguous. One canonical tier line is rendered by every CLI subcommand and included in the
tool's JSON, and a degraded query renders a distinct line naming the reason.

## Soft states and hard failures

The split is consistent and worth stating plainly.

| Situation | Behavior |
|-----------|----------|
| No index file yet | A store opens with a nil database; searches report a status, not an error, so a first agent run still starts |
| Index exists but is empty | A status, not an error |
| Embeddings server unreachable at query time | Degrade to lexical, set the reason, and report it |
| Embeddings dimension differs from the manifest | A hard error |
| Model or prefixes differ from the manifest | A hard error naming the reindex |
| Embeddings server unreachable at index time | A hard error, because that is when spend was about to happen |

A silent lexical fallback would be indistinguishable from "vectors did not help", which is why the reason is always
surfaced.

## Embedding

The client is hand-rolled against an OpenAI-compatible endpoint, batching 64 inputs at a time. On any batch failure it
binary-splits down to single inputs while preserving order, so a server with a lower batch cap still succeeds.

Vectors are placed by each response object's index field, and the batch fails on an out-of-range index, a duplicate
index, an empty vector, or a count mismatch. That is what guarantees a vector can never land on the wrong chunk. An
error-shaped body returned with HTTP 200 is also treated as a failure.

Inputs are validated before sending: non-empty, valid UTF-8, and under 128 KiB. Responses are read through a 64 MiB
limit. A query is truncated to 8192 characters, backing off any partial trailing rune.

The dimension probe runs once per process under a mutex, because one embedder is shared across concurrent MCP calls, and
it refuses to cache an empty result so a broken server never pins a bogus dimension.

A non-loopback base URL must be https, so a query is never sent in cleartext. The API key is configured as the name of an
environment variable, never as the secret.

## Citations

The canonical form is `<relpath>#<ordinal>`, produced by one function and set during hydration. The path is exactly as
relative or absolute as the root that was indexed.

`knowledge show` parses a citation back and resolves it, and a miss produces a purpose-written hint that citations shift
after a reindex.

## Watching

The watcher prints its tier line from a read-only store first, deliberately, so it never contends with the writer locks
it is about to take.

Missing or unreadable roots are warned about and dropped, and it errors only if none remain. Every eligible directory is
watched, with a file root watched through its parent, and the same skip rules as the index walk apply. A watch-limit
error is translated into a single actionable warning about the inotify limit and the watcher keeps going.

The event loop is a single-timer debounce with a 100 ms floor. Indexing runs in its own goroutine so a long pass never
blocks event draining, which would overflow the kernel queue, and a dirty flag coalesces changes arriving mid-pass into
exactly one follow-up pass. A lock held by a competing writer becomes a warning and a retry.

Events under the store's own directory are rejected outright, which is what breaks the WAL feedback loop.

Deletions are applied stat-guarded: if the path exists again, the delete is skipped. That is what makes an editor's
atomic-save rename harmless.

One subtlety worth knowing: a reactive pass is not a targeted per-file ingest. It re-walks every root and relies on the
content-hash skip for cheapness. The collected event paths are used only for reporting and for the guarded deletes.

## Locking and platform splits

Two independent mechanisms solve two different problems. A single open connection serializes writes within the process.
An advisory file lock serializes writers across processes, which the connection limit cannot do because WAL lets multiple
processes open the same file.

On Unix the lock is a non-blocking `flock`, and a busy lock fails fast rather than interleaving writes under a timeout.
Because it is an flock, the operating system releases it if the process dies, so a crashed writer never wedges future
indexing. On Windows it is an exclusive-create lock file, and the comment states the trade-off plainly: it does not
auto-release on crash, so a stale lock may need removing by hand.

No-follow open is used at exactly two sites, the database and the lock file, so a symlink planted at either path cannot
redirect a write. Permissions are enforced on the database and both WAL sidecars, with any symlink among them refused,
because SQLite creates the sidecars honoring the umask and could leave them world-readable.

Reader connections are opened read-only by the driver and additionally set query-only as defense in depth.

## Performance choices

- Embedding happens outside the transaction, so the slow network call never holds the single writer slot.
- Hash-based incremental indexing means unchanged files cost nothing, which is also what makes an interrupted index cheap
  to resume and the watcher's full re-walk affordable.
- Purge-then-insert per document covers first ingest and update in one path, and the triggers clear ghost chunks when a
  file shrinks.
- The reader pool is kept tiny and short-lived so no pooled connection pins a WAL snapshot long enough to block
  checkpointing and grow the WAL unbounded across a long session.
- Auto-vacuum returns freed pages to the operating system after a removal or reindex. It only takes effect on a freshly
  created empty database, which is precisely why the writer must create the file itself and why that pragma is ordered
  before the journal mode.
- Everything is bounded: 512 KiB source files, 1200 and 1500 byte chunks, 40 query terms, 8192-character queries, 128 KiB
  embed inputs, 64 MiB responses, at most 20 results, and a default 6000-token injection budget.

The tool's own budget converts injected tokens to characters at four per token and stops adding hits once the budget
would be exceeded, but always includes at least the first hit so a large first chunk is not silently dropped to nothing.

## The doctor

`fisk knowledge doctor` renders the tier line as its header and then runs a series of checks, of which exactly one is
fatal: FTS5 compiled in. Store presence, journal mode, index writability, each configured path, and the embeddings checks
are all reported without failing the command.

The policy is deliberate: an absent or unreachable embeddings server is never fatal, so a lexical-only operator is never
told their setup is broken.

## Store directory resolution is a documented footgun

The directory is `knowledge.directory` or, by default, `knowledge/<identity>`. A relative result is rebased under the
store directory when one is set, and an absolute one is honored verbatim.

The agent and the CLI must pass the same base or they resolve to different directories. That is why `--store-dir` and
`FISK_AI_STORE_DIR` exist and are documented as distinct from `--state-dir`, and why the agent raises
`WarnKnowledgeIndexAbsent` when a base was given but no index is there.

## Security posture

Text is stored unencrypted at mode 0600 inside a 0700 directory, the same posture as memory. The package documentation
says outright not to index secrets.

Retrieved text is framed to the model as untrusted reference data rather than instructions, in the system note and in the
tool description alike. The model-supplied query is sanitized before it reaches an operator's terminal.

## What is deliberately absent

Stating the scope matters as much as stating the features. Hybrid here means BM25 plus KNN plus reciprocal rank fusion,
and nothing else:

- No cross-encoder re-ranking.
- No diversification or per-document capping.
- No neighborhood expansion around a hit.
- No query expansion or rewriting.
- No snippet highlighting; the CLI simply truncates the line.
- No relevance threshold. Scores are internal and never exposed in the tool JSON or the CLI, and truncation to `top_k` is
  the only gate.
- Embedding is per file rather than per chunk. Any content change re-embeds every chunk of that file; there is no
  chunk-level embedding cache.

## Reserved and unused

- `IndexOptions.Extensions` is a real extension point that no caller sets, so the indexable extension set is not
  configurable from YAML or flags today.
- `documents.title` is computed and written on every upsert and read on no path at all. Chunk-level heading paths do all
  the work.
- `IndexStats.FirstBuild` is computed and read by nobody; both first-build previews check the document count themselves.
- `Hit.ChunkID` is populated and never read outside the package.
- The format version has a refusal for anything newer but no migration path.
- `Meta.Normalized` is always written true and any index with it false is a mismatch. It documents the invariant rather
  than selecting a behavior.
- `Store.VectorEnabled()` and `Store.Dir()` have no in-repository callers.
- The search status enum has no degraded member; degradation is a separate flag and the status stays fine.

{{% notice style="tip" title="Next" %}}
[Serving: MCP and A2A]({{% relref "serving" %}}) covers `knowledge_search` in its other role, as the only built-in a
Fisk AI process will hand to an outside client.
{{% /notice %}}
