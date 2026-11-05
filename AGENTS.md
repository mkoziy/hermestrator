# AGENTS.md

Instructions for coding agents (Codex, Pi, and human contributors)
working in this repository.

## Start here

The coding flow is orchestrated by [dagu](https://github.com/dagucloud/dagu):
DAG definitions live in `dags/`, the container setup in `worker/` (see
[docs/remote-worker.md](docs/remote-worker.md)). The QA flow is **not deployed**
and still carries its old [swamp](https://github.com/swamp-club/swamp)
workflows/models until it is migrated; **[CLAUDE.md](CLAUDE.md)** holds the
swamp conventions that apply to that legacy part only. It is a swamp-managed
file; don't hand-edit the marked section.

## What this repo does

`dags/github-ticket-poller.yaml` polls GitHub repos for `agent-ready` issues
with a plan already committed to `agent/issue-<N>`, and enqueues
`dags/github-ticket-worker.yaml` for each one; the worker implements it via
ralphex and opens a PR, then mirrors the run into the notes vault. See
[README.md](README.md) for the full flow and
[docs/remote-worker.md](docs/remote-worker.md) for the container.

A second, independent flow QAs the PR the dev flow opened —
`workflows/workflow-github-qa-poller.yaml` / `-worker.yaml`. It is currently
not deployed; see "QA flow" below and
[docs/plans/20260916-qa-agent-flow.md](docs/plans/completed/20260916-qa-agent-flow.md).

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
- Check required CLI tools (`gh`, `jq`, `dagu`, ...) are on `PATH` before use.

## DAGs

Coding-flow DAGs are plain dagu YAML in `dags/`. Gotchas verified against
dagu 2.25:

- No `name:` key — the DAG name is the filename. Check a change with
  `dagu validate dags/<file>.yaml` (CI runs it inside the built image).
- Steps get only an env allowlist. A variable a script reads must be set in the
  DAG's `env:`/`params:` or listed in the image's `DAGU_ENV_PASSTHROUGH*`.
- Run ids must match `[A-Za-z0-9_-]` and be shorter than 64 characters, and an
  id is unique forever (finished runs included) — see the dedup note under
  "Commits and PRs".
- `action: dag.enqueue` cannot set a run id; anything that needs a
  deterministic id calls `dagu enqueue --run-id` from a script.
- A failing `handler_on.exit` fails the run even when every step succeeded.
- Run `dagu` CLI commands as the `worker` user, never root.

Swamp rules in CLAUDE.md (search before you build, `swamp workflow create`)
apply only to the not-yet-migrated QA workflows.

## Docker / worker image

`worker/Dockerfile` is multi-stage: a shared `base` stage (swamp, gh, mise,
codex, pi — everything every target needs), then two targets built on top of
it: `dev` (the ralphex-based coding image, adds ralphex/gremlins and dagu —
the one k3s/Compose run) and `qa` (browser-driven QA worker, built `FROM base`
— adds chromium/agent-browser, deliberately not ralphex; not deployed, see "QA
flow" below). Put a tool in `base` only if every target actually uses it; a
version/checksum bump on a shared tool then happens once, not once per target.
swamp stays in `base` only until the QA flow moves to dagu. `SHELL
["/bin/bash", "-o", "pipefail", "-c"]` does not carry across a new `FROM`
even within the same file — any stage with a `RUN ... | cmd` pipe needs
its own `SHELL` redeclaration, or hadolint's `DL4006` will catch the
missing one.

It pins exact versions and SHA-256 checksums for every downloaded binary
(swamp, dagu, ralphex, gh, the Pi adapter script, mise). If you bump a version,
update its pinned checksum in the same change — don't drop verification.
Never bake credentials into the image; runtime secrets are injected via
environment variables only (see the table in
[docs/remote-worker.md](docs/remote-worker.md)).

The image intentionally does **not** preinstall language runtimes/package
managers (bun, node beyond the base image, etc.) — it only provides `mise`.
Each onboarded repo pins its own tool versions via its own `mise.toml` and
installs them from `scripts/agent-setup.sh` (see "Repo setup script" below).
This keeps the shared worker image generic instead of hardcoding one repo's
tool version for every repo the worker runs.

## CI

`.github/workflows/build-and-push.yml` is a matrix over `[dev, qa]`: each
lints the shared Dockerfile with hadolint, builds its own `--target`, runs a
target-specific smoke check inside the container (`dev`: `swamp version`,
`dagu version`, `dagu validate` on both DAGs, `ralphex --version`, etc.; `qa`:
`swamp version`, `agent-browser --version`, `chromium --version`, etc. — no
`ralphex --version`, it isn't installed there), then pushes to GHCR with a
`qa-`-prefixed tag for `qa` and the existing untouched tag scheme for `dev`.
Keep both targets buildable and both smoke checks passing for any Dockerfile
change.

## GitHub tokens

Fine-grained PATs are scoped to a single GitHub owner — there is no generic
`GH_TOKEN`, every owner polled gets its own `GH_TOKEN_<OWNER>` env var
(`GH_TOKEN_MOONTECHS` for `moontechs/*`, `GH_TOKEN_MKOZIY` for `mkoziy/*`).
The notes vault is a separate GitHub repository and uses its own
`VAULT_GH_TOKEN`; do not alias it to an owner token. Its fine-grained PAT must
be scoped to the vault's owner and grant the repository contents read/write.
`worker/dagu-entrypoint.sh` logs `gh` in with `VAULT_GH_TOKEN` and runs
`gh auth setup-git --hostname github.com --force` at container start, so the
vault `git pull`/`push` has a credential helper regardless of which owner's
token a given ticket run ends up exporting.

Onboarding a new owner:

1. Create a fine-grained PAT scoped to that owner, granting only
   **Contents** (read/write — `gh api .../contents/...` and `git push`),
   **Issues** (read — `gh issue list`), **Pull requests** (read/write —
   `gh pr list`/`gh pr create`). Skip `Administration` and `Actions` — this
   pipeline never uses them.
2. Add a `case "$REPO" in ... esac` arm for the new owner in all four
   ticket scripts — `scripts/github-ticket-poller.sh`,
   `scripts/github-ticket-worker.sh`, `scripts/github-qa-poller.sh`,
   `scripts/github-qa-worker.sh` — exporting `GH_TOKEN` from a
   distinctly-named `GH_TOKEN_<OWNER>` env var. The `*)` fallback arm fails
   loudly on an unmapped owner — never silently fall through to some other
   owner's token.
3. Set that `GH_TOKEN_<OWNER>` var wherever the container's other secrets
   already live — `docker-compose.yml`'s `dagu` service for local dev, the
   equivalent k3s Secret/env for a cluster deployment. The image's
   `DAGU_ENV_PASSTHROUGH_PREFIXES=GH_TOKEN_` already forwards any
   `GH_TOKEN_*` variable to the steps, so no passthrough change is needed.
   The poller reads issues with it and the worker pushes commits and opens the
   PR with it. (The not-yet-migrated QA scripts need it too, wherever they
   run.)

`VAULT_GH_TOKEN` is not used by any ticket script; the entrypoint consumes it
for the vault checkout and then unsets it. Inject it separately from the owner
tokens.

These are plain env vars, injected the same way as `CODEX_ACCESS_TOKEN`,
`OPENAI_API_KEY`, etc. (see the table in
[docs/remote-worker.md](docs/remote-worker.md)).

## Repo setup script

Every ticket run clones the target repo into a fresh, empty `/tmp` workspace
(see `scripts/github-ticket-worker.sh`) — nothing is cached between runs, so
dependencies never exist until installed there. `github-ticket-worker.sh`
handles this generically for any language by running `scripts/agent-setup.sh`
right after clone, before ralphex starts, if that file exists and is
executable in the target repo. No file → no-op, so onboarding a repo that
needs no setup requires nothing here.

Onboarding a new repo that *does* need setup: add `scripts/agent-setup.sh` to
that repo (not to hermestrator) with this prompt:

> Create `scripts/agent-setup.sh` in this repo's root. This script is run by
> an external ticket worker right after it clones the repo into a fresh,
> empty workspace and before a coding agent starts work — its job is to make
> the checkout ready for whatever validation gates the agent is expected to
> run (lint, test, typecheck, build), across every part of this repo that
> has one (e.g. a monorepo's separate workspaces/services).
>
> - `#!/usr/bin/env bash` with `set -Eeuo pipefail`, executable (`chmod +x`).
> - The worker image only provides `mise` (https://mise.jdx.dev) — no
>   language runtimes. Add this repo's own `mise.toml`/`.mise.toml` pinning
>   the exact tool versions its CI and local devs use (e.g. `bun = "x.y.z"`),
>   run `mise install` before anything else, and make sure the rest of the
>   script and everything the agent runs afterward resolves those exact
>   binaries (not whatever happens to be on PATH already).
> - Install this repo's dependencies using whatever this repo's own
>   tooling/package manager already is — don't introduce a new one. Prefer a
>   single top-level command (e.g. one workspace install) over per-directory
>   installs if the repo's tooling supports it.
> - If this repo has a client-side git hook (pre-commit, pre-push, etc.)
>   that gates lint/typecheck/test, make it mandatory here: after install,
>   verify the hook file actually exists and is executable, and exit
>   non-zero if it doesn't. A coding agent silently committing past a
>   missing gate is worse than the run failing loudly at setup time.
> - After install, do a cheap sanity check that the validation commands this
>   repo's CI/pre-commit hooks actually rely on resolve correctly (e.g. run
>   `--version` on the lint/test binaries, or the repo's own `check`
>   command), and exit non-zero if something didn't resolve — don't leave a
>   half-set-up workspace to fail silently later, deeper into the agent's
>   run.
> - No unrelated build/toolchain steps (native mobile builds, docs builds,
>   docker builds, etc.) — only what's needed for lint/test/typecheck/build
>   commands the agent will actually invoke.
> - Match what this repo's own CI or pre-commit hooks already run — don't
>   invent new tooling or commands that don't already exist somewhere in the
>   repo.
> - No comments beyond one line if something is genuinely non-obvious; this
>   is a small infra script, not documentation.

## QA flow (not deployed)

> QA was removed from k3s and Compose when the coding flow moved to dagu. The
> text below describes the swamp-based design that stays in the repo as the
> source for the later QA migration.

Independent of the dev flow above (implement → PR), a second poller/worker
pair QAs the PR the dev flow opened:

- **Trigger**: `scripts/github-ticket-worker.sh`'s `mark_ready_for_qa`
  helper (called from all three of its PR-ready exit paths — reuse an
  existing open PR, reuse a concurrently-created PR, a newly created PR —
  don't add a fourth call site that bypasses it) removes `agent-ready` and
  any stale `agent-qa-failed`, and adds `agent-qa-ready`.
- **`workflows/workflow-github-qa-poller.yaml`** is one workflow covering
  every polled repo (`repos` input, comma-separated) — deliberately *not*
  one file per repo like the dev flow's `-files-nest.yaml`/etc., so the
  repo list/schedule/label live in one place. It scans `agent-qa-ready`
  issues with an open PR on `agent/issue-<N>` and triggers
  `workflow-github-qa-worker.yaml`. Same `agent-pi`/`agent-codex` label
  routing, in-flight-run guard, and detached-trigger pattern as the dev
  poller — see `scripts/github-qa-poller.sh`.
- **`workflows/workflow-github-qa-worker.yaml`** pins the PR's head SHA
  before checkout (so a push landing between label-add and checkout can't
  silently QA a stale commit), runs `scripts/agent-setup.sh` if present,
  then invokes the agent **directly** — `codex exec` / `pi --print`, not
  ralphex. ralphex is a diff-oriented plan/implement/review tool; even its
  `--review` mode requires committed changes to `git diff` against, and QA
  makes no code changes at all. Its own `agent` input/`AGENT` env var
  (`codex`/`pi`, default `pi`) is deliberately ralphex-free naming — see
  `agent-pi`/`agent-codex` label routing in `scripts/github-qa-poller.sh`.
  The one exception is `scripts/vault-write-note.sh`'s shared note schema,
  which still stores the value under the dev flow's `ralphex_config` JSON
  key/`RALPHEX_CONFIG` env var — that writer is shared verbatim with the
  dev flow and isn't QA-specific to rename.
- The QA agent runs in its JSON event-stream mode (`codex exec --json`,
  `pi --print --mode json`) rather than plain text, because both CLIs
  buffer plain-text output entirely in memory and only write it on exit —
  a killed run would leave an empty log with no clue what happened. It is
  killed on idle (`QA_IDLE_TIMEOUT_SECONDS`, default 900s of no new
  events — a single tool call, e.g. a test suite or lighthouse audit, can
  go quiet for minutes with zero events; see `run_qa_agent` in
  `scripts/github-qa-worker.sh`), not on a flat wall-clock deadline;
  `QA_TIMEOUT_SECONDS` (default 5400s) is a hard cap against a genuinely
  runaway agent. It must end its output with an
  exact `QA_VERDICT: PASS` or `QA_VERDICT: FAIL: <reason>` line
  (`worker/qa/prompts/task.txt`) — a missing/garbage line is always
  parsed as a fail, never a silent pass.
- Screenshots go to the Obsidian vault, not GitHub: the QA agent writes PNGs
  under `RUN_ARTIFACTS_DIR/WORKFLOW_RUN_ID/screenshots` (same shared volume
  used for logs), and `vault-write-note.sh`'s `write-note` step — already
  invoked with those two env vars by both worker workflows — copies them
  into `<repo>/issue-<n>/runs/<run_ts>-screenshots/` next to that run's
  note, embedding them with Obsidian `![[...]]` links. No-op when the
  directory is empty (the dev flow never populates it). An earlier version
  pushed screenshots to a dedicated `qa-screenshots` GitHub branch and
  linked them from the verdict comment; that path silently stopped working
  after the first-ever run (a `--single-branch` clone never populates
  `origin/<branch>` for `git checkout -B ... origin/<branch>` on any branch
  but the one cloned) and was dropped in favor of the vault.
- Verdict comment posted, then `agent-qa-ready` → `agent-qa-passed` or
  `agent-qa-failed` (whichever verdict label isn't being added is removed
  first, so a re-run can't leave both present). `qa_failed → agent_ready`
  is a **manual** step — a human reviews the failure comment and re-adds
  `agent-ready` by hand; nothing here automates that transition.
- All three QA labels (`agent-qa-ready`/`-passed`/`-failed`) must be
  created in each target repo before enabling this flow — same unstated
  precondition `agent-ready`/`agent-pi`/`agent-codex` already have; `gh
  issue edit` requires a label to exist repo-wide before it can be
  added/removed.
- The `qa` worker image is heavy (Chrome/agent-browser) and deliberately
  not a persistent daemon like `coding-worker`: `SWAMP_WORKER_IDLE_TIMEOUT`
  makes it drain whatever `pool:qa` work is queued and exit, meant to be
  started on a schedule (host cron + `docker compose run --rm qa-worker`,
  or a k3s `CronJob` — see `docs/remote-worker.md`), not left running.

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
- The poller (`scripts/github-ticket-poller.sh`, run by
  `dags/github-ticket-poller.yaml`) enqueues the worker with
  `dagu enqueue --run-id gh-<issue>-<8-char repo hash>-<12-char branch head
  sha>`. dagu rejects an id it has ever seen, so the same branch state is
  never enqueued twice — a ralphex run slower than the 15-minute cron
  interval can't get a duplicate stacked on top of it. A new commit on the
  branch (ralphex progress push, a follow-up plan) yields a new id. A failed
  run with no new commit is therefore not retried automatically — push a new
  commit to the `agent/issue-<N>` branch to retrigger it.
- The post-ralphex archive step in `github-ticket-worker.sh` only `git mv`s
  the plan into `docs/plans/archive/` if it's still at its original path.
  Some plans instruct ralphex to move themselves elsewhere (e.g.
  `docs/plans/completed/`) as one of their own tasks — if ralphex already did
  that and committed it, the original path is gone and `git mv` on it would
  fail with a fatal "bad source" error (exit 128), killing an otherwise
  fully-successful run right before push/PR.
