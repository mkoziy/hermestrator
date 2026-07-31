# Spec: Genkit PM web dashboard

## Problem statement

Hermestrator currently runs Hermes as a messaging-driven coding agent. Its PM
workflow is expressed primarily as instructions, so mandatory issue, critique,
review, and approval gates depend on model discipline. The operator also lacks
a focused view of conversations, durable workflow state, subprocess activity,
cost, and recovery.

The replacement needs a deterministic PM runtime whose primary interface is a
web conversation. It must preserve agentic judgment inside bounded phases,
delegate implementation to existing coding executors, and remain deployable as
one container.

## Solution

Introduce a Go PM service built on the Genkit Agents API. The service exposes an
authenticated HTMX/Tabler dashboard through `net/http`, uses OpenRouter for PM
reasoning, persists Genkit sessions and operational records in SQLite, and
sends read-only Telegram notifications for action-required and terminal
events.

The main product workflow is:

`grill-with-docs → to-spec → to-tickets → issue → executor selection →`
`executor-owned plan → critique → plan approval → execution → verification →`
`PR → code review → fixes → merge approval → merge → cleanup`.

The first delivery is a functional dashboard vertical slice. It proves the
highest-risk integration seam before executor orchestration is added.

## Agreed seams

The primary test seam is the complete `net/http` application handler. Tests use
`httptest` with fake GitHub, Genkit model, session-store, and Telegram adapters
to exercise login context, repository selection, conversational generation,
streaming HTML, persistence, and notification emission.

Lower-level tests are reserved for deterministic state transitions,
authorization rules, and SQLite invariants.

## User stories

1. As an operator, I can sign in with GitHub and only access the dashboard when
   my GitHub identity is allowed.
2. As an operator, I can search repositories accessible to the configured
   GitHub automation identity and register one with the PM service.
3. As an operator, I can open a repository workspace and start a persistent PM
   conversation.
4. As an operator, I receive one question at a time during discovery rather
   than a questionnaire.
5. As an operator, I can reload the page or restart the service without losing
   the conversation or its current state.
6. As an operator, I see model output arrive incrementally without a frontend
   framework or full-page reload.
7. As an operator, I can see the active workflow phase, selected model role,
   elapsed time, and recent activity beside the conversation.
8. As an operator, I receive a Telegram notification only when action is
   required or a workflow reaches failure, cancellation, or completion.
9. As an operator, a Telegram notification links back to the authenticated
   dashboard and cannot approve or mutate work itself.
10. As an operator, I can inspect the Genkit trace associated with a PM turn
    when the optional Developer UI is enabled.
11. As an operator, I can see which configured OpenRouter model role handled a
    turn and its reported token usage and cost.
12. As an administrator, I can configure phase-specific model IDs through
    environment variables without exposing model configuration controls in
    the dashboard.

## Implementation decisions

- Genkit owns agent sessions, typed custom state, snapshots, artifacts,
  interrupts, background execution, middleware, tools, and telemetry.
- The web dashboard is the primary interaction surface. Telegram is read-only
  notification delivery.
- Use `net/http`, `html/template`, and server-sent or durable Genkit streaming.
- Use HTMX and Tabler from pinned CDN URLs. Keep custom JavaScript minimal.
- Use `github.com/go-pkgz/auth/v2` for GitHub OAuth, secure JWT cookies, XSRF
  protection, request identity, and allowlist validation.
- Keep dashboard authentication separate from `GH_TOKEN`, which remains the
  automation credential used by `gh` and git.
- Use a pure-Go SQLite driver and WAL mode for single-container persistence.
- Permit one active implementation ticket per repository; different
  repositories may execute concurrently.
- Use a fresh clone for every implementation issue and remove it after a
  confirmed merge while retaining run records and artifacts.
- If ralphex is chosen, invoke the `ralphex` binary directly with distinct
  PM-owned planning and execution config directories. Ralphex generates and
  executes its own plan. The PM may critique or request regeneration but must
  not rewrite it.
- Do not depend on the existing `docker/ralphex-wrapper.sh`,
  `docker/ralphex-headless-plan.sh`, or other Hermes-specific orchestration
  scripts. If direct ralphex planning cannot run non-interactively, fall back
  to direct Codex CLI or Pi agent invocation using the same plan contract.
- Use `grill-with-docs` to update `CONTEXT.md` as terms settle and create ADRs
  only for consequential, hard-to-reverse trade-offs.
- ADR eligibility follows [ADR 0002](../adr/0002-pm-assesses-adr-eligibility.md):
  the PM assesses every settled decision, creates an inspectable proposal for
  each eligible decision, and the operator confirms but cannot override that
  assessment.
- Use phase-specific OpenRouter roles configured by environment variables.
- Prefer Genkit facilities, then the Go standard library, before additional
  dependencies.
- Add the repository's `golang-patterns` skill and the full HTMX skill with
  references to implementation and review context.

## Testing decisions

- Treat the HTTP handler as the primary vertical seam.
- Run strict `golangci-lint`, formatting checks, `go vet`, unit tests, and
  selected race tests through one canonical `make check` command.
- Install a tracked pre-push hook that invokes the same command.
- Reject unexplained or blanket `nolint` directives.
- Use fake executables and temporary Git repositories in later executor
  integration tests.
- Verify persistence by restarting the service against the same database.
- Verify that secrets, OAuth credentials, `GH_TOKEN`, and model keys never
  appear in stored events or rendered pages.

## Out of scope for the first vertical slice

- Creating GitHub issues or pull requests from the dashboard.
- ralphex, Codex, or Pi execution.
- Plan and merge approval flows.
- Repository leases and multi-repository execution.
- Clone lifecycle and crash reconciliation.
- Full `grill-with-docs` mutation of `CONTEXT.md` and ADRs.
- Production replacement of the Hermes gateway.
- A custom trace explorer duplicating the Genkit Developer UI.
- Telegram commands, conversations, or approval buttons.
- GitLab or non-GitHub trackers.
- Kubernetes deployment and `$HERMES_HOME` self-backup.

## Further notes

The first slice must be demoable end to end with a real GitHub login, one
registered repository, one durable conversation, one OpenRouter response, and
one test Telegram notification. Later tickets extend this same seam instead of
building horizontal infrastructure in isolation.
