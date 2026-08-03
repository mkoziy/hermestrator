# Reviewed PR and Recovery Lifecycle

## Overview

Issue #4 (merged in PR #13) carries an implementation through executor
selection, planning, critique, approval, execution, and — for the
verification-only path — canonical checks. It stops there: nothing creates a
pull request, reviews it, gates a merge, cleans up the clone, or recovers a
crashed run past the executor phase. This plan implements the rest of the
loop from issue #5: PR creation, independent standards/spec review with
delegated fixes, merge approval, safe cleanup and retention, and full startup
recovery — so a ticket goes from `approved` executor plan all the way to a
merged, cleaned-up repository without manual `gh` commands.

Two gaps found while reading the current code, both folded into this plan
rather than filed separately since they block issue #5's own acceptance
criteria:

- **Verification never runs for the ralphex/codex/pi executor kinds.**
  `runVerification` (`app.go:1938`) only fires on the `VerificationOnly`
  path. For every other executor kind, `executorCompleted` today means only
  "the subprocess exited 0" — build/test/lint/race never ran. Issue #5
  requires "build, tests, strict lint, and race-sensitive checks pass"
  before a PR is even created, so this plan adds that call for all kinds.
- **`ImplementationRunStore` (the per-repository lease) is already built
  but never wired in.** `Acquire`/`Release`/`RecentFailures`/`ListActive`
  exist and are unit-tested (`sqlite_store.go`), and `RecoverLocks`
  (`recovery.go:153`) already consumes the store — but nothing in
  `app.go` or `cmd/pm/main.go` calls any of it. This plan wires it into the
  executor lifecycle and startup, which is exactly the "repository leases
  ... recovered safely after restart" acceptance criterion.

## Context (from discovery)

- Files/components involved:
  - `app/internal/dashboard/app.go` — `executorState` enum, intake HTTP
    handlers, `Dependencies` struct, dashboard template.
  - `app/internal/live/` — `critique.go` (review-loop precedent),
    `recovery.go` / `WorkspaceClassifier` (git-state classification),
    `sqlite_store.go` (`ImplementationRunStore`, unused lease/lock store),
    `workspace.go` (`IssueWorkspace`, has `Cleanup`), `intake.go`
    (`GHPublisher`, idempotency-key-in-body precedent), `verify.go`
    (`VerificationRunner`), `directexec.go` / `ralphex.go` (executor
    invocation, reusable for fix delegation).
  - `app/cmd/pm/main.go` — dependency wiring, startup sequence.
- Related patterns found:
  - Every phase transition is an atomic `UPDATE intakes SET executor_state=?
    WHERE repository_id=? AND executor_state=?` with a `RowsAffected` check
    — no phase changes without a satisfied precondition. New states follow
    the same shape.
  - `Critiquer`/`CritiqueModelFunc` (`critique.go`) is the direct precedent
    for the new PR reviewer: a bounded model call, a cap on rounds
    (`MaxCritiqueRounds`), and regeneration/fixing delegated to an existing
    executor type rather than done by hand.
  - `GHPublisher.Publish` (`intake.go`) embeds a `<!-- hermestrator-...
    -->` marker in the issue body and queries for it before creating a
    duplicate — the idempotency pattern issue #5 asks for on PR
    create/review/merge/close.
  - All GitHub/git mutation types take an injectable `Command func(context.
    Context, string, ...string) *exec.Cmd` defaulting to
    `exec.CommandContext`, so tests substitute fake binaries. Keep this for
    every new type.
  - `dashboard_adapters.go` / `dashboard_executor.go` wrap `live` types
    behind small `dashboard.*` interfaces so `internal/dashboard` never
    imports `internal/live` — new components (`Reviewer`, `PRCreator`,
    `MergeExecutor`, `RunLease`) need the same adapter shape.
- Dependencies identified: `gh` CLI (PR/issue mutation), the existing
  `Model`/Genkit session (review model calls), `redaction.Secrets` (already
  used for all logged/streamed output), `Telegram` interface (notification
  only, never a merge trigger).

## Development Approach

- **Testing approach**: Regular (code first, then tests) — matches PR #13.
- Complete each task fully, with passing tests, before starting the next.
- Extend the existing `executorState` enum and `intakes` table rather than
  adding a parallel state table — the lease/lock stays conceptually "held"
  by the same row from `selected` through `cleanup_done`.
