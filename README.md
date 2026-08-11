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

## Repository layout

| Path | Purpose |
| --- | --- |
| `workflows/` | swamp workflow definitions (poller + worker) |
| `scripts/` | shell implementations invoked by the workflows |
| `models/` | swamp model definitions |
| `worker/` | Dockerfile and entrypoint for the remote coding worker, plus per-agent ralphex profiles |
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
