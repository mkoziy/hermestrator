# `.swamp-actions.yml` manifest format

**This is a target-repo file, not a hermestrator file.** It lives at the root of a repo that the
`github-ticket-actions` flow (poller + `github-actions-worker.sh`) runs against — not in
hermestrator itself. When an open issue in that repo carries the `run-actions` label, the worker
clones the repo, reads this manifest from the clone root, and runs its steps sequentially on
`base_branch`.

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

## Explicitly out of scope for v1

The following fields are **not** supported and will not be read by v1:

- `env` — no per-step environment variable injection
- `timeout` — no per-step timeout (the worker step's own workflow-level timeout is the only
  ceiling)
- `continue-on-error` — no way to mark a step as non-fatal; any non-zero exit stops the run
- `working-dir` — no per-step working directory override; every step runs from the repo root

These may be added in a future manifest version, but v1 intentionally omits them to keep the
format — and the worker script parsing it — minimal.