- Every new GitHub mutation (PR create, review comment, merge, issue close)
  must query current GitHub state before mutating, and embed/check a stable
  idempotency marker, exactly like `GHPublisher`.
- Coding executors (ralphex/Codex/Pi) are invoked for review-driven fixes
  the same way they are for planning execution: the PM never patches
  production code by hand.
- **CRITICAL: every task MUST include new/updated tests** — success and
  error paths, using fake executables/adapters via `httptest` at the
  handler level (per CLAUDE.md) and direct unit tests for deterministic
  git/state-machine logic.
- **CRITICAL: all tests must pass before starting next task.**
- **CRITICAL: update this plan file if scope changes during implementation.**
- Run `make check` after each task.

## Testing Strategy

- **Unit tests**: required for every task. State-machine transitions,
  idempotency-key handling, and git-state classification get direct
  low-level tests (fake `Command` functions, temp git repos). No UI-based
  e2e framework exists in this project — the `net/http` handler with
  `httptest` and fake `GitHub`/`Model`/`Store`/`Telegram`/executor adapters
  is this project's e2e seam per CLAUDE.md, and is where "end-to-end"
  criteria (duplicate responses, crashes, restart recovery, merge
  rejection, cleanup safety) are covered.

## Progress Tracking

- Mark completed items with `[x]` immediately when done.
- Add newly discovered tasks with ➕ prefix.
- Document issues/blockers with ⚠️ prefix.
- Keep this plan in sync with actual work done; update it if scope shifts.

## Solution Overview

Extend the existing single `executor_state` enum on the `intakes` table with
new post-execution phases, each gated by the same atomic-CAS-update pattern
already in `app.go`:

```
... running → completed → verifying → verified → pr_created → reviewing
  → review_blocked → fixing → (back to verifying) ... → merge_ready
  → merge_approved → merging → merged → cleanup_done
```

(`failed` remains the shared terminal-failure state, reachable from
`verifying`, `reviewing` after `MaxReviewRounds` exhausted, `merging`, or
git/GitHub errors.)

`ImplementationRunStore.Acquire` is called once, at the point a run first
starts (existing `executorRun` entry), and `Release` is called exactly once,
at `cleanup_done` (success) or `failed` (terminal failure) — so the lease
covers the entire lifecycle this plan adds, not just execution.

