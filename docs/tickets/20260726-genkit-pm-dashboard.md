# Tickets: Genkit PM web dashboard

## Ticket 1: Deliver the authenticated conversational dashboard slice

Blocked by: none

Spec: https://github.com/mkoziy/hermestrator/issues/1
Issue: https://github.com/mkoziy/hermestrator/issues/2

Build the smallest end-to-end dashboard that proves GitHub authentication,
repository selection, a durable Genkit conversation, incremental HTMX
rendering, model-role telemetry, and read-only Telegram notifications.

### Source layout

Place the first slice in the repository's `app/` Go module, rather than in a
role-specific module such as `pm/` or `services/pm/`. The dashboard and runtime
are shared application capabilities: PM, review, and future roles can use the
same UI and packages without crossing Go module or `internal/` visibility
boundaries. Keep role-specific behavior as packages within `app/` until a role
has a genuine independent deployment boundary.

### Acceptance criteria

- GitHub login is implemented with `github.com/go-pkgz/auth/v2`.
- Only configured GitHub users can access protected pages and endpoints.
- The repository picker lists repositories visible to the configured GitHub
  automation identity and persists the selected repository.
- A repository page supports a durable Genkit/OpenRouter conversation.
- Responses stream into HTMX fragments rendered with Tabler components.
- A restart preserves the conversation through a SQLite-backed Genkit store.
- The page shows phase, model role, elapsed time, recent activity, token usage,
  and reported cost where available.
- Telegram sends a read-only notification for a configured test or
  action-required event and includes a dashboard link.
- Genkit traces are available in the optional Developer UI.
- The full HTTP interaction is covered through the agreed `httptest` seam.
- `make check` runs formatting verification, module checks, `go vet`, strict
  `golangci-lint`, tests, and selected race tests.
- A tracked pre-push hook invokes `make check`.
- The `golang-patterns` skill and the complete HTMX skill with references are
  available to local development agents and baked into the image.

## Ticket 2: Turn dashboard discovery into tracked GitHub work

Issue: https://github.com/mkoziy/hermestrator/issues/3

Blocked by: https://github.com/mkoziy/hermestrator/issues/2

Add `grill-with-docs`, live `CONTEXT.md` and selective ADR artifacts,
`to-spec`, tracer-bullet `to-tickets`, GitHub issue publication, and the
repository-scoped intake workspace.

## Ticket 3: Add executor-owned planning and implementation

Issue: https://github.com/mkoziy/hermestrator/issues/4

Blocked by: https://github.com/mkoziy/hermestrator/issues/3

Add executor routing, fresh issue clones, ralphex-owned plan generation,
mandatory plan critique and approval, Codex/Pi fallback, subprocess streaming,
and verification gates. Invoke executor binaries directly with PM-owned config
directories; do not reuse the existing Hermes-related wrapper or headless-plan
scripts.

## Ticket 4: Complete the PR, review, merge, recovery, and cleanup loop

Issue: https://github.com/mkoziy/hermestrator/issues/5

Blocked by: https://github.com/mkoziy/hermestrator/issues/4

Add PR creation, independent standards/spec review, post-review fixes, merge
approval, crash reconciliation, issue closure, clone cleanup, retention, and
repository leases. Review fixes must use direct ralphex, Codex CLI, or Pi agent
invocation rather than existing Hermes-related scripts.
