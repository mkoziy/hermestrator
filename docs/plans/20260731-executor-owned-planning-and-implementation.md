# Executor-owned planning and implementation

Issue: https://github.com/mkoziy/hermestrator/issues/4 (Ticket 3)

## Overview

Add the PM's executor-orchestration slice: a typed policy selects ralphex,
Codex, Pi, or verification-only execution; the PM invokes the selected
executor binary directly (never rewriting its plan) inside a fresh per-issue
clone; a critique gate and an explicit operator-approval gate stand between
plan generation and execution; execution streams sanitized output into the
dashboard and can be cancelled; a verification gate runs the repo's canonical
checks before PR creation is allowed; and restart recovery classifies
in-flight work correctly. PR creation, review, and merge are out of scope
(Ticket 4 / issue #5).

This is the direct implementation of issue #4's acceptance criteria, which
duplicate Ticket 3 in `docs/tickets/20260726-genkit-pm-dashboard.md`.

## Context (from discovery)

- Files/components involved:
  - `app/internal/dashboard/app.go` — domain types, adapter interfaces,
    `Dependencies`, HTTP handlers (`dashboard` package, ~1850 lines).
  - `app/internal/live/adapters.go`, `intake.go`, `synthesis.go`, `oauth.go`,
    `sqlite_store.go` — adapters implementing `dashboard` interfaces
    (`live` package).
  - `app/internal/redaction/redaction.go` — secret redaction, reused for
    sanitizing executor output.
  - ADR 0001, `docs/specs/20260726-genkit-pm-dashboard.md`,
    `docs/tickets/20260726-genkit-pm-dashboard.md`.
- Related patterns found:
  - `CloneIntake` (`app/internal/live/intake.go`) already implements the
    "isolated clone under a controlled path, DI'd `exec.Command`, cleanup
    that refuses to escape its base directory" shape needed for per-issue
    implementation clones — extend this shape, don't reinvent it.
  - Every adapter that shells out (`GHPublisher`, `CloneIntake`) takes a
    `Command func(context.Context, string, ...string) *exec.Cmd` field so
    tests inject real coreutils (`printf`, `sh -c`, `sleep`) instead of
    fixture scripts or mocks. Use this for ralphex/Codex/Pi/verification
    invocation and for hang/cancel test scenarios.
  - `discoveryAgent` (`app/internal/live/adapters.go`) shows the pattern for
    a bounded Genkit agent function; reuse for the critique agent.
  - `dashboard` package defines pure types + interfaces; `live` package
    supplies adapters; `New(Dependencies)` wires them. New interfaces go in
    `dashboard`, new adapters go in `live`.
- Dependencies identified: none new — Genkit (already a dependency),
  `os/exec`, `syscall` (process-group signaling on the target OS).

## Development Approach

- **Testing approach**: Regular (code first, then tests), matching this
  repo's existing pattern.
- Complete each task fully, with passing tests, before starting the next.
- Reuse the `Command func(...) *exec.Cmd` DI convention everywhere a binary
  is invoked — no new fixture-script mechanism.
- Keep executor selection and repo-lock logic as plain deterministic Go in
  the `dashboard` package (no LLM call) — only the critique step needs a
  model, per CLAUDE.md's "keep agent judgment within the phase it is
  executing."
- Genkit-vs-plain-Go split, stated explicitly: subprocess management
  (ralphex/Codex/Pi/verification invocation) is plain Go because Genkit does
  not spawn external CLIs. The approval gate reuses the existing
  pending-turn/confirmation HTTP pattern (not a Genkit interrupt) because
  that pattern is already proven in this codebase for an identical
  "operator must act before the workflow proceeds" shape. Repo-lock and
  recovery classification are deterministic Go over SQLite state, not model
  calls — only the critique step (Task 9) is a Genkit agent.
- Do not touch or reuse `docker/ralphex-wrapper.sh` or
  `docker/ralphex-headless-plan.sh`, and do not reuse the `./ralphex/*`
  Hermes profile directories — they were read once during planning to
  confirm the real ralphex/codex/pi CLI invocation shape (documented above
  and in Solution Overview), not as a dependency of this implementation.
  PM-owned planning/execution config directories are new, separate paths.
- Telemetry (spans/structured logging for executor runs, per ADR 0001's
  "use Genkit for... telemetry") is deferred out of this plan. Nothing here
  blocks adding it later; it isn't required to satisfy issue #4's
  acceptance criteria, which only ask for output streaming, heartbeat,
  duration, and exit status (Task 11) — not tracing spans.
- **CRITICAL: every task MUST include new/updated tests.**
- **CRITICAL: all tests must pass (`make check` in `app/`) before starting
  the next task.**
- **CRITICAL: update this plan file if scope changes during implementation.**

## Testing Strategy

- Unit tests for deterministic logic: selection policy, repo lock, recovery
  classification (table-driven, per `dashboard` package convention).
- HTTP-seam tests (`httptest`) for every new dashboard handler (selection
  visibility, approval gate, cancellation, streaming render), per
  CLAUDE.md's "treat the complete `net/http` handler as the primary test
  seam."
- Subprocess tests use injected coreutils commands (`printf`, `sh -c`,
  `sleep`) exactly like `adapters_test.go` / `intake_test.go` — covering
  success, non-zero exit, hang, cancellation mid-run, and partial output.
- No UI e2e framework exists in this repo; dashboard behavior is covered by
  the HTTP-seam tests above.

## Solution Overview

- **Executor selection** (`dashboard` package, pure function): a
  `SelectExecutor` function taking ticket scope, repository policy, and
  prior-failure history, returning a typed `ExecutorSelection{Kind,
  Rationale}`. Rendered to the operator before planning starts.
- **Process runner** (`live` package): one small subprocess primitive with
  streaming stdout/stderr, heartbeat, duration, exit status, and
  process-group cancellation. Ralphex, Codex, Pi, and verification all sit
  on top of it instead of four separate subprocess implementations.
- **Issue workspace** (`live` package): fresh clone per issue under a
  controlled workspace root, structurally identical to `CloneIntake` but
  never promoted/merged with intake clones — implementation and intake
  workspaces stay separate on disk and in code.
- **Verified against the real binaries** (`ralphex --help`, `codex --help`,
  `pi --help`, and the existing `docker/ralphex-wrapper.sh` /
  `docker/ralphex-headless-plan.sh`, read for their CLI-invocation shape
  only — not reused as scripts, per CLAUDE.md): `ralphex --plan` is
  interactive by design — after drafting it waits on an accept/revise/
  `$EDITOR`/reject prompt with no non-interactive flag to skip that gate.
  There is no ralphex flag that makes plan *generation* non-interactive.
  `ralphex <plan-file>` (bare), `--review`, `--external-only`, and
  `--config-dir=<dir>` (env `RALPHEX_CONFIG_DIR`) for *execution* are all
  real, non-interactive-safe, and accept an arbitrary directory path. This
  reframes planning vs. execution below.
- **Plan generation (all executor selections)**: ralphex can never generate
  a plan non-interactively, so planning always calls `codex exec` or `pi -p`
  directly — never `ralphex --plan` — regardless of which executor was
  selected for the run. This isn't a runtime fallback triggered by a
  detected failure; it's unconditional, because the non-interactive gap is
  a structural property of the installed ralphex CLI, not a transient
  condition. The PM reads a PM-owned planning profile (model, effort,
  sandbox — its own format, not ralphex's `read_cfg` config schema) to
  decide codex-vs-pi and invokes it via the process runner, producing a
  plan file with the structure ralphex/the PM can validate (a `# Plan:`
  title line and `### Task N:` sections, matching what ralphex itself
  expects of a plan file). The PM never invokes `ralphex --plan`.
- **Ralphex execution**: when `SelectExecutor` picks `Ralphex`, the PM runs
  `ralphex --config-dir <PM-owned execution profile dir> <plan-file>` via
  the process runner. This *is* ralphex's real non-interactive execution
  path (task/review/finalize phases), confirmed safe by the existing
  wrapper. The PM reads the plan it generated; it never edits it.
- **Codex/Pi direct execution**: when `SelectExecutor` picks `Codex` or
  `Pi` directly, execution skips ralphex's orchestration entirely — the
  same plan file from the planning step is handed to `codex exec` or
  `pi -p` directly as a bounded task (issue #4: "Codex or Pi receives a
  bounded task directly when selected").
- **`VerificationOnly` selection**: when `SelectExecutor` returns
  `VerificationOnly`, the workflow skips planning, critique, and execution
  entirely and goes straight to the verification gate (Task 13) against the
  workspace as cloned.
- **Critique gate**: a bounded Genkit agent (same shape as `discoveryAgent`)
  scores premise, logic, blind spots, effort, and execution risk. Material
  findings re-invoke the planning step (task 5/7) rather than patching the
  plan by hand.
- **Approval gate**: an explicit operator HTTP action, following the
  existing pending-turn/confirmation pattern used for publication
  confirmation; execution cannot start until it fires.
- **Verification gate**: runs the target repo's canonical build/test/lint/
  race commands via the process runner before PR creation is allowed
  (PR creation itself is out of scope — this task only produces the
  pass/fail gate and blocks on failure).
- **Repository lock**: a single SQLite partial unique index keyed by
  repository ID, mirroring the existing
  `one_active_pending_turn_per_repository` index in `app.go` (line 308),
  enforcing at most one running implementation issue per repository at the
  database layer — no separate in-memory structure. Acquire = insert a row;
  a second acquire for the same repository fails on the unique constraint;
  release = delete/mark terminal. Acquired before the clone (Task 4) starts,
  released on terminal state or cancellation. Different repositories run
  concurrently because the index is scoped per repository ID.
- **Restart recovery**: on startup, classify each known in-flight issue
  workspace into `NoChanges | UncommittedChanges | LocalCommits |
  RemoteBranchExists | ExecutorFailed` from git state, matching acceptance
  criteria.

## Technical Details

- New `dashboard` package file `executor.go`: `ExecutorKind`,
  `ExecutorSelection`, `SelectExecutor(...)`, `CritiqueResult`,
  `VerificationResult`, `RecoveryState`, and the new adapter interfaces
  (`ExecutorRunner`, `Workspace` — note: `Workspace` name is taken by the
  existing view-model type, so use `IssueClone` for the interface and
  `IssueWorkspace`/similar for the concrete adapter — confirm no collision
  when writing Task 1).
- New `live` package files: `process.go`, `workspace.go`, `planning.go`,
  `ralphex.go`, `directexec.go`, `critique.go`, `preflight.go`, `verify.go`,
  `recovery.go`, each with a matching `_test.go`.
- `Dependencies` in `app.go` gains fields for the new interfaces; `New(...)`
  wires them; existing fields/handlers are untouched except where a new
  handler needs to render selection/approval/streaming state.
- Sanitize all executor stdout/stderr through `redaction.Secrets` before it
  reaches dashboard storage or rendering (mirrors CLAUDE.md's "do not store
  or render secrets ... in events, artifacts, logs, or pages").

## What Goes Where

- **Implementation Steps**: all code, tests, and doc updates below.
- **Post-Completion**: manual verification against a real ralphex/Codex/Pi
  installation; this plan's automated tests use injected coreutils, not the
  real binaries.

## Implementation Steps

### Task 1: Executor selection policy and types

**Files:**
- Create: `app/internal/dashboard/executor.go`
- Create: `app/internal/dashboard/executor_test.go`

- [ ] define `ExecutorKind` (`Ralphex`, `Codex`, `Pi`, `VerificationOnly`)
      and `ExecutorSelection{Kind, Rationale}` in `executor.go`
- [ ] implement `SelectExecutor(scope, repoPolicy, priorFailures)
      ExecutorSelection` as deterministic Go logic; `priorFailures` is a
      plain `[]FailureRecord{Kind, Reason}` slice the caller supplies —
      task 14 is what actually populates it from persisted run history, so
      keep the type minimal here and don't invent a fetch mechanism in this
      task
- [ ] write table-driven tests covering each selection branch (success
      cases)
- [ ] write tests for ambiguous/conflicting inputs falling back to
      `VerificationOnly` (error/edge cases)
- [ ] run tests — must pass before task 2

### Task 2: Surface executor selection before planning begins

**Files:**
- Modify: `app/internal/dashboard/app.go`
- Modify: `app/internal/dashboard/app_test.go`

- [ ] add a handler/fragment that renders `ExecutorSelection` (kind +
      rationale) to the operator before any planning call is made
- [ ] wire `SelectExecutor` into the existing per-issue workflow state
- [ ] write an `httptest` case asserting the rationale is visible pre-plan
      (success case)
- [ ] write a test asserting planning cannot start before selection is
      rendered (error/edge case)
- [ ] run tests — must pass before task 3

### Task 3: Generic streaming process runner

**Files:**
- Create: `app/internal/live/process.go`
- Create: `app/internal/live/process_test.go`

- [ ] implement a `ProcessRunner` with an injectable `Command func(context.Context,
      string, ...string) *exec.Cmd` field, streaming stdout/stderr line-by-line
      to a callback, tracking heartbeat and duration, and returning exit status
- [ ] start the child in its own process group by setting
      `cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}` on the
      `*exec.Cmd` returned by the injected `Command` (context cancellation
      alone only kills the direct child, not its descendants), and
      implement cancellation via `syscall.Kill(-pgid, ...)` on the group
- [ ] write tests for success (using `printf`) and non-zero exit (`sh -c
      'exit 1'`)
- [ ] write tests for hang + cancellation (`sleep`) and partial-output
      capture before cancel
- [ ] run tests — must pass before task 4

### Task 4: Per-issue clone workspace lifecycle

**Files:**
- Create: `app/internal/live/workspace.go`
- Create: `app/internal/live/workspace_test.go`

- [ ] implement an issue-clone adapter mirroring `CloneIntake`'s shape
      (injectable `Command`, path validation, `Cleanup` that refuses to
      escape its base directory) but rooted under a distinct, configurable
      workspace root and keyed by issue rather than by discovery session
- [ ] implement `Start(ctx, Repository, issueNumber) (path, error)` doing a
      fresh `gh repo clone`
- [ ] write tests for successful clone + cleanup (success case)
- [ ] write tests for path-escape rejection and clone failure surfacing
      `gh` stderr (error/edge cases)
- [ ] run tests — must pass before task 5

### Task 5: Plan generation via direct Codex/Pi invocation

**Files:**
- Create: `app/internal/live/planning.go`
- Create: `app/internal/live/planning_test.go`

- [ ] define the plan contract once here: the plan file path (relative to
      the issue workspace) and the minimal structural markers a valid plan
      must have (a `# Plan:` title line, one or more `### Task N:`
      sections) — export it so tasks 6/7/8 reference the same
      constant/type instead of redefining it
- [ ] implement a planning call that reads a PM-owned planning profile
      (model, effort, sandbox — the PM's own simple format; not ralphex's
      `read_cfg` schema, and not stored under the existing `./ralphex/`
      tree per issue #4's constraints) and invokes `codex exec` or `pi -p`
      directly via `ProcessRunner` with a bounded planning prompt, writing
      the result to the plan contract path
- [ ] validate the produced output against the plan contract before
      returning success; treat a structurally invalid plan as an error
- [ ] write tests for successful plan generation via the codex path (fake
      `codex` via injected `Command`)
- [ ] write tests for successful plan generation via the pi path (fake `pi`)
- [ ] write a test for a structurally invalid plan output being rejected
- [ ] run tests — must pass before task 6

### Task 6: Ralphex direct invocation for execution

**Files:**
- Create: `app/internal/live/ralphex.go`
- Create: `app/internal/live/ralphex_test.go`

- [ ] implement an execution call invoking `ralphex --config-dir <PM-owned
      execution profile dir> <plan-file>` (the plan from task 5) via
      `ProcessRunner`, streaming output — this is ralphex's real
      non-interactive execution path (task/review/finalize phases); the PM
      never calls `ralphex --plan` or bare `ralphex`
- [ ] assert (by construction — no file-write call in this path) that the
      PM never writes or patches the plan file once execution starts
- [ ] write a test asserting the execution invocation shape:
      `--config-dir` set to the execution profile, plan file as the
      positional argument
- [ ] write a test proving no plan-file write occurs on the PM side during
      execution (e.g. assert the plan file's mtime/content is unchanged)
- [ ] run tests — must pass before task 7

### Task 7: Codex/Pi direct execution

**Files:**
- Create: `app/internal/live/directexec.go`
- Create: `app/internal/live/directexec_test.go`

- [ ] implement a direct-execution call: when `SelectExecutor` (task 1)
      picked `Codex` or `Pi` rather than `Ralphex`, hand the plan file from
      task 5 to `codex exec` or `pi -p` directly as a bounded task via
      `ProcessRunner`, skipping ralphex's orchestration entirely
- [ ] write tests for direct Codex execution using the task-5 plan file
      (fake `codex` via injected `Command`)
- [ ] write tests for direct Pi execution using the task-5 plan file (fake
      `pi`)
- [ ] write a test asserting `ralphex` is never invoked on this path
      (negative assertion)
- [ ] run tests — must pass before task 8

### Task 8: Pre-critique repository and plan verification

**Files:**
- Create: `app/internal/live/preflight.go`
- Create: `app/internal/live/preflight_test.go`

- [ ] implement a check that confirms workspace paths exist, required tools
      (`ralphex`/`codex`/`pi`/`gh`) are resolvable, remote git state matches
      expectations, and the generated plan file has the expected structure
- [ ] surface failures as a typed result blocking progression to critique
- [ ] write tests for the all-clear case
- [ ] write tests for each failure mode (missing tool, remote mismatch,
      malformed plan)
- [ ] run tests — must pass before task 9

### Task 9: Plan critique gate

**Files:**
- Create: `app/internal/live/critique.go`
- Create: `app/internal/live/critique_test.go`

- [ ] implement a bounded Genkit agent (shape matching `discoveryAgent` in
      `adapters.go`) that evaluates premise, logic, blind spots, effort, and
      execution risk against the generated plan
- [ ] on material findings, re-invoke task 5's planning call (the sole plan
      generator, regardless of selected executor) instead of editing the
      plan
- [ ] cap regeneration at a fixed number of rounds (e.g. 3); once exceeded,
      surface the run as blocked, awaiting-operator instead of looping
      silently — critique is a quality gate, not an infinite retry engine
- [ ] write tests with a fake model returning "approved" (success case)
- [ ] write tests with a fake model returning material findings, asserting
      regeneration is triggered and no plan file is hand-edited
- [ ] write a test asserting the round cap surfaces a blocked state instead
      of regenerating indefinitely
- [ ] run tests — must pass before task 10

### Task 10: Mandatory operator approval gate

**Files:**
- Modify: `app/internal/dashboard/app.go`
- Modify: `app/internal/dashboard/app_test.go`

- [ ] add an approval handler following the existing publication-confirmation
      pattern; implementation cannot start until this fires
- [ ] block the execution entry point (task 6/7 callers) on approval state
- [ ] write an `httptest` case for approval unlocking execution (success)
- [ ] write an `httptest` case asserting execution is rejected pre-approval
      (error/edge case)
- [ ] run tests — must pass before task 11

### Task 11: Stream sanitized executor output into the dashboard

**Files:**
- Modify: `app/internal/dashboard/app.go`
- Modify: `app/internal/dashboard/app_test.go`
- Modify: `app/internal/live/process.go`

- [ ] pipe `ProcessRunner` output through `redaction.Secrets` before storage
      and route it to the dashboard as heartbeat/duration/exit-status/
      artifact updates, extending the existing `Artifact` type/rendering
- [ ] write a test asserting a secret-looking value in executor output never
      reaches the rendered page (success case, security-relevant)
- [ ] write a test asserting heartbeat/duration/exit status render correctly
      for a completed run
- [ ] run tests — must pass before task 12

### Task 12: Cancellation

**Files:**
- Modify: `app/internal/dashboard/app.go`
- Modify: `app/internal/dashboard/app_test.go`
- Modify: `app/internal/live/process_test.go`

- [ ] add an operator-triggered cancel handler that cancels the executor's
      context, relying on task 3's process-group signal
- [ ] write a test with a hanging fake executor (`sleep`) cancelled mid-run,
      asserting the process group is terminated and partial output is
      preserved
- [ ] write a test asserting cancellation of an already-finished run is a
      no-op, not an error
- [ ] run tests — must pass before task 13

### Task 13: Verification gate before PR creation

**Files:**
- Create: `app/internal/live/verify.go`
- Create: `app/internal/live/verify_test.go`

- [ ] implement a verification run of the target repo's canonical
      build/test/strict-lint/race-sensitive checks via `ProcessRunner`,
      gating a `ReadyForPR` boolean (PR creation itself stays out of scope
      per issue #4)
- [ ] wire the `VerificationOnly` selection (task 1) to call this gate
      directly against the cloned workspace, skipping planning/critique/
      execution entirely
- [ ] write a test for an all-passing verification run
- [ ] write a test for a failing check blocking `ReadyForPR`
- [ ] write a test asserting `VerificationOnly` selection reaches this gate
      without any planning/execution call happening
- [ ] run tests — must pass before task 14

### Task 14: Repository concurrency lock

**Files:**
- Modify: `app/internal/live/sqlite_store.go`
- Modify: `app/internal/live/sqlite_store_test.go`

- [ ] add an `implementation_runs` table (repository_id, executor kind,
      state, failure_reason, timestamps) with a partial unique index scoped
      to `(repository_id) WHERE state IN (...active states...)`, mirroring
      `one_active_pending_turn_per_repository` (app.go:308) — no separate
      in-memory map
- [ ] add acquire (insert row) / release (mark terminal, recording
      failure_reason on failure) helpers; call acquire before task 4's
      clone starts and release on terminal state or cancellation
- [ ] add a query returning recent terminal runs (kind + failure_reason)
      for a repository, feeding `SelectExecutor`'s `priorFailures` input
      (task 1) — this is the only place that input is populated
- [ ] write a test asserting a second acquire for the same repo fails on the
      unique constraint while the first is held (success case for the
      lock's core guarantee)
- [ ] write a test asserting concurrent acquires for different repositories
      both succeed
- [ ] write a test asserting release + re-acquire for the same repository
      succeeds (lock isn't stuck after a completed/cancelled run)
- [ ] run tests — must pass before task 15

### Task 15: Restart recovery classification

**Files:**
- Create: `app/internal/live/recovery.go`
- Create: `app/internal/live/recovery_test.go`

- [ ] implement classification of each known in-flight issue workspace into
      `NoChanges | UncommittedChanges | LocalCommits | RemoteBranchExists |
      ExecutorFailed` from git status/log and last-known process state
- [ ] as part of the same startup pass, reconcile task 14's repository lock:
      any lock row whose owning run has no confirmed-alive process gets
      released (or transitioned to a terminal state derived from the git
      classification above) — an unclean PM crash must not leave a
      repository permanently locked
- [ ] write table-driven tests covering each of the five states
- [ ] write a test for an ambiguous/corrupt workspace surfacing an explicit
      error rather than guessing a state
- [ ] write a test asserting a crash-orphaned lock is released during
      recovery, allowing a subsequent acquire for the same repository
- [ ] run tests — must pass before task 16

### Task 16: Verify acceptance criteria

- [ ] verify every checkbox in issue #4's acceptance criteria against the
      implementation
- [ ] verify concurrency: two issues on different repositories can execute
      simultaneously; two on the same repository cannot
- [ ] run `make check` in `app/` — must be clean
- [ ] confirm no code path reads `docker/ralphex-wrapper.sh` or
      `docker/ralphex-headless-plan.sh`
- [ ] confirm test coverage includes success, failure, hang, cancellation,
      partial output, and resume, per the issue's testing requirement

### Task 17: Update documentation

- [ ] update `app/README.md` if the new workspace-root/config-dir layout
      needs operator-facing documentation
- [ ] update `CLAUDE.md` only if a genuinely new project-wide pattern was
      introduced beyond what's already documented there
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion

**Manual verification:**
- `ralphex`, `codex`, and `pi` are confirmed installed and their CLI shapes
  verified during planning (see Solution Overview); automated tests still
  use injected coreutils rather than the real binaries, so run the full
  flow once against the real binaries end-to-end (including an actual
  model call) to confirm behavior, not just CLI surface.
- Confirm dashboard streaming renders acceptably over the operator's actual
  network/browser (latency of heartbeat updates, artifact rendering).

**External system updates:**
- None — PR creation, review, and merge remain in Ticket 4 (issue #5).
