# Domain Context

## Glossary

- **PM service** — the Go service that owns product-management conversations,
  workflow state, approvals, executor delegation, and the operator UI. It is
  intended to replace Hermes as the primary agent runtime.
- **Repository** — a GitHub repository registered with the PM service. At most
  one implementation ticket may execute for a repository at a time.
- **Ticket session** — the durable Genkit session associated with one GitHub
  implementation issue.
- **Intake session** — the pre-issue conversation that runs
  `grill-with-docs`, resolves vocabulary, and produces a spec and ticket
  candidates.
- **Intake draft** — inspectable, unpublished discovery output. It may be
  revised freely and must be discarded safely when abandoned.
- **Confirmed specification** — a synthesized record of settled discovery
  decisions. It is immutable for publication purposes until regenerated and
  reconfirmed by the operator.
- **Confirmed ticket set** — tracer-bullet vertical slices with explicit
  blocking edges that the operator has approved for GitHub publication.
- **ADR proposal** — an intake artifact used only when a consequential,
  hard-to-reverse decision has a real alternative and trade-off.
- **Workflow phase** — a deterministic PM state. The model reasons within a
  phase; it does not choose arbitrary phase transitions.
- **Artifact** — a durable Genkit output such as a spec, ticket, plan,
  critique, diff, or review report.
- **Executor** — ralphex, Codex, Pi, or a verification-only command runner.
- **Repository lease** — the durable lock that serializes active
  implementation work for one repository.
- **Operator** — an authenticated GitHub user allowed to use the PM dashboard.
- **Action-required notification** — a read-only Telegram notification linking
  to a dashboard question, approval, or failure.
