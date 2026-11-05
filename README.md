# hermestrator

Automated GitHub ticket implementation on top of [dagu](https://github.com/dagucloud/dagu)
and [ralphex](https://github.com/umputun/ralphex): poll a repo for
`agent-ready` issues that already have a plan committed, then run a coding
agent (Codex or Pi) against each one and open a pull request.

## How it works

```
github-ticket-poller  (dagu DAG, cron every 15m, one step per repo)
  → scripts/github-ticket-poller.sh
      finds open `agent-ready` issues with an agent/issue-<N> branch
      that already contains a committed plan, and no PR yet
  → dagu enqueue github-ticket-worker  (run id pinned to the branch head,
      so the same branch state is never enqueued twice)

github-ticket-worker  (dagu DAG, queue `coding`, max 1 at a time)
  → scripts/github-ticket-worker.sh
      checks out agent/issue-<N>, runs ralphex to implement the issue,
      opens or reuses the pull request
  → on exit (success or failure): scripts/vault-sync.sh
      mirrors the run (progress log, ticket, PR link) into the Obsidian vault
```

Everything runs inside one `dagu start-all` container built from
[worker/Dockerfile](worker/Dockerfile) (`dev` target) — see
[docs/remote-worker.md](docs/remote-worker.md) for image contents, required
runtime credentials, volumes, and local Docker Compose setup. The dagu web UI
(run history, logs, manual runs) is on port 8080.

A second, independent QA flow (checks out the PR, drives the app with a
browser, posts a verdict with screenshots) still lives in `workflows/`,
`scripts/github-qa-*.sh` and the `qa` image target. It is built on swamp, is
**not deployed**, and will move to dagu later.

## Repository layout

| Path | Purpose |
| --- | --- |
| `dags/` | dagu DAG definitions (ticket poller + worker) |
| `scripts/` | shell implementations invoked by the DAGs |
| `worker/` | Multi-stage Dockerfile (`base`/`dev`/`qa`), the dagu entrypoint and config, per-agent ralphex profiles and QA prompts |
| `docs/` | operational docs (container setup, research notes, plans) |
| `tests/` | shell regression tests for the scripts |
| `workflows/`, `models/` | legacy swamp definitions, QA flow only |
| `.github/workflows/` | CI: builds and publishes the worker image on tag push |

## Getting started

To run the stack locally with Docker Compose, see
[docs/remote-worker.md](docs/remote-worker.md#local-docker-development).
Run the tests with `for t in tests/*.sh; do bash "$t"; done`.

## Contributing

See [AGENTS.md](AGENTS.md) for conventions this repo expects coding agents
(and contributors) to follow.
