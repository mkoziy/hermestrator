# ADR 0002: Let the PM assess ADR eligibility

- Status: Accepted
- Date: 2026-07-30

## Context

The intake dashboard currently lets an operator propose an ADR by supplying a
decision, alternative, trade-off, and two self-attested checkboxes. This makes
the ADR boundary optional: trivial decisions can become ADRs, while future
operators cannot tell why a decision did or did not deserve durable
architectural context.

## Decision

The PM assesses every settled discovery decision for ADR eligibility. An
eligible decision must be consequential, hard to reverse, and supported by a
real alternative and trade-off. The assessment records its rationale,
alternatives, trade-off, and reversal cost.

For every independent eligible decision, the PM automatically creates one
inspectable ADR proposal. The operator may confirm each proposal before
publication, but cannot override an ineligible assessment. Ineligible
assessments remain visible in the intake history with their rationale and do
not create ADR proposals.

## Alternatives considered

- **Operator-authored ADRs** — preserves direct control, but reduces the gate
  to self-attestation and makes the workflow inconsistent.
- **Store no ineligible assessments** — keeps the UI smaller, but loses the
  explanation for why a decision remained in the spec or glossary.

## Consequences

- Intake needs durable, typed assessment and proposal artifacts rather than
  one manually submitted ADR form.
- Publication must require confirmation for every generated ADR proposal.
- The PM's assessment prompt and tools require tests that distinguish genuine
  architectural trade-offs from ordinary product decisions.