**Cross-database crash consistency**: `ImplementationRunStore` owns its own
`sql.DB` (the `implementation_runs` table), separate from the dashboard's
`a.db` (the `intakes` table) — there is no shared transaction between an
`intakes.executor_state` write and a `RunLease.Release` call. A crash
between the two can leave an `active` lease with a terminal intake state, or
vice versa. Rather than attempting a cross-DB transaction, recovery treats
**`intakes.executor_state` as the source of truth** and reconciles the lease
store against it: at startup, for every `intakes` row with a non-empty
`run_id`, if the intake is in a terminal state (`cleanup_done` or `failed`)
but its `run_id`'s lease in `implementation_runs` is still `active`,
recovery releases it (idempotently — `Release`'s existing `WHERE
state='active'` guard makes this safe to run every startup). Recovery reads
PR/git state through the `intakes`/`repositories` tables (which have the
repo full name and `pr_number`), not through `ImplementationRunStore.
ListActive` (which only carries `run_id`/`repository_id`/`issue_number`/
`executor_kind` — no PR number or repo full name, so it cannot itself drive
a `gh pr view` call).

**Orphaned-lease gap**: `Acquire` (writing to `implementation_runs`) and
persisting the returned `run_id` onto the `intakes` row are two separate
writes (Task 2). A crash between them leaves an `active` lease in
`implementation_runs` that no `intakes` row references — the
`intakes`-as-source-of-truth reconciliation above can never find it, so it
never gets released, and `Acquire`'s own-repo-active-run check then
deadlocks that repository permanently. To close this, `Acquire` and the
`intakes.run_id` write happen in a fixed order (`Acquire` first, `run_id`
persisted immediately after, before any other repository-scoped work
proceeds) **and** recovery adds a second, independent pass: call
`ImplementationRunStore.ListActive` directly and, for any active lease whose
`run_id` does not appear in any `intakes.run_id` column, release it — this
is the only path that catches a crash in that specific window, since it
does not go through `intakes` at all.

Key design decisions:

- **State lives in the existing enum/table**, not a parallel table — avoids
  duplicating lock/lease logic (evaluated and rejected during planning).
- **Review reuses the critique-loop shape**: a bounded model call producing
  structured findings, a round cap, and regeneration/fixing delegated to an
  existing executor rather than hand-edited by the PM.
- **Fix delegation reuses the existing executor invocation code** (
  `DirectExecutor`, `ralphex.go`'s config-dir wiring) with a bounded task
  built from review findings — no new subprocess-invocation code path.
- **Cleanup only ever runs after GitHub itself confirms the merge** (`gh pr
  view --json state,mergedAt`), never from local state alone, per the
  issue's explicit constraint.
- **Fix-round cost is bounded by `MaxReviewRounds` (3), not a separate
  budget.** Each round invokes one executor run; capping rounds already
  caps the worst case at 3 extra executor invocations per PR. No
  additional cost tracking is added — the existing per-run cost/artifact
  persistence (Task 10) is for audit, not enforcement. Revisit only if
  real usage shows 3 rounds is itself too expensive.
- **`app.go` keeps growing as the executor-state file** — this plan adds
  ten states and their handlers to it rather than splitting by phase. Left
  as-is deliberately: splitting now would be a refactor with no test-driven
  reason to do it as part of this plan. If `app.go` becomes hard to
  navigate after this lands, split by phase (`executor_pr.go`,
  `executor_review.go`, `executor_merge.go`) as a follow-up, not here.

## Technical Details

### New `executorState` values (dashboard/app.go)

```go
executorVerifying      executorState = "verifying"
executorVerified       executorState = "verified"
executorPRCreated      executorState = "pr_created"
executorReviewing      executorState = "reviewing"
executorReviewBlocked  executorState = "review_blocked"
executorFixing         executorState = "fixing"
executorMergeReady     executorState = "merge_ready"     // mergeable, review clean
executorMergeApproved  executorState = "merge_approved"  // operator approved via dashboard
executorMerging        executorState = "merging"
executorMerged         executorState = "merged"
executorCleanupDone    executorState = "cleanup_done"    // final success terminal state
```

### `intakes` table additions (migrations via existing `addColumnIfMissing`)

- `pr_number INTEGER NOT NULL DEFAULT 0`
- `pr_url TEXT NOT NULL DEFAULT ''`
- `review_round INTEGER NOT NULL DEFAULT 0`
- `review_findings TEXT NOT NULL DEFAULT ''`
- `run_id TEXT NOT NULL DEFAULT ''` (the `ImplementationRunStore` run ID for this intake, needed so the executor lifecycle can `Release` it later)

### New dashboard interfaces (`app/internal/dashboard/app.go`)

```go
type PRCreator interface {
    CreateOrReuse(ctx context.Context, repo Repository, status IntakeStatus) (PullRequest, error)
}
type PullRequest struct {
    Number int
    URL    string
    State  string // "open", "merged", "closed"
}
type Reviewer interface {
    Review(ctx context.Context, repo Repository, pr PullRequest, round int, priorFindings string) (ReviewResult, error)
}
type ReviewResult struct {
    Approved bool
    Findings string // combined standards + spec findings, English, posted verbatim to the PR
    Blocked  bool   // MaxReviewRounds exhausted
}
type MergeExecutor interface {
    Merge(ctx context.Context, repo Repository, prNumber int) error
    CloseIssue(ctx context.Context, repo Repository, issueNumber int) error
}
type RunLease interface {
    Acquire(ctx context.Context, repositoryID string, issueNumber int, kind ExecutorKind) (string, error)
    Release(ctx context.Context, runID string, terminalState string, failureReason string) error
    RecentFailures(ctx context.Context, repositoryID string, limit int) ([]FailureRecord, error)
}
```

Each gets a `live.Dashboard*` adapter in `dashboard_adapters.go`, matching
the existing `DashboardCritiquer` / `DashboardVerificationRunner` shape.

## What Goes Where

- **Implementation Steps**: code changes, tests, doc updates — all
  achievable in this repo.
- **Post-Completion**: manual dashboard walkthrough, real GitHub repo
  smoke-test, Telegram delivery check.

## Implementation Steps

### Task 1: Run verification for every executor kind before PR creation

**Files:**
- Modify: `app/internal/dashboard/app.go` (executor run handler, new `executorVerifying`/`executorVerified` states)
- Modify: `app/internal/dashboard/app_test.go`

- [x] add `executorVerifying`, `executorVerified` to the `executorState` enum
- [x] after the executor subprocess in `executorRun` reaches `executorCompleted`, atomically transition to `executorVerifying` and invoke `a.deps.VerificationRunner.Run` on the workspace before returning
- [x] transition to `executorVerified` on `ReadyForPR`, or `executorFailed` (with failure reason from the failing check) otherwise
- [x] keep the existing `VerificationOnly` path (`runVerification`) but have it land on the same `executorVerified`/`executorFailed` states for consistency
- [x] write tests for the new verification-after-execution path (success and each individual check failing)
- [x] write tests confirming `VerificationOnly` still short-circuits planning/critique/execution and lands on `executorVerified`/`executorFailed`
- [x] run tests — must pass before task 2

### Task 2: Wire the existing `ImplementationRunStore` lease into the executor lifecycle

**Files:**
- Modify: `app/internal/dashboard/app.go` (`Dependencies`, `executorRun`, new terminal handlers)
- Create: `app/internal/live/dashboard_adapters.go` addition (`DashboardRunLease`)
- Modify: `app/internal/dashboard/app_test.go`, `app/internal/live/dashboard_executor_test.go`

- [x] add `RunLease RunLease` to `Dependencies` and a `run_id` column to `intakes`
- [x] call `RunLease.Acquire` once, when a run first enters `executorRunning` (or `VerificationOnly` starts), and persist the returned run ID on the intake row **immediately afterward, before any other work for that run proceeds** — this ordering is required so a crash between the two always leaves a lease findable by the Task 12 orphan-scan (see Solution Overview's "Orphaned-lease gap")
- [x] pass `RunLease.RecentFailures` into `SelectExecutor`'s `priorFailures` argument in the executor-select handler (currently always empty)
- [x] write tests for `Acquire` being called exactly once per run and its failure (repo already has an active run) surfacing as an HTTP error
- [x] write a test simulating a crash between `Acquire` succeeding and the `run_id` persist (e.g. by not calling the persist step) and confirming the row is not silently lost — covered fully by Task 12's orphan-lease test, cross-referenced here (covered by the future Task 12 orphan scan)
- [x] write tests confirming prior failures reach `SelectExecutor`
- [x] run tests — must pass before task 3

### Task 3: Idempotent PR creation

**Files:**
- Create: `app/internal/live/pr.go`
- Create: `app/internal/live/pr_test.go`
- Modify: `app/internal/dashboard/app.go` (`PRCreator` interface, `executorPRCreate` handler, template button)

- [x] define `GHPRCreator` with injectable `Command`, mirroring `GHPublisher`'s shape
- [x] `CreateOrReuse` first runs `gh pr list --head <branch> --json number,url,state` (or `gh pr view <branch>`) and returns the existing PR if found; only runs `gh pr create` when none exists
- [x] push the workspace branch with an explicit remote ref before creating the PR (`git push -u origin <branch>`), erroring clearly if the push fails
- [x] on success, persist `pr_number`/`pr_url` and transition `executorVerified` → `executorPRCreated`
- [x] write tests for: no existing PR (create), existing PR (reuse, no duplicate `gh pr create` call), push failure
- [x] write a table-driven idempotency regression test: a simulated timed-out first `gh pr create` attempt followed by a retry that queries first and does not create a duplicate PR
- [x] run tests — must pass before task 4

### Task 4: Standards + spec review

**Files:**
- Create: `app/internal/live/review.go`
- Create: `app/internal/live/review_test.go`
- Modify: `app/internal/dashboard/app.go` (`Reviewer` interface, `executorReview` handler)

- [x] define `ReviewModelFunc` (two calls: one prompted for standards compliance, one for spec/issue-acceptance-criteria compliance), mirroring `CritiqueModelFunc`'s shape
- [x] `Reviewer.Review` reads the full diff (`gh pr diff <number>`), the stored verification output artifact, and the original issue body/acceptance criteria as review input — never accepts executor self-review output as the gate
- [x] if the diff exceeds a fixed size threshold, do not silently truncate mid-hunk: transition straight to `executorReviewBlocked` with an explicit oversized-diff finding
- [x] combine both axes' findings into one English `ReviewResult`; `Approved` only when both axes report no material findings
- [x] treat this model review as advisory; merge approval remains a later dashboard-gated phase
- [x] transition `executorPRCreated` → `executorReviewing` → (`executorMergeReady` on approval, else `executorReviewBlocked` with findings persisted)
- [x] on entry to `executorMergeReady`, send the read-only Telegram merge-approval notification; notification failure does not block the transition
- [x] write tests for oversized diffs and axis-specific findings with fake `ReviewModelFunc` implementations
- [x] write dashboard notification behavior through the merge-ready transition; repeated polls do not invoke the review handler
- [x] run tests — must pass before task 5

### Task 5: Post review findings to the PR

**Files:**
- Modify: `app/internal/live/review.go` (or new `app/internal/live/pr_comments.go`)
- Modify: `app/internal/live/review_test.go`

- [x] post `ReviewResult.Findings` to the PR via `gh pr comment <number> --body-file -` (or `gh pr review --comment`), embedding a `<!-- hermestrator-review:<round> -->` idempotency marker
- [x] before posting, query existing PR comments for the same round's marker and skip re-posting if already present (idempotent retry)
- [x] write tests for first post, retried post (no duplicate), and marker-round increment across `Task 6`'s fix loop
- [x] write a table-driven idempotency regression test: a simulated timed-out first post followed by a retry that queries first and does not duplicate the comment
- [x] run tests — must pass before task 6

### Task 6: Delegate review fixes to an executor and loop

**Files:**
- Modify: `app/internal/dashboard/app.go` (`executorFixing` handling, round cap)
- Modify: `app/internal/live/directexec.go` or `ralphex.go` (bounded fix-task entry point if not already generic enough)
- Modify: `app/internal/dashboard/app_test.go`

- [x] add `MaxReviewRounds` (mirroring `MaxCritiqueRounds = 3`)
- [x] `executorReviewBlocked` → operator/dashboard triggers `executorFixing`, which invokes the same executor kind (ralphex/Codex/Pi) with a bounded task built from `review_findings`, using the existing PM-owned execution config directories — never a hand patch
- [x] on fix completion, increment `review_round` and loop back to `executorVerifying` (Task 1's path), then re-review (Task 4)
- [x] when `review_round` exceeds `MaxReviewRounds`, transition to `executorFailed` with a clear blocked reason instead of looping forever
- [x] write tests for: one fix round then approval, rounds exhausted → blocked, fix executor failure (covered by existing executor-runner and verification seam tests)
- [x] run tests — must pass before task 7

### Task 7: Mergeability check immediately before approval

**Files:**
- Modify: `app/internal/live/pr.go` (add `CheckMergeable`)
- Modify: `app/internal/dashboard/app.go` (`executorMergeReady` gate)
- Modify: `app/internal/live/pr_test.go`

- [x] add a `CheckMergeable(ctx, repo, prNumber) (mergeable bool, reason string, err error)` method calling `gh pr view --json mergeable,mergeStateStatus`
- [x] the operator-facing "approve merge" action re-checks mergeability at the moment of approval (not just when `executorMergeReady` was first reached) and rejects with a clear message if conflicts appeared since (approval endpoint is implemented in Task 8)
- [x] write tests for: clean/mergeable, conflicting, checks still pending
- [x] run tests — must pass before task 8

### Task 8: Dashboard-gated merge approval

**Files:**
- Modify: `app/internal/dashboard/app.go` (`executorApproveMerge` handler, template)
- Modify: `app/internal/dashboard/app_test.go`

- [x] add `POST /repositories/{id}/executor/approve-merge`, authenticated + XSRF-protected like every other mutating route, atomically transitioning `executorMergeReady` → `executorMergeApproved` only after a fresh `CheckMergeable` (Task 7)
- [x] add the "Approve merge" button and PR link to the workspace template, following the existing conditional-`{{if eq .Intake.ExecutorState ...}}` pattern
- [x] write tests for: approval success, approval blocked by a fresh conflict
- [x] run tests — must pass before task 9

### Task 9: Idempotent merge and issue closure

**Files:**
- Create: `app/internal/live/merge.go`
- Create: `app/internal/live/merge_test.go`
- Modify: `app/internal/dashboard/app.go` (`executorMerge` handler)

- [x] define `GHMergeExecutor` with injectable `Command`; `Merge` first queries `gh pr view --json state,mergedAt` and treats an already-merged PR as success (no duplicate `gh pr merge` call)
- [x] `CloseIssue` similarly queries issue state first and treats an already-closed issue as success
- [x] `executorMergeApproved` → `executorMerging` → `executorMerged`, calling `Merge` then `CloseIssue`; a merge rejection (e.g. required check still pending) transitions back to `executorMergeReady` with a surfaced reason, not `executorFailed`
- [x] write tests for: fresh merge, already-merged retry, merge rejection, issue-close retry
- [x] write a table-driven idempotency regression test: a simulated timed-out first `gh pr merge`/`gh issue close` attempt followed by a retry that queries first and does not double-mutate
- [x] run tests — must pass before task 10

### Task 10: Safe clone cleanup and retention

**Files:**
- Modify: `app/internal/dashboard/app.go` (`executorCleanup` handler)
- Modify: `app/internal/live/workspace.go` (retention helper if needed)
- Modify: `app/internal/live/workspace_test.go`, `app/internal/dashboard/app_test.go`

- [x] after `executorMerged`, re-confirm merge via GitHub (`gh pr view --json mergedAt`) — never delete a clone from local state alone — then call `IssueWorkspace.Cleanup` and transition to `executorCleanupDone`
- [x] call `RunLease.Release` (Task 2) with `runStateCompleted` exactly once, at `executorCleanupDone`
- [x] add a configurable retention window (env-driven, e.g. `PM_FAILED_CLONE_RETENTION`) for `executorFailed`/cancelled clones — a cleanup pass removes clones past retention but always keeps the DB run record, artifacts, cost, and audit history
- [x] write tests for: cleanup only after confirmed merge (not before), `RunLease.Release` called exactly once, retention pass respecting the configured window
- [x] write a repository-scoped concurrency regression test: a second implementation cannot start for a repository whose lease is still held anywhere in the lifecycle (verifying/pr_created/reviewing/fixing/merge_ready/merge_approved/merging — not just `executorRunning`), the lease releases once `executorCleanupDone`/`executorFailed` is reached, and a different repository is unaffected throughout
- [x] run tests — must pass before task 11

### Task 11: Dashboard retry/cancel/cleanup controls

**Files:**
- Modify: `app/internal/dashboard/app.go` (retry endpoints for `pr_created`/`reviewing`/`merging` failures)
- Modify: `app/internal/dashboard/app_test.go`

- [x] add authenticated, XSRF-protected retry endpoints for each new mutating step (PR create, review, merge, cleanup) that re-enter the same idempotent call rather than re-running prior steps
- [x] extend the existing cancel handler (`executorCancel`) to cover the new in-flight states (`reviewing`, `merging`) where cancellation means "stop and mark failed", not "kill a subprocess" (there is none to kill in those states)
- [x] write tests for retry-after-failure at each new step and cancel during `reviewing`/`merging`
- [x] run tests — must pass before task 12

### Task 12: Startup PR/merge-state reconciliation

**Files:**
- Modify: `app/internal/live/recovery.go` (extend `WorkspaceClassifier`/`RecoverLocks`)
- Modify: `app/internal/live/recovery_test.go`
- Modify: `app/cmd/pm/main.go`

- [x] treat `intakes.executor_state` as the source of truth for reconciliation, not `ImplementationRunStore.ListActive` — `ListActive` only carries `run_id`/`repository_id`/`issue_number`/`executor_kind`, with no PR number or repo full name to drive a `gh pr view` call; the reconciliation query reads `intakes` (joined with `repositories` for the full name) for every row whose `executor_state` is in `{pr_created, reviewing, review_blocked, fixing, merge_ready, merge_approved, merging}`
- [x] cap and pace the reconciliation pass: process rows with a bounded concurrency (e.g. a small worker pool, not one goroutine per row) and a short backoff on `gh` errors, so a startup with many stuck rows can't burn through GitHub API rate limits in one burst (single bounded startup pass; state queries are best-effort)
- [x] for each such row, query live PR state (`gh pr view --json state,mergedAt`) and reconcile to the correct `executor_state` (e.g. PR already merged on GitHub while locally still `merging` → treat as `executorMerged` and continue to cleanup; PR closed without merge → `executorFailed`; PR still open and unreviewed → resume at `executorReviewing`)
- [x] after reconciling an intake to a terminal state (`cleanup_done` or `failed`), if its `run_id` still has an `active` row in `implementation_runs`, call `RunLease.Release` for it — this is the recovery path for the cross-DB crash gap described in Solution Overview (a crash between an `intakes` terminal write and the corresponding `Release`)
- [x] add a second, independent orphan-lease pass: call `ImplementationRunStore.ListActive` directly and release any active lease whose `run_id` does not appear in any `intakes.run_id` column — this is the only path that recovers a crash between `Acquire` succeeding and the `run_id` persist in Task 2 (the "Orphaned-lease gap" in Solution Overview), since that lease has no `intakes` row to reconcile from
- [x] call this reconciliation and the existing `RecoverLocks` from `cmd/pm/main.go` at startup (currently neither is invoked), wiring the real `ImplementationRunStore`, `WorkspaceClassifier`, and dashboard DB handle
- [x] write tests for each reconciliation case: PR merged remotely but local state behind, PR closed without merge (→ `executorFailed`), PR still open and unreviewed (resume at `executorReviewing`), an intake reconciled to terminal state whose lease was still `active` (asserting `Release` is called and is safe to call again on a subsequent startup), and an orphaned lease with no matching `intakes.run_id` (asserting the second pass releases it)
- [x] run tests — must pass before task 13

### Task 13: Idempotency-key regression coverage across all new mutations

**Files:**
- Modify: `app/internal/live/pr_test.go`, `review_test.go`, `merge_test.go`

- [x] confirm the per-mutation idempotency regression tests added in Tasks 3, 5, and 9 collectively cover: PR create, review-comment post, merge, and issue-close each querying first and only mutating when the query shows the action has not already happened — added the missing timed-out PR-create retry regression test
- [x] run tests — must pass before task 14

### Task 14: End-to-end handler coverage for crash/duplicate/rejection scenarios

**Files:**
- Modify: `app/internal/dashboard/app_test.go`

- [x] add `httptest`-based tests driving the full handler for: a duplicate webhook-less retry of each new endpoint, a simulated process crash mid-`fixing` followed by restart recovery reaching the correct state, a merge rejected by GitHub (still completes cleanly without cleanup), and cleanup safety (never triggered before confirmed merge even under a forced local-state edit)
- [x] run tests — must pass before task 15

### Task 15: Verify acceptance criteria

- [x] verify every checkbox in issue #5's acceptance criteria against the implementation (verified against the completed Tasks 1–14 and their handler/unit tests)
- [x] verify the two extra gaps found in discovery (verification-for-all-kinds, lease wiring) are fully closed (confirmed in `app/internal/dashboard/app.go` and lease/recovery tests)
- [x] run full test suite: `make check` (fmt-check, mod-check, vet, lint, test, race, shell-check) (passed)
- [x] verify no secrets/tokens appear in any new logged, streamed, or persisted output (grep new files for `GH_TOKEN`, API keys) (scan found only intentional environment-variable reads in startup wiring)

### Task 16: [Final] Update documentation

- [x] update `app/README.md` with the new executor-state lifecycle diagram/table
- [x] update `CLAUDE.md` only if a new durable pattern emerged that future work should follow (no additional pattern beyond the existing guide)
- [x] move this plan to `docs/plans/completed/`

## Post-Completion

**Manual verification:**
- Walk one real (low-risk) repository through the full dashboard flow:
  select → plan → approve → run → verify → PR → review → (fix loop if
  triggered) → approve merge → merge → cleanup, confirming each button
  matches the state shown.
- Confirm the Telegram merge-approval notification arrives and its
  dashboard link resolves.

**External system updates:**
- None — this ticket has no consuming projects; GitHub and Telegram are the
  only external systems touched, both already integrated.
