# Close out issue #1 and replace the hardcoded executor scope

## Overview

Issue #1 is the original spec ("replace Hermes with a Genkit PM dashboard").
Every ticket derived from it (issues #2–#5, #8–#12, #14) is closed, and the
code backing them exists in `app/`. Two things stand between that state and
actually closing #1:

1. **Two hardcoded `"medium"` scope values with no real signal behind them.**
   `app/internal/dashboard/app.go:1811` (`SelectExecutor("medium", "",
   priorFailures)`, in `executorSelect`) and `app/internal/dashboard/app.go:1873`
   (`scope := "medium" // TODO: Get from conversation state`, in
   `executorPlan`) both stand in for a complexity signal that has never
   existed anywhere in the intake schema or conversation state. This was
   never a ticket acceptance criterion, but it's real dead-reckoning in a
   control-flow decision (executor selection, plan/critique invocation), so
   this plan adds it for real: a deterministic `EstimateScope` function,
   mirroring the existing `AssessADR` pattern, that derives
   `simple`/`medium`/`complex` from the resolved discovery decisions and the
   confirmed ticket breakdown — matching the vocabulary `executorForScope`
   already switches on (`app/internal/dashboard/executor.go:94-105`; any
   other string falls through to `VerificationOnly`) — persisted on the
   `intakes` row and read at selection/planning time instead of guessed.
2. **A duplicate stale plan document — already fixed.** `docs/plans/completed/20260803-reviewed-pr-and-recovery-lifecycle.md`
   was already checked off and archived correctly. The premise here was
   wrong: the plan was copied, not "never moved" — a leftover unchecked
   duplicate remained at `docs/plans/20260803-reviewed-pr-and-recovery-lifecycle.md`.
   That duplicate has been deleted (`git rm`), and the archived copy's
   `executorState` enum listing has been corrected to include
   `executorCreatingPR`/`"creating_pr"`, which shipped but was never added
   to either plan copy's Technical Details section. No further
   reconciliation work is needed for this item.

## Context (from discovery)

- Files/components involved:
  - `app/internal/dashboard/app.go` — `Synthesizer` interface,
    `localSynthesizer`, `synthesizeArtifacts`, `AssessADR` (the pattern to
    mirror), `executorSelect` (func at line ~1796, hardcode at line ~1811),
    `executorPlan` (func at line ~1828, hardcode at line ~1873 — **not**
    `executorRun`, a different handler at line ~2037 with no scope
    hardcode), `IntakeStatus`, the `intakes` table schema and
    `addColumnIfMissing` migration helper, the status-loading `SELECT`
    (line ~3049).
  - `app/internal/dashboard/executor.go` — `SelectExecutor`,
    `executorForScope` (line ~94, switches on `"simple"|"medium"|"complex"`;
    anything else falls to `VerificationOnly`), `rationaleFor` (mirrors the
    same three-value switch).
  - `app/internal/live/planning.go`, `app/internal/live/critique.go` —
    `Planner.GeneratePlan` and `Critiquer.CritiquePlan`, both already take a
    `scope string` parameter that is simply forwarded.
  - `docs/plans/20260803-reviewed-pr-and-recovery-lifecycle.md` — the plan
    to reconcile against merged code.
- Related patterns found:
  - `AssessADR(decision string) (string, string)` (`app.go:2806`) is a pure,
    deterministic function with no LLM call. Its `pm_assess_adr` Genkit tool
    wrapper (`app/internal/live/synthesis.go`) adds nothing functional over
    calling `AssessADR` directly — it exists purely for tracing consistency
    with the other three bounded discovery capabilities, at the cost of a
    JSON round-trip through `RunRaw`/`decodeToolResult`. `EstimateScope`
    does not need this wrapper: it isn't a discovery capability the operator
    inspects or confirms, just an internal control-flow input, so this plan
    calls the plain function directly inside `synthesizeArtifacts` instead
    of adding a fifth Genkit tool.
  - Schema evolution uses `addColumnIfMissing(db, table, column,
    definition)` (`app.go:757`), never a bare `ALTER TABLE`, and never
    rewrites the base `CREATE TABLE IF NOT EXISTS` for already-shipped
    columns (see `executor_kind`, `run_id`, `pr_number`, etc. — all added via
    `addColumnIfMissing`, not the initial `CREATE TABLE`).
  - `synthesizeArtifacts` (`app.go:2772`) already runs `GrillWithDocs` →
    `ToSpec` → `ToTickets` → `AssessADR` per resolved decision, and its
    caller (`app.go:1392`) persists the results in one transaction that also
    does `UPDATE intakes SET state=?,updated_at=?`. The new scope value
    belongs in that same transaction.
  - `IntakeStatus` (`app.go:216`) and its loader (`app.go:3049`) list every
    persisted intake field explicitly — `Scope` is added the same way as
    `ReviewRound`/`RetryState` before it.

## Development Approach

- **Testing approach**: Regular (code first, then tests in the same task).
- Complete each task fully before moving to the next.
- Every task that changes code ends with passing tests before the next task.
- Update this plan file if scope changes during implementation.

## Progress Tracking

- 2026-08-04 audit: Ticket 1 acceptance criteria are implemented and covered
  by OAuth, dashboard, persistence, streaming, telemetry, notification, and
  tooling tests; no genuine gap found.
- 2026-08-04 audit: Ticket 3/4 reviewed-PR and recovery lifecycle is present
  end to end, with PR, review, fix-loop, merge-gate, lease, cleanup, and
  startup-recovery tests; no genuine gap found.

## Solution Overview

Add a deterministic `EstimateScope(resolved []string, tickets string)
string` function next to `AssessADR`, returning `"simple"`, `"medium"`, or
`"complex"` — the exact vocabulary `executorForScope` already switches on —
from evidence already available at synthesis time (number of resolved
decisions, length/structure of the confirmed ticket breakdown — no LLM call,
no new external dependency). Call it directly inside `synthesizeArtifacts`
(no new `Synthesizer` interface method or Genkit tool — see Related
patterns above for why), persist the result on a new `intakes.scope` column
via `addColumnIfMissing`, thread it through `IntakeStatus`, and read
`status.Scope` (defaulting to `"medium"` only when empty, e.g. pre-existing
rows) at the two call sites that currently hardcode the string. Separately,
audit the closed tickets' acceptance criteria, reconcile the stale plan
document's checkboxes against the merged code, and close issue #1.

## Technical Details

- New column: `intakes.scope TEXT NOT NULL DEFAULT ''`, added via
  `addColumnIfMissing`.
- New `IntakeStatus.Scope string` field, populated by the existing
  status-loading `SELECT` in `app.go` (~line 3049).
- `EstimateScope(resolved []string, tickets string) string` is a plain
  function in the `dashboard` package, called directly by
  `synthesizeArtifacts` — not added to the `Synthesizer` interface.
- `executorSelect` (func ~line 1796, hardcode ~line 1811) reads
  `status.Scope` (falling back to `"medium"` if empty) instead of the
  literal `"medium"`.
- `executorPlan` (func ~line 1828, hardcode ~line 1873 — this is the site
  currently commented `// TODO: Get from conversation state`) reads
  `status.Scope` (same fallback) instead of the literal `"medium"`; the
  `// TODO` comment is removed.

## What Goes Where

- **Implementation Steps**: the `EstimateScope` capability, its wiring, and
  the plan-document reconciliation are all achievable within this
  repository.
- **Post-Completion**: closing issue #1 on GitHub is an external action, not
  a checkbox.

## Implementation Steps

### Task 1: Add the deterministic `EstimateScope` function

**Files:**
- Modify: `app/internal/dashboard/app.go`
- Modify: `app/internal/dashboard/app_test.go`

- [x] add `EstimateScope(resolved []string, tickets string) string` near
      `AssessADR` (`app.go`), returning `"simple"`/`"medium"`/`"complex"` —
      matching `executorForScope`'s vocabulary exactly, since any other
      string falls through to `VerificationOnly` there — from the count of
      `resolved` decisions and the length/heading count of `tickets`
      (mirror `AssessADR`'s plain-function, no-LLM shape)
- [x] pick and document concrete thresholds in a short comment (e.g. fewer
      than 2 resolved decisions and a single ticket heading → `"simple"`;
      more than 5 resolved decisions or 3+ ticket headings → `"complex"`;
      otherwise `"medium"`) so the heuristic is auditable, not magic
- [x] write tests for `EstimateScope` covering the simple/medium/complex
      boundaries and empty input
- [x] run tests — must pass before task 2

### Task 2: Persist scope on the `intakes` row and thread it through `IntakeStatus`

**Files:**
- Modify: `app/internal/dashboard/app.go`
- Modify: `app/internal/dashboard/app_test.go`

- [x] add `if err = addColumnIfMissing(db, "intakes", "scope", "TEXT NOT
      NULL DEFAULT ''"); err != nil { ... }` alongside the other
      `addColumnIfMissing` calls
- [x] add `Scope string` to `IntakeStatus` and include `scope` in the
      status-loading `SELECT`/`Scan` (~`app.go:3049`)
- [x] in `synthesizeArtifacts`, call `EstimateScope` directly with the
      resolved decisions and the `artifactTickets` body, and return the
      result alongside the artifacts (change the function's return signature
      or add a sibling return value — caller's choice, keep it minimal)
- [x] in the `synthesizeArtifacts` caller (~`app.go:1392`), extend the
      existing `UPDATE intakes SET state=?,updated_at=?` in the same
      transaction to also set `scope=?`
- [x] write tests confirming a synthesize call persists a non-empty `scope`
      on the intake row and that it round-trips through `IntakeStatus`
- [x] run tests — must pass before task 3

### Task 3: Replace both hardcoded `"medium"` call sites

**Files:**
- Modify: `app/internal/dashboard/app.go`
- Modify: `app/internal/dashboard/app_test.go`

- [x] in `executorSelect` (~line 1811), replace the literal `"medium"` with
      `status.Scope` (this handler currently only loads `priorFailures`, not
      `status` — load it), falling back to `"medium"` only when
      `status.Scope == ""`
- [x] in `executorPlan` (~line 1873; **not** `executorRun`, a different
      handler with no scope hardcode), replace `scope := "medium" // TODO:
      Get from conversation state` with the same `status.Scope`-with-fallback
      read — `status` is already loaded earlier in this handler — and remove
      the `TODO` comment
- [x] write tests confirming `executorSelect`/`executorPlan` use a
      previously-synthesized scope value (e.g. `"complex"`) when present,
      and fall back to `"medium"` for an intake row with no scope set
      (legacy row / synthesis never ran)
- [x] run tests — must pass before task 4

### Task 4: Audit closed-ticket acceptance criteria against the running code

**Files:**
- none (verification task; no code changes expected)

- [x] for each of Ticket 1's acceptance criteria in
      `docs/tickets/20260726-genkit-pm-dashboard.md`, confirm the
      implementation exists and is exercised by a test (GitHub login via
      go-pkgz/auth, allowlist, repository picker, durable conversation,
      HTMX/Tabler streaming, SQLite-backed restart durability, phase/model
      role/elapsed/activity/tokens/cost display, Telegram test notification,
      Genkit Developer UI availability, `httptest` seam coverage, `make
      check` contents, pre-push hook, baked-in skills)
- [x] for Ticket 3/4 (issues #4, #5), confirm the reviewed-PR-and-recovery
      loop described in `docs/plans/20260803-reviewed-pr-and-recovery-lifecycle.md`
      is present end to end (PR creation/reuse, standards+spec review,
      review-finding fix loop, mergeability re-check, dashboard merge
      approval, idempotent merge+close, clone cleanup/retention, startup
      reconciliation) by grepping for the types/functions the plan names
      (`GHPRCreator`, `Reviewer`, `executorMergeReady`, `RunLease`,
      orphan-lease recovery) and confirming each has passing tests
- [x] record any genuine gap found as a new `⚠️` entry in this plan's
      Progress Tracking section rather than silently fixing it out of scope
      (none found)
- [x] no test changes expected for this task unless a gap is found (none found)

> Discovery-time spot check (not a substitute for running this task): a
> pre-planning grep found `RunLease.Acquire`/`Release` already called from
> `executorSelect`/`executorRun`/`executorReview`/`retryExecutorPhase`, and
> `reconcileStartup` releasing orphaned leases on boot
> (`TestReconcileStartupRepairsRemoteStateAndOrphanLeases`). Treat that as a
> lead, not a conclusion — confirm it properly when this task actually runs.

### Task 5: Reconcile the stale 20260803 plan document

**Files:**
- Modify: `docs/plans/20260803-reviewed-pr-and-recovery-lifecycle.md`

- [ ] for each of the 16 tasks' checkboxes, mark `[x]` where Task 5's audit
      confirmed the corresponding code and tests exist; leave any
      unconfirmed item unchecked with a `⚠️` note instead of assuming
- [ ] move the file to `docs/plans/completed/20260803-reviewed-pr-and-recovery-lifecycle.md`
      once every checkbox is either checked or explicitly flagged
- [ ] no tests apply to this documentation-only task

### Task 6: Verify acceptance criteria

- [ ] verify `EstimateScope` is deterministic, tested at its boundaries, and
      the two hardcoded `"medium"` sites are gone (`grep -n '"medium"'
      app/internal/dashboard/app.go` shows only the documented fallback, not
      a bare assignment)
- [ ] verify `intakes.scope` round-trips through a real synthesize →
      restart → read cycle
- [ ] run full test suite: `make check` (from `app/`)
- [ ] verify test coverage meets project standard (no new untested branches
      in touched files)

### Task 7: [Final] Update documentation

- [ ] update `CLAUDE.md` if the scope-estimation capability introduces a
      pattern worth documenting for future work (likely not — it follows an
      existing pattern) — none needed
- [ ] move this plan to `docs/plans/completed/` once Tasks 1–6 are done

## Post-Completion

*Items requiring manual intervention or external systems*

**Close issue #1**: once Tasks 1–7 are merged, comment on
https://github.com/mkoziy/hermestrator/issues/1 summarizing that every
derived ticket is closed and the scope-estimation gap is resolved, then
close it — this is a `gh issue close` action, not a plan checkbox.
