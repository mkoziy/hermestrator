# `.swamp-actions.yml` manifest format

**This is a target-repo file, not a hermestrator file.** It lives at the root of a repo that the
`github-ticket-actions` flow (poller + `github-actions-worker.sh`) runs against — not in
hermestrator itself. When an open issue in that repo carries the `run-actions` label, the worker
clones the repo, reads this manifest from the clone root, and runs its steps sequentially on
`base_branch`.

Note: the flow's scripts/models use the `github-actions-*` stem (no `ticket`), diverging from the
`github-ticket-actions` workflow name and from the naming triad other flows follow. This is a
known, deliberate divergence — see the "Deliberate naming divergence" note in
`docs/plans/20260814-github-ticket-actions.md`.

## v1 schema

```yaml
version: 1
steps:
  - name: <non-empty string>
    run: <non-empty string, shell command>
```

- `version` — required, integer, must be `1`.
- `steps` — required, non-empty list.
- Each step requires:
  - `name` — non-empty string, used to label step output (`[step: <name>]`) and to report which
    step failed if one does.
  - `run` — non-empty string, a shell command executed via `bash -c` from the repo root.
- Steps execute in order. The first step to exit non-zero stops the run; later steps do not run.

## Example

```yaml
version: 1
steps:
  - name: docker-build
    run: docker build -t my-app .
  - name: test
    run: docker run --rm my-app npm test
```

## Failure visibility

If `.swamp-actions.yml` is missing (or any other pre-clone validation in
`github-actions-worker.sh` fails before it runs), the worker exits via its
`fail()` helper without ever emitting a `VAULT_NOTE_JSON:` marker line. The
report job's `comment-issue` and `write-note` steps both key off that marker,
so a missing manifest produces **no issue comment and no vault note** — only
the workflow run's own log shows the `.swamp-actions.yml is missing at the
repository root` error. Confirmed during Task 10's dry run.

## Platform limitation: NOTE_JSON_RAW is threaded through an env var

The `run-actions` workflow's `report` job passes the worker step's full stdout to
`github-actions-comment.sh`/`vault-write-actions-note.sh` as the `NOTE_JSON_RAW`
environment variable (via `data.query(...)[0].attributes.stdout` in
`workflows/workflow-github-ticket-actions.yaml`). The `command/shell` model
type's `execute` method only accepts `env` (a string map) and has no `stdin` or
file-based input mechanism, so there is no way to hand a large payload to a step
without materializing it into an OS environment variable first — this is a
platform-level constraint of `command/shell`, not something fixable in this
workflow's YAML. In practice `steps_log` (the largest field in the JSON) is
capped by the manifest's own verbosity, and OS env size limits (`ARG_MAX`,
typically several MB) are far above what a normal build/test log produces, but
a sufficiently verbose manifest could still hit the ceiling. If that becomes a
real problem, the fix has to happen at the `command/shell` model level (e.g. an
input that writes to a file instead of an env var), not in this workflow.

## Explicitly out of scope for v1

The following fields are **not** supported and will not be read by v1:

- `env` — no per-step environment variable injection
- `timeout` — no per-step timeout (the worker step's own workflow-level timeout is the only
  ceiling)
- `continue-on-error` — no way to mark a step as non-fatal; any non-zero exit stops the run
- `working-dir` — no per-step working directory override; every step runs from the repo root

These may be added in a future manifest version, but v1 intentionally omits them to keep the
format — and the worker script parsing it — minimal.
