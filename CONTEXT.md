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
- **ADR proposal** — an inspectable draft artifact automatically created when
  the PM finds a consequential, hard-to-reverse settled decision with a real
  alternative and trade-off. The PM's eligibility assessment is authoritative:
  an operator may confirm an eligible proposal before publication but cannot
  force an ineligible decision into an ADR. Each independent eligible decision
  creates its own proposal and requires its own confirmation.
- **ADR eligibility assessment** — the PM's assessment of every settled
  discovery decision against the ADR criteria. It records whether the decision
  is ADR-worthy and the supporting alternatives, trade-off, and reversal cost.
  A non-eligible assessment remains visible in the intake history with its
  rationale but creates no ADR proposal.
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
