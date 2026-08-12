# AGENTS.md

Instructions for coding agents (Codex, Pi, and human contributors)
working in this repository.

## Start here

This repo is managed with [swamp](https://github.com/swamp-club/swamp).
**[CLAUDE.md](CLAUDE.md)** is the source of truth for swamp conventions
(models, workflows, extensions, CEL expressions) — read it first. It is a
swamp-managed file; don't hand-edit the marked section.

## What this repo does

`workflows/workflow-github-ticket-poller.yaml` polls a GitHub repo for
`agent-ready` issues with a plan already committed to `agent/issue-<N>`, and
triggers `workflows/workflow-github-ticket-worker.yaml` to implement each one
via ralphex and open a PR. See [README.md](README.md) for the full flow and
[docs/remote-worker.md](docs/remote-worker.md) for the worker image.

## Shell scripts (`scripts/`)

New or edited scripts should match the existing style in
`scripts/github-ticket-worker.sh` and `scripts/github-ticket-poller.sh`:

- `#!/usr/bin/env bash` + `set -Eeuo pipefail`.
- Validate every external input (repo, issue number, branch name) with an
  explicit regex or `git check-ref-format` before using it — these scripts
  run against attacker-influenceable GitHub data.
- Required env vars declared up top with `: "${VAR:?VAR is required}"`;
  optional ones with `: "${VAR:=default}"`.
- A single `fail()` helper for user-facing errors; a `cleanup()` trap on
  `EXIT` for anything that needs teardown (temp worktrees, workspaces).
- Check required CLI tools (`gh`, `jq`, `swamp`, ...) are on `PATH` before use.

## Workflows and models

Don't hand-write workflow YAML from scratch — use `swamp workflow create`
and the `swamp` skill (see CLAUDE.md rule 9). Same for models: search for an
existing type before writing a custom extension (CLAUDE.md rule 1).

## Docker / worker image

`worker/Dockerfile` pins exact versions and SHA-256 checksums for every
downloaded binary (swamp, ralphex, gh, the Pi adapter script). If you bump a
version, update its pinned checksum in the same change — don't drop
verification. Never bake credentials into the image; runtime secrets are
injected via environment variables only (see the table in
[docs/remote-worker.md](docs/remote-worker.md)).

## CI

`.github/workflows/build-and-push.yml` lints the Dockerfile with hadolint,
builds the image, runs a smoke check (`swamp version`, `ralphex --version`,
etc. inside the container), then pushes to GHCR on tag push. Keep the image
buildable and the smoke check passing for any Dockerfile change.

## Commits and PRs

- Keep commits scoped to one logical change.
- Don't commit secrets, `.env` files, or anything matching `*secret*` /
  `credentials*` — both `.gitignore` and `.dockerignore` already exclude
  these; don't work around that.
- When ralphex opens a PR for an `agent/issue-<N>` branch, its plan lives in
  `docs/plans/` on that branch per the worker script's contract — don't
  remove that convention without updating `scripts/github-ticket-worker.sh`
  and `scripts/github-ticket-poller.sh` together, since the poller depends
  on it to detect a "plan committed" state.
- After a successful run, the worker moves the processed plan to
  `docs/plans/archive/` and removes the `agent-ready` label. A follow-up on
  the same issue is just: commit a new `docs/plans/*.md` file to the same
  `agent/issue-<N>` branch and re-add `agent-ready` — the poller re-triggers
  the worker, which pushes into the still-open PR or opens a new one if the
  previous PR was closed/merged.
- The poller routes each issue to a ralphex profile by label: `agent-pi` →
  `ralphex-pi`, `agent-codex` → `ralphex-codex`, neither → the workflow's
  `ralphex_config` default (project-wide). If an issue somehow carries both,
  `agent-pi` wins.
- The poller workflows (`workflow-github-ticket-poller.yaml`,
  `-files-nest.yaml`) run on their own `command/shell` model
  (`github_ticket_poller_shell`), deliberately separate from the worker's
  (`github_ticket_worker_shell`). A poller step synchronously calls `swamp
  workflow run github-ticket-worker` from inside its own model-method
  execution — if it shared the worker's model, that call would deadlock
  waiting for a lock its own outer execution already holds. Don't merge them
  back onto one model.
