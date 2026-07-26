# ADR 0001: Use Genkit for the PM runtime

- Status: Accepted
- Date: 2026-07-26

## Context

The current Hermes agent relies on a large prompt and skill document to enforce
an issue-first project-management workflow. Mandatory gates can still be
skipped because the LLM remains responsible for remembering and following the
process. The replacement must run in one Docker container, use OpenRouter,
delegate coding to ralphex, Codex, or Pi, resume after crashes, expose strong
observability, and make the web dashboard the primary interaction surface.

## Decision

Build the PM service in Go on Genkit's Agents API.

Use Genkit for typed agent state, server-managed sessions, snapshots,
interrupts, background execution, artifacts, tools, middleware, model calls,
and telemetry. Use custom orchestration to encode the mandatory PM phases and
approval gates. Persist Genkit sessions and operational projections in
SQLite. Serve the product UI with `net/http`, HTMX, and Tabler. Use the Genkit
Developer UI for agent-level diagnostics rather than reproducing its trace
explorer.

The service uses OpenRouter through Genkit's OpenAI-compatible integration.
It prefers Genkit middleware and tools, then the Go standard library, before
adding other dependencies. GitHub login is provided by
`github.com/go-pkgz/auth/v2`.

The PM service invokes ralphex, Codex, and Pi binaries directly. It owns
separate ralphex configuration directories for planning and execution and does
not reuse the existing Hermes-specific ralphex wrappers or helper scripts.

## Alternatives considered

- **Google ADK Go** — strong graph and human-in-the-loop support, but less
  direct OpenRouter and local observability fit.
- **CloudWeGo Eino** — mature Go graph and agent primitives, but Genkit offers
  a closer match for state snapshots, background execution, and developer
  tooling.
- **Custom agent loop** — maximum control, but would require implementing
  model tools, interrupts, session continuity, and telemetry already supplied
  by Genkit.

## Consequences

- The Agents API is beta and may introduce breaking changes in minor releases;
  Genkit dependencies must be pinned and upgrades reviewed deliberately.
- Product-specific GitHub, executor, repository-lock, and cleanup state still
  requires application code and indexed persistence.
- The Genkit Developer UI is a diagnostic surface, not the authenticated PM
  operator console.
- The first delivery is a web-first vertical slice; Telegram is notification
  only.
