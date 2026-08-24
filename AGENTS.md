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

## GitHub tokens

Fine-grained PATs are scoped to a single GitHub owner — there is no generic
`GH_TOKEN`, every owner polled gets its own `GH_TOKEN_<OWNER>` env var
(`GH_TOKEN_MOONTECHS` for `moontechs/*`, `GH_TOKEN_MKOZIY` for `mkoziy/*`).
The notes vault is a separate GitHub repository and uses its own
`VAULT_GH_TOKEN`; do not alias it to an owner token. Its fine-grained PAT must
be scoped to the vault's owner and grant the repository contents read/write.
`worker/entrypoint.sh` runs `gh auth setup-git --hostname github.com --force`
unconditionally at container start (not gated on any specific token env var
existing) so the credential helper is wired up regardless of which owner's
token a given run ends up exporting.

Onboarding a new owner:

1. Create a fine-grained PAT scoped to that owner, granting only
   **Contents** (read/write — `gh api .../contents/...` and `git push`),
   **Issues** (read — `gh issue list`), **Pull requests** (read/write —
   `gh pr list`/`gh pr create`). Skip `Administration` and `Actions` — this
   pipeline never uses them.
2. Add a `case "$REPO" in ... esac` arm for the new owner in both
   `scripts/github-ticket-poller.sh` and `scripts/github-ticket-worker.sh`,
   exporting `GH_TOKEN` from a distinctly-named `GH_TOKEN_<OWNER>` env var.
   The `*)` fallback arm fails loudly on an unmapped owner — never silently
   fall through to some other owner's token.
3. Set that `GH_TOKEN_<OWNER>` var wherever the pod's other secrets already
   live — `docker-compose.yml`'s `orchestrator` *and* `coding-worker`
   services for local dev (the poller job runs unlabeled, i.e. on the
   orchestrator; the worker job runs on `pool: coding`), the equivalent k3s
   Secret/env for a cluster deployment. Both need it: the poller reads
   issues with it, the coding worker pushes commits and opens the PR with
   it. If both roles happen to run in the same container, set it once there
   — the scripts don't care how many containers are involved.

The orchestrator additionally needs `VAULT_GH_TOKEN` for `vault-repo`; it is
not used by either ticket script and must be injected separately from the
owner tokens.

No workflow YAML or vault involved — this is a plain env var, injected the
same way as `CODEX_ACCESS_TOKEN`, `OPENAI_API_KEY`, etc. (see the table in
[docs/remote-worker.md](docs/remote-worker.md)). Swamp's `local_encryption`
vault type was tried and reverted here: its auto-generated decryption key
lives under `.swamp/` (gitignored, host-local), so it isn't available
wherever the orchestrator actually runs unless manually provisioned —
plain env vars avoid that key-distribution problem entirely.

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
- `github-ticket-poller.sh` invokes `swamp workflow run github-ticket-worker`
  without `--server` — as a plain subprocess of the poller's own model-method
  execution, that call has no connection to the `swamp serve` instance its
  own workflow is running under. The `server_url` workflow input (env
  `SWAMP_SERVE_URL`, which `swamp workflow run` reads without a `--server`
  flag) supplies it; default `ws://127.0.0.1:9090` assumes the poller runs
  in the same pod/container as the orchestrator. Without it, the triggered
  worker run fails instantly: "no worker dispatcher is active".
- Before triggering, `github-ticket-poller.sh` checks
  `swamp workflow history search --input repo=... --input issue_number=...`
  for any non-terminal `github-ticket-worker` run on that issue and skips if
  one exists. Without this, a ralphex run slower than the 15-minute cron
  interval gets a duplicate worker run stacked on top of it every tick,
  contending for the same `command/shell` model lock and failing both.
- The post-ralphex archive step in `github-ticket-worker.sh` only `git mv`s
  the plan into `docs/plans/archive/` if it's still at its original path.
  Some plans instruct ralphex to move themselves elsewhere (e.g.
  `docs/plans/completed/`) as one of their own tasks — if ralphex already did
  that and committed it, the original path is gone and `git mv` on it would
  fail with a fatal "bad source" error (exit 128), killing an otherwise
  fully-successful run right before push/PR.
