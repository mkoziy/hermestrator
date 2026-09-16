# QA testing skill

Reference for the QA agent (codex or pi) running worker/qa/prompts/task.txt.
task.txt has the ordered checklist; this is the "how" for the steps that
need more than one line — read it when a step isn't obvious from the
ticket alone.

## Deriving test cases from the ticket

Don't test every input you can think of — pick a small set that actually
finds bugs:

- **Golden path**: the scenario the ticket describes working. Always test
  this first; if it fails, the edge cases don't matter yet.
- **Boundary values**: empty input, the max the UI allows, one past it,
  zero, negative. Bugs cluster at boundaries far more than in the middle
  of a valid range.
- **Equivalence classes**: group inputs that should behave the same way
  and test one representative per group, not all of them (e.g. one valid
  email, one malformed one — not five variations of "malformed").
- **Risk-based cutoff**: you're timeboxed. Spend the time on paths that are
  user-visible or touch data (submit, delete, payment, auth) before
  cosmetic or rarely-hit permutations.

## Visual regression

Only run this when a baseline actually exists — a screenshot linked in the
ticket, or one already committed in the repo (e.g. docs/screenshots/).
There is no baseline for a first-time feature; don't screenshot the PR
build and call that a baseline, it proves nothing.

```
agent-browser diff screenshot --baseline <path-to-baseline.png>
```

Flag differences that look unintended (broken layout, missing element,
wrong color). Expected UI changes described by the ticket are not
regressions.

## Performance

Sanity check, not a full audit — you don't have time budget or a real
baseline to compare against:

```
agent-browser vitals <url> --json
```

Flag values that are clearly broken, not just non-optimal:
- LCP over ~4s
- CLS over ~0.25
- TTFB over ~1.5s on a page with no obvious heavy backend work

If a number is borderline, mention it in passing rather than failing the
build over it — this is a smoke check for regressions the ticket didn't
intend, not a performance review.

## Accessibility

Sanity check, not a full audit — same spirit as performance:

```
agent-browser a11y <url> --json
```

Flag violations it reports on the golden path. Not every finding is worth
a FAIL — a missing `alt` on a decorative image isn't the same severity as
a form control with no accessible label. Use judgment; note the rest in
your reasoning.

## Console and network errors

```
agent-browser console
agent-browser errors
```

Run these after exercising each flow, not just once at the end — an error
thrown mid-interaction can scroll out of a single end-of-run check.

## Verdict

Every check above produces evidence, not a verdict on its own. Fail only
for things that break the ticket's actual requirement. A stray console
warning from a third-party script, or a borderline vitals number, is a
note in your reasoning — not a FAIL by itself. See task.txt for the exact
`QA_VERDICT:` line format.
