## Project Overview


## Things not to do, ever

- Don't use the word `seam`
- Don't use the `gh` command
- Don't use the `git` command
- Never run `find /` with any arguments, stick to the go code directories

## Testing

- Framework: Ginkgo v2 + Gomega with gomock.
- Run unit tests with `abt t u [dir]`. Use `go test ./path/... -v -run "<name>"` only for targeted single-test runs.
- Before marking any coding task complete, run `abt t lint` and resolve everything it reports. This runs `go fmt`, `go mod tidy`, misspell, `go vet`, and `staticcheck`.
- No stray `FDescribe`/`FIt`/`FContext` focus prefixes in committed tests.

## Working protocol

- **Collaborate not Autonomy** The prevailing model is that we collaborate on solving a problem, you do not autonomously work and edit whatever you like. This means you summarize steps, you seek permission and you explain what is landing.
  - **The request is the whole scope.** Do exactly what was asked and stop. Not the adjacent
    file, not the stale comment you noticed, not the count in another document, not the
    thing you would have done differently. A request to change one thing is not a licence
    to tidy its neighbourhood.
  - **Adjacent problems get reported, not fixed.** When something outside the request looks
    wrong, say so in one or two sentences and wait. I decide whether it is in scope. This
    holds even when the fix is a one-liner, and especially when it is.
  - **Never edit a document's intent to accommodate your change.** If your change does not
    fit what a document says, that is a question for me, not a licence to reword the
    document until it fits.
  - **A mistake is fixed with the minimum edit.** Correcting your own error does not open
    new scope. Revert what you got wrong; do not touch more files on the way.
  - **Bias to asking.** When you are unsure whether something is in scope, it is not. Ask.
- **Exploratory questions** ("how could we…", "what do you think about…"): propose an approach and wait for explicit confirmation before implementing. Do not start work on the assumption that exploration implies approval.
- **Non-trivial plans** must be reviewed before presenting to the user:
  1. Draft the plan and present it to the user in short overview form
  2. Spawn three `Agent` calls in parallel once the user agrees to the plan in point 1: one security-and-consistency reviewer, one adversarial reviewer, one UX reviewer.
  3. Incorporate suggestions that hold up. **A finding being correct does not make it in scope.** Adopt only what the change as scoped actually needs: a reviewer showing that a step does not work, or that the plan states something false. Everything else is reported in one or two lines and becomes its own tracker item, however good it is. Reviewers widen; the plan does not have to.
  4. Present the final plan to the user with a short "reviewer input adopted" section so the user can see what shifted, and say what was reported rather than adopted.
  5. If you have questions, ask them in the review. But before continuing, always give the user a chance to ask 
     questions or steer the plan as a final step. Just because he answered your questions does not mean you are 
     ready to move on. Ask for final user input.
- **A plan is proportional to its change.** If the change is a deletion, the plan is short. When a document grows past what the work warrants, that is the signal scope crept, not that the writing needs tightening.
- **Planning documents state the design as it is now.** No revision history, no "an earlier draft said", no change log in the footer. When a decision changes, rewrite the affected text in place, including anywhere the old version leaked to. A revision marker in the status line is fine.
- **Suspected bugs in existing code**: do not write tests that lock in behavior you suspect is wrong. Stop, describe the concern, ask the user how to proceed.
- **Change code only after approval**: While discussing or working on a plan, do not change code without explicit approval, questions like "What's next?" does not mean edit code, it means answer the question - what will we work on next.

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
- **`a2a`, `mcpserver`, `serve/asyncjobs` and `tasks` are not there yet.** Their shape is not
  settled, so do not hold work on them to the same bar and do not treat their current API as a
  contract. `serve/asyncjobs` is one channel implementation among the several section 12 of the
  Network Serve summary expects, and `tasks` has no importer yet.
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
