# Model intake synthesis as bounded Genkit capabilities

## Overview

PR #7 (closes issue #3) implements tracked-work intake end to end and every
acceptance criterion in the issue now passes (`make check` is clean, 20+
HTTP-seam tests cover discovery/confirm/publish/persist/restart/cleanup). One
architecture-conformance item from the issue's own follow-up list remains
unresolved: `grillWithDocs`, `toSpec`, `toTickets`, and `assessADR`
(`app/internal/dashboard/app.go:1447-1560`) are plain Go functions called
directly by the HTTP handler. CLAUDE.md requires the PM service to model
capabilities as bounded Genkit tools rather than local helpers; today only the
discovery turn goes through a real Genkit agent (`app/internal/live/adapters.go`).

This plan wraps those four functions as named, schema-typed `genkit.DefineTool`
capabilities, invoked directly (no LLM in the loop — synthesis stays
deterministic) via `Tool.RunRaw`. This is the smallest change that satisfies
both the issue follow-up and CLAUDE.md, without introducing LLM
non-determinism into artifact synthesis, which no acceptance criterion asks
for. The issue follow-up names three capabilities (`grill-with-docs`,
`to-spec`, `to-tickets`); this plan also wraps the fourth sibling,
`assessADR`, since it funnels through the same `synthesizeArtifacts` call and
leaving it as a bare local helper would be inconsistent.

Accepted cost: `Tool.RunRaw` round-trips through JSON (see Context), so each
non-`string` result needs a re-marshal/unmarshal step instead of a plain type
assertion. That's the price of registry-backed tools (naming, schema,
telemetry) for functions that don't otherwise need serialization — judged
worth it here because CLAUDE.md's rule is about capability modeling, not
performance, and the values involved are small strings/structs.

Explicit interpretation call, stated plainly rather than implied: these four
tools have **no agentic consumer**. Nothing ever lets an LLM decide to call
them — the HTTP handler calls `RunRaw` directly, every time, deterministically.
This is the minimal-compliance reading of "model as bounded Genkit
capabilities" (named, schema-typed, registry-visible units), not the fuller
architectural one where an agent exercises judgment over when to invoke them.
The fuller version (a synthesis agent that chooses how to call these tools)
was considered and explicitly deferred — see the "B: agent-driven" option
rejected during planning — because no acceptance criterion asks for
non-deterministic synthesis output and it would need new prompt design,
non-deterministic tests, and a real reason to exist beyond a checkbox. If
that reason later shows up (e.g. synthesis needs judgment a regex can't
provide), revisit this plan's approach rather than extending it.

This entire plan is discretionary: PR #7 is already `MERGEABLE`, `make check`
is clean, and every acceptance criterion in issue #3 passes without this
change. Doing this work is the user's explicit choice (architecture
conformance with CLAUDE.md and the issue's own follow-up note), not a merge
blocker. If priorities change, PR #7 can merge as-is and this becomes its own
follow-up ticket instead.

## Context (from discovery)

- `app/internal/dashboard/app.go`: houses `Dependencies` (small port
  interfaces like `Intake`, `Publisher`, `Model`, `ContextUpdater`) and the
  pure synthesis functions at lines 1447 (`synthesizeArtifacts`), 1467
  (`assessADR`), 1507 (`grillWithDocs`), 1535 (`toSpec`), 1539 (`toTickets`).
  Called from `synthesizeIntake` (line 1034/1054).
- `app/internal/live/adapters.go`: production adapters. Already creates a
  `*genkit.Genkit` in `NewOpenRouterModel` (line 127) and registers one tool
  (`pm_discovery_context`, line 136) consumed by a `aix.Agent` via
  `genkitx.DefineCustomAgent`.
- Pattern already established: `dashboard` package defines small interfaces;
  `live` package implements them against real services (Genkit, GitHub);
  `app_test.go` uses in-package fakes so the HTTP handler stays the test seam
  — this plan follows that pattern rather than importing genkit into
  `dashboard`.
