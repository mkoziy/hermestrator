# hermestrator

Automated GitHub ticket handling on top of [swamp](https://github.com/swamp-club/swamp).
Two independent flows share this repo's `scripts/`, `workflows/`, and `models/`:

- **agent-ready / ralphex** — poll for `agent-ready` issues that already have a
  plan committed, run a coding agent ([ralphex](https://github.com/umputun/ralphex),
  Codex or Pi) against each one, and open a pull request.
- **run-actions** — poll for `run-actions` issues, clone the target repo, and
  run the steps declared in its own `.swamp-actions.yml` manifest directly on
  `base_branch` (no agent, no branch, no PR) — see
  [docs/swamp-actions-manifest.md](docs/swamp-actions-manifest.md) for the
  manifest schema. Requires `yq` at runtime to parse the manifest (already
  pinned in [worker/Dockerfile](worker/Dockerfile)).

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

```
workflow-github-ticket-actions-poller  (per-repo template, cron)
  → scripts/github-actions-poller.sh
      finds open `run-actions` issues, guards against a still-active or
      orphaned prior run, strips the label
  → triggers workflow-github-ticket-actions for each match

workflow-github-ticket-actions  (manual or triggered)
  → scripts/github-actions-worker.sh
      clones the repo, checks out base_branch directly, runs the steps
      declared in its .swamp-actions.yml manifest, reports success/failure
  → posts a status comment on the issue and syncs a run note to the vault
```

Each workflow's implementation step (`implement-github-issue`, `run-actions`)
is a `swamp` model-method step labeled `pool: coding`, so it executes on a
remote worker built from [worker/Dockerfile](worker/Dockerfile) — see
[docs/remote-worker.md](docs/remote-worker.md) for image contents, required
runtime credentials, and local Docker Compose setup. The surrounding
vault-sync and issue-comment steps run unlabeled, on the orchestrator itself.

## Repository layout

| Path | Purpose |
| --- | --- |
| `workflows/` | swamp workflow definitions — poller + worker for both the ralphex flow and the run-actions flow |
| `scripts/` | shell implementations invoked by the workflows — two independent sets, one per flow |
| `models/` | swamp model definitions — command/shell models backing both flows |
| `worker/` | Dockerfile and entrypoint for the remote coding worker, plus per-agent ralphex profiles |
| `docs/` | operational docs — remote worker setup, research notes, and [swamp-actions-manifest.md](docs/swamp-actions-manifest.md) (the run-actions `.swamp-actions.yml` schema) |
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
