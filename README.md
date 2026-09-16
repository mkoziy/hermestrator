# hermestrator

Automated GitHub ticket implementation on top of [swamp](https://github.com/swamp-club/swamp)
and [ralphex](https://github.com/umputun/ralphex): poll a repo for
`agent-ready` issues that already have a plan committed, then run a coding
agent (Codex or Pi) against each one and open a pull request.

## How it works

```
workflow-github-ticket-poller  (cron, every 15m)
  → scripts/github-ticket-poller.sh
      finds open `agent-ready` issues with an agent/issue-<N> branch
      that already contains a committed plan, and no PR yet
  → triggers workflow-github-ticket-worker for each match

workflow-github-ticket-worker  (manual or triggered)
  → scripts/github-ticket-worker.sh
      checks out agent/issue-<N>, runs ralphex to implement the issue,
      opens or reuses the pull request
```

Both workflows run as `swamp` model-method steps labeled `pool: coding`, so
they execute on a remote worker built from [worker/Dockerfile](worker/Dockerfile) —
see [docs/remote-worker.md](docs/remote-worker.md) for image contents, required
runtime credentials, and local Docker Compose setup.

A second, independent flow QAs the PR the flow above opened — checks it out,
starts the app, runs any e2e tests, drives it with a browser, and posts a
pass/fail verdict with screenshots back to the issue:

```
workflow-github-qa-poller  (cron, every 15m, covers every polled repo)
  → scripts/github-qa-poller.sh
      finds open `agent-qa-ready` issues with an open PR on agent/issue-<N>
  → triggers workflow-github-qa-worker for each match

workflow-github-qa-worker  (manual or triggered)
  → scripts/github-qa-worker.sh
      checks out the PR at its pinned head commit, runs a coding agent
      directly (no ralphex — QA makes no code changes) to QA it, posts a
      verdict comment, swaps agent-qa-ready for agent-qa-passed/-failed
```

Labeled `pool: qa`; runs on a separate, heavier worker image (Chrome +
`agent-browser`) built from the same `worker/Dockerfile`'s `qa` target, and
is meant to be started on a schedule rather than run as a persistent daemon
— see `docs/remote-worker.md`'s "QA ticket worker" section.

## Repository layout

| Path | Purpose |
| --- | --- |
| `workflows/` | swamp workflow definitions (poller + worker) |
| `scripts/` | shell implementations invoked by the workflows |
| `models/` | swamp model definitions |
| `worker/` | Multi-stage Dockerfile (`base`/`dev`/`qa`) and entrypoint for the remote workers, plus per-agent ralphex profiles and QA prompts |
| `docs/` | operational docs (remote worker setup, research notes) |
| `.github/workflows/` | CI: builds and publishes the worker image on tag push |

## Getting started

This repo is managed with swamp. If you don't have it installed, or are new
to swamp, run:

```bash
swamp --help
```

and follow the `swamp` / `swamp-getting-started` guidance in [CLAUDE.md](CLAUDE.md).

To run the worker locally with Docker Compose, see
[docs/remote-worker.md](docs/remote-worker.md#local-docker-development).

## Contributing

See [AGENTS.md](AGENTS.md) for conventions this repo expects coding agents
(and contributors) to follow.