- `aix.Tool[In, Out].RunRaw(ctx, input any) (any, error)` allows direct
  invocation of a Genkit tool outside an agent's tool-calling loop — confirmed
  via `go doc github.com/firebase/genkit/go/ai/exp Tool`. Critically, `RunRaw`
  round-trips through JSON internally (`ToolDef.RunRaw` → `RunRawMultipart` in
  genkit v1.11.0's `ai/tools.go:533-561`, `ai/gen.go:373`): it
  `json.Marshal`s the input and `json.Unmarshal`s the output into `any`. So the
  returned value is JSON-decoded, not the concrete `Out` type — a `[]string`
  comes back as `[]interface{}`, a struct comes back as `map[string]interface{}`.
  Only bare `string` results survive a direct type assertion unchanged. `aix.Tool`
  exposes no typed `Run(ctx, In) (Out, error)` — `RunRaw`/`RunRawMultipart` are
  the only call methods (`ai/exp/tools.go:88-96`). Every call site in this plan
  must re-marshal/unmarshal into the concrete type instead of type-asserting.
- JSON-round-trip safety checked directly: `Conversation`, `Repository`, and
  `Message` (`app.go:34-47`) have only exported fields of plain-JSON-safe
  types (`string`, `time.Time`, `[]Message`, `[]PendingTurn`, `Reply`,
  `Status` — all themselves exported-primitive structs). No unexported
  fields, funcs, or channels. Safe as `RunRaw` input/output types without
  data loss.
- Tool-registry scope checked directly: `ai.DefineTool`/`ToolDef.Register`
  take an explicit `api.Registry` parameter (`ai/tools.go:304,522`) rather
  than writing to a package-global registry, and `aix.DefineTool` forwards
  the same `r api.Registry` (`ai/exp/tools.go:157`). Each `*genkit.Genkit`
  instance is its own registry, so `synthesis_test.go` calling
  `genkit.Init` per test (same pattern as `adapters_test.go`) cannot collide
  on tool names with `main.go`'s instance or with `adapters_test.go`'s.
- Beta-API exposure: this adds 4 more call sites depending on
  `genkit.DefineTool`/`Tool.RunRaw` internals (the JSON round-trip behavior
  specifically), on top of the Agents API CLAUDE.md already flags as beta.
  A genkit version bump that changes `RunRaw`'s output shape would break
  `decodeToolResult` without a compile error — covered by Task 4's
  deep-equal tests, but `go.mod` version bumps touching `firebase/genkit`
  should re-run those tests deliberately, not just `go test ./...` in CI.
- `cmd/pm/main.go` wires `Dependencies` for the running server.

## Development Approach

- **Testing approach**: Regular (code first, then tests) — existing tests for
  the four functions already pin their behavior; keep them passing through
  the refactor, add new tests for the tool-backed adapter and the
  interface-selection wiring.
- Keep the deterministic logic unchanged; only change how it's invoked and
  named.
- Do not touch discovery-turn agent code (`discoveryAgent`) — out of scope.
- Update this plan file if any task's scope changes.

## Solution Overview

1. Export the four pure functions from `dashboard` (rename, no logic change)
   so `live` can call them from inside tool functions.
2. Add a `Synthesizer` port interface to `dashboard` with context-taking
   methods, add it to `Dependencies`, default to a local (non-Genkit)
   implementation in `New()` when unset. (`ContextUpdater` is optional via a
   runtime type-assertion on `Intake`, not a `New()` default — this is a new,
   simpler defaulting pattern, not a reuse of that one.)
3. Change `synthesizeArtifacts` from a free function into something that
   calls `a.deps.Synthesizer` instead of the package-level functions directly.
4. Add `live.GenkitSynthesizer`: registers 4 `genkit.DefineTool` capabilities
   wrapping the exported `dashboard` functions, and implements `Synthesizer`
   by calling `Tool.RunRaw`.
5. Wire it in `cmd/pm/main.go` using the existing `*genkit.Genkit` instance
   (expose it from `OpenRouterModel`).

## Technical Details

New interface in `dashboard/app.go` (near the other port interfaces, ~line 104):

```go
// Synthesizer turns settled discovery output into drafts. Implementations
// must not call GitHub or grant write authority; see Intake and Publisher.
type Synthesizer interface {
	GrillWithDocs(context.Context, Conversation) ([]string, error)
	ToSpec(context.Context, Repository, []string) (string, error)
	ToTickets(context.Context, Repository, []string) (string, error)
	AssessADR(context.Context, string) (assessment, proposal string, err error)
}
```

`Dependencies` gains `Synthesizer Synthesizer`. `New()` defaults it to
`localSynthesizer{}` (new unexported type in `dashboard`, wraps the exported
pure functions with no I/O) when nil, so the ~20 existing test call sites
that build `Dependencies{...}` without a `Synthesizer` keep compiling and
behaving exactly as today.

`synthesizeArtifacts` becomes a method `(a *application) synthesizeArtifacts(ctx
context.Context, repo Repository, conversation Conversation) ([]Artifact, error)`
calling `a.deps.Synthesizer.GrillWithDocs/ToSpec/ToTickets/AssessADR`, threading
the returned error into the existing `http.Error(..., http.StatusInternalServerError)`
path in `synthesizeIntake`.

`live.GenkitSynthesizer` (new file `app/internal/live/synthesis.go`):

```go
type GenkitSynthesizer struct {
	grillWithDocs *aix.Tool[dashboard.Conversation, []string]
	toSpec        *aix.Tool[specInput, string]
	toTickets     *aix.Tool[ticketsInput, string]
	assessADR     *aix.Tool[string, adrResult]
}

func NewGenkitSynthesizer(g *genkit.Genkit) *GenkitSynthesizer { ... }
```

Each tool's `fn` body calls the corresponding exported `dashboard` function
and returns its result — no new logic, just naming/schema + registration.
`RunRaw` returns `any` holding JSON-decoded data, not the concrete `Out` type
(see Context) — so the `Synthesizer` methods on `GenkitSynthesizer` must
re-encode: `json.Marshal` the `RunRaw` result, then `json.Unmarshal` into the
concrete `Out` (e.g. `[]string`, or an `adrResult` struct with `Assessment`/
`Proposal` fields). A bare type assertion (`result.([]string)`) is wrong and
must not be used — it fails for `GrillWithDocs` (`[]interface{}` at runtime)
and `AssessADR` (`map[string]interface{}` at runtime); only the two
`string`-returning tools (`ToSpec`, `ToTickets`) would happen to work with a
raw assertion, so don't rely on that either — use the same marshal/unmarshal
helper for all four for consistency.

`OpenRouterModel` exposes its `*genkit.Genkit` (e.g. a `Genkit() *genkit.Genkit`
method) so `cmd/pm/main.go` can build `live.NewGenkitSynthesizer(model.Genkit())`
and set `deps.Synthesizer` alongside the existing `deps.Model`.

## What Goes Where

- **Implementation Steps**: all code changes below are completable in this
  repo.
- **Post-Completion**: merging PR #7 and closing issue #3 requires the
  repository owner's action (GitHub merge button / branch protection), not
  something this plan executes.

## Implementation Steps

### Task 1: Export the four synthesis functions

**Files:**
- Modify: `app/internal/dashboard/app.go`
- Modify: `app/internal/dashboard/app_test.go`

- [x] rename `grillWithDocs` → `GrillWithDocs`, `toSpec` → `ToSpec`,
      `toTickets` → `ToTickets`, `assessADR` → `AssessADR` in
      `app/internal/dashboard/app.go` (all call sites in the same file,
      including `synthesizeArtifacts`)
- [x] update the two direct test call sites (`app_test.go:471`, `:519`) to
      the new exported names
- [x] run tests — must pass before task 2: `cd app && go test ./...`

### Task 2: Add the `Synthesizer` port and default local implementation

**Files:**
- Modify: `app/internal/dashboard/app.go`

- [x] add the `Synthesizer` interface (context-taking methods per Technical
      Details) near the other port interfaces (~line 104)
- [x] add unexported `localSynthesizer struct{}` implementing `Synthesizer`
      by calling `GrillWithDocs`/`ToSpec`/`ToTickets`/`AssessADR` directly,
      no I/O, returns nil error
- [x] add `Synthesizer Synthesizer` field to `Dependencies`
- [x] in `New()`, default `deps.Synthesizer` to `localSynthesizer{}` when nil
- [x] write a test that `New(Dependencies{})` (no `Synthesizer` set) still
      synthesizes artifacts correctly end-to-end through the HTTP seam
      (success case)
- [x] write a test that a fake `Synthesizer` returning an error from any
      method causes `synthesizeIntake` to respond
      `http.StatusInternalServerError` instead of panicking (error case)
- [x] run tests — must pass before task 3

### Task 3: Route `synthesizeArtifacts` through `deps.Synthesizer`

**Files:**
- Modify: `app/internal/dashboard/app.go`
- Modify: `app/internal/dashboard/app_test.go`

- [x] change `synthesizeArtifacts` into `(a *application) synthesizeArtifacts`
      taking `ctx context.Context` and returning `([]Artifact, error)`,
      calling `a.deps.Synthesizer.GrillWithDocs`/`ToSpec`/`ToTickets`/`AssessADR`
      instead of the package-level functions
- [x] update the call site in `synthesizeIntake` (line ~1054) to handle the
      new error return
- [x] update the direct-call test at `app_test.go:471` to construct an
      `application` (via existing `mustApp` helper) and call the method, or
      drop it in favor of the Task 2 HTTP-seam tests if it becomes redundant
- [x] run tests — must pass before task 4

### Task 4: Register Genkit tools in `live.GenkitSynthesizer`

**Files:**
- Create: `app/internal/live/synthesis.go`
- Create: `app/internal/live/synthesis_test.go`

- [x] define `specInput{Repo dashboard.Repository; Resolved []string}` and
      `ticketsInput` (same shape) and `adrResult{Assessment, Proposal string}`
      request/response types
- [x] define `GenkitSynthesizer` struct holding the 4 `*aix.Tool[...]` fields
- [x] implement `NewGenkitSynthesizer(g *genkit.Genkit) *GenkitSynthesizer`,
      registering `pm_grill_with_docs`, `pm_to_spec`, `pm_to_tickets`,
      `pm_assess_adr` via `genkitx.DefineTool`, each `fn` delegating to the
      matching exported `dashboard` function
- [x] add an unexported `decodeToolResult[T any](result any) (T, error)`
      helper that `json.Marshal`s `result` then `json.Unmarshal`s into `T`,
      returning a wrapped error (not a panic) on either failure
- [x] implement the 4 `Synthesizer` interface methods on `*GenkitSynthesizer`
      via `tool.RunRaw(ctx, input)` followed by `decodeToolResult[Out]` — do
      **not** type-assert the `RunRaw` result directly (it's JSON-decoded
      `any`, e.g. `[]interface{}`/`map[string]interface{}`, not the concrete
      type; see Technical Details)
- [x] write a test using `genkit.Init(ctx, genkit.WithExperimental())` (same
      pattern as `adapters_test.go:159`) that registers the tools and asserts
      `reflect.DeepEqual` between each method's output and calling the
      matching `dashboard` function directly — deep-equal, not just
      non-nil/type checks, so the decode step is actually verified (success
      cases; this is what would have caught the `RunRaw` JSON round-trip
      mismatch)
- [x] write a test asserting a `RunRaw` error or a `decodeToolResult` decode
      failure surfaces as a Go `error` rather than panicking (error case)
- [x] run tests — must pass before task 5

### Task 5: Wire `GenkitSynthesizer` into the running server

**Files:**
- Modify: `app/internal/live/adapters.go`
- Modify: `app/cmd/pm/main.go`

- [x] add a `Genkit() *genkit.Genkit` accessor on `OpenRouterModel` returning
      the `g` created in `NewOpenRouterModel` (store it on the struct)
- [x] in `main.go`, after constructing the `OpenRouterModel`, set
      `deps.Synthesizer = live.NewGenkitSynthesizer(model.Genkit())`
- [x] write/extend a `main.go` smoke check only if one already exists for
      dependency wiring; otherwise rely on Task 4's tests plus `make check`
      (no test framework currently covers `main.go` wiring — do not add one
      for this alone)
- [x] add one HTTP-seam test in `synthesis_test.go` (live package, to avoid
      circular import) that builds `Dependencies` with
      `Synthesizer: live.NewGenkitSynthesizer(<a genkit.Init'd instance>)`
      instead of the `localSynthesizer{}` default, and drives
      `POST /repositories/{id}/intake/synthesize` end to end — every other
      seam test exercises only `localSynthesizer`, so without this the
      production `RunRaw`/decode path is never covered by an integration test
- [x] run full suite — must pass before task 6: `cd app && go test ./...`

### Task 6: Verify acceptance criteria

- [ ] verify `grillWithDocs`/`toSpec`/`toTickets`/`assessADR` behavior is
      byte-for-byte unchanged for existing inputs (Task 1-3 tests still pass
      unmodified in assertions, only call syntax changed)
- [ ] verify production wiring (`main.go`) builds and starts against the
      Genkit-backed `Synthesizer`
- [ ] run `make check` from repo root — must be clean
- [ ] confirm no `dashboard` package file imports
      `github.com/firebase/genkit/*` (keeps the HTTP-seam test philosophy in
      CLAUDE.md intact)

### Task 7: Update issue #3 checklist and PR #7 description

- [ ] check off the now-satisfied acceptance criteria in issue #3 that were
      stale relative to code (one-question discovery, repo-resolvable
      inspection, ADR gating, tracer-bullet tickets, abandoned cleanup, HTTP
      seam coverage, idempotent publish/promotion recovery)
- [ ] check off "Model `grill-with-docs`, `to-spec`, and `to-tickets` as
      bounded Genkit capabilities" in the follow-ups section
- [ ] update `docs/tickets/20260726-genkit-pm-dashboard.md` if it references
      this follow-up as outstanding
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion

**External system updates:**
- Push the branch, verify PR #7's CI (if configured) is green, and merge —
  merging is a repository-owner action outside this plan's scope.
- Closing issue #3 happens automatically on merge (PR body already contains
  "Closes #3").
