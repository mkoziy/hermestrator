# Agentic discovery tools (ADR 0003)

## Overview

Implements [ADR 0003](../adr/0003-pm-discovery-reads-repository-on-demand.md)
and closes [issue #14](https://github.com/mkoziy/hermestrator/issues/14).

Discovery currently learns repository facts from `CloneIntake.Inspect`: a
fixed, one-shot push of four conventionally named files (`CONTEXT.md`,
`README.md`, `go.mod`, `package.json`) read before the operator's first
message. Repositories that document themselves differently give the PM
nothing, and discovery falls back to asking the operator questions the
repository could have answered itself.

This plan replaces `Inspect` outright with three read-only Genkit tools —
`pm_discovery_glob`, `pm_discovery_grep`, `pm_discovery_read` — scoped to the
same isolated intake clone, callable by the model on demand during a
discovery turn. The perimeter widens from four files to the whole clone
(including source), bounded by a per-turn tool-call cap and a system-prompt
guardrail limiting the tools to answering discovery questions, not code
review.

## Context (from discovery)

- Files/components involved:
  - `app/internal/live/intake.go` — `CloneIntake`, `Inspect`, `validateChild`/`regularChild` traversal guards.
  - `app/internal/live/adapters.go` — `OpenRouterModel`, `discoveryAgent`, `Stream`, the `Inspector`-derived `RepositoryEvidence` prompt prefix.
  - `app/internal/dashboard/app.go` — `Inspector` interface, `Conversation.RepositoryEvidence`, `artifactRepositoryEvidence`, the `inspection` column and its load/write sites, `startIntake`.
  - `app/cmd/pm/main.go` — construction order of `model` (`NewOpenRouterModel`) and `Intake` (`live.CloneIntake`).
- Related patterns found:
  - `MaxCritiqueRounds` (`app/internal/live/critique.go:20`) is the existing round-cap constant pattern to mirror for `MaxDiscoveryToolCalls`.
  - `synthesis.go`'s `GenkitSynthesizer` is the existing pattern for wrapping deterministic Go functions as typed Genkit tools via `genkitx.DefineTool`.
  - `redactSecrets` (`app/internal/redaction`) already runs on final discovery replies; ADR requires it on every tool result too.
  - `CloneIntake.validateChild`/`regularChild` (`intake.go`) already guard against symlink/traversal escapes for single path segments; glob/grep/read need the same guard generalized to arbitrary relative paths under the clone root.
- Dependencies identified: no new external dependencies — glob/grep use stdlib `path/filepath` (`WalkDir`, `Match`) and `regexp`, matching the ADR's explicit choice.

## Development Approach

- **Testing approach**: Regular (code first, then tests).
- Complete each task fully, with passing tests, before moving to the next.
- `Inspect`/`Inspector`/`repository-evidence`/`RepositoryEvidence` are removed outright in the same plan, not kept as a parallel fallback (ADR is explicit about this).
- Every new tool result must pass through `redactSecrets` before reaching the model.
- No new dependencies; stdlib only.
- Run `make check` after each task.

## Testing Strategy

- Unit tests in `app/internal/live/intake_test.go` for glob/grep/read behavior (matches, no-matches, traversal/symlink refusal, size cap).
- Unit tests in `app/internal/live/adapters_test.go` for the per-turn call cap and redaction of tool output. `NewOpenRouterModel` hardcodes the real OpenRouter HTTPS endpoint, and the one existing `Stream` test (`TestOpenRouterStreamFinishesAfterFirstAgentTurn`) avoids the real model path entirely by hand-building a fake `genkitx.DefineCustomAgent` — neither is usable to exercise the real tool-calling loop. Instead, build the test's own `genkit.Genkit` registry and call `genkit.DefineModel(g, "openrouter/<test-model>", ...)` with a `ModelFunc` that scripts N rounds of tool-request parts followed by a final text response, then call `discoveryAgent` (or an `OpenRouterModel` assembled by hand, as the existing test does) against that registry. This drives `GenerateStream`/`ai.WithTools` for real, without touching the hardcoded OpenRouter URL.
- Handler-level tests in `app/internal/dashboard/app_test.go` updated wherever they asserted on `RepositoryEvidence`/`repository-evidence`/`Inspector`, replaced with coverage that discovery no longer depends on eager inspection.
- No UI/e2e test tooling in this project; the `net/http` handler tests are the primary seam per `CLAUDE.md`.

## Progress Tracking

- Mark completed items with `[x]` immediately when done.
- Add newly discovered tasks with an ➕ prefix.
- Document issues/blockers with a ⚠️ prefix.

## Solution Overview

- `CloneIntake` gains `Glob`, `Grep`, and `Read` methods, all validated through a generalized `regularDescendant(root, relativePath)` guard (replacing the single-segment `regularChild` for these three).
- `NewOpenRouterModel` takes the concrete `CloneIntake` (same package, no interface needed — a one-implementation `DiscoveryReader` interface would be an unrequested abstraction) so it can register `pm_discovery_glob`/`pm_discovery_grep`/`pm_discovery_read` as Genkit tools at startup, mirroring `synthesis.go`'s tool-registration pattern.
- `main.go` is reordered so `live.CloneIntake` is constructed before `live.NewOpenRouterModel`, and the same value is passed to both the model constructor and `dashboard.Dependencies.Intake`.
- The isolated clone's path already lives in `intakes.clone_path` (`status.Path`). `dashboard.Conversation` gains a `ClonePath` field (replacing `RepositoryEvidence`), populated the same way `RepositoryEvidence` used to be. `adapters.go`'s `Stream` injects `ClonePath` and a fresh per-turn call counter into `context.Context` before generating, so the tool functions (registered once, called many times) know which clone to read and how many calls remain.
- `MaxDiscoveryToolCalls = 10` caps calls per operator turn. When the cap is hit, tools return a short "budget exhausted" string instead of an error, so the model still produces an answer instead of the turn failing. The counter is set on the `context.Context` passed to `agent.Connect` (not per-`SendText`, which only enqueues on a channel) so it naturally scopes to one turn; use `atomic.Int32` — it is correct whether Genkit executes tool calls sequentially or concurrently within a step, costs nothing extra over a mutex-guarded int, and needs no investigation into Genkit's internals to be safe.
- Each Genkit tool's model-facing input struct carries only what the model should choose: `pm_discovery_glob`/`pm_discovery_grep` take `struct{ Pattern string }`, `pm_discovery_read` takes `struct{ RelativePath string }`. None of the three expose `Path`/clone root as a model-supplied field — the clone path is resolved from the per-turn context value the tool function reads internally, never from model input.
- If the per-turn context value is missing or `ClonePath` is empty when a tool fires (e.g. a stale session, or `Stream` invoked before an intake clone finished), each tool function returns a fixed "no repository clone available" string rather than walking or joining against an empty path.
- The discovery system prompt gains a sentence scoping the tools to requirements/architecture/conventions questions, explicitly not code-quality or correctness review (that stays the `Reviewer` phase's job).
- `Inspect`, `Inspector`, `artifactRepositoryEvidence`, and the `inspection` DB column's read/write call sites are deleted. The `inspection` column itself is left in the schema (dropping SQLite columns needs a migration path this project doesn't have yet, and an unused, never-written column is harmless) — noted as a deliberate simplification, not a gap in the acceptance criteria.

## Technical Details

- `regularDescendant(root, relativePath string) (string, error)`: resolves `root` via `filepath.EvalSymlinks`, joins `relativePath`, rejects `..`-escaping and symlinked path segments at every level (not just the leaf), returns the validated absolute path. Existing `validateChild` becomes a thin call into this with a single-segment path.
- `CloneIntake.Glob(ctx, path, pattern string) (string, error)`: `filepath.WalkDir` from `path`, matching `filepath.Base(rel)` (the file's base name, not the full relative path) against `pattern` with `filepath.Match` — this is what lets `*.md` find `docs/adr/0001.md`, since `filepath.Match`'s `*` does not cross `/` and there is no `**` support. Document this base-name-only limitation in the tool's Genkit description so the model doesn't expect full-path glob patterns. Returns a newline-joined list of matching relative paths (capped, e.g. 200 entries) or "no matches".
- `CloneIntake.Grep(ctx, path, pattern string) (string, error)`: compiles `pattern` with `regexp.Compile` (RE2, no catastrophic-backtracking risk; a pattern-length bound is a cheap sanity cap, not a ReDoS mitigation), walks the clone skipping `.git`, matches line-by-line against regular files under a total output cap (matching `Inspect`'s existing 16 KiB discipline), returns `path:line: text` per match.
- `CloneIntake.Read(ctx, path, relativePath string) (string, error)`: validates via `regularDescendant`, reads up to 16 KiB (same cap `Inspect` used), refuses non-regular files exactly like `regularChild` does today.
- Context-carried per-turn state: a small unexported struct (clone path + call counter) set via `context.WithValue` on the `context.Context` passed to `agent.Connect` in `Stream` (this ctx is what `GenerateStream` and the tool functions actually run under — see Task 5), read by the three tool functions. Exceeding `MaxDiscoveryToolCalls` returns a fixed string, not an error.
- All three tool results pass through `redactSecrets` before being returned from the tool function.

## What Goes Where

- **Implementation Steps** (`[ ]`): all code, tests, and doc changes below — everything is achievable within this repository.
- **Post-Completion**: none — this is a pure backend change with existing `make check` coverage; no external system needs updating.

## Implementation Steps

### Task 1: Generalize the traversal guard to arbitrary relative paths

**Files:**
- Modify: `app/internal/live/intake.go`
- Modify: `app/internal/live/intake_test.go`

- [x] add `regularDescendant(root, relativePath string) (string, error)` in `intake.go`, resolving symlinks and rejecting `..`-escapes and symlinked intermediate segments
- [x] reimplement `validateChild`/`regularChild` in terms of `regularDescendant` so existing behavior (and existing tests) is unchanged
- [x] write tests for `regularDescendant` accepting nested regular files
- [x] write tests for `regularDescendant` refusing `..`-escapes, symlinked intermediate directories, and symlinked leaf files
- [x] run tests — `go test ./internal/live` passes; `make check` is unavailable (no target)

### Task 2: Implement `CloneIntake.Glob` and `CloneIntake.Grep`

**Files:**
- Modify: `app/internal/live/intake.go`
- Modify: `app/internal/live/intake_test.go`

- [ ] implement `Glob(ctx, path, pattern string) (string, error)` using `filepath.WalkDir` + `filepath.Match` against `filepath.Base(rel)`, skipping `.git`, capping the number of matches returned
- [ ] implement `Grep(ctx, path, pattern string) (string, error)` using `regexp.Compile` + line-scanning over regular files, skipping `.git`, capping total output bytes
- [ ] write tests for `Glob` matching a base-name pattern across subdirectories (e.g. `*.md` matching `docs/adr/0001.md`), no-matches, and the documented base-name-only limitation
- [ ] write tests for `Grep` matching, no-matches, invalid regex error, and output cap
- [ ] run tests — must pass before task 3

### Task 3: Remove `Inspector`, `RepositoryEvidence`, and the `repository-evidence` artifact from the dashboard package

**Files:**
- Modify: `app/internal/dashboard/app.go`
- Modify: `app/internal/dashboard/app_test.go`

This runs before `CloneIntake.Inspect` is deleted (Task 4) so every package compiles and tests pass at each task boundary — the dashboard package's `Inspector` type-assertion call site must stop referencing the interface before the interface's sole implementation is removed.

- [ ] delete the `Inspector` interface and its call site in `startIntake`
- [ ] delete `Conversation.RepositoryEvidence`, `artifactRepositoryEvidence`, and the `intake_artifacts` insert that created the `repository-evidence` artifact in `startIntake`
- [ ] stop reading/writing the `inspection` column (leave the column itself in the schema — dropping it needs a migration path this project doesn't have; noted as a deliberate simplification)
- [ ] add `Conversation.ClonePath string`, populated from `intakes.clone_path` the same place `RepositoryEvidence` used to be loaded
- [ ] update/remove tests asserting on `RepositoryEvidence`/`repository-evidence` (e.g. `TestIntakePersistsInspectableRepositoryEvidence`); add a test that `startIntake` no longer creates a `repository-evidence` artifact
- [ ] run tests — must pass before task 4

### Task 4: Implement `CloneIntake.Read` and remove `Inspect`

**Files:**
- Modify: `app/internal/live/intake.go`
- Modify: `app/internal/live/intake_test.go`

- [ ] implement `Read(ctx, path, relativePath string) (string, error)` via `regularDescendant`, 16 KiB cap, refusing non-regular files
- [ ] delete `CloneIntake.Inspect` (its last caller, the dashboard package's `Inspector` assertion, was removed in Task 3)
- [ ] write tests for `Read` success, size cap, non-regular-file refusal, and traversal refusal
- [ ] remove/replace tests that exercised `Inspect` (`TestCloneIntakeInspectionRefusesSymlinkedEvidence` etc.) with equivalents for `Read`
- [ ] run tests — must pass before task 5

### Task 5: Wire discovery tools into `OpenRouterModel`, cap per-turn calls, and reorder `main.go`

**Files:**
- Modify: `app/internal/live/adapters.go`
- Modify: `app/internal/live/adapters_test.go`
- Modify: `app/cmd/pm/main.go`

Includes the `main.go` caller update in the same task as the `NewOpenRouterModel` signature change, so the module compiles at this task's test gate instead of breaking until a later task.

- [ ] add `const MaxDiscoveryToolCalls = 10` in `adapters.go`, mirroring `MaxCritiqueRounds`'s pattern
- [ ] change `NewOpenRouterModel` to accept the concrete `CloneIntake` and register `pm_discovery_glob`, `pm_discovery_grep`, `pm_discovery_read` as Genkit tools via `genkitx.DefineTool`, wired into `discoveryAgent` via `ai.WithTools`
- [ ] add the per-turn call-counter context value, injected on the `context.Context` passed to `agent.Connect` in `Stream`; each tool checks and increments it, returning a fixed "tool budget exhausted" string instead of an error once `MaxDiscoveryToolCalls` is exceeded
- [ ] pass every tool result (`Glob`, `Grep`, and `Read`, all three) through `redactSecrets` before returning it
- [ ] update `Stream` to inject `conversation.ClonePath` and a fresh counter into the `Connect` ctx instead of prefixing the prompt with `RepositoryEvidence`
- [ ] update the discovery system prompt to scope tool usage to requirements/architecture/conventions questions, not code-quality/correctness review
- [ ] in `main.go`, construct `live.CloneIntake{...}` before `live.NewOpenRouterModel(...)` and pass the same value into both `NewOpenRouterModel` and `dashboard.Dependencies.Intake`
- [ ] write tests that a turn issuing 11 tool calls still completes with an answer (cap enforced, no error)
- [ ] write tests that output from each of the three tools is redacted before reaching the model/response
- [ ] run tests — must pass before task 6

### Task 6: Verify acceptance criteria

- [ ] verify `Inspect`/`Inspector`/`repository-evidence` are fully removed (grep confirms no references outside the ADR doc)
- [ ] verify discovery can answer questions using `glob`/`grep`/`read` over the full clone, not just the 4 legacy files (manual or integration-style test against a fixture repo with non-standard docs)
- [ ] verify a turn cannot exceed `MaxDiscoveryToolCalls` calls and degrades gracefully instead of erroring
- [ ] verify tool results are redacted before reaching the model
- [ ] verify the system prompt keeps tool usage scoped to discovery, not code review
- [ ] run full test suite: `make check` (from `app/`)

### Task 7: Update documentation

- [ ] update `docs/adr/0003-pm-discovery-reads-repository-on-demand.md`'s "Consequences" section if any detail changed during implementation (e.g. exact match/output caps)
- [ ] update `docs/specs/20260726-genkit-pm-dashboard.md` / `docs/tickets/20260726-genkit-pm-dashboard.md` only if they still describe the old fixed-file `Inspect` behavior (current check found none — confirm still true)
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion

None — this change is fully covered by the existing `make check` / `httptest` seam; no manual verification or external system updates are required.
