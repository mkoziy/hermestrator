# AI guide

Hermestrator is becoming a web-first project-management runtime. Its accepted
architecture is recorded in [ADR 0001](docs/adr/0001-use-genkit-for-the-pm-runtime.md).
Read that ADR, then the current
[dashboard specification](docs/specs/20260726-genkit-pm-dashboard.md) and
[ticket breakdown](docs/tickets/20260726-genkit-pm-dashboard.md), before making
architecture or workflow changes.

## Current direction

- Build the PM service in Go in the `app/` module.
- Use Genkit's Agents API for agent sessions, typed state, snapshots,
  interrupts, background work, tools, middleware, artifacts, and telemetry.
- Use OpenRouter through Genkit's OpenAI-compatible integration.
- Serve the authenticated operator dashboard with `net/http`, `html/template`,
  HTMX, and Tabler. The Genkit Developer UI is for diagnostics, not a product
  dashboard replacement.
- Persist Genkit sessions and operational projections in SQLite. Use a pure-Go
  driver and WAL mode.
- Authenticate dashboard users with `github.com/go-pkgz/auth/v2`. Keep this
  OAuth identity separate from `GH_TOKEN`, the automation credential for GitHub
  and git.
- Send Telegram notifications only for action-required and terminal events.
  They must link back to the dashboard and never mutate or approve work.

## Workflow boundaries

The intended workflow is:

`grill-with-docs → to-spec → to-tickets → issue → executor selection →
executor-owned plan → critique → plan approval → execution → verification →
PR → code review → fixes → merge approval → merge → cleanup`.

The application, not an LLM prompt, must enforce mandatory phase transitions
and approval gates. Keep agent judgment within the phase it is executing.

When executor orchestration is introduced, invoke ralphex, Codex, and Pi
binaries directly. Own separate ralphex configuration directories for planning
and execution. Do not reuse `docker/ralphex-wrapper.sh`,
`docker/ralphex-headless-plan.sh`, or other Hermes-specific orchestration.
Ralphex owns its own plans; the PM may critique or request regeneration but
must not rewrite them.

## Implementation and testing

- Treat the complete `net/http` handler as the primary test seam. Use
  `httptest` and fake GitHub, Genkit/model, session-store, and Telegram
  adapters for vertical behavior.
- Reserve lower-level tests for deterministic state transitions, authorization,
  and SQLite invariants.
- Do not store or render secrets, OAuth credentials, `GH_TOKEN`, or model API
  keys in events, artifacts, logs, or pages.
- Prefer Genkit facilities, then the Go standard library, before adding
  dependencies. Pin Genkit dependencies and review upgrades deliberately: the
  Agents API is beta.
- Use the repository's `golang-patterns` skill for Go work. Maintain the
  canonical `make check` validation command and the tracked pre-push hook when
  adding project tooling.

## Scope discipline

The first vertical slice proves real GitHub login, repository registration,
durable OpenRouter conversation streaming, telemetry, and a test Telegram
notification. Follow the acceptance criteria in Ticket 1.

Do not revive Hermes gateway behavior, its Docker startup wiring, or
`$HERMES_HOME` backup work. Kubernetes deployment, executor orchestration,
GitHub issue/PR mutations, approval flows, repository leases, clone lifecycle,
and Telegram commands belong to later tickets unless the task explicitly
expands scope.
