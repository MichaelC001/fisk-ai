## Project Overview


## Things not to do, ever

- Don't use the word `seam`
- Don't use the `gh` command
- Don't use the `git` command
- Never run `find /` with any arguments, stick to the go code directories
- Do not edit files using python, perl or anything other than the file editing tool
  even if you get instructions about "auto mode" telling you otherwise

## Testing

- Framework: Ginkgo v2 + Gomega with gomock.
- Run unit tests with `abt t u [dir]`. Use `go test ./path/... -v -run "<name>"` only for targeted single-test runs.
- Before marking any coding task complete, run `abt t lint` and resolve everything it reports. This runs `go fmt`, `go mod tidy`, misspell, `go vet`, and `staticcheck`.
- No stray `FDescribe`/`FIt`/`FContext` focus prefixes in committed tests.

## Working protocol

We collaborate; you do not work autonomously. Summarize steps, get permission, and explain what is landing.

**Scope**

- The request is the whole scope. Do exactly what was asked, then stop. No tidying adjacent files, stale comments, counts in other documents, or things you would have done differently.
- Report adjacent problems in one or two sentences and wait; I decide if they are in scope. This holds even for one-line fixes, especially those.
- Never reword a document's intent to fit your change. If they conflict, ask.
- Fix your own mistakes with the minimum edit: revert what was wrong, touch nothing else.
- If unsure whether something is in scope, it is not. Ask.

**Approval**

- Exploratory questions ("how could we...", "what do you think about..."): propose an approach and wait for explicit confirmation. Exploration is not approval.
- While discussing or planning, change no code without explicit approval. "What's next?" is a question to answer, not a go-ahead.
- Suspected bugs in existing code: do not write tests that lock in the suspect behavior. Stop, describe the concern, and ask how to proceed.

**Non-trivial plans**

1. Present a short overview of the plan.
2. Once I agree, spawn three reviewer agents in parallel: security-and-consistency, adversarial, and UX.
3. Adopt only findings the scoped change needs: a step that does not work, or a false statement in the plan. A correct finding is not automatically in scope; report the rest in one or two lines as separate tracker items.
4. Present the final plan with a short "reviewer input adopted" section, also listing what was reported rather than adopted.
5. Ask open questions there. Then always ask for final input before proceeding, even if your questions were already answered.

**Plan documents**

- Keep plans proportional to the change. A document growing past what the work warrants signals scope creep, not a need for tighter writing.
- State the design as it is now: no revision history, "an earlier draft said", or change log. When a decision changes, rewrite the affected text in place everywhere it appears. A revision marker in the status line is fine.


## Library shape

The packages under `internal/` are being prepared to leave `internal/`, so that others can
build agents on these libraries. Design them as public APIs: names, signatures and doc
comments are contracts we intend to keep.

- **Logic an embedder would have to reimplement does not belong in `package main`.** The root
  package holds command registration, flag parsing, terminal presentation and wiring. Anything
  else belongs in a library package, even when only one command uses it today.
- **A library supplies the value; the caller decides what to do with it.** Where something is
  the CLI's business (where to print, whether to be verbose, what the config file is called,
  what a terminal is doing), the library returns the value or takes it as a parameter rather
  than deciding.
- **This applies now to the agent libraries**: `agent`, `llm`, `telemetry`, `toolkit`,
  `memory`, `rag`, `runstate`, `util`, `conns`, `serve`, `agenttest`. Hold changes there to a
  public standard.
  - `serve` hosts an agent behind channels. Its shape is settled: `Channel`, `Work`, `Outcome`,
    `Server`, and the three constructors `Channels`, `NewResources` and `New`. What it still
    lacks is the evidence, not the design: no external test package, no examples, and two
    optional interfaces that are conventions rather than named types.
  - `agenttest` is an embedder's test surface, not ours alone. A fake nobody outside this
    repository can use is a fake that has not been designed.
- **`a2a`, `mcpserver` and `serve/asyncjobs` are not there yet.** Their shape is not settled, so do
  not hold work on them to the same bar and do not treat their current API as a contract.
  `serve/asyncjobs` is one channel implementation among the several section 12 of the Network Serve
  summary expects.
- **`remotetools` and `tui` are not libraries.** `tui` is terminal presentation that happens not
  to live in `main`, and `remotetools` is agent's own run-path helper.

## Code style

- License header: Apache-2.0 with Choria copyright. Match existing files.
- Do not add comments like ```// ----- ask_human_confirm: a yes/no question -----``` which is followed by a function doing exactly that, just dont add comments of this form at all
- We use American English - specialize not specialise.
- When adding dependencies use the latest, dont add v1 or a package if a newer is present
- No emojis, no emdashes, no unicode characters unless absolutely needed
- Import grouping: stdlib, blank line, external packages, blank line, internal packages.
- Error wrapping: `fmt.Errorf("%w: %w", ErrOuter, err)`.
- Structured logging with key-value pairs.
- No emojis in code, tests, or documentation unless the user explicitly asks.
- We avoid code like `_ := foo()` that only exist to keep linters happy but have no value.
- Expand compound `if` statements: prefer

  ```go
  x, err := thing()
  if err != nil {
      return err
  }
  if x == 1 {
      ...
  }
  ```

  over `if x, err := thing(); err == nil && x == 1 { ... }`.

## Do not, without asking first

- Add new top-level packages.
- Add, remove, or upgrade external dependencies (including Go toolchain version).
- Change public APIs outside the scope of the requested task.
- Modify `ABTaskFile`, `Dockerfile.goreleaser`, or CI configuration.
- Edit or delete an existing file under `examples/`.

### Why examples are behind an ask

Every example drives the libraries through their exported surface alone, as a `func Example`
with an `// Output:` comment that `go test` runs and compares. When one stops compiling, or its
output changes, that is a report that a published API changed. Editing the example until it
passes converts the report into silence, and the examples stop measuring anything.

So when an example goes red, say which library change did it and ask. The answer decides
whether the API change stands and the example follows it, or the API change is wrong and the
example was right to complain.

Two things this does not cover. Adding an example for a new surface is ordinary work. Changing
an example's own comments or variable names is not an API report. Editing what it calls or what
it prints is.
