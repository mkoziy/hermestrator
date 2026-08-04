# ADR 0003: Let discovery read the repository on demand, including source

- Status: Accepted
- Date: 2026-08-03

## Context

Discovery (`grill-with-docs`) currently learns repository facts from
`CloneIntake.Inspect`: a fixed push of four conventionally named files —
`CONTEXT.md`, `README.md`, `go.mod`, `package.json` — read once, before the
operator's first message. Repositories that document themselves differently
(a different manifest format, docs living elsewhere) give the PM nothing,
and it falls back to asking the operator questions the repository itself
could have answered.

This whitelist also sits below the layer where the project has otherwise
kept a firm line: CLAUDE.md and the dashboard spec restrict the PM to
documentation during discovery, and treat source code as something only a
delegated executor (ralphex/Codex/Pi) touches, after a fresh, issue-scoped
clone, later in the workflow. Widening what discovery reads therefore
touches a boundary the project has been deliberate about, which is why this
gets an ADR rather than a plan-only change.

## Decision

Discovery gets three read-only tools — `glob`, `grep`, `read` — scoped to
the same isolated intake clone `Inspect` already used, replacing `Inspect`
outright rather than running alongside it. The perimeter is the entire
clone, including source files, not just the four whitelisted names.

This does not conflict with CLAUDE.md's "the PM never patches production
code by hand": that constraint is about writes. Reading source during
discovery to answer a requirements or architecture question is a different
act from editing it, and the intake clone remains write-restricted to
`ContextUpdater.UpdateContext` (glossary updates to `CONTEXT.md`) exactly as
before.

Two limits keep the wider read boundary bounded rather than open-ended:

- **A per-operator-turn cap of 10 tool calls** (`MaxDiscoveryToolCalls`),
  not a whole-intake budget — mirrors the existing round-cap pattern
  (`MaxCritiqueRounds`) elsewhere in the codebase, applied at the turn
  granularity since that is where runaway tool-looping would actually show
  up.
- **A system-prompt guardrail** scoping the tools' purpose to answering
  discovery questions about requirements, architecture, and conventions —
  explicitly not code-quality or correctness review, which stays the sole
  responsibility of the later, independent `Reviewer` phase
  (`docs/plans/20260803-reviewed-pr-and-recovery-lifecycle.md`). Without
  this, discovery could drift into duplicating a review that already has an
  owner.

Every tool result is passed through the existing `redactSecrets` before
reaching the model — previously only applied to the final response text,
now also to intermediate tool output, since `grep`/`read` over arbitrary
source can surface secrets a curated 4-file push never risked.

## Alternatives considered

- **Keep the fixed whitelist, just extend the list of names** (e.g. add
  `ARCHITECTURE.md`, `docs/adr/**`, `CLAUDE.md`, `pyproject.toml`,
  `Cargo.toml`) — simpler, no new tool surface, but still blind to any
  repository whose documentation doesn't match the list, and doesn't scale
  to arbitrary third-party repositories.
- **Let the repository declare its own doc paths** (a config file
  Hermestrator reads first) — shifts the burden to every registered
  repository's maintainers and still needs a fallback for repositories that
  never write it.
- **Keep `Inspect` as a baseline seed, add tools on top** — considered and
  rejected during discussion: running both means two mechanisms answering
  the same question, and the fixed push adds no coverage the on-demand
  tools don't already provide once they exist.

## Consequences

- The PM's read boundary during discovery grows from four named files to
  the entire repository (read-only). This is the acceptance criterion this
  ADR exists to record: it is a deliberate, not incidental, widening.
- Tool use is bounded per operator turn: at most 10 calls, glob returns at
  most 200 matching paths, and read and grep each return at most 16 KiB.
  Glob patterns match file base names, so `*.md` searches nested
  directories; grep patterns use Go's regular-expression syntax and scan at
  most 32 MiB of regular-file data per call.
- `Inspector`, `CloneIntake.Inspect`, the `repository-evidence` artifact,
  and the prompt-prefix plumbing of `RepositoryEvidence` are removed, not
  kept as a fallback — see the completed implementation plan in
  `docs/plans/completed/20260804-agentic-discovery-tools.md`
  for the removal task.
- `redactSecrets` must run on every tool result, not just the final model
  response — a gap that did not exist under the old fixed-whitelist design
  and must not regress if more tools are added later.
- This decision would not have been flagged by the existing `AssessADR`
  keyword heuristic (`containsAny(..., "migration", "durable", "database",
  "schema", "protocol", "security", "authentication")`), since it changes a
  scope-of-responsibility boundary rather than matching one of those
  categories. Recorded here as a known blind spot in that heuristic; fixing
  the heuristic itself is out of scope for this ADR.
